package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The spec's verdict as data (the spec-intake concept, pieces 7 and 8,
// roadmap 290): every criterion of a plan carries a verdict row on the
// task that holds the plan's criteria, the integration card. The review
// writes its rows on every review; an approver's recorded decision — a
// human criterion's met at the merge approval, a waiver — writes the
// approver's row, which is the verdict that stands.
const (
	CatchDelta        = "delta"
	CatchDeltaFiled   = "delta_filed"
	CatchDeltaRefused = "delta_refused"
	CatchBehaviour    = "behaviour"
	FinderReview      = "review"
	FinderApprover    = "approver"
)

// CriterionVerdict is the verdict on one criterion: the review's answer
// (criterion, verdict, evidence, fix) or the standing row the store reads
// (with kind, source and decided_by). A human criterion's review row
// carries evidence and an empty verdict: the approver's decision at the
// merge approval is its verdict.
type CriterionVerdict struct {
	Criterion string    `json:"criterion"`
	Verdict   string    `json:"verdict"`
	Evidence  string    `json:"evidence,omitempty"`
	Fix       string    `json:"fix,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Source    string    `json:"-"`
	DecidedBy string    `json:"-"`
	CreatedAt time.Time `json:"-"`
}

// PutCriteriaVerdicts stores task's verdict rows of one source, each row
// over the row of the same criterion and source.
func (s *PGStore) PutCriteriaVerdicts(ctx context.Context, taskID, source, by string, rows []CriterionVerdict) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO criteria_verdicts (task_id, criterion_id, kind, verdict, evidence, fix, source, decided_by)
		SELECT $1, (v->>'criterion'), COALESCE(v->>'kind', ''), COALESCE(v->>'verdict', ''), COALESCE(v->>'evidence', ''), COALESCE(v->>'fix', ''), $2, $3
		FROM jsonb_array_elements($4::jsonb) AS a(v)
		ON CONFLICT (task_id, criterion_id, source) DO UPDATE SET verdict = EXCLUDED.verdict, evidence = EXCLUDED.evidence,
			fix = EXCLUDED.fix, decided_by = EXCLUDED.decided_by, created_at = now()`,
		taskID, source, by, string(b))
	return err
}

