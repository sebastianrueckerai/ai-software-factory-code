package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// A plan card's session that ends without a push may have found its work
// on main already: while the card's branch holds no work main lacks, the
// proofs of the criteria the card cites run at main's head, and when
// every one passes the card closes done. An invariant the card cites
// counts for nothing: it holds before the work as after it. The run is
// blind: it opens no proof, writes no row and records no event, and its
// line and its finding name the criteria alone.

// ProofsAtMain is the proofs of a plan card's cited criteria run at main's head after a session ended without a push.
type ProofsAtMain struct {
	Head     string   `json:",omitempty"`
	Met      bool     `json:",omitempty"`
	Reason   string   `json:",omitempty"` // the done close's reason
	Findings []string `json:",omitempty"` // the next session's: the criteria not met, by id alone
}

// proofNone is the outcome of a cited criterion with no proof to run.
const proofNone = "no proof"

// mainOutcome is one criterion on the proofs_at_main line.
type mainOutcome struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"` // a proof* word, proofNone or proofFlagged
}

// ProofsAtMain runs the proofs of the criteria plan card taskID cites at
// main's head in one runner pod: each cited criterion's stored or opened
// proof, no invariant. A flagged stored proof makes no run, nor does a
// criterion without a proof. A task no plan filed, the plan's integration
// card, which closes through the criteria gate, a card that cites no
// criterion but invariants, and a card whose branch holds work main
// lacks answer ProofsAtMain{}, as does a branch that cannot be read, with
// its error. A pod that ended early leaves the criteria it did not reach
// no report. A sandbox that cannot start is its line and ProofsAtMain{},
// so the card goes on.
func (r *Runner) ProofsAtMain(ctx context.Context, taskID string) (ProofsAtMain, error) {
	defer beatUntilDone(ctx, "proofs at main")()
	t, repo, err := r.task(ctx, taskID)
	if err != nil || !t.PlanCard() {
		return ProofsAtMain{}, err
	}
	p, _, err := planOfStore(ctx, r.store, t.Parent, t.ID)
	if err != nil || roleOf(p, t.PlanKey) == RoleIntegration {
		return ProofsAtMain{}, err
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	if err != nil {
		return ProofsAtMain{}, err
	}
	runs := slices.DeleteFunc(gateRuns(p, rows), criterionRun.holdout)
	ids, err := r.cardCriteria(ctx, t, p)
	ids = slices.DeleteFunc(ids, func(id string) bool {
		return slices.ContainsFunc(runs, func(c criterionRun) bool { return c.id == id && c.kind == CriterionInvariant })
	})
	if err != nil || len(ids) == 0 {
		return ProofsAtMain{}, err
	}
	if ahead, err := r.workAhead(ctx, t); ahead != "" || err != nil {
		return ProofsAtMain{}, err
	}
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return ProofsAtMain{}, err
	}
	outcomes := map[string]string{}
	var entries []harness.Proof
	for _, id := range ids {
		switch i := slices.IndexFunc(runs, func(c criterionRun) bool { return c.id == id }); {
		case i < 0 || runs[i].held != "":
			outcomes[id] = proofNone
		case runs[i].flagged():
			outcomes[id] = proofFlagged
		default:
			entries = append(entries, runs[i].proof())
		}
	}
	if len(entries) > 0 {
		_, ran, err := r.runAt(ctx, t, repo, "", head, entries)
		var f *startFailure
		if errors.As(err, &f) {
			r.record(ctx, t.ID, "proofs_at_main", map[string]any{"head": head, "start_failed": f.text})
			return ProofsAtMain{}, nil
		}
		if err != nil {
			return ProofsAtMain{}, err
		}
		for id, o := range ran {
			outcomes[id] = o.Outcome
		}
	}
	line := make([]mainOutcome, len(ids))
	var unmet []string
	for i, id := range ids {
		if line[i] = (mainOutcome{id, outcomes[id]}); outcomes[id] != proofPassed {
			unmet = append(unmet, id)
		}
	}
	r.record(ctx, t.ID, "proofs_at_main", map[string]any{"head": head, "criteria": line})
	at := ShortSHA(head)
	switch len(unmet) {
	case 0:
		return ProofsAtMain{Head: head, Met: true, Reason: "proofs met at " + at + ": " + strings.Join(ids, ", ")}, nil
	case 1:
		return ProofsAtMain{Head: head, Findings: []string{"the proof of " + unmet[0] + " is not met at main " + at}}, nil
	}
	return ProofsAtMain{Head: head, Findings: []string{"the proofs of " + textutil.AndList(unmet) + " are not met at main " + at}}, nil
}

