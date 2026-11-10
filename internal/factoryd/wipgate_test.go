package factoryd

import (
	"context"
	"strings"
	"testing"
)

const wipRefusal = "interim push's work in progress"

// An interim commit is never judged, approved, opened as a pull request or
// merged as finished work: every gate asked about one refuses it, naming it,
// and does nothing.
func TestGatesRefuseAnInterimHead(t *testing.T) {
	fj := &gateForgejo{committers: map[string]string{"w2": wipEmail}}
	r, store, _ := newGateRunner(t, gateTask, fj)
	pr := PullRequest{Number: 9, URL: "https://git.example/sf/repo/pulls/9"}

	for name, call := range map[string]func() error{
		"approval": func() error {
			_, err := execActivity(t, r, ActivityRequestApproval, "t1", ApprovalInput{PR: pr, Commit: "w2"})
			return err
		},
		"review": func() error {
			_, err := execActivity(t, r, ActivityModelReview, "t1", pr, "w2", GateResults{})
			return err
		},
		"audit": func() error {
			_, err := execActivity(t, r, ActivityGateAudit, "t1", pr, "w2", GateResults{})
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), wipRefusal) || !strings.Contains(err.Error(), "w2") {
			t.Errorf("%s: err = %v, want the refusal naming w2 as an interim push's work in progress", name, err)
		}
	}
	if pending, _ := store.ApprovalRequests(context.Background(), "pending"); len(pending) != 0 || len(store.approvals) != 0 {
		t.Errorf("approvals = %+v, want none opened for an interim head", store.approvals)
	}

	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "w2", "sebastian", "", []string(nil))
	if res := getInto[MergeResult](t, val, err); res.Merged || !strings.Contains(res.Refusal, wipRefusal) || len(fj.merges) != 0 {
		t.Errorf("merge = %+v with %d merge calls, want the refusal and no merge", res, len(fj.merges))
	}
}

func TestOpenPullRequestAndChecksRefuseAnInterimHead(t *testing.T) {
	fj := &prForgejo{head: "w2", committers: map[string]string{"w2": wipEmail}}
	r, _, _ := newActivityRunner(t, prTask, fj)
	for name, call := range map[string]func() error{
		"open pull request": func() error { _, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "w2"); return err },
		"checks wait":       func() error { _, err := execActivity(t, r, ActivityWaitChecks, "t1", "w2"); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), wipRefusal) {
			t.Errorf("%s: err = %v, want the refusal", name, err)
		}
	}
	if len(fj.created) != 0 {
		t.Errorf("pull requests opened = %v, want none for an interim head", fj.created)
	}
}
