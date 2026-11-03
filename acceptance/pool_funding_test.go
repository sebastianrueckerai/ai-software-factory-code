package acceptance

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The pool's funding state, accepted end to end: the agent's own raise
// spends the pool a confirmed Set loaded, one card_funded row the
// aggregate reads; the member's confirmed raise is their money and meets
// no pool bound while the card's own state still limits it; a raise the
// bounds refuse changes nothing and falls back to attach_analysis, whose
// findings the ask carries to the human; and every readout — the project
// face, the settings, the policy and the chat — reads the one projection
// of this repository's UTC-month rows, never a model-spend total, an
// error never a zero.

// fundRig answers the rig with anna — writer and approver of the code
// project — logged in, the pool's file on infra's main, no pool in force,
// and card t-e waiting on a human over its budget of EUR 2 with its
// session floor at 2.
func fundRig(t *testing.T) (*AcceptanceRig, string, *http.Cookie, string) {
	t.Helper()
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.LoginAs(t, "anna")
	r.PutInfraFile(accInfraFile, accPoolHead)
	r.SetCardBudget("t-e", 2)
	r.SetSessionFloor("t-e", 2)
	return r, path, ck, csrf
}

// seedFundRow records one card_funded row this repository's pool counts:
// the agent decided it via the chat in the UTC month of at.
func seedFundRow(t *testing.T, r *AcceptanceRig, eur float64, at time.Time) {
	t.Helper()
	seedFundRowAs(t, r, "sf/code", "agent", factoryd.ViaChat, eur, at)
}

// seedFundRowAs records one card_funded row exactly as a decoy needs it:
// another repository, a human decider (none at all), another via.
func seedFundRowAs(t *testing.T, r *AcceptanceRig, repository, decider, via string, eur float64, at time.Time) {
	t.Helper()
	e := factoryd.Event{Repository: repository, Kind: factoryd.EventCardFunded, Via: via, At: at,
		Detail: map[string]any{"amount_eur": eur}}
	if decider != "" {
		e.Detail["decider"] = decider
	}
	if _, err := r.Record(t.Context(), e); err != nil {
		t.Fatalf("seed the card_funded row of %s: %v", repository, err)
	}
}

// agentFundRows answers the code project's agent-decided chat card_funded
// rows.
func agentFundRows(r *AcceptanceRig) []factoryd.Event {
	var out []factoryd.Event
	for _, e := range r.Events() {
		if e.Repository == "sf/code" && e.Kind == factoryd.EventCardFunded && e.Via == factoryd.ViaChat &&
			e.Detail["decider"] == "agent" {
			out = append(out, e)
		}
	}
	return out
}

// agentCall has the model call tool with args on the member's next
// message — unconfirmed at this autonomy — and answers the streamed
// events; the model's closing reply follows the call.
func agentCall(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, tool, args, closing string) []string {
	t.Helper()
	r.Reply([]string{Call(1, tool, args)}, []string{Say(closing)})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"the card waits on money"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	return eventsOf(t, body)
}

// proposeFund has the model call fund with args — confirmed at this
// autonomy — and answers the proposal's token.
func proposeFund(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, args string) string {
	t.Helper()
	r.Reply([]string{Call(1, "fund", args)})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"fund the card"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	for _, e := range eventsOf(t, body) {
		if !strings.HasPrefix(e, "proposal ") {
			continue
		}
		var p struct {
			ID   string `json:"id"`
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "proposal ")), &p); err != nil {
			t.Fatalf("the proposal event %q: %v", e, err)
		}
		if p.Tool == "fund" {
			return p.ID
		}
	}
	t.Fatalf("the stream holds no fund proposal:\n%s", body)
	return ""
}

// lastToolRow answers the last tool row the member's chat stored.
func lastToolRow(t *testing.T, r *AcceptanceRig, login, tool string) factoryd.ChatToolCall {
	t.Helper()
	rows := r.ToolCalls("sf/code", login)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Name == tool {
			return rows[i]
		}
	}
	t.Fatalf("the chat stored no %s row: %+v", tool, rows)
	return factoryd.ChatToolCall{}
}

// chatRead runs the model's read of tool and answers its stored row's
// result, whole where the stream truncates it.
func chatRead(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, login, tool string) string {
	t.Helper()
	r.Reply([]string{Call(1, tool, `{}`)}, []string{Say("read")})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"read the pool"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	return lastToolRow(t, r, login, tool).Result
}

