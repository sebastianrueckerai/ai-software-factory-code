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

// A card the newest revision drops cannot merge (piece 10): its merge
// request answers 409 naming the revision and the card. A started card a
// revision keeps verbatim approves as before (Review Focus 3).
func TestStaleCardMergeApprovalRefuses(t *testing.T) {
	r := newRig(t)
	both, _ := json.Marshal(factoryd.Plan{Summary: "The shortener", Cards: []factoryd.PlanCard{
		{Key: "foundation", Role: "foundation", Title: "Foundation"},
		{Key: "links-api", Role: "feature", Title: "Links API"},
	}})
	foundation, _ := json.Marshal(factoryd.Plan{Summary: "The shortener, foundation only", Cards: []factoryd.PlanCard{
		{Key: "foundation", Role: "foundation", Title: "Foundation"},
	}})
	kept, _ := json.Marshal(factoryd.Plan{Summary: "The shortener, again", Cards: []factoryd.PlanCard{
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
		{TaskID: "t-plan", Revision: 1, Plan: both, SHA256: "sha-1"},
		{TaskID: "t-plan", Revision: 2, Plan: foundation, SHA256: "sha-2"},
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
	want := "The plan's revision 2 drops the card links-api, so it cannot merge: deny the merge (deny_merge)."
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("a dropped card's approve = %d %q", rec.Code, rec.Body.String())
	}
	if len(merged) != 0 {
		t.Fatalf("the merge was signalled: %q", merged)
	}

	// revision 2 keeps the card verbatim: the approve goes through
	r.tasks.plans["t-plan"][1].Plan = kept
	rec = r.do(post("/approvals/31/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a kept card's approve = %d %q", rec.Code, rec.Body.String())
	}
	if fmt.Sprint(merged) != "[t-card pr 9 by sebastian]" {
		t.Fatalf("merges = %q", merged)
	}
}
