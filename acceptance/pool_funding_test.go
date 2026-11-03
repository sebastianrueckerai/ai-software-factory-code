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

// The escalation pool's funding, accepted end to end: the agent's own
// fund and the human's confirmed one both answer the card's live case
// through Act, the agent's on rung 2 and the human's on rung 3, and both
// are pool draws the month's line counts; a draw Act refuses changes
// nothing and falls back to attach_analysis, whose findings the ask
// carries to the human; and every readout (the project face, the
// settings, the policy and the chat) reads the one projection of this
// repository's UTC-month pool draws in three parts, never a model-spend
// total, an error never a zero.

// fundRig answers the rig with anna, writer and approver of the code
// project, logged in, the pool's file on infra's main, no pool in force,
// and card t-e waiting on a human over its budget of EUR 2.
func fundRig(t *testing.T) (*AcceptanceRig, string, *http.Cookie, string) {
	t.Helper()
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.LoginAs(t, "anna")
	r.PutInfraFile(accInfraFile, accPoolHead)
	r.SetCardBudget("t-e", 2)
	return r, path, ck, csrf
}

// seedFundRow records one pool draw of eur on this repository's card
// t-e, as Act's draw lands it, in the UTC month of at.
func seedFundRow(t *testing.T, r *AcceptanceRig, eur float64, at time.Time) {
	t.Helper()
	seedRow(t, r, "sf/code", factoryd.EventCardFunded, map[string]any{"pool": true, "rung": 2, "amount_eur": eur}, at)
}

// seedRow records one row of kind with detail on repository's card t-e
// in the UTC month of at.
func seedRow(t *testing.T, r *AcceptanceRig, repository, kind string, detail map[string]any, at time.Time) {
	t.Helper()
	if _, err := r.Record(t.Context(), factoryd.Event{Repository: repository, TaskID: "t-e", Kind: kind,
		Via: factoryd.ViaChat, At: at, Detail: detail}); err != nil {
		t.Fatalf("seed the %s row of %s: %v", kind, repository, err)
	}
}

// lastAct is the last answer Act took, the fields a fund carries.
func lastAct(t *testing.T, r *AcceptanceRig) []any {
	t.Helper()
	acted := r.Acted()
	if len(acted) == 0 {
		t.Fatalf("Act took no answer")
	}
	in := acted[len(acted)-1]
	return []any{in.TaskID, in.CaseID, in.By, in.Via, in.Rung, in.Res.Action, in.Res.Args["eur"], in.Res.Note}
}

// agentCall has the model call tool with args on the human's next
// message, unconfirmed at this autonomy, and answers the streamed
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

// TestPoolAuthorisesFunding is the pool's chain from its change to its
// draws: anna, writer and approver, confirms the model's set of 10 to 12
// with her reason, and exactly one store-timestamped pool_changed row
// names her, the set, both pools and the pull request. The agent's next
// turn funds the waiting card 1 EUR without a click: one answer to the
// card's live case through Act on rung 2, the card at 3 EUR and the
// month's draws at 4 of 12. Anna's own confirmed fund of 2 EUR is the
// same answer on rung 3, a pool draw too: every rung draws from the pool,
// and only the human's draw may pass it.
func TestPoolAuthorisesFunding(t *testing.T) {
	r, path, ck, csrf := fundRig(t)
	r.SetLoadedPool(10)
	now := time.Now().UTC()
	for _, eur := range []float64{1, 1, 1} { // the month's draws stand at 3
		seedFundRow(t, r, eur, now)
	}

	// anna confirms the model's set of 10 to 12 with the exact reason
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
	changed := poolChangedRows(r)
	if len(changed) != 1 {
		t.Fatalf("%d pool_changed rows, want the one: %+v", len(changed), changed)
	}
	e := changed[0]
	equal(t, "the row", []any{e.Actor, e.Via, e.Repository, e.At.IsZero()},
		[]any{"anna", factoryd.ViaChat, "sf/code", false})
	equal(t, "the row's detail", e.Detail, map[string]any{"action": "set", "from_eur": 10.0,
		"confirmed_eur": 12.0, "to_eur": 12.0, "reason": "the review findings cost money", "pr": "sf/infra#101"})

	// the agent's fund of 1 EUR: no human click, the card's live case
	// answered on rung 2
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	fund := `{"task_id":"t-e","eur":1,"text":"the replica is worth it"}`
	equal(t, "the unattended fund", agentCall(t, r, path, ck, csrf, "fund", fund, "funded it"), []string{
		`tool {"arguments":` + fund + `,"duration_ms":0,"name":"fund","result":"Case 1 took fund."}`,
		`applied {"result":"Case 1 took fund.","tool":"fund"}`,
		`text {"text":"funded it"}`, `done {}`})
	equal(t, "the agent's answer", lastAct(t, r),
		[]any{"t-e", int64(1), "anna", factoryd.ViaChat, 2, factoryd.ActFund, 1.0, "the replica is worth it"})
	card, _, _ := r.Task(t.Context(), "t-e")
	equal(t, "the card's budget", card.BudgetEUR, 3.0)
	equal(t, "the pool line after the agent's fund", poolLineOf(t, r, now), []any{12.0, 4.0, true, false,
		"0 of 30 sessions, 0 h of 30 h agent time and €4.00 of €12.00 drawn this UTC month"})

	// anna's confirmed fund of 2 EUR: the same answer on rung 3, a pool
	// draw as well
	r.SetLoadedAutonomy(policy.AutonomyManual)
	theirFund := `{"task_id":"t-e","eur":2,"text":"my call"}`
	token = proposeFund(t, r, path, ck, csrf, theirFund)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK || !strings.Contains(body, "Case 1 took fund.") {
		t.Fatalf("anna's Do it = %d:\n%s", code, body)
	}
	equal(t, "anna's answer", lastAct(t, r), []any{"t-e", int64(1), "anna", factoryd.ViaChat, 3, factoryd.ActFund, 2.0, "my call"})
	card, _, _ = r.Task(t.Context(), "t-e")
	equal(t, "the card's budget", card.BudgetEUR, 5.0)
	if drawn, err := r.PoolDrawn(t.Context(), "sf/code", now); err != nil || drawn != (policy.Draw{EUR: 6}) {
		t.Fatalf("the month's draws = %+v, %v; want 6 EUR of the pool's 12", drawn, err)
	}
}

