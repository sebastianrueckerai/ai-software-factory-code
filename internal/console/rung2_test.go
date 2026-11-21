package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// wakeTurnOf is a wake turn of birth on a tree whose premium cap is cap EUR
// and whose chat wakes on wake_model small.
func wakeTurnOf(r *rig, birth string, cap float64) *turn {
	doc := policy.Document{Budget: policy.Budget{PremiumMonthEUR: cap}}
	doc.Prices.MaxOutputTokens = map[string]int{policy.Premium: 8192}
	return &turn{c: r.c, r: httptest.NewRequest(http.MethodGet, "/", nil), tree: policy.Tree{Document: doc},
		cfg: policy.ChatConfig{Model: policy.Big, WakeModel: policy.Small},
		a:   actor{wake: true, birth: birth}}
}

// A wake that names a case runs on big at any premium spend: under, at and
// over the cap, with a premium that has none, and with the ledger unread. A
// watch's wake keeps wake_model, and a member's turn its own model.
func TestACaseWakeRunsOnBigAtAnyPremiumSpend(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		name, birth string
		spent, cap  float64
		want        string
	}{
		{"no spend", "case_climbed", 0, 50, policy.Big},
		{"under the old share", "case_climbed", 42.49, 50, policy.Big},
		{"over the cap", "case_climbed", 80, 50, policy.Big},
		{"no cap of its own", "case_climbed", 1000, 0, policy.Big},
		{"a watch's wake", "wake_card", 0, 50, policy.Small},
		{"a timer's wake", "wake_timer", 49, 50, policy.Small},
		{"the refill", "queue_refill", 0, 50, policy.Small},
	} {
		r.ledger.premium = tc.spent
		if got := wakeTurnOf(r, tc.birth, tc.cap).model(); got != tc.want {
			t.Errorf("%s: the wake runs on %s, want %s", tc.name, got, tc.want)
		}
	}
	r.c.cfg.Costs = nil
	if got := wakeTurnOf(r, "case_climbed", 50).model(); got != policy.Big {
		t.Errorf("no ledger: %s, want big", got)
	}
	member := wakeTurnOf(r, "case_climbed", 50)
	member.a.wake = false
	if got := member.model(); got != policy.Big {
		t.Errorf("a member's turn: %s, want its own model big", got)
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
	if err != nil || attached != 1 || note != "Recorded the analysis on the card #13; the humans read it beside its ask there." {
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

// usage is a stream's final usage chunk with the call's own cost.
func usage(eur float64) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]any{"cost": eur}})
	return string(b)
}

