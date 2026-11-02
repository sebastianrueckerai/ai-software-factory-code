package console

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat waives a criterion on the human's word: their typed line in
// the thread is the decision's authority, the verdict row and the event
// name them, and the request itself stays as it was (the sweep closes
// it).
func TestChatWaivesACriterionOnTheHumansWord(t *testing.T) {
	r, _, g := newChatRig(t)
	verdictRig(r)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	var calls []string
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		calls = append(calls, fmt.Sprintf("%s %s %s by %s: %s", id, cid, verdict, by, reason))
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{
			Criterion: cid, Verdict: verdict, Evidence: reason, Kind: factoryd.CriterionInvariant, Source: "approver", DecidedBy: by})
		return nil
	}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	args := `{"task_id":"t-plan","action":"waive","criterion":"c3","reason":"the timeout is the provider's SLA"}`
	g.replies = [][]string{{call(1, "answer_criteria", args)}, {say("Waived c3.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"waive c3: the timeout is the provider's SLA"}})
	equal(t, "stream", events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"answer_criteria","result":"Criterion c3 waived."}`,
		`applied {"result":"Criterion c3 waived.","tool":"answer_criteria"}`,
		`text {"text":"Waived c3."}`, `done {}`})
	if want := []string{"t-plan c3 waived by sebastian: the timeout is the provider's SLA"}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-plan", IssueNumber: 3,
		Kind: factoryd.EventCriterionDecided, Detail: map[string]any{"approval": int64(30), "criterion": "c3", "verdict": "waived"}})
	if a := r.approvals.get(30); a.Status != "pending" {
		t.Errorf("the waiver decided the request itself: %+v", a)
	}
}

// The chat approves the grant of one more bounded card on the human's
// word: the approval names them, not the chat identity.
func TestChatGrantsTheCriteriaCardOnTheHumansWord(t *testing.T) {
	r, _, g := newChatRig(t)
	verdictRig(r)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	var calls []string
	r.c.cfg.GrantCriteria = func(_ context.Context, id, by string, approval int64) (int, error) {
		calls = append(calls, fmt.Sprintf("grant %s by %s approval %d", id, by, approval))
		return 2, nil
	}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	g.replies = [][]string{{call(1, "answer_criteria", `{"task_id":"t-plan","action":"grant"}`)}, {say("Granted.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"grant the plan one more bounded card"}})
	note := "Granted one more bounded card for 2 criteria; the planner drafts it and the board shows it."
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-plan","action":"grant"},"duration_ms":0,"name":"answer_criteria","result":"` + note + `"}`,
		`applied {"result":"` + note + `","tool":"answer_criteria"}`,
		`text {"text":"Granted."}`, `done {}`})
	if want := []string{"grant t-plan by sebastian approval 30"}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if a := r.approvals.get(30); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "granted: one more bounded card" {
		t.Errorf("the grant left the request %+v", a)
	}
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-plan", IssueNumber: 3,
		Kind: factoryd.EventCriterionDecided, Detail: map[string]any{"approval": int64(30), "criteria": 2, "verb": "granted"}})
}

// On a criteria_rerun ask the chat's rerun starts the review on the
// human's word, and a grant refuses naming the rerun: that ask grants no
// card.
func TestChatRerunsTheCriteriaNeverJudgedOnTheHumansWord(t *testing.T) {
	r, _, g := newChatRig(t)
	verdictRig(r)
	r.approvals.rows[len(r.approvals.rows)-1].Kind = factoryd.ApprovalCriteriaRerun
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	var calls []string
	r.c.cfg.RerunCriteria = func(_ context.Context, id, by string) error {
		calls = append(calls, "rerun "+id+" by "+by)
		return nil
	}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "answer_criteria", `{"task_id":"t-plan","action":"grant"}`)}, {call(2, "answer_criteria", `{"task_id":"t-plan","action":"rerun"}`)}, {say("It runs.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"run the review on them"}})
	refusal := "Refused: Plan t-plan's ask is criteria never judged: its approve runs the model review on them (action rerun), it grants no card."
	note := "The model review runs on the criteria never judged, against the merged feature; the board shows its verdicts, or why it did not run."
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-plan","action":"grant"},"duration_ms":0,"name":"answer_criteria","result":"` + refusal + `"}`,
		`tool {"arguments":{"task_id":"t-plan","action":"rerun"},"duration_ms":0,"name":"answer_criteria","result":"` + note + `"}`,
		`applied {"result":"` + note + `","tool":"answer_criteria"}`,
		`text {"text":"It runs."}`, `done {}`})
	if want := []string{"rerun t-plan by sebastian"}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// The chat confirms the delivered cards on the human's word: their line
