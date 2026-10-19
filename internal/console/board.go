package console

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

var funcs = template.FuncMap{
	// markup is the writer's markdown as HTML, for the drawer's question
	// text; askline is a text's first line, what a card's face shows
	"markup": markup, "askline": askline,
	"eur4": func(v float64) string { return fmt.Sprintf("%.4f", v) },
	// away reports whether a link leaves the console, so it opens its own
	// tab while a console path stays in this one
	"away": func(href string) bool { return !strings.HasPrefix(href, "/") },
	// servers is n with the noun in its number: 1 server, 2 servers
	"servers": func(n int) string {
		if n == 1 {
			return "1 server"
		}
		return fmt.Sprintf("%d servers", n)
	},
	// eur2 is money to the cent, as a card's face reads spend against budget
	"eur2": func(v float64) string { return fmt.Sprintf("\u20ac%.2f", v) },
	// the metrics page's rows (C30): the deck's formatters, the weeks'
	// spark, and the window-move subtraction
	"mDur": mDur, "mDurMove": mDurMove, "mPct": mPct, "mPctMove": mPctMove,
	"mEur": mEur, "mEurMove": mEurMove, "mCount": mCount, "mCountMove": mCountMove,
	"mK": mK, "mDec": mDec, "mDecMove": mDecMove, "mMix": mMix, "mDiff": mDiff,
	"mChat": mChat, "msub": msub, "sparkSVG": sparkSVG,
	// accents are the colour labels a writer may put on a card
	"accents": func() []string { return factoryd.Accents },
	// maxAnswer is the answer box's bound (answerFits)
	"maxAnswer": func() int { return factoryd.MaxAnswer },
	// lease renders minutes as whole hours when they are
	"lease": func(minutes int) string {
		if minutes%60 == 0 {
			return fmt.Sprintf("%d h", minutes/60)
		}
		return fmt.Sprintf("%d min", minutes)
	},
	"utc": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	// ago and schedule word a cluster job's last runs and cron
	"ago":      jobAge,
	"schedule": jobSchedule,
	// until words a lease's end from either side of now, as ago words a past run
	"until": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		d := time.Until(t)
		if d > 0 {
			return "in " + coarse(d)
		}
		return coarse(-d) + " ago"
	},
	// pct is spend of cap in whole percent, 0 without a cap; gaugeClass
	// is the gauge's class: warn from 80 percent, over past the cap
	"pct":    pct,
	"pct100": func(spent, cap float64) int { return min(pct(spent, cap), 100) },
	// initial is a member's avatar letter
	"initial": func(s string) string { return strings.ToUpper(string([]rune(s)[:min(1, len([]rune(s)))])) },
	// switch is whether the top bar shows the project select: the board
	// page's data has the projects, the other pages' has none
	"switch": func(d any) bool { b, ok := d.(boardData); return ok && len(b.Projects) > 0 },
	// cur marks the bar's destination whose page is open (C23): the open
	// page's word alone carries the cyan
	"cur": func(page, dest string) template.HTMLAttr {
		if page == dest {
			return ` aria-current="page"`
		}
		return ""
	},
	"gaugeClass": func(spent, cap float64) string {
		switch {
		case cap > 0 && spent > cap:
			return " over"
		case pct(spent, cap) >= 80:
			return " warn"
		}
		return ""
	},
	"inc":             func(i int) int { return i + 1 },
	"join":            strings.Join,
	"verdictForm":     func(id int64, csrf, back string) verdictForm { return verdictForm{id, csrf, back} },
	"alertSupervisor": alertSupervisor,
}

// alertSupervisor is what the supervisor did on a machine alert; an
// escalation's detail stays private.
func alertSupervisor(a factoryd.Alert) supervised {
	if a.Kind == factoryd.AlertEscalation {
		return supervised{}
	}
	return supervisedBy(a.Detail["action"], a.Detail["diagnosis"], a.Detail["recommendation"])
}

// supervised is a supervisor's action and diagnosis, for the "supervised"
// template.
type supervised struct{ Action, Diagnosis string }

func supervisedBy(action, diagnosis, recommendation any) supervised {
	s := supervised{}
	s.Action, _ = action.(string)
	s.Diagnosis, _ = diagnosis.(string)
	if rec, _ := recommendation.(string); rec != "" {
		s.Diagnosis += " Recommendation: " + rec
	}
	return s
}

// verdictForm is the "verdict" template's data: an alert's two buttons.
type verdictForm struct {
	ID         int64
	CSRF, Back string
}

// The project screen's four columns (ruling 255 as C8 amends it), a view
// over the task states and never a state of their own: In progress holds
// running, closing and in review, the stage word on the card; the lane
// and the fine state are chips on the card.
const (
	colNeedsYou = iota
	colQueued
	colProgress
	colDone
)

var columnTitles = []string{"Needs you", "Queued", "In progress", "Done"}

// fiveColumn is t's column for viewer, -1 for a status none holds: a card
// waiting on its author needs the viewer when they are the author, else
// it waits like a queued one; for no viewer (the pulse's) it needs a person.
func fiveColumn(t factoryd.Task, viewer string) int {
	switch t.Status {
	case "waiting-human":
		return colNeedsYou
	case "waiting-poster":
		if t.Author == viewer || viewer == "" {
			return colNeedsYou
		}
		return colQueued
	case "backlog", "queued", "waiting-deps":
		return colQueued
	case "running", "closing", "in-review":
		return colProgress
	case "done", "failed", "closed":
		return colDone
	}
	return -1
}

// waitingSince is when bt began to wait, which ranks the Needs you column
// oldest first: its escalation, else the gate it waits in, else its
// filing.
func waitingSince(bt factoryd.BoardTask) time.Time {
	switch {
	case !bt.EscalatedAt.IsZero():
		return bt.EscalatedAt
	case bt.GateSince != nil:
		return *bt.GateSince
	}
	return bt.CreatedAt
}

// arrivedAt is when k arrived in its column (Sebastian, 2026-10-14, the
// default order, newest first): its admission in progress, its finishing
// when final, else its filing. Needs you stays oldest waiting first.
func arrivedAt(k card) time.Time {
	switch {
	case slices.Contains([]string{"running", "closing", "in-review"}, k.Status) && k.AdmittedAt != nil:
		return *k.AdmittedAt
	case factoryd.Final(k.Status) && k.FinishedAt != nil:
		return *k.FinishedAt
	}
	return k.CreatedAt
}

// arrange applies the column menus (C13) to the four columns after the
// find: each of Queued, In progress and Done takes its order, then its
// show, and gets its menu; the show narrows below the whole count the
// find left, so the head reads "n of m".
func arrange(d *boardData, f boardFilter) {
	for _, m := range colMenus {
		col, cf := &d.Columns[m.Col], f.col(m.Name)
		col.Cards = ordered(col.Cards, cf.Order)
		if cf.Only != "" {
			col.Cards = slices.DeleteFunc(col.Cards, func(k card) bool { return !shows(k, cf.Only) })
		}
		col.Menu = menuOf(m, cf, f, d.cols()[m.Col].Cards)
	}
}

// ordered is cards in the menu's order, a clone: arrival newest first by
// default, oldest first, next up (the store's rank), by stage (planning,
// building, verifying) or by spend, the last two newest first among
// equals.
func ordered(cards []card, order string) []card {
	out := slices.Clone(cards)
	newest := func(a, b card) int { return arrivedAt(b).Compare(arrivedAt(a)) }
	switch order {
	case "next":
	case "oldest":
		slices.SortStableFunc(out, func(a, b card) int { return arrivedAt(a).Compare(arrivedAt(b)) })
	case "stage":
		slices.SortStableFunc(out, newest)
		slices.SortStableFunc(out, func(a, b card) int { return cmp.Compare(stageRank(a.Stage), stageRank(b.Stage)) })
	case "spend":
		slices.SortStableFunc(out, newest)
		slices.SortStableFunc(out, func(a, b card) int { return cmp.Compare(b.SpentEUR, a.SpentEUR) })
	default:
		slices.SortStableFunc(out, newest)
	}
	return out
}

func stageRank(stage string) int {
	switch stage {
	case "planning":
		return 0
	case "building":
		return 1
	case "verifying":
		return 2
	}
	return 3
}

// shows is whether k stays under the menu's show only.
func shows(k card, only string) bool {
	switch only {
	case "blocked":
		return k.Blocked > 0 || k.WaitingOn != ""
	case "exceptional":
		return k.Stale || k.Provider || k.NearBudget || k.Retrying || k.Held
	case "failed":
		return k.Status == "failed"
	}
	if n, ok := strings.CutPrefix(only, "plan:"); ok {
		return strconv.FormatInt(k.PlanIssue, 10) == n
	}
	return true
}

// menuOf is the column's menu for the head: Queued's shows gain one per
// plan among whole, the column's cards before the show.
func menuOf(m menuSpec, cf colFilter, f boardFilter, whole []card) *colMenu {
	onlys := slices.Clone(m.Onlys)
	if m.Name == "queued" {
		var plans []int64
		for _, k := range whole {
			if k.PlanIssue > 0 && !slices.Contains(plans, k.PlanIssue) {
				plans = append(plans, k.PlanIssue)
			}
		}
		slices.Sort(plans)
		for _, p := range plans {
			onlys = append(onlys, option{fmt.Sprintf("plan:%d", p), fmt.Sprintf("from plan #%d", p)})
		}
	}
	var state []string
	if cf.Order != "" {
		state = append(state, label(m.Orders, cf.Order))
	}
	if cf.Only != "" {
		state = append(state, label(onlys, cf.Only))
	}
	clear := f
	clear.setCol(m.Name, colFilter{})
	return &colMenu{Name: m.Name, OnlyName: m.Name + "-only", Order: cf.Order, Only: cf.Only, Orders: m.Orders, Onlys: onlys,
		State: strings.Join(state, " · "), ClearQuery: clear.query()}
}

// label is the choice's label, "from plan #n" for a plan the column no
// longer holds.
func label(options []option, value string) string {
	for _, o := range options {
		if o.Value == value {
			return o.Label
		}
	}
	return "from plan #" + strings.TrimPrefix(value, "plan:")
}

// find narrows the columns to the terms of q (C13): a word matches what
// the face says (the ref, the title, the state and stage words, the
// activity line, the chips, an item's headline), a key:value term a fact
// behind the face (plan:558, stage:verifying, status:failed, is:failed
// and the other exceptional states, by:sebastian, model:coding-cheap,
// pr:612), and every term must match. Each column keeps its whole count;
// the columns given stay as they were, for the column menus.
func find(cols []column, q string) []column {
	terms := strings.Fields(strings.ToLower(q))
	out := make([]column, len(cols))
	for i, col := range cols {
		col.Total = col.Count()
		if len(terms) > 0 {
			var cards []card
			for _, k := range col.Cards {
				if found(k, nil, terms) {
					cards = append(cards, k)
				}
			}
			var needs []need
			for _, n := range col.Needs {
				if found(n.Card, n.Approval, terms) {
					needs = append(needs, n)
				}
			}
			col.Cards, col.Needs = cards, needs
		}
		out[i] = col
	}
	return out
}

