package factoryd

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The actions a case is answered with: what every rung picks from, and
// what the human's buttons and the chat's tools do.
const (
	ActResume        = "resume"
	ActContinue      = "continue"
	ActHandover      = "handover" // args: model = same | stronger | cheaper
	ActReverify      = "reverify"
	ActRouteApproval = "route_approval"
	ActRerunChecks   = "rerun_checks"
	ActExtend        = "extend"        // args: sessions, agent_time_s, lines
	ActFund          = "fund"          // args: eur
	ActRaiseCeiling  = "raise_ceiling" // args: eur
	ActSuccessor     = "successor"
	ActRetry         = "retry" // args: task_id of the failed dependency
	ActReplan        = "replan"
	ActClose         = "close"
	ActWithdraw      = "withdraw"
	ActNote          = "note" // records a step and resolves nothing: a park, or keep waiting
)

// Resolution is one answer on a case: the action with its arguments, an
// optional draw that rides beside it, and a note.
type Resolution struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
	Fund   *policy.Draw   `json:"fund,omitempty"`
	Note   string         `json:"note,omitempty"`
}

// Action is one row of the action table.
type Action struct {
	Name  string
	Rungs [4]bool // which rungs may pick it; index 3 is the human
	// Applies is "" when the action applies to the facts, else why not, in
	// a sentence. The bounds of the draw an answer makes are drawWithin's,
	// by rung.
	Applies func(f CaseFacts, r Resolution) string
	InRun   bool // the run carries it out on case_resolved; false: Act runs it
}

// handoverModels are the rungs a handover names, against the session's.
var handoverModels = []string{"same", "stronger", "cheaper"}

// Actions is the action table, in the order Offered lists it.
var Actions = []Action{
	{Name: ActResume, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string { return sessionRoom(f) }},
	{Name: ActContinue, Rungs: rungs(1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		switch {
		case strings.TrimSpace(r.Note) == "":
			return "the answer carries no note for the session"
		case f.Running:
			return ""
		}
		return sessionRoom(f)
	}},
	{Name: ActHandover, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		if why := sessionRoom(f); why != "" {
			return why
		}
		if model, _ := r.Args["model"].(string); !slices.Contains(handoverModels, model) {
			return fmt.Sprintf("a handover names its model as same, stronger or cheaper, not %q", model)
		}
		return ""
	}},
	{Name: ActRouteApproval, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case !f.PROpen:
			return "no pull request is open"
		case f.Checks != "green":
			return fmt.Sprintf("the checks on the head are %s, not green", cmp.Or(f.Checks, "unread"))
		case !f.ReviewApproved:
			return "the newest review of the head did not approve it"
		}
		return ""
	}},
	{Name: ActReverify, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case !f.PROpen:
			return "no pull request is open"
		case f.Head == "":
			return "no head reached the gates"
		case f.Running:
			return "a session is running"
		}
		return ""
	}},
	{Name: ActRerunChecks, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		switch {
		case !f.PROpen:
			return "no pull request is open"
		case f.Checks != "red":
			return fmt.Sprintf("the checks on the head are %s, not red", cmp.Or(f.Checks, "unread"))
		case f.RerunOnHead:
			return "the checks on this head were run again once already"
		}
		return ""
	}},
	{Name: ActExtend, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		d := extendDraw(r.Args)
		switch {
		case d.Sessions < 0 || d.AgentTime < 0:
			return "an extend takes no part below 0"
		case d.Sessions == 0 && d.AgentTime == 0:
			return "an extend needs sessions or agent time above 0"
		case boughtNothing(f):
			return boughtNothingText(f)
		}
		return ""
	}},
	{Name: ActFund, Rungs: rungs(1, 2, 3), InRun: true, Applies: func(f CaseFacts, r Resolution) string {
		switch eur, _ := r.Args["eur"].(float64); {
		case !(eur > 0) || math.IsInf(eur, 1):
			return "a fund needs an amount above 0 EUR"
		case boughtNothing(f):
			return boughtNothingText(f)
		}
		return ""
	}},
	{Name: ActRaiseCeiling, Rungs: rungs(2, 3), Applies: func(f CaseFacts, r Resolution) string {
		switch eur, _ := r.Args["eur"].(float64); {
		case !(eur > 0) || math.IsInf(eur, 1):
			return "a raise of the ceiling needs an amount above 0 EUR"
		case Cents(eur) > Cents(f.PoolLeft.EUR):
			return fmt.Sprintf("the escalation pool leaves %.2f EUR this month and the raise is %.2f EUR", f.PoolLeft.EUR, eur)
		}
		return ""
	}},
	{Name: ActSuccessor, Rungs: rungs(0, 1, 2, 3), InRun: true, Applies: func(f CaseFacts, _ Resolution) string {
		if !f.PROpen && f.Head == "" {
			return "no pull request is open and no head reached the gates"
		}
		return ""
	}},
	{Name: ActRetry, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, r Resolution) string {
		id, _ := r.Args["task_id"].(string)
		if id == "" {
			if slices.ContainsFunc(f.Dependencies, func(d DepFact) bool { return retryRefusal(d) == "" }) {
				return ""
			}
			return "no failed dependency is open to a retry"
		}
		i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return d.TaskID == id })
		if i < 0 {
			return "the card does not depend on " + id
		}
		return retryRefusal(f.Dependencies[i])
	}},
	{Name: ActReplan, Rungs: rungs(0, 1, 2, 3), Applies: func(f CaseFacts, _ Resolution) string {
		if f.Plan == "" && f.Running {
			return "a session is running on this direct card"
		}
		return ""
	}},
	{Name: ActClose, Rungs: rungs(2, 3), Applies: applyAlways},
	{Name: ActWithdraw, Rungs: rungs(0, 1, 2), Applies: applyAlways},
	{Name: ActNote, Rungs: rungs(1, 2, 3), Applies: func(_ CaseFacts, r Resolution) string {
		if strings.TrimSpace(r.Note) == "" {
			return "the answer carries no note"
		}
		return ""
	}},
}