// CriteriaVerdicts are task's verdict rows, in criterion order.
func (s *PGStore) CriteriaVerdicts(ctx context.Context, taskID string) ([]CriterionVerdict, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT criterion_id, COALESCE(kind, ''), verdict, evidence, fix, source, COALESCE(decided_by, ''), created_at
		FROM criteria_verdicts WHERE task_id = $1 ORDER BY criterion_id, source`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CriterionVerdict
	for rows.Next() {
		var v CriterionVerdict
		if err := rows.Scan(&v.Criterion, &v.Kind, &v.Verdict, &v.Evidence, &v.Fix, &v.Source, &v.DecidedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CriteriaVerdictTasks are the tasks that carry verdict rows, the plans
// whose criteria the sweep judges.
func (s *PGStore) CriteriaVerdictTasks(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT task_id FROM criteria_verdicts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PlanCatch is one row of the recording rule's ledger.
type PlanCatch struct {
	TaskID   string
	Revision int
	Kind     string
	Finder   string
	Detail   string
}

// PlanCatches are plan task id's catches, revision, kind and idx order.
func (s *PGStore) PlanCatches(ctx context.Context, id string) ([]PlanCatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, revision, kind, finder, detail FROM plan_catches
		WHERE task_id = $1 ORDER BY revision, kind, idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanCatch
	for rows.Next() {
		var c PlanCatch
		if err := rows.Scan(&c.TaskID, &c.Revision, &c.Kind, &c.Finder, &c.Detail); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrNoCriterion refuses a decision on a criterion the plan does not hold.
var ErrNoCriterion = errors.New("the plan holds no such criterion")

// ErrNotHuman refuses an approver's met on a criterion whose verdict the
// review owns.
var ErrNotHuman = errors.New("only a human criterion's verdict is the approver's to record")

// planOfStore is acceptedPlan without the Runner: the newest plan revision
// of plan task id, and its number.
func planOfStore(ctx context.Context, store Store, parent, child string) (Plan, int, error) {
	rev, ok, err := store.PlanRevision(ctx, parent, 0)
	if err != nil || !ok {
		return Plan{}, 0, cmp.Or(err, error(terminalError("plan %s of card %s has no revision", parent, child)))
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return Plan{}, 0, terminalError("plan %s revision %d: %v", parent, rev.Revision, err)
	}
	return p, rev.Revision, nil
}

// DecideCriterion records an approver's decision on one criterion of plan
// task id: waived, with their one-line reason, for any criterion; met, for
// a human criterion, whose proof is this recording. The row lands on the
// plan's integration card, where the review's rows stand.
func DecideCriterion(ctx context.Context, store Store, id, cid, verdict, by, reason string) error {
	if verdict != "waived" && verdict != "met" {
		return fmt.Errorf("no such verdict %q", verdict)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2000 {
		return errors.New("a one-line reason is required")
	}
	t, ok, err := store.Task(ctx, id)
	if err != nil {
		return fmt.Errorf("plan %s: %w", id, err)
	}
	if !ok {
		return fmt.Errorf("plan %s: %w", id, ErrNotFound)
	}
	p, _, err := planOfStore(ctx, store, id, t.ID)
	if err != nil {
		return err
	}
	var c PlanCriterion
	if i := slices.IndexFunc(p.Criteria, func(c PlanCriterion) bool { return c.ID == cid }); i < 0 {
		return ErrNoCriterion
	} else {
		c = p.Criteria[i]
	}
	if verdict == "met" && c.Kind != CriterionHuman {
		return ErrNotHuman
	}
	integration, err := integrationCard(ctx, store, id, p)
	if err != nil {
		return err
	}
	return store.PutCriteriaVerdicts(ctx, integration.ID, "approver", by,
		[]CriterionVerdict{{Criterion: cid, Verdict: verdict, Evidence: reason, Kind: c.Kind}})
}

// GrantCriteria writes the catch rows of an approver's grant of one more
// bounded card on plan task id: one delta due per criterion still open, so
// the sweep's round drafts it. It reports how many criteria the grant
// covers; 0 when every criterion is met or waived already.
func GrantCriteria(ctx context.Context, store Store, id, by string, approval int64) (int, error) {
	t, ok, err := store.Task(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("plan %s: %w", id, err)
	}
	if !ok {
		return 0, fmt.Errorf("plan %s: %w", id, ErrNotFound)
	}
	p, revision, err := planOfStore(ctx, store, id, t.ID)
	if err != nil {
		return 0, err
	}
	integration, err := integrationCard(ctx, store, id, p)
	if err != nil {
		return 0, err
	}
	rows, err := store.CriteriaVerdicts(ctx, integration.ID)
	if err != nil {
		return 0, err
	}
	var details []string
	for _, c := range openCriteria(p, rows) {
		details = append(details, fmt.Sprintf("%s grant %d", c.ID, approval))
	}
	if len(details) == 0 {
		return 0, nil
	}
	if _, err := store.PutPlanCatches(ctx, id, revision, CatchDelta, FinderApprover, details); err != nil {
		return 0, err
	}
	log.Printf("plan %s: %s granted one more bounded card for %d criteria", id, by, len(details))
	return len(details), nil
}

// RecordHumanCriteria records the approver's met on every human criterion
// of the plan of task taskID's plan, the verdict their merge approval of
// the feature is (a human criterion's proof is the same recording):
// nothing when taskID is not a plan's integration card.
func RecordHumanCriteria(ctx context.Context, store Store, taskID, approver string) (int, error) {
	t, ok, err := store.Task(ctx, taskID)
	if err != nil {
		return 0, fmt.Errorf("task %s: %w", taskID, err)
	}
	if !ok {
		return 0, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if t.Source != SourcePlan || t.Parent == "" {
		return 0, nil
	}
	p, _, err := planOfStore(ctx, store, t.Parent, t.ID)
	if err != nil {
		return 0, err
	}
	if roleOf(p, t.PlanKey) != RoleIntegration {
		return 0, nil
	}
	var rows []CriterionVerdict
	for _, c := range p.Criteria {
		if c.Kind != CriterionHuman {
			continue
		}
		rows = append(rows, CriterionVerdict{Criterion: c.ID, Verdict: "met",
			Evidence: "merge approved by " + approver, Kind: c.Kind})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return len(rows), store.PutCriteriaVerdicts(ctx, t.ID, "approver", approver, rows)
}

// roleOf is the plan's role of card key.
func roleOf(p Plan, key string) string {
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == key }); i >= 0 {
		return p.Cards[i].Role
	}
	return ""
}

// integrationCard is the plan's integration card task.
func integrationCard(ctx context.Context, store Store, parent string, p Plan) (Task, error) {
	cards, err := store.PlanCards(ctx, parent)
	if err != nil {
		return Task{}, err
	}
	i := slices.IndexFunc(cards, func(c Task) bool { return c.Source == SourcePlan && roleOf(p, c.PlanKey) == RoleIntegration })
	if i < 0 {
		return Task{}, terminalError("plan %s has no integration card", parent)
	}
	return cards[i], nil
}

// effectiveVerdict is a criterion's standing verdict: the approver's row
// over the review's; "" when no row holds one.
func effectiveVerdict(rows []CriterionVerdict, criterion string) string {
	verdict := ""
	for _, v := range rows {
		if v.Criterion != criterion {
			continue
		}
		if v.Source == "approver" {
			return v.Verdict
		}
		if verdict == "" {
			verdict = v.Verdict
		}
	}
	return verdict
}

// openCriteria are the plan's criteria whose standing verdict is neither
// met nor waived.
func openCriteria(p Plan, rows []CriterionVerdict) []PlanCriterion {
	var open []PlanCriterion
	for _, c := range p.Criteria {
		if v := effectiveVerdict(rows, c.ID); v != "met" && v != "waived" {
			open = append(open, c)
		}
	}
	return open
}

// reviewCriteria is the criteria addendum to the review's answer format
// when the task is a plan's integration card: the verdict per criterion,
// the behaviours, and what evidence and fix mean.
const reviewCriteria = ` The task carries the plan's criteria: criteria lists each with its id, kind, test, inputs, expected, claim and prompt. Add "criteria": [{"criterion": "<id>", "verdict": "met|partial|unmet", "evidence": "...", "fix": "card|planner"}], one entry for every criterion. Evidence names the test that ran and its numbers, or what you saw in the diff; never just the criterion's words back. fix card when the gap belongs to this card's own directories; planner when the fix reaches beyond them or reopens a merged card's items. For a criterion of kind human, verdict is "" and evidence says what the human should look at: their approval is its verdict. Add "behaviours": ["..."], one line per behaviour the feature has as a whole, each a sentence a later change can be judged against.`

// criteriaBlock is the criteria as the review input carries them.
func criteriaBlock(p Plan) string {
	b, err := json.Marshal(p.Criteria)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// normalizeCriteria fits the model's criteria answer to the plan: unknown
// ids drop, a human criterion keeps its evidence and loses its verdict and
// fix, a criterion of any other kind without a verdict drops. Behaviours
// are trimmed and capped.
func normalizeCriteria(p Plan, answer []CriterionVerdict, behaviours []string) ([]CriterionVerdict, []string) {
	byID := map[string]PlanCriterion{}
	for _, c := range p.Criteria {
		byID[c.ID] = c
	}
	var out []CriterionVerdict
	seen := map[string]bool{}
	for _, v := range answer {
		c, ok := byID[v.Criterion]
		if !ok || seen[v.Criterion] {
			continue
		}
		v.Verdict, v.Evidence, v.Fix = strings.TrimSpace(v.Verdict), strings.TrimSpace(v.Evidence), strings.TrimSpace(v.Fix)
		if c.Kind == CriterionHuman {
			v.Verdict, v.Fix = "", ""
		} else {
			switch v.Verdict {
			case "met", "partial", "unmet":
			default:
				continue
			}
			if v.Fix != "card" && v.Fix != "planner" {
				v.Fix = ""
			}
		}
		if len(v.Evidence) > maxReviewFinding {
			v.Evidence = v.Evidence[:maxReviewFinding]
		}
		seen[v.Criterion] = true
		v.Kind = c.Kind
		out = append(out, v)
	}
	var bs []string
	for _, b := range behaviours {
		if b = strings.TrimSpace(b); b != "" && len(bs) < maxReviewFindings {
			if len(b) > maxReviewFinding/3 {
				b = b[:maxReviewFinding/3]
			}
			bs = append(bs, b)
		}
	}
	return out, bs
}

// reviewCriteriaStep is the review's criteria half (piece 7), run when the
// reviewed task is the plan's integration card: it stores the verdict rows,
// turns an unmet criterion that belongs to this card into a finding, hands
// one too large for it to the planner as a delta due, and notes the
// feature's behaviours as records that notify no one.
func (r *Runner) reviewCriteriaStep(ctx context.Context, t Task, repo Repo, p Plan, revision, round int, rv *Review) {
	planned := map[string]bool{}
	for _, c := range p.Cards {
		planned[c.Key] = true
	}
	if len(p.Criteria) == 0 || t.Parent == "" || (roleOf(p, t.PlanKey) != RoleIntegration && planned[t.PlanKey]) {
		rv.Criteria, rv.Behaviours = nil, nil
		return
	}
	integration, err := integrationCard(ctx, r.store, t.Parent, p)
	if err != nil {
		log.Printf("task %s: the criteria rows' home: %v", t.ID, err)
		rv.Criteria, rv.Behaviours = nil, nil
		return
	}
	rows, behaviours := normalizeCriteria(p, rv.Criteria, rv.Behaviours)
	rv.Criteria, rv.Behaviours = rows, behaviours
	if len(rows) > 0 {
		if err := r.store.PutCriteriaVerdicts(ctx, integration.ID, "review", FactoryDecider, rows); err != nil {
			log.Printf("task %s: store the criteria verdicts: %v", integration.ID, err)
		}
	}
	var planner []string
	for _, v := range rows {
		switch {
		case v.Verdict == "met":
		case v.Fix == "planner":
			planner = append(planner, fmt.Sprintf("%s at %s round %d", v.Criterion, t.PlanKey, round))
		case v.Verdict != "":
			rv.Findings = append(rv.Findings, fmt.Sprintf("criterion %s not met (%s): %s", v.Criterion, v.Verdict, v.Evidence))
		}
	}
	if len(planner) > 0 {
		if _, err := r.store.PutPlanCatches(ctx, t.Parent, revision, CatchDelta, FinderReview, planner); err != nil {
			log.Printf("task %s: record the delta dues: %v", t.ID, err)
		}
	}
	if len(behaviours) > 0 {
		var lines []string
		for _, b := range behaviours {
			lines = append(lines, "- "+b)
		}
		r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: the feature's behaviours\n\n"+strings.Join(lines, "\n"))
		if _, err := r.store.PutPlanCatches(ctx, t.Parent, revision, CatchBehaviour, FinderReview, behaviours); err != nil {
			log.Printf("task %s: record the behaviours: %v", t.ID, err)
		}
	}
	r.record(ctx, t.ID, "criteria_verdicts", map[string]any{"round": round, "criteria": rows, "behaviours": behaviours})
}

// deltaSystem is the delta round's system prompt: one card under the open
// plan that closes the named criteria.
const deltaSystem = `You are the planner of a software factory. A plan's criteria are not met after its cards ran. You draft one more bounded card — a delta card — under the open plan, which closes the gap the criteria name. Answer with one JSON object and nothing else: {"card": {"key": "...", "role": "feature", "title": "...", "brief": "...", "directories": ["..."], "depends_on": ["keys of the plan's existing cards"], "acceptance": [{"criterion": "the criterion id", "text": "the command and its expected result"}], "size": "small|medium|large", "rung": "cheap|frontier", "rung_reason": "...", "image": "..."}}. The card's key is new. directories are the directories the card may change. depends_on names only keys the plan holds. acceptance carries every criterion the card closes, its command the criterion's test. Keep the card small: it closes the gap, nothing else.`

// deltaPrompt is the delta round's system prompt for a project pinning
// pins.
func deltaPrompt(pins map[string]policy.ImagePin) string {
	return strings.TrimSpace(deltaSystem + " " + imagesLine(pins))
}

// deltaInput is the delta round's whole context.
type deltaInput struct {
	Spec     *Spec              `json:"spec,omitempty"`
	Summary  string             `json:"summary"`
	Criteria []deltaCriterion   `json:"criteria"`
	Verdicts []CriterionVerdict `json:"verdicts"`
	Cards    []PlanCard         `json:"cards"`
	Unfiled  []string           `json:"unfiled_keys,omitempty"`
}

// deltaCriterion is one open criterion as the delta round reads it.
type deltaCriterion struct {
	PlanCriterion
	Evidence string `json:"evidence"`
}

// draftDelta runs one delta round on plan t (the planner's one model call,
// the hold-out round's shape) and files the card it answers: the card is
// merged into the plan and judged by the plan rules again (readPlan), then
// filed like any child (ruling 225). Dues are the catch rows the round
// answers; they are marked filed or refused when it ends, so the round runs
// once per due. A failed call stays due: the next pass tries again.
func (r *Runner) draftDelta(ctx context.Context, t Task, repo Repo, dues []PlanCatch) {
	p, revision, err := planOfStore(ctx, r.store, t.ID, "")
	if err != nil {
		log.Printf("plan %s: the delta round: %v", t.ID, err)
		return
	}
	integration, err := integrationCard(ctx, r.store, t.ID, p)
	if err != nil {
		log.Printf("plan %s: the delta round: %v", t.ID, err)
		return
	}
	verdicts, err := r.store.CriteriaVerdicts(ctx, integration.ID)
	if err != nil {
		log.Printf("plan %s: the delta round: %v", t.ID, err)
		return
	}
	open := openCriteria(p, verdicts)
	var criteria []deltaCriterion
	for _, c := range open {
		dc := deltaCriterion{PlanCriterion: c}
		for _, v := range verdicts {
			if v.Criterion == c.ID && v.Evidence != "" {
				dc.Evidence = v.Evidence
				break
			}
		}
		criteria = append(criteria, dc)
	}
	if len(criteria) == 0 {
		return // the gap closed between the due and the round
	}
	cards, err := r.store.PlanCards(ctx, t.ID)
	if err != nil {
		log.Printf("plan %s: the delta round: %v", t.ID, err)
		return
	}
	filed := map[string]bool{}
	for _, c := range cards {
		filed[c.PlanKey] = true
	}
	var unfiled []string
	for _, c := range p.Cards {
		if !filed[c.Key] {
			unfiled = append(unfiled, c.Key)
		}
	}
	input := deltaInput{Spec: p.Spec, Summary: p.Summary, Criteria: criteria, Verdicts: verdicts, Cards: p.Cards, Unfiled: unfiled}
	alias := r.callModel(t, policy.PurposeReview)
	content, _, err := r.supervisor.Chat(ctx, t.ID, alias, policy.PurposeReview,
		r.cfg.Policy().Document.Limits.SupervisorTimeout, deltaPrompt(projectPins(r.cfg.Policy(), t.Repository)), input)
	if err != nil {
		log.Printf("plan %s: the delta round's call: %v", t.ID, err)
		return
	}
	var answer struct {
		Card PlanCard `json:"card"`
	}
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&answer); err != nil {
		log.Printf("plan %s: the delta round's answer: %v", t.ID, err)
		r.refuseDelta(ctx, t, repo, dues, "the answer is not a card: "+err.Error())
		return
	}
	merged := p
	merged.Cards = append(slices.Clone(p.Cards), answer.Card)
	b, err := json.Marshal(merged)
	if err != nil {
		r.refuseDelta(ctx, t, repo, dues, err.Error())
		return
	}
	v, err := r.pinnedVersion(ctx, 0)
	if err != nil {
		log.Printf("plan %s: the delta round: %v", t.ID, err)
		return
	}
	shape, process, err := planShape(v, t)
	if err != nil {
		r.refuseDelta(ctx, t, repo, dues, err.Error())
		return
	}
	if _, findings := readPlan("```json\n"+string(b)+"\n```", shape, v.Document.Limits, projectPins(v, t.Repository)); len(findings) > 0 {
		r.refuseDelta(ctx, t, repo, dues, "the card broke the plan rules: "+strings.Join(findings, "; "))
		return
	}
	if _, err := r.fileChildren(ctx, t, repo, v, process, fmt.Sprintf("plan #%d", t.IssueNumber), merged, FactoryDecider); err != nil {
		log.Printf("plan %s: file the delta card: %v", t.ID, err)
		return
	}
	r.markCatches(ctx, t.ID, revision, CatchDeltaFiled, dues)
	var ids []string
	for _, c := range criteria {
		ids = append(ids, c.ID)
	}
	r.commentOnce(ctx, t, repo, t.IssueNumber, fmt.Sprintf("factoryd: filed the delta card %s for criteria %s (one planner round).", answer.Card.Key, strings.Join(ids, ", ")))
	r.record(ctx, t.ID, "delta_card", map[string]any{"key": answer.Card.Key, "criteria": ids, "finder": dues[0].Finder})
}

