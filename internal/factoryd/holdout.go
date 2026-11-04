package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The hold-outs (the spec-intake concept, piece 6): criteria the confirmed
// spec's list misses, blind to the planner and the builder by routing and
// storage. A confirmation round's zone proposes them, the review model
// stating each as a claim with its inputs and expected numbers beside the
// project's past revert reasons, and no code; the human okays or drops
// each, and the prover writes an okayed one's test later, as it does every
// criterion's. Every blind proof is a plan_proofs row; stored hold-outs
// run at the criteria gate of the plan's integration card, their verdicts
// on the gate's timeline line alone.

// The plan proof states: a hold-out proposal waits for the human's okay
// or drop; okayed is a hold-out kept, a claim without a test until the
// prover files one; filed is the prover's last filing until its dry run
// on main; stored runs at the criteria gate; opened is one whose test is
// public.
const (
	ProofProposed = "proposed"
	ProofOkayed   = "okayed"
	ProofDropped  = "dropped"
	ProofFiled    = "filed"
	ProofStored   = "stored"
	ProofOpened   = "opened"
)

// The plan proof origins: a criterion of the plan, or a hold-out.
const (
	ProofOriginCriterion = "criterion"
	ProofOriginHoldout   = "hold-out"
)

// The flags a proof's dry run on main may raise.
const (
	ProofFlagGreen      = "green on main"
	ProofFlagUnrunnable = "unrunnable"
)

// PlanProof is one blind proof of a plan task as plan_proofs stores it: a
// criterion's test (origin criterion, Criterion its id) or a hold-out
// (origin hold-out), a claim with inputs and expected numbers until the
// prover files its test. Idx never changes; Round is the revision that
// added it. Path, Suite, Name and Code are the test, Artifact the file a
// judgement or human proof writes under CHECK_ARTIFACTS, Flag what its dry
// run on main found.
type PlanProof struct {
	Idx       int    `json:"idx"`
	Round     int    `json:"round"`
	ID        string `json:"id"`
	Origin    string `json:"origin"`
	Criterion string `json:"criterion"`
	Claim     string `json:"claim"`
	Inputs    string `json:"inputs"`
	Expected  string `json:"expected"`
	State     string `json:"state"`
	Path      string `json:"path"`
	Suite     string `json:"suite"`
	Name      string `json:"name"`
	Code      string `json:"code"`
	Artifact  string `json:"artifact"`
	Flag      string `json:"flag"`
}

// ErrHoldoutsOpen refuses a confirmation answer while its round's
// proposals wait for the human's okay or drop.
var ErrHoldoutsOpen = errors.New("the round's hold-outs wait for your okay or drop")

// ErrNoHoldout names a hold-out the task does not hold.
var ErrNoHoldout = errors.New("no such hold-out")

// ErrHoldoutClick refuses a click the hold-out's state takes not: a row
// decided already.
var ErrHoldoutClick = errors.New("the hold-out takes no such click now")

// holdoutID admits a hold-out id: the criterion charset.
var holdoutID = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// One round's and one claim's bounds.
const (
	maxHoldouts    = 5
	maxHoldoutText = 200
	holdoutPkg     = "holdout"
)

// pullRef is a pull request's head ref: what a repro pod fetches.
func pullRef(pr int64) string { return fmt.Sprintf("refs/pull/%d/head", pr) }

// holdoutEntry is a hold-out's id in the runner's list: criterionID
// admits no hyphen, so it never meets a criterion's.
func holdoutEntry(idx int) string { return fmt.Sprintf("holdout-%d", idx) }

// holdoutProof is the run of one hold-out's test: its file written into a
// scratch package of its own inside the repository, its test run there.
func holdoutProof(h PlanProof) harness.Proof {
	dir := fmt.Sprintf(".%s%d", holdoutPkg, h.Idx)
	return harness.Proof{ID: holdoutEntry(h.Idx), Suite: "./" + dir, Name: h.Name,
		File: &harness.ProofFile{Path: dir + "/holdout_test.go", Content: "package " + holdoutPkg + "\n" + h.Code + "\n"}}
}

