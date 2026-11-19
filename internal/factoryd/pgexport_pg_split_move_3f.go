package factoryd

// The export surface of the third move's last round (pg-split-move-3f): the
// names the rung 2, monitor and cases_events *_pg_test.go files reach in
// factoryd as they move into internal/factoryd/pg.

// PgScrub runs the runner's unexported scrub, for the moved rung 2 test
// that writes a climbed row's masked problem.
func (r *Runner) PgScrub(s string) string { return r.scrub(s) }

// PgRungStatement runs the runner's unexported rungStatement, for the moved
// rung 2 tests that read the statement a rung's model is given.
func (r *Runner) PgRungStatement(c Case, f CaseFacts, digest string, steps []CaseStep, rung int) string {
	return r.rungStatement(c, f, digest, steps, rung)
}

// PgOrderedActions aliases the unexported orderedActions, for the moved
// rung 2 test that empties the action table.
func PgOrderedActions(f CaseFacts) []Action { return orderedActions(f) }

// PgTaskRun is the workflow's unexported taskRun, whose card words the
// moved cases_events test reads.
type PgTaskRun = taskRun

// PgSessionCapCard and PgPlannerTimeCard run the taskRun's unexported card
// writers, for the moved cases_events test that reads the kind their reasons
// open.
func (t *taskRun) PgSessionCapCard(head, last string, findings []string) string {
	return t.sessionCapCard(head, last, findings)
}

func (t *taskRun) PgPlannerTimeCard(u TaskUsage) string { return t.plannerTimeCard(u) }

// PgReplanText and PgReplanClosers alias the replan's unexported text and
// closing steps, for the moved rung 2 test that cuts a replan.
func PgReplanText(trip *Trip, d Decision) string { return replanText(trip, d) }

var PgReplanClosers = replanClosers

// The unexported words the moved tests pin, aliased as constants.
const (
	PgMonthCapReached   = monthCapReached
	PgProjectCapReached = projectCapReached
	PgNoModelAnswer     = noModelAnswer
	PgPlainInfo         = plainInfo
	PgPodNeverStarted   = podNeverStarted
	PgScreeningAsks     = screeningAsks
	PgStaleQuiet        = staleQuiet
	PgStartRefused      = startRefused
	PgStepReplan        = stepReplan
	PgProviderRefused   = providerRefused
)

// PgRefusal runs the action's unexported refusal, for the moved rung 2
// tests that read why an action is refused.
func (a Action) PgRefusal(f CaseFacts, rung int, res Resolution) string {
	return a.refusal(f, rung, res)
}

// PgMonitorKinds aliases the monitor's unexported event kinds, for the
// moved monitor parity test that reads them from the store.
var PgMonitorKinds = monitorKinds

// PgSetLaunchRung1 sets the runner's unexported launchRung1, for the moved
// monitor test that keeps rung 1's conversation from running.
func (r *Runner) PgSetLaunchRung1(f func(run func())) { r.launchRung1 = f }

// PgDepFailedReason and PgPlanCardFailedReason run the unexported reasons a
// failed dependency's stop and a plan's wait on its failed card ask with,
// for the moved trip and rung 2 tests that open those cases.
func PgDepFailedReason(d Dependency) string { return depFailedReason(d) }

func PgPlanCardFailedReason(k Task) string { return planCardFailedReason(k) }
