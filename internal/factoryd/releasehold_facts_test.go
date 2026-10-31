package factoryd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDivideConquerHoldFacts records what a repository's holds cost, each
// with the instant it began: the first hold starts at the plans' first
// merge and ends naturally; the next ends by Deploy now, whose clock
// restart opens a third, still-active interval. The facts carry
// fractional timestamps and hours precisely, and a project whose CI
// never asks /release is not held at all.
func TestDivideConquerHoldFacts(t *testing.T) {
	ctx := context.Background()
	f := &chainForgejo{runs: map[string][]map[string]any{demoRepo: {pushRun(21, demoCommit, "success")}},
		jobs: map[int64][]map[string]any{21: {actionJob(211, "checks", "success", 1), actionJob(212, "promote", "success", 1)}}}
	k, store, clock := newKeeper(t, f)
	openPlanIn(store, "sf/other", "t-other", 5, keeperAt(9, 0)) // open, but its CI never asks /release
	// the first hold begins at 10:30:00.5, the earliest merge under the plan
	start := keeperAt(10, 30).Add(500 * time.Millisecond)
	for _, id := range []string{"t-a", "t-d"} {
		card := store.tasks[id]
		card.FinishedAt = &start
		store.tasks[id] = card
	}
	*clock = keeperAt(11, 0)
	keeperPass(t, k)
	if got := releaseRow(store, demoRepo).HeldSince; !got.Equal(start) {
		t.Fatalf("held since %v, want the first merge %v", got, start)
	}
	// the plan closed: the next pass ends the hold 1.5 hours after it began
	store.closed["t-plan"] = keeperAt(11, 30)
	*clock = start.Add(90 * time.Minute)
	keeperPass(t, k)
	// a new plan opens a new hold, and Deploy now ends it 2.25 hours in
	openPlanIn(store, demoRepo, "t-next", 13, keeperAt(13, 0))
	*clock = keeperAt(13, 5)
	keeperPass(t, k)
	if got := releaseRow(store, demoRepo).HeldSince; !got.Equal(keeperAt(13, 0)) {
		t.Fatalf("held since %v, want 13:00", got)
	}
	*clock = keeperAt(15, 15)
	if commit, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member"); err != nil || commit != demoCommit {
		t.Fatalf("DeployNow = %q, %v; want %s", commit, err, demoCommit)
	}
	// the restarted hold stays open: no third interval until it ends
	*clock = keeperAt(15, 16)
	keeperPass(t, k)

	want := []Event{
		{At: start.Add(90 * time.Minute), Repository: demoRepo, Kind: EventProductionReleased, Actor: "factoryd", Via: ViaFactoryd,
			Detail:      map[string]any{"how": "closed", "held_since": "2026-10-29T10:30:00.5Z", "hours": 1.5},
			IdentityKey: "production_released sf/demo-app 2026-10-29T10:30:00.5Z"},
		{At: keeperAt(15, 15), Repository: demoRepo, Kind: EventProductionReleased, Actor: "sebastian", Via: ViaButton,
			Detail:      map[string]any{"how": "deploy_now", "held_since": "2026-10-29T13:00:00Z", "hours": 2.25, "commit": demoCommit, "decider": "member"},
			IdentityKey: "production_released sf/demo-app 2026-10-29T13:00:00Z"},
	}
	if got := releasedRows(store); !reflect.DeepEqual(got, want) {
		t.Errorf("production_released rows %+v, want %+v", got, want)
	}
	held, err := k.Hold(ctx, demoRepo)
	if err != nil || !held.Held() || !held.Since.Equal(keeperAt(15, 15)) {
		t.Errorf("hold %+v, %v; want production held since 15:15", held, err)
	}
	if row := releaseRow(store, demoRepo); !row.HeldSince.Equal(keeperAt(15, 15)) || row.DeployNow != demoCommit {
		t.Errorf("row %+v; want the open interval held since 15:15 at the deployed commit", row)
	}
	if _, ok := store.releases["sf/other"]; ok || slices.ContainsFunc(releasedRows(store), func(e Event) bool { return e.Repository == "sf/other" }) {
		t.Errorf("sf/other carries a row or a fact; want none: a project whose CI never asks /release is not held")
	}
}

// failEvents refuses the next fail event rows, as a store that cannot
// write them, then serves the store it wraps.
type failEvents struct {
	*fakeStore
	fail int
}

func (s *failEvents) Record(ctx context.Context, e Event) (int64, error) {
	if s.fail > 0 {
		s.fail--
		return 0, errors.New("the events table is gone")
	}
	return s.fakeStore.Record(ctx, e)
}

