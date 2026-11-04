package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
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
	p, err := r.filedPlan(ctx, t.Parent, t.ID)
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
		ran, err := r.runAtMain(ctx, t, repo, head, entries)
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

// runAtMain runs entries at main's head in a sandbox of t's, torn down
// when they end.
func (r *Runner) runAtMain(ctx context.Context, t Task, repo Repo, head string, entries []harness.Proof) (map[string]proofOutcome, error) {
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		return nil, err
	}
	defer done()
	_, outcomes, err := r.runProofs(ctx, t, repo, branchRef(prBase), head, "", entries)
	var ended *proofsEnded
	if errors.As(err, &ended) {
		err = nil
	}
	return outcomes, err
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
