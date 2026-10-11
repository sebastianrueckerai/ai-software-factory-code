package factoryd

// Statuses are a task's statuses, the final ones last.
var Statuses = append([]string{"backlog", "queued", "waiting-deps", "running", "closing", "in-review", "waiting-human", "waiting-poster"},
	FinalStatuses...)

// StatusMeaning is status in one sentence, as the project agent reads it.
func StatusMeaning(status string) string {
	switch status {
	case "backlog":
		return "waits in the backlog until a member moves it into a lane"
	case "queued":
		return "waits for its session to start"
	case "waiting-deps":
		return "waits for the cards it depends on"
	case "running":
		return "runs an agent session"
	case "closing":
		return "is being closed"
	case "in-review":
		return "passes the gates after its session"
	case "waiting-human":
		return "waits on a human: an answer, an approval or a decision"
	case "waiting-poster":
		return "waits on its poster's information"
	case "done":
		return "is done"
	case "failed":
		return "has failed"
	case "closed":
		return "was closed"
	default:
		return "is in an unknown state"
	}
}

// GateMeaning is gate in one line, as the project agent reads it.
func GateMeaning(gate string) string {
	switch gate {
	case GateIntake:
		return "the issue becomes a card"
	case GateSession:
		return "an agent works in a sandbox"
	case GateChecks:
		return "the pull request's CI"
	case GatePolicy:
		return "risk class from the changed paths"
	case GateMigration:
		return "schema migrations flagged for review"
	case GateReproduction:
		return "the issue's reproduction command against the fix"
	case GateReview:
		return "a model reviews the diff"
	case GateAudit:
		return "a model audits the diff for security"
	case GateApproval:
		return "a human approves the merge"
	case GateMerge:
		return "factory-merge merges"
	case GateHuman:
		return "a human decides"
	case GateCommissioning:
		return "a server is commissioned"
	case GateScreening:
		return "a model screens an outsider's issue"
	case GateDependencies:
		return "waits for the cards it depends on"
	case GateMergeQueue:
		return "waits for its turn to merge"
	default:
		return "is in an unknown gate"
	}
}