// pageGET answers a page's body as the member of ck.
func pageGET(t *testing.T, r *AcceptanceRig, ck *http.Cookie, path string) string {
	t.Helper()
	rec := r.request(http.MethodGet, path, nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestPoolAuthorisesFunding is the pool's own authorisation chain: anna,
// member and approver, confirms the model's Set 10 → 12 with her reason —
// exactly one complete store-timestamped pool_changed row names her, the
// set and both pools, and the pull request — and the agent's next turn
// raises the waiting card by EUR 1 from the pool without a click: the
// card holds 3, the month's use 4 of 12, EUR 8 left, one agent-decided
// card_funded row. A raise the member confirms is their money: the pool
// does not count it, while the card's own state still refuses a raise it
// needs no raise for. The raise's own money is judged first — a store
// that refuses the amount, or the audit row it must carry, moves neither
// the card nor the spine.
func TestPoolAuthorisesFunding(t *testing.T) {
	r, path, ck, csrf := fundRig(t)
	r.SetLoadedPool(10)
	now := time.Now().UTC()
	for _, eur := range []float64{1, 1, 1} { // the month's use stands at 3
		seedFundRow(t, r, eur, now)
	}

	// anna confirms the model's Set 10 → 12 with the exact reason
	args := `{"action":"set","eur":12,"reason":"the review findings cost money"}`
	token := proposePool(t, r, path, ck, csrf, args)
	r.OnReload(func() { r.SetLoadedPool(12) })
	answer := "The pool of " + accProjectName + " is €12.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it."
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the Do it", body, args, answer)
	if c := r.InfraCommits(); len(c) != 1 || !strings.Contains(c[0].Message, "Requested-by: anna\n") {
		t.Fatalf("the commit = %+v, want anna's own token in its message", r.InfraCommits())
	}
	var changed []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventPoolChanged {
			changed = append(changed, e)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("%d pool_changed rows, want the one: %+v", len(changed), changed)
	}
	e := changed[0]
	equal(t, "the row", []any{e.Actor, e.Via, e.Repository, e.At.IsZero()},
		[]any{"anna", factoryd.ViaChat, "sf/code", false})
	equal(t, "the row's detail", e.Detail, map[string]any{"action": "set", "from_eur": 10.0,
		"confirmed_eur": 12.0, "to_eur": 12.0, "reason": "the review findings cost money", "pr": "sf/infra#101"})

	// the agent's turn.call fund of EUR 1: no human click
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	fund := `{"task_id":"t-e","eur":1,"text":"the replica is worth it"}`
	equal(t, "the unattended fund", agentCall(t, r, path, ck, csrf, "fund", fund, "raised it"), []string{
		`tool {"arguments":` + fund + `,"duration_ms":0,"name":"fund","result":"Raised #13's budget by 1.00 EUR to 3.00 and posted your note as the answer; the card resumes."}`,
		`applied {"result":"Raised #13's budget by 1.00 EUR to 3.00 and posted your note as the answer; the card resumes.","tool":"fund"}`,
		`text {"text":"raised it"}`, `done {}`})
	card, _, _ := r.Task(t.Context(), "t-e")
	equal(t, "the card's budget", card.BudgetEUR, 3.0)
	rows := agentFundRows(r)
	if len(rows) != 4 || rows[3].Actor != "anna" || rows[3].TaskID != "t-e" {
		t.Fatalf("the agent rows = %+v, want the raise's own row by anna on t-e", rows)
	}
	equal(t, "the raise's row", []any{rows[3].Detail["decider"], rows[3].Detail["amount_eur"], rows[3].Detail["from_eur"], rows[3].Detail["to_eur"]},
		[]any{"agent", 1.0, 2.0, 3.0})
	used, err := r.AgentFundedEUR(t.Context(), "sf/code", now)
	if err != nil || used != 4 {
		t.Fatalf("the month's use = %v, %v; want 4 of the pool's 12, EUR 8 left", used, err)
	}

	// the member's confirmed raise is their money: the pool keeps 4 of 12
	r.SetLoadedAutonomy(policy.AutonomyManual)
	theirFund := `{"task_id":"t-e","eur":2,"text":"my call"}`
	needsNone := proposeFund(t, r, path, ck, csrf, theirFund)
	r.Reply([]string{Say("the card needs no raise")}) // the refused press's follow-up turn
	_, body = pressPoolRaw(t, r, path, ck, csrf, needsNone, "")
	if !strings.Contains(body, "Refused: Card #13's remaining budget can fund a session's opening calls: it needs no raise; answer it (answer).") {
		t.Fatalf("the member's raise of a card that needs none = %s", body)
	}
	if card, _, _ = r.Task(t.Context(), "t-e"); card.BudgetEUR != 3 {
		t.Fatalf("the refused raise moved the budget to %v", card.BudgetEUR)
	}
	r.SetSessionFloor("t-e", 4) // the card waits on a human again, at 3 of its floor 4
	token = proposeFund(t, r, path, ck, csrf, theirFund)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK || !strings.Contains(body, "Raised #13's budget by 2.00 EUR to 5.00") {
		t.Fatalf("the member's Do it = %d:\n%s", code, body)
	}
	if card, _, _ = r.Task(t.Context(), "t-e"); card.BudgetEUR != 5 {
		t.Fatalf("budget_eur = %v, want 5", card.BudgetEUR)
	}
	if used, err = r.AgentFundedEUR(t.Context(), "sf/code", now); err != nil || used != 4 {
		t.Fatalf("the month's use = %v, %v; want 4 still: the member's raise spends no pool", used, err)
	}
	var theirs []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventCardFunded && e.Detail["amount_eur"] == 2.0 {
			theirs = append(theirs, e)
		}
	}
	if len(theirs) != 1 || theirs[0].Detail["decider"] == "agent" {
		t.Fatalf("the member's row = %+v, want one the pool does not count", theirs)
	}

	// the raise and its card_funded row commit as one: an amount the
	// store refuses — no amount above zero, a fraction below a cent — and
	// a spine that refuses the audit row each leave the card and the
	// funding rows exactly as they stand
	held, _, _ := r.Task(t.Context(), "t-e")
	auditRows, spine := len(agentFundRows(r)), len(r.Events())
	raise := factoryd.Event{Repository: "sf/code", Kind: factoryd.EventCardFunded, Via: factoryd.ViaChat,
		Actor: "anna", TaskID: "t-e", Detail: map[string]any{"decider": "agent"}}
	unchanged := func(what string) {
		t.Helper()
		card, _, _ := r.Task(t.Context(), "t-e")
		if card.BudgetEUR != held.BudgetEUR || len(agentFundRows(r)) != auditRows || len(r.Events()) != spine {
			t.Fatalf("%s moved the card to %v or wrote a row (agent rows %d of %d, spine %d of %d)",
				what, card.BudgetEUR, len(agentFundRows(r)), auditRows, len(r.Events()), spine)
		}
	}
	for _, tc := range []struct {
		name string
		eur  float64
		want string
	}{
		{"no amount in EUR above zero", -1, "no amount in EUR above zero"},
		{"a fraction below a cent", 0.001, "no amount in whole cents"},
	} {
		if _, _, err := r.FundCard(t.Context(), "t-e", tc.eur, raise); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("the %s refusal = %v, want it to name %q", tc.name, err, tc.want)
		}
		unchanged("the " + tc.name + " refusal")
	}
	r.SetCardBudget("t-e", 2.005) // a budget itself no whole cents
	if _, _, err := r.FundCard(t.Context(), "t-e", 1, raise); err == nil || !strings.Contains(err.Error(), "no amount in whole cents") {
		t.Fatalf("the raise on a budget no whole cents = %v", err)
	}
	r.SetCardBudget("t-e", held.BudgetEUR)
	unchanged("the refused raise on a budget no whole cents")
	r.FailRecords(errors.New("the spine is down"))
	if _, _, err := r.FundCard(t.Context(), "t-e", 1, raise); err == nil || err.Error() != "the spine is down" {
		t.Fatalf("the refused audit row = %v, want the spine's own refusal", err)
	}
	unchanged("the refused audit row")
	r.FailRecords(nil)

	// the sum PGStore's numeric accepts: a budget of 2.20 raised by 0.10
	// lands at 2.30, the row's own figures from the integer cents of the
	// sum
	r.SetCardBudget("t-e", 2.20)
	from, to, err := r.FundCard(t.Context(), "t-e", 0.10, raise)
	if err != nil || from != 2.2 || to != 2.3 {
		t.Fatalf("the 2.20 + 0.10 raise = %v, %v, %v; want 2.20, 2.30", from, to, err)
	}
	if card, _, _ = r.Task(t.Context(), "t-e"); card.BudgetEUR != 2.3 {
		t.Fatalf("the stored budget = %v, want 2.3", card.BudgetEUR)
	}
	if rows := agentFundRows(r); rows[len(rows)-1].Detail["to_eur"] != 2.3 {
		t.Fatalf("the success row's to_eur = %v, want 2.3", rows[len(rows)-1].Detail["to_eur"])
	}
}

