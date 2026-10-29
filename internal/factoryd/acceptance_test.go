package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"go.temporal.io/sdk/workflow"
)

// Roadmap 208: a plan's acceptance commands are the runs that prove a
// card's work. A session that ran one green carries the fact home, the
// workflow closes the card done on it, and an approver may close a card
// that waits on a human whose trail shows it — no answer owed.

func TestRunsAcceptance(t *testing.T) {
	for _, tt := range []struct {
		run, item string
		want      bool
	}{
		{"bash check", "bash check", true},
		{"  bash   check ", "bash check", true}, // whitespace only
		{"bash check -- ./...", "bash check", true},
		{"bash check", "bash check -- ./...", false}, // the run did less than the plan named
		{"bash", "bash check", false},
		{"bashcheck", "bash check", false},
		{"bash lint", "bash check", false},
		{"Bash check", "bash check", false}, // a shell command's case is its meaning
		{"", "bash check", false},
		{"bash check", "", false},
		// a step of an && chain: its exit 0 says every step passed
		{"cd /workspace/repo && bash check -- ./... 2>&1", "bash check", true},
		{"cd /workspace/repo &&bash check&& echo done", "bash check", true},
		{"cd /workspace/repo && bash check || true", "bash check", false},
		{"cd /workspace/repo && bash check | tee log", "bash check", false},
		// a pipe the run adds hides the command's exit behind its last stage
		{"bash check | tee log", "bash check", false},
		{"bash check -- ./... 2>&1 | tail -20", "bash check", false},
		// a command's own operators are its own: a run of it is its exit
		{"go test ./... | grep -v skip", "go test ./... | grep -v skip", true},
		{"go test ./... | grep -v skip -e flaky", "go test ./... | grep -v skip", true},
		{"cd /workspace/repo && go test ./... | grep -v skip && echo done", "go test ./... | grep -v skip", true},
		{"go test ./... | grep -v skip | tee log", "go test ./... | grep -v skip", false},
		{"cat list | go test ./... | grep -v skip", "go test ./... | grep -v skip", false},
		{"cd /workspace/repo; bash check", "bash check", false},
		{"cd /workspace/repo && bash check & wait", "bash check", false},
		{"cd /workspace/repo && bash check\necho $?", "bash check", false},
		{"cd /workspace/repo && bash checks", "bash check", false},
	} {
		if got := runsAcceptance(tt.run, tt.item); got != tt.want {
			t.Errorf("runsAcceptance(%q, %q) = %v, want %v", tt.run, tt.item, got, tt.want)
		}
	}
}

func TestAcceptanceHit(t *testing.T) {
	cmd := func(command string, exit int) harness.Event {
		return harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": command, "exit": exit}}
	}
	green, red, wider := cmd("bash check", 0), cmd("bash check", 1), cmd("bash check -- ./...", 0)
	acceptance := []string{"bash check"}
	// the newest green run wins: a red after a green does not undo the
	// card's evidence, and a green after it does not lose to it
	hit := acceptanceHit([]harness.Event{green, red, wider}, acceptance)
	if hit == nil || hit.Command != "bash check -- ./..." || hit.Exit != 0 {
		t.Fatalf("hit = %+v, want the newest green run, bash check -- ./... at 0", hit)
	}
	hit = acceptanceHit([]harness.Event{green, red}, acceptance)
	if hit == nil || hit.Command != "bash check" {
		t.Fatalf("hit = %+v, want the earlier green to stand over the later red", hit)
	}
	if acceptanceHit([]harness.Event{red}, acceptance) != nil {
		t.Error("a session that only ran the command red carries no acceptance")
	}
	if acceptanceHit([]harness.Event{green}, nil) != nil {
		t.Error("a card without acceptance commands never closes this way")
	}
	// a recorded command's exit arrives as a JSON number, a built one as
	// an int; other kinds and shapeless details are not runs
	if acceptanceHit([]harness.Event{cmd("bash check", 0), {Kind: harness.EventModelCall, Detail: map[string]any{"command": "bash check", "exit": 0}},
		{Kind: harness.EventCommand, Detail: map[string]any{"command": "bash check"}}}, acceptance) == nil {
		t.Error("the green run among other events went unseen")
	}
}