// found is whether card k, or the approval item a, matches every term.
func found(k card, a *approvalCard, terms []string) bool {
	text := strings.ToLower(faceWords(k, a))
	for _, t := range terms {
		if key, val, ok := strings.Cut(t, ":"); ok && key != "" {
			if match, known := fact(k, a, key, val); known {
				if !match {
					return false
				}
				continue
			}
		}
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}

// faceWords is what the face says, for a word of the find to match.
func faceWords(k card, a *approvalCard) string {
	if a != nil {
		switch a.Kind {
		case factoryd.ApprovalMerge:
			w := []string{fmt.Sprintf("merge pr #%d", a.PRNumber), a.Merge.RiskClass + " risk", a.TaskID}
			if a.Merge.Migration {
				w = append(w, "migration")
			}
			return strings.Join(w, " ")
		case factoryd.ApprovalPlan:
			return fmt.Sprintf("plan #%d %s %s", a.Issue, a.PlanSummary, a.TaskID)
		}
		return strings.Join([]string{"resource request", a.Resource.ServerType, a.Resource.Purpose, a.TaskID}, " ")
	}
	w := []string{k.ID, k.Title, k.Status, k.Stage, k.Activity, k.StaleText, k.Reason, k.Children}
	if k.IssueNumber > 0 {
		w = append(w, fmt.Sprintf("#%d", k.IssueNumber))
	}
	if k.PRNumber > 0 {
		w = append(w, fmt.Sprintf("pr #%d", k.PRNumber))
	}
	if k.PlanIssue > 0 {
		w = append(w, fmt.Sprintf("from plan #%d", k.PlanIssue))
	}
	if k.Routine() {
		w = append(w, "routine")
	}
	if k.Blocked > 0 {
		w = append(w, fmt.Sprintf("blocked by %d %s", k.Blocked, k.BlockedOn))
	}
	if k.WaitingOn != "" {
		w = append(w, "waiting on "+k.WaitingOn)
	}
	if k.Checks != "" {
		w = append(w, k.Checks+" checks")
	}
	if k.NearBudget {
		w = append(w, "near its budget")
	}
	if k.Retrying {
		w = append(w, "retrying")
	}
	if k.Held {
		w = append(w, heldWords)
	}
	if k.Provider {
		w = append(w, providerWords)
	}
	if k.Stale {
		w = append(w, "stale")
	}
	if k.Status == "waiting-poster" {
		w = append(w, "waiting on the author")
	}
	if k.Kind == "human" {
		w = append(w, "human task")
	}
	return strings.Join(w, " ")
}

// fact is whether k, or the approval item a, has the fact key:val; known
// is false for a key the find does not have, which then reads as a word.
func fact(k card, a *approvalCard, key, val string) (match, known bool) {
	val = strings.TrimPrefix(val, "#")
	eq := func(s string) (bool, bool) { return strings.EqualFold(s, val), true }
	switch key {
	case "is":
		return isState(k, val), true
	case "plan":
		return eq(strconv.FormatInt(k.PlanIssue, 10))
	case "stage":
		return eq(k.Stage)
	case "status":
		return eq(k.Status)
	case "by":
		return eq(k.Author)
	case "model":
		return eq(k.ModelAlias)
	case "pr":
		n := k.PRNumber
		if a != nil {
			n = a.PRNumber
		}
		return eq(strconv.FormatInt(n, 10))
	}
	return false, false
}

// isState is whether k is in the exceptional state v of is:v.
func isState(k card, v string) bool {
	switch v {
	case "failed":
		return k.Status == "failed"
	case "blocked":
		return k.Blocked > 0 || k.WaitingOn != ""
	case "stale":
		return k.Stale
	case "provider":
		return k.Provider
	case "near":
		return k.NearBudget
	case "retrying":
		return k.Retrying
	case "held":
		return k.Held
	}
	return false
}

// stateColumns are the wide view's columns after the lanes, in board
// order; closing counts as running.
var stateColumns = []struct{ Title, Status string }{
	{"Running", "running"}, {"Waiting on a human", "waiting-human"}, {"Waiting on the author", "waiting-poster"},
	{"In review", "in-review"}, {"Done", "done"}, {"Failed", "failed"}, {"Closed", "closed"},
}

type card struct {
	factoryd.Task
	IssueURL string
	PRURL    string
	Reason   string   // an escalation's reason
	Choices  []string // the answers its agent offered, a button each
	Capped   bool     // an escalated card at its session cap, which an answer grants a session
	// Ask says the card waits on an answer: an intake row holds its ask.
	// Without one a human card that waits offers the factory or a close
	// (roadmap 1131).
	Ask bool
	// Question is the escalation's five-part question (the question
	// format): the ask as the headline, the why under it, its buttons by
	// value, and the info and recommendation behind the fold. nil on a
	// card whose ask no template shaped.
	Question *factoryd.HumanQuestion
	// AnswersPending counts the trusted answers the card's workflow has
	// not taken: one that holds some cannot process another, so its
	// answer box yields to the liveness warning.
	AnswersPending int
	// Fund is the approver's Fund action on an escalated card whose
	// remaining budget cannot fund a call (ruling 322's floor), which
	// raises its budget (roadmap 210).
	Fund *fundForm
	// AcceptanceClose is the approver's Close on a waiting card whose
	// event trail shows one of its plan's acceptance commands run green
	// in a session (roadmap 208): the work is its own evidence, so the
	// close ends it done where an answer would buy the next session.
	// nil hides the button; Evidence is the newest green run, the button's
	// title.
	AcceptanceClose *acceptanceCloseForm
	// Total is the task's time so far, or in all once it finished; GateAge
	// how long it has been in its gate, which GateText names as the card
	// reads it.
	Total, GateAge string
	// Stage is the work stage of a card in progress (C8): planning on a
	// plan-lane card, building on a coder's, verifying in review, "" in
	// any other status. StageAge is how long the card has been in it.
	Stage, StageAge    string
	GateText           string
	EscalationAlert    int64  // the escalation's open alert
	WaitingOn          string // a card in waiting-deps: the issues it waits on
	Badge              string // the card's starting lane, in a column after the lanes
	PlanIssue          int64  // a plan's card: the plan's issue
	Moves              []move // a backlog card: the starting lanes the viewer may move it into
	Plan               *planCards
	Retry              *retry // a failed plan card's Retry, for an approver who may move it into ready
	Go                 *goForm
	Activity           string // the task's newest event as one line, or the escalation's question
	WaitingForActivity bool   // the activity line stands for the wait's reason
	// Title is the face's first line: human_summary when a writer set
	// one, the plan key of a planned card, else the objective's first
	// paragraph, the title as the issue was filed.
	Title string
	// StatusClass is the pill's class, one hue per state.
	StatusClass string
	// Checks is "n/n" from the task's newest checks step, "" on a card
	// without a pull request, a final one, or one whose timeline holds none.
	Checks string
	// NearBudget is spend at 80 percent of the budget or more.
	NearBudget bool
	// CallsText is the task's call rows summed beside the spend line
	// (roadmap 206): its tokens to date and the modeled pay-as-you-go
	// cost of them, "" on a task without calls.
	CallsText string
	// Stale is a card without an event for the stale window: an amber
	// ring, and these words on its activity line.
	Stale     bool
	StaleText string
	// stepAt is the card's newest timeline step's time: the work its own
	// session last reported, an input to the stale ring beside the newest
	// event.
	stepAt time.Time
	// Children roll up an accepted plan: n running, n done, n waiting.
	Children string
	// Blocked is how many cards this one depends on that are not done;
	// BlockedOn names their issues in its title attribute ("#3, #5").
	Blocked   int
	BlockedOn string
	// Retrying is a card whose newest session ended for a retry and whose
	// next has not started; Held one whose next session waits for a roll
	// (held); Provider a running one whose next session waits on the model
	// provider (onProvider); Fine are its fine states as chips.
	Retrying bool
	Held     bool
	Provider bool
	Fine     []fine
	// MayWriteSummary is whether the viewer writes the task's project, so
	// the expanded view carries the summary and accent forms; CSRF is the
	// session's token for them.
	MayWriteSummary bool
	CSRF            string
	// Filter is the board's filter the card's forms carry back so an
	// action keeps the view.
	Filter boardFilter
	// Place is where the board puts the card (boardCard).
	Place string
}

// Closes: the face offers its viewer, a writer, Close: a card in the
// backlog or waiting on a human, or failed without an issue, which has no
// other way out.
func (k card) Closes() bool {
	return k.MayWriteSummary && (slices.Contains([]string{"backlog", "waiting-human"}, k.Status) || k.IssueNumber == 0 && k.Status == "failed")
}

// Routine: a routine's run filed the card, which wears the chip (C16).
func (k card) Routine() bool { return k.Source == factoryd.SourceRoutine }

// fine is a card's fine state as a chip in the pill's hues.
type fine struct{ Words, Class string }

// fineStates are what k's column and pill leave unsaid: retrying; in the
// five columns waiting on the author, which the wide view has a column
// for; and stale. Failed and closed are the pill's own words.
func fineStates(k card, wide bool) []fine {
	var out []fine
	switch {
	case k.Held:
		out = append(out, fine{heldWords, "st-waiting"})
	case k.Provider:
		out = append(out, fine{providerWords, "st-waiting"})
	case k.Retrying:
		out = append(out, fine{"retrying", "st-running"})
	case !wide && k.Status == "waiting-poster":
		out = append(out, fine{"waiting on the author", "st-waiting hollow"})
	}
	if k.Stale {
		out = append(out, fine{"stale", "st-stale"})
	}
	return out
}

// statusClasses are the pill's classes, one hue per state: running the
// accent blue, a wait amber, review violet, done green, failed red, closed
// grey; waiting-poster wears the hollow pill. closing counts as running.
var statusClasses = map[string]string{
	"running": "st-running", "closing": "st-running", "queued": "st-queued",
	"waiting-human": "st-waiting", "waiting-poster": "st-waiting hollow",
	"in-review": "st-review", "done": "st-done", "failed": "st-failed",
	"closed": "st-closed", "backlog": "st-backlog", "plan": "st-plan",
	"waiting-deps": "st-waiting",
}

func statusClass(status string) string { return cmp.Or(statusClasses[status], "st-other") }

// nearBudget: spent is 80 percent of budget or more, both in the whole
// cents the face shows (0.8 * 3.00 is 2.4000000000000004 in floats, so
// 2.40 of 3.00 would stay muted): the spend turns prominent with the
// warning sign and the words "near its budget".
func nearBudget(spent, budget float64) bool {
	cents := func(eur float64) int64 { return int64(math.Round(eur * 100)) }
	return cents(spent)*5 >= cents(budget)*4
}

// newestChecksCount is the task's newest checks step: passed over total of
// its jobs; "" when the steps hold none or it named no counts.
func newestChecksCount(steps []map[string]any) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if s, _ := steps[i]["step"].(string); s != "checks" {
			continue
		}
		jobs, _ := steps[i]["jobs"].([]any)
		var passed, ran int
		for _, j := range jobs {
			job, _ := j.(map[string]any)
			switch st, _ := job["status"].(string); st {
			case "skipped":
				continue
			case "success":
				passed++
			}
			ran++
		}
		if ran == 0 {
			return ""
		}
		return fmt.Sprintf("%d/%d", passed, ran)
	}
	return ""
}

// retrying: the newest session of steps ended for a retry (the
// supervisor's retry_stronger) and the next has not started yet.
func retrying(steps []map[string]any) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		switch steps[i]["step"] {
		case "session_start":
			return false
		case "session_end":
			outcome, _ := steps[i]["outcome"].(string)
			return strings.HasPrefix(outcome, "retry")
		}
	}
	return false
}

// heldWords are the chip of a card whose next session waits for a roll.
const heldWords = "waiting for a roll"

// held: the newest held step of steps, a session waiting while a roll
// holds sessions, has no session_start after it.
func held(steps []map[string]any) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		switch steps[i]["step"] {
		case "session_start":
			return false
		case "held":
			return true
		}
	}
	return false
}

// providerWords are the chip of a card whose next session waits on the
// model provider.
const providerWords = "waiting on the provider"

// onProvider: the newest provider_refused step of steps, a refused
// session's wait on the model provider, has no session_start or held
// step after it.
func onProvider(steps []map[string]any) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		switch steps[i]["step"] {
		case "session_start", "held":
			return false
		case factoryd.StepProviderRefused:
			return true
		}
	}
	return false
}

// newestStepOf is steps' newest step's time, the card's own work as the
// stale ring reads it; a step without a time is skipped, and none
// leaves the zero time.
func newestStepOf(steps []map[string]any) time.Time {
	for i := len(steps) - 1; i >= 0; i-- {
		at, err := time.Parse(time.RFC3339, fmt.Sprint(steps[i]["time"]))
		if err != nil {
			continue
		}
		return at
	}
	return time.Time{}
}

// checksTTL is how long what a card's face reads from its timeline is
// kept: the renders, polls and card swaps within it read it once.
const checksTTL = 30 * time.Second

// checks is a card's remembered checks count, retry, hold and wait on
// the provider, the newest timeline step's time, and when they were
// read.
type checks struct {
	count                    string
	retrying, held, provider bool
	stepAt                   time.Time
	at                       time.Time
}

// faceTimelines reads the newest checks step onto every card that can
// have one, a card with a pull request, and whether it is retrying or
// held onto every running card: neither final, at most once per checksTTL. A
// timeline that cannot be read keeps the card without the chips — the
// object store answers no body for a task that never wrote a timeline,
// and reading is best-effort here, never a reason to fail the board.
func (c *Console) faceTimelines(ctx context.Context, everyCard func(func(*card))) {
	everyCard(func(k *card) {
		if factoryd.Final(k.Status) || k.PRNumber == 0 && k.Status != "running" {
			return
		}
		got := c.timelineFace(ctx, k.ID)
		if k.PRNumber > 0 {
			k.Checks = got.count
		}
		k.Retrying, k.Held, k.Provider = got.retrying, got.held, got.provider && k.Status == "running"
		k.stepAt = got.stepAt
	})
}

// timelineFace is what task id's timeline says for its face, read at most
// once per checksTTL.
func (c *Console) timelineFace(ctx context.Context, id string) checks {
	c.mu.Lock()
	got, ok := c.checks[id]
	c.mu.Unlock()
	if ok && time.Since(got.at) < checksTTL {
		return got
	}
	got = checks{at: time.Now()}
	if steps, err := c.readTimeline(ctx, id); err != nil {
		log.Printf("console: timeline of %s for its checks: %v", id, err)
	} else {
		got.count, got.retrying, got.held, got.provider = newestChecksCount(steps), retrying(steps), held(steps), onProvider(steps)
		got.stepAt = newestStepOf(steps)
	}
	c.mu.Lock()
	for other, old := range c.checks {
		if time.Since(old.at) >= checksTTL {
			delete(c.checks, other)
		}
	}
	if c.checks == nil {
		c.checks = map[string]checks{}
	}
	c.checks[id] = got
	c.mu.Unlock()
	return got
}

// depsCount is how many issues a "#3, #5" list names (0 an empty one).
func depsCount(list string) int {
	if list == "" {
		return 0
	}
	return strings.Count(list, "#")
}

// planRollup is an accepted plan's children as one line: only the parts
// with cards in them, in the order running, done, waiting.
func planRollup(cards []card) string {
	var running, done, waiting int
	for _, k := range cards {
		switch {
		case k.Status == "running" || k.Status == "closing":
			running++
		case k.Status == "done":
			done++
		case strings.HasPrefix(k.Status, "waiting"):
			waiting++
		}
	}
	var parts []string
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", done))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", waiting))
	}
	return strings.Join(parts, " \u00b7 ")
}

// move is a backlog card's button into a starting lane. The board's filter
// rides on the form from the card's Filter.
type move struct{ Lane, Title, CSRF string }

// fundForm is an approver's Fund action beside a capped card's answer box:
// the stated amount raises the card's budget_eur (roadmap 210).
type fundForm struct{ CSRF string }

// acceptanceCloseForm is an approver's Close beside a waiting card's
// answer box: the close ends the card done on its acceptance green
// (roadmap 208), which the button's Evidence names.
type acceptanceCloseForm struct{ CSRF, Evidence string }

// retry is a failed plan card's Retry button.
type retry struct{ CSRF string }

// goForm is the pair on a human card that waits without an ask: Have the
// factory do it, which files the work as the factory's own card, and
// Close with note.
type goForm struct{ CSRF string }

// planCards are an accepted plan's cards and, for an approver while any
// is in the backlog, Start plan (CSRF set).
type planCards struct {
	Cards   []card
	Backlog int
	CSRF    string
}

type column struct {
	Title string
	Lane  string // a lane column's lane, where a card is dropped
	// Total is the column's count before the find narrowed it (C13); the
	// head reads "n of m" when they differ.
	Total int
	// Menu is the column's menu (C13) in the four columns: Queued, In
	// progress and Done have one, Needs you none.
	Menu  *colMenu
	Cards []card
	// Needs is the five columns' Needs you: what the wide view's human
	// lane holds and every card that needs the viewer, oldest first.
	Needs []need
	// Finished is a column of finished cards' key, the status it holds
	// (done for the four columns' Done, which holds all three): the col of
	// its next page and the name of its <key>-shown. Whole is how many
	// finished cards it has when Cards holds the newest pages of them, and
	// More the form of the next page, nil when Cards holds them all.
	Finished string
	Whole    int
	More     *more
	// Ghosts are the column's routines at its foot (C16), and
	// GhostsHidden those the board's routines choice hides.
	Ghosts       []ghost
	GhostsHidden int
}

// ghost is a routine as a card that is not yet there (C16): its schedule
// in words, its next run ("Mon 06:00 · in 20 min · human card"), its last
// ("last #781 done in 2 d"), a held run's words, the open card (Open) it
// runs, and its acts, offered to a writer (CSRF set) with the board's
// filter. Skipped says a Skip once is pending, which its act undoes.
type ghost struct {
	Project, Name, Title        string
	Words, NextWords, LastWords string
	Soon, Held, Running         bool
	Skipped                     bool
	HeldWords                   string
	Open                        int64
	Filter                      boardFilter
	CSRF                        string
	// Files is the config's side of what the routine files, Side the one
	// in force; Toggled says a live override set the side, so the
	// toggle's act is the undo.
	Files, Side string
	Toggled     bool
	// Drawer says the acts sit in the routine's drawer, which answers them
	// on its tab Tab.
	Drawer bool
	Tab    string
}

// Count is the number the column's head shows: its needs, else its
// finished cards when it pages them, else its cards.
func (c column) Count() int {
	switch {
	case len(c.Needs) > 0:
		return len(c.Needs)
	case c.Whole > 0:
		return c.Whole
	}
	return len(c.Cards)
}

// more is the form of a column's next page of finished cards: scrolled to
// or pressed, it asks GET /board/finished for the page after the card
// After names; without script it asks for the board showing Shown of the
// column's cards.
type more struct {
	Filter     boardFilter
	Key, Title string
	After      factoryd.Cursor
	Shown      int
}

// AfterAt is the arrival time After names.
func (m more) AfterAt() string { return m.After.At.UTC().Format(time.RFC3339Nano) }

// colMenu is a column's menu as its head renders it: its query names,
// the choices with the current one, the non-default choices' labels as
// the state line's words, and the board without this column's choices
// as the state line's clear link.
type colMenu struct {
	Name, OnlyName string
	Order, Only    string
	Orders, Onlys  []option
	State          string
	ClearQuery     string
}

// need is an item of the human lane, or of the Needs you column: an
// approval request, an escalated card with its answer form (CSRF, the
// verdict for an approver and the board to go back to), or a card that
// needs the viewer; Since is when it began to wait.
type need struct {
	Since     time.Time
	Approval  *approvalCard
	Card      card
	Escalated bool
	CSRF      string
	Approver  bool
	Back      string
	Suffix    string // on the ids of the item's fields: "-d" in the drawer, so the column's copy keeps its own
}