// workAhead is the head of card t's branch on the fork while main lacks
// it, or "" when the fork holds no branch for the card past its base or
// main holds the branch's head.
func (r *Runner) workAhead(ctx context.Context, t Task) (head string, err error) {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return "", err
	}
	if head, err = r.branchHead(ctx, t, repo); err != nil || head == "" {
		return "", err
	}
	base, err := r.forgejo.MergeBase(ctx, repo.Owner, repo.Name, r.forgejo.Fork(repo).Owner, head)
	if err != nil || base == head {
		return "", err
	}
	return head, nil
}

// runAt runs entries at head of ref in a sandbox of t's, torn down when
// they end. An empty ref fetches head itself.
func (r *Runner) runAt(ctx context.Context, t Task, repo Repo, ref, head string, entries []harness.Proof) (map[string]harness.ProofRun, map[string]proofOutcome, error) {
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		return nil, nil, err
	}
	defer done()
	runs, outcomes, err := r.runProofs(ctx, t, repo, ref, head, "", entries)
	var ended *proofsEnded
	if errors.As(err, &ended) {
		err = nil
	}
	return runs, outcomes, err
}

// cardCriteria are the criterion ids card t's acceptance items cite, in the plan's order. A delta card's criteria come from its delta_card line.
func (r *Runner) cardCriteria(ctx context.Context, t Task, p Plan) ([]string, error) {
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == t.PlanKey }); i >= 0 {
		var ids []string
		for _, c := range p.Criteria {
			if slices.ContainsFunc(p.Cards[i].Acceptance, func(it PlanItem) bool { return it.Criterion == c.ID }) {
				ids = append(ids, c.ID)
			}
		}
		return ids, nil
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+t.Parent+"/timeline.jsonl")
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(tl, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var m struct {
			Step     string   `json:"step"`
			Key      string   `json:"key"`
			Criteria []string `json:"criteria"`
		}
		if json.Unmarshal(lines[i], &m) == nil && m.Step == "delta_card" && m.Key == t.PlanKey {
			return m.Criteria, nil
		}
	}
	return nil, nil
}

// runnableRuns are the runs of plan p's promises no verdict of rows judged
// that a run of the factory checks: a test, an invariant or a judgement
// whose proof is stored, opened or carried, neither held nor flagged.
func runnableRuns(p Plan, proofs []PlanProof, rows []CriterionVerdict) []criterionRun {
	never := neverJudged(p, rows)
	return slices.DeleteFunc(gateRuns(p, proofs), func(c criterionRun) bool {
		return c.holdout() || c.held != "" || c.flagged() || c.kind == CriterionHuman ||
			!slices.ContainsFunc(never, func(n PlanCriterion) bool { return n.ID == c.id })
	})
}

// runIDs are the promises runs check.
func runIDs(runs []criterionRun) []string {
	ids := make([]string, 0, len(runs))
	for _, c := range runs {
		ids = append(ids, c.id)
	}
	return ids
}

// untestedRuns are plan id's runnableRuns on the rows of its last card,
// home: every row while the card's pull request is open, else the rows
// that settle a promise as the plan's gate g stands (planGate.settling).
func (r *Runner) untestedRuns(ctx context.Context, id string) ([]criterionRun, Plan, Task, planGate, error) {
	p, _, err := planOfStore(ctx, r.store, id, "")
	if err != nil {
		return nil, Plan{}, Task{}, planGate{}, err
	}
	home, err := IntegrationCard(ctx, r.store, id, p)
	if err != nil {
		return nil, Plan{}, Task{}, planGate{}, err
	}
	rows, err := r.store.CriteriaVerdicts(ctx, home.ID)
	if err != nil {
		return nil, Plan{}, Task{}, planGate{}, err
	}
	g, err := planGateOf(ctx, r.store, id, home)
	if err != nil {
		return nil, Plan{}, Task{}, planGate{}, err
	}
	if !runsAtPull(home) {
		rows = g.settling(rows)
	}
	proofs, err := r.store.PlanProofs(ctx, id)
	if err != nil {
		return nil, Plan{}, Task{}, planGate{}, err
	}
	return runnableRuns(p, proofs, rows), p, home, g, nil
}

