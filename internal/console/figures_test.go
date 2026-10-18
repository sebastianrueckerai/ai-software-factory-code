package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The board page carries what the status panel and the stage word read:
// the factory's spend this UTC day and the policy's day cap as /costs
// shows them, the GitHub export's remote, and each card in progress with
// its stage and the time in it: planning on a plan-lane card and building
// on a coder's since the admission (the filing when never admitted),
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
		Lane: policy.LanePlan, Repository: "sf/code", Objective: "plan it", CreatedAt: now.Add(-8 * time.Minute)}})

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
		"t-rev": {"verifying", "6 min"}, "t-queue": {"", ""}, "t-deps": {"", ""}, "t-done": {"", ""}, "t-fail": {"", ""}, "t-back": {"", ""}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: stage %q for %q, want %q for %q", id, got[id][0], got[id][1], w[0], w[1])
		}
	}
}
