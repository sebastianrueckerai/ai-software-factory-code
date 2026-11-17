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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// A plan card's session that ends without a push may have found its work
// on main already: the proofs of the criteria the card cites run at
// main's head, and when every one passes the card closes done. The run is
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
// main's head in one runner pod: each cited invariant's suite and each
// cited criterion's stored or opened proof. A flagged stored proof makes
// no run, nor does a criterion without a proof. A task no plan filed, the
// plan's integration card, which closes through the criteria gate, and a
// card that cites nothing answer ProofsAtMain{}. A pod that ended early
// leaves the criteria it did not reach no report. A sandbox that cannot
// start is its line and ProofsAtMain{}, so the card goes on.
func (r *Runner) ProofsAtMain(ctx context.Context, taskID string) (ProofsAtMain, error) {
	defer beatUntilDone(ctx, "proofs at main")()
	t, repo, err := r.task(ctx, taskID)
	if err != nil || t.Source != SourcePlan || t.Parent == "" {
		return ProofsAtMain{}, err
	}
	p, _, err := planOfStore(ctx, r.store, t.Parent, t.ID)
	if err != nil || roleOf(p, t.PlanKey) == RoleIntegration {
		return ProofsAtMain{}, err
	}
	ids, err := r.cardCriteria(ctx, t, p)
	if err != nil || len(ids) == 0 {
		return ProofsAtMain{}, err
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	if err != nil {
		return ProofsAtMain{}, err
	}
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return ProofsAtMain{}, err
	}
	runs := slices.DeleteFunc(gateRuns(p, rows), criterionRun.holdout)
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
		_, ran, err := r.runAt(ctx, t, repo, branchRef(prBase), head, entries)
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
	at := head[:min(7, len(head))]
	switch len(unmet) {
	case 0:
		return ProofsAtMain{Head: head, Met: true, Reason: "proofs met at " + at + ": " + strings.Join(ids, ", ")}, nil
	case 1:
		return ProofsAtMain{Head: head, Findings: []string{"the proof of " + unmet[0] + " is not met at main " + at}}, nil
	}
	return ProofsAtMain{Head: head, Findings: []string{"the proofs of " + textutil.AndList(unmet) + " are not met at main " + at}}, nil
}

// runAt runs entries at head of ref in a sandbox of t's, torn down when
// they end.
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

// untestedRuns are the runs of plan id's promises that a test checks and
// no verdict judged yet: a test, an invariant or a judgement with no
// verdict row on the plan's last card, home, whose proof is stored,
// opened or carried.
func (r *Runner) untestedRuns(ctx context.Context, id string) ([]criterionRun, Plan, Task, error) {
	p, _, err := planOfStore(ctx, r.store, id, "")
	if err != nil {
		return nil, Plan{}, Task{}, err
	}
	home, err := IntegrationCard(ctx, r.store, id, p)
	if err != nil {
		return nil, Plan{}, Task{}, err
	}
	rows, err := r.store.CriteriaVerdicts(ctx, home.ID)
	if err != nil {
		return nil, Plan{}, Task{}, err
	}
	proofs, err := r.store.PlanProofs(ctx, id)
	if err != nil {
		return nil, Plan{}, Task{}, err
	}
	never := neverJudged(p, rows)
	return slices.DeleteFunc(gateRuns(p, proofs), func(c criterionRun) bool {
		return c.holdout() || c.held != "" || c.flagged() || c.kind == CriterionHuman ||
			!slices.ContainsFunc(never, func(n PlanCriterion) bool { return n.ID == c.id })
	}), p, home, nil
}

// askTestRun is run_criteria on card t, a plan or its last card: the
// criteria gate pass runs the tests of the plan's promises no test checked
// yet (runTests). A plan with none whose test can run is refused.
func (r *Runner) askTestRun(ctx context.Context, t Task) ([]CaseAction, error) {
	plan := t.ID
	if t.Track != policy.TrackPlan {
		plan = t.Parent
	}
	runs, _, _, err := r.untestedRuns(ctx, plan)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, ErrActRefused("no promise of this plan waits for a test that can run: drop the promises no test checks, or judge the human ones")
	}
	if r.gateRunning(plan) {
		return nil, ErrActRefused("the plan's tests run now: their results land on its promises within minutes")
	}
	r.queueTests(plan)
	return []CaseAction{{Action: ActRunCriteria, Result: fmt.Sprintf("The tests of %s run within minutes, and each result lands on its promise.",
		plural(len(runs), "promise"))}}, nil
}

// runTests runs the tests of plan id's promises no verdict judged yet
// (untestedRuns) in one runner pod of its last card: at the head of the
// card's pull request while the card is open, else at main's head. The
// rows are the criteria gate's (criteriaReport): a test's or an
// invariant's gate row, met when its test passed, else not met with the
// run's line; a judgement's verdict on the artifact its test wrote. A
// sandbox that cannot start writes no row, so a press runs it again.
func (r *Runner) runTests(ctx context.Context, id string) {
	runs, p, home, err := r.untestedRuns(ctx, id)
	if err != nil || len(runs) == 0 {
		if err != nil {
			log.Printf("plan %s: the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		}
		return
	}
	_, repo, err := r.task(ctx, home.ID)
	if err != nil {
		log.Printf("plan %s: the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	ref := branchRef(prBase)
	var head string
	if home.PRNumber != 0 && !Final(home.Status) && home.Status != "merged" {
		var pr PullRequestRef
		pr, err = r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, home.PRNumber)
		ref, head = pullRef(home.PRNumber), pr.HeadSHA
	} else {
		head, err = r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	}
	if err != nil {
		log.Printf("plan %s: the head of the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	r.gateMu.Lock()
	r.gateRun = id
	r.gateMu.Unlock()
	defer func() {
		r.gateMu.Lock()
		r.gateRun = ""
		r.gateMu.Unlock()
	}()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), criteriaGateBound)
	defer cancel()
	entries := make([]harness.Proof, 0, len(runs))
	for _, c := range runs {
		entries = append(entries, c.proof())
	}
	ran, outcomes, err := r.runAt(rctx, home, repo, ref, head, entries)
	if err != nil {
		log.Printf("plan %s: the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	results := make(map[string]gateResult, len(runs))
	for _, c := range runs {
		run, ok := ran[c.id]
		results[c.id] = gateResult{outcome: outcomes[c.id], output: run.Output, ran: ok && run.Started, artifacts: run.Artifacts, dropped: run.Dropped}
	}
	rep := criteriaReport(p, head, runs, results)
	if err := r.store.PutCriteriaVerdicts(rctx, home.ID, CriteriaSourceGate, FactoryDecider, gateRows(rep.proofs)); err != nil {
		log.Printf("plan %s: the rows of the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	if err := r.store.PutCriteriaVerdicts(rctx, home.ID, CriteriaSourceJudgement, FactoryDecider, rep.noVerdict); err != nil {
		log.Printf("plan %s: the rows of the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	if _, _, _, err := r.judgeArtifacts(rctx, home, home.ID, p, rep.artifacts); err != nil {
		log.Printf("plan %s: the judgement of the tests no test ran: %s", id, r.Scrub.String(err.Error()))
		return
	}
	r.record(rctx, id, "criteria_run", map[string]any{"task": home.ID, "ref": ref, "head": head, "proofs": rep.proofs})
}
