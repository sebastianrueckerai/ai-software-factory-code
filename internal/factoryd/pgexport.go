package factoryd

import (
	"context"
	"database/sql"
)

// The export surface the internal/factoryd/pg test package reaches, as the
// suite split laid it down: accessor methods, constructors and aliases for
// the unexported names the *_pg_test.go files moved there still touch.
// Later move cards extend this file with the names their groups reach.

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
