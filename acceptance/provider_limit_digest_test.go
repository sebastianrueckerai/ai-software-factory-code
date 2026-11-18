package acceptance

// The frozen card's acceptance proofs on the park rig: the real store, the
// real runner's digest and monitor, the real console over them and, for
// the brief, the compiled gateway on the same database. A card's timeline
// here is seeded in the timeline writer's own shape, its lines stamped
// hours back, since the digest, the board and the life all read the hours
// against the wall clock.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// plFrozenWords is the provider's words the frozen card's refusal carries.
const plFrozenWords = "rate limit exceeded, resets at the named hour"

// plTimeline writes card id's timeline: each line a step at its moment,
// the detail beside it, as the timeline writer appends them.
func plTimeline(t *testing.T, r *parkRig, id string, lines ...map[string]any) {
	t.Helper()
	var body []byte
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		body = append(append(body, raw...), '\n')
	}
	if err := r.objects.Put(context.Background(), "sessions/"+id+"/timeline.jsonl", body); err != nil {
		t.Fatal(err)
	}
}

// plStep is one timeline line of step at at.
func plStep(step string, at time.Time, detail map[string]any) map[string]any {
	l := map[string]any{"step": step, "time": at.UTC().Format(time.RFC3339Nano)}
	for k, v := range detail {
		l[k] = v
	}
	return l
}

// plRunningCard files card id running on the park project's repository,
// filed with its budget and admitted at since: its building stage began
// then.
func plRunningCard(t *testing.T, r *parkRig, id string, issue int64, since time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := r.store.CreateTask(ctx, factoryd.Task{ID: id, Kind: "agent", Track: policy.TrackDirect, Source: "webhook",
		Repository: parkRepo, IssueNumber: issue, Status: "running", BudgetEUR: 5, Objective: "build the frozen card's work",
		RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.DB().ExecContext(ctx, `UPDATE tasks SET created_at = $2, admitted_at = $2 WHERE id = $1`, id, since); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Record(ctx, factoryd.Event{At: since, Repository: parkRepo, TaskID: id, IssueNumber: issue,
		Kind: factoryd.EventCardCreated, Actor: parkBot, Via: factoryd.ViaFactoryd, Detail: map[string]any{"budget_eur": 5.0}}); err != nil {
		t.Fatal(err)
	}
}

// plFlagCases are the monitor_flag cases the card holds, by their key.
func plFlagCases(t *testing.T, r *parkRig, id string) []string {
	t.Helper()
	cases, err := r.store.CasesOf(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range cases {
		if c.Kind == factoryd.CaseMonitorFlag {
			keys = append(keys, c.StateKey)
		}
	}
	return keys
}

// TestDigestQuietInsideFreeze: a card in its building stage whose session
// the provider refused 3 h 12 m in with a named reset 7 h out, a grant
// before that session and a check-in the frozen class refused inside the
// interval. Read at moments across the freeze — a card each, its stage
// begun that long before now — provider_limit owns the wait, so neither
// the quiet bound nor the check-in's or the grant's flags fire: no digest
// holds a flag and the monitor's sweep opens no case. Ten hours in, the
// face splits the clocks — "3 h 12 min, frozen 6 h 48 min", Total the same
// — and its chip shows the words and the reset; the life records the
// frozen interval. [digest_quiet_in_freeze]
func TestDigestQuietInsideFreeze(t *testing.T) {
	r := newParkRig(t)
	ctx := context.Background()
	moments := []int{193, 300, 420, 599, 600} // minutes into the stage; the reset is at 612
	var tenHours time.Time
	for i, m := range moments {
		id := fmt.Sprintf("t-frozen-%d", m)
		start := time.Now().Add(-time.Duration(m) * time.Minute)
		at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
		plRunningCard(t, r, id, int64(31+i), start)
		if _, err := r.store.Record(ctx, factoryd.Event{At: at(150), Repository: parkRepo, TaskID: id, IssueNumber: int64(31 + i),
			Kind: factoryd.EventSessionGranted, Actor: parkMember, Via: factoryd.ViaConsole, Detail: map[string]any{"sessions": 1}}); err != nil {
			t.Fatal(err)
		}
		lines := []map[string]any{
			plStep("session_start", at(180), map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1}),
			plStep("session_end", at(192), map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1,
				"outcome": string(factoryd.OutcomeProviderRefused), "summary": "the provider refused", "started": at(180).UTC().Format(time.RFC3339)}),
			plStep(factoryd.StepProviderRefused, at(192), map[string]any{"status": 429, "reason": "provider limit for class coder",
				"backoff_s": 25200, "words": plFrozenWords}),
		}
		if m >= 300 {
			lines = append(lines, plStep("checkin", at(300), map[string]any{"moment": factoryd.CheckInGate, "decision": factoryd.CheckHandover,
				"reason": factoryd.CheckinRefusedReason + "class frozen"}))
		}
		plTimeline(t, r, id, lines...)
		d, err := r.runner.CardDigest(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Flags) != 0 {
			t.Fatalf("at +%d: Flags = %v, want none inside the freeze", m, d.Flags)
		}
		tenHours = start
	}
	m := &factoryd.Monitor{Store: r.store, Runner: r.runner, Policy: func() policy.Tree { return r.version.Tree }, Now: time.Now}
	if err := m.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, at := range moments {
		if keys := plFlagCases(t, r, fmt.Sprintf("t-frozen-%d", at)); len(keys) != 0 {
			t.Fatalf("at +%d: the sweep opened flag cases %q, want none inside the freeze", at, keys)
		}
	}

	ck := r.session(t)
	reset := tenHours.Add(192*time.Minute + 7*time.Hour)
	chip := "frozen until " + reset.UTC().Format("15:04 UTC") + ": " + plFrozenWords
	board := r.get(t, "/board", ck).Body.String()
	for _, want := range []string{`building</span> 3 h 12 min, frozen 6 h 48 min</span>`, chip} {
		if !strings.Contains(board, want) {
			t.Errorf("the board lacks %s", want)
		}
	}
	drawer := r.get(t, "/cards/t-frozen-600/drawer?tab=overview", ck).Body.String()
	for _, want := range []string{
		`<span class="it">3 h 12 min, frozen 6 h 48 min</span>`,
		"frozen on the provider limit until " + reset.UTC().Format("15:04 UTC") + ": " + plFrozenWords,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %s", want)
		}
	}
}