// holdoutDryVerdict judges one hold-out's outcome: red is a hold-out's
// shape (the test runs and fails), green already passes (a guard to
// keep), unrunnable is anything else (no such test, a build that fails, a
// skip, no report).
func holdoutDryVerdict(o proofOutcome) (dry, evidence string) {
	ev := shortenEvidence(o.Message)
	switch o.Outcome {
	case proofFailed:
		return "red", ev
	case proofPassed:
		return "green", ev
	}
	return "unrunnable", ev
}

// holdoutProposal is the review model's proposal of one hold-out, as its
// JSON call answers. From, the past failure a proposal may cite, is
// admitted so a citing answer decodes, and stored nowhere.
type holdoutProposal struct {
	ID       string `json:"id"`
	Claim    string `json:"claim"`
	Inputs   string `json:"inputs"`
	Expected string `json:"expected"`
	From     string `json:"from"`
}

// askHoldouts runs a confirmation round's proposal: the review model
// states the claims the spec's criteria miss in one call, and the round's
// rows land proposed. The zone shows what landed; the human's only clicks
// are okay and drop. A failure leaves the round empty, the confirmation
// question stands on its own, and the timeline names why.
func (r *Runner) askHoldouts(ctx context.Context, t Task, revision int, p Plan) error {
	defer beatUntilDone(ctx, "holdouts")()
	rows, err := r.store.PlanProofs(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, h := range rows {
		if h.Round == revision {
			return nil // an activity retry: the round asked already
		}
	}
	if err := r.store.SweepPlanProofs(ctx, t.ID); err != nil {
		return err
	}
	reverts, err := r.store.PastReverts(ctx, t.Repository)
	if err != nil {
		return err
	}
	alias := r.callModel(t, policy.PurposeReview)
	if alias == "" {
		return nil
	}
	proposals, err := r.proposeHoldouts(ctx, t, revision, p, alias, reverts)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	if len(proposals) == 0 {
		return nil
	}
	inserted, err := r.store.PutPlanProofs(ctx, t.ID, revision, proposals)
	if err != nil || !inserted {
		return err
	}
	ids := make([]string, len(proposals))
	for i, h := range proposals {
		ids[i] = h.ID
	}
	r.record(ctx, t.ID, "holdouts_proposed", map[string]any{"round": revision, "holdouts": ids})
	return nil
}

// gateHoldouts are the plan task's hold-outs the criteria gate runs: the
// stored ones and the opened ones, in idx order. A read that fails runs
// the gate without them and names itself in the timeline.
func (r *Runner) gateHoldouts(ctx context.Context, parent string) []PlanProof {
	rows, err := r.store.PlanProofs(ctx, parent)
	if err != nil {
		r.record(ctx, parent, "gate_criteria", map[string]any{"holdouts_unread": err.Error()})
		return nil
	}
	var out []PlanProof
	for _, h := range rows {
		if h.Origin == ProofOriginHoldout && (h.State == ProofStored || h.State == ProofOpened) {
			out = append(out, h)
		}
	}
	return out
}

// proposeHoldouts is the one model call: the confirmed spec, the plan's
// criteria and the project's past failures in, the proposals out, each a
// proposed row of origin hold-out. Ids off the charset and claims, inputs
// or expected numbers empty or past maxHoldoutText runes are dropped; an
// id a plan criterion or an earlier row holds gains _h.
func (r *Runner) proposeHoldouts(ctx context.Context, t Task, revision int, p Plan, alias string, reverts []string) ([]PlanProof, error) {
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
	if err := decodeAnswer(content, &answer); err != nil {
		return nil, fmt.Errorf("the answer is not the hold-outs json: %w", err)
	}
	takenRows, err := r.store.PlanProofs(ctx, t.ID)
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
	field := func(s string) (string, bool) {
		s = strings.Join(strings.Fields(s), " ")
		return s, s != "" && utf8.RuneCountInString(s) <= maxHoldoutText
	}
	out := []PlanProof{}
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
		claim, ok1 := field(pr.Claim)
		inputs, ok2 := field(pr.Inputs)
		expected, ok3 := field(pr.Expected)
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		out = append(out, PlanProof{Idx: next + len(out), Round: revision, ID: id, Origin: ProofOriginHoldout,
			Claim: r.untrusted(t, claim), Inputs: r.untrusted(t, inputs), Expected: r.untrusted(t, expected), State: ProofProposed})
	}
	return out, nil
}

