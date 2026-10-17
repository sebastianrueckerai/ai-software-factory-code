package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// A card the plan has passed cannot merge (piece 10): its merge request
// answers 409 naming the revision it ran against, and a card whose plan
// holds no newer revision approves as before.
func TestStaleCardMergeApprovalRefuses(t *testing.T) {
	r := newRig(t)
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The shortener", Cards: []factoryd.PlanCard{
		{Key: "foundation", Role: "foundation", Title: "Foundation"},
		{Key: "links-api", Role: "feature", Title: "Links API"},
	}})
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "Shortener",
			Repository: "sf/code", IssueNumber: 3}},
		{Task: factoryd.Task{ID: "t-card", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 9,
			Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "links-api", PlanRevision: 1, PRNumber: 9}},
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {
		{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"},
		{TaskID: "t-plan", Revision: 2, Plan: plan, SHA256: "sha-2"},
	}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 31, Kind: factoryd.ApprovalMerge, TaskID: "t-card",
		Project: "sf/code", PRNumber: 9, Status: "pending", Spec: json.RawMessage(`{"head_sha":"b7e04c19d2a3"}`)})
	var merged []string
	r.c.cfg.ApproveMerge = func(_ context.Context, id string, pr int64, by string) error {
		merged = append(merged, fmt.Sprintf("%s pr %d by %s", id, pr, by))
		return nil
	}
	ck, s := r.loggedIn(t)
	rec := r.do(post("/approvals/31/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	want := "The plan moved to revision 2, the card ran against revision 1, so it cannot merge: deny the merge (deny_merge) and let the planner re-file the plan."
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("a stale card's approve = %d %q", rec.Code, rec.Body.String())
	}
	if len(merged) != 0 {
		t.Fatalf("the merge was signalled: %q", merged)
	}

	// the plan holds no revision past the card's: the approve goes through
	r.tasks.plans["t-plan"] = r.tasks.plans["t-plan"][:1]
	rec = r.do(post("/approvals/31/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a current card's approve = %d %q", rec.Code, rec.Body.String())
	}
	if fmt.Sprint(merged) != "[t-card pr 9 by sebastian]" {
		t.Fatalf("merges = %q", merged)
	}
}
