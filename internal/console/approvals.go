package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// resourceSpec is what a resource request asks for (cloudres.Reserve).
type resourceSpec struct {
	ServerType     string   `json:"server_type"`
	Purpose        string   `json:"purpose"`
	LeaseMinutes   int      `json:"lease_minutes"`
	VolumeGB       int      `json:"volume_gb"`
	Location       string   `json:"location"`
	HourlyEUR      float64  `json:"hourly_eur"`
	ReservationEUR float64  `json:"reservation_eur"`
	BeyondEnvelope []string `json:"beyond_envelope"`
}

// approvalCard is a pending approval request with what its kind needs.
type approvalCard struct {
	factoryd.ApprovalRequest
	IssueURL string
	PRURL    string
	Monthly  float64
	Merge    factoryd.MergeSpec
	Resource resourceSpec
	CSRF     string
}

func (c *Console) approvalCard(a factoryd.ApprovalRequest, tasks map[string]factoryd.Task, csrf string) approvalCard {
	k := approvalCard{ApprovalRequest: a, CSRF: csrf}
	if a.MonthlyEUR != nil {
		k.Monthly = *a.MonthlyEUR
	}
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + a.Project
	if t, ok := tasks[a.TaskID]; ok && t.IssueNumber > 0 {
		k.IssueURL = fmt.Sprintf("%s/issues/%d", base, t.IssueNumber)
	}
	var err error
	switch a.Kind {
	case "merge":
		k.PRURL = fmt.Sprintf("%s/pulls/%d", base, a.PRNumber)
		err = json.Unmarshal(a.Spec, &k.Merge)
	case "resource":
		err = json.Unmarshal(a.Spec, &k.Resource)
	}
	if err != nil {
		log.Printf("console: approval request %d: spec: %v", a.ID, err)
	}
	return k
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
	case errors.Is(err, factoryd.ErrNotFound) && a.Kind == "merge":
		log.Printf("console: approval request %d was decided by the factory before %s's %s was recorded", a.ID, current(r).user, status)
	case err != nil:
		log.Printf("console: decide approval request %d: %v", a.ID, err)
		http.Error(w, "cannot record the decision: "+err.Error(), http.StatusConflict)
		return false
	}
	log.Printf("console: %s %s %s request %d of %s: %s", current(r).user, status, a.Kind, a.ID, a.TaskID, note)
	return true
}

// approve: a resource request is approved for its task (one commission of
// that spec) or for its project (the envelope widens to fit it); a merge
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
	case "resource":
		switch r.PostFormValue("scope") {
		case "task":
			if !c.decided(w, r, a, "approved", "approved for the task") {
				return
			}
		case "project":
			env, err := c.cfg.Resources.ApproveForProject(r.Context(), a.ID, user)
			if err != nil {
				log.Printf("console: approve request %d for its project: %v", a.ID, err)
				http.Error(w, "cannot approve for the project: "+err.Error(), http.StatusConflict)
				return
			}
			log.Printf("console: %s approved resource request %d for %s: envelope now %+v", user, a.ID, a.Project, env)
		default:
			http.Error(w, "choose the task or the project", http.StatusBadRequest)
			return
		}
	case "merge":
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
	case "resource":
		if !c.decided(w, r, a, "denied", note) {
			return
		}
	case "merge":
		repo, err := factoryd.SplitRepo(a.Project)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
