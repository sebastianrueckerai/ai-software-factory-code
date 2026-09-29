package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// stagesWorkflow is TaskWorkflow on testStages edited by edit, with a
// session cap of sessions.
func stagesWorkflow(sessions int, edit func(w *policy.Workflow)) TaskWorkflow {
	cfg := testWorkflowConfig
	cfg.MaxSessions = sessions
	w := policy.Workflow{Stages: testStages()}
	if edit != nil {
		edit(&w)
	}
	cfg.Stages = w.Stages
	return TaskWorkflow{Config: func() WorkflowConfig { return cfg }}
}

// setStage changes one stage of w.
func setStage(w *policy.Workflow, name string, f func(s *policy.Stage)) {
	for i := range w.Stages {
		if w.Stages[i].Name == name {
			f(&w.Stages[i])
		}
	}
}

func models(sessions []SessionInput) []string {
	var out []string
	for _, s := range sessions {
		out = append(out, strings.TrimPrefix(s.Model, "factory/"))
	}
	return out
}

func (h *workflowHarness) skipped() []string {
	var out []string
	for _, r := range h.recorded {
		if r.step == "stage_skipped" {
			out = append(out, r.detail["stage"].(string)+": "+r.detail["reason"].(string))
		}
	}
	return out
}

// Red checks go back to the agent: the first return keeps the cheap
// model, the second from the same stage takes the stronger one; at the
// cap of three the third red goes to the card with every finding so far.
func TestStagesRedChecksKeepThenClimbThenCard(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"), pushed("s3", "c3"))
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"red 1"}},
		ChecksResult{State: ChecksFailure, Findings: []string{"red 2"}},
		ChecksResult{State: ChecksFailure, Findings: []string{"red 3"}})
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if got := models(g.sessions); !slices.Equal(got, []string{"coding-cheap", "coding-cheap", "coding-frontier"}) {
		t.Fatalf("models = %v", got)
	}
	if !slices.Equal(g.sessions[1].Findings, []string{"red 1"}) || !slices.Equal(g.sessions[2].Findings, []string{"red 2"}) {
		t.Fatalf("findings = %v / %v", g.sessions[1].Findings, g.sessions[2].Findings)
	}
	if len(g.approvals) != 1 || g.approvals[0].Commit != "c3" {
		t.Fatalf("approvals = %+v", g.approvals)
	}
	why := strings.Join(g.approvals[0].WhyHuman, "\n")
	for _, want := range []string{"session cap", "checks, round 1: red 1", "checks, round 2: red 2", "checks, round 3: red 3"} {
		if !strings.Contains(why, want) {
			t.Fatalf("why human lacks %q:\n%s", want, why)
		}
	}
}

// on_findings human sends red checks to the card after one session; the
// other stages still run and no auto merge is tried.
func TestStagesHumanFindingsGoToTheCard(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, func(w *policy.Workflow) {
		setStage(w, "checks", func(s *policy.Stage) { s.OnFindings = "human" })
	}), func(g *gateHarness) {
		g.gate = PolicyGate{Class: "normal", Outcome: policy.OutcomeAuto, MergeRule: policy.MergeAuto}
		g.repro = []ReproResult{{Ran: true, Exit: 0, BaseExit: 1, Command: "go test ./x"}}
		yes := true
		g.review = Review{Verdict: "approve", Summary: "fine", StrictImprovement: &yes}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"lint failed"}})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.sessions) != 1 {
		t.Fatalf("final = %q, sessions %d", final, len(g.sessions))
	}
	if want := []string{"policy", "repro c1", "review c1", "approval c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if len(g.autoNotes) != 0 || !slices.Contains(g.approvals[0].WhyHuman, "checks: lint failed") {
		t.Fatalf("auto notes %v, why human %v", g.autoNotes, g.approvals[0].WhyHuman)
	}
}