// The rig's pool draws read the UTC month of an instant in any zone, as
// PGStore's date_trunc does.
func TestPoolDrawnRigMonth(t *testing.T) {
	r, _, _, _ := fundRig(t)
	seedFundRow(t, r, 1.5, time.Date(2025, 11, 30, 23, 0, 0, 0, time.UTC))
	cet := time.FixedZone("CET", 3600)
	for _, tc := range []struct {
		at   time.Time
		want policy.Draw
	}{
		{time.Date(2025, 12, 1, 0, 30, 0, 0, cet), policy.Draw{EUR: 1.5}}, // 2025-11-30 23:30 UTC
		{time.Date(2025, 12, 1, 1, 30, 0, 0, cet), policy.Draw{}},         // 2025-12-01 00:30 UTC
	} {
		if drawn, err := r.PoolDrawn(t.Context(), "sf/code", tc.at); err != nil || drawn != tc.want {
			t.Errorf("the draws at %s = %+v, %v; want %+v", tc.at, drawn, err, tc.want)
		}
	}
}

// TestPoolEscalationFallback is the refusal that hands the money back to
// the human: the agent's fund past the month's pool, which the store
// refuses through Act, reaches the agent whole as a 409, moves no budget
// and writes no row, and the agent's analysis of two verified findings
// lands inside the card's open ask, where the human reads it with the
// question.
func TestPoolEscalationFallback(t *testing.T) {
	r, path, ck, csrf := fundRig(t)
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	if _, err := r.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{
		Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#13 needs you: fund?", TaskID: "t-e"}); err != nil {
		t.Fatal(err)
	}
	const spent = "the escalation pool leaves 30 sessions, 1800 minutes of agent time and 0.50 EUR this month: only the human draws more"
	if got := (&factoryd.DrawBoundError{Left: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 0.5}}).Error(); got != spent {
		t.Fatalf("the store's refusal reads %q, want %q", got, spent)
	}
	r.RefuseActs(spent)
	raise := `{"task_id":"t-e","eur":1,"text":"the replica is worth it"}`
	equal(t, "the refused fund", agentCall(t, r, path, ck, csrf, "fund", raise, "the human decides"), []string{
		`tool {"arguments":` + raise + `,"duration_ms":0,"name":"fund","result":"Refused: ` + spent + `"}`,
		`text {"text":"the human decides"}`, `done {}`})
	equal(t, "the refused call's status", lastToolRow(t, r, "anna", "fund").Status, http.StatusConflict)
	equal(t, "the refused answer", lastAct(t, r), []any{"t-e", int64(1), "anna", factoryd.ViaChat, 2, factoryd.ActFund, 1.0, "the replica is worth it"})
	card, _, _ := r.Task(t.Context(), "t-e")
	if card.BudgetEUR != 2 || card.Status != "waiting-human" {
		t.Fatalf("the refusal moved the card to %v %v", card.BudgetEUR, card.Status)
	}
	if rows := r.Events(); len(rows) != 0 {
		t.Fatalf("the refusal wrote %+v", rows)
	}
	r.RefuseActs("")

	// the analysis of two verified findings, retained inside the ask
	found := "Two verified findings: the replica doubles the p99 latency, and the escalation pool is spent."
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
// face, the settings, the policy and the chat agree on the policy's pool
// while the file sets none, then on the file's EUR 10 with this month's
// pool draws of this repository in three parts (2 sessions, 1 h 20 min
// of agent time and EUR 3), never the model-spend total, and an
// unreadable read stays unreadable, never a zero. The explicit-instant
// November-to-December fixture turns the month with the pool kept.
func TestPoolReadouts(t *testing.T) {
	r, path, ck, csrf := poolRig(t)
	pages := map[string]string{
		"the project face": "/?project=" + accProjectName,
		"the settings":     "/settings",
		"the policy":       "/policy",
	}
	readAll := func(what, want string) {
		t.Helper()
		for name, p := range pages {
			if body := pageGET(t, r, ck, p); !strings.Contains(body, want) {
				t.Fatalf("%s lacks %s %q:\n%s", name, what, want, body)
			}
		}
	}
	policys := "0 of 30 sessions, 0 h of 30 h agent time and €0.00 of €60.00 drawn this UTC month"
	readAll("the policy's pool", policys)
	for _, tool := range []string{"settings", "policy"} {
		if read := chatRead(t, r, path, ck, csrf, "sebastian", tool); !strings.Contains(read, policys) {
			t.Fatalf("the chat's %s read of the policy's pool:\n%s", tool, read)
		}
	}

	// the file's 10 and this month's pool draws of this repository alone
	r.SetLoadedPool(10)
	now := time.Now().UTC()
	// the month before this one off its own UTC start: AddDate off the
	// day itself would normalise a March 31 into March 3 and count the
	// decoy toward this month
	lastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	for _, eur := range []float64{1.5, 1, 0.5} {
		seedFundRow(t, r, eur, now)
	}
	seedRow(t, r, "sf/code", factoryd.EventCardExtended,
		map[string]any{"pool": true, "rung": 3, "sessions": 2, "agent_time_s": 4800, "amount_eur": 0.0}, now)
	seedRow(t, r, "sf/other", factoryd.EventCardFunded, map[string]any{"pool": true, "amount_eur": 5.0}, now) // another repository
	seedRow(t, r, "sf/code", factoryd.EventCardFunded, map[string]any{"amount_eur": 4.0}, now)                // no pool draw
	seedFundRow(t, r, 9, lastMonth)                                                                           // another month
	r.SetProjectSpend(factoryd.ProjectSpend{EUR: 42, ChatEUR: 41})                                            // the model spend, never a draw
	drawn := "2 of 30 sessions, 1 h 20 min of 30 h agent time and €3.00 of €10.00 drawn this UTC month"
	readAll("the file's pool", drawn)
	if face := pageGET(t, r, ck, pages["the project face"]); !strings.Contains(face, "pool: "+drawn) {
		t.Fatalf("the face's pool cell:\n%s", face)
	}
	for _, tool := range []string{"settings", "policy"} {
		if read := chatRead(t, r, path, ck, csrf, "sebastian", tool); !strings.Contains(read, drawn) {
			t.Fatalf("the chat's %s read of the file's pool:\n%s", tool, read)
		}
	}
	want := policy.Draw{Sessions: 2, AgentTime: 80 * time.Minute, EUR: 3}
	if got, err := r.PoolDrawn(t.Context(), "sf/code", now); err != nil || got != want {
		t.Fatalf("the draws = %+v, %v; want %+v, the decoys excluded", got, err, want)
	}

	// the read's failure never becomes a zero
	r.FailPoolReads(errors.New("the spine is down"))
	unreadable := "the pool is 30 sessions, 30 h of agent time and €10.00 this UTC month, but what its cards drew could not be read"
	readAll("the unreadable draws", unreadable)
	for name, p := range pages {
		if body := pageGET(t, r, ck, p); strings.Contains(body, "€0.00 of €10.00") {
			t.Fatalf("%s reads the failed read as zero:\n%s", name, body)
		}
	}
	if face := pageGET(t, r, ck, pages["the project face"]); !strings.Contains(face, `class="cell pool warn"`) {
		t.Fatalf("the face's unread pool is no warning:\n%s", face)
	}
	if read := chatRead(t, r, path, ck, csrf, "sebastian", "settings"); !strings.Contains(read, unreadable) {
		t.Fatalf("the chat's settings read of the failed read:\n%s", read)
	}
	r.FailPoolReads(nil)

	// the November-to-December turn, the explicit instant alone: the
	// production read-helper (Console.ProjectPool) carries its instant in
	// its contract, so the fixture reads it at both, the pool from the
	// policy in force and the draws the month's own rows, and the turn of
	// the month keeps the pool while the draws reset
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
		return []any{pl.Pool.EUR, pl.Drawn.EUR, pl.Month, pl.Words}
	}
	equal(t, "November's readout", readout(nov), []any{10.0, 2.0, "2026-11",
		"0 of 30 sessions, 0 h of 30 h agent time and €2.00 of €10.00 drawn this UTC month"})
	equal(t, "December's readout", readout(dec), []any{10.0, 0.0, "2026-12",
		"0 of 30 sessions, 0 h of 30 h agent time and €0.00 of €10.00 drawn this UTC month"})
}