// Waiting is how long the item has waited for a member, in minutes or
// hours and minutes; empty when the item's start is unknown.
func (n need) Waiting() string {
	if n.Since.IsZero() {
		return ""
	}
	return minutes(time.Since(n.Since))
}

// viewWide is the board's wide view: the lanes as columns first, then the
// task states, as the board was before the five columns.
const viewWide = "wide"

// boardFilter is what the board was asked to show: the project and the
// view; every form on it carries it back, so an action keeps the view.
type boardFilter struct {
	Project, View string
	// Q is the find (C13): words and key:value terms that narrow every
	// column, kept by every form, the poll, the fragments and the stream.
	Q string
	// Queued, Progress and Done are the column menus' choices (C13),
	// read from queued, queued-only, progress, progress-only, done and
	// done-only in the query.
	Queued, Progress, Done colFilter
	// Shown is how many cards each column of finished cards shows at
	// least, by its key (<key>-shown): a board that loaded more of them
	// keeps them. Col and After ask for the page of Col after the card
	// After names (GET /board/finished). The query and the forms carry
	// none of the three.
	Shown map[string]int
	Col   string
	After factoryd.Cursor
	// Routines is the routines control's choice (C16): "" shows every
	// routine's ghost, within those whose next run is at most Within
	// Unit away, none none. Within (1 to 999) and Unit (days or hours)
	// are set with within alone.
	Routines string
	Within   int
	Unit     string
}

// periodUnits are the units of the routines control's period.
var periodUnits = map[string]time.Duration{"days": 24 * time.Hour, "hours": time.Hour}

// period is the routines control's period as a duration.
func (f boardFilter) period() time.Duration { return time.Duration(f.Within) * periodUnits[f.Unit] }

// Period is the routines control's period in words: "1 day", "2 hours".
func (f boardFilter) Period() string {
	if f.Within == 1 {
		return "1 " + strings.TrimSuffix(f.Unit, "s")
	}
	return strconv.Itoa(f.Within) + " " + f.Unit
}

// whole: the find or the Done column's show narrows the finished cards,
// so the board reads them all rather than a page.
func (f boardFilter) whole() bool { return f.Q != "" || !f.wide() && f.Done.Only != "" }

// colFilter is a column's order and show from its menu (C13): "" is the
// default, arrival newest first and every card.
type colFilter struct{ Order, Only string }

// option is a menu's choice: its query value and its label.
type option struct{ Value, Label string }

// menuSpec is a column menu: its query name, its column and its choices;
// the first of each is the default. Queued's shows gain "from plan #n"
// per plan with queued cards.
type menuSpec struct {
	Name   string
	Col    int
	Orders []option
	Onlys  []option
}

// colMenus are the three column menus; Needs you is always oldest
// waiting first and has none.
var colMenus = []menuSpec{
	{"queued", colQueued, []option{{"", "newest first"}, {"oldest", "oldest first"}, {"next", "next up"}}, []option{{"", "all"}, {"blocked", "blocked only"}}},
	{"progress", colProgress, []option{{"", "newest first"}, {"oldest", "oldest first"}, {"stage", "by stage"}, {"spend", "by spend"}}, []option{{"", "all"}, {"exceptional", "exceptional only"}}},
	{"done", colDone, []option{{"", "newest first"}, {"oldest", "oldest first"}}, []option{{"", "all"}, {"failed", "failed only"}}},
}

// col is the menu's choices for the column named name.
func (f boardFilter) col(name string) colFilter {
	switch name {
	case "queued":
		return f.Queued
	case "progress":
		return f.Progress
	case "done":
		return f.Done
	}
	return colFilter{}
}

func (f *boardFilter) setCol(name string, c colFilter) {
	switch name {
	case "queued":
		f.Queued = c
	case "progress":
		f.Progress = c
	case "done":
		f.Done = c
	}
}

// readCols reads the menus' choices and the routines control's with get,
// a value the menu does not offer, and all, reading as the default, and
// how many cards each column of finished cards shows. A number the
// routines control's period does not take reads as 1, a unit as days.
func readCols(f *boardFilter, get func(string) string) {
	switch get("routines") {
	case "none":
		f.Routines = "none"
	case "within":
		f.Routines, f.Within, f.Unit = "within", 1, "days"
		if n, err := strconv.Atoi(get("within")); err == nil && n >= 1 && n <= 999 {
			f.Within = n
		}
		if u := get("unit"); periodUnits[u] > 0 {
			f.Unit = u
		}
	}
	for _, key := range factoryd.FinalStatuses {
		if n, err := strconv.Atoi(get(key + "-shown")); err == nil && n > 0 {
			if f.Shown == nil {
				f.Shown = map[string]int{}
			}
			f.Shown[key] = n
		}
	}
	for _, m := range colMenus {
		c := colFilter{Order: get(m.Name), Only: get(m.Name + "-only")}
		if !slices.ContainsFunc(m.Orders, func(o option) bool { return o.Value == c.Order }) {
			c.Order = ""
		}
		if n, ok := strings.CutPrefix(c.Only, "plan:"); ok {
			if _, err := strconv.ParseInt(n, 10, 64); err != nil {
				c.Only = ""
			}
		} else if !slices.ContainsFunc(m.Onlys, func(o option) bool { return o.Value == c.Only }) {
			c.Only = ""
		}
		f.setCol(m.Name, c)
	}
}

// hidden is one field a form carries to keep the board's filter.
type hidden struct{ Name, Value string }

// Hidden are the filter's fields with a value, as a form's hidden
// inputs, but those named in except: the find's form leaves out q, a
// menu's form its own column's two, the routines control's form
// routines with its period.
func (f boardFilter) Hidden(except ...string) []hidden {
	fields := []hidden{{"project", f.Project}, {"view", f.View}, {"q", f.Q}}
	for _, m := range colMenus {
		c := f.col(m.Name)
		fields = append(fields, hidden{m.Name, c.Order}, hidden{m.Name + "-only", c.Only})
	}
	fields = append(fields, hidden{"routines", f.Routines})
	if f.Routines == "within" && !slices.Contains(except, "routines") {
		fields = append(fields, hidden{"within", strconv.Itoa(f.Within)}, hidden{"unit", f.Unit})
	}
	var out []hidden
	for _, h := range fields {
		if h.Value != "" && !slices.Contains(except, h.Name) {
			out = append(out, h)
		}
	}
	return out
}

// formFilter is the board's filter a form carried back.
func formFilter(r *http.Request) boardFilter {
	f := boardFilter{Project: r.PostFormValue("project"), View: r.PostFormValue("view"), Q: strings.TrimSpace(r.PostFormValue("q"))}
	readCols(&f, r.PostFormValue)
	return f
}

// urlFilter is the board's filter in the URL's query.
func urlFilter(r *http.Request) boardFilter {
	q := r.URL.Query()
	f := boardFilter{Project: q.Get("project"), View: q.Get("view"), Q: strings.TrimSpace(q.Get("q"))}
	readCols(&f, q.Get)
	return f
}

func (f boardFilter) wide() bool { return f.View == viewWide }

// query is the filter as a URL query, empty when it names nothing.
func (f boardFilter) query() string {
	q := url.Values{}
	if f.Project != "" {
		q.Set("project", f.Project)
	}
	if f.View != "" {
		q.Set("view", f.View)
	}
	if f.Q != "" {
		q.Set("q", f.Q)
	}
	for _, m := range colMenus {
		if c := f.col(m.Name); c.Order != "" {
			q.Set(m.Name, c.Order)
		}
		if c := f.col(m.Name); c.Only != "" {
			q.Set(m.Name+"-only", c.Only)
		}
	}
	if f.Routines != "" {
		q.Set("routines", f.Routines)
	}
	if f.Routines == "within" {
		q.Set("within", strconv.Itoa(f.Within))
		q.Set("unit", f.Unit)
	}
	if q.Encode() == "" {
		return ""
	}
	return "?" + q.Encode()
}

// statusLine is the shown project's state above the board (ruling 251),
// which the project agent reads too: its word, its cards working, those
// waiting on the model provider, what needs the viewer, its spend this
// UTC month (the chat's part apart) against its month cap, and its GitHub
// export's state word.
type statusLine struct {
	Project string
	// Filter is the board's own, which the panel's forms carry back so
	// an action keeps the view (the cards' forms carry their card's).
	Filter                    boardFilter
	State                     string
	Working, Waiting, NeedYou int
	Spend                     factoryd.ProjectSpend
	CapEUR                    float64
	// TodayEUR is the factory's spend this UTC day and DayCapEUR the
	// policy's day cap, as /costs shows them.
	TodayEUR, DayCapEUR float64
	GitHub              string
	GitHubURL           string // the export's remote on GitHub, "" when not exported
	// RoutineCell is the strip's routine cell (C16), the first held run
	// ("↻ bump held", RoutineHeld) else the soonest under an hour away
	// ("↻ bump in 20 min"), "" for none.
	RoutineCell string
	RoutineHeld bool
	// ChatSpent: the project's chat task spent its monthly budget, which
	// stands the chat until a raise (the Fund the chat form) or the
	// month's turn; ChatTask is its task id. Approver: the viewer may
	// fund it.
	ChatSpent bool
	ChatTask  string
	Approver  bool
	// Dependencies are the factory's own dependencies' latest health with
	// each row's age, the panel's list (roadmap 299); the strip's one dot
	// carries the worst of them: DepsDown red, DepsDegraded amber, DepsUp
	// green (Sebastian, 2026-10-19).
	Dependencies []depRow
	DepsDown     bool
	DepsDegraded bool
	DepsUp       bool
}

// depRow is one dependency's state as the panel shows it: its latest
// state and the age of that word.
type depRow struct {
	factoryd.DependencyState
	Age string
}

// waiting is "; n waiting on the provider", "" for none.
func (s statusLine) waiting() string {
	if s.Waiting == 0 {
		return ""
	}
	return fmt.Sprintf("; %d %s", s.Waiting, providerWords)
}

// level is one autonomy state as the level control offers it.
type level struct {
	Name  string `json:"name"`
	Words string `json:"words"`
}

type boardData struct {
	User  string
	CSRF  string
	Filed string
	Note  string // what the human's last action did
	Page  string // the layout marks the bar's current destination: the board
	// Project and View are the board's filter, which every form
	// on it carries back so an action keeps the view.
	boardFilter
	Query       string   // the filter as a URL query for the poll's hx-get
	Writes      bool     // the viewer writes the shown project: its drawer and its stream's rows are theirs
	Chat        bool     // the project agent's panel shows beside the five columns, its input to a writer
	Messengers  bool     // the layout's notifications link
	ToggleQuery string   // the filter with the other view
	ClearQuery  string   // the filter without the find, the ×'s link (C13)
	whole       []column // the columns before the find narrowed them, the menus'
	Projects    []string // the projects the viewer writes, and the shown one
	Status      statusLine
	Columns     []column
	HumanTasks  []card
	Escalations []need
	Approvals   []approvalCard
	Problems    []string
	Approver    bool // may authorize the shown project's spend (rulings 128, 254)
	Factory     bool // writes the factory's own code: the layout links the factory-level pages
	// Autonomy is the shown project's autonomy state, which the chat's
	// level control shows and an approver changes (roadmap 239, C14), and
	// Levels the four states with their words; empty: no policy in force.
	Autonomy string
	Levels   []level
	// IntakeClosed is the shown project's intake switch (the chat
	// header's): closed refuses the wake's own filings while the member's
	// ask still files.
	IntakeClosed bool
	// Docs is the shown project's document repository (chat.doc_repo),
	// the drawer's Documents button and its reads; empty: the project
	// has no documents (roadmap 277).
	Docs string
	// Commands are the chat's commands, its own and the config tree's
	// (roadmap 239).
	Commands []policy.Command
	// PolicyVersion, PolicyCommit and PolicyTreeURL are the configuration
	// version factoryd runs, as /policy shows it: its number and its
	// commit, linked to the commit's tree in Forgejo. All zero when the
	// policy could not be read, where the board shows no line.
	PolicyVersion int64
	PolicyCommit  string
	PolicyTreeURL string
	// HasRoutines: the four columns show the project's routines, so the
	// head offers the routines control; AllRoutines is the board with
	// every ghost, the link of a lane's hidden line (C16).
	HasRoutines bool
	AllRoutines string
}

func pct(spent, cap float64) int {
	if cap <= 0 {
		return 0
	}
	return int(math.Round(spent / cap * 100))
}

// cols are the columns as read, before the find narrowed them: the
// column menus read the project's cards, not the find's.
func (d boardData) cols() []column {
	if d.whole != nil {
		return d.whole
	}
	return d.Columns
}

// Final is a done, failed or closed card: a two-line row on the board. A
// done card that tracks its own deploy wears a stage, so it stays a full
// card until its change runs live (roadmap 296).
func (k card) Final() bool { return k.Stage == "" && factoryd.Final(k.Status) }

// Glyph is the final state's mark beside its word.
func (k card) Glyph() string {
	switch k.Status {
	case "done":
		return "✓"
	case "failed":
		return "✕"
	case "closed":
		return "⊘"
	}
	return "·"
}

// Speaks is whether the face shows the activity line: a card in progress,
// one that waits, one stalled on the provider or stale; a queued card
// keeps its face bare.
func (k card) Speaks() bool {
	return k.Stage != "" || strings.HasPrefix(k.Status, "waiting") || k.Stale || k.Provider || k.Retrying || k.Held
}

// configTree is commit's tree of the configuration directory in Forgejo,
// as /policy, the board and the resources page link it.
func (c *Console) configTree(commit string) string {
	return c.cfg.ConfigRepo + "/src/commit/" + url.PathEscape(commit) + "/" + c.cfg.ConfigDir
}

// counts are the board's column counts in the page's order, the wide
// view's human lane first.
func (c *Console) counts(d boardData) ([]string, error) {
	var out []string
	if d.wide() {
		out = append(out, strconv.Itoa(len(d.Approvals)), strconv.Itoa(len(d.Escalations)), strconv.Itoa(len(d.HumanTasks)))
	}
	for _, col := range d.Columns {
		var b strings.Builder
		if err := c.tmpl.ExecuteTemplate(&b, "count", col); err != nil {
			return nil, err
		}
		out = append(out, b.String())
	}
	return out, nil
}

// faces is what of the board's cards changes with time alone, for the
// stream to compare: each face's stale ring, wait on the provider, retry
// or hold, checks and minute ages.
func (d boardData) faces() string {
	var b strings.Builder
	add := func(k card) {
		fmt.Fprintf(&b, "%s %t %q %t %t %t %s %s %s %s\n", k.ID, k.Stale, k.StaleText, k.Provider, k.Retrying, k.Held, k.Checks, k.Total, k.GateAge, k.StageAge)
	}
	for _, col := range d.Columns {
		for _, k := range col.Cards {
			add(k)
		}
		for _, g := range col.Ghosts {
			fmt.Fprintf(&b, "ghost %s %s\n", g.Name, g.NextWords)
		}
		for _, n := range col.Needs {
			if n.Approval == nil {
				add(n.Card)
				fmt.Fprintf(&b, "waiting %s\n", n.Waiting())
			} else {
				fmt.Fprintf(&b, "approval %d %s\n", n.Approval.ID, n.Approval.Waiting())
			}
		}
	}
	for _, k := range d.HumanTasks {
		add(k)
	}
	for _, n := range d.Escalations {
		add(n.Card)
	}
	return b.String()
}

