package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// criterionRow is one criterion of a criteria or merge approval card: the
// criterion, its standing verdict and the evidence behind it.
type criterionRow struct {
	factoryd.PlanCriterion
	Verdict  string
	Evidence string
	Word     string                 // "blind" or "open" for a met gate row, from its evidence
	Artifact *factoryd.GateArtifact // a human criterion's artifact as the gate's newest run wrote it; nil without one
	CutLine  string                 // "the first 8 KiB of <n> KiB" when the card shows the artifact's first maxCardArtifact bytes
	Human    bool                   // Met and Not met are offered: a human criterion on a merge card
}

// maxCardArtifact is the most of an artifact a merge card shows.
const maxCardArtifact = 8 << 10

// approvalCard is a pending approval request with what its kind needs.
type approvalCard struct {
	factoryd.ApprovalRequest
	IDSuffix string // on the item's ids: "-d" in the drawer, so the column's copy keeps its own
	IssueURL string
	Issue    int64
	PlanName string // a plan approval: the plan's name
	PRURL    string
	Monthly  float64
	Merge    factoryd.MergeSpec
	// Healed: a merge request opened on red checks whose recorded head's
	// checks are green now (#1013, a re-run), so the card offers its Approve
	// again and says so.
	Healed bool
	// HeadUnread: the head's current checks could not be read (#1013), so
	// the card shows the face its frozen spec gives it, which may be
	// stale; the card says so rather than fail silently.
	HeadUnread bool
	// HeadUnreadWhy: that read's error, scrubbed, named in the card's
	// problem sentence.
	HeadUnreadWhy string
	Resource      cloudres.ResourceSpec
	// a plan's criteria gap: the spec, the open criteria with their
	// standing verdicts, whether every card of the plan is delivered
	// (done or merged), which the confirm button keys on, and whether a
	// criterion is still open, which the grant answers
	CriteriaSpec factoryd.CriteriaSpec
	Criteria     []criterionRow
	Delivered    bool
	Grantable    bool
	// Unsettled: a merge card's human criteria with no verdict, which hold
	// its Approve. A gate criterion without a row does not: the press
	// reaches the workflow, which runs the gate again.
	Unsettled []string
	// a plan: its revision, the summary, the cards in filing order and the
	// keys it takes off main
	PlanSpec    factoryd.PlanSpec
	PlanSummary string
	PlanRows    []factoryd.PlanRow
	// PlanOutcome and PlanGlossary are the spec's, PlanStill what its later
	// rounds plan and Criteria its criteria as rows with no verdict: the
	// drawer's layers under the decision, the fold holding the last three.
	PlanOutcome  string
	PlanGlossary []string
	PlanStill    []string
	// TakeOffRefs are the cards the plan takes off main, each as the
	// confirm button's line reads it: #its issue, else its key. TakeOffWhys
	// are the reasons the plan gives, one line each, beside the button.
	TakeOffRefs []string
	TakeOffWhys []string
	Questions   []planQuestion
	Holdouts    []planHoldout
	Open        string // "<n> questions open" while any waits for its answer
	Asks        bool   // the card shows its questions' forms, as the drawer does
	Confirming  bool   // the revision's confirmation waits; its Request changes is the card's
	// Confirm is the waiting confirmation itself, hoisted above the card's
	// other questions: the lead and the two forms answer it.
	Confirm    *planQuestion
	Escalation string // the plan task's escalation, such as its session cap
	Problem    string // why the human's last action on the card was refused
	// TaskStatus is its task's, where the task's card stands once the
	// request is decided
	TaskStatus string
	// Filter is the board's filter, which the card's forms keep.
	Filter   boardFilter
	CSRF     string
	Approver bool
	// Writer: the viewer writes the project, so the card's title opens
	// its task's drawer, which holds the card's facts in full
	Writer bool
	// Opened is the drawer copy of a plan card's merge card alone: the
	// plan's opened proofs, each beside the pull request's version.
	// OpenedUnread: they could not be read, and the card says so.
	Opened       []openedView
	OpenedUnread bool
	// Decide is the merge or resource face's decide view-model, built on
	// the shared decide struct and its builder (decide.go): the partial
	// case-decide renders it in the five-part shape, and its send POSTs
	// the chosen action, the fund amount and the note to the approve
	// endpoint in one request. Nil on every other kind and for a viewer
	// who may not decide. Hosted: the card renders inside its case, whose
	// own parts div already shows the ask, so the approval's copy does
	// not repeat them.
	Decide *decide
	Hosted bool
}

