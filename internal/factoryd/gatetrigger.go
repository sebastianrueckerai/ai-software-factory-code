package factoryd

import "context"

// The criteria gate's trigger: the fact every replan stop and every
// check-in decision against a re-cut card names. The gate runs only at a
// living card's pull request merge press, so a card whose open pull
// request is that trigger must not be read as PR-less, and a failed forge
// read is the error, never a false: an unreadable pull request is not a
// closed one.

// CheckInPRFacts is the card's pull request as the check-in's ask names
// it: the number, its checks state, and whether it is the plan's
// criteria-gate trigger.
type CheckInPRFacts struct {
	Number      int64
	Checks      ChecksState
	GateTrigger bool
}

// gateTrigger says whether card t is plan p's criteria-gate trigger: t
// holds an open pull request (read through the forge client as the merge
// press reads it), t is the plan's integration card or a delta card (the
// same test CriteriaCard uses to decide the gate runs), and the plan
// still holds criteria without a settled verdict. The error arm is the
// fail direction: when the card holds a pull request and the forge read
// fails, gateTrigger returns the error and never a false — a caller must
// never read an unreadable pull request as no open pull request. With no
// pull request to read it returns false, nil.
func (r *Runner) gateTrigger(ctx context.Context, t Task, p Plan) (bool, error) {
	if t.Source != SourcePlan || t.Parent == "" || !CriteriaCard(p, t.PlanKey) {
		return false, nil
	}
	home, err := IntegrationCard(ctx, r.store, t.Parent, p)
	if err != nil {
		return false, err
	}
	rows, err := r.store.CriteriaVerdicts(ctx, home.ID)
	if err != nil {
		return false, err
	}
	if len(openCriteria(p, rows)) == 0 {
		return false, nil
	}
	if t.PRNumber == 0 {
		return false, nil
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return false, terminalError("%v", err)
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
	if err != nil {
		return false, err
	}
	return pr.State == "open" && !pr.Merged, nil
}

// checkinPRFacts reads the pull request facts the session check-in's ask
// carries: the card's pull request number, its checks state at its head,
// and gateTrigger. A forge read failure is the error, never a missing
// pull request. Cards off a plan hold no facts; a plan's card without a
// pull request holds none either.
func (r *Runner) checkinPRFacts(ctx context.Context, t Task) (*CheckInPRFacts, error) {
	if t.Source != SourcePlan || t.Parent == "" {
		return nil, nil
	}
	p, err := r.filedPlan(ctx, t.Parent, t.ID)
	if err != nil {
		return nil, err
	}
	trigger, err := r.gateTrigger(ctx, t, p)
	if err != nil {
		return nil, err
	}
	if t.PRNumber == 0 {
		return nil, nil
	}
	f := &CheckInPRFacts{Number: t.PRNumber, GateTrigger: trigger, Checks: ChecksPending}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return nil, terminalError("%v", err)
	}
	cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, t.SHA)
	if err != nil {
		return nil, err
	}
	if cs.State != "" {
		f.Checks = ChecksState(cs.State)
	}
	return f, nil
}