// finishedPerPage is how many finished cards a column reads at a time.
const finishedPerPage = 50

func (c *Console) card(t factoryd.Task) card {
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + t.Repository
	k := card{Task: t}
	if t.IssueNumber > 0 {
		k.IssueURL = fmt.Sprintf("%s/issues/%d", base, t.IssueNumber)
	}
	if t.PRNumber > 0 {
		k.PRURL = fmt.Sprintf("%s/pulls/%d", base, t.PRNumber)
	}
	end := time.Now()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	if !t.CreatedAt.IsZero() {
		k.Total = minutes(end.Sub(t.CreatedAt))
	}
	if t.Gate != "" && t.GateSince != nil {
		k.GateAge, k.GateText = minutes(time.Since(*t.GateSince)), t.Gate
	}
	k.StatusClass = statusClass(t.Status)
	k.Stage, k.StageAge = stage(t, end)
	k.Title = cmp.Or(t.HumanSummary, func() string {
		if t.PlanKey != "" {
			return t.PlanKey
		}
		return factoryd.TitleClause(t.Objective)
	}())
	if t.BudgetEUR > 0 {
		k.NearBudget = nearBudget(t.SpentEUR, t.BudgetEUR)
	}
	return k
}

// stage is t's work stage and how long it has been in it at now:
// planning or building since the admission, verifying since its gate
// began, else since the admission; a card never admitted counts from its
// filing.
func stage(t factoryd.Task, now time.Time) (string, string) {
	var word string
	switch t.Status {
	case "running", "closing":
		word = "building"
		if t.Lane == policy.LanePlan {
			word = "planning"
		}
	case "in-review":
		word = "verifying"
	default:
		return "", ""
	}
	since := t.CreatedAt
	if t.AdmittedAt != nil {
		since = *t.AdmittedAt
	}
	if word == "verifying" && t.GateSince != nil {
		since = *t.GateSince
	}
	if since.IsZero() {
		return word, ""
	}
	return word, minutes(now.Sub(since))
}

// minutes is a duration in whole minutes, hours and minutes from an
// hour, days and hours from a day; under a minute it says so.
func minutes(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 24*60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d d %d h", m/(24*60), m%(24*60)/60)
}

// coarse is d in one unit, in minutes()'s words: minutes under an hour,
// hours under a day, else days.
func coarse(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}

// span is a gate's duration to the second under an hour.
func span(d time.Duration) string {
	sec := int(d.Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d s", sec)
	case sec < 3600 && sec%60 == 0:
		return fmt.Sprintf("%d min", sec/60)
	case sec < 3600:
		return fmt.Sprintf("%d min %d s", sec/60, sec%60)
	}
	return minutes(d)
}

// errNoProject: the board was asked for a project agents do not work on.
var errNoProject = errors.New("no such project")

// board renders the whole page, or only the board for htmx's poll.
func (c *Console) board(w http.ResponseWriter, r *http.Request) {
	d, ok := c.readBoard(w, r, urlFilter(r))
	if !ok {
		return
	}
	d.Filed = r.URL.Query().Get("filed")
	name := "page.html"
	if r.URL.Path == "/board" {
		name = "board-fragment"
	}
	c.render(w, name, d)
}

// readBoard is the board under f for a GET, false after it answered the
// refusal.
func (c *Console) readBoard(w http.ResponseWriter, r *http.Request, f boardFilter) (boardData, bool) {
	if f.View != "" && !f.wide() {
		http.Error(w, "view must be "+viewWide+" or none", http.StatusBadRequest)
		return boardData{}, false
	}
	d, err := c.boardOf(r, f, button(r))
	switch {
	case errors.Is(err, errNoProject):
		http.Error(w, "no project "+f.Project+" takes agent tasks", http.StatusNotFound)
		return d, false
	case err != nil:
		log.Printf("console: board: %v", err)
		http.Error(w, "cannot read the board", http.StatusInternalServerError)
		return d, false
	}
	return d, true
}

// finishedPage answers GET /board/finished: the page of the column of
// finished cards col after the card that after (its arrival time) and
// after-id name, its cards and the form of the page after
// it, which htmx puts in place of the form that asked. The page reads
// only its fragment's column — not the open cards, the approval
// requests, the status line or the pulse a whole board read takes.
func (c *Console) finishedPage(w http.ResponseWriter, r *http.Request) {
	f, q := urlFilter(r), r.URL.Query()
	f.Col, f.After.ID = q.Get("col"), q.Get("after-id")
	at, err := time.Parse(time.RFC3339Nano, q.Get("after"))
	if err != nil || f.After.ID == "" || f.whole() {
		http.Error(w, "a page of finished cards follows a card (after, its arrival time in RFC 3339, and after-id), and a find or a show reads them all", http.StatusBadRequest)
		return
	}
	f.After.At = at
	if f.View != "" && !f.wide() {
		http.Error(w, "view must be "+viewWide+" or none", http.StatusBadRequest)
		return
	}
	col, ok := c.readFinishedColumn(w, r, f)
	if !ok {
		return
	}
	c.render(w, "finished-page", col)
}

// finishedColumn is the view's column of finished cards key — the four
// columns' Done, which holds all three final states, or the wide view's
// column of that state — false when the view has none.
func finishedColumn(f boardFilter, key string) (column, bool) {
	if !f.wide() {
		if key != "done" {
			return column{}, false
		}
		return column{Title: columnTitles[colDone], Finished: key}, true
	}
	for _, s := range stateColumns {
		if s.Status == key && factoryd.Final(s.Status) {
			return column{Title: s.Title, Finished: s.Status}, true
		}
	}
	return column{}, false
}

// readFinishedColumn is the board's column of finished cards f.Col, f's
// view of it: the column's page after f.After with the form of the page
// after it, false after it answered the refusal. shownProject picks the
// project the whole board would show under f, so the column holds the
// same cards boardOf's would.
func (c *Console) readFinishedColumn(w http.ResponseWriter, r *http.Request, f boardFilter) (column, bool) {
	ctx := r.Context()
	v, all, err := c.projectsOf(r, f)
	if err != nil {
		log.Printf("console: board: %v", err)
		http.Error(w, "cannot read the board", http.StatusInternalServerError)
		return column{}, false
	}
	shown, _, _, err := c.shownProject(ctx, r, v, all, f)
	if errors.Is(err, errNoProject) {
		http.Error(w, "no project "+f.Project+" takes agent tasks", http.StatusNotFound)
		return column{}, false
	}
	col, ok := finishedColumn(f, f.Col)
	if !ok {
		http.Error(w, "the board has no column of finished cards "+f.Col, http.StatusBadRequest)
		return column{}, false
	}
	if shown.Name == "" {
		// the viewer writes no project and named none, so no board named
		// one either: the fragment shows none of its column
		return col, true
	}
	f.Project = shown.Name
	v.filter = f
	d := boardData{Columns: []column{col}}
	page, err := c.readFinished(ctx, &d, f, shown.Repository)
	if err != nil {
		log.Printf("console: board: finished tasks: %v", err)
		http.Error(w, "cannot read the board", http.StatusInternalServerError)
		return column{}, false
	}
	col = d.Columns[0]
	for _, bt := range page {
		v.byID[bt.ID] = bt.Task
	}
	// a failed plan card's Retry follows its replacement, an open card the
	// page does not read; boardCard reads it from the board's tasks
	for _, bt := range page {
		t := bt.Task
		if t.Status == "failed" && t.Source == factoryd.SourcePlan && t.Parent != "" && t.PlanKey == "" {
			if rt, ok, err := c.cfg.Tasks.Task(ctx, factoryd.RetryID(t.ID)); err == nil && ok &&
				rt.Kind != factoryd.KindChat && !factoryd.Final(rt.Status) {
				v.byID[rt.ID] = rt
			}
		}
	}
	for _, bt := range page {
		// a finished card of the human lane holds no column (boardOf)
		if v.humanLane(bt.Task) {
			continue
		}
		col.Cards = append(col.Cards, c.boardCard(ctx, v, bt))
	}
	c.finishCards(ctx, v, func(fn func(*card)) {
		for i := range col.Cards {
			fn(&col.Cards[i])
		}
	})
	return col, true
}

// showBoard answers a board action with the board of the form's project:
// to htmx the fragment carrying note, and problems on the approval cards
// of the tasks they name; a browser's form a redirect to the board after a
// success (a reload resends nothing), else the page.
func (c *Console) showBoard(w http.ResponseWriter, r *http.Request, status int, note string, problems map[string]string) {
	f := formFilter(r)
	if r.Header.Get("HX-Request") == "" && status == http.StatusOK {
		http.Redirect(w, r, "/"+f.query(), http.StatusSeeOther)
		return
	}
	d, err := c.boardOf(r, f, button(r))
	if errors.Is(err, errNoProject) {
		f.Project = ""
		d, err = c.boardOf(r, f, button(r))
	}
	if err != nil {
		log.Printf("console: board: %v", err)
		http.Error(w, note+" The board could not be read; reload it.", http.StatusInternalServerError)
		return
	}
	d.Note = note
	// the approval cards: the wide view's human lane, else Needs you
	cards := make([]*approvalCard, 0, len(d.Approvals))
	for i := range d.Approvals {
		cards = append(cards, &d.Approvals[i])
	}
	for _, col := range d.Columns {
		for _, n := range col.Needs {
			if n.Approval != nil {
				cards = append(cards, n.Approval)
			}
		}
	}
	for _, a := range cards {
		if p, ok := problems[a.TaskID]; ok {
			a.Problem = p
		}
	}
	name := "page.html"
	if r.Header.Get("HX-Request") != "" {
		name = "board-fragment"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	c.render(w, name, d)
}

// boardView is what the cards of one render share: the viewer, the
// policy in force, the board's filter (which every
// card's forms carry back),
// the projects agents work on by repository, the tasks read so far by id,
// the pending approval requests and the tasks they card (planned: by a
// plan's request). boardOf fills it for the whole board, cardFragment for
// one card; problems are what the board says it could not do.
type boardView struct {
	user, csrf      string
	policy          policy.Version
	policyOK        bool // the policy could be read; false leaves the board's version line off
	filter          boardFilter
	byRepo          map[string]factoryd.Project
	byID            map[string]factoryd.Task
	pending         []factoryd.ApprovalRequest
	carded, planned map[string]bool
	// mergePended names the tasks whose merge approval pends: in the four
	// columns the card yields In progress to that approval (Needs you
	// holds it), as a plan does to its acceptance.
	mergePended map[string]bool
	deploys     map[string]bool // a done card whose change is not live yet: it tracks its own deploy (roadmap 296)
	unchecked   bool            // a may-move check failed, logged once per render
	problems    []string
}

// viewOf reads what every card of a render shares, and the projects agents
// work on, by name.
func (c *Console) viewOf(r *http.Request, f boardFilter) (*boardView, []factoryd.Project, error) {
	v, all, err := c.projectsOf(r, f)
	if err != nil {
		return v, nil, err
	}
	ctx := r.Context()
	if v.pending, err = c.cfg.Approvals.ApprovalRequests(ctx, "pending"); err != nil {
		log.Printf("console: approvals: %v", err)
		v.problems = append(v.problems, "approval requests could not be read")
	}
	for _, a := range v.pending {
		v.carded[a.TaskID] = true
		v.planned[a.TaskID] = v.planned[a.TaskID] || a.Kind == factoryd.ApprovalPlan
		v.mergePended[a.TaskID] = v.mergePended[a.TaskID] || a.Kind == factoryd.ApprovalMerge
	}
	return v, all, nil
}

// projectsOf reads the view's policy and the projects agents work on, by
// name: what a page of finished cards reads, whose cards never wait on an
// approval.
func (c *Console) projectsOf(r *http.Request, f boardFilter) (*boardView, []factoryd.Project, error) {
	ctx, s := r.Context(), current(r)
	v := &boardView{user: s.user, csrf: s.csrf, filter: f, byRepo: map[string]factoryd.Project{},
		byID: map[string]factoryd.Task{}, carded: map[string]bool{}, planned: map[string]bool{}, deploys: map[string]bool{},
		mergePended: map[string]bool{}}
	v.policy, v.policyOK = c.inForce(ctx)
	all, err := c.coderProjects(ctx)
	if err != nil {
		return v, nil, fmt.Errorf("projects: %w", err)
	}
	for _, p := range all {
		v.byRepo[p.Repository] = p
	}
	return v, all, nil
}

// humanLane: t has no agent, so its card is in the human lane: a human
// task. A roll is no card (Sebastian, 2026-10-18): each card tracks its
// own deploy, so the roll tasks the roller files never show.
func (v *boardView) humanLane(t factoryd.Task) bool {
	return t.Kind == "human"
}

// column is bt's column: the status mapping, except a done card whose
// change is not live yet, which stays in progress until its deploy lands
// (roadmap 296): no card is done before it is usable.
func (v *boardView) column(bt factoryd.BoardTask, viewer string) int {
	col := fiveColumn(bt.Task, viewer)
	if col == colDone && bt.Status == "done" && v.deploys[bt.ID] {
		return colProgress
	}
	return col
}

// deployStage is the chain position a done card shows while its change is
// not live yet: where its deploy chain sits (roadmap 297), the words of
// the chain itself, no roll in them.
func deployStage(state string) string {
	switch state {
	case "":
		return "waiting for the chain"
	case "failure", "cancelled":
		return "chain red"
	}
	return "chain running"
}

// escalated: t waits on an escalation of its own; a waiting plan's card is
// its escalation, which Request changes answers.
func (v *boardView) escalated(t factoryd.Task) bool {
	return t.Status == "waiting-human" && !v.planned[t.ID]
}

// approves: the viewer approves repository's project (ruling 254).
func (v *boardView) approves(repository string) bool {
	return slices.Contains(v.policy.ApproversOf(repository), v.user)
}

// mayReady: the viewer, an approver, may move t's cards into ready, as
// Start plan and Retry do.
func (c *Console) mayReady(ctx context.Context, v *boardView, t factoryd.Task) bool {
	if !v.approves(t.Repository) {
		return false
	}
	_, proc := processOf(v.byRepo, t)
	ready, _ := proc.Lane(policy.LaneReady)
	ok, err := c.mayMoveCached(ctx, t.Repository, v.user, ready.MovedBy)
	if err != nil {
		log.Printf("console: may %s move %s's cards into ready: %v", v.user, t.Repository, err)
	}
	return ok
}

// boardCard is bt's card as the board shows it, in a column, the human
// lane or the escalations, and as the card fragment swaps it in; its
// activity line, checks and forms follow from finishCards. Place is
// where the board puts it, in either view: the script swaps a card whose
// place changed with the whole board.
func (c *Console) boardCard(ctx context.Context, v *boardView, bt factoryd.BoardTask) card {
	t := bt.Task
	k := c.card(t)
	k.Place = fmt.Sprintf("%s %s %s %t %t %d %d", t.Status, t.Lane, t.Accent, v.carded[t.ID], v.planned[t.ID], bt.EscalationAlert,
		v.column(bt, v.user))
	if v.deploys[t.ID] {
		k.Stage = deployStage(bt.ChainState)
		if t.FinishedAt != nil {
			k.StageAge = minutes(time.Since(*t.FinishedAt))
		}
	}
	k.WaitingOn = bt.WaitingOn
	// "blocked by n" counts the dependencies that are not done yet;
	// WaitingOn already says so for a card in waiting-deps, where the
	// face shows that chip and never both.
	if bt.Dependencies != "" && bt.WaitingOn == "" {
		k.Blocked, k.BlockedOn = depsCount(bt.Dependencies), bt.Dependencies
	}
	if t.Gate == factoryd.GateMergeQueue {
		k.GateText = fmt.Sprintf("merge queue, %d ahead,", bt.QueueAhead)
	}
	k.Ask = bt.Escalation != ""
	if v.humanLane(t) {
		return k
	}
	if t.Source == factoryd.SourcePlan && t.Parent != "" {
		k.PlanIssue = c.planIssue(ctx, v.byID, t.Parent)
	}
	switch t.Status {
	case "backlog":
		name, proc := processOf(v.byRepo, t)
		for _, lane := range policy.LaneNames {
			why, err := moveRefusal(ctx, t, capFacts{}, name, proc, lane, v.user, c.mayMoveCached)
			if err != nil && !v.unchecked {
				v.unchecked = true
				log.Printf("console: may %s move %s into %s: %v", v.user, t.ID, lane, err)
				v.problems = append(v.problems, "who may move a card could not be checked: the move buttons are hidden")
			}
			if err == nil && why == "" {
				k.Moves = append(k.Moves, move{Lane: lane, Title: laneTitles[lane], CSRF: v.csrf})
			}
		}
	case "queued", "waiting-deps":
	default:
		if t.Lane == policy.LanePlan || t.Lane == policy.LaneReady {
			k.Badge = t.Lane
		}
	}
	// the four columns are no lanes: a card wears its lane when it is
	// not the default, ready, and its status does not say the same word
	if !v.filter.wide() {
		k.Badge = t.Lane
		if t.Lane == t.Status || t.Lane == policy.LaneReady {
			k.Badge = ""
		}
	}
	if t.Lane == policy.LanePlan && t.Status == "done" {
		var err error
		if k.Plan, err = c.planCards(ctx, t, c.mayReady(ctx, v, t), v.csrf); err != nil {
			log.Printf("console: the cards of plan %s: %v", t.ID, err)
			v.problems = append(v.problems, "the cards of plan "+t.ID+" could not be read")
		} else {
			k.Children = planRollup(k.Plan.Cards)
		}
	}
	// a retry that stopped before its replacement got an issue is finished by the next
	replacement, retried := v.byID[factoryd.RetryID(t.ID)]
	if t.Status == "failed" && t.Source == factoryd.SourcePlan && t.Parent != "" && (t.PlanKey != "" || retried && replacement.IssueNumber == 0) &&
		c.mayReady(ctx, v, t) {
		k.Retry = &retry{CSRF: v.csrf}
	}
	if v.escalated(t) {
		k.Reason, k.Choices, k.EscalationAlert = cardReason(bt.Escalation), bt.Choices, bt.EscalationAlert
		k.Question, k.AnswersPending = bt.Question, bt.AnswersPending
		k.Capped = capFactsFrom(t, v.policy.Document.Limits.Sessions, bt.Escalation, v.pending).capped
		// the Fund action is offered where the fund's floor refuses a
		// session's opening calls for want of budget (roadmap 243): the same
		// card, to its project's approvers; a refusal no raise answers
		// offers none
		if v.approves(t.Repository) && factoryd.BudgetRefusal(c.unfundedSession(ctx, t)) {
			k.Fund = &fundForm{CSRF: v.csrf}
		}
		// the acceptance close is offered where the card's own trail
		// proves the work (roadmap 208): the same card, to its approvers
		if v.approves(t.Repository) {
			if evidence, ok := c.acceptanceEvidence(ctx, t.ID); ok {
				k.AcceptanceClose = &acceptanceCloseForm{CSRF: v.csrf, Evidence: evidence}
			}
		}
	}
	return k
}

// activityLine is event e on its card's activity line: what happened in
// words, by whom, which way and when; a task_status row names the status and gate
// it left the task in.
func activityLine(e factoryd.Event) string {
	at := e.At.UTC().Format("15:04")
	if e.Kind == factoryd.EventTaskStatus {
		words := fmt.Sprintf("status %v", e.Detail["status"])
		if gate, _ := e.Detail["gate"].(string); gate != "" {
			words += ", gate " + gate + ","
		}
		return words + " at " + at
	}
	return fmt.Sprintf("%s by %s via %s at %s", strings.ReplaceAll(e.Kind, "_", " "), e.Actor, e.Via, at)
}

// maxCardReason bounds what a card shows of an escalation body even
// without bullets to cut (a screened author's reasons, an agent's ask).
const maxCardReason = 2000

// cardReason is what a needs-you card shows of an escalation body: its
// question and state lines, with the findings history — the session
// bullets an escalation carries for the issue — one pointer line (roadmap
// 244). A card with a user decision names the decision and its context;
// the transcript lives on the issue the answer happens on.
func cardReason(body string) string {
	var keep []string
	dropped := false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "- ") {
			dropped = true
			continue
		}
		keep = append(keep, l)
	}
	if dropped {
		keep = append(keep, "- the findings history is on the issue")
	}
	s := strings.Join(keep, "\n")
	if len(s) > maxCardReason {
		s = textutil.Truncate(s, maxCardReason) + "\n- the rest is on the issue"
	}
	return s
}

