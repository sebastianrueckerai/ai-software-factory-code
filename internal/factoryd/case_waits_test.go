package factoryd

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// caseWaitStores are the stores whose case wait rows must agree: the fake the
// workflow tests read here, and Postgres in internal/factoryd/pg.
var caseWaitStores = map[string]func(*testing.T) Store{
	"fake": func(*testing.T) Store { return &fakeStore{} },
}

// A live case whose wake passed is due, the oldest wake first, open or at
// the human. One due in an hour, one with no wake, a resolved one and a
// withdrawn one are not, and a cleared wake is no longer due. A rung other
// than 0 to 3 is refused.
func TestSetCaseWakeAndDueCases(t *testing.T) {
	for name, open := range caseWaitStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
			opened := func(key, status string) Case {
				open := s.OpenCase
				if status == CaseAtHuman {
					open = func(ctx context.Context, c Case) (Case, bool, error) { return openAtHuman(ctx, s, c) }
				}
				c, ok, err := open(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: key, Problem: key})
				if err != nil || !ok {
					t.Fatalf("OpenCase %s = %v, %v", key, ok, err)
				}
				return c
			}
			wake := func(c Case, rung int, on []string, when *time.Time) {
				if err := s.SetCaseWake(ctx, c.ID, rung, on, when); err != nil {
					t.Fatalf("SetCaseWake %s: %v", c.StateKey, err)
				}
			}
			due, later, never, done, earlier := opened("due", ""), opened("later", ""), opened("never", ""), opened("done", ""), opened("earlier", CaseAtHuman)
			gone := opened("gone", "")
			if never.OwnerRung != 0 || never.NextWakeAt != nil || len(never.WakeOn) != 0 {
				t.Fatalf("a new case = rung %d, wake %v on %v; want rung 0, no wake, no events", never.OwnerRung, never.NextWakeAt, never.WakeOn)
			}
			wake(due, 0, []string{"card_moved", "case_answered"}, at(-time.Minute))
			wake(later, 0, nil, at(time.Hour))
			wake(done, 0, nil, at(-2*time.Minute))
			wake(earlier, 3, nil, at(-3*time.Minute))
			wake(gone, 0, nil, at(-4*time.Minute))
			if err := s.ResolveCase(ctx, done.ID, CaseResolution{By: FactoryDecider, Rung: 0, Action: "wait"}, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.WithdrawCase(ctx, gone.ID, "card ended"); err != nil {
				t.Fatal(err)
			}

			got, err := s.DueCases(ctx, now, 10)
			if err != nil || !slices.Equal(caseIDs(got), []int64{earlier.ID, due.ID}) {
				t.Fatalf("DueCases = %v, %v; want %d due 3m ago then %d due 1m ago", caseIDs(got), err, earlier.ID, due.ID)
			}
			if c := got[1]; c.OwnerRung != 0 || !slices.Equal(c.WakeOn, []string{"card_moved", "case_answered"}) ||
				c.NextWakeAt == nil || !c.NextWakeAt.Equal(now.Add(-time.Minute)) {
				t.Fatalf("due case = rung %d, on %v, at %v; want rung 0, on card_moved and case_answered, at %s", c.OwnerRung, c.WakeOn, c.NextWakeAt, now.Add(-time.Minute))
			}
			if got, err := s.DueCases(ctx, now, 1); err != nil || !slices.Equal(caseIDs(got), []int64{earlier.ID}) {
				t.Fatalf("DueCases limit 1 = %v, %v; want %d alone", caseIDs(got), err, earlier.ID)
			}

			wake(due, 3, nil, nil)
			if got, err := s.DueCases(ctx, now, 10); err != nil || !slices.Equal(caseIDs(got), []int64{earlier.ID}) {
				t.Fatalf("DueCases after the clear = %v, %v; want %d alone", caseIDs(got), err, earlier.ID)
			}
			if c, err := s.CaseByID(ctx, due.ID); err != nil || c.OwnerRung != 3 || c.NextWakeAt != nil || len(c.WakeOn) != 0 {
				t.Fatalf("cleared case = %+v, %v; want rung 3, no wake, no events", c, err)
			}
			if err := s.SetCaseWake(ctx, done.ID, 0, nil, at(time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("SetCaseWake on a resolved case = %v, want ErrNotFound", err)
			}
			if err := s.SetCaseWake(ctx, gone.ID, 0, nil, at(time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("SetCaseWake on a withdrawn case = %v, want ErrNotFound", err)
			}
			for _, rung := range []int{-1, 4} {
				if err := s.SetCaseWake(ctx, earlier.ID, rung, nil, nil); err == nil || errors.Is(err, ErrNotFound) {
					t.Fatalf("SetCaseWake to rung %d = %v, want a refusal", rung, err)
				}
			}
			if err := s.SetCaseWake(ctx, earlier.ID+1000, 0, nil, nil); !errors.Is(err, ErrNotFound) {
				t.Fatalf("SetCaseWake on no case = %v, want ErrNotFound", err)
			}
		})
	}
}

