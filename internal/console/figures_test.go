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

// Under a premium cap the strip and the Spend panel show the premium
// class's spend this month against it, and the chat's policy read names
// the cap; without one, no premium meter and no line.
func TestBoardShowsThePremiumMeterUnderItsCap(t *testing.T) {
	r, ck := screenRig(t)
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
	for _, want := range []string{`aria-label="Premium spend of its month cap">25 percent</meter>`, `<span class="lbl">premium this month</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %s", want)
		}
	}
	if text, err := r.c.policyText(context.Background(), actor{}); err != nil || !strings.Contains(text, "  premium month cap: €50.00 (past it, premium calls run on big)\n") {
		t.Errorf("policy read = (%q, %v), want the premium cap", text, err)
	}
	r.ledger.premiumCap = 0
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "Premium spend") || strings.Contains(body, "premium this month") {
		t.Error("the board shows a premium meter without a premium cap")
	}
}

// The board page carries what the status panel and the stage word read:
// the factory's spend this UTC day and the policy's day cap as /costs
// shows them, the GitHub export's remote, and each card in progress with
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