// finishCards fills what each card's face reads per render rather than
// from its task: the newest event's activity line and the stale ring, the
// checks count and the retry, the fine states, and for a writer of the
// card's project the summary and accent forms with the board's filter.
// each visits every card once.
func (c *Console) finishCards(ctx context.Context, v *boardView, each func(func(*card))) {
	var ids []string
	each(func(k *card) { ids = append(ids, k.ID) })
	newest, err := c.cfg.Events.NewestTaskEvents(ctx, ids)
	if err != nil {
		log.Printf("console: newest events: %v", err)
	}
	// A card with no event keeps its escalation's first line, the
	// question it waits on an answer to; a card with neither claims
	// nothing. On a waiting card that reason is what the human owes it:
	// it stands in place of the newest event. The stale ring — one whose
	// newest activity is older than the stale window wears it, the
	// newest of its newest event and its newest timeline step (its own
	// session's last report), one that never had either its own age —
	// comes only on a card that is running its work: never a final one,
	// never one in the backlog, the plan lane or the queue; a card parked
	// for a slot owes no work while it waits.
	setActivity := func(k *card) {
		lastAt := func(at time.Time) time.Time {
			if k.stepAt.After(at) {
				return k.stepAt
			}
			return at
		}
		ring := func(d time.Duration) bool { return d >= factoryd.StaleWindow }
		live := !factoryd.Final(k.Status) && k.Status != "backlog" && k.Status != "plan" && k.Status != "queued"
		reason := func() string {
			q, _, _ := strings.Cut(strings.TrimPrefix(k.Reason, factoryd.AgentAsks), "\n")
			return q
		}
		if e, ok := newest[k.ID]; ok {
			k.Activity = activityLine(e)
			// the stage began at its status row, when that row is the newest event
			if k.Stage != "" && e.Kind == factoryd.EventTaskStatus && fmt.Sprint(e.Detail["status"]) == k.Status && !e.At.IsZero() {
				k.StageAge = minutes(time.Since(e.At))
			}
			k.Stale = live && ring(time.Since(lastAt(e.At)))
			if k.Stale {
				k.StaleText = "nothing for over " + minutes(time.Since(lastAt(e.At)))
			}
			// a shaped question's face carries its ask alone — its why
			// belongs to the drawer, never the board
			if k.Question != nil && strings.HasPrefix(k.Status, "waiting") {
				k.Activity, k.WaitingForActivity = askline(k.Question.Ask), true
			} else if k.Reason != "" && strings.HasPrefix(k.Status, "waiting") {
				k.Activity = reason()
				k.WaitingForActivity = true
			}
			return
		}
		k.Stale = live && ring(time.Since(lastAt(k.CreatedAt)))
		if k.Stale {
			k.StaleText = "no event yet, " + minutes(time.Since(lastAt(k.CreatedAt))) + " on the card"
		}
		if k.Question != nil {
			k.Activity = askline(k.Question.Ask)
		} else if k.Reason != "" {
			k.Activity = reason()
		}
	}
	c.faceTimelines(ctx, each)
	each(setActivity)
	c.callTotals(ctx, each)
	each(func(k *card) { k.Fine = fineStates(*k, v.filter.wide()) })
	setForms := func(k *card) {
		ok, err := c.mayMoveCached(ctx, k.Repository, v.user, policy.MovedByWriters)
		if err != nil && !v.unchecked {
			v.unchecked = true
			log.Printf("console: may %s write %s: %v", v.user, k.Repository, err)
		}
		if k.MayWriteSummary = ok; ok {
			k.CSRF = v.csrf
		}
		k.Filter = v.filter
		// the pair a human card that waits without an ask offers its writer
		// (roadmap 1131): the factory, or a close on a note
		if ok && k.Kind == "human" && !k.Ask && (k.Status == "backlog" || k.Status == "waiting-human") {
			k.Go = &goForm{CSRF: v.csrf}
		}
	}
	each(setForms)
}

// callTotals fills each card's calls line beside its spend: the task's
// call rows summed — its tokens and modeled cost to date (roadmap 206) —
// one query for the whole render; a read that fails leaves the line off.
func (c *Console) callTotals(ctx context.Context, each func(func(*card))) {
	var ids []string
	each(func(k *card) { ids = append(ids, k.ID) })
	totals, err := c.cfg.Costs.CallTotals(ctx, ids)
	if err != nil {
		log.Printf("console: call totals: %v", err)
		return
	}
	each(func(k *card) { k.CallsText = callsLine(totals[k.ID]) })
}

// callsLine is a task's call rows summed on one line — its tokens to
// date, prompt, cached and output, and the modeled pay-as-you-go cost of
// them (roadmap 206: never money billed) — "" on a task without call
// rows. The card face shows it beside its spend; the session and plan
// reads carry the same line.
func callsLine(t policy.CallTotals) string {
	if t == (policy.CallTotals{}) {
		return ""
	}
	return commas.Sprintf("%d prompt · %d cached · %d output tokens · €%.4f modeled",
		t.Prompt, t.Cached, t.Output, t.ModeledEUR)
}

// shownProject is the board's shown project under f among all, the
// projects agents work on — f.Project's, else the first the viewer
// writes — with projects, the top bar's select choices, every project the
// viewer writes and the shown one, and writable saying each project by
// name. A project whose writers could not be checked is offered, its
// buttons hidden, rather than an empty board; a member sees only a
// project they write or read (ruling 254), the factory's writers every
// board, read-only where they do not write; errNoProject is the refusal.
// The whole board and a page of finished cards show the same one.
func (c *Console) shownProject(ctx context.Context, r *http.Request, v *boardView, all []factoryd.Project, f boardFilter) (shown factoryd.Project, projects []string, writable map[string]bool, err error) {
	writable = map[string]bool{}
	for _, p := range all {
		writes, werr := c.mayMoveCached(ctx, p.Repository, v.user, policy.MovedByWriters)
		if werr != nil && !v.unchecked {
			v.unchecked = true
			log.Printf("console: may %s write %s: %v", v.user, p.Repository, werr)
			v.problems = append(v.problems, "who may move a card could not be checked: the move buttons are hidden")
		}
		writable[p.Name] = writes
		if writes || werr != nil {
			projects = append(projects, p.Name)
			if f.Project == "" && shown.Name == "" {
				shown = p
			}
		}
		if p.Name == f.Project {
			shown = p
		}
	}
	if f.Project != "" && shown.Name == "" {
		return shown, projects, writable, errNoProject
	}
	// a member sees only the projects they write or read (ruling 254); the
	// factory's writers see every board, read-only where they do not write
	if shown.Name != "" && !writable[shown.Name] && v.user != "" && !c.factoryWriter(r) {
		if reads, rerr := c.reads(ctx, current(r), shown); rerr != nil || !reads {
			return shown, projects, writable, errNoProject
		}
	}
	return shown, projects, writable, nil
}

