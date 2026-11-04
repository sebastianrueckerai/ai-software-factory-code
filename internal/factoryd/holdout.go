package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The hold-outs (the spec-intake concept, piece 6): criteria the confirmed
// spec's proof does not name, blind to the planner and the builder by
// routing and storage. A confirmation round's zone proposes them, the
// review model drafting each criterion and its test against the
// repository beside the project's past revert reasons, and the dry run
// on the main head before the form closes: red before, green after.
// Stored hold-outs run at the criteria gate of the plan's integration
// card, their verdicts on the gate's timeline line alone.

// The hold-out states: a proposal waits for the human's okay or drop;
// unrunnable is a draft that never ran, whose only click is drop; stored
// runs at the criteria gate; opened is one whose criterion is public.
const (
	HoldoutProposed   = "proposed"
	HoldoutDropped    = "dropped"
	HoldoutStored     = "stored"
	HoldoutUnrunnable = "unrunnable"
	HoldoutOpened     = "opened"
)

// Hold-out origins.
const (
	HoldoutDrafted    = "drafted"
	HoldoutMechanical = "mechanical"
)

// Holdout is one hold-out criterion of a plan task, as stored: round is
// the plan revision whose confirmation proposed it, idx its number on
// the task, which never changes.
type Holdout struct {
	Idx   int    `json:"idx"`
	Round int    `json:"round"`
	ID    string `json:"id"`
	// Origin says what drafted it: the spec, or a past failure it cites.
	Origin string `json:"origin"`
	// Criterion is the hold-out's claim in the spec's words; TestName the
	// test its draft defines, TestCode the draft itself.
	Criterion string `json:"criterion"`
	State     string `json:"state"`
	// DryHead is the main head the draft ran on, DryVerdict red, green or
	// unrunnable, DryOutput one line of why.
	DryHead    string `json:"dry_head"`
	DryVerdict string `json:"dry_verdict"`
	DryOutput  string `json:"dry_output"`
	TestName   string `json:"test_name"`
	TestCode   string `json:"test_code"`
}

// ErrHoldoutsOpen refuses a confirmation answer while its round's
// proposals wait for the human's okay or drop.
var ErrHoldoutsOpen = errors.New("the round's hold-outs wait for your okay or drop")

// ErrNoHoldout names a hold-out the task does not hold.
var ErrNoHoldout = errors.New("no such hold-out")

// ErrHoldoutClick refuses a click the hold-out's state takes not: a row
// decided already, or an okay on a draft that could not run.
var ErrHoldoutClick = errors.New("the hold-out takes no such click now")

var (
	// holdoutID admits a hold-out id: the criterion charset.
	holdoutID = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	// holdoutTestName admits a drafted test's name.
	holdoutTestName = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)
)

// One round's and one draft's bounds.
const (
	maxHoldouts    = 5
	maxHoldoutCode = 4 << 10
	maxHoldoutText = 200
	holdoutPkg     = "holdout"
)

// pullRef is a pull request's head ref, branchRef a branch's: what a
// repro pod fetches.
func pullRef(pr int64) string { return fmt.Sprintf("refs/pull/%d/head", pr) }

func branchRef(branch string) string { return "refs/heads/" + branch }

// holdoutEntry is a hold-out's id in the runner's list: criterionID
// admits no hyphen, so it never meets a criterion's.
func holdoutEntry(idx int) string { return fmt.Sprintf("holdout-%d", idx) }

// holdoutProof is the dry run of one draft: its file written into a
// scratch package of its own inside the repository, its test run there.
func holdoutProof(h Holdout) harness.Proof {
	dir := fmt.Sprintf(".%s%d", holdoutPkg, h.Idx)
	return harness.Proof{ID: holdoutEntry(h.Idx), Suite: "./" + dir, Name: h.TestName,
		File: &harness.ProofFile{Path: dir + "/holdout_test.go", Content: "package " + holdoutPkg + "\n" + h.TestCode + "\n"}}
}

