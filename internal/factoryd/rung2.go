package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// RungTwoStatement is what the factory agent's wake receives for case id:
// the case's statement for rung 2, built by code.
func (r *Runner) RungTwoStatement(ctx context.Context, id int64) (text string, err error) {
	defer func() { err = r.scrubbed(err) }()
	c, err := r.store.CaseByID(ctx, id)
	if err != nil {
		return "", err
	}
	t, _, err := r.task(ctx, c.TaskID)
	if err != nil {
		return "", err
	}
	head, _ := c.Facts["head"].(string)
	model, _ := c.Facts["model"].(string)
	f, err := r.tripFacts(ctx, t, head, model)
	if err != nil {
		return "", err
	}
	f.atCase(c)
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return "", err
	}
	steps = slices.DeleteFunc(steps, func(s CaseStep) bool { return s.Rung == 2 && s.Outcome == StepInProgress })
	d, derr := r.CardDigest(ctx, t.ID)
	var choices []QuestionChoice
	if answersByChoices(c, t.Kind) {
		ask, _ := r.caseAsk(ctx, c)
		if choices = askOffers(ask.Detail); choices == nil {
			choices = []QuestionChoice{}
		}
	}
	return r.statementOf(c, f, DigestText(r.Scrub, d, derr), steps, 2, choices), nil
}

// BoardText is the project's board digest as the factory agent's wake
// reads it, masked.
func (r *Runner) BoardText(ctx context.Context, repository string) (string, error) {
	d, err := r.BoardDigest(ctx, repository)
	if err != nil {
		return "", r.scrubbed(err)
	}
	return r.scrub(d.Render()), nil
}

// scrubbed is err with its text masked, nil for nil.
func (r *Runner) scrubbed(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(r.scrub(err.Error()))
}

// RungTwoRecommendation is the action rung 2 recommends to the human, with
// its arguments and the words that ground it.
type RungTwoRecommendation struct {
	Action, Grounds string
	Args            map[string]any
}

// HandUpRung2 ends rung 2's step on a case of card taskID handed up, with
// found as its finding, the recommendation as its decision and grounds and
// the turn's reads, and climbs: the human is asked once, with the
// recommendation. The case is caseID, or with 0 the card's open case rung 2
// holds. It answers the case it handed up, 0 when rung 2 holds no case of
// the card, and the ask stays as it was. A case of another card is refused
// with the card's open cases.
func (r *Runner) HandUpRung2(ctx context.Context, taskID string, caseID int64, found string, rec RungTwoRecommendation, reads []CaseRead) (handled int64, err error) {
	var c Case
	var own CaseStep
	open, err := r.store.OpenCasesOf(ctx, taskID)
	if err != nil {
		return 0, err
	}
	if caseID != 0 {
		if c, err = r.store.CaseByID(ctx, caseID); err != nil {
			return 0, err
		}
		if c.TaskID != taskID {
			ids := make([]string, len(open))
			for i, k := range open {
				ids[i] = strconv.FormatInt(k.ID, 10)
			}
			return 0, ErrActRefused(fmt.Sprintf("case %d is not a case of this card: its open cases are %s", caseID, cmp.Or(textutil.AndList(ids), "none")))
		}
		steps, err := r.store.CaseSteps(ctx, c.ID)
		if err != nil {
			return 0, err
		}
		own, _ = inProgress(steps, 2)
	} else {
		for _, k := range open {
			steps, err := r.store.CaseSteps(ctx, k.ID)
			if err != nil {
				return 0, err
			}
			if step, ok := inProgress(steps, 2); ok {
				c, own = k, step
				break
			}
		}
	}
	if own.ID == 0 || c.Status != CaseOpen {
		return 0, nil
	}
	if rec.Action != "" {
		a, ok := ActionByName(rec.Action)
		if !ok || !a.Rungs[3] {
			names := make([]string, 0, len(Actions))
			for _, a := range Actions {
				if a.Rungs[3] {
					names = append(names, a.Name)
				}
			}
			return 0, ErrActRefused(fmt.Sprintf("%q is no action the human may take: recommend %s", rec.Action, textutil.AndList(names)))
		}
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var did []CaseAction
	if rec.Action != "" {
		did = []CaseAction{{Action: rec.Action, Args: r.scrubArgs(rec.Args), Result: resultRecommended}}
	}
	found = textutil.Truncate(r.scrub(found), 1200)
	grounds := textutil.Truncate(r.scrub(strings.TrimSpace(rec.Grounds)), 1200)
	lock, _ := r.caseLocks.LoadOrStore(c.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	err = r.store.EndCaseStep(ctx, CaseStep{ID: own.ID, Outcome: StepHandedUp, Found: found, Did: did, Decision: rec.Action, Grounds: grounds, Read: nonNil(reads)})
	lock.(*sync.Mutex).Unlock()
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	head, _ := c.Facts["head"].(string)
	model, _ := c.Facts["model"].(string)
	f, err := r.tripFacts(ctx, t, head, model)
	if err != nil {
		return c.ID, err
	}
	f.atCase(c)
	_, err = r.climb(ctx, t, c, f, 2, found)
	return c.ID, err
}

// scrubArgs is args with every string value masked, nested ones included.
func (r *Runner) scrubArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = r.scrubValue(v)
	}
	return out
}

func (r *Runner) scrubValue(v any) any {
	switch x := v.(type) {
	case string:
		return r.scrub(x)
	case map[string]any:
		return r.scrubArgs(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.scrubValue(e)
		}
		return out
	}
	return v
}
