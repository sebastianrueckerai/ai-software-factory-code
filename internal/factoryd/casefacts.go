package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// CaseFacts is what the ladder knows about a case when it decides: the
// state of the work, the money, the open findings, the dependencies and
// what the pool still holds. Stored as the case's facts.
type CaseFacts struct {
	// Kind is the kind of the case the facts are weighed for, "" for an
	// answer on no case.
	Kind CaseKind `json:"kind,omitempty"`
	// Gate is the activity a gate stop's case names, "" for any other case.
	Gate string `json:"gate,omitempty"`
	// Final is the card's status once it ended, "" while it lives.
	Final          string `json:"final,omitempty"`
	Head           string `json:"head,omitempty"`
	PR             int64  `json:"pr,omitempty"`
	PROpen         bool   `json:"pr_open,omitempty"`         // PR is set and the card neither merged nor ended
	Checks         string `json:"checks,omitempty"`          // green, red, pending or ""
	ReviewApproved bool   `json:"review_approved,omitempty"` // the newest model_review on Head was reviewed and approved
	RerunOnHead    bool   `json:"rerun_on_head,omitempty"`   // Head's checks ran again, or a rerun made Head
	WaitedOnHead   bool   `json:"waited_on_head,omitempty"`  // a case on Head ran its stopped checks wait again
	Stage          string `json:"stage,omitempty"`           // the gate the card is in
	Attempts       int    `json:"attempts"`
	SessionCap     int    `json:"session_cap"`
	// AgentTimeS is the agent time the card used and AgentTimeCapS its cap,
	// the class's sized by the card plus what was added, in seconds with
	// their fraction. SessionLeastS is what a session needs to start, as
	// the run reads it: minSession, or the card's sized agent time when
	// shorter. The cap and the least are 0 when the config is unread.
	AgentTimeS    float64 `json:"agent_time_s"`
	AgentTimeCapS float64 `json:"agent_time_cap_s"`
	SessionLeastS float64 `json:"session_least_s"`
	// Model is the model the next session runs on, "" when none is known
	// yet: the class's session_model, else the one the run's trip names,
	// else the card's last session's.
	Model      string  `json:"model,omitempty"`
	BudgetEUR  float64 `json:"budget_eur"`
	SpentEUR   float64 `json:"spent_eur"`   // Task.EnforcedSpent()
	LeftEUR    float64 `json:"left_eur"`    // Task.Left()
	ReserveEUR float64 `json:"reserve_eur"` // the gate reserve a session's ceiling holds out of LeftEUR
	FloorEUR   float64 `json:"floor_eur"`   // what the next opening on Model needs, as the run's floor reads it (needOf), else the policy's floor
	// FloorFree: the run's floor funds the next session unweighed, with no
	// price source or no model to price (funded).
	FloorFree  bool     `json:"floor_free,omitempty"`
	CeilingEUR float64  `json:"ceiling_eur"`        // the card's own, else CardCeiling
	Findings   []string `json:"findings,omitempty"` // the finding ids (findingID) Head's newest checks, review and audit raised
	// LastDrawAt is the time of the card's newest draw from the pool when
	// its row recorded the head and the checks then: DrawHead and
	// DrawChecks. A draw from before rows recorded them leaves all three
	// empty, and DrawFindings too.
	LastDrawAt *time.Time `json:"last_draw_at,omitempty"`
	DrawHead   string     `json:"draw_head,omitempty"`
	DrawChecks string     `json:"draw_checks,omitempty"`
	// DrawFindings are the finding ids the newest draw's row recorded,
	// nil when it recorded none, which is not the empty list.
	DrawFindings    []string `json:"draw_findings"`
	PushedSinceDraw bool     `json:"pushed_since_draw,omitempty"`
	// RevisedSinceDraw is set when a planner filed a revision since the
	// last draw: what a planner's draw buys, as a push does for an agent.
	RevisedSinceDraw bool `json:"revised_since_draw,omitempty"`
	// TimeDraw is the newest draw that raised agent time, the one an agent
	// time stop weighs: a fund or a sessions extend beside it raised
	// nothing that stop names.
	TimeDraw     *DrawFacts  `json:"time_draw,omitempty"`
	Pushed       bool        `json:"pushed,omitempty"` // a session pushed a branch head
	Dependencies []DepFact   `json:"dependencies,omitempty"`
	Planner      bool        `json:"planner,omitempty"` // the card is a plan, whose sessions file revisions and push nothing
	Plan         string      `json:"plan,omitempty"`    // the plan's task id, for a plan or a plan's card
	Running      bool        `json:"running,omitempty"` // a session is live
	Autonomy     string      `json:"autonomy"`
	PoolLeft     policy.Draw `json:"pool_left"`
	CardDrawLeft policy.Draw `json:"card_draw_left"`
	Partial      []string    `json:"partial,omitempty"` // the readers that failed, by name
	// Killed is the memory kill that ended the session before the stop.
	Killed *SessionKill `json:"killed,omitempty"`
}

