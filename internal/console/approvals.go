package console

import (
	"context"
	"encoding/json"
	"errors"
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
	// FilterProject and FilterAccent are the board's filter, which the
	// card's forms keep.
	FilterProject, FilterAccent string
	CSRF                        string
	Approver                    bool
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
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, k.TaskID, k.PlanSpec.Revision)
	if err != nil || !ok {
		return fmt.Errorf("revision %d of %s: found %v, %v", k.PlanSpec.Revision, k.TaskID, ok, err)
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return fmt.Errorf("revision %d of %s: %w", rev.Revision, k.TaskID, err)
	}
	k.PlanSummary, k.PlanRows = p.Summary, factoryd.PlanRows(p)
	return nil
}

// pending finds the pending approval request the path names.
func (c *Console) pending(w http.ResponseWriter, r *http.Request) (factoryd.ApprovalRequest, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return factoryd.ApprovalRequest{}, false
	}
	all, err := c.cfg.Approvals.ApprovalRequests(r.Context(), "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		http.Error(w, "cannot read the approval requests", http.StatusInternalServerError)
		return factoryd.ApprovalRequest{}, false
	}
	for _, a := range all {
		if a.ID == id {
			return a, true
		}
	}
	http.Error(w, fmt.Sprintf("approval request %d is not pending (any more); reload the board", id), http.StatusConflict)
	return factoryd.ApprovalRequest{}, false
}

// decided records the human's decision on a; a request the factory
// decided meanwhile (a new attempt superseded it) is logged, not an error:
// Forgejo already carries the human's action.
func (c *Console) decided(w http.ResponseWriter, r *http.Request, a factoryd.ApprovalRequest, status, note string) bool {
	err := c.cfg.Approvals.DecideApproval(r.Context(), a.ID, a.Kind, status, current(r).user, note)
	switch {
	case errors.Is(err, factoryd.ErrNotFound) && a.Kind == factoryd.ApprovalMerge:
		log.Printf("console: approval request %d was decided by the factory before %s's %s was recorded", a.ID, current(r).user, status)
	case err != nil:
		log.Printf("console: decide approval request %d: %v", a.ID, err)
		http.Error(w, "cannot record the decision; see the factoryd log", http.StatusInternalServerError)
		return false
	}
	log.Printf("console: %s %s %s request %d of %s: %s", current(r).user, status, a.Kind, a.ID, a.TaskID, note)
	return true
}

// approve: a resource request is approved for its task (one commission of
// that spec; the project's envelope is its file's, ruling 183); a merge
// request gets the human's APPROVED review on the pull request, and only
// once Forgejo has it is the workflow signalled, which merges as
// factory-merge.
func (c *Console) approve(w http.ResponseWriter, r *http.Request) {
	a, ok := c.pending(w, r)
	if !ok {
		return
	}
	user := current(r).user
	switch a.Kind {
	case factoryd.ApprovalResource:
		if !c.decided(w, r, a, "approved", "approved for the task") {
			return
		}
		c.act(r, a.Project, a.TaskID, 0, factoryd.EventMergeApproved, map[string]any{"approval": a.ID, "kind": "resource"})
	case factoryd.ApprovalMerge:
		var spec factoryd.MergeSpec
		if err := json.Unmarshal(a.Spec, &spec); err != nil || spec.HeadSHA == "" {
			http.Error(w, "the merge request names no head commit", http.StatusInternalServerError)
			return
		}
		if err := c.review(r, a, "APPROVED", "Approved in the factory console.", spec.HeadSHA); err != nil {
			c.forgejoFailed(w, r, "submit the approving review", err)
			return
		}
		if err := c.cfg.ApproveMerge(r.Context(), a.TaskID, a.PRNumber, user); err != nil {
			log.Printf("console: %s approved PR #%d of %s, the signal failed: %v", user, a.PRNumber, a.TaskID, err)
			http.Error(w, "your approving review is on the pull request, but the task's workflow could not be signalled; approve again",
				http.StatusBadGateway)
			return
		}
		log.Printf("console: %s approved PR #%d of %s at %s", user, a.PRNumber, a.TaskID, spec.HeadSHA)
		c.act(r, a.Project, a.TaskID, 0, factoryd.EventMergeApproved, map[string]any{"pr": a.PRNumber})
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// deny needs the human's reason. A resource request is denied (the agent
// is told the reason). A merge request gets a REQUEST_CHANGES review as the
// human, which sends the task back to its agent (changes_requested), or is
// rejected: the reason is commented and the pull request closed as the
// human, which fails the task.
func (c *Console) deny(w http.ResponseWriter, r *http.Request) {
	note := strings.TrimSpace(r.PostFormValue("note"))
	if note == "" || len(note) > 64<<10 {
		http.Error(w, "a reason of up to 64 KiB is required", http.StatusBadRequest)
		return
	}
	a, ok := c.pending(w, r)
	if !ok {
		return
	}
	switch a.Kind {
	case factoryd.ApprovalResource:
		if !c.decided(w, r, a, "denied", note) {
			return
		}
		c.act(r, a.Project, a.TaskID, 0, factoryd.EventMergeDenied, map[string]any{"approval": a.ID, "kind": "resource"})
	case factoryd.ApprovalMerge:
		repo, err := factoryd.SplitRepo(a.Project)
		if err != nil {
			log.Printf("console: approval request %d: %v", a.ID, err)
			http.Error(w, "the request names no repository", http.StatusInternalServerError)
			return
		}
		switch r.PostFormValue("action") {
		case "request_changes":
			var spec factoryd.MergeSpec
			if err := json.Unmarshal(a.Spec, &spec); err != nil {
				http.Error(w, "cannot read the merge request", http.StatusInternalServerError)
				return
			}
			if err := c.review(r, a, "REQUEST_CHANGES", note, spec.HeadSHA); err != nil {
				c.forgejoFailed(w, r, "submit the change request", err)
				return
			}
			if !c.decided(w, r, a, "denied", "changes requested: "+note) {
				return
			}
			c.act(r, a.Project, a.TaskID, 0, factoryd.EventMergeDenied, map[string]any{"pr": a.PRNumber, "action": "request_changes"})
		case "reject":
			pr := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
			if err := c.asUser(r, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", pr, a.PRNumber),
				map[string]string{"body": "Rejected in the factory console: " + note}, http.StatusCreated, nil); err != nil {
				c.forgejoFailed(w, r, "comment the rejection", err)
				return
			}
			if err := c.asUser(r, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", pr, a.PRNumber),
				map[string]string{"state": "closed"}, http.StatusCreated, nil); err != nil {
				c.forgejoFailed(w, r, "close the pull request", err)
				return
			}
			if !c.decided(w, r, a, "denied", "rejected: "+note) {
				return
			}
			c.act(r, a.Project, a.TaskID, 0, factoryd.EventMergeDenied, map[string]any{"pr": a.PRNumber, "action": "reject"})
		default:
			http.Error(w, "choose request changes or reject", http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// review submits a pull request review on a's pull request as the human.
func (c *Console) review(r *http.Request, a factoryd.ApprovalRequest, event, body, commit string) error {
	repo, err := factoryd.SplitRepo(a.Project)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), a.PRNumber)
	return c.asUser(r, http.MethodPost, path, map[string]string{"event": event, "body": body, "commit_id": commit}, http.StatusOK, nil)
}