// holdoutDryVerdict judges one draft's outcome: red is a hold-out's shape
// (the test runs and fails), green already passes (a guard to keep),
// unrunnable is anything else (no such test, a build that fails, a skip,
// no report).
func holdoutDryVerdict(o proofOutcome) (dry, evidence string) {
	ev := shortenEvidence(o.Message)
	switch o.Outcome {
	case proofFailed:
		return "red", ev
	case proofPassed:
		return "green", ev
	}
	return HoldoutUnrunnable, ev
}

// holdoutProposal is the review model's proposal of one hold-out, as its
// JSON call answers; From cites the past failure a mechanical proposal
// names, "" for one drafted from the spec.
type holdoutProposal struct {
	ID        string `json:"id"`
	Criterion string `json:"criterion"`
	TestName  string `json:"test_name"`
	TestCode  string `json:"test_code"`
	From      string `json:"from"`
}

// askHoldouts runs a confirmation round's pipeline: the review model
// proposes criteria with drafted tests in one call, the drafts dry-run on
// the main head in one sandbox, drafts that cannot run get one corrective
// pass and dry-run again, and the round's rows land with their verdicts.
// The zone shows what landed; the human's only clicks are okay and drop.
// A failure leaves the round empty, the confirmation question stands on
// its own, and the timeline names why.
func (r *Runner) askHoldouts(ctx context.Context, t Task, revision int, p Plan) error {
	defer beatUntilDone(ctx, "holdouts")()
	rows, err := r.store.PlanHoldouts(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, h := range rows {
		if h.Round == revision {
			return nil // an activity retry: the round asked already
		}
	}
	if err := r.store.SweepPlanHoldouts(ctx, t.ID); err != nil {
		return err
	}
	reverts, err := r.store.PastReverts(ctx, t.Repository)
	if err != nil {
		return err
	}
	alias := r.callModel(t, policy.PurposeReview)
	drafts := []Holdout{}
	if alias != "" {
		if drafts, err = r.proposeHoldouts(ctx, t, revision, p, alias, reverts); err != nil {
			r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
			return nil
		}
	}
	if len(drafts) == 0 {
		return nil
	}
	_, repo, err := r.task(ctx, t.ID)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	branch, err := r.forgejo.RepoDefaultBranch(ctx, repo.Owner, repo.Name)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, branch)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	// the planner session's sandbox went with its session: the dry runs
	// build their own
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": "dry run: " + err.Error()})
		return nil
	}
	defer done()
	for pass := 0; ; pass++ {
		var pending []harness.Proof
		for _, h := range drafts {
			if h.DryVerdict == "" {
				pending = append(pending, holdoutProof(h))
			}
		}
		if len(pending) == 0 || pass > 1 {
			break
		}
		outcomes, err := r.runProofs(ctx, t, repo, branchRef(branch), head, "", pending)
		if err != nil {
			r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": "dry run: " + err.Error()})
			return nil
		}
		for i, h := range drafts {
			if h.DryVerdict != "" {
				continue
			}
			drafts[i].DryVerdict, drafts[i].DryOutput = holdoutDryVerdict(outcomes[holdoutEntry(h.Idx)])
			drafts[i].DryHead = head
		}
		if pass == 0 {
			again, err := r.redraftHoldouts(ctx, t, p, alias, drafts)
			if err != nil {
				r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": "redraft: " + err.Error()})
			}
			for i, h := range drafts {
				if h.DryVerdict == HoldoutUnrunnable && again[h.ID] != "" {
					drafts[i].TestCode = again[h.ID]
					drafts[i].DryVerdict, drafts[i].DryOutput = "", ""
				}
			}
		}
	}
	verdicts := make(map[string]string, len(drafts))
	for i, h := range drafts {
		drafts[i].State = HoldoutProposed
		if h.DryVerdict == HoldoutUnrunnable {
			drafts[i].State = HoldoutUnrunnable
		}
		verdicts[h.ID] = h.DryVerdict
	}
	inserted, err := r.store.PutPlanHoldouts(ctx, t.ID, revision, drafts)
	if err != nil || !inserted {
		return err
	}
	r.record(ctx, t.ID, "holdouts_proposed", map[string]any{"round": revision, "verdicts": verdicts})
	return nil
}

