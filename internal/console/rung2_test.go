package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// failingCosts is a Costs whose enforced spend cannot be read.
type failingCosts struct{ *fakeLedger }

func (failingCosts) Enforced(context.Context, time.Time) (policy.Enforced, error) {
	return policy.Enforced{}, errors.New("the ledger is down")
}

// wakeTurnOf is a wake turn of birth on a tree whose premium cap is cap EUR
// and whose chat wakes on wake_model small.
func wakeTurnOf(r *rig, birth string, cap float64, premiumLimit bool) *turn {
	doc := policy.Document{Budget: policy.Budget{PremiumMonthEUR: cap}}
	if premiumLimit {
		doc.Prices.MaxOutputTokens = map[string]int{policy.Premium: 8192}
	}
	return &turn{c: r.c, r: httptest.NewRequest(http.MethodGet, "/", nil), tree: policy.Tree{Document: doc},
		cfg: policy.ChatConfig{Model: policy.Big, WakeModel: policy.Small},
		a:   actor{wake: true, birth: birth}}
}

// A wake that names a case runs on premium while the month's premium spend
// is under 85 percent of premium's cap, and on big from then. By hand: 85
// percent of 50.00 EUR is 42.50, so 42.49 is premium and 42.50, the
// threshold, is big. A spend that cannot be read, and a premium with no
// output limit, run on big. A watch's wake keeps wake_model, and a cap of 0
// (premium has none of its own) leaves premium always.
func TestACaseWakeRunsOnPremiumUntilItsShareIsSpent(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		name, birth string
		spent, cap  float64
		limit       bool
		want        string
	}{
		{"under the share", "wake_ask", 42.49, 50, true, policy.Premium},
		{"at the share", "wake_ask", 42.50, 50, true, policy.Big},
		{"over the share", "case_climbed", 49, 50, true, policy.Big},
		{"a climbed case under the share", "case_climbed", 0, 50, true, policy.Premium},
		{"a watch's wake", "wake_card", 0, 50, true, policy.Small},
		{"a timer's wake", "wake_timer", 49, 50, true, policy.Small},
		{"the refill", "queue_refill", 0, 50, true, policy.Small},
		{"no cap of its own", "wake_ask", 1000, 0, true, policy.Premium},
		{"no output limit", "wake_ask", 0, 50, false, policy.Big},
	} {
		r.ledger.premium = tc.spent
		if got := wakeTurnOf(r, tc.birth, tc.cap, tc.limit).model(); got != tc.want {
			t.Errorf("%s: the wake runs on %s, want %s", tc.name, got, tc.want)
		}
	}
	r.c.cfg.Costs = failingCosts{r.ledger}
	if got := wakeTurnOf(r, "wake_ask", 50, true).model(); got != policy.Big {
		t.Errorf("a spend that cannot be read: %s, want big", got)
	}
	r.c.cfg.Costs = nil
	if got := wakeTurnOf(r, "wake_ask", 50, true).model(); got != policy.Big {
		t.Errorf("no ledger: %s, want big", got)
	}
	member := wakeTurnOf(r, "wake_ask", 50, true)
	member.a.wake = false
	if got := member.model(); got != policy.Big {
		t.Errorf("a member's turn: %s, want its own model", got)
	}
}

