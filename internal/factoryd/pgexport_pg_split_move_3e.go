package factoryd

import (
	"context"
	"time"
)

// The export surface of the third move round (pg-split-move-3e): the names
// the ladder pair's *_pg_test.go files reach in factoryd as they move into
// internal/factoryd/pg.

// PgObjects exposes the runner's unexported objects, for the moved trip
// tests that seed the timeline the runner reads.
func (r *Runner) PgObjects() ObjectStore { return r.objects }

// PgSetObjects sets the runner's unexported objects, for the moved trip
// test that takes its timeline down.
func (r *Runner) PgSetObjects(o ObjectStore) { r.objects = o }

// PgTripFacts runs the runner's unexported tripFacts, for the moved trip
// tests that read the facts of the case a trip would open.
func (r *Runner) PgTripFacts(ctx context.Context, t Task, head, model string) (CaseFacts, error) {
	return r.tripFacts(ctx, t, head, model)
}

// PgRecord runs the runner's unexported record, for the moved trip tests
// that write a timeline step themselves.
func (r *Runner) PgRecord(ctx context.Context, taskID, step string, detail map[string]any) {
	r.record(ctx, taskID, step, detail)
}

// PgRecommendation runs the runner's unexported recommendation, for the
// moved trip tests that read what a hand-up recommends.
func (r *Runner) PgRecommendation(t Task, up *HandUp) string {
	return r.recommendation(t, up)
}

// PgPreselected exposes CaseAction's unexported preselected, for the
// moved trip tests that read an action's preselected args.
func (a CaseAction) PgPreselected() map[string]any { return a.preselected() }

// PgRecommendedBy is the moved recommendedBy: who an ask's recommendation
// names as its writer.
func PgRecommendedBy(up *HandUp) string { return recommendedBy(up) }

// PgSetNow sets the runner's unexported clock, for the moved rung 1 tests
// that move time past a step's bound.
func (r *Runner) PgSetNow(now func() time.Time) { r.now = now }

// PgSetWorkflows sets the runner's unexported cfg's workflow client, for
// the moved rung 1 tests that script how a signal fails.
func (r *Runner) PgSetWorkflows(w Workflows) { r.cfg.Workflows = w }

// PgSessionParts runs the runner's unexported sessionParts, for the moved
// rung 1 tests that read the words of a session draw.
func (r *Runner) PgSessionParts(f CaseFacts, in ActInput) (before, after []string) {
	return r.sessionParts(f, in)
}

// PgRungUp runs the runner's unexported rungUp, for the moved rung 1 test
// that hands a case up from a rung that could not run.
func (r *Runner) PgRungUp(ctx context.Context, t Task, c Case, f CaseFacts, stepID int64, out RungOutcome, decision, grounds, found string, did []CaseAction) {
	r.rungUp(ctx, t, c, f, stepID, out, decision, grounds, found, did)
}

// PgSignalFaced runs signalFaced on a runner over store and wf alone, for
// the moved rung 1 test that pins its retries.
func PgSignalFaced(ctx context.Context, store Store, wf Workflows, taskID string, caseID, comment int64) {
	(&Runner{store: store, cfg: RunnerConfig{Workflows: wf}}).signalFaced(ctx, taskID, caseID, comment)
}

// PgResendCaseSignals aliases the heal pass's unexported resendCaseSignals,
// for the moved rung 1 tests that send a failed face's signal again.
func PgResendCaseSignals(ctx context.Context, store Store, wf Workflows, now time.Time) {
	resendCaseSignals(ctx, store, wf, now)
}

// PgSessionRoom, PgMinutesWord and PgToolNames alias the unexported words
// and tool names the moved rung 1 tests pin.
func PgSessionRoom(f CaseFacts) string         { return sessionRoom(f) }
func PgMinutesWord(d time.Duration) string     { return minutesWord(d) }
func PgToolNames(tools []CheckinTool) []string { return toolNames(tools) }

// PgNamedRef is the checkout's unexported namedRef, which the moved rung 1
// test's CheckoutRefs stub returns.
type PgNamedRef = namedRef

// PgGit runs the unexported git helper, for the moved git fixture.
func PgGit(ctx context.Context, dir, login string, args ...string) (string, error) {
	return git(ctx, dir, login, args...)
}

// PgSessionNote runs the runner's unexported sessionNote, for the moved
// rung 1 tests that read the note a session starts with.
func (r *Runner) PgSessionNote(f CaseFacts, in ActInput) string { return r.sessionNote(f, in) }

// PgReviewOfTheHead runs the runner's unexported reviewOfTheHead, for the
// moved rung 1 test that pins which earlier review a head reuses.
func (r *Runner) PgReviewOfTheHead(ctx context.Context, taskID, commit string, earlier []Review) (Review, bool) {
	return r.reviewOfTheHead(ctx, taskID, commit, earlier)
}

// PgCaseTools builds the case tools over r, t, f and repo, for the moved
// rung 1 test that reads files through them.
func PgCaseTools(r *Runner, t Task, f CaseFacts, repo Repo) *CaseTools {
	return &CaseTools{r: r, t: t, f: f, repo: repo}
}

// PgSetFacts sets the tools' unexported facts, for the moved rung 1 test
// that reads on after the case's head is gone.
func (c *CaseTools) PgSetFacts(f CaseFacts) { c.f = f }

// PgClose runs the tools' unexported close, for the moved rung 1 test that
// ends the session its read tools opened.
func (c *CaseTools) PgClose() { c.close() }