// refuseDelta marks the dues refused and says so on the plan's issue: the
// human grants again or waives.
func (r *Runner) refuseDelta(ctx context.Context, t Task, repo Repo, dues []PlanCatch, why string) {
	_, revision, err := planOfStore(ctx, r.store, t.ID, "")
	if err != nil {
		log.Printf("plan %s: the refused delta: %v", t.ID, err)
		return
	}
	r.markCatches(ctx, t.ID, revision, CatchDeltaRefused, dues)
	r.commentOnce(ctx, t, repo, t.IssueNumber, fmt.Sprintf("factoryd: the delta round for criteria %s did not pass the plan rules: %s. Grant it again or waive a criterion.", strings.Join(duesDetail(dues), ", "), why))
	r.record(ctx, t.ID, "delta_refused", map[string]any{"criteria": duesDetail(dues), "why": why})
}

// markCatches writes the marker kind over the dues, so a due the round
// answered never runs again.
func (r *Runner) markCatches(ctx context.Context, id string, revision int, kind string, dues []PlanCatch) {
	var details []string
	for _, d := range dues {
		details = append(details, d.Detail)
	}
	if _, err := r.store.PutPlanCatches(ctx, id, revision, kind, dues[0].Finder, details); err != nil {
		log.Printf("plan %s: mark the delta dues %s: %v", id, kind, err)
	}
}

