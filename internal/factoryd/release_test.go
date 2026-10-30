package factoryd

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestHoldReason(t *testing.T) {
	plan := func(issue int64) Group { return Group{Issue: issue} }
	for _, c := range []struct {
		groups []Group
		want   string
	}{
		{[]Group{plan(12)}, "plan #12 is open on main"},
		{[]Group{plan(12), plan(15)}, "plans #12 and #15 are open on main"},
		{[]Group{plan(12), plan(15), plan(18)}, "plans #12, #15 and #18 are open on main"},
	} {
		if got := HoldReason(c.groups); got != c.want {
			t.Errorf("HoldReason(%d groups) = %q, want %q", len(c.groups), got, c.want)
		}
	}
}

func TestGroupMerged(t *testing.T) {
	g := Group{Children: map[string]int{"merged": 2, "done": 1, "queued": 2, "closed": 1}}
	if merged, total := g.Merged(); merged != 3 || total != 5 {
		t.Errorf("Merged() = (%d, %d), want (3, 5)", merged, total)
	}
}

func TestPGStoreReleases(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_releases"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkReleaseRows(t, s)
	checkFixesMergedSince(t, s)
}

func TestFakeStoreReleases(t *testing.T) {
	checkReleaseRows(t, newFakeStore())
	checkFixesMergedSince(t, newFakeStore())
}

func releaseAt(h, m int) time.Time { return time.Date(2026, 11, 8, h, m, 0, 0, time.UTC) }

// checkReleaseRows drives a release row through its writers. A pass that
// read the row before a Deploy now never undoes the press.
func checkReleaseRows(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	at := releaseAt
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(repository string) Release {
		t.Helper()
		r, err := s.Release(ctx, repository)
		must(err)
		return utcRelease(r)
	}
	check := func(step string, want Release) {
		t.Helper()
		if got := read(want.Repository); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Release = %+v, want %+v", step, got, want)
		}
	}

	check("no row", Release{Repository: "sf/a"})
	must(s.RecordReleaseAsk(ctx, "sf/b", "b1", false, at(8, 0)))
	must(s.RecordReleaseAsk(ctx, "sf/a", "c1", true, at(9, 0)))
	must(s.RecordReleaseAsk(ctx, "sf/a", "c2", true, at(9, 5)))
	must(s.RecordReleaseAsk(ctx, "sf/a", "c3", false, at(9, 10)))
	want := Release{Repository: "sf/a", AsksAt: at(9, 10), ToldHeld: "c2"}
	check("three asks", want)

	must(s.PutReleaseHold(ctx, "sf/a", at(10, 0), time.Time{}, time.Time{}))
	want.HeldSince = at(10, 0)
	check("the clock starts", want)
	must(s.PutReleaseHold(ctx, "sf/a", at(10, 0), at(10, 20), time.Time{}))
	want.AskedAt = at(10, 20)
	check("the ask", want)

	must(s.SetDeployNow(ctx, "sf/a", "c9", at(10, 30)))
	want.DeployNow, want.HeldSince, want.AskedAt = "c9", at(10, 30), time.Time{}
	check("Deploy now", want)
	must(s.PutReleaseHold(ctx, "sf/a", at(10, 0), at(10, 35), at(10, 20)))
	check("a pass that read the row before the press", want)
	must(s.PutReleaseHold(ctx, "sf/a", at(10, 30), at(10, 40), time.Time{}))
	want.AskedAt = at(10, 40)
	check("an ask after the press", want)
	must(s.PutReleaseHold(ctx, "sf/a", at(10, 30), at(10, 45), time.Time{}))
	check("a stale read once more", want)

	must(s.ClearDeployNow(ctx, "sf/a", "c8"))
	check("ClearDeployNow of another commit", want)
	must(s.ClearDeployNow(ctx, "sf/a", "c9"))
	want.DeployNow = ""
	check("ClearDeployNow", want)
	must(s.ClearDeployNow(ctx, "sf/none", "c9"))

	must(s.SetDeployNow(ctx, "sf/a", "c9", at(11, 0)))
	must(s.EndReleaseHold(ctx, "sf/a"))
	want.HeldSince, want.AskedAt = time.Time{}, time.Time{}
	check("the release", want)

	must(s.ClearToldHeld(ctx, "sf/a", "c1"))
	check("ClearToldHeld of another commit", want)
	must(s.ClearToldHeld(ctx, "sf/a", "c2"))
	want.ToldHeld = ""
	check("ClearToldHeld", want)

	must(s.PutReleaseHold(ctx, "sf/c", at(10, 0), at(10, 20), at(9, 0)))
	newHold := Release{Repository: "sf/c", HeldSince: at(10, 0), AskedAt: at(10, 20)}
	check("PutReleaseHold on no row", newHold)
	must(s.SetDeployNow(ctx, "sf/d", "d9", at(11, 0)))
	newDeploy := Release{Repository: "sf/d", HeldSince: at(11, 0), DeployNow: "d9"}
	check("SetDeployNow on no row", newDeploy)

	rows, err := s.Releases(ctx)
	must(err)
	for i := range rows {
		rows[i] = utcRelease(rows[i])
	}
	if all := []Release{want, {Repository: "sf/b", AsksAt: at(8, 0)}, newHold, newDeploy}; !reflect.DeepEqual(rows, all) {
		t.Errorf("Releases = %+v, want %+v", rows, all)
	}
}

