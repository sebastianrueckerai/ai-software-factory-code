package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

const heldRepo = "demo-app/demo-app"

// pgKeeperStore is the PG store with the project of heldRepo, which the
// keeper files its ask under.
type pgKeeperStore struct{ *PGStore }

func (s pgKeeperStore) ProjectByRepository(_ context.Context, repository string) (Project, bool, error) {
	return seedProject(repository), true, nil
}

// heldRig is the rung rig whose store holds plan #1580 open on demo-app's
// main: #1581 merged at 10:00, #1602 done, and #1609 running,
// #1610 queued and #1611 failed unmerged, so 2 of 5 cards are merged and
// the plan waits on three. The release keeper, at 16:00 with the hold since
// 10:00, files the ask; caseID is its roll-held case.
type heldRig struct {
	rungRig
	stub   *deployStub
	keeper *ReleaseKeeper
	clock  *time.Time
	forge  *chainForgejo
	caseID int64
	cardID string
}

func heldAt(h, m int) time.Time { return time.Date(2026, 10, 29, h, m, 0, 0, time.UTC) }

// openHeldPlan opens plan id (issue) on heldRepo's main with one card merged at merged.
func (g rungRig) openHeldPlan(t *testing.T, id string, issue int64, merged time.Time, others map[int64]string) {
	t.Helper()
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Objective: "x", Repository: heldRepo, IssueNumber: issue, Track: policy.TrackPlan}); err != nil {
		t.Fatal(err)
	}
	card := func(n int64, status string) string {
		cid := fmt.Sprintf("%s-%d", id, n)
		if err := g.s.CreateTask(ctx, Task{ID: cid, Kind: "agent", Status: status, Objective: "x", Repository: heldRepo, IssueNumber: n, Track: policy.TrackDirect,
			Source: SourcePlan, Parent: id, PlanKey: cid}); err != nil {
			t.Fatal(err)
		}
		return cid
	}
	cid := card(issue+1, "merged")
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET merge_commit = 'm1', finished_at = $2 WHERE id = $1`, cid, merged); err != nil {
		t.Fatal(err)
	}
	for n, status := range others {
		card(n, status)
	}
}

func newHeldRig(t *testing.T, state string) heldRig {
	t.Helper()
	ctx := context.Background()
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.atAutonomy(state)
	if _, err := g.s.db.ExecContext(ctx, `DELETE FROM releases`); err != nil {
		t.Fatal(err)
	}
	h := heldRig{rungRig: g, stub: &deployStub{}, forge: &chainForgejo{}}
	g.r.DeployNow = h.stub.deploy
	g.openHeldPlan(t, "p1580", 1580, heldAt(10, 0), map[int64]string{1602: "done", 1609: "running", 1610: "queued", 1611: "failed"})
	if err := g.s.RecordReleaseAsk(ctx, heldRepo, "c0c0c0c", false, heldAt(9, 0)); err != nil {
		t.Fatal(err)
	}
	now := heldAt(16, 0)
	own, _ := SplitRepo(codeRepo)
	h.clock = &now
	h.keeper = &ReleaseKeeper{Store: pgKeeperStore{g.s}, Forgejo: h.forge.srv(t), Timeline: g.r.timeline, Own: own, Now: func() time.Time { return now }}
	if err := h.keeper.Pass(ctx); err != nil {
		t.Fatalf("the keeper's pass: %v", err)
	}
	card, ok, err := g.s.ChainCard(ctx, heldRepo, SourceProductionHeld)
	if err != nil || !ok {
		t.Fatalf("the keeper filed no ask: %v", err)
	}
	c, err := g.s.NewestAskCase(ctx, card.ID)
	if err != nil || c.ID == 0 {
		t.Fatalf("NewestAskCase = %+v, %v", c, err)
	}
	h.caseID, h.cardID = c.ID, card.ID
	g.r.forgejo = h.forge.srv(t)
	return h
}

func (h heldRig) deployAct(rung int, by string) (ActResult, error) {
	via := ViaChat
	if rung == 3 {
		via = ViaConsole
	}
	return h.r.Act(context.Background(), ActInput{TaskID: h.cardID, CaseID: h.caseID, By: by, Via: via, Rung: rung, Res: Resolution{Action: ActDeployNow, Note: "Deploy anyway"}})
}

// The keeper files the ask with the plan's states as the case's facts. By
// hand from the rig: #1581 merged and #1602 done make 2 merged, the other
// three cards make 5 in all, and the plan waits on #1609 running, #1610
// queued and #1611 failed, in issue order. The hold began with the first
// merge at 10:00. The case offers the ask's own choices.
func TestRollHeldCaseHoldsThePlansStates(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	c := caseByID(t, h.s, h.caseID)
	got, err := json.Marshal(c.Facts["plans"])
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"issue":1580,"merged":2,"total":5,"waits_on":[{"issue":1609,"status":"running"},{"issue":1610,"status":"queued"},{"issue":1611,"status":"failed"}]}]`
	if c.Kind != CaseRollHeld || string(got) != want || c.Facts["held_since"] != "2026-10-29T10:00:00Z" {
		t.Errorf("case %s, plans %s, held_since %v; want roll_held, %s, 2026-10-29T10:00:00Z", c.Kind, got, c.Facts["held_since"], want)
	}
	if c.Facts["options"] != nil {
		t.Errorf("the case holds a copy of the ask's options: %v", c.Facts["options"])
	}
}