// DrawFacts is a draw as the bought-nothing guard weighs it: its time, the
// head, the checks and the findings it recorded, and whether a session
// pushed or a planner filed a revision after it.
type DrawFacts struct {
	At       time.Time `json:"at"`
	Head     string    `json:"head"`
	Checks   string    `json:"checks"`
	Findings []string  `json:"findings"`
	Pushed   bool      `json:"pushed,omitempty"`
	Revised  bool      `json:"revised,omitempty"`
}

// SessionLeft is the budget a session may spend, the headroom SessionFunded
// reads: what the card has left less the gate reserve held for its gates.
func (f CaseFacts) SessionLeft() float64 { return policy.SessionHeadroom(f.LeftEUR, f.ReserveEUR) }

// agentTime is f's agent-time cap, what is left of it and what a session
// needs to start.
func (f CaseFacts) agentTime() (limit, left, need time.Duration) {
	limit = seconds(f.AgentTimeCapS)
	return limit, limit - seconds(f.AgentTimeS), seconds(f.SessionLeastS)
}

// seconds is s seconds to the nanosecond.
func seconds(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }

// unfunded: what is left for a session is under what its opening needs,
// the gateway's own test of the card's headroom (policy.FitResource),
// where the run's floor weighs it.
func (f CaseFacts) unfunded() bool { return !f.FloorFree && f.FloorEUR > f.SessionLeft() }

// noSession is the stop the run asks at its loop top on f before a
// session, "" when one fits, in the run's order: the budget floor when the
// card's money no longer funds a session's opening (a plan's loop reads
// none), the session cap once
// the attempts reached it, the agent-time cap when less is left than a
// session needs. A reply that buys back what the stopped session took
// (refund) leaves the floor alone to weigh. read is false when a limit
// weighed is unread, which fits none.
func (f CaseFacts) noSession(refund bool) (asked CaseKind, read bool) {
	limit, left, need := f.agentTime()
	switch {
	case slices.Contains(f.Partial, "floor"):
		return "", false
	case !f.Planner && f.unfunded():
		return CaseBudgetFloor, true
	case refund:
		return "", true
	case f.SessionCap <= 0 || limit <= 0 || need <= 0:
		return "", false
	case f.Attempts >= f.SessionCap:
		return CaseSessionCap, true
	case left < need:
		return CaseAgentTimeCap, true
	}
	return "", true
}

// atCase weighs f for case c: its kind, and the gate a gate stop names.
func (f *CaseFacts) atCase(c Case) {
	f.Kind, f.Gate = c.Kind, ""
	if c.Kind == CaseGateStop {
		f.Gate, _ = c.Facts["gate"].(string)
	}
}

// DepFact is one dependency of the card that is not done.
type DepFact struct {
	TaskID   string `json:"task_id"`
	Issue    int64  `json:"issue"`
	Status   string `json:"status"`
	Why      string `json:"why,omitempty"`      // the end reason of an ended one
	Replaced string `json:"replaced,omitempty"` // the replacement's task id, if retried
	RetryOf  string `json:"retry_of,omitempty"` // the failed card an ended one replaced, if it is a retry
}