// gateHoldouts are the plan task's hold-outs the criteria gate runs: the
// stored ones and the opened ones, in idx order. A read that fails runs
// the gate without them and names itself in the timeline.
func (r *Runner) gateHoldouts(ctx context.Context, parent string) []Holdout {
	rows, err := r.store.PlanHoldouts(ctx, parent)
	if err != nil {
		r.record(ctx, parent, "gate_criteria", map[string]any{"holdouts_unread": err.Error()})
		return nil
	}
	var out []Holdout
	for _, h := range rows {
		if h.State == HoldoutStored || h.State == HoldoutOpened {
			out = append(out, h)
		}
	}
	return out
}

// proposeHoldouts is the one model call: the confirmed spec, the plan's
// criteria and the project's past failures in, the proposals out. Ids
// off the charset, drafts off the bounds and code without its test are
// dropped; an id a plan criterion or an earlier proposal holds gains _h.
func (r *Runner) proposeHoldouts(ctx context.Context, t Task, revision int, p Plan, alias string, reverts []string) ([]Holdout, error) {
	spec := p.Spec
	if spec == nil {
		spec = &Spec{}
	}
	input := struct {
		Spec         *Spec           `json:"spec"`
		Criteria     []PlanCriterion `json:"criteria"`
		PastFailures []string        `json:"past_failures"`
		Max          int             `json:"max"`
	}{Spec: spec, Criteria: p.Criteria, PastFailures: reverts, Max: maxHoldouts}
	content, _, err := r.supervisor.Chat(ctx, t.ID, alias, policy.PurposeReview,
		r.cfg.Policy().Document.Limits.SupervisorTimeout, holdoutSystem, input)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Holdouts []holdoutProposal `json:"holdouts"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil {
		return nil, fmt.Errorf("the answer is not the hold-outs json: %w", err)
	}
	takenRows, err := r.store.PlanHoldouts(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	next := 1
	taken := map[string]bool{}
	for _, h := range takenRows {
		taken[h.ID] = true
		if h.Idx >= next {
			next = h.Idx + 1
		}
	}
	for _, c := range p.Criteria {
		taken[c.ID] = true
	}
	out := []Holdout{}
	for _, pr := range answer.Holdouts {
		if len(out) >= maxHoldouts {
			break
		}
		id := strings.ToLower(strings.TrimSpace(pr.ID))
		if !holdoutID.MatchString(id) {
			continue
		}
		if taken[id] {
			id += "_h"
		}
		if taken[id] || !holdoutID.MatchString(id) {
			continue
		}
		taken[id] = true
		name := strings.TrimSpace(pr.TestName)
		code := strings.TrimSpace(pr.TestCode)
		criterion := strings.Join(strings.Fields(pr.Criterion), " ")
		if !holdoutTestName.MatchString(name) || criterion == "" || len(criterion) > maxHoldoutText ||
			code == "" || len(code) > maxHoldoutCode ||
			!strings.Contains(code, "func "+name+"(") {
			continue
		}
		origin := HoldoutDrafted
		if strings.TrimSpace(pr.From) != "" {
			origin = HoldoutMechanical
		}
		out = append(out, Holdout{Round: revision, ID: id, Origin: origin,
			Criterion: r.untrusted(t, criterion), TestName: name, TestCode: code})
	}
	for i := range out {
		out[i].Idx = next + i
	}
	return out, nil
}

// redraftHoldouts is the corrective pass: the drafts that could not run,
// with why, back through the model once; the answer maps id to new code.
func (r *Runner) redraftHoldouts(ctx context.Context, t Task, p Plan, alias string, drafts []Holdout) (map[string]string, error) {
	if alias == "" {
		return nil, nil
	}
	spec := p.Spec
	if spec == nil {
		spec = &Spec{}
	}
	type failed struct {
		ID       string `json:"id"`
		TestName string `json:"test_name"`
		TestCode string `json:"test_code"`
		Output   string `json:"output"`
	}
	in := struct {
		Spec   *Spec    `json:"spec"`
		Failed []failed `json:"failed"`
	}{Spec: spec}
	for _, h := range drafts {
		if h.DryVerdict == HoldoutUnrunnable {
			in.Failed = append(in.Failed, failed{ID: h.ID, TestName: h.TestName, TestCode: h.TestCode, Output: h.DryOutput})
		}
	}
	if len(in.Failed) == 0 {
		return nil, nil
	}
	content, _, err := r.supervisor.Chat(ctx, t.ID, alias, policy.PurposeReview,
		r.cfg.Policy().Document.Limits.SupervisorTimeout, holdoutFixSystem, in)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Holdouts []holdoutProposal `json:"holdouts"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil {
		return nil, fmt.Errorf("the answer is not the hold-outs json: %w", err)
	}
	out := map[string]string{}
	for _, pr := range answer.Holdouts {
		id, code := strings.TrimSpace(pr.ID), strings.TrimSpace(pr.TestCode)
		if id != "" && code != "" && len(code) <= maxHoldoutCode {
			out[id] = code
		}
	}
	return out, nil
}

