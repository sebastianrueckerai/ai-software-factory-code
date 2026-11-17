package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// CardHold is a human's hold on a card that has not started: it gets no
// work slot until a human releases it, or until its release card, any
// card of its repository, ends done or merged. A release card that ends
// failed or closed releases nothing, and the board warns of it. The
// release card follows its work into a retry or a successor, as a
// dependency edge does (takeOverEdges).
type CardHold struct {
	Reason  string
	Release string // the release card's task id, "" for none
	By      string
	At      time.Time
}

// Held reports whether t's hold holds it: one stands and the card has not
// started.
func (t Task) Held() bool {
	return t.Hold != nil && (t.Status == "queued" || t.Status == "waiting-deps")
}

// Holdable reports whether t may take a hold, as SetCardHold judges it
// but for its dependencies: a bound agent card queued and never admitted
// or resumed, or in waiting-deps.
func (t Task) Holdable() bool {
	return t.Kind == "agent" && !t.Unbound() &&
		(t.Status == "queued" && t.AdmittedAt == nil && t.ResumedAt == nil || t.Status == "waiting-deps")
}

// errHeld is SetAdmitted's refusal of a card a hold took after admit read
// it.
var errHeld = errors.New("the card is on hold")

// HoldView is where a card's hold stands, read with its release card:
// Line words it with the human's reason; Warn marks a release card that
// releases nothing, one that ended failed or closed or one that became a
// plan (a direct card cut into one), whose done comes before its work
// lands; Released one that ended done or merged, whose hold the card's
// next admission or dependency check ends.
type HoldView struct {
	Line     string
	Warn     bool
	Released bool
	release  Task
}

// holdView reads where card t's hold stands; t holds one.
func holdView(ctx context.Context, store interface {
	Task(ctx context.Context, id string) (Task, bool, error)
}, t Task) (HoldView, error) {
	h := t.Hold
	if h.Release == "" {
		return HoldView{Line: "on hold: " + h.Reason}, nil
	}
	rel, _, err := store.Task(ctx, h.Release)
	if err != nil {
		return HoldView{}, err
	}
	name, err := releaseName(ctx, store, rel)
	if err != nil {
		return HoldView{}, err
	}
	v := HoldView{Line: fmt.Sprintf("on hold until %s is merged or done: %s", name, h.Reason), release: rel}
	switch {
	case rel.Track == policy.TrackPlan:
		v.Warn = true
		v.Line = fmt.Sprintf("on hold: its release card %s became a plan, which is done once it files its cards, before their work lands, so a human releases the hold or holds the card on the plan's last card: %s",
			name, h.Reason)
	case rel.Status == "done" || rel.Status == "merged":
		v.Released = true
	case rel.Status == "failed" || rel.Status == "closed":
		v.Warn = true
		fit := "releases the hold or holds the card on another card"
		if rel.Status == "failed" {
			fit = fmt.Sprintf("retries #%d, whose retry takes the hold over, releases the hold or holds the card on another card", rel.IssueNumber)
		}
		v.Line = fmt.Sprintf("on hold: its release card %s ended %s and releases nothing, so a human %s: %s", name, rel.Status, fit, h.Reason)
	}
	return v, nil
}

// strandedHolds records on each card a hold keeps until release card rel,
// which ended final (failed or closed) and so releases nothing, the row the
// Poker tells the human (EventCardHoldStranded): once per hold and end, by
// its identity.
func strandedHolds(ctx context.Context, tl *Timeline, store Store, rel Task, final string) {
	held, err := store.HeldOn(ctx, rel.ID)
	if err != nil {
		log.Printf("task %s: the holds it releases: %s", rel.ID, maskedErr(err))
		return
	}
	if len(held) == 0 {
		return
	}
	name, err := releaseName(ctx, store, rel)
	if err != nil {
		log.Printf("task %s: its plan: %s", rel.ID, maskedErr(err))
		return
	}
	for _, h := range held {
		tl.RecordFact(ctx, Event{Repository: h.Repository, TaskID: h.ID, IssueNumber: h.IssueNumber, Kind: EventCardHoldStranded,
			Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"release_task": rel.ID, "release": name, "release_issue": rel.IssueNumber, "status": final},
			IdentityKey: IdentityKey(EventCardHoldStranded, h.ID, h.Hold.At.UTC().Format(time.RFC3339Nano), rel.ID, final)})
	}
}

// releaseName is how a hold names its release card rel: its issue, and its
// plan's when it is a plan's card.
func releaseName(ctx context.Context, store interface {
	Task(ctx context.Context, id string) (Task, bool, error)
}, rel Task) (string, error) {
	name := fmt.Sprintf("#%d", rel.IssueNumber)
	if rel.Parent == "" {
		return name, nil
	}
	p, ok, err := store.Task(ctx, rel.Parent)
	if err != nil {
		return "", err
	}
	if ok && p.Track == policy.TrackPlan {
		name += fmt.Sprintf(" (plan #%d)", p.IssueNumber)
	}
	return name, nil
}

// holdFact is card t's hold as a case fact (CaseFacts.Hold), "" when no
// hold holds it.
func (r *Runner) holdFact(ctx context.Context, t Task) (string, error) {
	if !t.Held() {
		return "", nil
	}
	v, err := holdView(ctx, r.store, t)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("card %s is %s", cardName(t), v.Line), nil
}

// The readers of the hold facts, as CaseFacts.Partial names them.
const (
	partialHold     = "hold"
	partialPlanHold = "plan hold"
)