// tripFacts reads every fact of card t on head, the head of the card's
// newest gate_head row when head is "", with the next session on model,
// the card's last session's when model is "", and names the readers that
// failed in Partial, their facts left zero. It fails only when ctx ends,
// since facts read past that are no facts.
func (r *Runner) tripFacts(ctx context.Context, t Task, head, model string) (CaseFacts, error) {
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	f := CaseFacts{Head: head, PR: t.PRNumber, PROpen: t.PRNumber > 0 && t.Status != "merged" && !Final(t.Status), Stage: t.Gate,
		Attempts: t.Usage.Attempts, SessionCap: SessionCap(t, v.Document.Limits.Sessions) + t.Usage.SessionsAdded(),
		AgentTimeS: t.Usage.AgentTime.Seconds(), Model: sessionModel(v.Document, t, cmp.Or(model, t.ModelAlias)),
		BudgetEUR: t.BudgetEUR, SpentEUR: t.EnforcedSpent(), LeftEUR: t.Left(), ReserveEUR: v.Document.Limits.GateReserve(), CeilingEUR: cmp.Or(t.CeilingEUR, CardCeiling(v.Document, project)),
		Planner: t.Track == policy.TrackPlan, Running: t.Status == "running", Autonomy: project.AutonomyState()}
	if Final(t.Status) {
		f.Final = t.Status
	}
	if InPlan(t) {
		f.Plan = GroupOf(t)
	}
	esc := v.EscalationOf(t.Repository)
	var reran []string
	// the readers in order, each after the head the events name
	readers := []struct {
		name string
		read func() error
	}{
		{"events", func() error {
			events, err := r.store.TaskEvents(ctx, t.ID, cardDigestEvents)
			if err == nil {
				f.eventFacts(events)
			}
			return err
		}},
		{"timeline", func() error {
			tl, err := r.objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
			if err == nil {
				lines, _ := lineSteps(tl)
				f.Findings, reran = headFindings(lines, f.Head), rerunHeads(tl)
			}
			return err
		}},
		{"checks", func() error {
			if f.Head == "" {
				return nil
			}
			res, err := r.HeadChecks(ctx, t.ID, f.Head)
			if err == nil {
				f.Checks = checksWord(res.State)
			}
			return err
		}},
		{"config", func() error {
			tc, err := r.TaskConfig(ctx, t.ID, 0)
			if err == nil {
				sized := policy.SizeAgentTime(t.Size, tc.AgentTime)
				f.AgentTimeCapS = (sized + t.Usage.TimeAdded()).Seconds()
				f.SessionLeastS = min(minSession, sized).Seconds()
			}
			return err
		}},
		{"floor", func() error {
			if f.Model == "" || r.cfg.Prices == nil {
				f.FloorEUR, f.FloorFree = v.Document.Budget.SessionFloor(f.Model != "" && r.frontier(f.Model)), true
				return nil
			}
			n, err := r.needOf(ctx, v, t, f.Model, true)
			if err == nil && n.unread != "" {
				err = errors.New(n.unread)
			}
			f.FloorEUR = n.eur
			return err
		}},
		{"dependencies", func() (err error) {
			f.Dependencies, err = r.depFacts(ctx, t.ID)
			return err
		}},
		{"cases", func() error {
			cases, err := r.store.CasesOf(ctx, t.ID)
			f.RerunOnHead = f.Head != "" && (slices.Contains(reran, f.Head) || (err == nil && slices.ContainsFunc(cases, func(c Case) bool {
				return c.Resolution != nil && c.Resolution.Action == ActRerunChecks && c.Facts["head"] == f.Head
			})))
			f.WaitedOnHead = f.Head != "" && err == nil && slices.ContainsFunc(cases, func(c Case) bool {
				return c.Resolution != nil && c.Resolution.Action == ActWaitChecks && c.Facts["head"] == f.Head
			})
			return err
		}},
		{"pool", func() error {
			drawn, err := r.store.PoolDrawn(ctx, t.Repository, r.clock())
			if err == nil {
				f.PoolLeft = esc.Pool.Minus(drawn)
			}
			return err
		}},
		{"card draw", func() error {
			drawn, err := r.store.CardDrawn(ctx, t.ID)
			if err == nil {
				f.CardDrawLeft = esc.CardDrawOf(t.Size).Minus(drawn)
			}
			return err
		}},
	}
	for _, rd := range readers {
		if err := ctx.Err(); err != nil {
			return f, err
		}
		if rd.read() != nil {
			f.Partial = append(f.Partial, rd.name)
		}
	}
	return f, ctx.Err()
}

// eventFacts sets f's head, its review, whether a session pushed, and its
// last draw from the card's events, newest first: the head of the newest
// gate_head row unless f has one, the newest model_review of the head,
// and the newest draw from the pool, when its row recorded the head and
// the checks, with the findings it recorded and whether a session pushed
// after it or a planner filed a revision. A row of no sessions, no agent
// time and no money, an extend of lines or of a check-in share alone,
// drew nothing and is no draw. TimeDraw is the newest draw that raised
// agent time, read the same way: the one an agent time stop weighs.
func (f *CaseFacts) eventFacts(events []Event) {
	word := func(e Event, k string) string { s, _ := e.Detail[k].(string); return s }
	pushed := func(e Event) bool { return e.Kind == EventSessionPushed }
	f.Pushed = slices.ContainsFunc(events, pushed)
	if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventGateHead }); i >= 0 && f.Head == "" {
		f.Head = word(events[i], "head")
	}
	if i := slices.IndexFunc(events, func(e Event) bool {
		return e.Kind == EventModelReview && f.Head != "" && word(e, "commit") == f.Head
	}); i >= 0 {
		reviewed, _ := events[i].Detail["reviewed"].(bool)
		f.ReviewApproved = reviewed && word(events[i], "verdict") == "approve"
	}
	drawn := func(timeOnly bool) int {
		return slices.IndexFunc(events, func(e Event) bool {
			sessions, _ := toInt(e.Detail["sessions"])
			seconds, _ := toInt(e.Detail["agent_time_s"])
			eur, _ := e.Detail["amount_eur"].(float64)
			return (e.Kind == EventCardFunded || e.Kind == EventCardExtended) && e.Detail["pool"] == true &&
				(seconds != 0 || !timeOnly && (sessions != 0 || eur != 0))
		})
	}
	if d := drawFactsAt(events, drawn(false), pushed); d != nil {
		f.LastDrawAt, f.DrawHead, f.DrawChecks, f.DrawFindings = &d.At, d.Head, d.Checks, d.Findings
		f.PushedSinceDraw, f.RevisedSinceDraw = d.Pushed, d.Revised
	}
	f.TimeDraw = drawFactsAt(events, drawn(true), pushed)
}

