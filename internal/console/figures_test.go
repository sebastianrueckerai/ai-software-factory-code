package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Under a premium cap the Spend panel shows the premium class's spend
// this month against it, and the chat's policy read names the cap;
// without one, no premium meter and no line. The strip names the premium
// cap only once it is hit.
func TestBoardShowsThePremiumMeterUnderItsCap(t *testing.T) {
	r, ck := screenRig(t)
	r.c.clock = func() time.Time { return time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC) }
	r.ledger.premiumCap, r.ledger.premium = 50, 12.5
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, r.readSession(t, ck)))
	d, err := r.c.boardOf(req, urlFilter(req), button(req))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status.PremiumEUR != 12.5 || d.Status.PremiumCapEUR != 50 {
		t.Errorf("premium %.2f of %.2f, want 12.50 of 50", d.Status.PremiumEUR, d.Status.PremiumCapEUR)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`aria-label="premium this month">25 percent</meter>`, `<span class="lbl">premium this month <span class="turn">(resets 1 December at 00:00 UTC)</span></span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %s", want)
		}
	}
	if text, err := r.c.policyText(context.Background(), actor{}); err != nil || !strings.Contains(text, "  premium month cap: 50.00 EUR (past it, premium calls run on big)\n") {
		t.Errorf("policy read = (%q, %v), want the premium cap", text, err)
	}
	r.ledger.premiumCap = 0
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "premium this month") {
		t.Error("the board shows a premium meter without a premium cap")
	}
}

// The month meter under the policy's cap reads the enforced month spend,
// the whole factory's ledger with every outstanding hold as the gateway's
// headroom counts it, not the project's own ledger spend: at the October
// close (2026-10-31) the meter read 518 of 544, 95 percent, while the cap
// had stopped the factory. A project file's own cap keeps the project's
// spend, the chat's part apart.
func TestMonthMeterReadsTheEnforcedSpend(t *testing.T) {
	r, ck := screenRig(t)
	r.chat.spend = []spendRow{{"sf/code", time.Now(), 45, false}, {"demo-app/demo-app", time.Now(), 3, false}}
	r.ledger.enforced = policy.Enforced{Month: 48.5}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, r.readSession(t, ck)))
	d, err := r.c.boardOf(req, urlFilter(req), button(req))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status.Spend.EUR != 48.5 || d.Status.CapEUR != 50 || !d.Status.FactoryCap {
		t.Errorf("month %.2f of %.2f, factory cap %v, want 48.50 of 50 under the factory's cap", d.Status.Spend.EUR, d.Status.CapEUR, d.Status.FactoryCap)
	}
	if words := spendWords(d.Status.Spend, d.Status.CapEUR, d.Status.FactoryCap, time.Now()); !strings.Contains(words, "48.50 of 50.00 EUR") || strings.Contains(words, "the cards") {
		t.Errorf("spend words %q, want the factory's ledger without the project's split", words)
	}
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code"}}
	r.ledger.monthCaps = map[string]float64{"ai-software-factory-code": 60}
	if d, err = r.c.boardOf(req, urlFilter(req), button(req)); err != nil {
		t.Fatal(err)
	}
	if d.Status.Spend.EUR != 45 || d.Status.CapEUR != 60 || d.Status.FactoryCap {
		t.Errorf("under the file's cap: month %.2f of %.2f, factory cap %v, want 45.00 of 60, the project's own spend", d.Status.Spend.EUR, d.Status.CapEUR, d.Status.FactoryCap)
	}
}

// The board page carries what the status panel and the stage word read:
// the factory's enforced spend this UTC day and the policy's day cap, the
// GitHub export's remote, and each card in progress with
// its stage and the time in it: planning on a plan-track card and
// building on a direct one since the admission (the filing when never admitted),
// verifying since the gate began; no stage in any other status.
func TestBoardPageCarriesTheFiguresOfTheStatusPanel(t *testing.T) {
	r, ck := screenRig(t)
	now := time.Now().UTC()
	r.ledger.costs = policy.Costs{Days: []policy.DayCost{
		{Day: now.AddDate(0, 0, -1).Format(policy.DateLayout), Total: 9.5}, {Day: now.Format(policy.DateLayout), Total: 4.82}}}
	r.mirrors.rows["ai-software-factory-code "+factoryd.MirrorExport] = factoryd.Mirror{Project: "ai-software-factory-code",
		Direction: factoryd.MirrorExport, Remote: "sebastianrueckerai/ai-software-factory-code", State: "1 open pull request: #12"}
	admitted, gate := now.Add(-23*time.Minute), now.Add(-6*time.Minute)
	for i := range r.tasks.tasks {
		switch bt := &r.tasks.tasks[i]; bt.ID {
		case "t-run":
			bt.AdmittedAt = &admitted
		case "t-rev":
			bt.AdmittedAt, bt.GateSince, bt.Gate = &admitted, &gate, "merge queue"
		}
	}
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-planning", Kind: "agent", Status: "running",
		Track: policy.TrackPlan, Repository: "sf/code", Objective: "plan it", CreatedAt: now.Add(-8 * time.Minute)}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, r.readSession(t, ck)))
	d, err := r.c.boardOf(req, urlFilter(req), button(req))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status.TodayEUR != 4.82 || d.Status.DayCapEUR != 20 {
		t.Errorf("today %.2f of the day cap %.2f, want 4.82 of 20", d.Status.TodayEUR, d.Status.DayCapEUR)
	}
	if d.Status.GitHub != "1 open pull request" || d.Status.GitHubURL != "https://github.com/sebastianrueckerai/ai-software-factory-code/pulls" {
		t.Errorf("GitHub %q at %q", d.Status.GitHub, d.Status.GitHubURL)
	}
	got := map[string][2]string{}
	for _, col := range d.Columns {
		for _, k := range col.Cards {
			got[k.ID] = [2]string{k.Stage, k.StageAge}
		}
	}
	want := map[string][2]string{"t-run": {"building", "23 min"}, "t-close": {"building", "under a minute"}, "t-planning": {"planning", "8 min"},
		"t-rev": {"verifying", "6 min"}, "t-queue": {"", ""}, "t-deps": {"", ""}, "t-done": {"", ""}, "t-fail": {"", ""}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: stage %q for %q, want %q for %q", id, got[id][0], got[id][1], w[0], w[1])
		}
	}
}

// The confirm of a Reset says what the count becomes, the most that may
// run before the cap's period turns and what stays: at 14:02 UTC on 20
// November the day turns in 9 h 58 min and the month in 10 d 9 h 58 min.
// LiteLLM's backstop, 1875.00 - 1238.59 = 636.41 USD left, joins the
// sentence only under a cap above what it leaves. The premium's names the
// class its money is spent on.
func TestResetConfirmWords(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 2, 0, 0, time.UTC)
	backstop := &factoryd.Backstop{SpendUSD: 1238.59, MaxUSD: 1875}
	const day = "Today's count starts again from 0.00 EUR. Up to 100.00 EUR more may be spent before 00:00 UTC, in 9 h 58 min. The day cap stays 100.00 EUR; the month cap still holds."
	for _, tc := range []struct {
		cap      string
		eur      float64
		backstop *factoryd.Backstop
		want     string
	}{
		{policy.CapDay, 100, nil, day},
		{policy.CapDay, 100, backstop, day},
		{policy.CapMonth, 800, nil, "This month's count starts again from 0.00 EUR. Up to 800.00 EUR more may be spent before 1 December 00:00 UTC, in 10 d 9 h. The month cap stays 800.00 EUR; the day cap still holds."},
		{policy.CapMonth, 800, backstop, "This month's count starts again from 0.00 EUR. Up to 800.00 EUR more may be spent before 1 December 00:00 UTC, in 10 d 9 h. The month cap stays 800.00 EUR; the day cap still holds. LiteLLM's backstop leaves 636.41 USD this month."},
		{policy.CapProject, 30, backstop, "The project's month count starts again from 0.00 EUR. Up to 30.00 EUR more may be spent before 1 December 00:00 UTC, in 10 d 9 h. The project's month cap stays 30.00 EUR; the factory's caps still hold."},
		{policy.CapPremium, 50, backstop, "This month's premium count starts again from 0.00 EUR. Up to 50.00 EUR more may be spent on premium before 1 December 00:00 UTC, in 10 d 9 h. The premium cap stays 50.00 EUR; the day cap and the month cap still hold."},
	} {
		if got := resetWords(tc.cap, tc.eur, now, tc.backstop); got != tc.want {
			t.Errorf("resetWords(%s, %.2f, backstop %v) =\n%s\nwant\n%s", tc.cap, tc.eur, tc.backstop != nil, got, tc.want)
		}
	}
}

// The agent's spend words say the month's whole spend as the spend this
// month and, once a reset restarted the cap's count, the count apart with
// the reset: under the factory's cap 12 counted since sebastian's reset
// at 48 make 60 in the month; under a project's, the cards' and the
// chat's split is the count's. One clock: the reset at 14:02 of the day
// the words are read.
func TestSpendWordsSayTheWholeMonthAndTheCount(t *testing.T) {
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	reset := &policy.CapReset{Cap: policy.CapMonth, At: time.Date(2026, 11, 20, 14, 2, 0, 0, time.UTC), Login: "sebastian", BeforeEUR: 48}
	for _, tc := range []struct {
		spend   factoryd.ProjectSpend
		factory bool
		want    string
	}{
		{factoryd.ProjectSpend{EUR: 48.5, AllEUR: 48.5}, true, "spend this month 48.50 of 50.00 EUR, the factory's whole ledger"},
		{factoryd.ProjectSpend{EUR: 12, AllEUR: 60, Hit: true, Reset: reset}, true,
			"spend this month 60.00 EUR, the factory's whole ledger; counted against the 50.00 EUR cap since the reset 14:02 UTC by sebastian, 48.00 EUR before: 12.00 EUR, cap hit"},
		{factoryd.ProjectSpend{EUR: 3, ChatEUR: 1, AllEUR: 3}, false, "spend this month 3.00 of 50.00 EUR: the cards 2.00 EUR, the chat 1.00 EUR"},
		{factoryd.ProjectSpend{EUR: 10, ChatEUR: 2, AllEUR: 58, Reset: reset}, false,
			"spend this month 58.00 EUR; counted against the 50.00 EUR cap since the reset 14:02 UTC by sebastian, 48.00 EUR before: 10.00 EUR, the cards 8.00 EUR, the chat 2.00 EUR"},
	} {
		if got := spendWords(tc.spend, 50, tc.factory, now); got != tc.want {
			t.Errorf("spendWords(%+v, factory %v) =\n%s\nwant\n%s", tc.spend, tc.factory, got, tc.want)
		}
	}
}