// on_findings fail ends the task at the stage.
func TestStagesFailFindingsFailTheTask(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, func(w *policy.Workflow) {
		setStage(w, "reproduction", func(s *policy.Stage) { s.OnFindings = "fail" })
	}), func(g *gateHarness) {
		g.repro = []ReproResult{{Ran: true, Command: "go test ./x", Exit: 1, Output: "--- FAIL"}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})

	if final := g.run(t); final != "failed" || len(g.sessions) != 1 {
		t.Fatalf("final = %q, sessions %d", final, len(g.sessions))
	}
	if !strings.HasPrefix(g.finals[0][1], "reproduction: ") || !strings.Contains(g.finals[0][1], "--- FAIL") {
		t.Fatalf("reason = %q", g.finals[0][1])
	}
	if !slices.Equal(g.steps, []string{"policy", "repro c1"}) {
		t.Fatalf("steps = %v", g.steps)
	}
}

// A forbidden class on the default policy stage fails with the old
// reason; on_findings agent sends the paths back instead.
func TestStagesForbiddenClass(t *testing.T) {
	forbidden := func(g *gateHarness) {
		g.gate = PolicyGate{Class: "forbidden", Outcome: policy.OutcomeForbidden, Paths: []string{".forgejo/x.yml"}, PolicyVersion: 7}
	}
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), forbidden)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "failed" || g.finals[0][1] != "policy: policy version 7: risk class forbidden is forbidden to agents: .forgejo/x.yml" {
		t.Fatalf("final = %q, reason %q", final, g.finals[0][1])
	}

	g = newGateHarnessWith(t, stagesWorkflow(2, func(w *policy.Workflow) {
		setStage(w, "policy", func(s *policy.Stage) { s.OnFindings = "agent" })
	}), forbidden)
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(10*time.Hour, SignalPRClosed, TaskSignal{PR: 9})
	g.run(t)
	if len(g.sessions) != 2 || !slices.Equal(g.sessions[1].Findings, []string{g.finalsReasonForbidden()}) || g.sessions[1].Model != "factory/coding-cheap" {
		t.Fatalf("second session = %+v", g.sessions[1])
	}
}

func (g *gateHarness) finalsReasonForbidden() string {
	return "policy version 7: risk class forbidden is forbidden to agents: .forgejo/x.yml"
}

// A disabled stage does not run and is recorded as skipped; a stage for
// outsiders is skipped for a writer's task.
func TestStagesDisabledAndWhenAreSkippedOnTheTimeline(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, func(w *policy.Workflow) {
		setStage(w, "reproduction", func(s *policy.Stage) { s.Enabled = false })
		setStage(w, "review", func(s *policy.Stage) { s.When = "outsider" })
	}))
	g.state.Standing = harness.StandingWriter
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if want := []string{"policy", "approval c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	want := []string{"screening: the issue's author is not an outside account", "reproduction: disabled", "review: the issue's author is not an outside account"}
	if got := g.skipped(); !slices.Equal(got, want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

// A checks stage that did not run green never merges on auto (review
// I1): a workflow the loader would refuse, checks disabled, red checks
// never read, goes to the card with the reason.
func TestStagesSkippedChecksNeverAutoMerge(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, func(w *policy.Workflow) {
		setStage(w, "checks", func(s *policy.Stage) { s.Enabled = false })
	}), autoGates)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"red"}})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.approvals) != 1 || len(g.autoNotes) != 0 || !slices.Equal(g.merges, []string{"9 c1 sebastian"}) ||
		!slices.Contains(g.approvals[0].WhyHuman, "the checks did not run green") {
		t.Fatalf("approvals %+v, notes %v, merges %v", g.approvals, g.autoNotes, g.merges)
	}
}

// A telemetry task is not screened under when outsider.
func TestStagesTelemetryIsNotScreened(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingTelemetry
	g.onScreen(ScreenResult{Verdict: ScreenSuspect})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || g.screened != 0 {
		t.Fatalf("final = %q, screened %d", final, g.screened)
	}
}

// A failed escalation leaves the screening alert unjudged: no human
// decided (review M5).
func TestStagesFailedScreeningEscalationJudgesNothing(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingOutside
	g.onScreen(ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"x"}})
	g.escalateErr = temporal.NewNonRetryableApplicationError("forgejo down", errTerminal, nil)
	g.onSessions(pushed("s1", "c1"))
	if final := g.run(t); final != "failed" || len(g.judged) != 0 || len(g.sessions) != 0 {
		t.Fatalf("final = %q, judged %v, sessions %d", final, g.judged, len(g.sessions))
	}
}

