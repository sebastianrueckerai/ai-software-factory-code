package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
	Factory        bool   `json:"factory,omitempty"`         // the pull request is one factoryd opened
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
	// price source (funded).
	FloorFree bool `json:"floor_free,omitempty"`
	// CapWait is the cap wait the run's floor would ask before the next
	// session (SessionFunded): the day's, the month's or the project's cap
	// binding before the card's own budget, as its question line
	// (capWaitReason) says when it lifts and what raises it; "" for none.
	CapWait    string   `json:"cap_wait,omitempty"`
	CeilingEUR float64  `json:"ceiling_eur"`        // the card's own, else CardCeiling
	Findings   []string `json:"findings,omitempty"` // the finding ids (findingID) Head's newest checks, review and audit raised
	// FindingTexts maps each of Findings to its text.
	FindingTexts map[string]string `json:"finding_texts,omitempty"`
	// FindingOrder lists Findings in the order the gates raised them.
	FindingOrder []string `json:"finding_order,omitempty"`
	// FailedTests are the tests the red checks on Head failed, read when
	// Checks is red; HeadCommits are the pull request's commits after the
	// newest session's push, each "<sha8> <author> <subject>".
	FailedTests []FailedTest `json:"failed_tests,omitempty"`
	HeadCommits []string     `json:"head_commits,omitempty"`
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
	TimeDraw *DrawFacts `json:"time_draw,omitempty"`
	// UnreadGates are the gates (the review, the audit) the card's rows say
	// did not run, which rung 2's merge approval is refused on
	UnreadGates []string `json:"unread_gates,omitempty"`
	// RiskClass is the risk class a merge approval's change has, "" for any
	// other case, and HumanClasses, on a merge approval alone, the classes
	// whose merge the policy keeps with a human (policy.Risk.HumanClasses).
	RiskClass    string   `json:"risk_class,omitempty"`
	HumanClasses []string `json:"human_classes,omitempty"`
	// NoIssue: a queued work card the intake never linked to an issue, a plan's row whose issue did not link; a report card has none by design.
	NoIssue bool `json:"no_issue,omitempty"`
	// Criteria: the card is the integration or a delta card of a plan whose
	// criteria a human judges on its merge
	Criteria bool `json:"criteria,omitempty"`
	// ApprovalKind is the kind of the approval request a case of a plan's criteria or a merge holds
	ApprovalKind string    `json:"approval_kind,omitempty"`
	Pushed       bool      `json:"pushed,omitempty"` // a session pushed a branch head
	Dependencies []DepFact `json:"dependencies,omitempty"`
	Planner      bool      `json:"planner,omitempty"` // the card is a plan, whose sessions file revisions and push nothing
	// PlannerRetry: the plan's automatic second session, topped up to half
	// its agent time, is still to come (plannerTime).
	PlannerRetry bool   `json:"planner_retry,omitempty"`
	Plan         string `json:"plan,omitempty"` // the plan's task id, for a plan or a plan's card
	// PlanIssue, PlanName and PlanStatus name the plan of a plan's card and
	// Siblings are its other cards, by issue; all empty for a direct card.
	PlanIssue    int64         `json:"plan_issue,omitempty"`
	PlanName     string        `json:"plan_name,omitempty"`
	PlanStatus   string        `json:"plan_status,omitempty"`
	Siblings     []SiblingFact `json:"siblings,omitempty"`
	Running      bool          `json:"running,omitempty"` // a session is live: the card runs and holds a live session row
	Autonomy     string        `json:"autonomy"`
	PoolLeft     policy.Draw   `json:"pool_left"`
	CardDrawLeft policy.Draw   `json:"card_draw_left"`
	Partial      []string      `json:"partial,omitempty"` // the readers that failed, by name
	// Flag and Wait are the monitor flag a monitor case holds and the wait it
	// names; RunGone: the card is live and runs a workflow, and none runs.
	Flag    string `json:"flag,omitempty"`
	Wait    string `json:"wait,omitempty"`
	RunGone bool   `json:"run_gone,omitempty"`
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