// The analysis of a case rung 2 holds hands it up: the seam gets the card,
// the case, the found text, the typed recommendation with its grounds and the
// turn's reads, and the tool answers that the card waits at its face. A
// refusal comes back as a 409 with its words; a case rung 2 does not hold
// records the analysis in the ask as before.
func TestAttachAnalysisHandsUpACaseRungTwoHolds(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.noteRead("card_facts", `{"task_id":"13"}`, 42)
	tool, _ := r.c.chatTool("attach_analysis")

	type handed struct {
		task   string
		caseID int64
		found  string
		rec    factoryd.RungTwoRecommendation
		reads  []factoryd.CaseRead
	}
	var got []handed
	own := true
	var handErr error
	r.c.cfg.HandUpRung2 = func(_ context.Context, task string, caseID int64, found string, rec factoryd.RungTwoRecommendation, reads []factoryd.CaseRead) (int64, error) {
		got = append(got, handed{task, caseID, found, rec, reads})
		if own {
			return 7, handErr
		}
		return 0, handErr
	}
	var attached int
	r.c.cfg.AttachAnalysis = func(context.Context, string, string, string, factoryd.Event) error { attached++; return nil }

	in := toolInput{TaskID: "13", CaseID: 7, Found: "The pull request merged.", Recommendation: "nothing is left",
		Recommended: &recommended{Action: "close", Args: map[string]any{"why": "done"}}}
	if err := trn.byTaskID(&in); err != nil {
		t.Fatal(err)
	}
	note, _, err := tool.run(trn, in)
	want := handed{"t-e", 7, "The pull request merged.", factoryd.RungTwoRecommendation{Action: "close", Grounds: "nothing is left", Args: map[string]any{"why": "done"}},
		[]factoryd.CaseRead{{Tool: "card_facts", Args: `{"task_id":"13"}`, Bytes: 42}}}
	if err != nil || note != "Handed #13 to the human with your analysis and recommendation; the card waits at its face." || !reflect.DeepEqual(got, []handed{want}) || attached != 0 {
		t.Fatalf("note %q, err %v, handed %+v, attached %d; want the hand-up %+v and no ask analysis", note, err, got, attached, want)
	}

	handErr = factoryd.ErrActRefused(`"withdraw" is no action the human may take: recommend close`)
	_, _, err = tool.run(trn, in)
	if r := refusedWith(err); r == nil || r.status != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 refusal", err)
	}

	own, handErr = false, nil
	note, _, err = tool.run(trn, toolInput{TaskID: "t-e", Found: "found"})
	if err != nil || attached != 1 || note != "Recorded the analysis inside the ask of #13; the humans read it with the ask." {
		t.Fatalf("note %q, err %v, attached %d; want the old behaviour on a case no rung 2 holds", note, err, attached)
	}
}

func refusedWith(err error) *refusal {
	var no *refusal
	if errors.As(err, &no) {
		return no
	}
	return nil
}