// boardOf is the board of one project, f.Project or else the first the
// viewer writes, the project's cards, as the session's
// human sees it. The five columns are the default, the human lane folded
// into Needs you; the wide view puts the lanes of the project's process
// first in the fixed order backlog, plan, ready, then the state columns,
// under the human lane. act is the actor whose board read this is — the
// button's or the chat's — which its approval cards read the head's
// checks as (#1013).
func (c *Console) boardOf(r *http.Request, f boardFilter, act actor) (boardData, error) {
	ctx := r.Context()
	v, all, err := c.viewOf(r, f)
	d := boardData{User: v.user, CSRF: v.csrf, Page: "board", Factory: c.factoryWriter(r), Messengers: c.cfg.Messengers != nil}
	if v.policyOK {
		d.PolicyVersion, d.PolicyCommit, d.PolicyTreeURL = v.policy.ID, v.policy.Commit, c.configTree(v.policy.Commit)
	}
	if err != nil {
		return d, err
	}
	shown, projects, writable, err := c.shownProject(ctx, r, v, all, f)
	if err != nil {
		return d, err
	}
	d.Projects = projects
	if shown.Name == "" {
		d.Problems = append(d.Problems, "You write none of the projects agents work on.")
	} else if !slices.Contains(d.Projects, shown.Name) {
		d.Projects = append(d.Projects, shown.Name)
	}
	f.Project, d.Writes, d.Approver = shown.Name, writable[shown.Name], v.approves(shown.Repository)
	d.Commands = policy.BuiltinCommands
	if v, ok := c.inForce(ctx); ok {
		d.Commands = v.ChatCommands()
		if pc, ok := v.Projects[shown.Name]; ok && shown.Name != "" {
			d.Autonomy = pc.AutonomyState()
			for _, s := range policy.AutonomyStates {
				d.Levels = append(d.Levels, level{Name: s, Words: policy.AutonomyMeaning[s]})
			}
		}
		if shown.Name != "" {
			d.Docs = v.Document.ChatOf(v.Projects[shown.Name]).DocRepo
		}
	}
	d.Chat = c.cfg.Chat != nil && c.cfg.Operator != nil && shown.Name != "" && !f.wide()
	if d.Chat {
		if closed, err := c.cfg.Chat.ChatIntakeClosed(ctx, shown.Repository); err != nil {
			log.Printf("console: the intake of %s: %v", shown.Name, err)
		} else {
			d.IntakeClosed = closed
		}
	}
	v.filter, d.boardFilter, d.Query = f, f, f.query()
	wide := f.wide()
	other := f
	if other.View = viewWide; wide {
		other.View = ""
	}
	d.ToggleQuery = other.query()
	clear := f
	clear.Q = ""
	d.ClearQuery = clear.query()
	byLane, byStatus := map[string]int{}, map[string]int{}
	if wide {
		lanes := map[string]bool{}
		for _, l := range shown.Lanes {
			lanes[l.Name] = true
		}
		for _, name := range policy.LaneNames {
			if lanes[name] {
				byLane[name] = len(d.Columns)
				d.Columns = append(d.Columns, column{Title: laneTitles[name], Lane: name})
			}
		}
		for _, col := range stateColumns {
			byStatus[col.Status] = len(d.Columns)
			k := column{Title: col.Title}
			if factoryd.Final(col.Status) {
				k.Finished = col.Status
			}
			d.Columns = append(d.Columns, k)
		}
		byStatus["closing"] = byStatus["running"]
	} else {
		for _, title := range columnTitles {
			d.Columns = append(d.Columns, column{Title: title})
		}
		d.Columns[colDone].Finished = "done"
	}

	tasks, err := c.cfg.Tasks.BoardTasks(ctx, shown.Repository)
	if err != nil {
		return d, fmt.Errorf("tasks: %w", err)
	}
	// a done card the store still lists is merged, its change not
	// confirmed live yet: it tracks its own deploy (roadmap 296). The
	// finished page holds done cards that are live.
	for _, bt := range tasks {
		v.byID[bt.ID] = bt.Task
		if bt.Status == "done" && bt.Source != factoryd.SourceRoll {
			v.deploys[bt.ID] = true
		}
	}
	if shown.Name != "" {
		finished, err := c.readFinished(ctx, &d, f, shown.Repository)
		if err != nil {
			return d, fmt.Errorf("finished tasks: %w", err)
		}
		tasks = append(tasks, finished...)
		for _, bt := range finished {
			v.byID[bt.ID] = bt.Task
		}
	}
	escalations := map[string]factoryd.BoardTask{}
	var needs []need
	escalation := func(k card) need {
		return need{Card: k, Escalated: true, CSRF: v.csrf, Approver: d.Factory && c.approver(r), Back: "/" + d.Query}
	}
	// BoardTasks and readFinished read shown.Repository's cards only, so
	// every byID lookup below stays right: a plan parent not among them
	// (finished, another project's) is read by planIssue's own read, a
	// failed card's replacement keeps the failed card's repository, and a
	// pending approval's task lives in its project, which here is shown's
	for _, bt := range tasks {
		t := bt.Task
		if t.Source == factoryd.SourceRoll {
			continue // a roll is no card (Sebastian, 2026-10-18)
		}
		human, col := v.humanLane(t), v.column(bt, v.user)
		// the status line counts the project's cards, whatever the accent;
		// a plan that waits on its acceptance counts as its approval
		switch {
		case human:
			if !factoryd.Final(t.Status) {
				d.Status.NeedYou++
			}
		case col == colProgress && t.Status == "running" && c.timelineFace(ctx, t.ID).provider:
			d.Status.Waiting++
		case col == colProgress && t.Status != "in-review":
			d.Status.Working++
		case col == colNeedsYou && !v.planned[t.ID]:
			d.Status.NeedYou++
		}
		if t.Status == "waiting-human" && v.planned[t.ID] {
			escalations[t.ID] = bt
		}
		k := c.boardCard(ctx, v, bt)
		switch {
		case human:
			if factoryd.Final(t.Status) {
				break
			}
			if wide {
				d.HumanTasks = append(d.HumanTasks, k)
			} else {
				needs = append(needs, need{Since: t.CreatedAt, Card: k})
			}
		case wide:
			i, ok := byStatus[t.Status]
			if v.deploys[t.ID] {
				i, ok = byStatus["running"], true // done, its deploy not live yet
			}
			switch t.Status {
			case "backlog":
				i, ok = byLane[policy.LaneBacklog]
			case "queued", "waiting-deps":
				if i, ok = byLane[t.Lane]; !ok {
					i, ok = byLane[policy.LaneReady]
				}
			}
			if ok {
				d.Columns[i].Cards = append(d.Columns[i].Cards, k)
			}
			if v.escalated(t) {
				d.Escalations = append(d.Escalations, escalation(k))
			}
		case col == colNeedsYou:
			// a plan that waits on its acceptance needs its approval card
			if v.planned[t.ID] {
				break
			}
			n := need{Since: waitingSince(bt), Card: k}
			if v.escalated(t) {
				n = escalation(k)
				n.Since = waitingSince(bt)
			}
			needs = append(needs, n)
		case col >= 0:
			// a card whose merge approval pends yields In progress to
			// that approval, as a plan yields to its acceptance card
			if col == colProgress && v.mergePended[t.ID] && !v.planned[t.ID] {
				break
			}
			d.Columns[col].Cards = append(d.Columns[col].Cards, k)
		}
	}
	for _, a := range v.pending {
		if a.Project != shown.Repository {
			continue
		}
		k := c.approvalCard(ctx, a, act, v.byID, v.csrf, d.Approver, true)
		k.Filter, k.Writer = f, d.Writes
		// an escalation from before this revision's request (a cap a
		// writer granted) no longer holds
		if e := escalations[a.TaskID]; a.Kind == factoryd.ApprovalPlan && e.EscalatedAt.After(a.CreatedAt) {
			k.Escalation = e.Escalation
		}
		d.Status.NeedYou++
		if wide {
			d.Approvals = append(d.Approvals, k)
		} else {
			needs = append(needs, need{Since: a.CreatedAt, Approval: &k})
		}
	}
	if !wide {
		slices.SortStableFunc(needs, func(a, b need) int { return a.Since.Compare(b.Since) })
		d.Columns[colNeedsYou].Needs = needs
	}
	c.finishCards(ctx, v, func(fn func(*card)) {
		for i := range d.Columns {
			for j := range d.Columns[i].Cards {
				fn(&d.Columns[i].Cards[j])
			}
			for j := range d.Columns[i].Needs {
				if d.Columns[i].Needs[j].Approval == nil {
					fn(&d.Columns[i].Needs[j].Card)
				}
			}
		}
		for i := range d.HumanTasks {
			fn(&d.HumanTasks[i])
		}
		for i := range d.Escalations {
			fn(&d.Escalations[i].Card)
		}
	})
	d.whole = d.Columns
	d.Columns = find(d.Columns, f.Q)
	if !wide {
		arrange(&d, f)
	}
	if shown.Name != "" {
		c.statusLine(ctx, v, f, shown, &d)
		if c.cfg.Routines != nil {
			c.placeGhosts(ctx, &d)
		}
	}
	d.Problems = append(d.Problems, v.problems...)
	return d, nil
}

// readFinished reads repository's finished cards for d's columns of them:
// all of them under a find or a show, which narrow them all; else each
// column its first page, or as many as the board showed (f.Shown), or
// for GET /board/finished f.Col's page after f.After, and the column
// gets its whole count and the form of its next page.
func (c *Console) readFinished(ctx context.Context, d *boardData, f boardFilter, repository string) ([]factoryd.BoardTask, error) {
	q := factoryd.FinishedQuery{Repository: repository, Since: time.Now().Add(-finishedSince), Oldest: !f.wide() && f.Done.Order == "oldest"}
	if f.whole() {
		q.Statuses = factoryd.FinalStatuses
		all, _, err := c.cfg.Tasks.FinishedTasks(ctx, q)
		return all, err
	}
	var out []factoryd.BoardTask
	for i := range d.Columns {
		col := &d.Columns[i]
		if col.Finished == "" || f.Col != "" && col.Finished != f.Col {
			continue
		}
		q.Statuses, q.After, q.Limit = []string{col.Finished}, f.After, max(finishedPerPage, f.Shown[col.Finished])
		if !f.wide() {
			q.Statuses = factoryd.FinalStatuses
		}
		if f.Col != "" {
			q.Limit = finishedPerPage
		}
		page, rest, err := c.cfg.Tasks.FinishedTasks(ctx, q)
		if err != nil {
			return nil, err
		}
		col.Whole = rest
		if rest > len(page) {
			// the form asking for this page said how many the column then shows
			upTo := len(page)
			if f.Col != "" {
				upTo = f.Shown[col.Finished]
			}
			col.More = &more{Filter: f, Key: col.Finished, Title: col.Title, After: factoryd.CursorOf(page[len(page)-1].Task),
				Shown: upTo + finishedPerPage}
		}
		out = append(out, page...)
	}
	return out, nil
}

// statusLine completes the board's status line with the project's state
// word, its spend this UTC month (its chat's included) and its month cap:
// its file's budget.month_eur, else the policy's; the spent state of its
// chat's monthly budget.
func (c *Console) statusLine(ctx context.Context, v *boardView, f boardFilter, p factoryd.Project, d *boardData) {
	s := &d.Status
	s.Project, s.Filter = p.Name, f
	switch {
	case s.NeedYou > 0:
		s.State = "waiting on you"
	case s.Working > 0:
		s.State = "working"
	default:
		s.State = "idle"
	}
	if c.cfg.Dependencies != nil {
		if deps, err := c.cfg.Dependencies.DependencyStates(ctx); err != nil {
			log.Printf("console: the dependencies' health: %v", err)
			d.Problems = append(d.Problems, "the dependencies' health could not be read")
		} else {
			for _, row := range deps {
				s.Dependencies = append(s.Dependencies, depRow{DependencyState: row, Age: coarse(time.Since(row.CheckedAt))})
				switch row.State {
				case "down":
					s.DepsDown = true
				case "degraded":
					s.DepsDegraded = true
				}
			}
			s.DepsUp = len(deps) > 0 && !s.DepsDown && !s.DepsDegraded
		}
	}
	var err error
	if s.Spend, err = c.cfg.Chat.ProjectMonthSpend(ctx, p.Repository, time.Now()); err != nil {
		log.Printf("console: the spend of %s this month: %v", p.Repository, err)
		d.Problems = append(d.Problems, "the project's spend this month could not be read")
	}
	s.CapEUR = monthCap(v.policy, p.Repository)
	if v.policyOK {
		s.DayCapEUR = v.policy.Document.Budget.DayEUR
	}
	if s.TodayEUR, err = c.cfg.Costs.DaySpend(ctx, time.Now().UTC()); err != nil {
		log.Printf("console: the spend today: %v", err)
		d.Problems = append(d.Problems, "the spend today could not be read")
	}
	s.Approver = v.approves(p.Repository)
	if t, ok, err := c.cfg.Tasks.Task(ctx, factoryd.ChatTaskID(p.Name, time.Now())); err != nil {
		log.Printf("console: the chat task of %s: %v", p.Repository, err)
		d.Problems = append(d.Problems, "the chat's budget could not be read")
	} else if ok && t.Kind == factoryd.KindChat && t.Status == "running" && t.SpentEUR >= t.BudgetEUR {
		s.ChatSpent, s.ChatTask = true, t.ID
	}
	if c.cfg.Mirrors == nil {
		return
	}
	m, ok, err := c.cfg.Mirrors.Mirror(ctx, p.Name, factoryd.MirrorExport)
	if err != nil {
		log.Printf("console: the export of %s: %v", p.Name, err)
		d.Problems = append(d.Problems, "the project's GitHub export could not be read")
	}
	if ok {
		s.GitHub, _, _ = strings.Cut(m.State, ":")
		s.GitHubURL = "https://github.com/" + m.Remote + "/pulls"
	}
}

// placeGhosts stands each of the shown project's routines as a ghost at
// the foot of Queued in the four columns (C16, as the 2026-10-17 ruling
// has it: needs-you holds only what needs a human's action now, so every
// routine's preview queues like all scheduled work, and the routine's
// work reaches needs-you only as its filed card, through the card flow):
// the dated ones by their next run, then the merge-counted ones by the
// merges they lack, then the rest. The board's routines choice hides
// those whose next run is further away than its period, or all, and each
// lane counts what it hides. The strip's cell names the first held run,
// else the soonest under an hour away, whatever the choice.
func (c *Console) placeGhosts(ctx context.Context, d *boardData) {
	views, err := c.cfg.Routines.Views(ctx, d.Project)
	if err != nil {
		log.Printf("console: the routines of %s: %v", d.Project, err)
		d.Problems = append(d.Problems, "the project's routines could not be read")
		return
	}
	rank := func(rv factoryd.RoutineView) int {
		switch {
		case !rv.Next.IsZero():
			return 0
		case rv.MergesLeft > 0:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(views, func(a, b factoryd.RoutineView) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), a.Next.Compare(b.Next), cmp.Compare(a.MergesLeft, b.MergesLeft))
	})
	now := time.Now()
	if i := slices.IndexFunc(views, func(rv factoryd.RoutineView) bool { return rv.Held }); i >= 0 {
		d.Status.RoutineCell, d.Status.RoutineHeld = "↻ "+cellWord(views[i].Routine.Name)+" held", true
	} else if i := slices.IndexFunc(views, func(rv factoryd.RoutineView) bool { return rv.Soon }); i >= 0 {
		d.Status.RoutineCell = "↻ " + cellWord(views[i].Routine.Name) + " in " + coarse(views[i].Next.Sub(now))
	}
	if d.wide() || len(views) == 0 {
		return
	}
	d.HasRoutines = true
	all := d.boardFilter
	all.Routines = "all"
	d.AllRoutines = all.query()
	csrf := ""
	if d.Writes {
		csrf = d.CSRF
	}
	for _, rv := range views {
		if d.Routines == "none" || d.Routines == "within" && rv.Next.Sub(now) > d.period() {
			d.Columns[colQueued].GhostsHidden++
			continue
		}
		d.Columns[colQueued].Ghosts = append(d.Columns[colQueued].Ghosts, ghostOf(rv, now, d.boardFilter, csrf))
	}
}

// filesWords are the kinds of work a routine files, as its ghost says them.
var filesWords = map[string]string{policy.FilesHuman: "human card", policy.FilesAgent: "agent card",
	policy.FilesPullRequest: "pull request", policy.FilesReport: "report"}

// ghostOf is routine rv's ghost at now. Its next line names the run's
// time, how soon it comes and what it files. A merge-counted routine's
// names the merges it lacks, an event's only what it files, as the
// schedule line names the event. A pending Skip once leads the line, the
// run it names being the one after the skipped. Its last line names the
// newest run.
func ghostOf(rv factoryd.RoutineView, now time.Time, f boardFilter, csrf string) ghost {
	rt := rv.Routine
	g := ghost{Project: rv.Project, Name: rt.Name, Title: rt.Title, Words: rt.Words(), Soon: rv.Soon, Held: rv.Held, Skipped: rv.State.SkipNext,
		Files: rt.Files, Side: rv.FilesInForce(), Toggled: rv.State.NeedsMe != nil, Filter: f, CSRF: csrf}
	when, in, kind := nextWords(rv, now)
	switch {
	case in != "":
		g.NextWords = when + " · in " + in + " · " + kind
	case when != "":
		g.NextWords = when
	default:
		g.NextWords = kind
	}
	switch {
	case g.Skipped && when == "":
		g.NextWords = "skipped once · " + kind
	case g.Skipped:
		g.NextWords = "skipped once · next " + g.NextWords
	}
	if l := runWords(rv.Last); l != "" {
		g.LastWords = "last " + l
	}
	if g.Held {
		g.HeldWords = rv.Last.Note
	}
	if rv.Open != nil {
		g.Open, g.Running = rv.Open.IssueNumber, rv.Open.Status == "running"
	}
	return g
}

// nextWords are routine rv's next run at now as its ghost and its drawer
// say it: when it comes ("Mon 06:00", "after 6 more merges", "" for an
// event's, which the schedule's words name), how soon a dated one comes
// ("20 min", else ""), and what it files ("human card").
func nextWords(rv factoryd.RoutineView, now time.Time) (when, in, kind string) {
	when, kind = rv.NextWords, filesWords[rv.FilesInForce()]
	switch {
	case !rv.Next.IsZero():
		in = coarse(rv.Next.Sub(now))
	case rv.MergesLeft == 0:
		when = ""
	}
	return when, in, kind
}

// runWords are run l's card, outcome and the card's time once it ended
// ("#781 done in 2 d", "held"), "" before a routine's first run.
func runWords(l factoryd.RoutineRun) string {
	if l.Run == 0 {
		return ""
	}
	var w []string
	if l.Issue > 0 {
		w = append(w, fmt.Sprintf("#%d", l.Issue))
	}
	w = append(w, l.Outcome)
	if l.Issue > 0 && !l.Ended.IsZero() {
		w = append(w, "in "+coarse(l.Ended.Sub(l.Started)))
	}
	return strings.Join(slices.DeleteFunc(w, func(s string) bool { return s == "" }), " ")
}