// A session that ran one of its plan's acceptance commands green carries
// the hit home on the result and records the green run on the card's
// trail; a red-only session carries none.
func TestRunSessionCarriesAnAcceptanceGreenHome(t *testing.T) {
	for _, tt := range []struct {
		name string
		exit int
		want bool
	}{
		{"green", 0, true},
		{"red", 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			task := Task{ID: "t1", Status: "running", Source: SourcePlan, Parent: "t-plan", PlanKey: "check",
				Objective: "add a README line", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
			kube := newFakeKube()
			objects := &fakeObjectStore{}
			runner, _, store := newTestRunner(t, task, kube, objects)
			(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
			if err := store.CreateTask(context.Background(), Task{ID: "t-plan", Status: "in-review",
				Repository: "sf/ai-software-factory-code", Objective: "a plan", Track: "direct"}); err != nil {
				t.Fatal(err)
			}
			plan, _ := json.Marshal(Plan{Summary: "the work", Cards: []PlanCard{{Key: "check", Title: "a card", Brief: "its brief",
				Acceptance: []PlanItem{{Text: "bash check"}}}}})
			if _, err := store.AddPlanRevision(context.Background(), "t-plan", plan, "sha1"); err != nil {
				t.Fatal(err)
			}
			logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(),
				Detail: map[string]any{"command": "bash check", "exit": tt.exit}}) + "\n" +
				resultLine(false, "out of time")
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil, logs)
			if err != nil {
				t.Fatalf("RunSession: %v", err)
			}
			if res.Outcome != OutcomeNoPush {
				t.Fatalf("outcome = %s, want no_push", res.Outcome)
			}
			if !tt.want {
				if res.Acceptance != nil {
					t.Fatalf("a red-only session carried %+v home", res.Acceptance)
				}
				return
			}
			if res.Acceptance == nil || res.Acceptance.Command != "bash check" || res.Acceptance.Exit != 0 || res.Acceptance.Session != res.SessionID {
				t.Fatalf("result = %+v, want the green run of session %s", res.Acceptance, res.SessionID)
			}
			// the card's trail says what ran green, in which session
			trail, err := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(trail), `"event":"`+EventAcceptancePassed+`"`) ||
				!strings.Contains(string(trail), `"session":"`+res.SessionID+`"`) {
				t.Fatalf("the trail holds no acceptance green: %s", trail)
			}
		})
	}
}