// An open case is rung 0's from its opening and takes the wake its opener
// gives it: one opened with a wake at t0 reads rung 0 and is due at t0,
// one opened with none reads rung 0 and is never due, and one the door
// moves to the human after rung 2's hand-up is the human's, rung 3, with
// no wake. A case opened at the human is refused: only the door moves it
// there.
func TestAnOpenedCaseIsRungZerosWithItsOpenersWake(t *testing.T) {
	for name, open := range caseWaitStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			opened := func(key string, wake *time.Time) Case {
				c, ok, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: CaseOther, StateKey: key, Problem: key, NextWakeAt: wake})
				if err != nil || !ok {
					t.Fatalf("OpenCase %s = %v, %v", key, ok, err)
				}
				return c
			}
			woken, bare, faced := opened("woken", &t0), opened("bare", nil), opened("faced", &t0)
			if _, err := s.AppendCaseStep(ctx, CaseStep{CaseID: faced.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp}); err != nil {
				t.Fatal(err)
			}
			if err := s.ToHuman(ctx, faced.ID); err != nil {
				t.Fatalf("ToHuman after rung 2's hand-up: %v", err)
			}
			const refused = "OpenCase opens a case only as open: the door moves it to the human"
			if _, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: CaseOther, StateKey: "at once", Status: CaseAtHuman, Problem: "p"}); err == nil || err.Error() != refused {
				t.Fatalf("OpenCase at the human = %v, want %q", err, refused)
			}
			for _, c := range []Case{woken, caseOf(t, s, woken.ID)} {
				if c.OwnerRung != 0 || c.NextWakeAt == nil || !c.NextWakeAt.Equal(t0) {
					t.Fatalf("the case opened with a wake = rung %d, wake %v; want rung 0, due at %s", c.OwnerRung, c.NextWakeAt, t0)
				}
			}
			if c := caseOf(t, s, bare.ID); c.OwnerRung != 0 || c.NextWakeAt != nil {
				t.Fatalf("the case opened with no wake = rung %d, wake %v; want rung 0, no wake", c.OwnerRung, c.NextWakeAt)
			}
			if c := caseOf(t, s, faced.ID); c.Status != CaseAtHuman || c.OwnerRung != 3 || c.NextWakeAt != nil {
				t.Fatalf("the case the door moved = %s at rung %d, wake %v; want at_human at rung 3, no wake", c.Status, c.OwnerRung, c.NextWakeAt)
			}
			if got, err := s.DueCases(ctx, t0, 10); err != nil || !slices.Equal(caseIDs(got), []int64{woken.ID}) {
				t.Fatalf("DueCases at t0 = %v, %v; want %d alone", caseIDs(got), err, woken.ID)
			}
			if got, err := s.DueCases(ctx, t0.Add(-time.Millisecond), 10); err != nil || len(got) != 0 {
				t.Fatalf("DueCases before t0 = %v, %v; want none", caseIDs(got), err)
			}
		})
	}
}

func caseOf(t *testing.T, s Store, id int64) Case {
	t.Helper()
	c, err := s.CaseByID(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseByID %d: %v", id, err)
	}
	return c
}

