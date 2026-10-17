package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// criterionRow is one criterion of a criteria approval card: the
// criterion, its standing verdict and the evidence behind it.
type criterionRow struct {
	factoryd.PlanCriterion
	Verdict  string
	Evidence string
}

// approvalCard is a pending approval request with what its kind needs.
type approvalCard struct {
	factoryd.ApprovalRequest
	IDSuffix string // on the item's ids: "-d" in the drawer, so the column's copy keeps its own
	IssueURL string
	Issue    int64
	PRURL    string
	Monthly  float64
	Merge    factoryd.MergeSpec
	Resource cloudres.ResourceSpec
	// a plan's criteria gap: the spec, and the open criteria with their
	// standing verdicts
	CriteriaSpec factoryd.CriteriaSpec
	Criteria     []criterionRow
	// a plan: its revision, the summary and the cards in filing order
	PlanSpec    factoryd.PlanSpec
	PlanSummary string
	PlanRows    []factoryd.PlanRow
	Questions   []planQuestion
	Holdouts    []planHoldout
	Open        string // "<n> questions open" while any waits for its answer
	Escalation  string // the plan task's escalation, such as its session cap
	Problem     string // why the human's last action on the card was refused
	// Filter is the board's filter, which the card's forms keep.
	Filter   boardFilter
	CSRF     string
	Approver bool
	// Writer: the viewer writes the project, so the card's title opens
	// its task's drawer, which holds the card's facts in full
	Writer bool
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
// 264).
func (k approvalCard) Rerun() bool {
	return k.Kind == factoryd.ApprovalMerge && k.Merge.Factory && k.Merge.RedChecks
}

// Returns: the card is an agent's merge request on red checks, which cannot
// merge: it offers the return to the agent and the close, not Approve.
func (k approvalCard) Returns() bool {
	return k.Kind == factoryd.ApprovalMerge && !k.Merge.Factory && k.Merge.RedChecks
}

// planQuestion is a plan's question as its card shows it, numbered from 1.
type planQuestion struct {
	Index int
	factoryd.PlanQuestion
}

// planHoldout is a plan's hold-out criterion as its card shows it: the
// criterion, its state's one line, and whether its okay and drop work
// (the round's confirmation unanswered; an unrunnable draft takes no
// okay).
type planHoldout struct {
	Index int
	factoryd.Holdout
	Blurb   string
	CanOkay bool
	CanDrop bool
}

// holdoutBlurbs are the hold-out states' one lines.
var holdoutBlurbs = map[string]string{
	factoryd.HoldoutProposed:   "drafted; okay keeps it out of the plan and runs it at the gate, hidden from the agent",
	factoryd.HoldoutStored:     "kept out; it runs at the gate, hidden from the agent",
	factoryd.HoldoutUnrunnable: "its draft did not run; drop discards it",
	factoryd.HoldoutDropped:    "dropped",
	factoryd.HoldoutOpened:     "published: the audit found a real gap; the builder works it",
}

func (c *Console) approvalCard(ctx context.Context, a factoryd.ApprovalRequest, tasks map[string]factoryd.Task, csrf string, approver, face bool) approvalCard {
	k := approvalCard{ApprovalRequest: a, CSRF: csrf, Approver: approver}
	if a.MonthlyEUR != nil {
		k.Monthly = *a.MonthlyEUR
	}
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + a.Project
	t, ok := tasks[a.TaskID]
	if ok && t.IssueNumber > 0 {
		k.IssueURL, k.Issue = fmt.Sprintf("%s/issues/%d", base, t.IssueNumber), t.IssueNumber
	}
	var err error
	switch a.Kind {
	case factoryd.ApprovalMerge:
		k.PRURL = fmt.Sprintf("%s/pulls/%d", base, a.PRNumber)
		err = json.Unmarshal(a.Spec, &k.Merge)
	case factoryd.ApprovalResource:
		err = json.Unmarshal(a.Spec, &k.Resource)
	case factoryd.ApprovalPlan:
		if err = c.planOf(ctx, &k); err != nil {
			k.Problem = "the plan could not be read; see the factoryd log"
		}
	case factoryd.ApprovalCriteria:
		err = c.criteriaOf(ctx, &k)
	}
	if err != nil {
		log.Printf("console: approval request %d: %v", a.ID, err)
	}
	// the board's copy whispers: every part of the ask its first line,
	// the top findings alone — the drawer carries the whole text (ruling
	// 295). This is the one place a merge face is assembled, so a field
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
		for i := range k.Merge.Review.Criteria {
			k.Merge.Review.Criteria[i].Evidence = faceLine(k.Merge.Review.Criteria[i].Evidence, 200)
		}
		k.Merge.Recommendation = faceLine(k.Merge.Recommendation, 300)
	}
	return k
}