// TestDivideConquerHoldRetries keeps one fact per actual hold: repeated
// passes over overlapping open plans run one clock, repeated writes of
// the same hold's end — a crash between the releases write and the
// fact's, or its retry — land one row, and each new hold or Deploy now
// is its own interval. A fact the store refuses is said in the log and
// lands on the retry, with no release decision changing.
func TestDivideConquerHoldRetries(t *testing.T) {
	ctx := context.Background()
	f := &chainForgejo{runs: map[string][]map[string]any{demoRepo: {pushRun(21, demoCommit, "success")}},
		jobs: map[int64][]map[string]any{21: {actionJob(211, "checks", "success", 1), actionJob(212, "promote", "success", 1)}}}
	k, store, clock := newKeeper(t, f)
	// two plans open at once: three passes hold one interval
	openPlanIn(store, demoRepo, "t-zplan", 15, keeperAt(10, 45))
	for _, m := range []int{0, 1, 2} {
		*clock = keeperAt(11, m)
		keeperPass(t, k)
	}
	if got := releaseRow(store, demoRepo).HeldSince; !got.Equal(keeperAt(10, 0)) || len(releasedRows(store)) != 0 || len(f.issues) != 0 {
		t.Fatalf("held since %v, %d facts, %d issues; want one clock at 10:00, no fact, no ask", got, len(releasedRows(store)), len(f.issues))
	}
	// both plans closed: the hold ends once, and repeats of its write land
	store.closed["t-plan"] = keeperAt(17, 0)
	store.closed["t-zplan"] = keeperAt(17, 30)
	*clock = keeperAt(18, 0)
	keeperPass(t, k)
	*clock = keeperAt(18, 1)
	keeperPass(t, k)
	k.released(ctx, demoRepo, "closed", keeperAt(10, 0), keeperAt(18, 0), FactoryDecider, ViaFactoryd, "", "")
	k.released(ctx, demoRepo, "closed", keeperAt(10, 0), keeperAt(18, 0), FactoryDecider, ViaFactoryd, "", "")
	// a new hold, ended by Deploy now, and a second press on its restart
	openPlanIn(store, demoRepo, "t-next", 13, keeperAt(18, 30))
	*clock = keeperAt(18, 31)
	keeperPass(t, k)
	*clock = keeperAt(20, 30)
	if _, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member"); err != nil {
		t.Fatal(err)
	}
	k.released(ctx, demoRepo, "deploy_now", keeperAt(18, 30), keeperAt(20, 30), "sebastian", ViaButton, demoCommit, "member")
	*clock = keeperAt(20, 45)
	if _, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member"); err != nil {
		t.Fatal(err)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(20, 45), DeployNow: demoCommit}); !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v: Deploy now restarted the clock", got, want)
	}
	// the store refuses the last fact: the decision stands, the failure is said
	logs := captureLog(t)
	k.Timeline.Events = &failEvents{fakeStore: store, fail: 1}
	store.closed["t-next"] = keeperAt(20, 50)
	*clock = keeperAt(21, 0)
	keeperPass(t, k)
	if !strings.Contains(logs.String(), "event production_released: the row: the events table is gone") {
		t.Errorf("log %q, want the refused fact said", logs.String())
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0)}); !reflect.DeepEqual(got, want) {
		t.Errorf("row %+v, want %+v: the hold ended whatever the fact's write did", got, want)
	}
	// the retry lands the same fact once
	k.released(ctx, demoRepo, "closed", keeperAt(20, 45), keeperAt(21, 0), FactoryDecider, ViaFactoryd, "", "")

	want := []Event{
		{At: keeperAt(18, 0), Repository: demoRepo, Kind: EventProductionReleased, Actor: "factoryd", Via: ViaFactoryd,
			Detail:      map[string]any{"how": "closed", "held_since": "2026-10-29T10:00:00Z", "hours": 8.0},
			IdentityKey: "production_released sf/demo-app 2026-10-29T10:00:00Z"},
		{At: keeperAt(20, 30), Repository: demoRepo, Kind: EventProductionReleased, Actor: "sebastian", Via: ViaButton,
			Detail:      map[string]any{"how": "deploy_now", "held_since": "2026-10-29T18:30:00Z", "hours": 2.0, "commit": demoCommit, "decider": "member"},
			IdentityKey: "production_released sf/demo-app 2026-10-29T18:30:00Z"},
		{At: keeperAt(20, 45), Repository: demoRepo, Kind: EventProductionReleased, Actor: "sebastian", Via: ViaButton,
			Detail:      map[string]any{"how": "deploy_now", "held_since": "2026-10-29T20:30:00Z", "hours": 0.25, "commit": demoCommit, "decider": "member"},
			IdentityKey: "production_released sf/demo-app 2026-10-29T20:30:00Z"},
		{At: keeperAt(21, 0), Repository: demoRepo, Kind: EventProductionReleased, Actor: "factoryd", Via: ViaFactoryd,
			Detail:      map[string]any{"how": "closed", "held_since": "2026-10-29T20:45:00Z", "hours": 0.25},
			IdentityKey: "production_released sf/demo-app 2026-10-29T20:45:00Z"},
	}
	if got := releasedRows(store); !reflect.DeepEqual(got, want) {
		t.Errorf("production_released rows %+v, want one per hold %+v", got, want)
	}
}
