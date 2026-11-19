package factoryd

import (
	"context"
	"database/sql"
)

// The export surface the internal/factoryd/pg test package reaches, as the
// suite split laid it down: accessor methods, constructors and aliases for
// the unexported names the *_pg_test.go files moved there still touch.
// Later move cards extend this file with the names their groups reach.

// PgCardHeader exposes cardHeader: the X-Factory-Card value a check-in
// client sends for the card ctx carries.
func PgCardHeader(ctx context.Context) string { return cardHeader(ctx) }

// DB returns the store's database handle, for the tests moved into the pg
// package that read and write a store's rows directly.
func (s *PGStore) DB() *sql.DB { return s.db }

// PgInsertTask exposes the store's InsertTask (main exported insertTask
// with its own list) to the moved tests that inserted rows directly.
func (s *PGStore) PgInsertTask(ctx context.Context, t Task, onConflict string) error {
	return s.InsertTask(ctx, t, onConflict)
}

// PgRunner builds a Runner over its unexported store, objects and cfg
// fields, for the moved tests that constructed one directly.
func PgRunner(store Store, objects ObjectStore, cfg RunnerConfig) *Runner {
	return &Runner{store: store, objects: objects, cfg: cfg}
}

// Aliases of the names the moved tests call. main renamed the first four
// onto the package's export surface when it landed its own export list;
// holdsSlot joined them when main exported its next measured slice. The
// aliases keep the pg package's shim stable for the later move cards.
var (
	PgHoldsSlot = HoldsSlot
	PgToInt     = toInt
	PgErrHeld   = errHeld
)

// Aliases of the constants the moved tests read.
const (
	PgBriefTerminal   = BriefTerminal
	PgCapReached      = CapReached
	PgConsoleApproved = ConsoleApproved
	PgLeftQueue       = leftQueue
)

// PgFaceStop is the tests' door into the human face of a stop, as Climb
// faces it once rung 2 handed it up: the case opens as OpenTrip opens it, or
// for a restart the card's newest live ask takes the ask, a step of rung 2
// hands it up, and the face is production's (Face). No rung runs.
func (r *Runner) PgFaceStop(ctx context.Context, taskID, reason string, choices []string) (int64, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var c Case
	if reason == HealedReason {
		c, err = r.store.NewestAskCase(ctx, t.ID)
	} else {
		var f CaseFacts
		if f, err = r.tripFacts(ctx, t, "", ""); err == nil {
			if c, _, err = r.tripCase(ctx, t, f, TripInput{TaskID: taskID, Reason: reason, Choices: choices}); err == nil {
				c, _, err = r.store.OpenCase(ctx, c)
			}
		}
	}
	if err != nil {
		return 0, err
	}
	if _, err := r.store.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp}); err != nil {
		return 0, err
	}
	out, err := r.Face(ctx, t, c, reason, replyNext(reason), choices, nil)
	return out.Comment, err
}
