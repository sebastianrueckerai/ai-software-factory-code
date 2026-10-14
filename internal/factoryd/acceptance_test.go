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
				Objective: "add a README line", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
			kube := newFakeKube()
			objects := &fakeObjectStore{}
			runner, _, store := newTestRunner(t, task, kube, objects)
			(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
			if err := store.CreateTask(context.Background(), Task{ID: "t-plan", Status: "in-review",
				Repository: "sf/ai-software-factory-code", Objective: "a plan"}); err != nil {
				t.Fatal(err)
			}
			plan, _ := json.Marshal(Plan{Summary: "the work", Cards: []PlanCard{{Key: "check", Title: "a card", Brief: "its brief",
				Acceptance: []string{"bash check"}}}})
			if _, err := store.AddPlanRevision(context.Background(), "t-plan", plan, "sha1"); err != nil {
				t.Fatal(err)
			}
			logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(),
				Detail: map[string]any{"command": "bash check", "exit": tt.exit}}) + "\n" +
				resultLine(false, "out of time")
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil, logs)
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
