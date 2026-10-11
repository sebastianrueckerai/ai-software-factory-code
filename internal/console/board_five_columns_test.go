package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestFiveColumnsPlaceCardsCorrectly verifies that cards in every status
// land in the right column of the five, including both author cases for
// waiting on the author.
func TestFiveColumnsPlaceCardsCorrectly(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", []factoryd.Task{
		{ID: "t-wh", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Objective: "waiting human", Author: "other"},
		{ID: "t-wp-author", Kind: "agent", Status: "waiting-poster", Repository: "sf/code", Objective: "waiting poster author", Author: "sebastian"},
		{ID: "t-wp-other", Kind: "agent", Status: "waiting-poster", Repository: "sf/code", Objective: "waiting poster other", Author: "other"},
		{ID: "t-backlog", Kind: "agent", Status: "backlog", Lane: "backlog", Repository: "sf/code", Objective: "backlog"},
		{ID: "t-queued", Kind: "agent", Status: "queued", Lane: "ready", Repository: "sf/code", Objective: "queued"},
		{ID: "t-waiting-deps", Kind: "agent", Status: "waiting-deps", Lane: "ready", Repository: "sf/code", Objective: "waiting deps"},
		{ID: "t-running", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "running"},
		{ID: "t-closing", Kind: "agent", Status: "closing", Repository: "sf/code", Objective: "closing"},
		{ID: "t-in-review", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "in review"},
		{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", Objective: "done"},
		{ID: "t-failed", Kind: "agent", Status: "failed", Repository: "sf/code", Objective: "failed"},
		{ID: "t-closed", Kind: "agent", Status: "closed", Repository: "sf/code", Objective: "closed"},
	}...)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()

	cols := map[string][]string{
		"Needs you": {"waiting human", "waiting poster author"},
		"Queued":    {"waiting poster other", "backlog", "queued", "waiting deps"},
		"Working":   {"running", "closing"},
		"Verifying": {"in review"},
		"Done":      {"done", "failed", "closed"},
	}

	for colName, wantObjs := range cols {
		start := strings.Index(body, ">"+colName+" <span")
		if start < 0 {
			t.Fatalf("column %q missing", colName)
		}
		end := strings.Index(body[start:], "</section>")
		colBody := body[start : start+end]
		for _, obj := range wantObjs {
			if !strings.Contains(colBody, obj) {
				t.Errorf("column %q lacks %q", colName, obj)
			}
		}
	}
}

// TestFiveColumnsCountsInHeaders verifies the counts in the column headers.
func TestFiveColumnsCountsInHeaders(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", []factoryd.Task{
		{ID: "t-1", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Objective: "one"},
		{ID: "t-2", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Objective: "two"},
		{ID: "t-3", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "three"},
	}...)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()

	if !strings.Contains(body, ">Needs you <span class=\"count\">2</span>") {
		t.Error("Needs you count not 2")
	}
	if !strings.Contains(body, ">Working <span class=\"count\">1</span>") {
		t.Error("Working count not 1")
	}
}

// TestStatusLineShowsFourFigures verifies the status line's state word,
// working count, need-you count, and spend against cap.
func TestStatusLineShowsFourFigures(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", []factoryd.Task{
		{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "working"},
		{ID: "t-2", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Objective: "needs you"},
	}...)
	r.ledger.costs = policy.Costs{
		Days: []policy.DayCost{
			{Day: time.Now().UTC().Format(policy.DateLayout), Total: 5.50},
		},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()

	if !strings.Contains(body, `class="status-line"`) {
		t.Fatal("status line missing")
	}
	if !strings.Contains(body, ">working</span>") {
		t.Error("status line lacks state word 'working'")
	}
	if !strings.Contains(body, ">1 working</span>") {
		t.Error("status line lacks '1 working'")
	}
	if !strings.Contains(body, ">1 need you</a>") {
		t.Error("status line lacks '1 need you'")
	}
	if !strings.Contains(body, "€5.50") {
		t.Error("status line lacks spend €5.50")
	}
}

// TestWideViewShowsLanesFirst verifies the wide view keeps today's columns,
// lanes first.
func TestWideViewShowsLanesFirst(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "backlog", Lane: "backlog", Repository: "sf/code", Objective: "backlog card"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()

	backlogIdx := strings.Index(body, ">Backlog <span")
	runningIdx := strings.Index(body, ">Running <span")
	if backlogIdx < 0 || runningIdx < 0 {
		t.Fatal("wide view lacks Backlog or Running column")
	}
	if backlogIdx > runningIdx {
		t.Error("wide view: Backlog should come before Running")
	}
	if strings.Contains(body, `class="status-line"`) {
		t.Error("wide view should not show status line")
	}
}

// TestDrawerThreeTabsForWriter verifies the drawer's three tabs load for a
// writer and return the expected content.
func TestDrawerThreeTabsForWriter(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "test card", PRNumber: 5})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_start","time":"2026-09-28T10:00:00Z"}
`)

	for _, tab := range []string{"overview", "activity", "files"} {
		rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-1/drawer?tab="+tab, nil), ck)
		if rec.Code != http.StatusOK {
			t.Errorf("drawer tab %s = %d: %s", tab, rec.Code, rec.Body)
		}
	}
}

// TestDrawer403ForNonWriter verifies a non-writer gets 403.
func TestDrawer403ForNonWriter(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "test"})
	r.repoWriters["sf/code"] = nil
	r.c.mu.Lock()
	r.c.moves = nil
	r.c.mu.Unlock()

	rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-1/drawer", nil), ck)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-writer drawer = %d, want 403", rec.Code)
	}
}

// TestDrawer404Unknown verifies an unknown card gets 404.
func TestDrawer404Unknown(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-unknown/drawer", nil), ck)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown drawer = %d, want 404", rec.Code)
	}
}

// TestDrawerFilesNoPR verifies the Files tab shows "no pull request yet"
// when there is no PR.
func TestDrawerFilesNoPR(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no pr"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-1/drawer?tab=files", nil), ck).Body.String()
	if !strings.Contains(body, "No pull request yet") {
		t.Errorf("files tab without PR lacks message: %s", body)
	}
}