// duesDetail are the dues' criterion names, the detail before its first
// space.
func duesDetail(dues []PlanCatch) []string {
	out := make([]string, 0, len(dues))
	for _, d := range dues {
		if f := strings.Fields(d.Detail); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// CriteriaSpec is the criteria approval's spec: the plan revision the gap
// is under and the task that carries the verdict rows.
type CriteriaSpec struct {
	Revision int    `json:"revision"`
	Task     string `json:"task"`
}

// planClosed says the plan task's timeline already holds the plan's close.
func (r *Runner) planClosed(ctx context.Context, id string) bool {
	if r.timeline == nil || r.timeline.Objects == nil {
		return false
	}
	b, err := r.timeline.Objects.Get(ctx, "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte(`"event":"`+EventPlanClosed+`"`))
}

// planCloseCheck is the plan-close rule (piece 8): a plan closes when
// every criterion is met or waived; when its cards are all final and a
// criterion is still open, its gap is a criterion row, visible and owned:
// a criteria approval asks the human to grant one more bounded card or
// waive, and pokes them. A gap it asked about before, in the same shape,
// asks again never: only a changed gap re-opens the question.
func (r *Runner) planCloseCheck(ctx context.Context, parent string) {
	t, repo, err := r.task(ctx, parent)
	if err != nil {
		log.Printf("plan %s: the close check: %v", parent, err)
		return
	}
	p, _, err := planOfStore(ctx, r.store, parent, "")
	if err != nil {
		log.Printf("plan %s: the close check: %v", parent, err)
		return
	}
	if len(p.Criteria) == 0 {
		return
	}
	integration, err := integrationCard(ctx, r.store, parent, p)
	if err != nil {
		log.Printf("plan %s: the close check: %v", parent, err)
		return
	}
	rows, err := r.store.CriteriaVerdicts(ctx, integration.ID)
	if err != nil {
		log.Printf("plan %s: the close check: %v", parent, err)
		return
	}
	if len(openCriteria(p, rows)) == 0 {
		if r.planClosed(ctx, parent) {
			return
		}
		if err := r.store.ResolveApprovals(ctx, parent, ApprovalCriteria, "approved", FactoryDecider, "every criterion is met or waived"); err != nil {
			log.Printf("plan %s: close the criteria approval: %v", parent, err)
		}
		r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: every criterion of this plan is met or waived; the plan is closed.")
		r.timeline.recordEvent(ctx, parent, EventPlanClosed, map[string]any{"criteria": len(p.Criteria)})
		r.record(ctx, parent, "plan_closed", map[string]any{"criteria": len(p.Criteria)})
		return
	}
	cards, err := r.store.PlanCards(ctx, parent)
	if err != nil {
		log.Printf("plan %s: the close check: %v", parent, err)
		return
	}
	if !slices.ContainsFunc(cards, func(c Task) bool { return !Final(c.Status) }) {
		r.openGap(ctx, t, repo, p, openCriteria(p, rows), integration)
	}
}

// openGap opens the plan's criteria approval once per shape of gap.
func (r *Runner) openGap(ctx context.Context, t Task, repo Repo, p Plan, open []PlanCriterion, integration Task) {
	var ids []string
	for _, c := range open {
		ids = append(ids, c.ID)
	}
	reason := fmt.Sprintf("criteria not met with the plan's cards done: %s", strings.Join(ids, ", "))
	for _, status := range []string{"pending", "denied"} {
		all, err := r.store.ApprovalRequests(ctx, status)
		if err != nil {
			log.Printf("plan %s: the gap check: %v", t.ID, err)
			return
		}
		for _, q := range all {
			if q.TaskID == t.ID && q.Kind == ApprovalCriteria && (status == "pending" || q.Reason == reason) {
				return
			}
		}
	}
	spec, err := json.Marshal(CriteriaSpec{Task: integration.ID})
	if err != nil {
		return
	}
	id, err := r.store.OpenApproval(ctx, ApprovalRequest{
		Kind: ApprovalCriteria, TaskID: t.ID, Project: t.Repository, Spec: spec, Reason: reason,
	})
	if err != nil {
		log.Printf("plan %s: open the criteria approval: %v", t.ID, err)
		return
	}
	var lines []string
	for _, c := range open {
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", c.ID, c.Kind, cmp.Or(c.Test, c.Claim)))
	}
	r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: the plan's cards ran out with a criterion still unmet.\n\n"+strings.Join(lines, "\n")+
		"\n\nGrant one more bounded card, or waive a criterion with your reason.")
	r.record(ctx, t.ID, "approval_requested", map[string]any{"approval": id, "kind": ApprovalCriteria, "criteria": ids})
	r.timeline.RecordRow(ctx, t.ID, EventApprovalRequested, FactoryDecider, ViaFactoryd,
		map[string]any{"approval": id, "kind": ApprovalCriteria, "criteria": ids})
}

// CriteriaPass is the criteria sweep, run beside the stale sweep: it closes
// the plans whose criteria are all met or waived, opens the gap question of
// a plan whose cards ran out with one still open, and drafts the delta
// cards the reviews and the grants asked for. One model call per delta due,
// in the sweep's own time: no activity holds it.
func (r *Runner) CriteriaPass(ctx context.Context) {
	ids, err := r.store.CriteriaVerdictTasks(ctx)
	if err != nil {
		log.Printf("criteria pass: the verdict tasks: %v", err)
		return
	}
	parents := map[string]bool{}
	for _, id := range ids {
		t, ok, err := r.store.Task(ctx, id)
		if err != nil {
			log.Printf("criteria pass: task %s: %v", id, err)
			continue
		}
		if ok && t.Source == SourcePlan && t.Parent != "" {
			parents[t.Parent] = true
		}
	}
	for _, parent := range slices.Sorted(maps.Keys(parents)) {
		r.planCloseCheck(ctx, parent)
		r.draftDue(ctx, parent)
	}
}

// draftDue drafts the plan's unanswered delta dues: the delta catches no
// filed or refused catch marks.
func (r *Runner) draftDue(ctx context.Context, parent string) {
	catches, err := r.store.PlanCatches(ctx, parent)
	if err != nil {
		log.Printf("plan %s: the delta dues: %v", parent, err)
		return
	}
	marked := map[string]bool{}
	dues := map[string]PlanCatch{}
	for _, c := range catches {
		switch c.Kind {
		case CatchDelta:
			dues[c.Detail] = c
		case CatchDeltaFiled, CatchDeltaRefused:
			marked[c.Detail] = true
		}
	}
	var due []PlanCatch
	for _, d := range slices.Sorted(maps.Keys(dues)) {
		if !marked[d] {
			due = append(due, dues[d])
		}
	}
	if len(due) == 0 {
		return
	}
	t, repo, err := r.task(ctx, parent)
	if err != nil {
		log.Printf("plan %s: the delta round: %v", parent, err)
		return
	}
	r.draftDelta(ctx, t, repo, due)
}

// criteriaContext is the review's criteria half's context (piece 7): the
// plan when the reviewed card's review sees the whole feature, which the
// integration card's does and a delta card's does — a card no plan
// revision holds arrived by a delta round, after the cards before it. nil
// when this review does not answer the plan's criteria.
type criteriaContext struct {
	plan     Plan
	revision int
}

// criteriaContext of task t, nil when its review answers no criteria: a
// plan child of a plan without criteria, or a card whose review sees only
// its own part of the feature.
func (r *Runner) criteriaContext(ctx context.Context, t Task) *criteriaContext {
	if t.Source != SourcePlan || t.Parent == "" {
		return nil
	}
	p, revision, err := planOfStore(ctx, r.store, t.Parent, t.ID)
	if err != nil {
		log.Printf("task %s: the criteria context: %v", t.ID, err)
		return nil
	}
	if len(p.Criteria) == 0 {
		return nil
	}
	if roleOf(p, t.PlanKey) != RoleIntegration && slices.ContainsFunc(p.Cards, func(c PlanCard) bool { return c.Key == t.PlanKey }) {
		return nil
	}
	return &criteriaContext{plan: p, revision: revision}
}

// wholeFeatureDiff is the feature's whole diff (piece 7): every filed card
// of the plan in the plan's order, then the cards a delta round filed, the
// reviewed card's diff last. A card whose pull request cannot be read
// fails the read: the criteria verdicts judge the whole feature or do not
// stand.
func (r *Runner) wholeFeatureDiff(ctx context.Context, task Task, repo Repo, p Plan, mine string) (string, error) {
	cards, err := r.store.PlanCards(ctx, task.Parent)
	if err != nil {
		return "", err
	}
	byKey := map[string]Task{}
	for _, c := range cards {
		byKey[c.PlanKey] = c
	}
	planned := map[string]bool{}
	var order []Task
	for _, c := range p.Cards {
		planned[c.Key] = true
		if card, ok := byKey[c.Key]; ok && c.Key != task.PlanKey {
			order = append(order, card)
		}
	}
	var extras []string
	for key, card := range byKey {
		if !planned[key] && key != task.PlanKey && card.PRNumber != 0 {
			extras = append(extras, key)
		}
	}
	slices.Sort(extras)
	for _, key := range extras {
		order = append(order, byKey[key])
	}
	diffs := make([]string, 0, len(order)+1)
	for _, c := range order {
		if c.PRNumber == 0 {
			continue
		}
		d, err := r.forgejo.PullRequestDiff(ctx, repo.Owner, repo.Name, c.PRNumber, maxForgejoAnswer)
		if err != nil {
			return "", fmt.Errorf("card %s: %w", c.PlanKey, err)
		}
		diffs = append(diffs, d)
	}
	return strings.Join(append(diffs, mine), "\n"), nil
}
