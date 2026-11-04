package factoryd

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// caseWaitStores are the stores whose case wait rows must agree: Postgres,
// and the fake the workflow tests read.
var caseWaitStores = map[string]func(*testing.T) Store{
	"postgres": func(t *testing.T) Store { return caseStore(t) },
	"fake":     func(*testing.T) Store { return &fakeStore{} },
}

// A live case whose wake passed is due, the oldest wake first, open or at
// the human. One due in an hour, one with no wake and a resolved one are
// not, and a cleared wake is no longer due.
func TestSetCaseWakeAndDueCases(t *testing.T) {
	for name, open := range caseWaitStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
			opened := func(key, status string) Case {
				c, ok, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: key, Status: status, Problem: key})
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
			if never.OwnerRung != 3 || never.NextWakeAt != nil || len(never.WakeOn) != 0 {
				t.Fatalf("a new case = rung %d, wake %v on %v; want rung 3, no wake, no events", never.OwnerRung, never.NextWakeAt, never.WakeOn)
			}
			wake(due, 0, []string{"card_moved", "case_answered"}, at(-time.Minute))
			wake(later, 0, nil, at(time.Hour))
			wake(done, 0, nil, at(-2*time.Minute))
			wake(earlier, 3, nil, at(-3*time.Minute))
			if err := s.ResolveCase(ctx, done.ID, CaseResolution{By: FactoryDecider, Rung: 0, Action: "wait"}, nil, nil); err != nil {
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
			if err := s.SetCaseWake(ctx, earlier.ID+1000, 0, nil, nil); !errors.Is(err, ErrNotFound) {
				t.Fatalf("SetCaseWake on no case = %v, want ErrNotFound", err)
			}
		})
	}
}

// A step in progress past its due_at hands up and names its bound, the
// due_at less its start: 35m and 1h30m. A step with no due_at, one due
// later and one that ended are untouched, and an expired step is not
// expired again.
func TestExpireStepsHandsUp(t *testing.T) {
	for name, open := range caseWaitStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			start := time.Now().UTC().Truncate(time.Millisecond)
			at := func(d time.Duration) *time.Time { v := start.Add(d); return &v }
			c, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: "k", Problem: "a flag"})
			if err != nil {
				t.Fatal(err)
			}
			step := func(due *time.Time, outcome string) int64 {
				id, err := s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 0, Actor: FactoryDecider, DueAt: due, Outcome: outcome})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			rung := step(at(35*time.Minute), "")
			unbound := step(nil, StepInProgress)
			hour := step(at(90*time.Minute), StepInProgress)
			two := step(at(2*time.Hour), StepInProgress)
			ended := step(at(10*time.Minute), StepResolved)

			expired := func(now time.Time, id int64, found string, due time.Time) {
				t.Helper()
				got, err := s.ExpireSteps(ctx, now)
				if err != nil || len(got) != 1 {
					t.Fatalf("ExpireSteps at %s = %+v, %v; want step %d alone", now, got, err, id)
				}
				if st := got[0]; st.ID != id || st.CaseID != c.ID || st.Outcome != StepHandedUp || st.Found != found ||
					st.EndedAt == nil || st.DueAt == nil || !st.DueAt.Equal(due) {
					t.Fatalf("expired step = %+v, want step %d handed up with %q, due %s", st, id, found, due)
				}
			}
			expired(start.Add(36*time.Minute), rung, "the rung did not answer within 35m", start.Add(35*time.Minute))
			expired(start.Add(91*time.Minute), hour, "the rung did not answer within 1h30m", start.Add(90*time.Minute))

			steps, err := s.CaseSteps(ctx, c.ID)
			if err != nil || len(steps) != 5 {
				t.Fatalf("CaseSteps = %+v, %v; want five", steps, err)
			}
			byID := map[int64]CaseStep{}
			for _, st := range steps {
				byID[st.ID] = st
			}
			for id, want := range map[int64]string{unbound: StepInProgress, two: StepInProgress, ended: StepResolved} {
				if st := byID[id]; st.Outcome != want || st.Found != "" {
					t.Errorf("step %d = %+v, want it untouched, %s", id, st, want)
				}
			}
			if st := byID[unbound]; st.DueAt != nil || st.EndedAt != nil {
				t.Errorf("the step with no due_at = %+v, want no due_at and not ended", st)
			}
		})
	}
}
