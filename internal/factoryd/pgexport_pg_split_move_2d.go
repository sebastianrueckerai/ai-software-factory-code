package factoryd

import (
	"database/sql"
	"maps"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The export surface of the second move (pg-split-move-2d): the names the
// draws, heal_ask, offers, old_records and sessionkill *_pg_test.go files
// reach in factoryd as they moved into internal/factoryd/pg. Test-file
// helpers cannot cross the package line; those moved as verbatim copies in
// the pg package's own helpers instead.

// PgRigRunner builds a Runner over its unexported store, objects, cfg,
// timeline and forgejo fields, for the moved rigs that constructed one
// directly.
func PgRigRunner(store Store, objects ObjectStore, cfg RunnerConfig, timeline *Timeline, forgejo *Forgejo) *Runner {
	return &Runner{store: store, objects: objects, cfg: cfg, timeline: timeline, forgejo: forgejo}
}

// PgRungsOff is the moved rungsOff: v with every project at manual under
// escalation judgment autonomous_and_above, so no rung judges and a stop
// faces the human at once.
func PgRungsOff(r *Runner) {
	v := r.cfg.Policy()
	projects := maps.Clone(v.Projects)
	for name, p := range projects {
		p.Autonomy = policy.AutonomyManual
		p.Escalation = &policy.Escalation{Judgment: policy.JudgmentAutonomousAndAbove}
		projects[name] = p
	}
	v.Projects = projects
	r.cfg.Policy = func() policy.Version { return v }
}

// PgCfg exposes the runner's unexported cfg, for the moved heal-pass tests
// that read the policy and workflows their runner runs.
func (r *Runner) PgCfg() RunnerConfig { return r.cfg }

// PgForgejo exposes the runner's unexported forgejo client, for the moved
// heal-pass tests that pass it to the intake they run.
func (r *Runner) PgForgejo() *Forgejo { return r.forgejo }

// PgSetDB swaps the store's database handle and returns the old one, for
// the moved test that counts the statements the desk sends.
func (s *PGStore) PgSetDB(db *sql.DB) *sql.DB {
	old := s.db
	s.db = db
	return old
}

// PgTripAttempt aliases the ladder's unexported tripAttempt, whose fields
// the moved trip tests set.
type PgTripAttempt = tripAttempt
