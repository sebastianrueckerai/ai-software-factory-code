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

	// a revision that cannot be read refuses, and names what to do
	r.tasks.plans["t-plan"][0].Plan = json.RawMessage(`{"cards":`)
	rec = r.do(post("/approvals/31/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if want := "Cannot read the plan's revisions. Approve again; if it fails again, replan the plan."; rec.Code != http.StatusInternalServerError ||
		!strings.Contains(rec.Body.String(), want) {
		t.Fatalf("an unreadable revision's approve = %d %q", rec.Code, rec.Body.String())
	}
	if len(merged) != 1 {
		t.Fatalf("merges = %q, want the one before", merged)
	}
}

// A human's approve whose gate rows cannot be read fails before the
// approving review is posted, so it half-happens nowhere.
func TestApproveReadsTheGateRowsBeforeTheReview(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-card", Kind: "agent", Status: "in-review",
		Repository: "sf/code", IssueNumber: 9, PRNumber: 9}}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 31, Kind: factoryd.ApprovalMerge, TaskID: "t-card",
		Project: "sf/code", PRNumber: 9, Status: "pending", Spec: json.RawMessage(`{"head_sha":"b7e04c19d2a3"}`)})
	r.c.cfg.CheckMerge = func(context.Context, string, string) error { return fmt.Errorf("the store is down") }
	ck, s := r.loggedIn(t)
	rec := r.do(post("/approvals/31/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Cannot read the card's review and audit rows. Approve again.") {
		t.Fatalf("approve = %d %q", rec.Code, rec.Body.String())
	}
	if len(r.fg.reviews) != 0 {
		t.Fatalf("reviews posted: %v", r.fg.reviews)
	}
}

// The refusal of the agent's approval of an unreviewed merge offers a
// request for changes only where an agent can take the card back: a factory
// pull request and a roll have none.
func TestUnreviewedRefusalPicksTheRemedy(t *testing.T) {
	err := &factoryd.UnreviewedMergeError{Stages: []string{"audit"}}
	const state = "The audit did not run, so the project agent cannot approve this merge."
	for _, c := range []struct {
		name string
		task factoryd.Task
		spec factoryd.MergeSpec
		want string
	}{
		{"agent card", factoryd.Task{}, factoryd.MergeSpec{}, state + " Leave the call to a human, or deny the merge with a request for changes (deny_merge with request_changes), which sends the card back so the gates run on its next head."},
		{"factory pull request", factoryd.Task{}, factoryd.MergeSpec{Factory: true}, state + " Leave the call to a human: no agent works on this pull request."},
		{"roll", factoryd.Task{Source: factoryd.SourceRoll}, factoryd.MergeSpec{}, state + " Leave the call to a human: no agent works on this pull request."},
	} {
		got, ok := unreviewedRefusal(fmt.Errorf("approve: %w", err), c.task, c.spec)
		if r, isRefusal := got.(*refusal); !ok || !isRefusal || r.status != http.StatusConflict || r.msg != c.want {
			t.Errorf("%s: %v, want 409 %q", c.name, got, c.want)
		}
	}
	if _, ok := unreviewedRefusal(fmt.Errorf("the store is down"), factoryd.Task{}, factoryd.MergeSpec{}); ok {
		t.Error("another error read as a refusal")
	}
}