// plFrozenVersion is the park rig's version as the gateway loads it, the
// provider named: the coder class runs on zai, whose words read
// Asia/Singapore, and its briefs and check-ins call small, the alias the
// gateway prices.
func plFrozenVersion(r *parkRig) policy.Tree {
	tree := r.version.Tree
	doc := tree.Document
	doc.Classes = maps.Clone(doc.Classes)
	coder := doc.Classes[policy.ClassCoder]
	coder.Models, coder.SupervisorModel, coder.CheckinModel, coder.Provider = []string{"small"}, "small", "small", "zai"
	doc.Classes[policy.ClassCoder] = coder
	doc.ProviderLimits = map[string]policy.ProviderLimit{"zai": {TZ: "Asia/Singapore"}}
	doc.Prices.MaxOutputTokens = map[string]int{"small": 32768}
	doc.Version = 1
	doc.Risk = policy.Risk{Classes: []policy.RiskClass{{Name: "normal", Outcome: policy.OutcomeRequiresApproval}}, Default: "normal"}
	doc.EnvelopeDefault = policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4}
	tree.Document = doc
	return tree
}

// TestBriefWakeAtReset: the compiled gateway beside the park rig on one
// database. The brief writer's first call on the project's chat task
// reaches the provider, which names a reset an hour out: the gateway
// freezes the class and answers the named refusal, and the writer records
// exactly one brief_failed row carrying the reset and sets the case's wake
// to the reset plus its two-minute margin. The monitor's sweep at that
// wake wakes the case; the writer's next call is answered by the hold
// itself, no provider call, and records no second row; a sweep a second
// before the wake leaves the case asleep. A check-in for the card inside
// the same hold is refused with the reset, and the not-run record such a
// refusal leaves raises no flag. [brief_wakes_at_reset]
func TestBriefWakeAtReset(t *testing.T) {
	r := newParkRig(t)
	ctx := context.Background()
	v, _, err := r.store.RecordPolicy(ctx, "c-frozen", plFrozenVersion(r))
	if err != nil {
		t.Fatal(err)
	}
	gwStore, err := factorygateway.OpenStore(ctx, r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gwStore.DB().Close() })
	stub := newAccStubLiteLLM(t)
	gw := httptest.NewServer(factorygateway.NewServer(factorygateway.ServerOptions{
		Auth: accGatewayAuth{task: "t-brief"}, Store: gwStore, Minter: &accGatewayMinter{},
		Prices:     accGatewayPrices{price: policy.Price{In: 1e-9, Out: 1e-9, MaxOutput: 32768}},
		LiteLLMURL: stub.URL, FactorydToken: "factoryd-token",
	}).Routes("brief-proof"))
	t.Cleanup(gw.Close)

	// the provider's reset, an hour out, in its Singapore wall time (UTC+8);
	// the gateway answers it plus its minute's margin
	resetAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	reset := resetAt.Add(time.Minute)
	wake := reset.Add(2 * time.Minute)
	stub.serve(http.StatusTooManyRequests, `{"error":{"message":"Usage limit reached for 5 hour. Your limit will reset at `+
		resetAt.Add(8*time.Hour).Format("2006-01-02 15:04:05")+`"}}`, nil)

	// the card frozen on the class: its session refused 40 minutes ago,
	// a case at the human waiting for its brief
	start := time.Now().Add(-time.Hour)
	plRunningCard(t, r, "t-brief", 41, start)
	refusedAt := start.Add(20 * time.Minute)
	frozen := plStep(factoryd.StepProviderRefused, refusedAt, map[string]any{"status": 429, "reason": "provider limit for class coder",
		"backoff_s": int(reset.Sub(refusedAt).Seconds()), "words": "Usage limit reached for 5 hour"})
	plTimeline(t, r, "t-brief", frozen)
	c, _, err := r.store.OpenCase(ctx, factoryd.Case{TaskID: "t-brief", Repository: parkRepo, Kind: factoryd.CaseOther,
		StateKey: "stop", Status: factoryd.CaseAtHuman, Problem: "the stop", Facts: map[string]any{"reason": "the stop"}})
	if err != nil {
		t.Fatal(err)
	}
	model := &factoryd.SupervisorModel{GatewayURL: gw.URL, Token: "factoryd-token", Budget: factoryd.TokenBudget{AnswerTokens: 512, HeadroomTokens: 512}}
	w := &factoryd.BriefWriter{Store: r.store, CardDigest: r.runner.CardDigest, Current: func() policy.Version { return v }, Model: model}
	rows := func() (n int, backoff float64, why string) {
		t.Helper()
		if err := r.store.DB().QueryRowContext(ctx, `SELECT count(*), COALESCE(min((detail->>'backoff_s')::float8), 0), COALESCE(min(detail->>'error'), '')
			FROM events WHERE kind = $1 AND (detail->>'case')::bigint = $2`, factoryd.EventBriefFailed, c.ID).Scan(&n, &backoff, &why); err != nil {
			t.Fatal(err)
		}
		return n, backoff, why
	}
	wokeAt := func(what string) {
		t.Helper()
		held, err := r.store.CaseByID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if held.NextWakeAt == nil || !held.NextWakeAt.Equal(wake) {
			t.Fatalf("%s: the case's wake = %v, want %s, the reset plus the margin", what, held.NextWakeAt, wake)
		}
	}

	// the first call reaches the provider and freezes the class: one row
	// with the seconds to the reset, the case's wake at the reset
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := stub.calls(); len(calls) != 1 {
		t.Fatalf("provider calls %v, want the one the hold armed on", calls)
	}
	if n, backoff, why := rows(); n != 1 || backoff < 3650 || backoff > 3670 || why != "the class is frozen until "+reset.Format(time.RFC3339) {
		t.Fatalf("brief_failed rows = %d, backoff_s %v, error %q; want one row naming the reset %s, the seconds to it (about 3660)", n, backoff, why, reset.Format(time.RFC3339))
	}
	wokeAt("the frozen brief")
	if cases, err := r.store.BrieflessCases(ctx, 5); err != nil || len(cases) != 0 {
		t.Fatalf("BrieflessCases inside the freeze = %v, %v; want none: the pass waits at the case's wake", cases, err)
	}

	// a sweep a second before the wake leaves the case asleep; the sweep at
	// the wake wakes it, and the writer takes it again
	sweep := func(at time.Time) {
		t.Helper()
		m := &factoryd.Monitor{Store: r.store, Runner: r.runner, Policy: func() policy.Tree { return v.Tree }, Now: func() time.Time { return at }}
		if err := m.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sweep(wake.Add(-time.Second))
	wokeAt("a second before the wake")
	sweep(wake)
	if woken, err := r.store.CaseByID(ctx, c.ID); err != nil || woken.NextWakeAt != nil || woken.Status != factoryd.CaseAtHuman {
		t.Fatalf("the sweep left the frozen case = %+v, %v; want it woken at the human with no wake", woken, err)
	}
	if cases, err := r.store.BrieflessCases(ctx, 5); err != nil || !slices.ContainsFunc(cases, func(b factoryd.Case) bool { return b.ID == c.ID }) {
		t.Fatalf("BrieflessCases after the wake = %v, %v; want the case again", cases, err)
	}

	// the hold answers the next call itself: no provider call, no second row
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := stub.calls(); len(calls) != 1 {
		t.Fatalf("provider calls %v, want the one still: the hold answered the retry", calls)
	}
	if n, _, _ := rows(); n != 1 {
		t.Fatalf("brief_failed rows = %d after the retry; want one per freeze, not one per retry", n)
	}
	wokeAt("the retried brief")

	// a check-in for the card inside the hold, in the check-in client's
	// shape: the gateway refuses it with the reset, and its not-run record
	// raises no flag
	chat, err := r.runner.JudgmentTask(ctx, factoryd.Task{Repository: parkRepo})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"small","messages":[{"role":"user","content":"the check-in"}]}`))
	for k, v := range map[string]string{"Authorization": "Bearer factoryd-token", "Content-Type": "application/json",
		factorygateway.HeaderFactoryTask: chat.ID, factorygateway.HeaderFactoryPurpose: policy.PurposeCheckin, factorygateway.HeaderFactoryCard: policy.ClassCoder} {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	gw.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get(factorygateway.HeaderDecision) != factorygateway.RefusedProviderLimit ||
		!strings.Contains(rec.Body.String(), `"reset":"`+reset.Format(time.RFC3339)+`"`) {
		t.Fatalf("the check-in call = %d %q: %s, want the named refusal until %s", rec.Code, rec.Header().Get(factorygateway.HeaderDecision), rec.Body.String(), reset)
	}
	if calls := stub.calls(); len(calls) != 1 {
		t.Fatalf("provider calls %v, want the one still: the hold answered the check-in", calls)
	}
	plTimeline(t, r, "t-brief", frozen, plStep("checkin", time.Now(), map[string]any{"moment": factoryd.CheckInGate,
		"decision": factoryd.CheckHandover, "reason": factoryd.CheckinFrozenReason + reset.Format("15:04") + " UTC"}))
	d, err := r.runner.CardDigest(ctx, "t-brief")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Flags) != 0 {
		t.Fatalf("Flags = %v, want none: the frozen check-in flags nothing", d.Flags)
	}
}