// runsAtPull says a run of a plan's tests on its last card home runs at
// the head of the card's open pull request, not at main's.
func runsAtPull(home Task) bool {
	return home.PRNumber != 0 && !Final(home.Status) && home.Status != "merged"
}

// askTestRun is run_criteria on card t, a plan or its last card, by whom:
// the press is on record (asked) before the criteria gate pass runs the
// tests of the plan's promises no test checked yet (runTests), so a
// restart loses no press (healTests). A plan with none whose test can
// run, a closed plan, a done plan whose planner is to run again and a run
// on its way are refused.
func (r *Runner) askTestRun(ctx context.Context, t Task, by string) ([]CaseAction, error) {
	id := t.ID
	if t.Track != policy.TrackPlan {
		id = t.Parent
	}
	plan, _, err := r.task(ctx, id)
	if err != nil {
		return nil, err
	}
	runs, _, home, _, err := r.untestedRuns(ctx, id)
	if err != nil {
		return nil, err
	}
	asked, err := r.askedRuns(ctx, id, home.ID)
	if err != nil {
		return nil, err
	}
	switch {
	case len(runs) == 0:
		return nil, ErrActRefused("no promise of this plan has a test the factory can run: build the missing part, or drop the promise")
	case r.planClosed(ctx, id):
		return nil, ErrActRefused("the plan is closed: its promises take no run")
	case Final(home.Status) && !r.planAtRest(ctx, plan):
		return nil, ErrActRefused("the plan is being planned again: its new cards bring their own tests")
	case r.gateRunning(id) || len(asked) > 0 && (asked[0].Detail["outcome"] == gateAsked || asked[0].Detail["outcome"] == gateStarted):
		return nil, ErrActRefused("the plan's tests run now: their results land on its promises within minutes")
	}
	try := gateTry{plan: plan, card: home, attempt: fmt.Sprint(gateAttempts(asked) + 1), by: by, pr: home.PRNumber}
	if _, err := r.store.Record(ctx, Event{Repository: plan.Repository, TaskID: id, IssueNumber: plan.IssueNumber, Kind: EventCriteriaGateRun,
		Actor: by, Via: ViaFactoryd, Detail: try.detail(gateAsked, "", "")}); err != nil {
		return nil, fmt.Errorf("record the run of the tests: %w", err)
	}
	r.queueTests(id)
	return []CaseAction{{Action: ActRunCriteria, Result: fmt.Sprintf("The tests of %s run within minutes, and each result lands on its promise.",
		plural(len(runs), "promise"))}}, nil
}

// healTests starts again a run of plan id's tests a human asked for that
// no process runs: an asked one is queued again, and one that started and
// recorded no outcome past the pass's bound is cut, so its page offers the
// run again.
func (r *Runner) healTests(ctx context.Context, id string, p Plan) {
	home, err := IntegrationCard(ctx, r.store, id, p)
	if err != nil {
		return
	}
	asked, err := r.askedRuns(ctx, id, home.ID)
	if err != nil || len(asked) == 0 || r.gateRunning(id) {
		return
	}
	switch newest := asked[0]; newest.Detail["outcome"] {
	case gateAsked:
		r.queueTests(id)
	case gateStarted:
		if time.Since(newest.At) > CriteriaGatePassBound {
			plan, _, err := r.task(ctx, id)
			if err != nil {
				return
			}
			by, _ := newest.Detail["by"].(string)
			attempt, _ := newest.Detail["attempt"].(string)
			commit, _ := newest.Detail["commit"].(string)
			r.recordTry(ctx, gateTry{plan: plan, card: home, attempt: attempt, by: by, pr: home.PRNumber}, gateCut, commit, "the factory restarted while they ran")
		}
	}
}

