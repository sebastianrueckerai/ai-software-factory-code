package factoryd

import "context"

// The export surface of the last move round (pg-split-move-3g): the names
// the console_store, judgment_budget, store, case_waits and queue_reason
// *_pg_test.go files reach in factoryd as they move into
// internal/factoryd/pg.

// PgSpentJudgment runs the runner's unexported spentJudgment, for the moved
// judgment budget tests that read the hand-up a spent budget makes.
func (r *Runner) PgSpentJudgment(ctx context.Context, t Task, c Case) (*HandUp, error) {
	return r.spentJudgment(ctx, t, c)
}

// PgOnce and PgReconcile run the chain poller's unexported once and
// reconcile, for the moved console store test that drives one poll.
func (p *ChainPoller) PgOnce() { p.once() }

func (p *ChainPoller) PgReconcile(ctx context.Context, repository string) {
	p.reconcile(ctx, repository)
}

// PgMarkJudgmentSpent and PgMarkJudgmentUnread alias the unexported marks a
// face's problem line ends with when the judgment budget is spent or
// unreadable.
const (
	PgMarkJudgmentSpent  = markJudgmentSpent
	PgMarkJudgmentUnread = markJudgmentUnread
)

// PgStalled runs the chain poller's unexported stalled, for the moved console
// store test that stalls one commit.
func (p *ChainPoller) PgStalled(ctx context.Context, repository, sha string) {
	p.stalled(ctx, repository, sha)
}