// A real wake turn (StartWake) carries the reads hook: the hand-up gets the
// turn's read tool calls. When the turn ends, the thread's own audit total
// over the turn, 0.40 EUR, is shared evenly over the two cases the wake
// named (0.20 each) whether or not the turn ended them; a concurrent spend of
// 0.30 EUR on the chat task, which other turns and rung 1 share, counts for
// nothing.
func TestAWakeTurnRecordsItsReadsAndBooksItsOwnCost(t *testing.T) {
	r, chat, g := newChatRig(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	var reads []factoryd.CaseRead
	r.c.cfg.AttachAnalysis = func(context.Context, string, string, string, factoryd.Event) error { return nil }
	r.c.cfg.HandUpRung2 = func(_ context.Context, _ string, caseID int64, _ string, _ factoryd.RungTwoRecommendation, got []factoryd.CaseRead) (int64, error) {
		reads = got
		thread := "sf/code:sebastian:" + strconv.FormatInt(conv, 10)
		chat.mu.Lock()
		chat.totals = map[string][2]float64{thread: {3, 0.4}}
		chat.mu.Unlock()
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		for i := range r.tasks.tasks {
			if r.tasks.tasks[i].Kind == factoryd.KindChat {
				r.tasks.tasks[i].SpentEUR += 0.3 + 0.4
			}
		}
		return caseID, nil
	}
	type cost struct {
		id   int64
		rung int
		eur  float64
	}
	var costs []cost
	r.c.cfg.AddStepCost = func(_ context.Context, id int64, rung int, eur float64) error {
		costs = append(costs, cost{id, rung, eur})
		return nil
	}
	g.replies = [][]string{{call(1, "card", `{"task_id":"t-m"}`)}, {call(2, "attach_analysis", `{"task_id":"t-e","case_id":7,"found":"the replica adds lag"}`)}, {say("Handed up.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, factoryd.EventCaseClimbed, "Wake: 2 cases wait for the factory agent.", 7, 8); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	if len(reads) != 1 || reads[0].Tool != "card" || reads[0].Args != `{"task_id":"t-m"}` || reads[0].Bytes == 0 {
		t.Fatalf("reads = %+v, want the turn's one read of card", reads)
	}
	want := []cost{{7, 2, 0.2}, {8, 2, 0.2}}
	if len(costs) != 2 || costs[0].id != 7 || costs[1].id != 8 || costs[0].rung != 2 || costs[0].eur < 0.199 || costs[0].eur > 0.201 || costs[1].eur < 0.199 || costs[1].eur > 0.201 {
		t.Fatalf("costs = %+v, want %+v", costs, want)
	}
}

// A rung 2 answer in a wake turn (built as StartWake builds it) records the
// turn's reads on its step: Act gets them with the answer and the turn notes
// the case it ended. A human's answer carries none.
func TestRungTwoAnswerCarriesTheWakeTurnsReads(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	p, ok, err := r.c.projectOf(t.Context(), "ai-software-factory-code")
	if err != nil || !ok {
		t.Fatalf("project: %v %v", ok, err)
	}
	trn, err := r.c.wakeTurn(t.Context(), p, "sebastian", 1)
	if err != nil {
		t.Fatal(err)
	}
	task, _, _ := r.c.cfg.Tasks.Task(t.Context(), "t-e")
	trn.noteRead("card_facts", `{"task_id":"13"}`, 10)
	a := trn.a
	a.decider = "agent"
	if _, err := r.c.callAct(t.Context(), a, task, 5, factoryd.Resolution{Action: factoryd.ActSuccessor, Note: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	a.decider = "member"
	if _, err := r.c.callAct(t.Context(), a, task, 5, factoryd.Resolution{Action: factoryd.ActNote, Note: "y"}, nil); err != nil {
		t.Fatal(err)
	}
	reads := []factoryd.CaseRead{{Tool: "card_facts", Args: `{"task_id":"13"}`, Bytes: 10}}
	if len(r.acted) != 2 || r.acted[0].Rung != 2 || !reflect.DeepEqual(r.acted[0].Read, reads) || r.acted[1].Rung != 3 || r.acted[1].Read != nil {
		t.Fatalf("Act calls = %+v, want the agent's with the reads on rung 2 and the human's with none", r.acted)
	}
}

// Only the Waker's answer on a case skips the chat's confirm list, and Act's
// autonomy rule judges it. By hand: with answer, retry and set_autonomy on
// the list, a member's turn proposes an answer on a case; so does a wake
// turn that is no Waker's (a refill's resume); the Waker's turns, wake_ask
// and case_climbed, run it as rung 2's; the Waker's answer with no case
// proposes; the Waker's other tools proposes even with a stray case_id.
func TestOnlyTheWakersAnswerOnACaseSkipsTheChatsConfirm(t *testing.T) {
	r, _, _ := newChatRig(t)
	caseCard(r)
	_, s := r.loggedIn(t)
	var events []string
	answer := `{"task_id":"13","case_id":5,"text":"go on"}`
	for _, tc := range []struct {
		name, birth, tool, args string
		wake, propose           bool
	}{
		{"a member's turn", "", "answer", answer, false, true},
		{"a refill's resume", "resume", "answer", answer, true, true},
		{"the Waker's ask", "wake_ask", "answer", answer, true, false},
		{"the Waker's climbed case", "case_climbed", "answer", answer, true, false},
		{"the Waker's answer on no case", "wake_ask", "answer", `{"task_id":"13","text":"go on"}`, true, true},
		{"the Waker's retry with a stray case_id", "wake_ask", "retry", `{"task_id":"13","case_id":5}`, true, true},
		{"the Waker's set_autonomy with a stray case_id", "wake_ask", "set_autonomy", `{"autonomy":"autonomous","case_id":5}`, true, true},
	} {
		r.acted = nil
		trn := r.chatTurnOf(t, s, &events)
		trn.cfg.Confirm = []string{"answer", "retry", "set_autonomy"}
		trn.a.wake, trn.a.birth = tc.wake, tc.birth
		_, proposal, _ := trn.call(factoryd.OperatorCall{Name: tc.tool, Arguments: tc.args})
		if (proposal != "") != tc.propose {
			t.Errorf("%s: proposal %q, want a proposal: %v", tc.name, proposal, tc.propose)
		}
		if tc.tool == "answer" && !tc.propose && (len(r.acted) != 1 || r.acted[0].Rung != 2 || r.acted[0].CaseID != 5) {
			t.Errorf("%s: Act calls %+v, want one on case 5 at rung 2", tc.name, r.acted)
		}
	}
}

// When Act proposed, nothing was applied: the turn sends no applied event
// and the result carries no undo sentence.
func TestAProposalIsNoAppliedWrite(t *testing.T) {
	r, _, _ := newChatRig(t)
	caseCard(r)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.a.wake, trn.a.birth = true, "wake_ask"
	for _, proposes := range []bool{true, false} {
		r.actProposes = proposes
		events = nil
		result, _, _ := trn.call(factoryd.OperatorCall{Name: "answer", Arguments: `{"task_id":"13","case_id":5,"text":"go on"}`})
		applied := slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, "applied ") })
		if applied == proposes || strings.Contains(result, "cannot be undone") == proposes {
			t.Errorf("proposed %v: result %q, applied event %v; want an applied write only for what ran", proposes, result, applied)
		}
	}
}

// A wake turn's choice on a human card, at a level where Act proposes, runs
// none of the choice's work: no replan, no close, and Act hears it on
// rung 2. At full auto the work runs.
func TestAWakeTurnsChoiceOnAHumanCardRunsNoWorkWhereActProposes(t *testing.T) {
	for _, level := range []string{policy.AutonomyAssisted, policy.AutonomyFullAuto} {
		t.Run(level, func(t *testing.T) {
			r, _, _, replans := holdAskRig(t)
			r.openCases = map[string][]factoryd.Case{"t-ask": {{ID: 4, TaskID: "t-ask", Kind: factoryd.CaseRollHeld, Status: factoryd.CaseAtHuman}}}
			r.actProposes = level == policy.AutonomyAssisted
			_, s := r.loggedIn(t)
			a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: level, wake: true, birth: "wake_ask"}
			_, err := r.c.doAnswerCase(t.Context(), a, "t-ask", 4, "", factoryd.AnswerReplanOnTop+":t-pl", nil, 0)
			if level == policy.AutonomyAssisted {
				if err != nil || len(*replans) != 0 || len(r.acted) != 1 || r.acted[0].Rung != 2 || r.acted[0].Res.Action != factoryd.AnswerReplanOnTop+":t-pl" {
					t.Fatalf("err %v, replans %v, Act calls %+v; want no replan and one proposal on rung 2", err, *replans, r.acted)
				}
				return
			}
			if len(*replans) != 1 {
				t.Fatalf("err %v, replans %v; want the replan to run at full auto", err, *replans)
			}
		})
	}
}

// The work of a human card's choice runs inside Act, after Act accepted the
// answer. By hand: where Act refuses the answer or finds the case ended,
// at full auto for rung 2 and for a human alike, no replan runs and the
// issue stays open; the caller reads the refusal. Where Act accepts, the
// work runs once.
func TestAHumanCardChoiceRunsItsWorkOnlyWhereActAccepts(t *testing.T) {
	ended := factoryd.ErrCaseEnded{Case: factoryd.Case{ID: 4, Status: factoryd.CaseResolved}}
	for _, tc := range []struct {
		name    string
		level   string
		decider string
		actErr  error
		runs    bool
	}{
		{"rung 2 at full auto, Act refuses", policy.AutonomyFullAuto, "agent", factoryd.ErrActRefused("the question offers no such choice"), false},
		{"rung 2 at autonomous, Act refuses", policy.AutonomyAutonomous, "agent", factoryd.ErrActRefused("the question offers no such choice"), false},
		{"rung 2 at full auto, the case ended", policy.AutonomyFullAuto, "agent", ended, false},
		{"a human, Act refuses", policy.AutonomyManual, "member", factoryd.ErrActRefused("the question offers no such choice"), false},
		{"a human, the case ended", policy.AutonomyManual, "member", ended, false},
		{"rung 2 at full auto, Act accepts", policy.AutonomyFullAuto, "agent", nil, true},
		{"a human, Act accepts", policy.AutonomyManual, "member", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, replans := holdAskRig(t)
			r.openCases = map[string][]factoryd.Case{"t-ask": {{ID: 4, TaskID: "t-ask", Kind: factoryd.CaseRollHeld, Status: factoryd.CaseAtHuman}}}
			r.actErr = tc.actErr
			_, s := r.loggedIn(t)
			a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: tc.decider, level: tc.level}
			_, err := r.c.doAnswerCase(t.Context(), a, "t-ask", 4, "", factoryd.AnswerReplanOnTop+":t-pl", nil, 0)
			if (len(*replans) == 1) != tc.runs || (err == nil) != tc.runs {
				t.Fatalf("err %v, replans %v; want the work to run only where Act accepts (%v)", err, *replans, tc.runs)
			}
		})
	}
}