// The rig's pool sum reads the UTC month of an instant in any zone, as
// PGStore's date_trunc does.
func TestPoolFundingRigBounds(t *testing.T) {
	r, _, _, _ := fundRig(t)
	seedFundRow(t, r, 1.5, time.Date(2025, 11, 30, 23, 0, 0, 0, time.UTC))
	cet := time.FixedZone("CET", 3600)
	for _, tc := range []struct {
		at   time.Time
		want float64
	}{
		{time.Date(2025, 12, 1, 0, 30, 0, 0, cet), 1.5}, // 2025-11-30 23:30 UTC
		{time.Date(2025, 12, 1, 1, 30, 0, 0, cet), 0},   // 2025-12-01 00:30 UTC
	} {
		if used, err := r.AgentFundedEUR(t.Context(), "sf/code", tc.at); err != nil || used != tc.want {
			t.Errorf("the use at %s = %v, %v; want %v", tc.at, used, err, tc.want)
		}
	}
}

// TestPoolEscalationFallback is every refusal that hands the raise back
// to the human: a project without a pool, a pool the month has spent and
// a per-raise bound the raise passes each answer their own literal, move
// no budget and write no card_funded row, and the agent's analysis of two
// verified findings lands inside the card's open ask, where the human
// reads it with the question.
func TestPoolEscalationFallback(t *testing.T) {
	r, path, ck, csrf := fundRig(t)
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	if _, err := r.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{
		Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#13 needs you: fund?", TaskID: "t-e"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	raise := `{"task_id":"t-e","eur":1,"text":"the replica is worth it"}`
	for _, tc := range []struct {
		name    string
		pool    func()
		funded  int // the card_funded rows the scenarios seeded so far
		refusal string
	}{
		{"no pool", func() {}, 0,
			"This project sets no pool: discretionary_eur is absent or 0. Record your analysis with attach_analysis; the ask then reaches the human."},
		{"the month's pool spent", func() {
			seedFundRow(t, r, 2.5, now) // EUR 2.50 of a pool of EUR 3
			r.SetLoadedPool(3)
		}, 1,
			"The month's pool is €3.00 (discretionary_eur) and €2.50 is funded: €0.50 left, short of €1.00. Record your analysis with attach_analysis; the ask then reaches the human."},
		{"the per-raise bound", func() {
			r.SetLoadedPool(10)
			r.SetLoadedRaiseMax(0.5)
		}, 1,
			"A raise of €1.00 passes this project's raise_max_eur of €0.50: raise by at most that, or record your analysis with attach_analysis so the human can fund more."},
	} {
		tc.pool()
		equal(t, "the "+tc.name+" refusal", agentCall(t, r, path, ck, csrf, "fund", raise, "the human decides"), []string{
			`tool {"arguments":` + raise + `,"duration_ms":0,"name":"fund","result":"Refused: ` + tc.refusal + `"}`,
			`text {"text":"the human decides"}`, `done {}`})
		equal(t, "the refused call's status", lastToolRow(t, r, "anna", "fund").Status, http.StatusConflict)
		card, _, _ := r.Task(t.Context(), "t-e")
		if card.BudgetEUR != 2 || card.Status != "waiting-human" {
			t.Fatalf("the %s refusal moved the card to %v %v", tc.name, card.BudgetEUR, card.Status)
		}
		var funded, other int
		for _, e := range r.Events() {
			if e.Kind == factoryd.EventCardFunded {
				funded++
			} else {
				other++
			}
		}
		if funded != tc.funded || other != 0 {
			t.Fatalf("after the %s refusal: %d card_funded rows, %d other rows; want %d seeded and nothing written",
				tc.name, funded, other, tc.funded)
		}
	}

	// the analysis of two verified findings, retained inside the ask
	found := "Two verified findings: the replica doubles the p99 latency, and the month pool is spent."
	analysis := `{"task_id":"t-e","found":"` + found + `","recommendation":"Grant one sizing exception"}`
	equal(t, "the attach_analysis turn", agentCall(t, r, path, ck, csrf, "attach_analysis", analysis, "analysis attached"), []string{
		`tool {"arguments":` + analysis + `,"duration_ms":0,"name":"attach_analysis","result":"Recorded the analysis inside the ask of #13; the humans read it with the ask."}`,
		`applied {"result":"Recorded the analysis inside the ask of #13; the humans read it with the ask.","tool":"attach_analysis"}`,
		`text {"text":"analysis attached"}`, `done {}`})
	asks, err := r.ChatAsks(t.Context(), "sf/code", "sebastian")
	if err != nil || len(asks) != 1 {
		t.Fatalf("the asks = %d, %v; want the card's one", len(asks), err)
	}
	equal(t, "the ask with its findings", asks[0].Content,
		"#13 needs you: fund?\nAnalysis: "+found+"\nRecommendation: Grant one sizing exception")
	var notes []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventAnalysisAttached {
			notes = append(notes, e)
		}
	}
	if len(notes) != 1 || notes[0].TaskID != "t-e" || notes[0].Actor != "anna" || notes[0].Via != factoryd.ViaChat {
		t.Fatalf("the analysis rows = %+v, want anna's chat row on t-e", notes)
	}
	equal(t, "the analysis row's detail", notes[0].Detail,
		map[string]any{"found": found, "recommendation": "Grant one sizing exception", "source": "agent"})
	if card, _, _ := r.Task(t.Context(), "t-e"); card.BudgetEUR != 2 {
		t.Fatalf("the analysis moved the budget to %v", card.BudgetEUR)
	}
}