// A new run continues the stage memory the timeline kept (review M1):
// the task's second return from the review takes the stronger rung, the
// review gets the earlier one and the card shows every round.
func TestStagesHealedRunKeepsTheStageMemory(t *testing.T) {
	first := Review{Verdict: "concerns", Summary: "the flag is unchecked", Findings: []string{"main.go: check the flag"}}
	second := Review{Verdict: "concerns", Summary: "still unchecked", Findings: []string{"main.go: still"}}
	third := Review{Verdict: "concerns", Summary: "unchecked once more", Findings: []string{"cli.go: check it"}}
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), func(g *gateHarness) { g.reviews = []Review{second, third} })
	// the dead run: session 1, review concerns, session 2 pushed c2
	g.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{Attempts: 2},
		Returns: map[string]int{"review": 1}, History: []string{"review, round 1: main.go: check the flag"}, Reviews: []Review{first}, Round: 1}
	g.onSessions(pushed("s3", "c3"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.sessions) != 1 || g.sessions[0].Model != "factory/coding-frontier" || !slices.Equal(g.sessions[0].Findings, second.Findings) {
		t.Fatalf("sessions = %+v", g.sessions)
	}
	if len(g.earlier) != 2 || len(g.earlier[0]) != 1 || len(g.earlier[1]) != 2 {
		t.Fatalf("earlier = %+v", g.earlier)
	}
	in := g.approvals[0]
	why := strings.Join(in.WhyHuman, "\n")
	if len(in.EarlierReviews) != 2 || !strings.Contains(why, "review, round 1: main.go: check the flag") ||
		!strings.Contains(why, "review, round 2: main.go: still") || !strings.Contains(why, "review, round 3: cli.go: check it") {
		t.Fatalf("card = %+v", in)
	}
}

// The screening report task runs on the stronger rung (ruling 191).
func TestStagesScreeningReportRunsOnTheStrongerRung(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Report, g.state.Stronger = true, true
	var models []string
	g.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, pin int64) (ReportResult, error) {
			models = append(models, model)
			return ReportResult{}, nil
		})
	if final := g.run(t); final != "done" || !slices.Equal(models, []string{"factory/coding-frontier"}) {
		t.Fatalf("final = %q, models %v", final, models)
	}
}

// The review loop follows the one rule (ruling 190): with a session cap
// of three, concerns go back twice (the first on the same model, the
// second on the stronger one) and the third opens the card with every
// review; each review is given the ones before it.
func TestStagesReviewConcernsLoop(t *testing.T) {
	first := Review{Verdict: "concerns", Summary: "the flag is unchecked", Findings: []string{"main.go: check the flag"}}
	second := Review{Verdict: "concerns", Summary: "still unchecked", Findings: []string{"main.go: the flag is still unchecked"}}
	third := Review{Verdict: "concerns", Summary: "unchecked in one place", Findings: []string{"cli.go: check the flag"}}
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), func(g *gateHarness) { g.reviews = []Review{first, second, third} })
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"), pushed("s3", "c3"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if got := models(g.sessions); !slices.Equal(got, []string{"coding-cheap", "coding-cheap", "coding-frontier"}) {
		t.Fatalf("models = %v", got)
	}
	if !slices.Equal(g.sessions[1].Findings, first.Findings) || !slices.Equal(g.sessions[2].Findings, second.Findings) {
		t.Fatalf("findings = %v / %v", g.sessions[1].Findings, g.sessions[2].Findings)
	}
	if len(g.earlier) != 3 || len(g.earlier[0]) != 0 || len(g.earlier[1]) != 1 || len(g.earlier[2]) != 2 || g.earlier[2][1].Summary != second.Summary {
		t.Fatalf("earlier reviews given = %+v", g.earlier)
	}
	in := g.approvals[0]
	if len(g.approvals) != 1 || in.Review.Summary != third.Summary || len(in.EarlierReviews) != 2 ||
		in.EarlierReviews[0].Summary != first.Summary || in.EarlierReviews[1].Summary != second.Summary {
		t.Fatalf("card = %+v", in)
	}
	why := strings.Join(in.WhyHuman, "\n")
	for _, want := range []string{"session cap", "review, round 1: main.go: check the flag", "review, round 3: cli.go: check the flag"} {
		if !strings.Contains(why, want) {
			t.Fatalf("why human lacks %q:\n%s", want, why)
		}
	}
}