// A step in progress past its due_at hands up and names its bound, the
// due_at less its start: 35m, 1h30m and 2h, after the finding it carried.
// A step with no due_at, one due later, one that ended and the steps of a
// resolved and a withdrawn case are untouched, and an expired step is not
// expired again. Step ids are unique across cases.
func TestExpireStepsHandsUp(t *testing.T) {
	for name, open := range caseWaitStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			start := time.Now().UTC().Truncate(time.Millisecond)
			at := func(d time.Duration) *time.Time { v := start.Add(d); return &v }
			opened := func(key string) Case {
				c, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: key, Problem: "a flag"})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			var ids []int64
			step := func(c Case, due *time.Time, outcome, found string) int64 {
				id, err := s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 0, Actor: FactoryDecider, DueAt: due, Outcome: outcome, Found: found})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				return id
			}
			c := opened("k")
			rung := step(c, at(35*time.Minute), "", "")
			unbound := step(c, nil, StepInProgress, "")
			hour := step(c, at(90*time.Minute), StepInProgress, "asked again after a restart")
			two := step(c, at(2*time.Hour), StepInProgress, "")
			ended := step(c, at(10*time.Minute), StepResolved, "")
			resolved, withdrawn, other := opened("resolved"), opened("withdrawn"), opened("other")
			ofResolved, ofWithdrawn := step(resolved, at(10*time.Minute), "", ""), step(withdrawn, at(10*time.Minute), "", "")
			ofOther := step(other, at(2*time.Hour), "", "")
			if err := s.ResolveCase(ctx, resolved.ID, CaseResolution{By: FactoryDecider, Rung: 0, Action: "wait"}, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.WithdrawCase(ctx, withdrawn.ID, "card ended"); err != nil {
				t.Fatal(err)
			}
			if unique := slices.Compact(slices.Sorted(slices.Values(ids))); len(unique) != len(ids) {
				t.Fatalf("step ids = %v, want each once", ids)
			}

			type expiry struct {
				id    int64
				found string
				due   time.Time
			}
			expired := func(now time.Time, want ...expiry) {
				t.Helper()
				got, err := s.ExpireSteps(ctx, now)
				if err != nil || len(got) != len(want) {
					t.Fatalf("ExpireSteps at %s = %+v, %v; want %+v", now, got, err, want)
				}
				for i, w := range want {
					if st := got[i]; st.ID != w.id || st.Outcome != StepHandedUp || st.Found != w.found ||
						st.EndedAt == nil || st.DueAt == nil || !st.DueAt.Equal(w.due) {
						t.Fatalf("expired step = %+v, want step %d handed up with %q, due %s", st, w.id, w.found, w.due)
					}
				}
			}
			expired(start.Add(36*time.Minute), expiry{rung, "the rung did not answer within 35m", start.Add(35 * time.Minute)})
			expired(start.Add(91*time.Minute),
				expiry{hour, "asked again after a restart, then the rung did not answer within 1h30m", start.Add(90 * time.Minute)})
			expired(start.Add(121*time.Minute), expiry{two, "the rung did not answer within 2h", start.Add(2 * time.Hour)},
				expiry{ofOther, "the rung did not answer within 2h", start.Add(2 * time.Hour)})

			byID := map[int64]CaseStep{}
			for _, k := range []Case{c, resolved, withdrawn} {
				steps, err := s.CaseSteps(ctx, k.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, st := range steps {
					byID[st.ID] = st
				}
			}
			for id, want := range map[int64]string{unbound: StepInProgress, ended: StepResolved, ofResolved: StepResolved, ofWithdrawn: StepWithdrawn} {
				if st := byID[id]; st.Outcome != want || st.Found != "" {
					t.Errorf("step %d = %+v, want it untouched by the expiry, %s", id, st, want)
				}
			}
			if st := byID[unbound]; st.DueAt != nil || st.EndedAt != nil {
				t.Errorf("the step with no due_at = %+v, want no due_at and not ended", st)
			}
			for _, id := range []int64{ofResolved, ofWithdrawn} {
				if byID[id].EndedAt == nil {
					t.Errorf("step %d of an ended case = %+v, want it ended with its case", id, byID[id])
				}
			}
		})
	}
}