// openedView is an opened proof as the drawer shows it: the original
// code, and the pull request's version at the request's head when it
// differs. Missing: the pull request holds no file at the path.
type openedView struct {
	Criterion, Path, Original, Repository string
	Missing, Changed                      bool
}

// Waiting is how long the request has waited for a member, in minutes
// or hours and minutes; empty when its filing time is unknown.
func (k approvalCard) Waiting() string {
	if k.CreatedAt.IsZero() {
		return ""
	}
	return minutes(time.Since(k.CreatedAt))
}

// Rerun: the card is a factory pull request's merge request on red
// checks, whose approval offers their re-run instead of a merge (roadmap
// 264) — unless the head's checks healed (#1013), when it merges again.
func (k approvalCard) Rerun() bool {
	return k.Kind == factoryd.ApprovalMerge && k.Merge.Factory && k.Merge.RedChecks && !k.Healed
}

// Returns: the card is an agent's merge request on red checks, which cannot
// merge: it offers the return to the agent and the close, not Approve —
// unless the head's checks healed (#1013), when it merges again.
func (k approvalCard) Returns() bool {
	return k.Kind == factoryd.ApprovalMerge && !k.Merge.Factory && k.Merge.RedChecks && !k.Healed
}

// planQuestion is a plan's question as its card shows it, numbered from 1.
type planQuestion struct {
	Index int
	factoryd.PlanQuestion
	// Picks are its choices as the card shows them: masked, named by place
	Picks []planChoice
}

// planHoldout is a plan's hold-out as its card shows it: the claim with
// its inputs and expected numbers, its state's one line, and whether its
// okay and drop work (a proposal, the round's confirmation unanswered).
type planHoldout struct {
	Index int
	factoryd.PlanProof
	Blurb   string
	Decides bool
}

// holdoutBlurbs are the hold-out states' one lines.
var holdoutBlurbs = map[string]string{
	factoryd.ProofProposed: "proposed: okay keeps it out of the plan. The prover writes its test, hidden from the agent, unless it filed before the okay.",
	factoryd.ProofOkayed:   "kept out: once the plan's foundation is on main, or at once when the plan has none, the prover writes its test, hidden from the agent. A hold-out okayed after the prover files gets no test.",
	factoryd.ProofDropped:  "dropped",
}