// rungs marks the rungs of in a row's Rungs.
func rungs(of ...int) [4]bool {
	var out [4]bool
	for _, n := range of {
		out[n] = true
	}
	return out
}

func applyAlways(CaseFacts, Resolution) string { return "" }

// ActionByName is the row of the action table named name.
func ActionByName(name string) (Action, bool) {
	i := slices.IndexFunc(Actions, func(a Action) bool { return a.Name == name })
	if i < 0 {
		return Action{}, false
	}
	return Actions[i], true
}

// probes are the smallest answers Offered tries the actions that need
// arguments with: one session, one cent, the same model.
var probes = map[string]map[string]any{
	ActHandover: {"model": "same"}, ActExtend: {"sessions": 1}, ActFund: {"eur": 0.01}, ActRaiseCeiling: {"eur": 0.01},
}

// Offered lists the actions rung may take on facts f, in table order: its
// rows whose Applies holds and whose draw fits rung's bounds, tried with
// the smallest arguments (probes) where an action needs them.
func Offered(f CaseFacts, rung int) []Action {
	var out []Action
	for _, a := range Actions {
		if a.refusal(f, rung, Resolution{Action: a.Name, Args: probes[a.Name]}) == "" {
			out = append(out, a)
		}
	}
	return out
}

// refusal is why rung may not answer f with r by a, "" when it may: a's
// rungs, its Applies, and the draw r makes, which a draw that bought
// nothing refuses and drawWithin bounds by rung.
func (a Action) refusal(f CaseFacts, rung int, r Resolution) string {
	if rung < 0 || rung >= len(a.Rungs) || !a.Rungs[rung] {
		return fmt.Sprintf("rung %d may not pick %s", rung, a.Name)
	}
	if why := a.Applies(f, r); why != "" {
		return why
	}
	d, toBudget := r.draw()
	switch {
	case d.IsZero():
		return ""
	case d.Sessions < 0 || d.AgentTime < 0 || !(d.EUR >= 0) || math.IsInf(d.EUR, 1):
		return "the draw has a part below 0 or an endless one"
	case boughtNothing(f):
		return boughtNothingText(f)
	}
	return drawWithin(f, rung, d, toBudget)
}

// draw is what r draws from the pool, an extend's sessions and agent time,
// a fund's money and the draw beside the action, and whether money goes
// to the card's budget, which its ceiling bounds.
func (r Resolution) draw() (policy.Draw, bool) {
	var d policy.Draw
	switch r.Action {
	case ActExtend:
		d = extendDraw(r.Args)
	case ActFund:
		d.EUR, _ = r.Args["eur"].(float64)
	}
	if r.Fund != nil {
		d = d.Plus(*r.Fund)
	}
	return d, d.EUR != 0
}