// A real wake turn (StartWake) carries the reads hook: the hand-up gets the
// turn's read tool calls. The turn's cost is the sum of the usage.cost of its
// own model calls, 0.15 and 0.25 EUR, shared evenly over the two cases the
// wake named (0.20 each); the thread's audit total (1.00 EUR before, 2.00
// after) and the chat task's spend (0.70 EUR during the turn) are read by
// nothing.
func TestAWakeTurnRecordsItsReadsAndBooksItsOwnCost(t *testing.T) {
	r, chat, g := newChatRig(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	thread := "sf/code:sebastian:" + strconv.FormatInt(conv, 10)
	chat.mu.Lock()
	chat.totals = map[string][2]float64{thread: {9, 1.0}}
	chat.mu.Unlock()
	var reads []factoryd.CaseRead
	r.c.cfg.AttachAnalysis = func(context.Context, string, string, string, factoryd.Event) error { return nil }
	r.c.cfg.HandUpRung2 = func(_ context.Context, _ string, caseID int64, _ string, _ factoryd.RungTwoRecommendation, got []factoryd.CaseRead) (int64, error) {
		reads = got
		chat.mu.Lock()
		chat.totals = map[string][2]float64{thread: {12, 2.0}}
		chat.mu.Unlock()
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		for i := range r.tasks.tasks {
			if r.tasks.tasks[i].Kind == factoryd.KindChat {
				r.tasks.tasks[i].SpentEUR += 0.7
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
	g.replies = [][]string{{call(1, "card", `{"task_id":"t-m"}`), usage(0.15)},
		{call(2, "attach_analysis", `{"task_id":"t-e","case_id":7,"found":"the replica adds lag"}`), usage(0.25)}, {say("Handed up.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, factoryd.EventCaseClimbed, "Wake: 2 cases wait for the factory agent.", 7, 8); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	if len(reads) != 1 || reads[0].Tool != "card" || reads[0].Args != `{"task_id":"t-m"}` || reads[0].Bytes == 0 {
		t.Fatalf("reads = %+v, want the turn's one read of card", reads)
	}
	if len(costs) != 2 || costs[0].id != 7 || costs[1].id != 8 || costs[0].rung != 2 || costs[0].eur < 0.199 || costs[0].eur > 0.201 || costs[1].eur < 0.199 || costs[1].eur > 0.201 {
		t.Fatalf("costs = %+v, want 0.20 EUR on cases 7 and 8", costs)
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
	if _, err := r.c.callActWork(t.Context(), a, task, 5, factoryd.Resolution{Action: factoryd.ActSuccessor, Note: "x"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	a.decider = "member"
	if _, err := r.c.callActWork(t.Context(), a, task, 5, factoryd.Resolution{Action: factoryd.ActNote, Note: "y"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	reads := []factoryd.CaseRead{{Tool: "card_facts", Args: `{"task_id":"13"}`, Bytes: 10}}
	if len(r.acted) != 2 || r.acted[0].Rung != 2 || !reflect.DeepEqual(r.acted[0].Read, reads) || r.acted[1].Rung != 3 || r.acted[1].Read != nil {
		t.Fatalf("Act calls = %+v, want the agent's with the reads on rung 2 and the human's with none", r.acted)
	}
}

// Only the Waker's answer on a case, and its room for the card's stop,
// skip the chat's confirm list, and Act's autonomy rule judges them. By
// hand: with answer, give_room, retry and set_autonomy on the list, a
// member's turn proposes an answer on a case and a room; so does a wake
// turn that is no Waker's (a refill's resume); the Waker's turns, wake_ask
// and case_climbed, run them as rung 2's, the room on the card's live case
// 5; the Waker's answer with no case proposes, and so does its room on a
// card with no live case, #14, which Act would give on no case at any
// level; the Waker's other tools propose even with a stray case_id.
func TestOnlyTheWakersAnswerOnACaseSkipsTheChatsConfirm(t *testing.T) {
	r, _, _ := newChatRig(t)
	caseCard(r)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 14}})
	_, s := r.loggedIn(t)
	var events []string
	answer := `{"task_id":"13","case_id":5,"text":"go on"}`
	room := `{"task_id":"13","sessions":1,"money":true,"text":"go on"}`
	for _, tc := range []struct {
		name, birth, tool, args string
		wake, propose           bool
	}{
		{"a member's turn", "", "answer", answer, false, true},
		{"a refill's resume", "resume", "answer", answer, true, true},
		{"the Waker's climbed case", "case_climbed", "answer", answer, true, false},
		{"a member's room", "", "give_room", room, false, true},
		{"a refill's room", "resume", "give_room", room, true, true},
		{"the Waker's room", "case_climbed", "give_room", room, true, false},
		{"the Waker's room on a card with no case", "case_climbed", "give_room", `{"task_id":"14","sessions":1,"money":true,"text":"go on"}`, true, true},
		{"the Waker's answer on no case", "case_climbed", "answer", `{"task_id":"13","text":"go on"}`, true, true},
		{"the Waker's retry with a stray case_id", "case_climbed", "retry", `{"task_id":"13","case_id":5}`, true, true},
		{"the Waker's set_autonomy with a stray case_id", "case_climbed", "set_autonomy", `{"autonomy":"autonomous","case_id":5}`, true, true},
	} {
		r.acted = nil
		trn := r.chatTurnOf(t, s, &events)
		trn.cfg.Confirm = []string{"answer", "give_room", "retry", "set_autonomy"}
		trn.a.wake, trn.a.birth = tc.wake, tc.birth
		_, proposal, _ := trn.call(factoryd.OperatorCall{Name: tc.tool, Arguments: tc.args})
		if (proposal != "") != tc.propose {
			t.Errorf("%s: proposal %q, want a proposal: %v", tc.name, proposal, tc.propose)
		}
		if !tc.propose && (len(r.acted) != 1 || r.acted[0].Rung != 2 || r.acted[0].CaseID != 5) {
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
	trn.a.wake, trn.a.birth = true, "case_climbed"
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
			a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: level, wake: true, birth: "case_climbed"}
			_, err := r.c.doAnswerCase(t.Context(), a, "t-ask", 4, "", factoryd.AnswerReplanOnTop+":t-pl", nil)
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
			_, err := r.c.doAnswerCase(t.Context(), a, "t-ask", 4, "", factoryd.AnswerReplanOnTop+":t-pl", nil)
			if (len(*replans) == 1) != tc.runs || (err == nil) != tc.runs {
				t.Fatalf("err %v, replans %v; want the work to run only where Act accepts (%v)", err, *replans, tc.runs)
			}
		})
	}
}

// factoryd's own human card takes no answer while its case climbs, but rung
// 2's own: a human's press is refused naming the case, and nothing is acted
// on or closed; rung 2's choice in its wake on the case goes to its work
// through Act on rung 2, and the card, which has no issue, closes with no
// forge call.
func TestAHeldHumanCardTakesOnlyRungTwosAnswerBeforeItsFace(t *testing.T) {
	r := newRig(t)
	held := stallCard()
	held.Status, held.IssueNumber = "waiting-agent", 0
	r.tasks.tasks = []factoryd.BoardTask{held}
	r.openCases = map[string][]factoryd.Case{"t-stall": {{ID: 51, TaskID: "t-stall", Repository: "sf/code", Kind: factoryd.CaseHumanCard,
		Status: factoryd.CaseOpen, OwnerRung: 2}}}
	_, s := r.loggedIn(t)
	human := actor{s: s, repository: "sf/code", via: factoryd.ViaButton, decider: "member", level: policy.AutonomyAutonomous}
	_, err := r.c.doAnswerCase(t.Context(), human, "t-stall", 51, "", factoryd.AnswerCheckAgain, nil)
	want := "Card “needs you: the chain of sf/code has been pending” waits for the factory agent, which decides it or hands it to you: answer it once it stands in Needs you."
	if err == nil || err.Error() != want || len(r.acted) != 0 || len(r.closed) != 0 {
		t.Fatalf("the human's press = %v, Act calls %+v, closed %v; want %q and nothing done", err, r.acted, r.closed, want)
	}
	if strings.Contains(err.Error(), "t-stall") {
		t.Fatalf("the human's refusal names the task id: %v", err)
	}
	// the chat's agent outside rung 2's turn reads the title and the id its tools take
	chat := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: policy.AutonomyAutonomous}
	_, err = r.c.doAnswerCase(t.Context(), chat, "t-stall", 51, "", factoryd.AnswerCheckAgain, nil)
	want = "Card “needs you: the chain of sf/code has been pending” (t-stall) waits on its case 51, open at rung 2: rung 2 decides it or hands it up, and only then is it the human's, in Needs you."
	if err == nil || err.Error() != want || len(r.acted) != 0 || len(r.closed) != 0 {
		t.Fatalf("the agent's answer = %v, Act calls %+v, closed %v; want %q and nothing done", err, r.acted, r.closed, want)
	}
	agent := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: policy.AutonomyAutonomous, wake: true, birth: factoryd.EventCaseClimbed}
	if _, err := r.c.doAnswerCase(t.Context(), agent, "t-stall", 51, "", factoryd.AnswerCheckAgain, nil); err != nil {
		t.Fatalf("rung 2's answer: %v", err)
	}
	if len(r.acted) != 1 || r.acted[0].Rung != 2 || r.acted[0].Res.Action != factoryd.AnswerCheckAgain || len(r.closed) != 1 ||
		!strings.HasPrefix(r.closed[0], "t-stall by ") || len(r.fg.patched) != 0 {
		t.Fatalf("Act calls %+v, closed %v, patched %v; want rung 2's check_again closing the card with no forge call", r.acted, r.closed, r.fg.patched)
	}
}

// Every model call the turn makes counts, not only the main stream: a wake
// whose first call the provider refuses for its context compacts the thread
// (a second call, a compaction) and answers on a third. The compaction costs
// 0.20 EUR and the answer 0.10 EUR; the case's rung 2 step gets 0.30.
func TestAWakeTurnBooksItsCompactionCallToo(t *testing.T) {
	r, chat, g := newChatRig(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		for _, m := range []factoryd.ChatMessage{{Role: "user", Content: n + "1"}, {Role: "assistant", Content: n + "2"}} {
			m.Conversation = conv
			if _, err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", m); err != nil {
				t.Fatal(err)
			}
		}
	}
	var booked []float64
	r.c.cfg.AddStepCost = func(_ context.Context, id int64, rung int, eur float64) error {
		booked = append(booked, eur)
		return nil
	}
	g.compactCosts = []float64{0.2}
	g.replies = [][]string{{refuseContext}, {say("Looked."), usage(0.1)}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, factoryd.EventCaseClimbed, "Wake: 1 case waits for the factory agent.", 7); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	if len(booked) != 1 || booked[0] < 0.299 || booked[0] > 0.301 {
		t.Fatalf("booked %v, want 0.30 EUR: the compaction's 0.20 and the answer's 0.10", booked)
	}
}

// threadOf is the thread header of inbox conversation conv of login.
func threadOf(login string, conv int64) string {
	return "sf/code:" + login + ":" + strconv.FormatInt(conv, 10)
}

// Two wake turns at once, on two threads, each case gets only its own turn's
// cost: 0.40 EUR for case 7 and 0.10 EUR for case 8.
func TestTwoWakeTurnsAtOnceBookTheirOwnCosts(t *testing.T) {
	r, chat, g := newChatRig(t)
	a, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	b, err := chat.EnsureInbox(t.Context(), "sf/code", "morgan")
	if err != nil {
		t.Fatal(err)
	}
	g.threadReplies = map[string][][]string{
		threadOf("sebastian", a): {{say("A looked."), usage(0.4)}},
		threadOf("morgan", b):    {{say("B looked."), usage(0.1)}},
	}
	var mu sync.Mutex
	booked := map[int64]float64{}
	r.c.cfg.AddStepCost = func(_ context.Context, id int64, _ int, eur float64) error {
		mu.Lock()
		defer mu.Unlock()
		booked[id] += eur
		return nil
	}
	var wg sync.WaitGroup
	for _, w := range []struct {
		login string
		conv  int64
		id    int64
	}{{"sebastian", a, 7}, {"morgan", b, 8}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.c.StartWake(t.Context(), "ai-software-factory-code", w.login, w.conv, factoryd.EventCaseClimbed, "Wake.", w.id); err != nil {
				t.Errorf("StartWake: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(booked) != 2 || booked[7] < 0.399 || booked[7] > 0.401 || booked[8] < 0.099 || booked[8] > 0.101 {
		t.Fatalf("booked %v, want 0.40 on case 7 and 0.10 on case 8", booked)
	}
}

// A wake queued on a thread whose note another turn reads, and a failed
// turn's retry, book on the cases they were woken for: the book of a thread
// holds its wakes' cases and the cost of each turn that ran for them, and the
// last turn settles it.
func TestAThreadsWakeBookSettlesOnTheTurnThatEndsIt(t *testing.T) {
	r, chat, g := newChatRig(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := r.c.projectOf(t.Context(), "ai-software-factory-code")
	booked := map[int64]float64{}
	r.c.cfg.AddStepCost = func(_ context.Context, id int64, _ int, eur float64) error {
		booked[id] += eur
		return nil
	}
	key := threadKey("sf/code", "sebastian", conv)
	r.c.books.add(key, []int64{7}) // the wake of case 7, failed: its retry follows
	r.c.books.add(key, []int64{8}) // a wake queued behind it, whose note the retry reads
	g.compactCosts = []float64{0.3, 0.2}
	spend := func(retryPending bool) {
		trn, err := r.c.wakeTurn(t.Context(), p, "sebastian", conv)
		if err != nil {
			t.Fatal(err)
		}
		trn.retryPending = retryPending
		if _, err := r.c.cfg.Operator.Compact(trn.r.Context(), factoryd.ChatTaskID("ai-software-factory-code", time.Now()), trn.threadRef(), []factoryd.OperatorMessage{{Role: "user", Content: "x"}}); err != nil {
			t.Fatal(err)
		}
		trn.settleCost()
	}
	spend(true)
	if len(booked) != 0 {
		t.Fatalf("booked %v before the retry ran, want nothing", booked)
	}
	spend(false)
	if len(booked) != 2 || booked[7] < 0.249 || booked[7] > 0.251 || booked[8] < 0.249 || booked[8] > 0.251 {
		t.Fatalf("booked %v, want 0.25 on each of cases 7 and 8 (0.30 and 0.20 shared)", booked)
	}
}

// A wake's turn that the refused-rounds stop ends hands its case to the
// human at once, as a rung that did not answer does: the factory agent
// held the case, Act refused its answer five times, and the turn stopped.
// Rung 2's step ends handed up, saying the agent did not answer, the case
// is at the human, and its face says so in the factory's own words.
func TestAWakesStopHandsItsCaseToTheHuman(t *testing.T) {
	ra := newRealAct(t, "t-hq", 51, factoryd.SourceProductionHeld, raHeldQuestion(), policy.AutonomyAutonomous)
	ctx := t.Context()
	// the case as it climbs, before any face
	if _, err := ra.pg.DB().ExecContext(ctx, `UPDATE cases SET status = 'open', owner_rung = 0 WHERE id = $1`, ra.caseID); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(35 * time.Minute)
	if _, err := ra.pg.StartRungStep(ctx, factoryd.CaseStep{CaseID: ra.caseID, Rung: 2, Actor: factoryd.ChatUser, Outcome: factoryd.StepInProgress, DueAt: &due}); err != nil {
		t.Fatal(err)
	}
	if k := ra.caseNow(t); k.Status != factoryd.CaseOpen || k.OwnerRung != 2 {
		t.Fatalf("case = %+v, want open and the factory agent's before the wake", k)
	}

	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{holdProject: {Repository: "sf/code", Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeHuman}}
	r.tasks.tasks = append(r.tasks.tasks, holdAsk())
	r.c.cfg.CaseByID, r.c.cfg.HandUpRung2 = ra.pg.CaseByID, ra.runner.HandUpRung2
	no := factoryd.ErrActRefused(`the note, which the human reads as the grounds of your proposal: The text holds "factory-chat", a name of the factory's own machinery. Say what it means for the reader.`)
	r.actErrs = map[int]error{}
	for i := 1; i <= 6; i++ {
		r.actErrs[i] = no
		g.replies = append(g.replies, []string{call(i, "answer", `{"task_id":"#77","text":"Deploy anyway"}`)})
	}
	conv, err := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(ctx, "ai-software-factory-code", "sebastian", conv, factoryd.EventCaseClimbed, "Wake: 1 case waits for the factory agent.", ra.caseID); err != nil {
		t.Fatalf("StartWake: %v", err)
	}

	g.mu.Lock()
	asked := len(g.requests)
	g.mu.Unlock()
	rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", conv, 20)
	const stopped = "The turn stopped: every action of its last 5 rounds was refused. Send a message to go on."
	if asked != 5 || len(rows) == 0 || rows[len(rows)-1].Content != stopped {
		t.Fatalf("the model was asked %d times and the thread ends %+v; want 5 times and the stop's words", asked, rows[max(len(rows)-1, 0):])
	}
	if k := ra.caseNow(t); k.Status != factoryd.CaseAtHuman || k.OwnerRung != 3 {
		t.Fatalf("case = %+v, want it at the human", k)
	}
	steps, err := ra.pg.CaseSteps(ctx, ra.caseID)
	if err != nil {
		t.Fatal(err)
	}
	const said = "The factory agent did not answer: the factory refused every action it tried."
	own := slices.DeleteFunc(slices.Clone(steps), func(s factoryd.CaseStep) bool { return s.Rung != 2 })
	if len(own) != 1 || own[0].Outcome != factoryd.StepHandedUp || own[0].Found != said {
		t.Fatalf("steps = %+v, want the factory agent's one step handed up as not answered", steps)
	}
	if lead, found := factoryd.StepWords(own[0]); lead != said || found != "" {
		t.Errorf("the step reads %q and %q on the card's life, want %q alone", lead, found, said)
	}
	if info, _ := ra.face(t, "t-hq")["info"].(string); !strings.Contains(info, said) || strings.Contains(info, "found:") {
		t.Errorf("the face's view %q, want it to say %q as the factory's words, no finding", info, said)
	}
	if faults := facefixtures.HumanFaults(said); len(faults) > 0 {
		t.Errorf("%q: %v", said, faults)
	}
}

// A human's turn that the refused rounds stop is not the wake's: the case a
// wake queued behind it named stays with the factory agent, whose own turn
// runs next.
func TestAHumansStopLeavesAQueuedWakesCase(t *testing.T) {
	ra := newRealAct(t, "t-hq", 51, factoryd.SourceProductionHeld, raHeldQuestion(), policy.AutonomyAutonomous)
	ctx := t.Context()
	// the case as it climbs, before any face
	if _, err := ra.pg.DB().ExecContext(ctx, `UPDATE cases SET status = 'open', owner_rung = 0 WHERE id = $1`, ra.caseID); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(35 * time.Minute)
	if _, err := ra.pg.StartRungStep(ctx, factoryd.CaseStep{CaseID: ra.caseID, Rung: 2, Actor: factoryd.ChatUser, Outcome: factoryd.StepInProgress, DueAt: &due}); err != nil {
		t.Fatal(err)
	}

	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	r.ledger.files = map[string]policy.ProjectConfig{holdProject: {Repository: "sf/code", Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeHuman}}
	r.tasks.tasks = append(r.tasks.tasks, holdAsk())
	r.c.cfg.CaseByID, r.c.cfg.HandUpRung2 = ra.pg.CaseByID, ra.runner.HandUpRung2
	no := factoryd.ErrActRefused(`the note, which the human reads as the grounds of your proposal: The text holds "factory-chat", a name of the factory's own machinery. Say what it means for the reader.`)
	r.actErrs = map[int]error{}
	for i := 1; i <= 5; i++ {
		r.actErrs[i] = no
		g.replies = append(g.replies, []string{call(i, "answer", `{"task_id":"#77","text":"Deploy anyway"}`)})
	}
	g.replies = append(g.replies, []string{say("the wake's answer")})
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 6)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if code, _, _, err := postStream(r, ck, s, chatPath, url.Values{"message": {"deploy it"}}); err != nil || code != 200 {
			t.Errorf("the human's turn = %d %v", code, err)
		}
	}()
	<-g.arrived
	wakeDone := make(chan error, 1)
	go func() {
		wakeDone <- r.c.StartWake(ctx, "ai-software-factory-code", "sebastian", 1, factoryd.EventCaseClimbed, "Wake: 1 case waits for the factory agent.", ra.caseID)
	}()
	if !waitFor(t, time.Second, func() bool {
		rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", 1, 10)
		return len(rows) == 2 && rows[1].Queued && rows[1].BirthKind == factoryd.EventCaseClimbed
	}) {
		t.Fatal("the wake's row did not land queued while the human's turn ran")
	}
	close(g.hold)
	if err := <-wakeDone; err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	wg.Wait()

	rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", 1, 40)
	const stopped = "The turn stopped: every action of its last 5 rounds was refused. Send a message to go on."
	var stops, wakes int
	for _, m := range rows {
		if m.Role == "assistant" && m.Content == stopped {
			stops++
		}
		if m.Role == "assistant" && m.Content == "the wake's answer" {
			wakes++
		}
	}
	if stops != 1 || wakes != 1 {
		t.Fatalf("the thread holds %d stops and %d answers of the wake, want one of each: %+v", stops, wakes, rows)
	}
	if k := ra.caseNow(t); k.Status != factoryd.CaseOpen || k.OwnerRung != 2 {
		t.Fatalf("case = %+v, want it open with the factory agent", k)
	}
}
