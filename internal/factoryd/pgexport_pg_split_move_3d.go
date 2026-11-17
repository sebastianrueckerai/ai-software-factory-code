package factoryd

import (
	"context"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The export surface of the third move round (pg-split-move-3d): the names
// the act family's *_pg_test.go files reach in factoryd as they move into
// internal/factoryd/pg, one slice per moved file, smallest first.

// PgWaitRungs waits for the runner's rung 1 conversations, the moved board
// tests' settle.
func (r *Runner) PgWaitRungs() { r.rungs.Wait() }

// PgSetPolicy replaces the runner's unexported cfg's policy read, for the
// moved board tests that pin the policy version a case's facts carry.
func (r *Runner) PgSetPolicy(p func() policy.Version) { r.cfg.Policy = p }

// PgSetPolicyVersion replaces the runner's unexported cfg's policy version
// read, for the moved board tests that script how a pinned version answers.
func (r *Runner) PgSetPolicyVersion(f func(ctx context.Context, id int64) (policy.Version, bool, error)) {
	r.cfg.PolicyVersion = f
}
