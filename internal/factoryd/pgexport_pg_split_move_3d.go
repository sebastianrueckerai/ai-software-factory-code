package factoryd

import (
	"context"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"

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

// PgBoughtNothing exposes the actions' unexported boughtNothing, for the
// moved act tests that pin what a fund press bought.
func PgBoughtNothing(f CaseFacts) bool { return boughtNothing(f) }

// PgTripCase runs the runner's unexported tripCase, for the moved act test
// that pins the case a repeat check-in stop opens.
func (r *Runner) PgTripCase(ctx context.Context, t Task, f CaseFacts, in TripInput) (Case, []Case, error) {
	return r.tripCase(ctx, t, f, in)
}

// PgSetMetrics sets the runner's unexported cfg's meter, for the moved act
// test that counts its grant's intervention.
func (r *Runner) PgSetMetrics(m *telemetry.Metrics) { r.cfg.Metrics = m }

// PgSetCompletion sets the runner's unexported completion, for the moved
// act test whose salvage close runs the console's issue close.
func (r *Runner) PgSetCompletion(c *Completion) { r.completion = c }

// PgSetKube sets the runner's unexported kube, for the moved act test
// whose salvage close runs against an empty cluster.
func (r *Runner) PgSetKube(k Kube) { r.kube = k }

// PgSetForgejo sets the runner's unexported forgejo client, for the moved
// deploy tests whose Act posts on the chain fake's Forgejo.
func (r *Runner) PgSetForgejo(f *Forgejo) { r.forgejo = f }

// PgPoolDraws aliases the pool's draw rows' SQL, for the moved propose
// tests that count them directly.
const PgPoolDraws = policy.PoolDrawsSQL

// PgMonitorFlagPrefix aliases the questions' unexported monitorFlagPrefix,
// for the moved propose tests that build a monitor flag's reason.
const PgMonitorFlagPrefix = monitorFlagPrefix

// PgNoJudgmentLine aliases the ladder's unexported noJudgmentLine, for the
// moved propose tests that pin the spent judgment budget's words.
const PgNoJudgmentLine = noJudgmentLine

// PgSetStore sets the runner's unexported store, for the moved propose
// tests whose rung 2 reads the project's rows through the project store.
func (r *Runner) PgSetStore(s Store) { r.store = s }