// Roadmap 201: a session whose acceptance command is a step of an &&
// chain, past the line's first 512 bytes, carries the green home with the
// whole line: the worker's adapter keeps the command whole, and only a
// model's reading of it is cut. The workflow closes the card on it
// (TestWorkflowClosesOnAnAcceptanceGreen).
func TestRunSessionCarriesAnAcceptanceStepPastTheCut(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Source: SourcePlan, Parent: "t-plan", PlanKey: "check",
		Objective: "add a README line", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
	if err := store.CreateTask(context.Background(), Task{ID: "t-plan", Status: "in-review", Repository: "sf/ai-software-factory-code", Objective: "a plan", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(Plan{Summary: "the work", Cards: []PlanCard{{Key: "check", Title: "a card", Brief: "its brief",
		Acceptance: []PlanItem{{Text: "go test ./acceptance/"}}}}})
	if _, err := store.AddPlanRevision(context.Background(), "t-plan", plan, "sha1"); err != nil {
		t.Fatal(err)
	}
	chain := "cd /workspace/repo && " + strings.Repeat("test -f go.mod && ", 30) + "go test ./acceptance/ -run TestA1"
	if strings.Index(chain, "go test") <= 512 {
		t.Fatalf("the acceptance step starts at byte %d, want past 512", strings.Index(chain, "go test"))
	}
	part, _ := json.Marshal(map[string]any{"tool": "bash", "state": map[string]any{"status": "completed",
		"input": map[string]any{"command": chain}, "output": "ok", "metadata": map[string]any{"exit": 0}}})
	events := parseLines(&opencodeParser{}, `{"type":"tool_use","part":`+string(part)+`}`)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		envLine(events[0])+"\n"+resultLine(false, "out of time"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Acceptance == nil || res.Acceptance.Command != chain || res.Acceptance.Exit != 0 {
		t.Fatalf("acceptance = %+v, want the whole chain's green run", res.Acceptance)
	}
}

// A run proves an acceptance command only where its exit status is the
// command's: a red run of "go test ./acceptance/" piped to tee exits with
// tee's 0 and carries nothing home, while a command whose own text holds
// a pipe is proven by a green run of exactly that text, and not by one the
// run pipes on to tee. The workflow closes the card on what a session
// carries home (TestWorkflowClosesOnAnAcceptanceGreen).
func TestRunSessionAcceptanceOnlyWhereTheExitIsTheCommands(t *testing.T) {
	for _, tt := range []struct {
		name, item, run string
		want            bool
	}{
		{"red piped to tee", "go test ./acceptance/", "go test ./acceptance/ | tee test.log", false},
		{"its own pipe, exactly", "go test ./... | grep -v skip", "go test ./... | grep -v skip", true},
		{"its own pipe, then tee", "go test ./... | grep -v skip", "go test ./... | grep -v skip | tee log", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			task := Task{ID: "t1", Status: "running", Source: SourcePlan, Parent: "t-plan", PlanKey: "check",
				Objective: "add a README line", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
			runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
			(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
			if err := store.CreateTask(context.Background(), Task{ID: "t-plan", Status: "in-review", Repository: "sf/ai-software-factory-code", Objective: "a plan", Track: "direct"}); err != nil {
				t.Fatal(err)
			}
			plan, _ := json.Marshal(Plan{Summary: "the work", Cards: []PlanCard{{Key: "check", Title: "a card", Brief: "its brief", Acceptance: []PlanItem{{Text: tt.item}}}}})
			if _, err := store.AddPlanRevision(context.Background(), "t-plan", plan, "sha1"); err != nil {
				t.Fatal(err)
			}
			logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": tt.run, "exit": 0, "output": "ok"}}) + "\n" +
				resultLine(false, "out of time")
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil, logs)
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Acceptance != nil; got != tt.want || got && res.Acceptance.Command != tt.run {
				t.Fatalf("acceptance = %+v, want carried %v", res.Acceptance, tt.want)
			}
		})
	}
}

// The workflow closes a card done on the session's acceptance green
// without another session and without a human's answer, whatever the
// session's own verdict; without a green it ends as it always did.
func TestWorkflowClosesOnAnAcceptanceGreen(t *testing.T) {
	t.Run("no push", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "the checks pass here",
			Acceptance: &AcceptanceHit{Command: "bash check", Exit: 0, Session: "s1"}})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, want done", final)
		}
		if len(h.sessions) != 1 {
			t.Fatalf("sessions = %d, want one: the green bought no further one", len(h.sessions))
		}
		if h.finals[0][1] != `acceptance "bash check" exited 0 in session s1` {
			t.Fatalf("the close's reason is %q", h.finals[0][1])
		}
	})
	t.Run("escalate", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "the supervisor worries",
			Acceptance: &AcceptanceHit{Command: "go test ./acceptance/ passes", Exit: 0, Session: "s2"}})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, want done", final)
		}
		if len(h.escalated) != 0 {
			t.Fatalf("the green session escalated anyway: %v", h.escalated)
		}
	})
	t.Run("no green", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.env.OnGetVersion("session-cap", workflow.DefaultVersion, versionSessionCap).Return(workflow.DefaultVersion)
		h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "out of time"})
		if final := h.run(t); final != "failed" {
			t.Fatalf("final = %q, want failed", final)
		}
		if !strings.Contains(h.finals[0][1], "3 attempts exhausted") {
			t.Fatalf("the close's reason is %q, want the attempts exhausted", h.finals[0][1])
		}
	})
	t.Run("pushed keeps its verification", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(SessionResult{Outcome: OutcomePushed, SessionID: "s1", Commit: "c1", Summary: "pushed",
			Acceptance: &AcceptanceHit{Command: "bash check", Exit: 0, Session: "s1"}})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, want done", final)
		}
		if !slices.Equal(h.states, []string{"running", "in-review"}) {
			t.Fatalf("states = %v, want the pushed session's verification", h.states)
		}
	})
}