// A plan that joins the hold while the ask stands is named on the case at
// the next pass: plan #1590 merged its first card at 15:40, so it opens
// second by first merge, with 1 of 1 merged and no wait.
func TestRollHeldFactsFollowTheHold(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	h.openHeldPlan(t, "p1590", 1590, heldAt(15, 40), nil)
	*h.clock = heldAt(16, 10)
	if err := h.keeper.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(caseByID(t, h.s, h.caseID).Facts["plans"])
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"issue":1580,"merged":2,"total":5,"waits_on":[{"issue":1609,"status":"running"},{"issue":1610,"status":"queued"},{"issue":1611,"status":"failed"}]},{"issue":1590,"merged":1,"total":1,"waits_on":[]}]`
	if string(got) != want {
		t.Errorf("plans %s, want %s", got, want)
	}
	c := caseByID(t, h.s, h.caseID)
	if c.Facts["held_since"] != "2026-10-29T10:00:00Z" || c.Facts["source"] != SourceProductionHeld {
		t.Errorf("held_since %v, source %v after the refresh; want the hold's start and the ask's own keys kept", c.Facts["held_since"], c.Facts["source"])
	}
	q, err := h.r.askOf(context.Background(), h.cardID)
	if err != nil || q == nil || !strings.Contains(q.Info, "- plan #1590: 1 of 1 cards merged; waits for the plan to close.") {
		t.Errorf("the ask's info %+v, %v; want plan #1590 named", q, err)
	}
	events, err := h.s.TaskEvents(context.Background(), h.cardID, 100)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, e := range events {
		if info, _ := e.Detail["info"].(string); e.Kind == EventQuestionAsked && strings.Contains(info, "- plan #1590") {
			named = true
		}
	}
	if !named {
		t.Errorf("no question_asked row names plan #1590 in its info")
	}
}

// A case that ended between the keeper's read and its write is no error of
// the pass, and the store refuses a write on an ended case.
func TestRollHeldRefreshMeetsAnEndedCase(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	ctx := context.Background()
	if err := h.s.WithdrawCase(ctx, h.caseID, "ended"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.SetCaseFacts(ctx, h.caseID, map[string]any{"held_since": "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetCaseFacts on an ended case = %v, want ErrNotFound", err)
	}
	if c := caseByID(t, h.s, h.caseID); c.Facts["held_since"] != "2026-10-29T10:00:00Z" {
		t.Errorf("an ended case's held_since is %v, want it untouched", c.Facts["held_since"])
	}
	lost := raceStore{pgKeeperStore{h.s}}
	h.keeper.Store = lost
	*h.clock = heldAt(16, 10)
	if err := h.keeper.Pass(ctx); err != nil {
		t.Errorf("a pass that lost the case to its end = %v, want nil", err)
	}
}

// raceStore reads the ask's case live and finds it ended at the write.
type raceStore struct{ pgKeeperStore }

func (raceStore) NewestAskCase(context.Context, string) (Case, error) { return Case{ID: 1}, nil }

func (raceStore) SetCaseFacts(context.Context, int64, map[string]any) error {
	return fmt.Errorf("live case 1: %w", ErrNotFound)
}

// Rung 2's statement shows the plans' states and the hold's start, and
// offers the card's own options, not the agent card's table.
func TestRollHeldStatementShowsThePlansAndTheOptions(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	text, err := h.r.RungTwoStatement(context.Background(), h.caseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Production has been held since 2026-10-29T10:00:00Z.",
		"Plan #1580: 2 of 5 cards merged; waits on #1609 running, #1610 queued, #1611 failed.",
		"- hold_wait: Wait", "- deploy_now: Deploy anyway"} {
		if !strings.Contains(text, want) {
			t.Errorf("the statement lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "raise_ceiling") {
		t.Errorf("the statement lists the agent card's table:\n%s", text)
	}
}

// At autonomous and full auto rung 2's deploy_now runs once as the agent
// and resolves the case on rung 2; at assisted and manual no release runs
// and rung 2's step hands the case up.
func TestRung2DeployNowRunsOrProposesByLevel(t *testing.T) {
	for _, tc := range []struct {
		state string
		runs  bool
	}{{policy.AutonomyFullAuto, true}, {policy.AutonomyAutonomous, true}, {policy.AutonomyAssisted, false}, {policy.AutonomyManual, false}} {
		t.Run(tc.state, func(t *testing.T) {
			h := newHeldRig(t, tc.state)
			out, err := h.deployAct(2, "sebastian")
			if err != nil {
				t.Fatalf("Act: %v", err)
			}
			c := caseByID(t, h.s, h.caseID)
			if !tc.runs {
				if len(h.stub.calls) != 0 || c.Status != CaseAtHuman || !strings.HasPrefix(out.Text, "Proposed deploy_now on ") {
					t.Errorf("calls %+v, case %s, text %q; want none, at the human, a proposal", h.stub.calls, c.Status, out.Text)
				}
				events, err := h.s.TaskEvents(context.Background(), h.cardID, 100)
				if err != nil {
					t.Fatal(err)
				}
				var asked []Event
				for _, e := range events {
					if e.Kind == EventQuestionAsked {
						asked = append(asked, e)
					}
				}
				if len(asked) != 1 {
					t.Fatalf("%d question_asked rows on the ask, want the keeper's one", len(asked))
				}
				if pre, _ := asked[0].Detail["preselected"].(map[string]any); pre["action"] != ActDeployNow {
					t.Errorf("preselected %v, want deploy_now", asked[0].Detail["preselected"])
				}
				if len(h.forge.comments) != 1 || !strings.Contains(h.forge.comments[0], "The factory agent proposes deploy_now") {
					t.Errorf("comments %q, want one naming the proposal", h.forge.comments)
				}
				return
			}
			if len(h.stub.calls) != 1 || h.stub.calls[0] != (deployCall{heldRepo, "sebastian", ViaChat, "agent"}) {
				t.Fatalf("DeployNow calls %+v, want one as the agent", h.stub.calls)
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
	h := newHeldRig(t, policy.AutonomyManual)
	if _, err := h.deployAct(3, "sebastian"); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(h.stub.calls) != 1 || h.stub.calls[0] != (deployCall{heldRepo, "sebastian", ViaConsole, "member"}) {
		t.Fatalf("DeployNow calls %+v, want one as the member", h.stub.calls)
	}
	if c := caseByID(t, h.s, h.caseID); c.Status != CaseResolved || c.Resolution == nil || c.Resolution.Rung != 3 {
		t.Errorf("case %s, resolution %+v; want resolved on rung 3", c.Status, c.Resolution)
	}
}

const notRollHeld = "deploy_now releases a held production, and this case is no roll hold's"

// deploy_now on a case that is no roll hold's is refused with the table's
// words: on an agent card's session cap, and on a human card of another
// source whose question offers it, at rung 2, at rung 3 and at assisted,
// where the refusal comes before any proposal.
func TestDeployNowIsRefusedOnOtherCases(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	out := h.trip(t, tripCap)
	h.settle()
	_, err := h.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActDeployNow}})
	if err == nil || !strings.HasPrefix(err.Error(), notRollHeld) {
		t.Fatalf("Act on a session cap = %v, want %q", err, notRollHeld)
	}
	ctx := context.Background()
	for i, state := range []string{policy.AutonomyFullAuto, policy.AutonomyAssisted} {
		h.atAutonomy(state)
		card := Task{ID: "h-" + state, Kind: "human", Status: "waiting-human", Source: SourceStillRed, Objective: "x", Repository: heldRepo, IssueNumber: int64(50 + i), Track: policy.TrackDirect}
		if err := h.s.CreateTask(ctx, card); err != nil {
			t.Fatal(err)
		}
		q := HumanQuestion{Ask: "Main of " + state + " is red. Deploy anyway?", Why: "red", Info: "x", Choices: []QuestionChoice{{ActDeployNow, "Deploy anyway"}}}
		if err := recordAsk(ctx, h.s, h.r.timeline, card, q, "factoryd", SourceStillRed, ViaFactoryd, "", nil); err != nil {
			t.Fatal(err)
		}
		c, err := h.s.NewestAskCase(ctx, card.ID)
		if err != nil || c.Kind != CaseRedMain {
			t.Fatalf("case %+v, %v; want a red_main case", c, err)
		}
		for _, rung := range []int{2, 3} {
			_, err := h.r.Act(ctx, ActInput{TaskID: card.ID, CaseID: c.ID, By: "sebastian", Via: ViaChat, Rung: rung, Res: Resolution{Action: ActDeployNow}})
			if err == nil || !strings.HasPrefix(err.Error(), notRollHeld) {
				t.Errorf("%s, rung %d: Act = %v, want %q", state, rung, err, notRollHeld)
			}
		}
	}
	if len(h.stub.calls) != 0 {
		t.Errorf("DeployNow ran %d times", len(h.stub.calls))
	}
}

// No action that runs or draws is taken on a case whose card ended, at any
// rung and level: the human's close of the ask means Wait, and the case
// waits for the heal pass to withdraw it. At assisted the refusal comes
// before the proposal, so no face is touched.
func TestNoAnswerRunsOnAnEndedCard(t *testing.T) {
	for _, state := range []string{policy.AutonomyFullAuto, policy.AutonomyAssisted} {
		for _, status := range []string{"done", "closed"} {
			for _, rung := range []int{2, 3} {
				t.Run(fmt.Sprintf("%s %s rung %d", state, status, rung), func(t *testing.T) {
					h := newHeldRig(t, state)
					if _, err := h.s.db.ExecContext(context.Background(), `UPDATE tasks SET status = $2, finished_at = now() WHERE id = $1`, h.cardID, status); err != nil {
						t.Fatal(err)
					}
					_, err := h.deployAct(rung, "sebastian")
					if want := "ended " + status + ", so its case takes no deploy_now"; err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("Act = %v, want a refusal with %q", err, want)
					}
					if len(h.stub.calls) != 0 {
						t.Errorf("DeployNow ran %d times", len(h.stub.calls))
					}
					if c := caseByID(t, h.s, h.caseID); c.Status != CaseAtHuman {
						t.Errorf("case %s, want still at the human", c.Status)
					}
					events, _ := h.s.TaskEvents(context.Background(), h.cardID, 100)
					for _, e := range events {
						if e.Detail["preselected"] != nil {
							t.Errorf("a face carries a proposal on an ended card: %v", e.Detail)
						}
					}
				})
			}
		}
	}
}

// The ended-card rule holds on the non-choice path too: rung 2's replan on a
// card a human closed reopens nothing, and the human's close of the held ask
// after it ended still resolves the case.
func TestEndedCardTakesNoReplanButStillCloses(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	ctx := context.Background()
	out := h.trip(t, tripCap)
	h.settle()
	if _, err := h.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'closed', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	_, err := h.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActReplan}})
	if want := "ended closed, so its case takes no replan"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("replan on a closed card = %v, want %q", err, want)
	}
	if card, _, _ := h.s.Task(ctx, "t1"); card.Status != "closed" {
		t.Errorf("card %s after the refused replan, want closed", card.Status)
	}

	if _, err := h.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = $1`, h.cardID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Act(ctx, ActInput{TaskID: h.cardID, CaseID: h.caseID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActClose}}); err != nil {
		t.Fatalf("the human's close of an ended held ask = %v, want it taken", err)
	}
	if c := caseByID(t, h.s, h.caseID); c.Status != CaseResolved {
		t.Errorf("case %s after the close, want resolved", c.Status)
	}
}