// capWaitKinds are the case kinds of the cap waits, by the cap's decision.
var capWaitKinds = map[string]CaseKind{policy.RefusedDayCap: CaseDayCap, policy.RefusedMonthCap: CaseMonthCap, policy.RefusedProjectCap: CaseProjectCap}

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
// session, "" when one fits, in the run's order: the cap wait of a cap that
// binds before the card's budget, unless the stop waits on a cap itself
// (waiting), the budget floor when the card's money no longer funds a
// session's opening (a plan's loop reads neither), the session cap once
// the attempts reached it, the agent-time cap when less is left than a
// session needs, unless a plan's automatic second session tops it up. A
// reply that buys back what the stopped session took (refund) leaves the
// money alone to weigh. read is false when a limit weighed is unread,
// which fits none.
func (f CaseFacts) noSession(refund, waiting bool) (asked CaseKind, read bool) {
	limit, left, need := f.agentTime()
	switch {
	case !f.Planner && slices.Contains(f.Partial, "floor"):
		return "", false
	case !f.Planner && !waiting && capWaitDecision(f.CapWait) != "":
		return capWaitKinds[capWaitDecision(f.CapWait)], true
	case !f.Planner && f.unfunded():
		return CaseBudgetFloor, true
	case refund:
		return "", true
	case f.SessionCap <= 0 || limit <= 0 || need <= 0:
		return "", false
	case f.Attempts >= f.SessionCap:
		return CaseSessionCap, true
	case left < need && !f.PlannerRetry:
		return CaseAgentTimeCap, true
	}
	return "", true
}

// atCase weighs f for case c: its kind, and the gate a gate stop names.
func (f *CaseFacts) atCase(c Case) {
	f.Kind, f.Gate = c.Kind, ""
	f.ApprovalKind, _ = c.Facts["approval_kind"].(string)
	f.RiskClass, _ = c.Facts["class"].(string)
	f.Factory, _ = c.Facts["factory"].(bool)
	if f.Killed == nil && c.Facts["killed"] != nil {
		f.Killed = factsOf(c).Killed
	}
	if c.Kind == CaseGateStop {
		f.Gate, _ = c.Facts["gate"].(string)
	}
	f.Flag, f.Wait = "", ""
	if c.Kind == CaseMonitorFlag {
		f.Flag, _ = c.Facts["flag"].(string)
		f.Wait, _ = c.Facts["wait"].(string)
	}
}

// atCaseOf is atCase with the policy's reading: on a merge approval, the
// classes the policy keeps a human on, read from the policy version the
// class came from (the one the card's run pinned).
func (r *Runner) atCaseOf(ctx context.Context, f *CaseFacts, c Case) error {
	f.atCase(c)
	f.HumanClasses = nil
	if c.Kind != CaseMergeApproval {
		return nil
	}
	pin, _ := toInt(c.Facts["policy_version"])
	v, err := r.pinnedVersion(ctx, int64(pin))
	if err != nil {
		return err
	}
	f.HumanClasses = v.Document.Risk.HumanClasses()
	return nil
}

// DepFact is one dependency of the card that is not done.
type DepFact struct {
	TaskID   string `json:"task_id"`
	Issue    int64  `json:"issue"`
	Status   string `json:"status"`
	Why      string `json:"why,omitempty"`      // the end reason of an ended one
	Replaced string `json:"replaced,omitempty"` // the replacement's task id, if retried
	RetryOf  string `json:"retry_of,omitempty"` // the failed card an ended one replaced, if it is a retry
	Plan     bool   `json:"plan,omitempty"`     // the dependency is a plan card
	// Since is when an open dependency took its status, or asked its
	// question when it waits on a human; Ask is that question.
	Since *time.Time `json:"since,omitempty"`
	Ask   string     `json:"ask,omitempty"`
}

