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
	r.atCaseOf(ctx, &f, t, c)
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return "", err
	}
	steps = slices.DeleteFunc(steps, func(s CaseStep) bool { return s.Rung == 2 && s.Outcome == StepInProgress })
	d, derr := r.CardDigest(ctx, t.ID)
	var choices []QuestionChoice
	if answersByChoices(c, t.Kind) {
		ask, _ := r.CaseAsk(ctx, c)
		if choices = AskOffers(ask.Detail); choices == nil {
			choices = []QuestionChoice{}
		}
	}
	text = r.statementOf(c, f, DigestText(r.Scrub, d, derr), steps, 2, choices)
	if planAsk(c) {
		revision, _ := toInt(c.Facts["revision"])
		off, err := RevisionTakesOffMain(ctx, r.store, t.ID, revision)
		if err != nil {
			return "", err
		}
		open, err := r.store.UndecidedProofs(ctx, t.ID, revision)
		if err != nil {
			return "", err
		}
		text += planOffer(c, f.Autonomy, off, open > 0) + "\n"
	}
	return text, nil
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

// RungTwoRecommendation is what rung 2 recommends to the human: an action
// with its arguments, or on a plan's case the plan's answer (a choice of
// its question, confirm or revise), and the words that ground it.
type RungTwoRecommendation struct {
	Action, Grounds string
	Args            map[string]any
}

// HandUpRung2 ends rung 2's step on a case of card taskID handed up, with
// found as its finding, the recommendation as its decision and grounds and
// the turn's reads, and climbs: the human is asked once, with the
// recommendation, which the face preselects as an action; a plan answer
// is its decision alone. The case is caseID, or with 0 the card's open
// case rung 2 holds. It answers the case it handed up, 0 when rung 2 holds
// no case of the card, and the ask stays as it was. A case of another card
// is refused with the card's open cases.
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
		if err := rungTwoRecommends(c, rec.Action); err != nil {
			return 0, err
		}
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var did []CaseAction
	if rec.Action != "" && !planAsk(c) {
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
	r.atCaseOf(ctx, &f, t, c)
	_, err = r.Climb(ctx, t, c, f, 2, found)
	return c.ID, err
}

// rungTwoRecommends refuses rung 2's recommendation rec on case c unless the
// human can take it there: a plan's question one of its choices, any
// answer where it offers none, its confirmation confirm or revise, every
// other case an action the human may take.
func rungTwoRecommends(c Case, rec string) error {
	switch c.Kind {
	case CasePlanConfirmation:
		if rec != DecisionConfirm && rec != DecisionRevise {
			return ErrActRefused(fmt.Sprintf("%q is no decision of a plan's confirmation: recommend %s or %s", rec, DecisionConfirm, DecisionRevise))
		}
		return nil
	case CasePlanQuestion:
		if choices := planQuestion(c).Labels(); len(choices) > 0 && !slices.Contains(choices, rec) {
			return ErrActRefused(fmt.Sprintf("%q is no choice of this plan question: recommend one of %s", rec, textutil.AndList(choices)))
		}
		return nil
	}
	if a, ok := ActionByName(rec); ok && a.Rungs[3] {
		return nil
	}
	names := make([]string, 0, len(Actions))
	for _, a := range Actions {
		if a.Rungs[3] {
			names = append(names, a.Name)
		}
	}
	return ErrActRefused(fmt.Sprintf("%q is no action the human may take: recommend %s", rec, textutil.AndList(names)))
}

// RefusedRung2 hands up each of cases whose rung 2 turn the model provider
// refused with detail: its step in progress ends handed up, found "not
// judged: the model provider refused: <detail>", and the case climbs at
// once, as HandUpRung2 climbs it. A case rung 2 no longer holds is left as
// it is.
func (r *Runner) RefusedRung2(ctx context.Context, cases []int64, detail string) error {
	var errs []error
	for _, id := range cases {
		c, err := r.store.CaseByID(ctx, id)
		if err == nil {
			_, err = r.HandUpRung2(ctx, c.TaskID, c.ID, markProviderRefused+": "+detail, RungTwoRecommendation{}, nil)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("case %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
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