// A fund beside deploy_now is refused with the choice card's own words, not
// with words about a pool or a ceiling the card does not have.
func TestDeployNowWithAFundIsRefusedByTheChoiceCard(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyFullAuto)
	for _, rung := range []int{2, 3} {
		_, err := h.r.Act(context.Background(), ActInput{TaskID: h.cardID, CaseID: h.caseID, By: "sebastian", Via: ViaChat, Rung: rung,
			Res: Resolution{Action: ActDeployNow, Fund: &policy.Draw{EUR: 1}}})
		if want := "is a human card, so its answer draws nothing: answer it without the fund"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rung %d: Act = %v, want %q", rung, err, want)
		}
	}
	if len(h.stub.calls) != 0 {
		t.Errorf("DeployNow ran %d times", len(h.stub.calls))
	}
}

// A failure of DeployNow reaches the face in words that name the state, with
// the secrets of an unknown error masked: URL userinfo and a bearer value.
// The case stays open in each.
func TestDeployNowFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not held", ErrNotHeld, "production of demo-app/demo-app is not held: every merge goes to production on its own"},
		{"main red", ErrMainRed{Commit: "abcdef0123456789"}, "main of demo-app/demo-app at abcdef0 is red: deploy_now waits for a green run on main"},
		{"unknown", errors.New("forgejo: " + leaky), "production of demo-app/demo-app could not be released: forgejo: see https://[redacted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHeldRig(t, policy.AutonomyFullAuto)
			h.stub.err = tc.err
			_, err := h.deployAct(2, "sebastian")
			if err == nil || !strings.HasPrefix(err.Error(), strings.Split(tc.want, "[redacted]")[0]) {
				t.Fatalf("Act = %v, want a refusal starting %q", err, tc.want)
			}
			if tc.name != "unknown" && err.Error() != tc.want {
				t.Errorf("Act = %q, want %q", err, tc.want)
			}
			for _, secret := range []string{"user:pass", "abc123def456"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal holds %q: %v", secret, err)
				}
			}
			if c := caseByID(t, h.s, h.caseID); c.Status != CaseAtHuman {
				t.Errorf("case %s, want still at the human", c.Status)
			}
		})
	}
}
