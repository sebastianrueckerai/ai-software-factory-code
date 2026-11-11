package factoryd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// ownedByRung2 is the in-progress step of rung 2 on steps, if any.
func ownedByRung2(steps []CaseStep) (CaseStep, bool) {
	i := slices.IndexFunc(steps, func(s CaseStep) bool { return s.Rung == 2 && s.Outcome == StepInProgress })
	if i < 0 {
		return CaseStep{}, false
	}
	return steps[i], true
}

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
	d, derr := r.CardDigest(ctx, t.ID)
	return r.rungStatement(c, f, DigestText(r.Scrub, d, derr), steps, 2), nil
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
// holds. handled is false when rung 2 holds no case of the card, and the
// ask stays as it was.
func (r *Runner) HandUpRung2(ctx context.Context, taskID string, caseID int64, found string, rec RungTwoRecommendation, reads []CaseRead) (handled bool, err error) {
	var c Case
	var own CaseStep
	if caseID != 0 {
		if c, err = r.store.CaseByID(ctx, caseID); err != nil {
			return false, err
		}
		if c.TaskID != taskID {
			return false, nil
		}
		steps, err := r.store.CaseSteps(ctx, c.ID)
		if err != nil {
			return false, err
		}
		own, _ = ownedByRung2(steps)
	} else {
		cases, err := r.store.OpenCasesOf(ctx, taskID)
		if err != nil {
			return false, err
		}
		for _, k := range cases {
			steps, err := r.store.CaseSteps(ctx, k.ID)
			if err != nil {
				return false, err
			}
			if step, ok := ownedByRung2(steps); ok {
				c, own = k, step
				break
			}
		}
	}
	if own.ID == 0 || c.Status != CaseOpen {
		return false, nil
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
			return true, ErrActRefused(fmt.Sprintf("%q is no action the human may take: recommend %s", rec.Action, textutil.AndList(names)))
		}
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return true, err
	}
	var did []CaseAction
	if rec.Action != "" {
		did = []CaseAction{{Action: rec.Action, Args: rec.Args, Result: "recommended, not run"}}
	}
	found = textutil.Truncate(r.scrub(found), 1200)
	grounds := textutil.Truncate(r.scrub(strings.TrimSpace(rec.Grounds)), 1200)
	lock, _ := r.caseLocks.LoadOrStore(c.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	err = r.store.EndCaseStep(ctx, CaseStep{ID: own.ID, Outcome: StepHandedUp, Found: found, Did: did, Decision: rec.Action, Grounds: grounds, Read: nonNil(reads)})
	lock.(*sync.Mutex).Unlock()
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	head, _ := c.Facts["head"].(string)
	model, _ := c.Facts["model"].(string)
	f, err := r.tripFacts(ctx, t, head, model)
	if err != nil {
		return true, err
	}
	f.atCase(c)
	_, err = r.climb(ctx, t, c, f, 2, found)
	return true, err
}