// max_returns 1 on the review: one concerns goes back, the second opens
// the card, a session short of the cap.
func TestStagesReviewMaxReturns(t *testing.T) {
	first := Review{Verdict: "concerns", Summary: "the flag is unchecked", Findings: []string{"main.go: check the flag"}}
	second := Review{Verdict: "concerns", Summary: "still unchecked", Findings: []string{"main.go: the flag is still unchecked"}}
	one := 1
	g := newGateHarnessWith(t, stagesWorkflow(3, func(w *policy.Workflow) {
		setStage(w, "review", func(s *policy.Stage) { s.MaxReturns = &one })
	}), func(g *gateHarness) { g.reviews = []Review{first, second} })
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"), pushed("s3", "c3"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.sessions) != 2 {
		t.Fatalf("final = %q, sessions %d", final, len(g.sessions))
	}
	in := g.approvals[0]
	if in.Commit != "c2" || in.Review.Summary != second.Summary || len(in.EarlierReviews) != 1 {
		t.Fatalf("card = %+v", in)
	}
	why := strings.Join(in.WhyHuman, "\n")
	for _, want := range []string{"review reached its max_returns of 1", "review, round 1: main.go: check the flag", "review, round 2: main.go: the flag is still unchecked"} {
		if !strings.Contains(why, want) {
			t.Fatalf("why human lacks %q:\n%s", want, why)
		}
	}
	if strings.Contains(why, "session cap") {
		t.Fatalf("why human names the session cap:\n%s", why)
	}
}

// Screening: a clear verdict lets the coder start; the Screen activity
// runs before the first session.
func TestStagesScreeningClear(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingOutside
	g.onScreen(ScreenResult{Verdict: ScreenClear})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || g.screened != 1 || len(g.escalated) != 0 {
		t.Fatalf("final = %q, screened %d, escalated %v", final, g.screened, g.escalated)
	}
}

// A suspect verdict is the card: the task waits on a human; the human's
// answer lets it run (the screening alert judged noise) with the answer
// in the brief; no session runs before it.
func TestStagesScreeningSuspectWaitsThenRuns(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingOutside
	g.onScreen(ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"asks to print the token"}})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "fine, fix the slug only", Comment: 700})
	g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.escalated) != 1 || !strings.Contains(g.escalated[0], "asks to print the token") {
		t.Fatalf("escalated = %v", g.escalated)
	}
	if !slices.Equal(g.judged, []string{VerdictNoise}) || len(g.sessions) != 1 {
		t.Fatalf("judged %v, sessions %d", g.judged, len(g.sessions))
	}
	if len(g.sessions[0].Answers) != 1 || !strings.Contains(g.sessions[0].Answers[0], "fine, fix the slug only") || g.sessions[0].Attempt != 1 {
		t.Fatalf("session = %+v", g.sessions[0])
	}
}

// A human who closes the issue of a suspect report ends the task and
// judges the screening real.
func TestStagesScreeningSuspectClosed(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingOutside
	g.onScreen(ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"asks to print the token"}})
	g.onSessions(pushed("s1", "c1"))
	g.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "failed" || len(g.sessions) != 0 || !slices.Equal(g.judged, []string{VerdictReal}) {
		t.Fatalf("final = %q, sessions %d, judged %v", final, len(g.sessions), g.judged)
	}
}

// A factoryd restart while a suspect screening waits on a human: the
// healed run waits again; the human's answer judges the alert noise and
// runs the first session with no extra one granted; a close judges it real.
func TestStagesHealedScreeningWait(t *testing.T) {
	for _, closed := range []bool{false, true} {
		g := newGateHarnessWith(t, stagesWorkflow(3, nil))
		g.state = RunState{Status: "waiting-human", Standing: harness.StandingOutside, Screening: true,
			Question: "The screening finds this issue suspect: asks to print the token"}
		g.onScreen(ScreenResult{Verdict: ScreenSuspect})
		g.onSessions(pushed("s1", "c1"))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		if closed {
			g.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		} else {
			g.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go ahead", Comment: 700})
			g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		}
		final := g.run(t)
		if closed {
			if final != "failed" || len(g.sessions) != 0 || !slices.Equal(g.judged, []string{VerdictReal}) {
				t.Fatalf("closed: final %q, sessions %d, judged %v", final, len(g.sessions), g.judged)
			}
			continue
		}
		if final != "done" || g.screened != 0 || !slices.Equal(g.judged, []string{VerdictNoise}) || len(g.sessions) != 1 || g.sessions[0].Attempt != 1 {
			t.Fatalf("answered: final %q, screened %d, judged %v, sessions %+v", final, g.screened, g.judged, g.sessions)
		}
		for _, u := range g.usage {
			if u.ExtraAttempts != 0 {
				t.Fatalf("usage %+v: the answer granted a session", u)
			}
		}
	}
}