func (c *Console) approvalCard(ctx context.Context, a factoryd.ApprovalRequest, act actor, tasks map[string]factoryd.Task, csrf string, approver, face bool) approvalCard {
	k := approvalCard{ApprovalRequest: a, CSRF: csrf, Approver: approver, Grantable: true}
	if a.MonthlyEUR != nil {
		k.Monthly = *a.MonthlyEUR
	}
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + a.Project
	t, ok := tasks[a.TaskID]
	k.TaskStatus, k.PlanName = t.Status, t.PlanName
	if ok && t.IssueNumber > 0 {
		k.IssueURL, k.Issue = fmt.Sprintf("%s/issues/%d", base, t.IssueNumber), t.IssueNumber
	}
	var err error
	switch a.Kind {
	case factoryd.ApprovalMerge:
		k.PRURL = fmt.Sprintf("%s/pulls/%d", base, a.PRNumber)
		err = json.Unmarshal(a.Spec, &k.Merge)
		// a red request whose recorded head's checks are green now (#1013)
		// merges again; a read that fails leaves the red face, which says so
		if err == nil && k.Merge.RedChecks && k.Merge.HeadSHA != "" {
			green, gerr := c.headChecksGreen(ctx, act, a.Project, k.Merge.HeadSHA)
			if gerr != nil {
				log.Printf("console: approval request %d: %v", a.ID, gerr)
				k.HeadUnread, k.HeadUnreadWhy = true, forgejoCause(gerr)
			} else {
				k.Healed = green
			}
		}
		if err == nil && !face {
			if k.Opened, err = c.openedOf(ctx, act, t, a.Project, k.Merge.HeadSHA); err != nil {
				k.OpenedUnread = true
			}
		}
		if rows, open, ok, rerr := c.planRows(ctx, t); rerr != nil {
			log.Printf("console: approval request %d: %v", a.ID, rerr)
			k.Problem = "The console could not read the criterion rows: " + errText(rerr) + ". Reload the board."
		} else if ok {
			k.Criteria, k.Unsettled = rows, open
		}
	case factoryd.ApprovalResource:
		err = json.Unmarshal(a.Spec, &k.Resource)
	case factoryd.ApprovalPlan:
		if err = c.planOf(ctx, &k); err != nil {
			k.Problem = "The plan could not be read: " + errText(err) + ". Open the plan's approval again, or read the plan on its issue."
		}
	case factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		err = c.criteriaOf(ctx, &k)
	}
	if err != nil {
		log.Printf("console: approval request %d: %v", a.ID, err)
	}
	// the board's copy whispers: every part of the ask its first line,
	// the top findings alone — the drawer carries the whole text.
	// This is the one place a merge face is assembled, so a field
	// that grows cannot grow the column's card.
	if face && k.Kind == factoryd.ApprovalMerge {
		for i, w := range k.Merge.WhyHuman {
			k.Merge.WhyHuman[i] = faceLine(w, 200)
		}
		k.Merge.VerdictSummary = faceLine(k.Merge.VerdictSummary, 300)
		if len(k.Merge.Review.Findings) > 3 {
			k.Merge.Review.Findings = k.Merge.Review.Findings[:3]
		}
		for i, f := range k.Merge.Review.Findings {
			k.Merge.Review.Findings[i] = faceLine(f, 200)
		}
		k.Merge.Recommendation = faceLine(k.Merge.Recommendation, 300)
	}
	if (k.Kind == factoryd.ApprovalMerge || k.Kind == factoryd.ApprovalResource) && k.Approver && !k.Rerun() && !k.Returns() && len(k.Unsettled) == 0 {
		k.Decide = approvalDecide(&k)
	}
	return k
}

// approvalDecide builds merge or resource approval card k's decide
// view-model on the shared decide struct and its builder (decide.go): the
// five parts of the ask — the ask with its numbers, the why, what the call
// needs, the recommendation with its source — and the answers it offers,
// whose one send POSTs the chosen action, the fund amount and the note to
// the approve endpoint in one request.
func approvalDecide(k *approvalCard) *decide {
	d := newDecide(k.TaskID, k.CSRF, k.IDSuffix, k.Filter, k.Waiting())
	d.ApprovalID = k.ID
	d.Action = fmt.Sprintf("/approvals/%d/approve", k.ID)
	d.DataTo = k.TaskStatus
	d.IssueNumber, d.IssueURL = k.Issue, k.IssueURL
	switch k.Kind {
	case factoryd.ApprovalMerge:
		migration := ""
		if k.Merge.Migration {
			migration = " It carries a migration."
		}
		d.Ask = fmt.Sprintf("Merge PR #%d of %s? %s risk.%s", k.PRNumber, k.TaskID, k.Merge.RiskClass, migration)
		d.Why = strings.Join(k.Merge.WhyHuman, "; ")
		d.Info = "Model review: " + k.Merge.VerdictSummary
		d.Recommendation, d.Source = k.Merge.Recommendation, "the model review"
		d.Offers = []offer{
			{Name: "approve", Label: "Approve", Effect: fmt.Sprintf("merges PR #%d: factoryd merges it", k.PRNumber), Recommended: true, By: "the model review"},
			{Name: "deny", Label: "Deny", Effect: "sends the pull request back to the agent with your note, or rejects it"},
		}
	case factoryd.ApprovalResource:
		d.Ask = fmt.Sprintf("Commission a %s for %s? %.2f EUR per month, this lease %.2f EUR.",
			k.Resource.ServerType, k.Resource.Purpose, k.Monthly, k.Resource.ReservationEUR)
		d.Why = strings.Join(k.Resource.BeyondEnvelope, "; ")
		d.Info = "Approves the one request of this task; a wider envelope for the project changes its file in Git."
		d.Offers = []offer{
			{Name: "approve", Label: "Approve", Effect: "commissions the server for this lease; the project's envelope is its file's", Recommended: true},
			{Name: "deny", Label: "Deny", Effect: "refuses the request; the agent is told your reason"},
		}
	}
	return d
}