// cellWord is the routine name's word in the strip's cell: the name's last
// segment, "bump" for weekly-bump. A name is a slug ending in a letter or
// a digit, so the segment is a plain word.
func cellWord(name string) string { return name[strings.LastIndex(name, "-")+1:] }

// monthCap is repository's month cap: its project file's budget.month_eur,
// else the policy's month_eur.
func monthCap(v policy.Version, repository string) float64 {
	if _, eur := v.ProjectCap(repository); !math.IsInf(eur, 1) {
		return eur
	}
	return v.Document.Budget.MonthEUR
}

// spendWords is spend against the month cap as the project agent reads it.
func spendWords(spend factoryd.ProjectSpend, capEUR float64) string {
	return fmt.Sprintf("spend this month €%.2f of €%.2f: the cards €%.2f, the chat €%.2f", spend.EUR, capEUR, spend.EUR-spend.ChatEUR, spend.ChatEUR)
}

type timelineEntry struct {
	Time       string
	Raw        string // Time as the timeline wrote it, the cell's title
	Step       string
	Detail     string
	Reason     string
	Supervisor supervised // the session or the gate supervisor's decision
}

// readTimeline reads the task's timeline steps, oldest first.
func (c *Console) readTimeline(ctx context.Context, taskID string) ([]map[string]any, error) {
	body, err := c.cfg.Timelines.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

func entry(m map[string]any) timelineEntry {
	e := timelineEntry{}
	e.Time, _ = m["time"].(string)
	if at, err := time.Parse(time.RFC3339, e.Time); err == nil {
		e.Raw, e.Time = e.Time, at.UTC().Format("2006-01-02 15:04:05")
	}
	e.Step, _ = m["step"].(string)
	e.Reason, _ = m["reason"].(string)
	switch e.Step {
	case "supervisor":
		why, _ := m["reason"].(string)
		guidance, _ := m["guidance"].(string)
		e.Supervisor = supervisedBy(m["decision"], cmp.Or(why, guidance), nil)
	case "gate_supervisor":
		e.Supervisor = supervisedBy(m["action"], m["diagnosis"], m["recommendation"])
	}
	rest := maps.Clone(m)
	delete(rest, "time")
	delete(rest, "step")
	if len(rest) > 0 {
		b, _ := json.Marshal(rest) // #nosec G104 -- re-encoding decoded JSON cannot fail
		e.Detail = string(b)
	}
	return e
}

// gateRow is one row of the timeline page's durations table.
type gateRow struct {
	Gate, Attempt, Start, Duration, Outcome string
	Seconds                                 int
}

// gateRows are the task's gates from its timeline, each from its start to
// the step that ended it, the CI jobs under their checks, and the gate the
// task's row is in when no step ended it yet; longest scales the bars.
func gateRows(steps []map[string]any, t factoryd.Task, now time.Time) (rows []gateRow, longest int) {
	attempt := ""
	open := t.Gate != "" && t.GateSince != nil && !factoryd.Final(t.Status)
	for _, m := range steps {
		step, _ := m["step"].(string)
		if step == "session_start" || step == "session_end" {
			attempt = ""
			if a, ok := m["attempt"].(float64); ok {
				attempt = strconv.Itoa(int(a))
			}
		}
		gate, ok := factoryd.StepGates[step]
		startS, _ := m["started"].(string)
		start, err := time.Parse(time.RFC3339, startS)
		if !ok || err != nil {
			continue
		}
		end, _ := time.Parse(time.RFC3339, fmt.Sprint(m["time"]))
		if live, err := time.Parse(time.RFC3339, fmt.Sprint(m["live"])); err == nil {
			end = live // a commissioning ends at the server's live moment
		}
		if open && gate == t.Gate && !end.Before(*t.GateSince) {
			open = false
		}
		row := gateRow{Gate: gate, Start: start.UTC().Format("2006-01-02 15:04:05"), Outcome: stepOutcome(step, m),
			Seconds: int(end.Sub(start).Seconds())}
		if gate != factoryd.GateIntake {
			row.Attempt = attempt
		}
		row.Duration = span(end.Sub(start))
		rows = append(rows, row)
		if step == "checks" {
			rows = append(rows, jobRows(m["jobs"], attempt)...)
		}
	}
	if open {
		d := now.Sub(*t.GateSince)
		rows = append(rows, gateRow{Gate: t.Gate, Attempt: attempt, Start: t.GateSince.UTC().Format("2006-01-02 15:04:05"),
			Duration: "open for " + minutes(d), Outcome: "open", Seconds: int(d.Seconds())})
	}
	for _, r := range rows {
		longest = max(longest, r.Seconds)
	}
	return rows, max(longest, 1)
}

// jobRows are the CI jobs of a checks step (name, status, run_s, queue_s).
func jobRows(jobs any, attempt string) []gateRow {
	list, _ := jobs.([]any)
	var rows []gateRow
	for _, j := range list {
		job, _ := j.(map[string]any)
		run, _ := job["run_s"].(float64)
		outcome := fmt.Sprint(job["status"])
		if q, ok := job["queue_s"].(float64); ok {
			outcome += ", queued " + span(time.Duration(q)*time.Second)
		}
		d := time.Duration(run) * time.Second
		rows = append(rows, gateRow{Gate: "job " + fmt.Sprint(job["name"]), Attempt: attempt, Duration: span(d),
			Outcome: outcome, Seconds: int(run)})
	}
	return rows
}

// stepOutcome is how the step ended its gate.
func stepOutcome(step string, m map[string]any) string {
	str := func(k string) string { s, _ := m[k].(string); return s }
	switch step {
	case "intake":
		return str("source")
	case "session_end":
		return str("outcome")
	case "checks":
		return str("state")
	case "gate_policy":
		return str("class")
	case "gate_migration":
		return "flagged for review"
	case "gate_reproduction":
		switch exit, _ := m["exit"].(float64); {
		case str("skipped") != "":
			return "skipped"
		case exit == 0:
			return "passed"
		default:
			return fmt.Sprintf("failed (exit %d)", int(exit))
		}
	case "model_review":
		return cmp.Or(str("verdict"), "no verdict")
	case "signal":
		switch str("signal") {
		case factoryd.SignalApproved:
			return "approved by " + str("author")
		case factoryd.SignalChangesRequested:
			return "changes requested by " + str("author")
		case factoryd.SignalPRMerged:
			return "merged by " + str("author")
		}
		return "closed"
	case "merged":
		return "merged by " + str("merged_by")
	case "merge_failed":
		return "failed"
	case "merge_refused":
		return "refused"
	case "commissioned":
		return str("server_type")
	}
	return ""
}

func (c *Console) timeline(w http.ResponseWriter, r *http.Request) {
	t, ok, err := c.cfg.Tasks.Task(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	steps, err := c.readTimeline(r.Context(), t.ID)
	if err != nil {
		log.Printf("console: timeline of %s: %v", t.ID, err)
		http.Error(w, "cannot read the timeline", http.StatusBadGateway)
		return
	}
	var entries []timelineEntry
	for _, m := range steps {
		entries = append(entries, entry(m))
	}
	gates, longest := gateRows(steps, t, time.Now())
	c.render(w, "timeline.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf, "Factory": c.factoryWriter(r),
		"Card": c.card(t), "Entries": entries, "Gates": gates, "Longest": longest})
}

// userCall is a call as member s in the shape factoryd.GetPages calls.
func (c *Console) userCall(s *session) func(context.Context, string, string, []byte, int) ([]byte, error) {
	return func(ctx context.Context, method, path string, _ []byte, want int) ([]byte, error) {
		var raw json.RawMessage
		err := c.as(ctx, s, method, path, nil, want, &raw)
		return raw, err
	}
}

// labels lists repo's labels as member s, all pages.
func (c *Console) labels(ctx context.Context, s *session, repo factoryd.Repo) (map[string]int64, error) {
	ls, err := factoryd.GetPages[factoryd.RepoLabel](ctx, c.userCall(s),
		fmt.Sprintf("/api/v1/repos/%s/%s/labels?", url.PathEscape(repo.Owner), url.PathEscape(repo.Name)))
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, l := range ls {
		out[l.Name] = l.ID
	}
	return out, nil
}

var priorities = []string{"priority/p0", "priority/p1", "priority/p2", "priority/p3"}

// coderProjects are the projects agents work on, by name: the new-task
// form's choices, the first its default.
func (c *Console) coderProjects(ctx context.Context) ([]factoryd.Project, error) {
	ps, err := c.cfg.Projects.Projects(ctx)
	return slices.DeleteFunc(ps, func(p factoryd.Project) bool { return !p.Coder() }), err
}

// newTask is the New task form's values, kept when the form answers the
// findings of a first submit with the text as the member wrote it
// (roadmap 125).
type newTask struct {
	Project, Title, Body, Lane, Priority, Size string
}

func (c *Console) newTaskForm(w http.ResponseWriter, r *http.Request) {
	c.renderNewTask(w, r, newTask{Lane: policy.LaneReady}, nil)
}

// renderNewTask answers the New task form: the projects agents work on,
// their lanes and the priority labels the first's repository offers. A
// first submit whose text is flagged answers the form again, the form's
// values kept and the findings shown above it.
func (c *Console) renderNewTask(w http.ResponseWriter, r *http.Request, form newTask, flags []filingFlag) {
	ps, err := c.coderProjects(r.Context())
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	if len(ps) == 0 {
		http.Error(w, "no project takes agent tasks", http.StatusServiceUnavailable)
		return
	}
	labels, err := c.labels(r.Context(), current(r), ps[0].Repo())
	if err != nil {
		c.forgejoFailed(w, r, "read the labels", err)
		return
	}
	var offered []string
	for _, p := range priorities {
		if _, ok := labels[p]; ok {
			offered = append(offered, p)
		}
	}
	type laneOption struct{ Name, Title, Projects string }
	var lanes []laneOption
	for _, name := range policy.LaneNames {
		var in []string
		for _, p := range ps {
			if slices.ContainsFunc(p.Lanes, func(l policy.Lane) bool { return l.Name == name }) {
				in = append(in, p.Name)
			}
		}
		if in != nil {
			lanes = append(lanes, laneOption{name, laneTitles[name], strings.Join(in, " ")})
		}
	}
	c.render(w, "new.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf, "Factory": true,
		"Projects": ps, "Priorities": offered, "Lanes": lanes, "Form": form, "Flags": flags})
}

// createTask files the issue as the human (doFileCard). Text the
// sanitiser or the provider would change (roadmap 125) is not filed at
// once: the form answers the findings with everything the member wrote
// kept, and only its File as written button files the text as it stands,
// because a human may mean it.
func (c *Console) createTask(w http.ResponseWriter, r *http.Request) {
	form := newTask{Project: r.PostFormValue("project"), Title: r.PostFormValue("title"), Body: r.PostFormValue("body"),
		Lane: r.PostFormValue("lane"), Priority: r.PostFormValue("priority"), Size: r.PostFormValue("size")}
	if r.PostFormValue("as_written") == "" {
		if flags := filingFlags(form.Title, form.Body); len(flags) > 0 {
			c.renderNewTask(w, r, form, flags)
			return
		}
	}
	_, issue, err := c.doFileCard(r.Context(), button(r), form.Project, form.Title, form.Body, form.Lane, form.Priority, form.Size, "", "")
	if err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/?"+url.Values{"project": {form.Project}, "filed": {strconv.FormatInt(issue, 10)}}.Encode(), http.StatusSeeOther)
}

// answer posts the human's answer to an escalation (doAnswer). Words that
// matched none of the question's choices went to the chat (answerInChat):
// the member lands on it.
func (c *Console) answer(w http.ResponseWriter, r *http.Request) {
	note, err := c.doAnswerValue(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("answer"), r.PostFormValue("value"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	if note == answerInChatNote {
		http.Redirect(w, r, "/projects/"+url.PathEscape(r.PostFormValue("project"))+"/chat", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
}

// fundCard raises a waiting card's budget (doFund).
func (c *Console) fundCard(w http.ResponseWriter, r *http.Request) {
	eur, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("eur")), 64)
	if err != nil {
		c.fail(w, r, refuse(http.StatusBadRequest, "an amount in EUR above zero is required"))
		return
	}
	if _, err := c.doFund(r.Context(), button(r), r.PathValue("id"), eur, r.PostFormValue("reason")); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
}

// chatFund raises the project's chat task's monthly budget (doChatFund),
// the approver's answer to the spent-chat-budget ask.
func (c *Console) chatFund(w http.ResponseWriter, r *http.Request) {
	eur, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("eur")), 64)
	if err != nil {
		c.fail(w, r, refuse(http.StatusBadRequest, "an amount in EUR above zero is required"))
		return
	}
	if err := c.doChatFund(r.Context(), button(r), r.PathValue("name"), eur, r.PostFormValue("reason")); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
}

func (c *Console) forgejoFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	log.Printf("console: %s could not %s: %v", current(r).user, what, err)
	s, sErr := c.session(r)
	if sErr != nil {
		log.Printf("console: the session read: %v", sErr)
		http.Error(w, "cannot read the session", http.StatusServiceUnavailable)
		return
	}
	if s == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Error(w, "Forgejo refused to "+what+"; see the factoryd log", http.StatusBadGateway)
}

// writableCard is the card the path names, built as the board shows it
// under the URL's filter (?project, ?view); an unknown card is
// 404. The route's access-table rule has judged the writer. false: the
// answer is written.
func (c *Console) writableCard(w http.ResponseWriter, r *http.Request) (factoryd.BoardTask, card, *boardView, bool) {
	ctx, id := r.Context(), r.PathValue("id")
	bt, ok, err := c.cfg.Tasks.BoardTask(ctx, id)
	switch {
	case err != nil:
		log.Printf("console: card %s: %v", id, err)
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return bt, card{}, nil, false
	case !ok:
		http.NotFound(w, r)
		return bt, card{}, nil, false
	}
	v, _, err := c.viewOf(r, urlFilter(r))
	if err != nil {
		log.Printf("console: card %s: %v", id, err)
		http.Error(w, "cannot read the card", http.StatusInternalServerError)
		return bt, card{}, nil, false
	}
	v.byID[bt.ID] = bt.Task
	if bt.Status == "failed" {
		if rt, ok, err := c.cfg.Tasks.Task(ctx, factoryd.RetryID(bt.ID)); err == nil && ok {
			v.byID[rt.ID] = rt
		}
	}
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	return bt, k, v, true
}

// cardFragment answers GET /cards/{id} to a writer of the card's project,
// 404 to anyone else: the one card as the board renders it, its forms
// carrying the filter (?project, ?view) of the board it is
// swapped into.
func (c *Console) cardFragment(w http.ResponseWriter, r *http.Request) {
	if _, k, _, ok := c.writableCard(w, r); ok {
		c.render(w, "card", k)
	}
}

// drawerTab is a tab of the card drawer: its ?tab= and its title.
type drawerTab struct{ Name, Title string }

// drawerTabs are the card drawer's tabs: Session the worker's live
// session, the default of a running card, Overview of any other.
var drawerTabs = []drawerTab{{"overview", "Overview"}, {"chat", "Session"}, {"activity", "Activity"}, {"files", "Files"}}