// A writer's issue is not screened under when outsider.
func TestStagesScreeningOnlyForOutsiders(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingWriter
	g.onScreen(ScreenResult{Verdict: ScreenSuspect})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || g.screened != 0 {
		t.Fatalf("final = %q, screened %d", final, g.screened)
	}
}

// A report task runs one report session and ends done: no pull request,
// no stage.
func TestStagesReportTaskEndsDoneAtTheComment(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Report = true
	var reports []string
	g.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, pin int64) (ReportResult, error) {
			reports = append(reports, model)
			return ReportResult{Comment: 77}, nil
		})
	g.onSessions(pushed("s1", "c1"))
	if final := g.run(t); final != "done" || len(g.sessions) != 0 || len(g.steps) != 0 {
		t.Fatalf("final = %q, sessions %d, steps %v", final, len(g.sessions), g.steps)
	}
	if !slices.Equal(reports, []string{"factory/coding-cheap"}) || !slices.Equal(g.states, []string{"running"}) {
		t.Fatalf("reports %v, states %v", reports, g.states)
	}
}

// A promoted improvement's first session gets the analyzer's write-up.
func TestStagesWriteUpIsTheFirstSessionsFinding(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.WriteUp = "Change slug.go; risk normal; 1 h; do it."
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.run(t)
	if len(g.sessions[0].Findings) != 1 || !strings.Contains(g.sessions[0].Findings[0], "Change slug.go; risk normal; 1 h; do it.") {
		t.Fatalf("findings = %v", g.sessions[0].Findings)
	}
}

// The stages read the pinned version: GatePolicy and the review get it.
func TestStagesPassThePin(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalPRClosed, TaskSignal{PR: 9})
	g.run(t)
	if !slices.Equal(g.policyPins, []int64{7}) || len(g.approvals) != 1 || g.approvals[0].Gates.Pin != 7 {
		t.Fatalf("policy pins %v, approvals %+v", g.policyPins, g.approvals)
	}
}

// A model review that fails as an activity still leaves its step on the
// timeline, so a restart rebuilds every review (re-review m1).
func TestStagesFailedReviewIsOnTheTimeline(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), func(g *gateHarness) {
		g.reviewErr = temporal.NewNonRetryableApplicationError("gateway down", errTerminal, nil)
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalPRClosed, TaskSignal{PR: 9})
	g.run(t)
	var got []map[string]any
	for _, r := range g.recorded {
		if r.step == "model_review" {
			got = append(got, r.detail)
		}
	}
	if len(got) != 1 || got[0]["round"] != float64(1) || !strings.Contains(got[0]["note"].(string), "model review failed") || got[0]["commit"] != "c1" {
		t.Fatalf("model_review steps = %v", got)
	}
}

// A screening report task names no comment it did not post (re-review m2).
func TestStagesScreeningReportEndsWithoutAComment(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Report, g.state.Stronger = true, true
	g.run(t)
	if g.finals[0][1] != "report kept on the task, no issue comment" {
		t.Fatalf("reason = %q", g.finals[0][1])
	}
}

// withAudit lists the audit stage (ruling 188) between review and
// approval.
func withAudit(w *policy.Workflow) {
	i := slices.IndexFunc(w.Stages, func(s policy.Stage) bool { return s.Name == policy.StageReview })
	w.Stages = slices.Insert(w.Stages, i+1, policy.Stage{Name: policy.StageAudit, OnFindings: policy.FindingsHuman, Enabled: true})
}

var plantedFinding = AuditFinding{Item: "secret in the diff: password, secret or token assigned a literal", File: "internal/x/x_test.go", Line: 4,
	Why: "an added line has the shape of a password, secret or token assigned a literal (pre-check)"}