// openedOf are the opened proofs of plan card t's plan when t is its
// integration card or a delta card, each read at head in project as a's
// human; nil for any other card.
func (c *Console) openedOf(ctx context.Context, a actor, t factoryd.Task, project, head string) ([]openedView, error) {
	if t.Source != factoryd.SourcePlan || t.Parent == "" || head == "" {
		return nil, nil
	}
	_, p, ok, err := c.planTable(ctx, t.Parent, 0)
	if err != nil || !ok || !factoryd.CriteriaCard(p, t.PlanKey) {
		return nil, err
	}
	rows, err := c.cfg.Tasks.PlanProofs(ctx, t.Parent)
	if err != nil {
		return nil, fmt.Errorf("proofs of %s: %w", t.Parent, err)
	}
	var out []openedView
	for _, pp := range rows {
		if pp.State != factoryd.ProofOpened {
			continue
		}
		if a.s == nil {
			return nil, errors.New("the actor carries no session, so the pull request's files were not read")
		}
		route, err := rawRoute(project, pp.Path, head)
		if err != nil {
			return nil, err
		}
		v := openedView{Criterion: cmp.Or(pp.Criterion, pp.ID), Path: pp.Path, Original: pp.Code}
		var raw []byte
		err = c.as(ctx, a.s, http.MethodGet, route, nil, http.StatusOK, &raw)
		switch ae := (*apiError)(nil); {
		case errors.As(err, &ae) && ae.status == http.StatusNotFound:
			v.Missing = true
		case err != nil:
			return nil, &forgejoRefusal{what: "read " + pp.Path + " at " + head, err: err}
		case string(raw) != pp.Code:
			v.Changed, v.Repository = true, string(raw)
		}
		out = append(out, v)
	}
	return out, nil
}

// criteriaOf fills the criteria gap card k from its request's spec: the
// plan's criteria with their standing verdicts (the approver's row over
// the review's), the open ones the grant or a waiver answers.
func (c *Console) criteriaOf(ctx context.Context, k *approvalCard) error {
	if err := json.Unmarshal(k.Spec, &k.CriteriaSpec); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, k.TaskID, k.CriteriaSpec.Revision)
	if err != nil {
		return fmt.Errorf("revision %d of %s: %w", k.CriteriaSpec.Revision, k.TaskID, err)
	}
	if !ok {
		return fmt.Errorf("revision %d of %s is not recorded", k.CriteriaSpec.Revision, k.TaskID)
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return fmt.Errorf("revision %d of %s: %w", k.CriteriaSpec.Revision, k.TaskID, err)
	}
	rows, err := c.cfg.Tasks.CriteriaVerdicts(ctx, k.CriteriaSpec.Task)
	if err != nil {
		return fmt.Errorf("verdicts of %s: %w", k.CriteriaSpec.Task, err)
	}
	if k.Delivered, _, err = c.cfg.Tasks.PlanDelivered(ctx, k.TaskID); err != nil {
		return fmt.Errorf("the delivered cards of %s: %w", k.TaskID, err)
	}
	for _, c := range p.Criteria {
		v, _ := factoryd.StandingRow(rows, c)
		k.Criteria = append(k.Criteria, criterionRow{PlanCriterion: c, Verdict: v.Verdict, Evidence: v.Evidence})
	}
	k.Grantable = slices.ContainsFunc(k.Criteria, func(c criterionRow) bool { return c.Verdict != "met" && c.Verdict != "waived" })
	return nil
}