// TestPoolReadouts is the one projection every surface reads: the project
// face, the settings, the policy and the chat agree on the unset pool,
// the configured one's EUR 10, EUR 3 used and EUR 7 left — this
// repository's own UTC-month agent rows alone, never the model-spend
// total — and an aggregate error stays an error, never a zero use. The
// explicit-instant November-to-December fixture turns the month with the
// allowance preserved.
func TestPoolReadouts(t *testing.T) {
	r, path, ck, csrf := poolRig(t)
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	pages := map[string]string{
		"the project face": "/?project=" + accProjectName,
		"the settings":     "/settings",
		"the policy":       "/policy",
	}
	unset := "no pool: discretionary_eur is absent or 0, every agent raise waits for a member"
	if face := pageGET(t, r, ck, pages["the project face"]); strings.Contains(face, "cell pool") {
		t.Fatalf("the face reads a pool where the policy sets none:\n%s", face)
	}
	for name, p := range map[string]string{"the settings": "/settings", "the policy": "/policy"} {
		if body := pageGET(t, r, ck, p); !strings.Contains(body, unset) {
			t.Fatalf("%s reads the unset pool:\n%s", name, body)
		}
	}
	for _, tool := range []string{"settings", "policy"} {
		if read := chatRead(t, r, path, ck, csrf, "sebastian", tool); !strings.Contains(read, unset) {
			t.Fatalf("the chat's %s read of the unset pool:\n%s", tool, read)
		}
	}

	// configured 10, use 3, remaining 7: the rig's own rows alone
	r.SetLoadedPool(10)
	now := time.Now().UTC()
	// the month before this one off its own UTC start: AddDate off the
	// day itself would normalise a March 31 into March 3 and count the
	// decoy toward this month
	lastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	for _, eur := range []float64{1.5, 1, 0.5} {
		seedFundRow(t, r, eur, now)
	}
	seedFundRowAs(t, r, "sf/other", "agent", factoryd.ViaChat, 5, now)  // another repository
	seedFundRowAs(t, r, "sf/code", "", factoryd.ViaChat, 4, now)        // the human's own raise
	seedFundRowAs(t, r, "sf/code", "agent", factoryd.ViaButton, 2, now) // a raise off the chat
	seedFundRow(t, r, 9, lastMonth)                                     // another month
	r.SetProjectSpend(factoryd.ProjectSpend{EUR: 42, ChatEUR: 41})      // the model-spend total, never the use
	ready := "€3.00 of €10.00 funded this UTC month, €7.00 left, each raise at most €1.00; the loaded policy permits an unattended raise from it"
	for name, p := range pages {
		if body := pageGET(t, r, ck, p); !strings.Contains(body, ready) {
			t.Fatalf("%s reads the configured pool:\n%s", name, body)
		}
	}
	if face := pageGET(t, r, ck, pages["the project face"]); !strings.Contains(face, "pool €3.00") ||
		!strings.Contains(face, "/ €10.00") || !strings.Contains(face, "€7.00 left") {
		t.Fatalf("the face's month pool figures:\n%s", face)
	}
	for _, tool := range []string{"settings", "policy"} {
		if read := chatRead(t, r, path, ck, csrf, "sebastian", tool); !strings.Contains(read, ready) {
			t.Fatalf("the chat's %s read of the configured pool:\n%s", tool, read)
		}
	}
	if used, err := r.AgentFundedEUR(t.Context(), "sf/code", now); err != nil || used != 3 {
		t.Fatalf("the aggregate = %v, %v; want 3, the decoys excluded", used, err)
	}

	// the aggregate's error never becomes a zero use
	r.FailPoolReads(errors.New("the spine is down"))
	unreadable := "the pool stands at €10.00 but its usage could not be read: how much is left is unknown, and a standing pool is not unattended authority"
	for name, p := range pages {
		body := pageGET(t, r, ck, p)
		if !strings.Contains(body, unreadable) || strings.Contains(body, "€0.00 left") {
			t.Fatalf("%s reads the failed aggregate:\n%s", name, body)
		}
	}
	if face := pageGET(t, r, ck, pages["the project face"]); !strings.Contains(face, "pool unread") {
		t.Fatalf("the face's unread month pool:\n%s", face)
	}
	if read := chatRead(t, r, path, ck, csrf, "sebastian", "settings"); !strings.Contains(read, unreadable) {
		t.Fatalf("the chat's settings read of the failed aggregate:\n%s", read)
	}
	r.FailPoolReads(nil)

	// readiness explains the confirmation: at manual the raise is the
	// member's money
	r.SetLoadedAutonomy(policy.AutonomyManual)
	confirmed := "€3.00 of €10.00 funded this UTC month, €7.00 left, each raise at most €1.00; fund is confirmed at this autonomy: a raise is the member's money and meets no pool bound"
	for name, p := range pages {
		if body := pageGET(t, r, ck, p); !strings.Contains(body, strings.ReplaceAll(confirmed, "'", "&#39;")) {
			t.Fatalf("%s reads the confirmed fund:\n%s", name, body)
		}
	}

	// the November-to-December rollover, the explicit instant alone: the
	// production read-helper (Console.ProjectPool) carries its instant in
	// its contract, so the fixture reads it at both — allowance from the
	// policy in force, usage the month's own rows, remaining their
	// difference — and the turn of the month preserves the allowance
	// while the use resets and the remaining returns to it
	nov, dec := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC), time.Date(2026, 12, 3, 9, 0, 0, 0, time.UTC)
	r2, _, _ := NewAcceptanceRig(t, false, nil)
	r2.SetLoadedPool(10)
	seedFundRow(t, r2, 1.5, time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC))
	seedFundRow(t, r2, 0.5, time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC))
	v2, ok, err := r2.CurrentPolicy(t.Context())
	if err != nil || !ok {
		t.Fatalf("the policy in force = %v, %v", ok, err)
	}
	p2 := factoryd.Project{Name: accProjectName, Repository: "sf/code"}
	readout := func(at time.Time) []any {
		pl := r2.c.ProjectPool(t.Context(), v2, p2, at)
		return []any{pl.ConfiguredEUR, pl.UsedEUR, pl.RemainingEUR, pl.Month}
	}
	equal(t, "November's readout", readout(nov), []any{10.0, 2.0, 8.0, "2026-11"})
	equal(t, "December's readout", readout(dec), []any{10.0, 0.0, 10.0, "2026-12"})
	if words := r2.c.ProjectPool(t.Context(), v2, p2, dec).Words; !strings.Contains(words, "€10.00 left") {
		t.Fatalf("December's words lack the remaining 10:\n%s", words)
	}
}