const holdoutSystem = `You propose hold-out criteria for a software feature's spec: criteria its named proof does not cover, each with a Go test that fails on the repository's current main head and passes once the feature is built.
Rules:
- Propose at most the max named in the input; prefer none over a weak one.
- Each hold-out: an id (lowercase letters, digits, underscores), the criterion as one sentence in the spec's words, a test name (TestXxx), and the test's complete Go code (package holdout, standard library only, no network, no environment).
- The test must compile against the repository as it is on main and must fail there for the feature's absence.
- A hold-out may name one of the project's past failures (the from field cites it); a gap in the spec itself is drafted from the spec.
Answer one JSON object: {"holdouts":[{"id","criterion","test_name","test_code","from"}]}.`

const holdoutFixSystem = `These drafted Go tests could not run on the repository's main head; each carries its output. Rewrite each test so it compiles there and still fails for the feature's absence, keeping its id and its test name. Answer one JSON object: {"holdouts":[{"id","test_code"}]}.`

// DecideHoldout is the console's okay or drop of hold-out idx of round
// round of plan id, as a writer of its project: the round must be the
// newest (ErrStalePlan) and its confirmation undecided (ErrAnswered), as
// a changed spec asks the confirmation again;
// ErrHoldoutClick names a click the row's state takes not, ErrNoHoldout
// a row the task does not hold.
func DecideHoldout(ctx context.Context, store Store, taskID string, round, idx int, verb, by string) error {
	if _, err := pendingPlan(ctx, store, taskID); err != nil {
		return err
	}
	newest, ok, err := store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return err
	}
	if !ok || newest.Revision != round {
		return ErrStalePlan{Newest: newest.Revision}
	}
	qs, err := store.PlanQuestions(ctx, taskID, round)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(qs, func(q PlanQuestion) bool { return q.Kind == QuestionConfirmation && q.Answered() }) {
		return ErrAnswered
	}
	state := map[string]string{"okay": HoldoutStored, "drop": HoldoutDropped}[verb]
	if state == "" {
		return fmt.Errorf("%w: %s", ErrHoldoutClick, verb)
	}
	return store.DecidePlanHoldout(ctx, taskID, round, idx, state)
}