// planRows are planVerdicts' rows as the merge card shows them: a met
// gate row's blind or open moved from its evidence to its Word, and each
// human criterion's with its artifact, cut to maxCardArtifact bytes.
func (c *Console) planRows(ctx context.Context, t factoryd.Task) (rows []criterionRow, open []string, ok bool, err error) {
	rows, open, home, ok, err := c.planVerdicts(ctx, t)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	for i, row := range rows {
		if row.Verdict == "met" && (row.Kind == factoryd.CriterionTest || row.Kind == factoryd.CriterionInvariant) {
			for _, w := range []string{"blind", "open"} {
				if rest, cut := strings.CutPrefix(row.Evidence, w+": "); cut {
					rows[i].Word, rows[i].Evidence = w, rest
				}
			}
		}
		if row.Kind != factoryd.CriterionHuman {
			continue
		}
		rows[i].Human = true
		b, err := c.cfg.Timelines.Get(ctx, factoryd.ArtifactKey(home, row.ID))
		if err != nil {
			return nil, nil, false, fmt.Errorf("the artifact of %s: %w", row.ID, err)
		}
		if b == nil {
			continue
		}
		a := &factoryd.GateArtifact{}
		if err := json.Unmarshal(b, a); err != nil {
			return nil, nil, false, fmt.Errorf("the artifact of %s: %w", row.ID, err)
		}
		if len(a.Content) > maxCardArtifact {
			rows[i].CutLine = fmt.Sprintf("the first %d KiB of %d KiB", maxCardArtifact>>10, (len(a.Content)+1023)>>10)
			a.Content = strings.ToValidUTF8(a.Content[:maxCardArtifact], "")
		}
		rows[i].Artifact = a
	}
	return rows, open, true, nil
}

// planVerdicts are plan card t's plan's criteria with their standing rows
// on its integration card home, and open, the human criteria with no
// verdict, which hold the card's Approve; ok false when t is not the
// integration card or a delta card of a plan with criteria.
func (c *Console) planVerdicts(ctx context.Context, t factoryd.Task) (rows []criterionRow, open []string, home string, ok bool, err error) {
	if t.Source != factoryd.SourcePlan || t.Parent == "" {
		return nil, nil, "", false, nil
	}
	_, p, ok, err := c.planTable(ctx, t.Parent, 0)
	if err != nil || !ok || len(p.Criteria) == 0 || !factoryd.CriteriaCard(p, t.PlanKey) {
		return nil, nil, "", false, err
	}
	integration, err := factoryd.IntegrationCard(ctx, c.cfg.Tasks, t.Parent, p)
	if err != nil {
		return nil, nil, "", false, err
	}
	verdicts, err := c.cfg.Tasks.CriteriaVerdicts(ctx, integration.ID)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("verdicts of %s: %w", integration.ID, err)
	}
	for _, pc := range p.Criteria {
		v, _ := factoryd.StandingRow(verdicts, pc)
		rows = append(rows, criterionRow{PlanCriterion: pc, Verdict: v.Verdict, Evidence: v.Evidence})
		if v.Verdict == "" && pc.Kind == factoryd.CriterionHuman {
			open = append(open, pc.ID)
		}
	}
	return rows, open, integration.ID, true, nil
}

// scrubLayers masks the model-written plan layers the drawer's copy holds
// under the decision, as the case face masks its texts.
func (k *approvalCard) scrubLayers(hosts []string) {
	k.PlanSummary = sanitize.Outbound(scrubText(k.PlanSummary), hosts)
	k.PlanOutcome = sanitize.Outbound(scrubText(k.PlanOutcome), hosts)
	for i, g := range k.PlanGlossary {
		k.PlanGlossary[i] = sanitize.Outbound(scrubText(g), hosts)
	}
	for i, s := range k.PlanStill {
		k.PlanStill[i] = sanitize.Outbound(scrubText(s), hosts)
	}
	for i, row := range k.Criteria {
		row.Claim = sanitize.Outbound(scrubText(row.Claim), hosts)
		row.Suite = sanitize.Outbound(scrubText(row.Suite), hosts)
		k.Criteria[i] = row
	}
}

