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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// approvalCard is a pending approval request with what its kind needs.
type approvalCard struct {
	factoryd.ApprovalRequest
	IssueURL string
	Issue    int64
	PRURL    string
	Monthly  float64
	Merge    factoryd.MergeSpec
	Resource cloudres.ResourceSpec
	// a plan: its revision, the spec's first lines, the summary and the
	// cards in filing order
	PlanSpec    factoryd.PlanSpec
	SpecLines   string
	PlanSummary string
	PlanRows    []factoryd.PlanRow
	Escalation  string // the plan task's escalation, such as its session cap
	Problem     string // why the human's last action on the card was refused
	// Filter is the board's filter, which the card's forms keep.
	Filter   boardFilter
	CSRF     string
	Approver bool
}

// specLines are the plan card's first lines of a spec.
const specLines = 5

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
		if lines := strings.Split(t.Objective, "\n"); len(lines) > specLines {
			k.SpecLines = strings.Join(append(lines[:specLines], "…"), "\n")
		} else {
			k.SpecLines = t.Objective
		}
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
	summary, rows, ok, err := c.planTable(ctx, k.TaskID, k.PlanSpec.Revision)
	if err != nil || !ok {
		return fmt.Errorf("revision %d of %s: found %v, %v", k.PlanSpec.Revision, k.TaskID, ok, err)
	}
	k.PlanSummary, k.PlanRows = summary, rows
	return nil
}

// planTable is revision n (0 the newest) of plan task id's plan: its
// summary and its cards in filing order; false when there is none.
func (c *Console) planTable(ctx context.Context, id string, n int) (string, []factoryd.PlanRow, bool, error) {
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, id, n)
	if err != nil || !ok {
		return "", nil, false, err
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return "", nil, false, fmt.Errorf("revision %d of %s: %w", rev.Revision, id, err)
	}
	return p.Summary, factoryd.PlanRows(p), true, nil
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
