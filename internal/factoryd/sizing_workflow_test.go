package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// sizedConfig is TaskConfig of a direct coding card in a process with a
// plan track.
func sizedConfig() *TaskConfig {
	return &TaskConfig{Process: "spec", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, PlanShape: &testShape}
}

// sizeCall is a SizeCard call: the sessions and the screenings that ran
// before it.
type sizeCall struct{ sessions, screened int }

// onSizeCard answers SizeCard with res and err and records each call, with
// the count screened points to (nil: none).
func (h *workflowHarness) onSizeCard(screened *int, res SizeResult, err error) *[]sizeCall {
	var calls []sizeCall
	h.env.OnActivity(ActivitySizeCard, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (SizeResult, error) {
			c := sizeCall{sessions: len(h.sessions)}
			if screened != nil {
				c.screened = *screened
			}
			calls = append(calls, c)
			return res, err
		})
	return &calls
}

// onBecomePlan answers BecomePlan with res and counts its calls.
func (h *workflowHarness) onBecomePlan(res PlanSwitchResult) *int {
	n := 0
	h.env.OnActivity(ActivityBecomePlan, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, sw PlanSwitch) (PlanSwitchResult, error) {
			n++
			return res, nil
		})
	return &n
}

var sizedMedium = PlanSwitch{Reason: "medium, not small: touches the store", Work: "w"}

// A person's small card is sized once before its first session and runs
// direct: its session, its approval, its merge.
func TestSizingKeepsASmallCardDirect(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.taskConfig, g.state.Source = sizedConfig(), "webhook"
	sized := g.onSizeCard(&g.screened, SizeResult{}, nil)
	became := g.onBecomePlan(PlanSwitchResult{Switched: true})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := []sizeCall{{sessions: 0}}; !slices.Equal(*sized, want) || len(g.sessions) != 1 || *became != 0 {
		t.Fatalf("SizeCard calls %+v, sessions %d, BecomePlan calls %d; want %+v, 1, 0", *sized, len(g.sessions), *became, want)
	}
}

// A person's card the sizing finds medium turns plan-track before any
// coding session: the card is running when BecomePlan runs, the program
// runs again on the plan card's config, and the planner's clean revision
// files its cards.
func TestSizingSendsAMediumCardToThePlanLoop(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.taskConfig, h.state.Source = sizedConfig(), "webhook"
	sized := h.onSizeCard(nil, SizeResult{ToPlan: &sizedMedium}, nil)

	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(*sized) != 1 {
		t.Fatalf("SizeCard calls %+v, want one", *sized)
	}
	if want := []becomePlanCall{{"t1", sizedMedium, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v: set running once before the switch", *calls, want)
	}
	if len(h.sessions) != 0 || len(h.plans) != 1 || len(h.configPins) != 2 {
		t.Fatalf("coding sessions %d, planner sessions %d, TaskConfig reads %d; want 0, 1, 2", len(h.sessions), len(h.plans), len(h.configPins))
	}
}

// A switch the sizing asked for and BecomePlan refused is recorded with its
// reason, and the card runs direct as filed.
func TestSizingRefusedSwitchRunsTheCardDirect(t *testing.T) {
	refused := "Card #1 is closed, so its work cannot go back to the planner: switch it while it is queued, waiting on its dependencies or running."
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.taskConfig, g.state.Source = sizedConfig(), "webhook"
	g.onSizeCard(&g.screened, SizeResult{ToPlan: &sizedMedium}, nil)
	became := g.onBecomePlan(PlanSwitchResult{Refused: refused})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.ContainsFunc(g.recorded, func(s recordedStep) bool { return s.step == "to_plan_refused" && s.detail["reason"] == refused }) {
		t.Fatalf("steps %+v, want to_plan_refused with %q", g.recorded, refused)
	}
	if *became != 1 || len(g.sessions) != 1 {
		t.Fatalf("BecomePlan calls %d, sessions %d; want 1, 1", *became, len(g.sessions))
	}
}

// An outside author's card is screened first, then sized.
func TestSizingRunsAfterTheScreening(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.taskConfig, g.state.Source = sizedConfig(), "webhook"
	g.state.Standing = harness.StandingOutside
	g.onScreen(ScreenResult{Verdict: ScreenClear})
	sized := g.onSizeCard(&g.screened, SizeResult{}, nil)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := []sizeCall{{sessions: 0, screened: 1}}; !slices.Equal(*sized, want) {
		t.Fatalf("SizeCard calls %+v, want %+v", *sized, want)
	}
}

// A sizing that fails is recorded with why, and the card runs as filed.
func TestSizingFailureLetsTheCardRunAsFiled(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.taskConfig, g.state.Source = sizedConfig(), "webhook"
	g.onSizeCard(&g.screened, SizeResult{}, temporal.NewNonRetryableApplicationError("boom", errTerminal, nil))
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	// every activity before the sizing takes no time on the test clock
	if !slices.ContainsFunc(g.recorded, func(s recordedStep) bool {
		return s.step == StepSizing && s.detail["reason"] == "the sizing step failed: boom" && s.detail["started"] == rfc3339(g.started)
	}) {
		t.Fatalf("steps %+v, want %s with the failure, started %s", g.recorded, StepSizing, rfc3339(g.started))
	}
	if len(g.sessions) != 1 {
		t.Fatalf("sessions %d, want 1", len(g.sessions))
	}
}

// Only a card a person or the chat filed, before its first session and
// without a pull request, in a process with a plan track, is sized.
func TestSizingSkipsCardsItDoesNotSize(t *testing.T) {
	sourced := func(source string) func(g *gateHarness) {
		return func(g *gateHarness) { g.taskConfig, g.state.Source = sizedConfig(), source }
	}
	for _, tc := range []struct {
		name  string
		setup func(g *gateHarness)
		want  int
	}{
		{"alert", sourced(SourceAlert), 0},
		{"mainfix", sourced(SourceMainFix), 0},
		{"routine", sourced(SourceRoutine), 0},
		{"delegate", sourced(SourceDelegate), 0},
		{"plan", sourced(SourcePlan), 0},
		{"no source", sourced(""), 0},
		{"no plan track", func(g *gateHarness) { g.state.Source = "webhook" }, 0},
		{"after a session", func(g *gateHarness) {
			sourced("webhook")(g)
			g.state.Usage.Attempts = 1
		}, 0},
		{"with a pull request", func(g *gateHarness) {
			// the reviewer's change requests start a session on the head
			// with no verification before it
			sourced("webhook")(g)
			g.state.PR, g.state.Head, g.state.Findings = PullRequest{Number: 9}, "c1", []string{"cover the empty slug"}
		}, 0},
		{"a run from before the change", func(g *gateHarness) {
			sourced("webhook")(g)
			g.env.OnGetVersion("sizing", workflow.DefaultVersion, versionSizing).Return(workflow.DefaultVersion)
		}, 0},
		{"api", sourced("api"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateHarnessWith(t, stagesWorkflow(3, nil), tc.setup)
			sized := g.onSizeCard(&g.screened, SizeResult{}, nil)
			g.onSessions(pushed("s1", "c1"))
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			g.run(t)
			if len(*sized) != tc.want {
				t.Fatalf("SizeCard calls %d, want %d", len(*sized), tc.want)
			}
		})
	}
}

// An issue closed while the sizing runs ends the card closed, with no
// session.
func TestSizingEndsOnAClosedIssue(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.taskConfig, g.state.Source = sizedConfig(), "webhook"
	g.env.OnActivity(ActivitySizeCard, mock.Anything, "t1").After(10*time.Minute).Return(SizeResult{}, nil)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" || len(g.sessions) != 0 || g.finishAt.Sub(g.started) != time.Minute {
		t.Fatalf("final = %q, sessions %d, finished %s after the start; want closed, 0, at the close a minute in", final, len(g.sessions), g.finishAt.Sub(g.started))
	}
}