// SiblingFact is another card of the plan the case's card belongs to.
type SiblingFact struct {
	Issue  int64  `json:"issue"`
	Key    string `json:"key"`
	Status string `json:"status"`
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
		Planner: t.Track == policy.TrackPlan, Running: t.Status == "running", Autonomy: project.AutonomyState(), NoIssue: t.IssueNumber == 0 && t.Status == "queued" && !t.Report()}
	if Final(t.Status) {
		f.Final = t.Status
	}
	if InPlan(t) {
		f.Plan = GroupOf(t)
	}
	esc := v.EscalationOf(t.Repository)
	var reran []string
	var pushed string
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
				lines, _ := lineSteps(r.Scrub, tl)
				f.Findings, reran = headFindings(lines, f.Head), rerunHeads(tl)
				f.FindingTexts, f.FindingOrder = headFindingTexts(lines, f.Head, f.Findings)
				pushed = pushedCommit(tl)
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
		{"live session", func() error {
			// the card keeps its status while a rung works, so the status
			// alone no longer says a session runs: its live row does
			if !f.Running {
				return nil
			}
			_, live, err := r.store.LiveSession(ctx, t.ID)
			if err == nil {
				f.Running = live
			}
			return err
		}},
		{"checks_log", func() error {
			if f.Checks != "red" || f.Head == "" {
				return nil
			}
			repo, err := SplitRepo(t.Repository)
			if err != nil {
				return err
			}
			f.FailedTests, _ = r.failedTests(ctx, t, repo, f.Head)
			if len(f.FailedTests) == 0 {
				// every red job with an Actions run names itself, so none listed is a read that failed, or a red status that names no run
				return errors.New("the red checks' log could not be read")
			}
			return nil
		}},
		{"head_commits", func() error {
			if !f.PROpen || f.Head == "" || pushed == "" {
				return nil
			}
			repo, err := SplitRepo(t.Repository)
			if err != nil {
				return err
			}
			commits, err := r.forgejo.PullCommits(ctx, repo.Owner, repo.Name, t.PRNumber)
			if err != nil {
				return err
			}
			f.HeadCommits = commitsAfter(commits, f.Head, pushed, r.Scrub)
			return nil
		}},
		{"config", func() error {
			tc, err := r.TaskConfig(ctx, t.ID, 0)
			if err == nil {
				sized := tc.AgentTime
				if !f.Planner {
					// the plan's loop reads the class's time unsized
					sized = policy.SizeAgentTime(t.Size, tc.AgentTime)
				}
				f.AgentTimeCapS = (sized + t.Usage.TimeAdded()).Seconds()
				f.SessionLeastS = min(minSession, sized).Seconds()
				f.PlannerRetry = f.Planner && t.Usage.RetryTime == 0 && tc.AgentTime/2 >= min(minSession, tc.AgentTime)
			}
			return err
		}},
		{"floor", func() error {
			if r.cfg.Prices == nil {
				f.FloorEUR, f.FloorFree = v.Document.Budget.SessionFloor(f.Model != "" && r.frontier(f.Model)), true
				return nil
			}
			if f.Model == "" {
				f.FloorEUR = v.Document.Budget.SessionFloor(false)
				return nil
			}
			n, err := r.needOf(ctx, v, t, f.Model, true)
			if err == nil && n.unread != "" {
				err = errors.New(n.unread)
			}
			f.FloorEUR = n.eur
			if err != nil {
				return err
			}
			// the run's own floor, whose caps bind before the card's budget
			why, err := r.SessionFunded(ctx, t.ID, f.Model)
			if d, ok := CapDecision(why); ok && err == nil {
				_, state, _ := strings.Cut(why, "\n")
				f.CapWait, _, _ = strings.Cut(capWaitReason(d, capFloor(d), state, capTurn(d, r.clock())), "\n")
			}
			return err
		}},
		{"review rows", func() error {
			if !f.PROpen {
				return nil
			}
			_, err := CheckMergeApproval(ctx, r.store, func(context.Context, int64) (policy.Version, bool) { return v, true }, 0, t.ID, ChatUser)
			var unread *UnreviewedMergeError
			if errors.As(err, &unread) {
				f.UnreadGates, err = unread.Stages, nil
			}
			return err
		}},
		{"criteria", func() error {
			if t.Source != SourcePlan || t.Parent == "" {
				return nil
			}
			rev, ok, err := r.store.PlanRevision(ctx, t.Parent, 0)
			if err != nil || !ok {
				return err
			}
			var p Plan
			if err := json.Unmarshal(rev.Plan, &p); err != nil {
				return err
			}
			f.Criteria = len(p.Criteria) > 0 && CriteriaCard(p, t.PlanKey)
			return nil
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
		{"siblings", func() error {
			if f.Plan == "" {
				return nil
			}
			plan, ok, err := r.store.Task(ctx, f.Plan)
			if err == nil && !ok {
				err = ErrNotFound
			}
			if err != nil {
				return err
			}
			cards, err := r.store.PlanCards(ctx, f.Plan)
			if err != nil {
				return err
			}
			f.PlanIssue, f.PlanName, f.PlanStatus = int64(plan.IssueNumber), plan.PlanName, plan.Status
			for _, c := range cards {
				if c.ID != t.ID {
					f.Siblings = append(f.Siblings, SiblingFact{Issue: int64(c.IssueNumber), Key: c.PlanKey, Status: c.Status})
				}
			}
			slices.SortFunc(f.Siblings, func(a, b SiblingFact) int { return cmp.Compare(a.Issue, b.Issue) })
			return nil
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
// time and no money, an extend of lines alone,
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

// headFindingTexts maps each of ids, the finding ids headFindings named for
// head, to its text, and lists them in the order the gates raised them.
func headFindingTexts(steps []DigestStep, head string, ids []string) (map[string]string, []string) {
	if len(ids) == 0 {
		return nil, nil
	}
	var newest []DigestStep
	seen := map[string]bool{}
	for _, s := range slices.Backward(steps) {
		if s.commit == head && !seen[s.Kind] && slices.Contains([]string{"checks", "proofs", "review", "audit"}, s.Kind) {
			seen[s.Kind] = true
			newest = append(newest, s)
		}
	}
	texts, order := map[string]string{}, []string{}
	for _, s := range slices.Backward(newest) {
		for _, text := range s.texts {
			if id := findingID(text); slices.Contains(ids, id) && texts[id] == "" {
				texts[id] = text
				order = append(order, id)
			}
		}
	}
	return texts, order
}

// pushedCommit is the commit of the newest push the timeline records, ""
// for none.
func pushedCommit(tl []byte) string {
	lines := bytes.Split(tl, []byte("\n"))
	for _, raw := range slices.Backward(lines) {
		var l struct{ Step, Commit string }
		if json.Unmarshal(raw, &l) == nil && l.Step == "pushed" && l.Commit != "" {
			return l.Commit
		}
	}
	return ""
}

// commitsAfter are the commits of the pull request after the newest push,
// oldest first, as "<sha8> <author> <subject>": a list that holds the
// push ends there, one that does not (the push is no longer in the branch)
// lists none. Forgejo lists newest first; one that lists oldest first is
// read as it comes.
func commitsAfter(commits []PRCommit, head, pushed string, scrub *redact.Scrubber) []string {
	if n := len(commits); n > 1 && commits[n-1].SHA == head {
		commits = slices.Clone(commits)
		slices.Reverse(commits)
	}
	i := slices.IndexFunc(commits, func(c PRCommit) bool { return c.SHA == pushed })
	if i <= 0 {
		return nil
	}
	var out []string
	for _, c := range slices.Backward(commits[:i]) {
		out = append(out, scrub.String(fmt.Sprintf("%s %s %s", c.SHA[:min(8, len(c.SHA))], c.Author(), firstLineCut(c.Commit.Message))))
	}
	return out
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
// one (RetryID). An open one carries its plan mark, its status's start
// and, with a case at the human (a monitor flag's aside, which the card's run
// goes on beside), its problem and when the case opened.
func (r *Runner) depFacts(ctx context.Context, id string) ([]DepFact, error) {
	deps, err := r.store.OpenDependencies(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []DepFact
	for _, d := range deps {
		fact := DepFact{TaskID: d.TaskID, Issue: d.Issue, Status: d.Status}
		if !Final(d.Status) {
			dep, _, err := r.store.Task(ctx, d.TaskID)
			if err != nil {
				return nil, err
			}
			fact.Plan = dep.Track == policy.TrackPlan
			started, err := r.store.TaskEventsOfKind(ctx, d.TaskID, EventTaskStatus, 1)
			if err != nil {
				return nil, err
			}
			if len(started) == 1 {
				fact.Since = &started[0].At
			}
			cases, err := r.store.OpenCasesOf(ctx, d.TaskID)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(cases, func(c Case) bool { return c.Status == CaseAtHuman && c.Kind != CaseMonitorFlag }); i >= 0 {
				fact.Ask, fact.Since = r.Scrub.String(textutil.Truncate(cases[i].Problem, 300)), &cases[i].OpenedAt
			}
		} else {
			events, err := r.store.TaskEvents(ctx, d.TaskID, cardDigestEvents)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventTaskEnded }); i >= 0 {
				why, _ := events[i].Detail["reason"].(string)
				fact.Why = r.Scrub.String(why)
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

// factsAtCase are the facts Act judges case c of card t on: the card's facts
// read now, at the case's head and model, with the case's kind. c may be the
// zero Case, an answer with none.
func (r *Runner) factsAtCase(ctx context.Context, t Task, c Case) (CaseFacts, error) {
	head, _ := c.Facts["head"].(string)
	model, _ := c.Facts["model"].(string)
	f, err := r.tripFacts(ctx, t, head, model)
	if err != nil {
		return CaseFacts{}, err
	}
	if err := r.atCaseOf(ctx, &f, c); err != nil {
		return CaseFacts{}, err
	}
	f.RunGone = r.runGone(ctx, t)
	return f, nil
}

// runGoneStatuses are the statuses of a card whose workflow runs.
var runGoneStatuses = []string{"running", "in-review", "waiting-human", "waiting-deps"}

// runGone reports whether card t is live and runs a workflow of which none
// runs. A workflow that cannot be read is unknown: false, the error logged,
// and nothing else is told of it.
func (r *Runner) runGone(ctx context.Context, t Task) bool {
	if r.cfg.Workflows == nil || t.Kind == "human" || t.Kind == KindChat || !slices.Contains(runGoneStatuses, t.Status) {
		return false
	}
	alive, err := r.cfg.Workflows.Alive(ctx, t.ID)
	if err != nil {
		log.Printf("task %s: is its workflow alive: %s", t.ID, r.scrub(err.Error()))
		return false
	}
	return !alive
}