const holdoutSystem = `You propose hold-out criteria for a software feature's spec: claims its criteria do not cover, each proven later by a test a separate session writes against the repository.
Rules:
- Propose at most the max named in the input; prefer none over a weak one.
- Each hold-out: an id (lowercase letters, digits, underscores), the claim as one sentence in the spec's words, its inputs, and the numbers it expects. No code.
- State each claim at the feature's surface: what it does for a caller through its HTTP API, its command line, its store or the files it writes.
- A hold-out may come from one of the project's past failures; a gap in the spec itself is drafted from the spec.
Answer one JSON object: {"holdouts":[{"id","claim","inputs","expected"}]}.`

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
	state := map[string]string{"okay": ProofOkayed, "drop": ProofDropped}[verb]
	if state == "" {
		return fmt.Errorf("%w: %s", ErrHoldoutClick, verb)
	}
	return store.DecidePlanProof(ctx, taskID, round, idx, state)
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

// PutPlanProofs stores round's hold-out proposals: false when the round
// holds rows already, so an activity retry adds nothing.
func (s *PGStore) PutPlanProofs(ctx context.Context, id string, round int, proposals []PlanProof) (bool, error) {
	b, err := json.Marshal(proposals)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO plan_proofs (task_id, idx, round, id, origin, criterion, claim, inputs, expected,
			state, path, suite, name, code, artifact, flag)
		SELECT $1, (h->>'idx')::int, $2, h->>'id', h->>'origin', h->>'criterion', h->>'claim', h->>'inputs', h->>'expected',
			h->>'state', h->>'path', h->>'suite', h->>'name', h->>'code', h->>'artifact', h->>'flag'
		FROM jsonb_array_elements($3::jsonb) h
		WHERE NOT EXISTS (SELECT 1 FROM plan_proofs WHERE task_id = $1 AND round = $2)
		ON CONFLICT DO NOTHING`, id, round, b)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PlanProofs are task id's proof rows in idx order, every round's.
func (s *PGStore) PlanProofs(ctx context.Context, id string) ([]PlanProof, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, round, id, origin, criterion, claim, inputs, expected,
		state, path, suite, name, code, artifact, flag FROM plan_proofs
		WHERE task_id = $1 ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanProof
	for rows.Next() {
		var h PlanProof
		if err := rows.Scan(&h.Idx, &h.Round, &h.ID, &h.Origin, &h.Criterion, &h.Claim, &h.Inputs, &h.Expected,
			&h.State, &h.Path, &h.Suite, &h.Name, &h.Code, &h.Artifact, &h.Flag); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UndecidedProofs is how many of round's hold-outs wait for the human's
// okay or drop.
func (s *PGStore) UndecidedProofs(ctx context.Context, id string, round int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plan_proofs
		WHERE task_id = $1 AND round = $2 AND state = 'proposed'`, id, round).Scan(&n)
	return n, err
}

// DecidePlanProof stores the human's click on hold-out idx of round as its
// new state, okayed or dropped: a row decided already, or a click its
// state takes not, is ErrHoldoutClick; a hold-out the task does not hold
// is ErrNoHoldout.
func (s *PGStore) DecidePlanProof(ctx context.Context, id string, round, idx int, state string) error {
	var decided, holds bool
	err := s.db.QueryRowContext(ctx, `WITH done AS (UPDATE plan_proofs SET state = $4, decided_at = now()
			WHERE task_id = $1 AND round = $2 AND idx = $3 AND origin = 'hold-out' AND state = 'proposed'
				AND $4 IN ('okayed', 'dropped')
			RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM done), EXISTS (SELECT 1 FROM plan_proofs WHERE task_id = $1 AND idx = $3 AND origin = 'hold-out')`,
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

// SweepPlanProofs drops every hold-out proposal of task id: the round
// closed (its confirmation answered, or a newer round asked), and a
// proposal no click can reach is not a hold-out.
func (s *PGStore) SweepPlanProofs(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plan_proofs SET state = 'dropped', decided_at = now()
		WHERE task_id = $1 AND state = 'proposed'`, id)
	return err
}

// PastReverts are the project's last revert objectives, one line each:
// the material a hold-out proposal may draw on, so the project's own
// failures shape its proofs.
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
