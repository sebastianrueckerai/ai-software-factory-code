package factoryd

import (
	"context"
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

// TestDivideConquerHoldRetries keeps one fact per actual hold through
// the public paths alone: repeated passes over overlapping open plans
// run one clock, a pass that finds a hold already ended writes no
// second row, and each new hold or Deploy now restart is its own
// interval, however often passes and presses repeat.
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
	// both plans closed: the hold ends once, and the pass after its end
	// writes nothing
	store.closed["t-plan"] = keeperAt(17, 0)
	store.closed["t-zplan"] = keeperAt(17, 30)
	*clock = keeperAt(18, 0)
	keeperPass(t, k)
	*clock = keeperAt(18, 1)
	keeperPass(t, k)
	if n := len(releasedRows(store)); n != 1 {
		t.Fatalf("%d facts after the hold's end and a pass after it; want one", n)
	}
	// a new hold, ended by Deploy now, and a second press on its restart
	openPlanIn(store, demoRepo, "t-next", 13, keeperAt(18, 30))
	*clock = keeperAt(18, 31)
	keeperPass(t, k)
	*clock = keeperAt(20, 30)
	if _, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member"); err != nil {
		t.Fatal(err)
	}
	*clock = keeperAt(20, 45)
	if _, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member"); err != nil {
		t.Fatal(err)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(20, 45), DeployNow: demoCommit}); !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v: Deploy now restarted the clock", got, want)
	}
	// the restarted hold ends too, and the pass after writes no second row
	store.closed["t-next"] = keeperAt(20, 50)
	*clock = keeperAt(21, 0)
	keeperPass(t, k)
	*clock = keeperAt(21, 1)
	keeperPass(t, k)

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
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0)}); !reflect.DeepEqual(got, want) {
		t.Errorf("row %+v, want %+v: the last hold ended with the repository unheld", got, want)
	}
}

// TestReleaseKeeperHoldFactRetry holds the durability boundary of the
// hold's fact: the production_released row is written before the
// releases row it closes changes, and a write the store refuses is
// returned, never logged away, so the public retry recovers the
// interval exactly once. A pass whose fact was refused ends nothing —
// the releases row holds as before and the next pass writes the same
// identity, one row. A press whose fact was refused changes nothing —
// no commit, the clock keeps running — and the next press lands the
// same interval once, its hours, commit, decider, actor and via
// intact.
func TestReleaseKeeperHoldFactRetry(t *testing.T) {
	ctx := context.Background()
	f := &chainForgejo{runs: map[string][]map[string]any{demoRepo: {pushRun(21, demoCommit, "success")}},
		jobs: map[int64][]map[string]any{21: {actionJob(211, "checks", "success", 1), actionJob(212, "promote", "success", 1)}}}
	k, store, clock := newKeeper(t, f)
	// the plan open: the pass starts the hold's clock at its first merge
	*clock = keeperAt(11, 0)
	keeperPass(t, k)
	if got := releaseRow(store, demoRepo).HeldSince; !got.Equal(keeperAt(10, 0)) {
		t.Fatalf("held since %v, want the first merge 10:00", got)
	}
	// the plan closes and the fact's write is refused: the pass says so
	// and the hold stands exactly as it was
	store.closed["t-plan"] = keeperAt(17, 0)
	store.recordFails = map[string]int{EventProductionReleased: 1}
	*clock = keeperAt(18, 0)
	if err := k.Pass(ctx); err == nil || !strings.Contains(err.Error(), "record production_released") {
		t.Fatalf("pass = %v, want the refused fact as the pass's error", err)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(10, 0)}); !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v: the refused fact left the hold standing", got, want)
	}
	if n := len(releasedRows(store)); n != 0 {
		t.Fatalf("%d facts after the refused write; want none", n)
	}
	// the next pass retries the same identity and lands it once
	store.recordFails = nil
	keeperPass(t, k)
	want := []Event{
		{At: keeperAt(18, 0), Repository: demoRepo, Kind: EventProductionReleased, Actor: "factoryd", Via: ViaFactoryd,
			Detail:      map[string]any{"how": "closed", "held_since": "2026-10-29T10:00:00Z", "hours": 8.0},
			IdentityKey: "production_released sf/demo-app 2026-10-29T10:00:00Z"},
	}
	if got := releasedRows(store); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts %+v, want the retried end once %+v", got, want)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0)}); !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v: the retry ended the hold", got, want)
	}
	// a new hold's Deploy now press, its fact's write refused: no
	// commit, the clock keeps running, the row unchanged
	openPlanIn(store, demoRepo, "t-next", 13, keeperAt(18, 30))
	*clock = keeperAt(18, 31)
	keeperPass(t, k)
	store.recordFails = map[string]int{EventProductionReleased: 1}
	*clock = keeperAt(20, 30)
	commit, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member")
	if err == nil || !strings.Contains(err.Error(), "record production_released") || commit != "" {
		t.Fatalf("DeployNow = %q, %v; want no commit and the refused fact as the press's error", commit, err)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(18, 30)}); !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v: the refused press changed nothing", got, want)
	}
	// the next press lands the interval once, its linkage intact
	store.recordFails = nil
	if commit, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton, "member"); err != nil || commit != demoCommit {
		t.Fatalf("DeployNow = %q, %v; want %s", commit, err, demoCommit)
	}
	want = append(want, Event{At: keeperAt(20, 30), Repository: demoRepo, Kind: EventProductionReleased, Actor: "sebastian", Via: ViaButton,
		Detail:      map[string]any{"how": "deploy_now", "held_since": "2026-10-29T18:30:00Z", "hours": 2.0, "commit": demoCommit, "decider": "member"},
		IdentityKey: "production_released sf/demo-app 2026-10-29T18:30:00Z"})
	if got := releasedRows(store); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts %+v, want one row per hold %+v", got, want)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(20, 30), DeployNow: demoCommit}); !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v: the retried press restarted the clock at the commit", got, want)
	}
}