// checkFixesMergedSince: the fixes of sf/a merged after 10:00 are the
// alert (its merge event at 10:30), the chain-repair merged with no event
// (its finish at 10:50 stands in) and the mainfix (11:00), in that order.
// The alert and the mainfix finished after their merges, in the other
// order; the first chain-repair merged at 09:00 and finished after 10:00.
// The revert, the webhook card and sf/b's alert are no fixes of sf/a, and
// the failed alert never merged.
func checkFixesMergedSince(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	at := releaseAt
	for i, c := range []struct {
		id, repository, source, status, commit string
		merged, finished                       time.Time // merged: the merge event's, zero for none
	}{
		{"f1", "sf/a", SourceAlert, "done", "a1", at(10, 30), at(11, 30)},
		{"f2", "sf/a", SourceMainFix, "done", "a2", at(11, 0), at(11, 5)},
		{"f3", "sf/a", SourceChainRepair, "done", "a3", at(9, 0), at(10, 10)},
		{"f4", "sf/a", SourceRevert, "done", "a4", at(10, 45), at(10, 46)},
		{"f5", "sf/a", "webhook", "done", "a5", at(10, 40), at(10, 41)},
		{"f6", "sf/b", SourceAlert, "done", "b6", at(10, 50), at(10, 51)},
		{"f7", "sf/a", SourceChainRepair, "done", "a7", time.Time{}, at(10, 50)},
		{"f8", "sf/a", SourceAlert, "failed", "", time.Time{}, at(10, 55)},
	} {
		issue := int64(31 + i)
		if err := s.CreateTask(ctx, Task{ID: c.id, BudgetEUR: 1, Status: c.status, Repository: c.repository, Source: c.source,
			Track: policy.TrackDirect, IssueNumber: issue, CreatedAt: c.finished}); err != nil {
			t.Fatal(err)
		}
		if c.commit != "" {
			if err := s.SetTaskMergeCommit(ctx, c.id, c.commit); err != nil {
				t.Fatal(err)
			}
		}
		if !c.merged.IsZero() {
			if _, err := s.Record(ctx, Event{At: c.merged, Repository: c.repository, TaskID: c.id, IssueNumber: issue,
				Kind: EventPullRequestMerged, Actor: MergeUser, Via: "factoryd"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	fixes, err := s.FixesMergedSince(ctx, "sf/a", at(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range fixes {
		ids = append(ids, f.ID)
	}
	if want := []string{"f1", "f7", "f2"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("FixesMergedSince = %v, want %v", ids, want)
	}
}

func utcRelease(r Release) Release {
	r.AsksAt, r.HeldSince, r.AskedAt = r.AsksAt.UTC(), r.HeldSince.UTC(), r.AskedAt.UTC()
	return r
}
