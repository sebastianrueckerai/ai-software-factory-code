package factoryd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type deployCall struct{ repository, by, via, decider string }

type deployStub struct {
	mu    sync.Mutex
	calls []deployCall
	err   error
}

func (d *deployStub) deploy(_ context.Context, repository, by, via, decider string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deployCall{repository, by, via, decider})
	return "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d", d.err
}

// heldRig is the rung rig with a roll-held ask h1 (#40) on demo-app: the
// keeper's question with plan #1580 at 2 of 5 merged, waiting on #1609
// running and #1610 queued. It answers the ask's case id.
func heldRig(t *testing.T, state string) (rungRig, *deployStub, int64) {
	t.Helper()
	ctx := context.Background()
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.atAutonomy(state)
	stub := &deployStub{}
	g.r.DeployNow = stub.deploy
	card := Task{ID: "h1", Kind: "human", Status: "waiting-human", Source: SourceProductionHeld, Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 40, Track: policy.TrackDirect}
	if err := g.s.CreateTask(ctx, card); err != nil {
		t.Fatal(err)
	}
	q := HumanQuestion{Ask: "Production of demo-app has waited 6 hours: plan #1580 is open on main. Wait, deploy anyway, or replan?", Why: "held", Info: "plans",
		Choices: []QuestionChoice{{AnswerHoldWait, "Wait"}, {ActDeployNow, "Deploy anyway"}}}
	facts := map[string]any{"held_since": "2026-10-29T10:00:00Z", "plans": []HoldPlan{{Issue: 1580, Merged: 2, Total: 5,
		WaitsOn: []HoldWait{{Issue: 1609, Status: "running"}, {Issue: 1610, Status: "queued"}}}}}
	if err := recordAsk(ctx, g.s, g.r.timeline, card, q, "the production hold", SourceProductionHeld, ViaFactoryd, "", facts); err != nil {
		t.Fatal(err)
	}
	c, err := g.s.NewestAskCase(ctx, "h1")
	if err != nil || c.ID == 0 {
		t.Fatalf("NewestAskCase = %+v, %v", c, err)
	}
	return g, stub, c.ID
}

func (g rungRig) deployAct(rung int, by string, caseID int64) (ActResult, error) {
	via := ViaChat
	if rung == 3 {
		via = ViaConsole
	}
	return g.r.Act(context.Background(), ActInput{TaskID: "h1", CaseID: caseID, By: by, Via: via, Rung: rung, Res: Resolution{Action: ActDeployNow, Note: "Deploy anyway"}})
}

// The held roll's case is roll_held and its facts hold the plans' states as
// the keeper wrote them.
func TestRollHeldCaseHoldsThePlansStates(t *testing.T) {
	g, _, id := heldRig(t, policy.AutonomyFullAuto)
	c := caseByID(t, g.s, id)
	plans, _ := c.Facts["plans"].([]any)
	if c.Kind != CaseRollHeld || len(plans) != 1 {
		t.Fatalf("case %s with %d plans, want roll_held with one", c.Kind, len(plans))
	}
	p := plans[0].(map[string]any)
	waits := p["waits_on"].([]any)
	if p["issue"] != float64(1580) || p["merged"] != float64(2) || p["total"] != float64(5) || len(waits) != 2 ||
		waits[0].(map[string]any)["issue"] != float64(1609) || waits[0].(map[string]any)["status"] != "running" ||
		waits[1].(map[string]any)["issue"] != float64(1610) || waits[1].(map[string]any)["status"] != "queued" || c.Facts["held_since"] != "2026-10-29T10:00:00Z" {
		t.Errorf("plan facts %v, held_since %v", p, c.Facts["held_since"])
	}
}

// At autonomous and full auto rung 2's deploy_now runs once as the agent
// and resolves the case on rung 2; at assisted and manual no release runs
// and the proposal stands on the face.
func TestRung2DeployNowRunsOrProposesByLevel(t *testing.T) {
	for _, tc := range []struct {
		state string
		runs  bool
	}{{policy.AutonomyFullAuto, true}, {policy.AutonomyAutonomous, true}, {policy.AutonomyAssisted, false}, {policy.AutonomyManual, false}} {
		t.Run(tc.state, func(t *testing.T) {
			g, stub, id := heldRig(t, tc.state)
			out, err := g.deployAct(2, "sebastian", id)
			if err != nil {
				t.Fatalf("Act: %v", err)
			}
			c := caseByID(t, g.s, id)
			if !tc.runs {
				if len(stub.calls) != 0 || c.Status != CaseAtHuman || !strings.HasPrefix(out.Text, "Proposed deploy_now on #40") {
					t.Errorf("calls %+v, case %s, text %q; want none, at the human, a proposal", stub.calls, c.Status, out.Text)
				}
				return
			}
			if len(stub.calls) != 1 || stub.calls[0] != (deployCall{"demo-app/demo-app", "sebastian", ViaChat, "agent"}) {
				t.Fatalf("DeployNow calls %+v, want one as the agent", stub.calls)
			}
			if c.Status != CaseResolved || c.Resolution == nil || c.Resolution.Rung != 2 || c.Resolution.By != "sebastian" {
				t.Errorf("case %s, resolution %+v; want resolved on rung 2 by sebastian", c.Status, c.Resolution)
			}
			if want := "Deploy now: main of demo-app/demo-app at 9d9d9d9 goes to production."; !strings.Contains(out.Text, want) {
				t.Errorf("text %q lacks %q", out.Text, want)
			}
		})
	}
}

// The human's press goes through the same action with decider member.
func TestHumanDeployNowRunsTheSameAction(t *testing.T) {
	g, stub, id := heldRig(t, policy.AutonomyManual)
	if _, err := g.deployAct(3, "sebastian", id); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != (deployCall{"demo-app/demo-app", "sebastian", ViaConsole, "member"}) {
		t.Fatalf("DeployNow calls %+v, want one as the member", stub.calls)
	}
	if c := caseByID(t, g.s, id); c.Status != CaseResolved || c.Resolution == nil || c.Resolution.Rung != 3 {
		t.Errorf("case %s, resolution %+v; want resolved on rung 3", c.Status, c.Resolution)
	}
}

// deploy_now on any case that is no roll hold's is refused with the
// table's words, on the card's case as on an agent card's.
func TestDeployNowIsRefusedOnOtherCases(t *testing.T) {
	g, stub, _ := heldRig(t, policy.AutonomyFullAuto)
	out := g.trip(t, tripCap)
	g.settle()
	const want = "deploy_now releases a held production, and this case is no roll hold's"
	_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActDeployNow}})
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Act on a session cap = %v, want %q", err, want)
	}
	if len(stub.calls) != 0 {
		t.Errorf("DeployNow ran %d times", len(stub.calls))
	}
}

// A refusal of DeployNow reaches the face masked: URL userinfo and a
// bearer value of the error never show, and the case stays open.
func TestDeployNowFailureIsMasked(t *testing.T) {
	g, stub, id := heldRig(t, policy.AutonomyFullAuto)
	stub.err = errors.New("forgejo: " + leaky)
	_, err := g.deployAct(2, "sebastian", id)
	if err == nil || !strings.Contains(err.Error(), "could not be released") {
		t.Fatalf("Act = %v, want a refusal that the release failed", err)
	}
	for _, secret := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the refusal holds %q: %v", secret, err)
		}
	}
	if c := caseByID(t, g.s, id); c.Status != CaseAtHuman {
		t.Errorf("case %s, want still at the human", c.Status)
	}
}
