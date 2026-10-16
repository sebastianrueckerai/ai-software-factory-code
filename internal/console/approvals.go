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
	// a plan: its revision, the summary and the cards in filing order
	PlanSpec    factoryd.PlanSpec
	PlanSummary string
	PlanRows    []factoryd.PlanRow
	Questions   []planQuestion
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

func (c *Console) approvalCard(ctx context.Context, a factoryd.ApprovalRequest, tasks map[string]factoryd.Task, csrf string, approver bool) approvalCard {
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
	}
	if err != nil {
		log.Printf("console: approval request %d: %v", a.ID, err)
	}
	return k
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
	open := 0
	for i, q := range qs {
		k.Questions = append(k.Questions, planQuestion{Index: i + 1, PlanQuestion: q})
		if q.Answer == "" {
			open++
		}
	}
	if open > 0 {
		k.Open = fmt.Sprintf("%d %s open", open, plural(open, "question"))
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