// planOf fills plan card k from its request's revision.
func (c *Console) planOf(ctx context.Context, k *approvalCard) error {
	if err := json.Unmarshal(k.Spec, &k.PlanSpec); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	_, p, ok, err := c.planTable(ctx, k.TaskID, k.PlanSpec.Revision)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("revision %d of %s is not recorded", k.PlanSpec.Revision, k.TaskID)
	}
	k.PlanSummary, k.PlanRows = p.Summary, factoryd.PlanRows(p)
	k.PlanStill = p.Outline
	if p.Spec != nil {
		k.PlanOutcome, k.PlanGlossary = p.Spec.Outcome, p.Spec.Glossary
	}
	for _, c := range p.Criteria {
		k.Criteria = append(k.Criteria, criterionRow{PlanCriterion: c})
	}
	if k.TakeOffRefs, k.TakeOffWhys, err = c.takeOffOf(ctx, k.TaskID, p); err != nil {
		return err
	}
	qs, err := c.cfg.Tasks.PlanQuestions(ctx, k.TaskID, k.PlanSpec.Revision)
	if err != nil {
		return fmt.Errorf("questions of revision %d of %s: %w", k.PlanSpec.Revision, k.TaskID, err)
	}
	confirming := false
	confirmAt := -1
	open := 0
	for i, q := range qs {
		pq := planQuestion{Index: i + 1, PlanQuestion: q}
		for j, ch := range q.Choices {
			pq.Picks = append(pq.Picks, planChoice{Index: j + 1, Label: scrubText(ch)})
		}
		k.Questions = append(k.Questions, pq)
		if q.Kind == factoryd.QuestionConfirmation {
			confirming, confirmAt = !q.Answered(), i
		}
		if !q.Answered() {
			open++
		}
	}
	k.Confirming = confirming
	if confirming {
		k.Confirm = &k.Questions[confirmAt]
	}
	if open > 0 {
		k.Open = fmt.Sprintf("%d %s open", open, plural(open, "question"))
	}
	proofs, err := c.cfg.Tasks.PlanProofs(ctx, k.TaskID)
	if err != nil {
		return fmt.Errorf("hold-outs of %s: %w", k.TaskID, err)
	}
	i := 0
	for _, ho := range proofs {
		if ho.Origin != factoryd.ProofOriginHoldout || ho.Round != k.PlanSpec.Revision {
			continue
		}
		i++
		k.Holdouts = append(k.Holdouts, planHoldout{Index: i, PlanProof: ho, Blurb: holdoutBlurbs[ho.State],
			Decides: confirming && ho.State == factoryd.ProofProposed})
	}
	return nil
}

// takeOffOf are the cards plan p of task id takes off main, each as the
// confirm button's line reads it (#its issue, else its key), and the
// reason each gave.
func (c *Console) takeOffOf(ctx context.Context, id string, p factoryd.Plan) (refs, whys []string, err error) {
	if len(p.TakeOffMain) == 0 {
		return nil, nil, nil
	}
	cards, err := c.cfg.Tasks.PlanCards(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("the cards of plan %s: %w", id, err)
	}
	issue := map[string]int64{}
	for _, t := range cards {
		issue[t.PlanKey] = t.IssueNumber
	}
	for _, key := range p.TakeOffMain {
		ref := key
		if n := issue[key]; n > 0 {
			ref = fmt.Sprintf("#%d", n)
		}
		refs = append(refs, ref)
		if why := p.TakeOffReason(key); why != "" {
			whys = append(whys, ref+": "+why)
		}
	}
	return refs, whys, nil
}