// HoldoutGateResult is one hold-out a criteria gate reported red or
// unrunnable at the card's head, as a workflow history may hold it; its
// evidence is one line of the run.
type HoldoutGateResult struct {
	Idx      int    `json:"idx"`
	ID       string `json:"id"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

// HoldoutSettlement is what settling the gate's red hold-outs came to,
// as the workflow reads it: Ask an escalation for the human whose answer
// runs the gate again; Findings route the card back to a session; else
// the hold-outs are settled and the gate runs again.
type HoldoutSettlement struct {
	Ask      string   `json:",omitempty"`
	Findings []string `json:",omitempty"`
	Settled  bool
}

// SettleHoldouts settles at once: no audit runs. The workflow's criteria
// loop calls it only for a history that recorded Holdouts.
func (r *Runner) SettleHoldouts(ctx context.Context, taskID string, pin int64, pr int64, commit string, reds []HoldoutGateResult) (HoldoutSettlement, error) {
	return HoldoutSettlement{Settled: true}, nil
}

// PutPlanHoldouts stores round's hold-out rows, the proposals with their
// dry-run verdicts: false when the round holds rows already, so an
// activity retry adds nothing.
func (s *PGStore) PutPlanHoldouts(ctx context.Context, id string, round int, drafts []Holdout) (bool, error) {
	b, err := json.Marshal(drafts)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO plan_holdouts (task_id, idx, round, id, origin, criterion, state, test_name, test_code, dry_head, dry_verdict, dry_output)
		SELECT $1, (h->>'idx')::int, $2, h->>'id', h->>'origin', h->>'criterion', h->>'state', h->>'test_name', h->>'test_code', h->>'dry_head', h->>'dry_verdict', h->>'dry_output'
		FROM jsonb_array_elements($3::jsonb) h
		WHERE NOT EXISTS (SELECT 1 FROM plan_holdouts WHERE task_id = $1 AND round = $2)
		ON CONFLICT DO NOTHING`, id, round, b)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PlanHoldouts are task id's hold-out rows in idx order, every round's.
func (s *PGStore) PlanHoldouts(ctx context.Context, id string) ([]Holdout, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, round, id, origin, criterion, state, test_name, test_code,
		dry_head, dry_verdict, dry_output FROM plan_holdouts
		WHERE task_id = $1 ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Holdout
	for rows.Next() {
		var h Holdout
		if err := rows.Scan(&h.Idx, &h.Round, &h.ID, &h.Origin, &h.Criterion, &h.State, &h.TestName, &h.TestCode,
			&h.DryHead, &h.DryVerdict, &h.DryOutput); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UndecidedHoldouts is how many of round's hold-outs wait for the
// human's okay or drop.
func (s *PGStore) UndecidedHoldouts(ctx context.Context, id string, round int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plan_holdouts
		WHERE task_id = $1 AND round = $2 AND state IN ('proposed', 'unrunnable')`, id, round).Scan(&n)
	return n, err
}

// DecidePlanHoldout stores the human's click on hold-out idx of round as
// its new state (stored or dropped): a row decided already, or a click
// its state takes not, is ErrHoldoutClick; a row the task does not hold
// is ErrNoHoldout.
func (s *PGStore) DecidePlanHoldout(ctx context.Context, id string, round, idx int, state string) error {
	var decided, holds bool
	err := s.db.QueryRowContext(ctx, `WITH done AS (UPDATE plan_holdouts SET state = $4, decided_at = now()
			WHERE task_id = $1 AND round = $2 AND idx = $3
				AND (($4 = 'dropped' AND state IN ('proposed', 'unrunnable'))
					OR ($4 = 'stored' AND state = 'proposed' AND dry_verdict IN ('red', 'green')))
			RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM done), EXISTS (SELECT 1 FROM plan_holdouts WHERE task_id = $1 AND idx = $3)`,
		id, round, idx, state).Scan(&decided, &holds)
	switch {
	case err != nil:
		return err
	case decided:
		return nil
	case holds:
		return ErrHoldoutClick
	default:
		return ErrNoHoldout
	}
}

// SweepPlanHoldouts drops every undecided hold-out of task id: the round
// closed (its confirmation answered, or a newer round asked), and a
// proposal no click can reach is not a hold-out.
func (s *PGStore) SweepPlanHoldouts(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plan_holdouts SET state = 'dropped', decided_at = now()
		WHERE task_id = $1 AND state IN ('proposed', 'unrunnable')`, id)
	return err
}

// PastReverts are the project's last revert objectives, one line each:
// the material a hold-out draft may cite, so the project's own failures
// shape its proofs.
func (s *PGStore) PastReverts(ctx context.Context, repository string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT objective FROM tasks
		WHERE repository = $1 AND source = 'revert' ORDER BY created_at DESC LIMIT 3`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var objective string
		if err := rows.Scan(&objective); err != nil {
			return nil, err
		}
		if line, _, _ := strings.Cut(objective, "\n"); strings.TrimSpace(line) != "" {
			out = append(out, textutil.Truncate(strings.TrimSpace(line), maxHoldoutText))
		}
	}
	return out, rows.Err()
}