// extendDraw is the draw of an extend's args: its sessions and its agent
// time in seconds. Its lines ride free.
func extendDraw(args map[string]any) policy.Draw {
	sessions, _ := toInt(args["sessions"])
	seconds, _ := toInt(args["agent_time_s"])
	return policy.Draw{Sessions: sessions, AgentTime: time.Duration(seconds) * time.Second}
}

// sessionRoom is why f leaves no room to open a session, "" when it has
// it: none running, an attempt under the cap, the agent time a session
// needs (minSession, or the whole cap when it is shorter), and the money
// an opening needs.
func sessionRoom(f CaseFacts) string {
	limit, left := time.Duration(f.AgentTimeCapS)*time.Second, time.Duration(f.AgentTimeCapS-f.AgentTimeS)*time.Second
	switch need := min(minSession, limit); {
	case f.Running:
		return "a session is running"
	case f.Attempts >= f.SessionCap:
		return fmt.Sprintf("the session cap of %d is reached", f.SessionCap)
	case left <= 0:
		return fmt.Sprintf("the agent time cap of %s is reached", minutesWord(limit))
	case left < need:
		return fmt.Sprintf("the agent time cap of %s leaves %s and a session needs %s", minutesWord(limit), minutesWord(left), minutesWord(need))
	case Cents(f.LeftEUR) < Cents(f.FloorEUR):
		return fmt.Sprintf("%.2f EUR is left and an opening needs %.2f EUR", f.LeftEUR, f.FloorEUR)
	}
	return ""
}

// retryRefusal is why a retry does not take dependency d, "" when it does.
func retryRefusal(d DepFact) string {
	switch {
	case d.Status != "failed":
		return fmt.Sprintf("dependency #%d is %s, not failed", d.Issue, d.Status)
	case d.Replaced != "":
		return fmt.Sprintf("dependency #%d was retried as %s already", d.Issue, d.Replaced)
	case !retryableEnd(d.Why):
		return fmt.Sprintf("dependency #%d ended for good: %s", d.Issue, digestText(d.Why))
	}
	return ""
}

// boughtNothing reports the design's refusal of a draw: a draw happened, no push
// since, the head and the checks unchanged since it.
func boughtNothing(f CaseFacts) bool {
	return f.LastDrawAt != nil && !f.PushedSinceDraw && f.DrawHead == f.Head && f.DrawChecks == f.Checks
}

// boughtNothingText is boughtNothing's refusal.
func boughtNothingText(f CaseFacts) string {
	return "the last draw at " + f.LastDrawAt.UTC().Format("15:04") + " UTC bought nothing: no push since and the head and the checks are unchanged"
}

// drawWithin reports why draw d does not fit rung's bounds on f: the pool for
// rungs 0 to 2, the card bound for rungs 0 and 1, the ceiling for fund.
func drawWithin(f CaseFacts, rung int, d policy.Draw, toBudget bool) string {
	switch {
	case rung <= 1 && !fitsIn(d, f.CardDrawLeft):
		return fmt.Sprintf("the card's draw bound leaves %s: rung 2 or the human may draw more", drawText(f.CardDrawLeft))
	case rung <= 2 && !fitsIn(d, f.PoolLeft):
		return fmt.Sprintf("the escalation pool leaves %s this month: only the human draws more", drawText(f.PoolLeft))
	case toBudget && Cents(f.BudgetEUR)+Cents(d.EUR) > Cents(f.CeilingEUR):
		return fmt.Sprintf("%.2f EUR more takes the budget of %.2f EUR past the card's ceiling of %.2f EUR: raise the ceiling first",
			d.EUR, f.BudgetEUR, f.CeilingEUR)
	}
	return ""
}

// fitsIn is policy.Draw.Fits with money compared in cents, as the store
// keeps it.
func fitsIn(d, in policy.Draw) bool {
	return d.Sessions <= in.Sessions && d.AgentTime <= in.AgentTime && Cents(d.EUR) <= Cents(in.EUR)
}

// drawText says draw d's three parts.
func drawText(d policy.Draw) string {
	return fmt.Sprintf("%s, %s of agent time and %.2f EUR", textutil.Plural(d.Sessions, "session", "sessions"), minutesWord(d.AgentTime), d.EUR)
}