// drawFactsAt reads the draw at events[i], nil when i is none or its row
// recorded no head and checks.
func drawFactsAt(events []Event, i int, pushed func(Event) bool) *DrawFacts {
	if i < 0 {
		return nil
	}
	head, hasHead := events[i].Detail["head"].(string)
	checks, hasChecks := events[i].Detail["checks"].(string)
	if !hasHead || !hasChecks {
		return nil
	}
	d := &DrawFacts{At: events[i].At, Head: head, Checks: checks, Pushed: slices.ContainsFunc(events[:i], pushed),
		Revised: slices.ContainsFunc(events[:i], func(e Event) bool { return e.Kind == EventPlanRevision })}
	switch ids := events[i].Detail["findings"].(type) {
	case []string:
		d.Findings = append([]string{}, ids...)
	case []any:
		d.Findings = []string{}
		for _, id := range ids {
			if s, ok := id.(string); ok {
				d.Findings = append(d.Findings, s)
			}
		}
	}
	slices.Sort(d.Findings)
	return d
}

// headFindings are the sorted finding ids that head's newest checks, its
// newest criteria gate run, its newest review and its newest audit
// raised, of the timeline's steps in line order.
func headFindings(steps []DigestStep, head string) []string {
	if head == "" {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, s := range slices.Backward(steps) {
		if s.commit != head || seen[s.Kind] || !slices.Contains([]string{"checks", "proofs", "review", "audit"}, s.Kind) {
			continue
		}
		seen[s.Kind] = true
		ids = append(ids, s.Findings...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// stepChecksRerun is the timeline's line of a rerun of a head's checks:
// the head it ran again (commit) and the head the rerun made.
const stepChecksRerun = "checks_rerun"

// rerunHeads are the heads timeline tl ran the checks of again and the
// heads those reruns made.
func rerunHeads(tl []byte) []string {
	var heads []string
	for _, raw := range bytes.Split(tl, []byte("\n")) {
		var l struct{ Step, Commit, Head string }
		if json.Unmarshal(raw, &l) == nil && l.Step == stepChecksRerun {
			heads = append(heads, l.Commit, l.Head)
		}
	}
	return heads
}

// checksWord is a head's checks state as the facts name it.
func checksWord(s ChecksState) string {
	switch s {
	case ChecksSuccess:
		return "green"
	case ChecksFailure:
		return "red"
	}
	return "pending"
}

// CardCeiling is the most the policy lets one card of project be funded
// to: task_max_eur, or the project's own when it is lower.
func CardCeiling(d policy.Document, p policy.ProjectConfig) float64 {
	if p.Budget != nil && p.Budget.TaskMaxEUR > 0 {
		return min(d.Budget.TaskMaxEUR, p.Budget.TaskMaxEUR)
	}
	return d.Budget.TaskMaxEUR
}

// depFacts are card id's dependencies that are not done: an ended one
// with the reason of its task_ended row and the card it replaced when a
// retry filed it, a failed one with its replacement when a retry filed
// one (RetryID).
func (r *Runner) depFacts(ctx context.Context, id string) ([]DepFact, error) {
	deps, err := r.store.OpenDependencies(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []DepFact
	for _, d := range deps {
		fact := DepFact{TaskID: d.TaskID, Issue: d.Issue, Status: d.Status}
		if Final(d.Status) {
			events, err := r.store.TaskEvents(ctx, d.TaskID, cardDigestEvents)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventTaskEnded }); i >= 0 {
				fact.Why, _ = events[i].Detail["reason"].(string)
			}
			if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventCardCreated }); i >= 0 {
				fact.RetryOf, _ = events[i].Detail["replaces"].(string)
			}
		}
		if d.Status == "failed" {
			_, retried, err := r.store.Task(ctx, RetryID(d.TaskID))
			if err != nil {
				return nil, err
			}
			if retried {
				fact.Replaced = RetryID(d.TaskID)
			}
		}
		out = append(out, fact)
	}
	return out, nil
}
