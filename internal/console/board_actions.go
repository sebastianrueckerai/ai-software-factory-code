package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// answerRequest answers approval request q's case with res through Act.
func (c *Console) answerRequest(ctx context.Context, a actor, q factoryd.ApprovalRequest, res factoryd.Resolution) (string, error) {
	t, _, err := c.cfg.Tasks.Task(ctx, q.TaskID)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the task")
	}
	t.ID, t.Repository = q.TaskID, cmpOr(t.Repository, q.Project)
	open, err := c.cfg.OpenCasesOf(ctx, t.ID)
	if err != nil {
		return "", refuse(http.StatusServiceUnavailable, "The card's cases could not be read, so nothing was answered. Try again.")
	}
	i := slices.IndexFunc(open, func(k factoryd.Case) bool { return approvalOf(q, k) })
	if i < 0 {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Approval request %d has no live case yet: reload the board.", q.ID))
	}
	return c.boardAct(ctx, a, t, open[i], res)
}

// boardAct answers case k of card t with board action res through Act,
// which has the work done under the case's lock once every refusal has
// passed; what the work says is the answer, a failure of it masked.
func (c *Console) boardAct(ctx context.Context, a actor, t factoryd.Task, k factoryd.Case, res factoryd.Resolution) (string, error) {
	var say string
	out, err := c.actOnWork(ctx, a, t, k.ID, res, nil, func(ctx context.Context) (int64, error) {
		var err error
		say, err = c.boardWork(ctx, a, t, k, res)
		return 0, maskedErr(err)
	})
	if err != nil {
		return "", err
	}
	return cmpOr(say, out.Text), nil
}

// maskedErr is err with the secrets of its words masked unless it is a
// refusal the console wrote, whose words are the human's already.
func maskedErr(err error) error {
	var no *refusal
	var fj *forgejoRefusal
	if err == nil || errors.As(err, &no) || errors.As(err, &fj) {
		return err
	}
	return &maskedError{text: errText(err), err: err}
}

type maskedError struct {
	text string
	err  error
}

func (e *maskedError) Error() string { return e.text }
func (e *maskedError) Unwrap() error { return e.err }

// approvalIDOf is the approval request case k names, 0 for none.
func approvalIDOf(k factoryd.Case) int64 {
	b, err := json.Marshal(k.Facts["approval"])
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseInt(string(b), 10, 64)
	return id
}

func (c *Console) boardWork(ctx context.Context, a actor, t factoryd.Task, k factoryd.Case, res factoryd.Resolution) (string, error) {
	arg := func(key string) string { s, _ := res.Args[key].(string); return s }
	switch res.Action {
	case factoryd.ActApproveMerge, factoryd.ActDenyMerge, factoryd.ActAnswerCriteria:
		q, err := c.pendingRequest(ctx, a, approvalIDOf(k))
		if err != nil {
			return "", err
		}
		switch res.Action {
		case factoryd.ActApproveMerge:
			return c.approveMerge(ctx, a, q)
		case factoryd.ActDenyMerge:
			return c.denyMerge(ctx, a, q, arg("action"), res.Note)
		}
		verb := arg("verdict")
		for from, to := range criteriaVerdicts {
			if to == verb {
				verb = from
			}
		}
		return c.criterionVerdict(ctx, a, q, arg("criterion"), verb, res.Note)
	case factoryd.ActSetDependency, factoryd.ActRemoveDependency:
		return c.setDependency(ctx, a, cmpOr(arg("task_id"), t.ID), arg("depends_on"), res.Action == factoryd.ActRemoveDependency)
	case factoryd.ActFileCard:
		return c.fileFromCase(ctx, a, t, k, arg("title"), arg("body"), arg("size"))
	case factoryd.ActRestartPlan:
		return c.restartPlan(ctx, a, t, res.Note)
	}
	return "", refuse(http.StatusBadRequest, "there is no board action "+res.Action)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fileFromCase files the card of a root cause outside card t, whose case k
// it names in the body.
func (c *Console) fileFromCase(ctx context.Context, a actor, t factoryd.Task, k factoryd.Case, title, body, size string) (string, error) {
	from := "Filed from " + issueRef(t.ID, t.IssueNumber)
	if k.ID != 0 {
		from += fmt.Sprintf(", case %d (%s)", k.ID, k.Kind)
	}
	repo, issue, err := c.doFileCard(ctx, a, c.projectName(ctx, t.Repository), title, strings.TrimSpace(body+"\n\n"+from+"."),
		policy.TrackDirect, "", "", size, "", "", "", 0, 0, 0)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Filed %s#%d.", repo, issue), nil
}

// restartPlan ends the plan of card t as its Close does and files its
// objective anew as a plan, with note the shape to take.
func (c *Console) restartPlan(ctx context.Context, a actor, t factoryd.Task, note string) (string, error) {
	plan := t
	if factoryd.InPlan(t) && t.Track != policy.TrackPlan {
		var err error
		if plan, err = c.cardIn(ctx, a, factoryd.GroupOf(t)); err != nil {
			return "", err
		}
	}
	closed, err := c.closePlan(ctx, a, plan, note)
	if err != nil {
		return "", err
	}
	title, body, _ := strings.Cut(plan.Objective, "\n\n")
	repo, issue, err := c.doFileCard(ctx, a, c.projectName(ctx, plan.Repository), title,
		strings.TrimSpace(body+"\n\nThis restarts plan "+issueRef(plan.ID, plan.IssueNumber)+", which was closed. The shape to take:\n\n"+note),
		policy.TrackPlan, "", "", "", "", "", "", 0, 0, 0)
	if err != nil {
		return closed + " The new plan could not be filed: " + strings.TrimSuffix(errText(err), ".") + ". File it again with the shape in your note.", nil
	}
	return fmt.Sprintf("%s Filed %s#%d as the new plan.", closed, repo, issue), nil
}