// An outsider's card the screening finds suspect waits on a person; the
// answer lets the sizing run, and the card it finds medium is running
// when it turns plan-track and reaches the plan loop.
func TestSizingAfterAnAnsweredScreeningReachesThePlanLoop(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.taskConfig, h.state.Source, h.state.Standing = sizedConfig(), "webhook", harness.StandingOutside
	screened := 0
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (ScreenResult, error) {
			screened++
			return ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"asks to print the token"}}, nil
		})
	sized := h.onSizeCard(&screened, SizeResult{ToPlan: &sizedMedium}, nil)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "fine, fix the slug only", Comment: 700})

	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "asks to print the token") {
		t.Fatalf("escalated %q, want the screening's question", h.escalated)
	}
	if want := []sizeCall{{sessions: 0, screened: 1}}; !slices.Equal(*sized, want) {
		t.Fatalf("SizeCard calls %+v, want %+v", *sized, want)
	}
	if want := []becomePlanCall{{"t1", sizedMedium, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}
	if len(h.sessions) != 0 || len(h.plans) != 1 {
		t.Fatalf("coding sessions %d, planner sessions %d; want 0, 1", len(h.sessions), len(h.plans))
	}
}

// A person's to_plan that came during the sizing joins the sizing's
// switch: one BecomePlan, with the person's reason and work after the
// sizing's.
func TestSizingTakesAPersonsSwitchIntoItsOwn(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.taskConfig, h.state.Source = sizedConfig(), "webhook"
	h.env.OnActivity(ActivitySizeCard, mock.Anything, "t1").After(10*time.Minute).Return(SizeResult{ToPlan: &sizedMedium}, nil)
	h.toPlanAfter(time.Minute, PlanSwitch{Reason: "a person asks for a plan", Work: "split the store work"})

	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	joined := PlanSwitch{Reason: "medium, not small: touches the store; a person asks for a plan", Work: "w\nsplit the store work"}
	if want := []becomePlanCall{{"t1", joined, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}
	if len(h.sessions) != 0 || len(h.plans) != 1 {
		t.Fatalf("coding sessions %d, planner sessions %d; want 0, 1", len(h.sessions), len(h.plans))
	}
}