// drawerData is the card drawer on one tab, under the card's face.
// Overview: the objective as its headline, a planned card's provenance
// line and its body, the plan's table, a pending merge's reviews and
// audit, and the related cards. Activity: the timeline page's gates and
// steps. Files: the pull request's changed files. Chat: the newest
// session's messages, and the input that steers it while it runs.
type drawerData struct {
	Card                       card
	Tab                        string
	Tabs                       []drawerTab
	Headline, Provenance, Body string
	PlanSummary                string
	PlanRows                   []factoryd.PlanRow
	Merge                      *approvalCard
	Related                    []related
	Gates                      []gateRow
	Longest                    int
	Entries                    []timelineEntry
	Files                      []prFile
	MoreFiles                  bool // Files are the first filesPages pages
	Session                    *sessionRef
	Said                       []said
	Steers                     bool   // the session runs and takes the member's messages
	Note                       string // what the member's action did
	Problem                    string // what the tab could not read
	// Runs are the card's life for the Time section (C10) and Life its
	// at-a-glance line; Need is the open question with its actions when
	// the card waits on a person.
	Runs []run
	Life string
	Need *need
	// Thread is the chat thread the card came from, nil for none.
	Thread *threadLink
	// Door is the card door: the card waits on a person and its own
	// thread is the viewer's or not bound yet (the main agent concept).
	Door bool
	// RoutineLine names the routine run that filed the card, nil for a
	// card no routine filed.
	RoutineLine *routineProvenance
}

// threadLink is the chat thread behind a card as its drawer names it:
// the member's whose it is, its number, and for the viewer's own thread
// its title, "" once deleted.
type threadLink struct {
	Number     int64
	Who, Title string
	Own        bool
}

// cardThread is the chat thread behind card t: the one its oldest event
// from the chat names; nil for none (roadmap 239's card link).
func (c *Console) cardThread(ctx context.Context, t factoryd.Task, viewer string) *threadLink {
	events, err := c.cfg.Events.TaskEvents(ctx, t.ID, startEvents)
	if err != nil {
		log.Printf("console: the events of %s: %v", t.ID, err)
		return nil
	}
	var l *threadLink
	for _, e := range events {
		if n, ok := e.Detail["thread"].(float64); ok && e.Via == factoryd.ViaChat && n > 0 {
			l = &threadLink{Number: int64(n), Who: e.Actor, Own: e.Actor == viewer}
		}
	}
	if l != nil && l.Own && c.cfg.Chat != nil {
		if th, ok, err := c.cfg.Chat.Conversation(ctx, t.Repository, viewer, l.Number); err == nil && ok {
			l.Title = cmp.Or(th.Title, "untitled")
		}
	}
	return l
}

// Age is the card's time in its stage, or in all once it ended.
func (d drawerData) Age() string {
	var parts []string
	if d.Card.StageAge != "" {
		parts = append(parts, d.Card.StageAge)
	}
	// the card's place in the merge queue, the one gate worth a word here;
	// the Time section holds the rest
	if strings.HasPrefix(d.Card.GateText, "merge queue") {
		parts = append(parts, strings.TrimSuffix(d.Card.GateText, ","))
	}
	if d.Card.Total != "" {
		parts = append(parts, d.Card.Total+" in all")
	}
	return strings.Join(parts, ", ")
}

// Word is the card's stage word, or its status.
func (d drawerData) Word() string {
	if d.Card.Stage != "" {
		return d.Card.Stage
	}
	return d.Card.Status
}

// said is a message of a session in the drawer's Chat: the agent's text
// between its tool calls, or a member's steer. Key is what the stream's
// copy of it carries, so the live one is not added twice.
type said struct {
	Key, Who, Text string
	Agent          bool
	At             time.Time
}

// related is a card the drawer's card relates to, Why, in its live state.
type related struct {
	Why  string
	Card card
}

// cardDrawer answers GET /cards/{id}/drawer?tab=overview|activity|files|chat
// to a writer of the card's project, 403 to anyone else.
func (c *Console) cardDrawer(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab != "" && !slices.ContainsFunc(drawerTabs, func(t drawerTab) bool { return t.Name == tab }) {
		http.Error(w, "tab must be overview, activity, files or chat", http.StatusBadRequest)
		return
	}
	c.drawer(w, r, tab, "")
}

// cardThreadStart answers POST /cards/{id}/thread, the card door: the
// card's own conversation, made for the member when the card has none
// (the main agent concept). Its first row, written by code from the
// card's open ask, holds the ask's five parts with its choices, so an
// answer through the thread settles the card as the inbox's buttons do;
// no model runs. The answer is the conversation's number.
func (c *Console) cardThreadStart(w http.ResponseWriter, r *http.Request) {
	bt, _, _, ok := c.writableCard(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := c.cfg.Chat.EnsureCardThread(ctx, bt.Repository, current(r).user, bt.ID); err != nil {
		log.Printf("console: the card thread of %s: %v", bt.ID, err)
		http.Error(w, "cannot make the card's thread", http.StatusInternalServerError)
		return
	}
	who, n, _, err := c.cfg.Chat.CardThreadFor(ctx, bt.Repository, bt.ID)
	if err != nil {
		log.Printf("console: the card thread of %s: %v", bt.ID, err)
		http.Error(w, "cannot read the card's thread", http.StatusInternalServerError)
		return
	}
	if rows, err := c.cfg.Chat.ChatMessages(ctx, bt.Repository, who, n, 1); err == nil && len(rows) == 0 {
		c.seedCardAsk(ctx, bt, who, n)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"conversation":%d,"owner":%q}`, n, who)
}

// seedCardAsk writes the card's open ask as its thread's first row: the
// ask's five parts with its choices, the row the thread's answer buttons
// settle.
func (c *Console) seedCardAsk(ctx context.Context, bt factoryd.BoardTask, who string, n int64) {
	events, err := c.cfg.Events.TaskEvents(ctx, bt.ID, startEvents)
	if err != nil {
		log.Printf("console: the events of %s: %v", bt.ID, err)
		return
	}
	var ask *factoryd.Event
	for i := range events { // newest first
		if events[i].Kind == factoryd.EventQuestionAsked {
			ask = &events[i]
			break
		}
	}
	if ask == nil {
		return
	}
	if s := factoryd.SettleOf(factoryd.ChatMessage{Kind: factoryd.MessageEscalation,
		BirthKind: factoryd.EventQuestionAsked, At: ask.At}, events, bt.Status == "waiting-human", false); !s.Open {
		return
	}
	question, _ := ask.Detail["ask"].(string) // the wake path writes "ask", older rows "question"
	if question == "" {
		question, _ = ask.Detail["question"].(string)
	}
	ref := ask.TaskID
	switch {
	case bt.IssueNumber != 0:
		ref = "#" + strconv.FormatInt(bt.IssueNumber, 10)
	case ask.IssueNumber != 0:
		ref = "#" + strconv.FormatInt(ask.IssueNumber, 10)
	}
	content := ref + " needs you: " + question
	for _, part := range []struct{ label, key string }{{"Why", "why"}, {"Info", "info"}, {"Recommendation", "recommendation"}} {
		if s, _ := ask.Detail[part.key].(string); s != "" {
			content += "\n" + part.label + ": " + s
		}
	}
	m := factoryd.ChatMessage{Role: "user", Kind: factoryd.MessageEscalation, BirthKind: factoryd.EventQuestionAsked,
		Content: content, TaskID: bt.ID, Conversation: n}
	switch cs := ask.Detail["choices"].(type) {
	case []any:
		for _, ch := range cs {
			if s, ok := ch.(string); ok {
				m.Choices = append(m.Choices, s)
			}
		}
	case []string:
		m.Choices = append(m.Choices, cs...)
	}
	if _, err := c.cfg.Chat.AddChatMessage(ctx, bt.Repository, who, m); err != nil {
		log.Printf("console: the card thread of %s: %v", bt.ID, err)
	}
}

// steerCard hands the member's message from the drawer's Chat to card
// id's running session (doSteer) and answers the tab again with its note.
func (c *Console) steerCard(w http.ResponseWriter, r *http.Request) {
	note, err := c.doSteer(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("text"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.drawer(w, r, "chat", note)
}

// drawer renders the card drawer on tab, "" for the card's default, with
// note, what the member's action did.
func (c *Console) drawer(w http.ResponseWriter, r *http.Request, tab, note string) {
	bt, k, v, ok := c.writableCard(w, r)
	if !ok {
		return
	}
	if tab == "" {
		tab = "overview"
		if bt.Status == "running" {
			tab = "chat"
		}
	}
	ctx := r.Context()
	d := drawerData{Card: k, Tab: tab, Tabs: drawerTabs, Note: note}
	// the drawer's head carries the title as filed, whole: the objective's
	// first line, unless a writer's summary or a plan key names the card
	if headline, _, _ := objectiveParts(bt.Objective); headline != "" && bt.HumanSummary == "" && bt.PlanKey == "" {
		d.Card.Title = headline
	}
	problem := func(what string, err error) {
		log.Printf("console: the drawer of %s could not read %s: %v", bt.ID, what, err)
		d.Problem = "The " + what + " could not be read; see the factoryd log."
	}
	switch tab {
	case "overview":
		d.Headline, d.Provenance, d.Body = objectiveParts(bt.Objective)
		if bt.Lane == policy.LanePlan {
			var err error
			if _, d.PlanSummary, d.PlanRows, _, err = c.planTable(ctx, bt.ID, 0); err != nil {
				problem("plan", err)
			}
		}
		for _, a := range v.pending {
			if a.TaskID == bt.ID && a.Kind == factoryd.ApprovalMerge {
				m := c.approvalCard(ctx, a, button(r), v.byID, v.csrf, v.approves(a.Project), false)
				m.Filter, m.IDSuffix = k.Filter, "-d"
				m.Writer = true // the drawer's route already asked for a writer
				d.Merge = &m
			}
		}
		if v.escalated(bt.Task) && bt.Escalation != "" {
			d.Need = &need{Card: k, Escalated: true, CSRF: v.csrf, Approver: c.approver(r), Back: "/" + k.Filter.query(), Suffix: "-d"}
		}
		d.Runs = c.lifeOf(ctx, bt.Task)
		d.Life = lifeLine(d.Runs, bt.SpentEUR)
		d.Thread = c.cardThread(ctx, bt.Task, current(r).user)
		if v.escalated(bt.Task) && c.cfg.Chat != nil {
			// the card door: its own thread, unbound for the first
			// member to start, bound for every member to open
			d.Door = true
		}
		var err error
		if bt.Source == factoryd.SourceRoutine && c.cfg.Routines != nil {
			if d.RoutineLine, err = c.routineLineOf(ctx, bt.ID); err != nil {
				problem("routine", err)
			}
			// a human card's body ends in the line factoryd filed it with, which the routine line says
			if i := strings.LastIndex(d.Body, "\n"); d.RoutineLine != nil && strings.HasPrefix(d.Body[i+1:], factoryd.RoutineProvenance) {
				d.Body = strings.TrimSpace(d.Body[:max(i, 0)])
			}
		}
		if d.Related, err = c.relatedCards(ctx, bt.Task, k); err != nil {
			problem("related cards", err)
		}
	case "activity":
		steps, err := c.readTimeline(ctx, bt.ID)
		if err != nil {
			problem("timeline", err)
			break
		}
		for _, m := range steps {
			d.Entries = append(d.Entries, entry(m))
		}
		d.Gates, d.Longest = gateRows(steps, bt.Task, time.Now())
	case "files":
		if bt.PRNumber == 0 {
			break
		}
		var err error
		if d.Files, d.MoreFiles, err = c.prFiles(r, bt.Repository, bt.PRNumber); err != nil {
			problem("pull request's files", err)
		}
	case "chat":
		var err error
		if d.Session, d.Said, err = c.sessionChat(r, bt.ID); err != nil {
			problem("session", err)
		}
		d.Steers = d.Session != nil && d.Session.Ended.IsZero() && bt.Status == "running" && c.cfg.Steer != nil
	}
	c.render(w, "drawer.html", d)
}

// sessionChat is card id's newest session, nil before its first, and its
// messages in time order: the agent's text of each model call in the
// transcript, and the steers among the card's events from the session's
// start to its end.
func (c *Console) sessionChat(r *http.Request, id string) (*sessionRef, []said, error) {
	steps, err := c.readTimeline(r.Context(), id)
	all := sessionsOf(steps)
	if err != nil || len(all) == 0 {
		return nil, nil, err
	}
	s := sessionAt(all, 0)
	body, err := (&turn{c: c, r: r}).transcript(id, s)
	var no *refusal
	if err != nil && !(errors.As(err, &no) && no.status == http.StatusNotFound) {
		return &s, nil, err
	}
	var out []said
	eachStep(body, func(_ int, ev harness.Event) {
		if text := textAt(ev.Detail, "text"); ev.Kind == harness.EventModelCall && text != "" {
			out = append(out, said{Key: s.ID + "@" + ev.Time.UTC().Format(time.RFC3339Nano), Who: "agent", Text: text, Agent: true, At: ev.Time})
		}
	})
	events, err := c.cfg.Events.TaskEvents(r.Context(), id, startEvents)
	for _, e := range events {
		if e.Kind == factoryd.EventCardSteered && !e.At.Before(s.Started) && (s.Ended.IsZero() || !e.At.After(s.Ended)) {
			out = append(out, said{Key: fmt.Sprintf("e%d", e.ID), Who: e.Actor, Text: textAt(e.Detail, "text"), At: e.At})
		}
	}
	slices.SortStableFunc(out, func(a, b said) int { return a.At.Compare(b.At) })
	return &s, out, err
}

// objectiveParts is an objective as the drawer shows it: its first line
// the headline, a planned card's "Card k of plan #n ..." line its
// provenance, the rest its body.
func objectiveParts(objective string) (headline, provenance, body string) {
	headline, body, _ = strings.Cut(strings.TrimSpace(objective), "\n")
	body = strings.TrimSpace(body)
	if line, rest, _ := strings.Cut(body, "\n"); strings.HasPrefix(line, "Card ") && strings.Contains(line, " of plan #") {
		provenance, body = strings.TrimSpace(line), strings.TrimSpace(rest)
	}
	return strings.TrimSpace(headline), provenance, body
}

// relatedCards are the cards t relates to, each in its live state: those
// it waits on that are not done, the plan it is part of, and an accepted
// plan's cards (k's Plan).
func (c *Console) relatedCards(ctx context.Context, t factoryd.Task, k card) ([]related, error) {
	var ids, whys []string
	deps, err := c.cfg.Tasks.Dependencies(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, dep := range deps {
		if dep.Status != "done" {
			ids, whys = append(ids, dep.TaskID), append(whys, "blocked by")
		}
	}
	if t.Source == factoryd.SourcePlan && t.Parent != "" {
		ids, whys = append(ids, t.Parent), append(whys, "part of plan")
	}
	var out []related
	for i, id := range ids {
		rt, ok, err := c.cfg.Tasks.Task(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, related{whys[i], c.card(rt)})
		}
	}
	if k.Plan != nil {
		for _, child := range k.Plan.Cards {
			out = append(out, related{"child", child})
		}
	}
	return out, nil
}

// prFile is a changed file of a pull request, as Forgejo lists it.
type prFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// filesPages bounds the pages of a pull request's files the drawer reads:
// Forgejo computes the diff again for every page.
const filesPages = 10

// prFiles are pull request pr's changed files, read through the human's
// token, up to filesPages pages; more when Forgejo may list others.
func (c *Console) prFiles(r *http.Request, repository string, pr int64) ([]prFile, bool, error) {
	repo, err := factoryd.SplitRepo(repository)
	if err != nil {
		return nil, false, err
	}
	return factoryd.GetPagesUpTo[prFile](r.Context(), c.userCall(current(r)),
		fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/files?", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), pr), filesPages)
}