// planTable is revision n (0 the newest) of plan task id's plan: the
// revision and its plan; false when there is none.
func (c *Console) planTable(ctx context.Context, id string, n int) (factoryd.PlanRevision, factoryd.Plan, bool, error) {
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, id, n)
	if err != nil || !ok {
		return rev, factoryd.Plan{}, false, err
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return rev, factoryd.Plan{}, false, fmt.Errorf("revision %d of %s: %w", rev.Revision, id, err)
	}
	return rev, p, true, nil
}

// approve decides the approval request the path names: the chosen action,
// the fund amount and the note ride one POST (doApprove, doDeny).
func (c *Console) approve(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		var fund float64
		if v := strings.TrimSpace(r.PostFormValue("fund_eur")); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || !(f > 0) || math.IsInf(f, 0) {
				return "", refuse(http.StatusBadRequest, "fund_eur is an amount in EUR above zero")
			}
			fund = f
		}
		note := r.PostFormValue("note")
		switch r.PostFormValue("action") {
		case "deny", "reject", "request_changes":
			return c.doDeny(ctx, a, id, r.PostFormValue("action"), note)
		}
		return c.doApprove(ctx, a, id, note, fund)
	})
}

// deny denies the approval request the path names with the human's
// reason (doDeny).
func (c *Console) deny(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		return c.doDeny(ctx, a, id, r.PostFormValue("action"), r.PostFormValue("note"))
	})
}

// decideCriterion answers POST /approvals/{id}/criterion: doDecide with
// the form's criterion, verb and reason, through decideButton.
func (c *Console) decideCriterion(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		return c.doDecide(ctx, a, id, r.PostFormValue("criterion"), r.PostFormValue("verb"), r.PostFormValue("reason"))
	})
}

// confirm records the member's confirmation that the plan's delivered
// cards meet its criteria (doConfirmCriteria).
func (c *Console) confirm(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		return c.doConfirmCriteria(ctx, a, id, "")
	})
}

// rerun runs the red checks of the factory pull request card whose merge
// request the path names again (doRerun).
func (c *Console) rerun(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, c.doRerun)
}

// decideButton runs a button's decision on the approval request the path
// names and answers the press.
func (c *Console) decideButton(w http.ResponseWriter, r *http.Request, decision func(ctx context.Context, a actor, id int64) (string, error)) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, err = decision(r.Context(), button(r), id)
	c.pressed(w, r, err)
}

// review submits a pull request review on q's pull request as its decider
// (posterOf), so the agent's decision is never the human's review.
func (c *Console) review(ctx context.Context, a actor, q factoryd.ApprovalRequest, event, body, commit string) error {
	repo, err := factoryd.SplitRepo(q.Project)
	if err != nil {
		return err
	}
	if a, err = c.posterOf(ctx, a, q.Project); err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), q.PRNumber)
	return c.as(ctx, a.s, http.MethodPost, path, map[string]string{"event": event, "body": body, "commit_id": commit}, http.StatusOK, nil)
}

// headChecksGreen reads whether commit sha's checks are green in project
// now, as a's member: a merge request opened on red checks heals once the
// same head's checks turn green (#1013: a re-run) and may merge then. An
// actor that carries no session — the pulse's render, which no member
// asked for — fails closed as unread instead of following the nil
// session: a member session is not required to paint a card. Green
// mirrors factoryd's checksResult: a head that reports no statuses
// is still pending, and warning and skipped count as green.
func (c *Console) headChecksGreen(ctx context.Context, a actor, project, sha string) (bool, error) {
	if a.s == nil {
		return false, &forgejoRefusal{what: "read the head's checks", err: errors.New("the actor carries no session, so the head's current checks were not read")}
	}
	repo, err := factoryd.SplitRepo(project)
	if err != nil {
		return false, err
	}
	var cs factoryd.CombinedStatus
	path := fmt.Sprintf("/api/v1/repos/%s/%s/commits/%s/status", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(sha))
	if err := c.as(ctx, a.s, http.MethodGet, path, nil, http.StatusOK, &cs); err != nil {
		return false, &forgejoRefusal{what: "read the head's checks", err: err}
	}
	return len(cs.Statuses) > 0 && (cs.State == "success" || cs.State == "warning" || cs.State == "skipped"), nil
}
