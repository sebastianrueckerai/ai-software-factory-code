package console

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

const leakedBoard = "https://user:pass@forge/x answered Authorization: Bearer abc123def456"

// The Approve button answers the merge case of its request through Act: one
// call of approve_merge on that case by the human on rung 3, the work riding
// as Act's Work, which submits the review and signals the workflow.
func TestApproveGoesThroughActOnTheMergeCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.Work == nil {
		t.Fatal("the approval carries no work")
	}
	got.Work = nil
	want := factoryd.ActInput{TaskID: "t-m", CaseID: 7004, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActApproveMerge}, Detail: got.Detail}
	if !equalInput(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
	}
}

func equalInput(a, b factoryd.ActInput) bool {
	return a.TaskID == b.TaskID && a.CaseID == b.CaseID && a.By == b.By && a.Via == b.Via && a.Rung == b.Rung &&
		a.Res.Action == b.Res.Action && a.Res.Note == b.Res.Note
}

// A dependency edge is set through Act: one call of set_dependency on the
// card, which has no case, naming the card and the one it waits for, whose
// work writes today's one dependency_set row.
func TestSetDependencyGoesThroughAct(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	_, s := r.loggedIn(t)
	before := r.events.count()
	say, err := r.c.doSetDependency(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-b", "t-a", false)
	if err != nil || say != "#22 depends on #21 now." {
		t.Fatalf("doSetDependency = %q, %v", say, err)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActSetDependency || got.Res.Args["task_id"] != "t-b" || got.Res.Args["depends_on"] != "t-a" || got.CaseID != 0 || got.Rung != 3 {
		t.Fatalf("Act took %+v, want set_dependency of t-b on t-a with no case on rung 3", got)
	}
	if n := r.events.count() - before; n != 1 {
		t.Fatalf("%d rows written, want the one dependency_set row", n)
	}
}

// A failure of a board action's work that is no refusal of the console's
// carries its words masked: URL userinfo and a bearer value are gone.
func TestBoardWorkFailureIsMasked(t *testing.T) {
	err := maskedErr(errors.New("forge said " + leakedBoard))
	for _, secret := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error %q holds %q", err, secret)
		}
	}
	kept := refuse(http.StatusConflict, "a console sentence")
	if maskedErr(kept) != error(kept) {
		t.Error("a console refusal was rewrapped")
	}
}