// A clear audit passes to the approval as before: on auto, the merge
// needs no card.
func TestStagesAuditClearPassesToTheAutoMerge(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), autoGates)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := []string{"policy", "repro c1", "review c1", "audit c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if len(g.approvals) != 0 || len(g.autoNotes) != 1 || !slices.Equal(g.auditPins, []int64{7}) {
		t.Fatalf("approvals %d, auto notes %v, audit pins %v", len(g.approvals), g.autoNotes, g.auditPins)
	}
}

// Audit findings go to the card beside the review, never to the agent:
// one session, the card with the audit's section and why a human.
func TestStagesAuditFindingsGoToTheCardWithTheReview(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), func(g *gateHarness) {
		g.audits = []AuditResult{{Verdict: AuditFindings, Findings: []AuditFinding{plantedFinding}, Model: "coding-frontier"}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.sessions) != 1 {
		t.Fatalf("final = %q, sessions %d", final, len(g.sessions))
	}
	in := g.approvals[0]
	if in.Review.Verdict != "approve" || in.Gates.Audit == nil || !slices.Equal(in.Gates.Audit.Findings, []AuditFinding{plantedFinding}) {
		t.Fatalf("approval input = %+v", in)
	}
	if !slices.Equal(in.WhyHuman, []string{"audit: the security audit reports 1 finding for a human"}) {
		t.Fatalf("why human = %v", in.WhyHuman)
	}
}

// On the merge rule auto, audit findings turn the merge into the card.
func TestStagesAuditFindingsStopTheAutoMerge(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), autoGates, func(g *gateHarness) {
		g.audits = []AuditResult{{Verdict: AuditFindings, Findings: []AuditFinding{plantedFinding, {Item: auditNoVerdict, Why: "the answer is not one JSON object: EOF"}}}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.autoNotes) != 0 || len(g.approvals) != 1 || !slices.Equal(g.merges, []string{"9 c1 sebastian"}) ||
		!slices.Equal(g.approvals[0].WhyHuman, []string{"audit: the security audit reports 2 findings for a human"}) {
		t.Fatalf("auto notes %v, approvals %+v, merges %v", g.autoNotes, g.approvals, g.merges)
	}
}

// An audit that fails for good is a finding on the card, never a pass,
// and the timeline records it.
func TestStagesFailedAuditIsAFinding(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), autoGates, func(g *gateHarness) {
		g.auditErr = temporal.NewNonRetryableApplicationError("forgejo gone", "terminal", nil)
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.autoNotes) != 0 || len(g.approvals) != 1 {
		t.Fatalf("final = %q, auto notes %v, approvals %d", final, g.autoNotes, len(g.approvals))
	}
	a := g.approvals[0].Gates.Audit
	if a == nil || a.Verdict != AuditFindings || len(a.Findings) != 1 || a.Findings[0].Item != auditNoVerdict || !strings.Contains(a.Findings[0].Why, "forgejo gone") {
		t.Fatalf("audit on the card = %+v", a)
	}
	var steps []map[string]any
	for _, r := range g.recorded {
		if r.step == "audit" {
			steps = append(steps, r.detail)
		}
	}
	if len(steps) != 1 || steps[0]["verdict"] != AuditFindings {
		t.Fatalf("audit steps = %v", steps)
	}
}

// A project the audit skips: no finding, the auto merge goes on; a
// disabled audit stage is skipped on the timeline without a call.
func TestStagesAuditSkipped(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), autoGates, func(g *gateHarness) {
		g.audits = []AuditResult{{Skipped: auditNotSwitchedOn}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "done" || len(g.autoNotes) != 1 || len(g.approvals) != 0 {
		t.Fatalf("final = %q, auto notes %v, approvals %d", final, g.autoNotes, len(g.approvals))
	}

	g = newGateHarnessWith(t, stagesWorkflow(3, func(w *policy.Workflow) {
		withAudit(w)
		setStage(w, "audit", func(s *policy.Stage) { s.Enabled = false })
	}), autoGates)
	g.state.Standing = harness.StandingWriter
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "done" || slices.Contains(g.steps, "audit c1") {
		t.Fatalf("final = %q, steps %v", final, g.steps)
	}
	if got := g.skipped(); !slices.Contains(got, "audit: disabled") {
		t.Fatalf("skipped = %v", got)
	}
}
