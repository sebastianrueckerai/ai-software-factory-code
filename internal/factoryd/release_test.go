package factoryd

import (
	"context"
	"reflect"
	"testing"
	"time"

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

	// The fixes of sf/a merged after 10:00: the alert, then the mainfix,
	// by their merge events. Each card's finish is later than its merge
	// and in the other order; the chain-repair's finish is after 10:00,
	// its merge before.
	for _, q := range []string{
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, issue_number, merge_commit, finished_at) VALUES
			('f1', 1, 0, 'done', 'sf/a', 'agent', 'alert', 'direct', 31, 'a31', '2026-11-08T11:30:00Z'),
			('f2', 1, 0, 'done', 'sf/a', 'agent', 'mainfix', 'direct', 32, 'a32', '2026-11-08T11:05:00Z'),
			('f3', 1, 0, 'done', 'sf/a', 'agent', 'chain-repair', 'direct', 33, 'a33', '2026-11-08T10:10:00Z'),
			('f4', 1, 0, 'done', 'sf/a', 'agent', 'revert', 'direct', 34, 'a34', '2026-11-08T10:46:00Z'),
			('f5', 1, 0, 'done', 'sf/a', 'agent', 'webhook', 'direct', 35, 'a35', '2026-11-08T10:41:00Z'),
			('f6', 1, 0, 'done', 'sf/b', 'agent', 'alert', 'direct', 36, 'b36', '2026-11-08T10:51:00Z')`,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via) VALUES
			('2026-11-08T10:30:00Z', 'sf/a', 'f1', 31, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-11-08T11:00:00Z', 'sf/a', 'f2', 32, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-11-08T09:00:00Z', 'sf/a', 'f3', 33, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-11-08T10:45:00Z', 'sf/a', 'f4', 34, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-11-08T10:40:00Z', 'sf/a', 'f5', 35, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-11-08T10:50:00Z', 'sf/b', 'f6', 36, 'pull_request_merged', 'factory-merge', 'factoryd')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	fixes, err := s.FixesMergedSince(ctx, "sf/a", releaseAt(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range fixes {
		ids = append(ids, f.ID)
	}
	if want := []string{"f1", "f2"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("FixesMergedSince = %v, want %v", ids, want)
	}
}

func TestFakeStoreReleases(t *testing.T) {
	checkReleaseRows(t, newFakeStore())
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
		if got := read("sf/a"); !reflect.DeepEqual(got, want) {
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

	rows, err := s.Releases(ctx)
	must(err)
	for i := range rows {
		rows[i] = utcRelease(rows[i])
	}
	if all := []Release{want, {Repository: "sf/b", AsksAt: at(8, 0)}}; !reflect.DeepEqual(rows, all) {
		t.Errorf("Releases = %+v, want %+v", rows, all)
	}
}

func utcRelease(r Release) Release {
	r.AsksAt, r.HeldSince, r.AskedAt = r.AsksAt.UTC(), r.HeldSince.UTC(), r.AskedAt.UTC()
	return r
}