// criteriaOf fills the criteria gap card k from its request's spec: the
// plan's criteria with their standing verdicts (the approver's row over
// the review's), the open ones the grant or a waiver answers.
func (c *Console) criteriaOf(ctx context.Context, k *approvalCard) error {
	if err := json.Unmarshal(k.Spec, &k.CriteriaSpec); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, k.TaskID, k.CriteriaSpec.Revision)
	if err != nil || !ok {
		return fmt.Errorf("revision %d of %s: found %v, %v", k.CriteriaSpec.Revision, k.TaskID, ok, err)
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return fmt.Errorf("revision %d of %s: %w", k.CriteriaSpec.Revision, k.TaskID, err)
	}
	rows, err := c.cfg.Tasks.CriteriaVerdicts(ctx, k.CriteriaSpec.Task)
	if err != nil {
		return fmt.Errorf("verdicts of %s: %w", k.CriteriaSpec.Task, err)
	}
	for _, c := range p.Criteria {
		verdict, evidence := "", ""
		for _, v := range rows {
			if v.Criterion == c.ID && (v.Source == "approver" || verdict == "") {
				verdict, evidence = v.Verdict, v.Evidence
				if v.Source == "approver" {
					break
				}
			}
		}
		k.Criteria = append(k.Criteria, criterionRow{PlanCriterion: c, Verdict: verdict, Evidence: evidence})
	}
	return nil
}

// planOf fills plan card k from its request's revision.
func (c *Console) planOf(ctx context.Context, k *approvalCard) error {
	if err := json.Unmarshal(k.Spec, &k.PlanSpec); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	_, summary, rows, ok, err := c.planTable(ctx, k.TaskID, k.PlanSpec.Revision)
	if err != nil || !ok {
		return fmt.Errorf("revision %d of %s: found %v, %v", k.PlanSpec.Revision, k.TaskID, ok, err)
	}
	k.PlanSummary, k.PlanRows = summary, rows
	qs, err := c.cfg.Tasks.PlanQuestions(ctx, k.TaskID, k.PlanSpec.Revision)
	if err != nil {
		return fmt.Errorf("questions of revision %d of %s: %w", k.PlanSpec.Revision, k.TaskID, err)
	}
	confirming := false
	open := 0
	for i, q := range qs {
		k.Questions = append(k.Questions, planQuestion{Index: i + 1, PlanQuestion: q})
		if q.Answer == "" {
			open++
		}
		if q.Kind == factoryd.QuestionConfirmation {
			confirming = q.Answer == ""
		}
	}
	if open > 0 {
		k.Open = fmt.Sprintf("%d %s open", open, plural(open, "question"))
	}
	hos, err := c.cfg.Tasks.PlanHoldouts(ctx, k.TaskID)
	if err != nil {
		return fmt.Errorf("hold-outs of %s: %w", k.TaskID, err)
	}
	i := 0
	for _, ho := range hos {
		if ho.Round != k.PlanSpec.Revision {
			continue
		}
		i++
		k.Holdouts = append(k.Holdouts, planHoldout{Index: i, Holdout: ho,
			Blurb:   holdoutBlurbs[ho.State],
			CanOkay: confirming && ho.State == factoryd.HoldoutProposed,
			CanDrop: confirming && (ho.State == factoryd.HoldoutProposed || ho.State == factoryd.HoldoutUnrunnable)})
	}
	return nil
}

// planTable is revision n (0 the newest) of plan task id's plan: the
// revision, its summary and its cards in filing order; false when there
// is none.
func (c *Console) planTable(ctx context.Context, id string, n int) (factoryd.PlanRevision, string, []factoryd.PlanRow, bool, error) {
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, id, n)
	if err != nil || !ok {
		return rev, "", nil, false, err
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return rev, "", nil, false, fmt.Errorf("revision %d of %s: %w", rev.Revision, id, err)
	}
	return rev, p.Summary, factoryd.PlanRows(p), true, nil
}

// approve approves the approval request the path names (doApprove).
func (c *Console) approve(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) error {
		_, err := c.doApprove(ctx, a, id)
		return err
	})
}

// deny denies the approval request the path names with the human's
// reason (doDeny).
func (c *Console) deny(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) error {
		_, err := c.doDeny(ctx, a, id, r.PostFormValue("action"), r.PostFormValue("note"))
		return err
	})
}

// waive records the member's waiver of one criterion of the criteria
// request the path names, with their one-line reason (doWaive).
func (c *Console) waive(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) error {
		_, err := c.doWaive(ctx, a, id, r.PostFormValue("criterion"), r.PostFormValue("reason"))
		return err
	})
}

// rerun runs the red checks of the factory pull request card whose merge
// request the path names again (doRerun).
func (c *Console) rerun(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) error {
		_, err := c.doRerun(ctx, a, id)
		return err
	})
}

// decideButton runs a button's decision on the approval request the path
// names and goes back to the board the form carried.
func (c *Console) decideButton(w http.ResponseWriter, r *http.Request, decision func(ctx context.Context, a actor, id int64) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := decision(r.Context(), button(r), id); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
}

// review submits a pull request review on q's pull request as a's member.
func (c *Console) review(ctx context.Context, a actor, q factoryd.ApprovalRequest, event, body, commit string) error {
	repo, err := factoryd.SplitRepo(q.Project)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), q.PRNumber)
	return c.as(ctx, a.s, http.MethodPost, path, map[string]string{"event": event, "body": body, "commit_id": commit}, http.StatusOK, nil)
}