// holdStops are the answers that would start or close the card itself.
var holdStops = []string{ActResume, ActContinue, ActHandover, ActSuccessor, ActClose}

// holdRefusal is why rung, below the human, may not answer f with action
// and its args: only a human releases a held card, starts it or closes it, so
// no rung takes a resume, continue, handover, successor or close of it, a
// replan of it, which sends its work back to the planner to be filed
// again, nor a restart of its plan while a card of it is held, which
// closes the plan and files its work anew. A replan of a failed
// dependency's work leaves the card held. A hold that could not be read
// refuses as one. "" when rung may.
func holdRefusal(f CaseFacts, rung int, action string, args map[string]any) string {
	if rung >= 3 {
		return ""
	}
	var held, partial, on string
	switch {
	case action == ActRestartPlan:
		held, partial, on = f.PlanHold, partialPlanHold, "its plan"
	case slices.Contains(holdStops, action), action == ActReplan && argString(args, "task_id") == "":
		held, partial, on = f.Hold, partialHold, "it"
	default:
		return ""
	}
	if slices.Contains(f.Partial, partial) {
		what := "the card's hold"
		if partial == partialPlanHold {
			what = "the holds of its plan's cards"
		}
		return fmt.Sprintf("%s could not be read, so %s takes no %s on %s: try again in a minute, or hand it up to the human", what, who(rung), action, on)
	}
	if held == "" {
		return ""
	}
	return fmt.Sprintf("%s. Only a human releases a held card, starts it or closes it, so %s takes no %s on %s: %s", held, who(rung), action, on, holdFit(f, rung))
}

// holdFit is what fits rung on f instead of an answer a hold refuses: the
// note and the settle where the table offers them, and the hand-up; on no
// case, an ask of the human.
func holdFit(f CaseFacts, rung int) string {
	if f.Kind == "" {
		return "ask the human instead"
	}
	var fits []string
	for _, fit := range []struct{ action, words string }{{ActNote, "note the case"}, {ActSettle, "settle it"}} {
		if a, _ := ActionByName(fit.action); a.offerRefusal(f, rung) == "" {
			fits = append(fits, fit.words)
		}
	}
	fits = append(fits, "hand it up to the human")
	if len(fits) == 1 {
		return fits[0]
	}
	return strings.Join(fits[:len(fits)-1], ", ") + " or " + fits[len(fits)-1]
}

// cardHoldWait is the line of card t's hold, which keeps it from
// starting; "" when it holds none, or when its release card ended done or
// merged: that hold ends here, recorded as the release card's. A hold
// that changed since t was read keeps the card this time.
func cardHoldWait(ctx context.Context, store Store, t Task) (string, error) {
	if !t.Held() {
		return "", nil
	}
	v, err := holdView(ctx, store, t)
	if err != nil || !v.Released {
		return v.Line, err
	}
	ended, err := store.EndCardHold(ctx, t.ID, t.Hold.Release)
	if err != nil || !ended {
		return v.Line, err
	}
	if _, err := store.Record(ctx, Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Kind: EventCardHoldEnded,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"release": v.release.IssueNumber, "status": v.release.Status}}); err != nil {
		log.Printf("task %s: the end of its hold: %s", t.ID, maskedErr(err))
	}
	return "", nil
}

// HoldCard puts card id on hold for by, a human, for reason, until a
// human releases it or release ("" for none) ends done or merged. False:
// the card has started or holds a work slot already.
func (r *Runner) HoldCard(ctx context.Context, id, release, reason, by string) (bool, error) {
	return r.store.SetCardHold(ctx, id, CardHold{Reason: reason, Release: release, By: by, At: r.Clock()})
}

// ReleaseCardHold ends card id's hold for a human and wakes the card: a
// queued one through the work bound's pull, one in waiting-deps by
// dep_ended. False: it holds none.
func (r *Runner) ReleaseCardHold(ctx context.Context, id string) (bool, error) {
	t, ok, err := r.store.Task(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	ended, err := r.store.EndCardHold(ctx, id, "")
	if err != nil || !ended || r.cfg.Workflows == nil {
		return ended, err
	}
	if t.Status == "waiting-deps" {
		if err := r.cfg.Workflows.Signal(ctx, id, SignalDepEnded, TaskSignal{}); err != nil {
			log.Printf("task %s: signal %s after its hold: %s", id, SignalDepEnded, maskedErr(err))
		}
		return true, nil
	}
	pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	return true, nil
}

// heldFindings refuses revision next of the plan st judges when it drops
// the key of a card a human holds, whoever asked for the replan: the
// revision's restart would close the card, and only a human closes a held
// card, on its drawer. A kept key holds the card, which never started, and
// gives it the revision's entry for that key. Once the human closed it,
// replanFindings asks for its key to be left out.
func (r *Runner) heldFindings(ctx context.Context, st replanState, next Plan) ([]string, error) {
	var found []string
	for _, k := range st.old {
		if !k.Held() || k.PlanKey == "" || slices.ContainsFunc(next.Cards, func(c PlanCard) bool { return c.Key == k.PlanKey }) {
			continue
		}
		v, err := holdView(ctx, r.store, k)
		if err != nil {
			return nil, err
		}
		found = append(found, fmt.Sprintf("card %s (#%d) is %s. Only a human closes a held card, so keep its key: the card takes the entry this revision gives that key. "+
			"If the plan no longer needs the card, keep its key and ask in a question of this revision that the human close it or release its hold on its drawer",
			k.PlanKey, k.IssueNumber, v.Line))
	}
	return found, nil
}