// in the thread is the evidence the confirmation records.
func TestChatConfirmsTheDeliveredCriteriaOnTheHumansWord(t *testing.T) {
	r, _, g := newChatRig(t)
	verdictRig(r)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	var calls []string
	r.c.cfg.ConfirmDeliveredCriteria = func(_ context.Context, id, by, line string) (int, error) {
		calls = append(calls, fmt.Sprintf("confirm %s by %s: %s", id, by, line))
		return 2, nil
	}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	line := "the pages read well and no endpoint times out: confirm them"
	g.replies = [][]string{{call(1, "answer_criteria", `{"task_id":"t-plan","action":"confirm"}`)}, {say("Confirmed.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {line}})
	note := "Confirmed 2 criteria met on the delivered cards; the request closes on the next sweep."
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-plan","action":"confirm"},"duration_ms":0,"name":"answer_criteria","result":"` + note + `"}`,
		`applied {"result":"` + note + `","tool":"answer_criteria"}`,
		`text {"text":"Confirmed."}`, `done {}`})
	if want := []string{"confirm t-plan by sebastian: " + line}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-plan", IssueNumber: 3,
		Kind: factoryd.EventCriterionDecided, Detail: map[string]any{"approval": int64(30), "criteria": 2, "verb": "met"}})
}

// A wake's turn acts on no human's word: with no line of theirs in the
// thread, answer_criteria refuses naming the state and the fitting
// action, and nothing is decided.
func TestChatAnswersCriteriaOnlyOnTheHumansLine(t *testing.T) {
	r, chat, g := newChatRig(t)
	verdictRig(r)
	var calls []string
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		calls = append(calls, id+" "+cid+" "+verdict)
		return nil
	}
	g.replies = [][]string{{call(1, "answer_criteria", `{"task_id":"t-plan","action":"waive","criterion":"c3","reason":"not mine to decide"}`)}, {say("I asked them.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "The criteria gap of #3 waits."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || len(rows[1].ToolCalls) != 1 || rows[1].Content != "" || rows[2].Content != "I asked them." {
		t.Fatalf("the wake's rows = %+v", rows)
	}
	want := "Refused: This turn runs on no line the human typed, so their word decides no criteria: ask them to answer here or on the board's card, and answer_criteria runs on their word."
	if got := rows[1].ToolCalls[0].Result; got != want {
		t.Errorf("the wake's tool result = %q\nwant %q", got, want)
	}
	r.tasks.mu.Lock()
	criteria := len(r.tasks.criteria["t-int"])
	r.tasks.mu.Unlock()
	if len(calls) != 0 || criteria != 1 {
		t.Fatalf("a wake without the human's line decided something: calls %v, %d verdict rows", calls, criteria)
	}
}

// A wake over a thread that already holds a line of the human's acts on
// no word of theirs: an older line is not this turn's trigger, so
// answer_criteria refuses and nothing is decided.
func TestChatRefusesTheWakeOverAnOlderHumanLine(t *testing.T) {
	r, chat, g := newChatRig(t)
	verdictRig(r)
	var calls []string
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		calls = append(calls, id+" "+cid+" "+verdict)
		return nil
	}
	ck, s := r.loggedIn(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{say("Noted.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"waive c3: the timeout is the provider's SLA"}, "conversation": {fmt.Sprint(conv)}})
	g.replies = [][]string{{call(1, "answer_criteria", `{"task_id":"t-plan","action":"waive","criterion":"c3","reason":"not mine to decide"}`)}, {say("I asked them.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "The criteria gap of #3 waits."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	var refusal string
	for _, m := range rows {
		for _, c := range m.ToolCalls {
			if c.Name == "answer_criteria" {
				refusal = c.Result
			}
		}
	}
	want := "Refused: This turn runs on no line the human typed, so their word decides no criteria: ask them to answer here or on the board's card, and answer_criteria runs on their word."
	if refusal != want {
		t.Errorf("the wake's tool result = %q\nwant %q", refusal, want)
	}
	r.tasks.mu.Lock()
	criteria := len(r.tasks.criteria["t-int"])
	r.tasks.mu.Unlock()
	if len(calls) != 0 || criteria != 1 {
		t.Fatalf("a wake over an older line of the human's decided something: calls %v, %d verdict rows", calls, criteria)
	}
}