// runTests runs the tests a human asked for (askTestRun) as the criteria
// gate runs its own (gateTry): the tests of plan id's promises no verdict
// judged yet (untestedRuns) in one runner pod of its last card, at the
// head of the card's pull request while the card is open, else at main's
// head. The card is funded up to the gate reserve first (no_room when its
// ceiling has none). The rows are the gate's (criteriaReport): a test's
// gate row, met when its test passed, else not met with the run's line;
// an invariant's, on the head's required checks, which run no pod and
// leave no row while they still run, a cut the page shows; a judgement's
// verdict on the artifact its test wrote.
// The outcome is on record, a cut with why it stopped, which the
// promise's page shows. Only the newest press, still asked, runs.
func (r *Runner) runTests(ctx context.Context, id string) {
	rctx := context.WithoutCancel(ctx)
	runs, p, home, g, err := r.untestedRuns(ctx, id)
	if err != nil {
		log.Printf("plan %s: the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	asked, err := r.askedRuns(ctx, id, home.ID)
	if err != nil || len(asked) == 0 || asked[0].Detail["outcome"] != gateAsked {
		return
	}
	plan, repo, err := r.task(ctx, id)
	if err != nil {
		log.Printf("plan %s: the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	by, _ := asked[0].Detail["by"].(string)
	attempt, _ := asked[0].Detail["attempt"].(string)
	try := &gateTry{plan: plan, card: home, attempt: attempt, by: by, pr: home.PRNumber}
	switch {
	case len(runs) == 0:
		r.recordTry(rctx, *try, gateCut, "", "no test was left to run")
		return
	case r.planClosed(ctx, id):
		r.recordTry(rctx, *try, gateCut, "", "the plan closed")
		return
	case Final(home.Status) && !r.planAtRest(ctx, plan):
		r.recordTry(rctx, *try, gateCut, "", "the plan is being planned again")
		return
	}
	var ref, head string
	if runsAtPull(home) {
		var pr PullRequestRef
		pr, err = r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, home.PRNumber)
		ref, head = pullRef(home.PRNumber), pr.HeadSHA
	} else {
		head, err = r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	}
	if err != nil {
		log.Printf("plan %s: the head of the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		r.recordTry(rctx, *try, gateCut, "", "the head of its code could not be read")
		return
	}
	if !r.fundTry(ctx, *try, head) {
		return
	}
	if ref == "" {
		// a run at main's head settles on main as the factory's own does
		// (runCriteriaGate): the rows that judge no head on main go first
		if !g.onMain {
			if err := r.withdrawOffMain(rctx, id, p, home.ID); err != nil {
				log.Printf("plan %s: withdraw the rows that judge no head on main: %s", id, r.Scrub.String(err.Error()))
				r.recordTry(rctx, *try, gateCut, head, "its older results could not be withdrawn")
				return
			}
		}
		try.pr, try.mark = 0, g.mark
	}
	r.runTry(rctx, try, head, func(gctx context.Context) error {
		results := make(map[string]gateResult, len(runs))
		var entries []harness.Proof
		var checks *gateResult
		for _, c := range runs {
			if !c.checks {
				entries = append(entries, c.proof())
				continue
			}
			if checks == nil {
				res, err := r.HeadChecks(gctx, home.ID, head)
				if err != nil {
					return &tryFault{"the required checks of its code could not be read", err}
				}
				checks = &gateResult{checks: res, checked: head}
			}
			results[c.id] = *checks
		}
		if len(entries) > 0 {
			ran, outcomes, err := r.runAt(gctx, home, repo, ref, head, entries)
			if err != nil {
				return &tryFault{"its sandbox did not run", err}
			}
			for _, e := range entries {
				run, ok := ran[e.ID]
				results[e.ID] = gateResult{outcome: outcomes[e.ID], output: run.Output, ran: ok && run.Started, artifacts: run.Artifacts, dropped: run.Dropped}
			}
		}
		rep := criteriaReport(p, head, runs, results)
		// a run on the card's pull request writes as the card, which a card
		// that ended since refuses (ErrCardEnded)
		writer := ""
		if ref != "" {
			writer = home.ID
		}
		if err := r.store.PutGateVerdicts(gctx, writer, home.ID, CriteriaSourceGate, gateRows(rep.proofs), nil); err != nil {
			return &tryFault{"its results could not be stored", err}
		}
		if err := r.store.PutGateVerdicts(gctx, writer, home.ID, CriteriaSourceJudgement, rep.noVerdict, nil); err != nil {
			return &tryFault{"its results could not be stored", err}
		}
		if _, _, _, err := r.judgeArtifacts(gctx, home, writer, home.ID, p, rep.artifacts); err != nil {
			return &tryFault{"the model's judgement failed", err}
		}
		r.record(gctx, id, "criteria_run", map[string]any{"task": home.ID, "ref": ref, "head": head, "by": by, "proofs": rep.proofs})
		if rep.pending {
			return &tryFault{"the required checks of its code were still running", errors.New(head)}
		}
		return nil
	})
}
