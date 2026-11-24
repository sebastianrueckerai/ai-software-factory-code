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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

var funcs = template.FuncMap{
	"build": func() string { return build },
	// markup is the writer's markdown as HTML, for the drawer's question
	// text; askline is a text's first line, what a card's face shows
	"markup": markup, "askline": askline,
	"eur":   textutil.EUR,
	"rate":  textutil.Rate,
	"short": factoryd.ShortSHA,
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
	// the metrics page's rows (C30): the deck's formatters, the weeks'
	// spark, and the window-move subtraction
	"mDur": mDur, "mDurMove": mDurMove, "mPct": mPct, "mPctMove": mPctMove,
	"mEur": mEur, "mEurMove": mEurMove, "mCount": mCount, "mCountMove": mCountMove,
	"mK": mK, "mDec": mDec, "mDecMove": mDecMove, "mShare": mShare, "mDiff": mDiff,
	"mChat": mChat, "mSessionTime": mSessionTime, "msub": msub, "sparkSVG": sparkSVG,
	// the divide-and-conquer panels (roadmap 336): the deck's own
	// formatters above and the section's lists and coverage notes
	"mEur4": mEur4, "mEur4Move": mEur4Move, "mHours": mHours, "mHoursMove": mHoursMove,
	"mSizes": mSizes, "mNamed": mNamed, "mIntercepts": mIntercepts,
	"mRoundName": mRoundName, "mRoundSpans": mRoundSpans, "mOriginSpans": mOriginSpans,
	"mGateCov": mGateCov, "mSessCov": mSessCov, "mEconCov": mEconCov,
	"mGroupCov": mGroupCov, "mHoldCov": mHoldCov, "mReviewCov": mReviewCov,
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
	// is the gauge's class: warn from 80 percent, over at the cap or when
	// the gateway refused a call at it (hit)
	"pct":    pct,
	"pct100": func(spent, cap float64) int { return min(pct(spent, cap), 100) },
	// initial is a member's avatar letter
	"initial": func(s string) string { return strings.ToUpper(string([]rune(s)[:min(1, len([]rune(s)))])) },
	// words is a stored word of the factory's, its underscores spaces
	"words": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
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
	"gaugeClass": func(spent, cap float64, hit bool) string {
		switch {
		case policy.CapHit(spent, cap, hit):
			return " over"
		case pct(spent, cap) >= 80:
			return " warn"
		}
		return ""
	},
	"inc":  func(i int) int { return i + 1 },
	"join": strings.Join,
}

// alertSupervisor is what the supervisor did on a machine alert.
func alertSupervisor(a factoryd.Alert) supervised {
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

// The project screen's four columns (as C8 amends it), a view
// over the task states and never a state of their own: In progress holds
// running, closing and in review, the stage word on the card; the fine
// state is a chip on the card.
const (
	colNeedsYou = iota
	colQueued
	colProgress
	colDone
)

var columnTitles = []string{"Needs you", "Queued", "In progress", "Done"}

// columnHolds are the statuses each of the four columns holds, which its
// data-holds names for a card's action to move the card to.
var columnHolds = [][]string{
	colNeedsYou: {"waiting-human", "waiting-poster"},
	colQueued:   {"queued", "waiting-deps", "waiting-image"},
	colProgress: {"running", "closing", "waiting-agent", "in-review", "merged"},
	colDone:     {"done", "failed", "closed"},
}

// fiveColumn is t's column, -1 for a status none holds: Needs you holds
// every card whose next actor is a person, its author's wait included.
func fiveColumn(t factoryd.Task) int {
	for i, holds := range columnHolds {
		if slices.Contains(holds, t.Status) {
			return i
		}
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
		return k.Blocked > 0 || k.WaitingOn != "" || k.HoldLine != ""
	case "exceptional":
		return k.Stale || k.Provider || k.NearBudget || k.Retrying
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
// the face says (faceWords), or the title of a card whose face asks, a
// key:value term a fact behind the face (plan:558, stage:verifying,
// status:failed, is:failed and the other exceptional states,
// by:sebastian, model:small, pr:612), and every term must match. Each column keeps its whole count;
// the columns given stay as they were, for the column menus.
func find(cols []column, q string) []column {
	terms := strings.Fields(strings.ToLower(q))
	out := make([]column, len(cols))
	for i, col := range cols {
		col.Total = col.Count()
		if len(terms) > 0 {
			var cards []card
			for _, k := range col.Cards {
				if found(faceWords(k, nil, k.Face), k, nil, terms) {
					cards = append(cards, k)
				}
			}
			// a card whose face asks shows its title where only the title
			// matched, so the reader sees why it is in the result
			var needs []need
			for _, n := range col.Needs {
				words, title := faceWords(n.Card, n.Approval, n.Face), factoryd.CardTitle(n.Card.Task)
				switch {
				case found(words, n.Card, n.Approval, terms):
				case n.Approval == nil && title != "" && found(words+" "+title, n.Card, nil, terms):
					n.Face.Found = n.Face.own
				default:
					continue
				}
				needs = append(needs, n)
			}
			col.Cards, col.Needs = cards, needs
		}
		out[i] = col
	}
	return out
}

// found is whether words, what an item's face says, and the facts of
// card k, or of the approval item a, match every term.
func found(words string, k card, a *approvalCard, terms []string) bool {
	text := strings.ToLower(words)
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

// faceWords is what face f of card k, or of the approval item a, says,
// for a word of the find to match, with the item's numbers.
func faceWords(k card, a *approvalCard, f faceView) string {
	w := []string{f.text()}
	if a != nil {
		switch a.Kind {
		case factoryd.ApprovalMerge:
			w = append(w, fmt.Sprintf("merge pr #%d", a.PRNumber), a.Merge.RiskClass+" risk", a.TaskID)
			if a.Merge.Migration {
				w = append(w, "migration")
			}
		case factoryd.ApprovalPlan:
			w = append(w, fmt.Sprintf("plan #%d %s %s %s", a.Issue, a.PlanName, a.PlanSummary, a.TaskID))
		default:
			w = append(w, "resource request", a.Resource.ServerType, a.Resource.Purpose, a.TaskID)
		}
		return strings.Join(w, " ")
	}
	w = append(w, k.ID)
	if k.IssueNumber > 0 {
		w = append(w, fmt.Sprintf("#%d", k.IssueNumber))
	}
	if k.PRNumber > 0 {
		w = append(w, fmt.Sprintf("pr #%d", k.PRNumber))
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
		if val == "" {
			return false, true
		}
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			return n > 0 && (k.PlanIssue == n || k.Track == policy.TrackPlan && k.IssueNumber == n), true
		}
		return strings.Contains(strings.ToLower(cmp.Or(k.PlanName, k.Task.PlanName)), strings.ToLower(val)), true
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
		return k.Blocked > 0 || k.WaitingOn != "" || k.HoldLine != ""
	case "stale":
		return k.Stale
	case "provider":
		return k.Provider
	case "near":
		return k.NearBudget
	case "retrying":
		return k.Retrying
	}
	return false
}

type card struct {
	factoryd.Task
	// HeldFor is the issue of the open plan this card waits for, 0 for none.
	HeldFor  int64
	IssueURL string
	PRURL    string
	// RollID is the roll task a merged card waits for.
	RollID string
	Reason string // an escalation's reason, which the find reads
	// Question is the escalation's question: a human card's ask is its
	// title, and the activity line names it. nil on a card whose ask no
	// template shaped.
	Question *factoryd.HumanQuestion
	// MayExtend: the viewer may give the live agent card more room,
	// MayReplan send it back to the planner, MayRetry file the failed plan
	// card again; each an approver's action.
	MayExtend, MayReplan, MayRetry bool
	// Total is the task's time so far, or in all once it finished.
	Total string
	// Cover is a done card's cover clause (C38): the run that ran its
	// change live and how long after its merge, "" for a card the chain
	// does not cover.
	Cover string
	// Stage is the work stage of a card in progress (C8): planning on a
	// plan-track card, building on a direct one, verifying in review, ""
	// in any other status. StageAge is how long the card has been in it.
	// StageURL is the chain word's link, the run on the forge the word
	// names (C38), "" for a word that names none.
	Stage, StageAge, StageURL string
	WaitingOn                 string // a card in waiting-deps: the issues it waits on
	SlotWait                  string // a card queued for a work slot: "waits for its turn to start, N ahead (L running)"
	PlanIssue                 int64  // a plan's card: the plan's issue
	PlanURL                   string // a plan's card: the plan's issue on the forge
	PlanName                  string // a plan's card: the plan's name, on the face's plan chip
	PlanKey                   string // a plan's card: its key in the plan, on the face's plan chip
	Activity                  string // the task's newest event as one line, or the escalation's question
	WaitingForActivity        bool   // the activity line stands for the wait's reason
	// HoldLine is a human's hold on the card as the board words it, ""
	// for none.
	HoldLine string
	// Title is the face's first line: the card's title, the objective's
	// first paragraph whole (factoryd.CardTitle), or a human card's ask.
	Title string
	// StatusClass is the pill's class, one hue per state.
	StatusClass string
	// Checks is "n/n" from the task's newest checks step, "" on a card
	// without a pull request, a final one, or one whose timeline holds none.
	Checks string
	// Attempt is the newest session's number, shown beside the stage
	// clock, which the gate's restarts would else read as progress.
	Attempt string
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
	// Blocked is how many cards this one depends on that are not done;
	// BlockedOn names their issues in its title attribute ("#3, #5").
	Blocked   int
	BlockedOn string
	// Retrying is a card whose newest session ended for a retry and whose
	// next has not started; Provider a running one whose next session
	// waits on the model provider (onProvider), and Frozen the freeze it
	// waits in, the provider's words and the reset it named, "" on the
	// ladder's short backoff; Fine are its fine states as chips.
	Retrying bool
	Provider bool
	Frozen   string
	Fine     []fine
	// endAt is the moment the card's clocks were aged to: now, or its
	// end once it finished.
	endAt time.Time
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
	// QueueAhead counts the cards ahead of it in its merge queue, -1 when
	// it waits after the open plan; MergeBehind is the issue of the card
	// that holds the queue's turn, 0 at the front.
	QueueAhead  int
	MergeBehind int64
	// Failure is a failed card's failure in one clause, "" when its end
	// named none that fits one.
	Failure string
	// Face is the card's small card (faceOf).
	Face faceView
	// Helper is the card's newest helper task that has not ended, nil for
	// none: the step its state names.
	Helper *factoryd.Task
}

// Routine: a routine's run filed the card, which wears the chip (C16).
func (k card) Routine() bool { return k.Source == factoryd.SourceRoutine }

// fine is a card's fine state as a chip in the pill's hues.
type fine struct{ Words, Class string }

// fineStates are what k's column and pill leave unsaid: retrying, waiting
// on the author and stale. Failed and closed are the pill's own words.
func fineStates(k card) []fine {
	var out []fine
	switch {
	case k.Provider:
		// a named limit carries the words and the reset it froze the card to
		out = append(out, fine{cmp.Or(k.Frozen, providerWords), "st-waiting"})
	case k.Retrying:
		out = append(out, fine{"retrying", "st-running"})
	case k.Status == "waiting-poster":
		out = append(out, fine{"waiting on the author", "st-waiting hollow"})
	}
	if k.Stale {
		out = append(out, fine{"stale", "st-stale"})
	}
	return out
}

// statusClasses are the pill's classes, one hue per state: running the
// accent blue, a wait amber, review violet, merged teal, done green,
// failed red, closed grey; waiting-poster wears the hollow pill. closing
// and waiting-agent, the factory agent's turn on a human card, count as
// running.
var statusClasses = map[string]string{
	"running": "st-running", "closing": "st-running", "waiting-agent": "st-running", "queued": "st-queued",
	"waiting-human": "st-waiting", "waiting-poster": "st-waiting hollow",
	"in-review": "st-review", "merged": "st-merged", "done": "st-done", "failed": "st-failed",
	"closed":       "st-closed",
	"waiting-deps": "st-waiting", "waiting-image": "st-waiting",
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

// pushedHead is the task's newest pushed step's commit, "" when the
// steps hold no push.
func pushedHead(steps []map[string]any) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if s, _ := steps[i]["step"].(string); s == "pushed" {
			commit, _ := steps[i]["commit"].(string)
			return commit
		}
	}
	return ""
}

// newestChecksCount is the task's newest checks step that judged head,
// the newest pushed commit: passed over total of its jobs; "" when the
// steps hold none, it named no counts, or a newer push has run no checks
// on it yet. With no push at all, the newest checks step stands on its
// own, nothing supersedes it.
func newestChecksCount(steps []map[string]any, head string) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if s, _ := steps[i]["step"].(string); s != "checks" {
			continue
		}
		if head != "" {
			if commit, _ := steps[i]["commit"].(string); commit != head {
				continue
			}
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

// attemptOf is the task's newest session's number, "" when no session
// started or the step named no attempt.
func attemptOf(steps []map[string]any) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if s, _ := steps[i]["step"].(string); s == "session_start" {
			a, _ := steps[i]["attempt"].(float64)
			if a < 1 {
				return ""
			}
			return strconv.Itoa(int(a))
		}
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

// providerWords are the chip of a card whose next session waits on the
// model provider.
const providerWords = "waiting on the provider"

// onProvider: the newest provider_refused step of steps, a refused
// session's wait on the model provider, has no session_start after it.
func onProvider(steps []map[string]any) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		switch steps[i]["step"] {
		case "session_start":
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

// checks is a card's remembered checks count, retry and wait on the
// provider, the newest timeline step's time, and when they were read.
type checks struct {
	count              string
	attempt            string
	retrying, provider bool
	frozen             string
	freeze             []frozenSpan
	stepAt             time.Time
	at                 time.Time
}

// faceTimelines reads the newest checks step onto every card that can
// have one, a card with a pull request, and whether it is retrying or
// waits on the provider onto every running card: neither final, at most
// once per checksTTL. A running card frozen on a named provider limit
// carries the freeze's words and reset, and its stage and total clocks
// split the frozen time out. A timeline that cannot be read keeps the
// card without the chips — the
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
		k.Attempt = got.attempt
		k.Retrying, k.Provider = got.retrying, got.provider && k.Status == "running"
		if k.Provider {
			k.Frozen = got.frozen
		}
		k.splitClocks(got.freeze)
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
		got.count, got.attempt, got.retrying, got.provider = newestChecksCount(steps, pushedHead(steps)), attemptOf(steps), retrying(steps), onProvider(steps)
		got.freeze, got.frozen = freezeOf(steps, c.clock())
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

// planIssue is plan task id's issue, its name (its title while it has
// none) and its repository: from the board's tasks, else read.
func (c *Console) planIssue(ctx context.Context, byID map[string]factoryd.Task, id string) (int64, string, string) {
	t, ok := byID[id]
	if !ok {
		var err error
		if t, ok, err = c.cfg.Tasks.Task(ctx, id); err != nil || !ok {
			log.Printf("console: plan %s: found %v, %v", id, ok, err)
			return 0, "", ""
		}
		byID[id] = t
	}
	return t.IssueNumber, cmp.Or(t.PlanName, factoryd.CardTitle(t)), t.Repository
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

type column struct {
	Title string
	Holds string // the statuses the column holds, space-separated
	// Total is the column's count before the find narrowed it (C13); the
	// head reads "n of m" when they differ.
	Total int
	// Menu is the column's menu (C13) in the four columns: Queued, In
	// progress and Done have one, Needs you none.
	Menu  *colMenu
	Cards []card
	// Needs is Needs you's items: the approvals, the cases at the human
	// and every card that needs the viewer, oldest first.
	Needs []need
	// Finished is a column of finished cards' key, the status it holds
	// (done for the four columns' Done, which holds all three): the col of
	// its next page and the name of its <key>-shown. Whole is how many
	// finished cards it has when Cards holds the newest pages of them, and
	// More the form of the next page, nil when Cards holds them all.
	Finished string
	Whole    int
	More     *more
	// Ghosts are the faces of the column's routines at its foot (C16),
	// and GhostsHidden those the board's routines choice hides.
	Ghosts       []faceView
	GhostsHidden int
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

// need is an item of the Needs you column: an approval request, a card with its case at the human and its answer form
// (CSRF), or a card that needs the viewer; Since is when it began to wait.
type need struct {
	Since    time.Time
	Approval *approvalCard
	Card     card
	CSRF     string
	Case     *caseRow // the case the row shows, for a card that holds one at the human
	Suffix   string   // on the ids of the item's fields: "-d" in the drawer, so the column's copy keeps its own
	Face     faceView // the item's small card (faceOf)
}

// Waiting is how long the item has waited for a member, in minutes or
// hours and minutes; empty when the item's start is unknown.
func (n need) Waiting() string {
	if n.Since.IsZero() || n.Case != nil { // a case row waits as its case does, in its head
		return ""
	}
	return minutes(time.Since(n.Since))
}

// boardFilter is what the board was asked to show: the project, the find
// and the columns' choices; every form on it carries it back, so an action
// keeps the view.
type boardFilter struct {
	Project string
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
func (f boardFilter) whole() bool { return f.Q != "" || f.Done.Only != "" }

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
	if n, err := strconv.Atoi(get("done-shown")); err == nil && n > 0 {
		f.Shown = map[string]int{"done": n}
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
	fields := []hidden{{"project", f.Project}, {"q", f.Q}}
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
	f := boardFilter{Project: r.PostFormValue("project"), Q: strings.TrimSpace(r.PostFormValue("q"))}
	readCols(&f, r.PostFormValue)
	return f
}

// urlFilter is the board's filter in the URL's query.
func urlFilter(r *http.Request) boardFilter {
	q := r.URL.Query()
	f := boardFilter{Project: q.Get("project"), Q: strings.TrimSpace(q.Get("q"))}
	readCols(&f, q.Get)
	return f
}

// query is the filter as a URL query, empty when it names nothing.
func (f boardFilter) query() string {
	q := url.Values{}
	if f.Project != "" {
		q.Set("project", f.Project)
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

// statusLine is the shown project's state above the board,
// which the project agent reads too: its word, its cards working, those
// waiting on the model provider, what needs the viewer, its spend this
// UTC month against its month cap — its own spend under its file's cap,
// the factory's enforced spend under the policy's — and its GitHub
// export's state word.
type statusLine struct {
	Project string
	// Filter is the board's own, which the panel's forms carry back so
	// an action keeps the view (the cards' forms carry their card's).
	Filter                    boardFilter
	State                     string
	Working, Waiting, NeedYou int
	// Spend is the month's count against CapEUR, the month cap: since
	// Spend.Reset, the newest reset of that cap this month, when one
	// restarted it, Spend.AllEUR the month's whole.
	Spend  factoryd.ProjectSpend
	CapEUR float64
	// FactoryCap: the month cap is the policy's factory-wide one, not a
	// project file's, so the spend it stands next to is the factory's
	// enforced month spend, not the project's own.
	FactoryCap bool
	// TodayEUR is the factory's enforced spend this UTC day, holds
	// included, counted since DayReset when a human reset the day's count,
	// and DayCapEUR the policy's day cap. DayReset and MonthReset are the
	// resets in force as the strip and the panel say them, nil for none.
	TodayEUR, DayCapEUR  float64
	DayReset, MonthReset *resetLine
	// DayHit and PremiumHit: the cap is hit (policy.CapHit); the month's
	// is Spend.Hit.
	DayHit, PremiumHit bool
	// DayPress and MonthPress are the Reset of the day's and the month's
	// cap, nil for a viewer who may not reset it.
	DayPress, MonthPress *capPress
	// Backstop is LiteLLM's own budget, nil when unread; BackstopHit says
	// its spend reached its max.
	Backstop    *factoryd.Backstop
	BackstopHit bool
	// PremiumEUR is the factory's spend on the premium class this UTC
	// month and PremiumCapEUR its cap, budget.premium_month_eur; a cap of
	// 0 shows no premium meter.
	PremiumEUR, PremiumCapEUR float64
	GitHub                    string
	GitHubURL                 string // the export's remote on GitHub, "" when not exported
	// RoutineCell is the strip's routine cell (C16), the first held run
	// ("↻ bump held", RoutineHeld) else the soonest under an hour away
	// ("↻ bump in 20 min"), "" for none.
	RoutineCell string
	RoutineHeld bool
	// ChatSpent: the chat's budget stopped it this month (chatStop),
	// which stands the chat until a raise (the Fund the chat form or the
	// chat's own) or the month's turn; ChatTask is its task id. Approver:
	// the viewer may fund it.
	ChatSpent bool
	// Pool is the project's escalation pool (ProjectPool): the same
	// projection the chat's status line and the board read tool read, so
	// the face and the chat agree on its figures.
	Pool     poolLine
	ChatTask string
	Approver bool
	// Dependencies are the factory's own dependencies' latest health with
	// each row's age, the panel's list (roadmap 299); the strip's one dot
	// carries the worst of them: DepsDown red, DepsDegraded amber, DepsUp
	// green (Sebastian, 2026-10-19).
	Dependencies []depRow
	DepsDown     bool
	DepsDegraded bool
	DepsUp       bool
	// Hold is what holds the project's production, nil when nothing does.
	Hold *holdLine
}

// resetLine is a reset in force of a cap: the clause its strip cell's
// title appends and the trace line under its row in the panel.
type resetLine struct{ Title, Trace string }

// resetLineOf is reset r of the cap counted in period ("today", "this
// month"), whose whole is all, as read at now; nil for no reset.
func resetLineOf(r *policy.CapReset, all float64, period string, now time.Time) *resetLine {
	if r == nil {
		return nil
	}
	at, before := factoryd.ResetStamp(r.At, now), textutil.EUR(r.BeforeEUR)
	return &resetLine{Title: fmt.Sprintf("; %s reset its count at %s, %s EUR before", r.Login, at, before),
		Trace: fmt.Sprintf("counted since %s, when %s reset %s · %s EUR before · %s EUR in all %s", at, r.Login, period, before, textutil.EUR(all), period)}
}

// capPress is a cap's Reset under its row in the panel: the route its
// confirm posts to, the summary's label, the confirm's words and the cap
// they name, its row's hit, the form's CSRF and filter, and Seen, the id
// of the cap's newest reset of the period the panel shows, 0 for none.
type capPress struct {
	Action, Label, Words string
	CapEUR               float64
	Hit                  bool
	CSRF                 string
	Filter               boardFilter
	Seen                 int64
}

// holdLine is the hold as the strip and the panel show it.
type holdLine struct {
	For, Reason string
	Groups      []string
	// Released is the standing Deploy now whose commit is still main's
	// head: the panel says it rolls and offers no form.
	Released *releaseStanding
}

// releaseStanding is a Deploy now window whose commit is main's head.
type releaseStanding struct {
	Commit, Age string
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
	User string
	CSRF string
	Page string // the layout marks the bar's current destination: the board
	// boardFilter is the board's filter, which every form on it carries
	// back so an action keeps the view.
	boardFilter
	Query      string   // the filter as a URL query for the poll's hx-get
	Writes     bool     // the viewer writes the shown project: its drawer and its stream's rows are theirs
	Chat       bool     // the project agent's panel shows beside the four columns, its input to a writer
	Messengers bool     // the layout's notifications link
	ClearQuery string   // the filter without the find, the ×'s link (C13)
	whole      []column // the columns before the find narrowed them, the menus'
	Projects   []string // the projects the viewer writes, and the shown one
	Status     statusLine
	Columns    []column
	Problems   []string
	Approver   bool // may authorize the shown project's spend
	Factory    bool // writes the factory's own code: the layout links the factory-level pages
	// Autonomy is the shown project's autonomy state, which the chat's
	// level control shows and an approver changes (roadmap 239, C14), and
	// Levels the four states with their words; empty: no policy in force.
	Autonomy string
	Levels   []level
	// IntakeClosed is the shown project's intake switch (the chat
	// header's): closed stops exactly the roadmap pull, every other
	// filing still files; IntakeBy and IntakeAt name its last change.
	IntakeClosed bool
	IntakeBy     string
	IntakeAt     time.Time
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

// Final is a done, failed or closed card: a two-line row on the board.
func (k card) Final() bool { return factoryd.Final(k.Status) }

// ChainHue is the merged card's chain word's hue: red where the chain
// failed, amber while it stalls, the merged teal while it runs.
func (k card) ChainHue() string {
	switch k.Stage {
	case "chain red":
		return "st-failed"
	case "stalled":
		return "st-waiting"
	}
	return "st-merged"
}

// configTree is commit's tree of the configuration directory in Forgejo,
// as /policy, the board and the resources page link it.
func (c *Console) configTree(commit string) string {
	return c.cfg.ConfigRepo + "/src/commit/" + url.PathEscape(commit) + "/" + c.cfg.ConfigDir
}

// runURL is a chain run's page on the forge.
func (c *Console) runURL(repository string, run int64) string {
	return fmt.Sprintf("%s/%s/actions/runs/%d", strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/"), repository, run)
}

// counts are the board's column counts in the page's order.
func (c *Console) counts(d boardData) ([]string, error) {
	var out []string
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
// stream to compare: each face's stale ring, wait on the provider, retry,
// checks and minute ages.
func (d boardData) faces() string {
	var b strings.Builder
	add := func(k card) {
		fmt.Fprintf(&b, "%s %t %q %t %t %s %s %s %s\n", k.ID, k.Stale, k.StaleText, k.Provider, k.Retrying, k.Checks, k.Total, k.StageAge, k.Attempt)
	}
	for _, col := range d.Columns {
		for _, k := range col.Cards {
			add(k)
		}
		for _, g := range col.Ghosts {
			fmt.Fprintf(&b, "ghost %s %s\n", g.Routine.Key, g.Routine.Next)
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
	end := c.clock()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	if !t.CreatedAt.IsZero() {
		k.Total = minutes(end.Sub(t.CreatedAt))
	}
	k.StatusClass = statusClass(t.Status)
	k.endAt = end
	k.Stage, k.StageAge = stage(t, end)
	k.Title = factoryd.CardTitle(t)
	k.PlanKey = t.PlanKey
	if t.BudgetEUR > 0 {
		k.NearBudget = nearBudget(t.EnforcedSpent(), t.BudgetEUR)
	}
	return k
}

// stage is t's work stage and how long it has been in it at now:
// planning or building since the admission, verifying since its gate
// began, else since the admission; a card never admitted counts from its
// filing.
func stage(t factoryd.Task, now time.Time) (string, string) {
	word := runKind(t.Status, t.Track)
	if !slices.Contains([]string{"planning", "building", "verifying"}, word) {
		return "", ""
	}
	if since := stageStart(t, word); !since.IsZero() {
		return word, minutes(now.Sub(since))
	}
	return word, ""
}

// stageStart is when t entered its work stage word: the admission, the
// gate's start for verifying, else the filing.
func stageStart(t factoryd.Task, word string) time.Time {
	since := t.CreatedAt
	if t.AdmittedAt != nil {
		since = *t.AdmittedAt
	}
	if word == "verifying" && t.GateSince != nil {
		since = *t.GateSince
	}
	return since
}

// frozenLimit is the least backoff a provider_refused step's freeze
// carries: the ladder answers within half an hour, a named limit's reset
// lies beyond it.
const frozenLimit = 30 * time.Minute

// frozenSpan is one provider-limit freeze the card's timeline records:
// from the refusal to the reset it named.
type frozenSpan struct{ start, end time.Time }

// frozenIn is how long the freezes lie inside from..to: a freeze that
// begins before it counts from from, one that ends after it to to.
func frozenIn(freezes []frozenSpan, from, to time.Time) time.Duration {
	var sum time.Duration
	for _, f := range freezes {
		start, end := f.start, f.end
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if end.After(start) {
			sum += end.Sub(start)
		}
	}
	return sum
}

// splitClocks splits the card's stage and total clocks at its freezes:
// the stage reads "building 3 h 12 min, frozen 6 h 48 min", so hours the
// provider held do not read as work; Total splits the same way.
func (k *card) splitClocks(freezes []frozenSpan) {
	if len(freezes) == 0 || k.endAt.IsZero() {
		return
	}
	if k.Stage != "" {
		if since := stageStart(k.Task, k.Stage); !since.IsZero() {
			if frozen := frozenIn(freezes, since, k.endAt); frozen > 0 {
				k.StageAge = minutes(k.endAt.Sub(since)-frozen) + ", frozen " + minutes(frozen)
			}
		}
	}
	if !k.CreatedAt.IsZero() {
		if frozen := frozenIn(freezes, k.CreatedAt, k.endAt); frozen > 0 {
			k.Total = minutes(k.endAt.Sub(k.CreatedAt)-frozen) + ", frozen " + minutes(frozen)
		}
	}
}

// freezeOf is what steps say of a provider-limit freeze: the span of
// every provider_refused step whose backoff outlasts the ladder's, oldest
// first, and the newest refusal's words for the chip — "frozen until
// 17:04 UTC: …" with the provider's words — "" when it is the ladder's
// short backoff, or its reset has passed.
func freezeOf(steps []map[string]any, now time.Time) ([]frozenSpan, string) {
	var spans []frozenSpan
	last := -1
	for i, m := range steps {
		if m["step"] != factoryd.StepProviderRefused {
			continue
		}
		last = i
		if backoff := time.Duration(numAt(m, "backoff_s")) * time.Second; backoff > frozenLimit {
			if at := stepTime(m); !at.IsZero() {
				spans = append(spans, frozenSpan{start: at, end: at.Add(backoff)})
			}
		}
	}
	if last < 0 {
		return spans, ""
	}
	m, backoff, at := steps[last], time.Duration(numAt(steps[last], "backoff_s"))*time.Second, stepTime(steps[last])
	if backoff <= frozenLimit || at.IsZero() || !at.Add(backoff).After(now) {
		return spans, ""
	}
	words := "frozen until " + at.Add(backoff).UTC().Format("15:04 UTC")
	if w := strings.TrimSpace(textAt(m, "words")); w != "" {
		words += ": " + textutil.Truncate(w, 120)
	}
	return spans, words
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
	name := "page.html"
	if r.URL.Path == "/board" {
		name = "board-fragment"
	}
	c.render(w, name, d)
}

// readBoard is the board under f for a GET, false after it answered the
// refusal.
func (c *Console) readBoard(w http.ResponseWriter, r *http.Request, f boardFilter) (boardData, bool) {
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
	col, ok := c.readFinishedColumn(w, r, f)
	if !ok {
		return
	}
	c.render(w, "finished-page", col)
}

// finishedColumn is the board's column of finished cards key, Done, which
// holds all three final states; false for any other key.
func finishedColumn(key string) (column, bool) {
	if key != "done" {
		return column{}, false
	}
	return column{Title: columnTitles[colDone], Finished: key}, true
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
	col, ok := finishedColumn(f.Col)
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
	for _, bt := range page {
		v.byID[bt.ID] = bt.Task
	}
	for _, bt := range page {
		// a finished card of the human lane holds no column (boardOf)
		if v.humanLane(bt.Task) {
			continue
		}
		d.Columns[0].Cards = append(d.Columns[0].Cards, c.boardCard(ctx, v, bt))
	}
	c.finishCards(ctx, v, func(fn func(*card)) {
		for i := range d.Columns[0].Cards {
			fn(&d.Columns[0].Cards[i])
		}
	})
	c.setFaces(ctx, v, &d)
	return d.Columns[0], true
}

// pressed answers a press with no content and the board-refresh trigger:
// the board's re-render is the answer, so the press never waits on the
// board's reads. A form post without htmx keeps the redirect to the
// whole page.
func (c *Console) pressed(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		c.fail(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Trigger", "board-refresh")
	w.WriteHeader(http.StatusNoContent)
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
	byID            map[string]factoryd.Task
	pending         []factoryd.ApprovalRequest
	carded, planned map[string]bool
	writes          bool // the viewer writes the shown project
	unchecked       bool // a writers check failed, logged once per render
	problems        []string
	// planTrack names the repositories whose project's process has a plan
	// track.
	planTrack map[string]bool
	// refs resolve the references of the render's texts by repository,
	// each read once (linkRefs)
	refs map[string]renderRefs
	// cases are the shown repository's cases, plans the plans the faces
	// read and rollups their cards' states, each once
	cases   caseSet
	plans   map[string]planRead
	rollups map[string]rollup
	// requests are the requests the opened card read for its brief, which
	// its case's page reads no second time
	requests map[int64]approvalCard
	// rooms are the room facts the opened card read, by card
	rooms map[string]factoryd.CaseFacts
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
	}
	return v, all, nil
}

// projectsOf reads the view's policy and the projects agents work on, by
// name: what a page of finished cards reads, whose cards never wait on an
// approval.
func (c *Console) projectsOf(r *http.Request, f boardFilter) (*boardView, []factoryd.Project, error) {
	ctx, s := r.Context(), current(r)
	v := &boardView{user: s.user, csrf: s.csrf, filter: f, byID: map[string]factoryd.Task{}, carded: map[string]bool{}, planned: map[string]bool{}}
	v.policy, v.policyOK = c.inForce(ctx)
	all, err := c.coderProjects(ctx)
	if err != nil {
		return v, nil, fmt.Errorf("projects: %w", err)
	}
	v.planTrack = map[string]bool{}
	for _, p := range all {
		_, v.planTrack[p.Repository] = p.Tracks.Track(policy.TrackPlan)
	}
	return v, all, nil
}

// humanLane: t has no agent, so its card is in the human lane: a human
// task.
func (v *boardView) humanLane(t factoryd.Task) bool {
	return t.Kind == "human"
}

// deployStage is the chain position a merged card shows while its change
// is not live yet: the roll it waits for, else where its deploy chain
// sits, naming the run that carries it when one does (roadmap 297, C38).
func deployStage(state string, rollIssue, run int64) string {
	if rollIssue > 0 {
		return fmt.Sprintf("blocked by roll #%d", rollIssue)
	}
	switch state {
	case "":
		return "waiting for the chain"
	case "failure", "cancelled":
		if run > 0 {
			return fmt.Sprintf("run %d red", run)
		}
		return "chain red"
	case "stalled":
		if run > 0 {
			return fmt.Sprintf("run %d stalled", run)
		}
		return "stalled"
	}
	if run > 0 {
		return fmt.Sprintf("chain run %d", run)
	}
	return "chain running"
}

// escalated: t waits on an escalation of its own; a waiting plan's card is
// its escalation, which Request changes answers.
func (v *boardView) escalated(t factoryd.Task) bool {
	return t.Status == "waiting-human" && !v.planned[t.ID]
}

// approves: the viewer approves repository's project.
func (v *boardView) approves(repository string) bool {
	return slices.Contains(v.policy.ApproversOf(repository), v.user)
}

// boardCard is bt's card as the board shows it, in a column or in Needs
// you, and as the card fragment swaps it in; its activity line, checks
// and forms follow from finishCards. Place is where the board puts it:
// the script swaps a card whose place changed with the whole board.
func (c *Console) boardCard(ctx context.Context, v *boardView, bt factoryd.BoardTask) card {
	t := bt.Task
	k := c.card(t)
	k.Place = fmt.Sprintf("%s %s %s %t %t %d %d", t.Status, t.Track, t.Accent, v.carded[t.ID], v.planned[t.ID], bt.AskCase,
		fiveColumn(t))
	if bt.Status == "merged" {
		k.Stage = deployStage(bt.ChainState, bt.RollIssue, bt.ChainRun)
		k.RollID = bt.RollID
		if bt.RollID == "" && bt.ChainRun > 0 {
			k.StageURL = c.runURL(bt.Repository, bt.ChainRun)
		}
	}
	// a done card the chain covers names the run that ran its change
	// live, and how long after its merge (C38)
	if bt.Status == "done" && bt.PRNumber > 0 && bt.FinishedAt != nil && bt.ChainRun > 0 &&
		bt.ChainGreen >= bt.ChainRun && !bt.ChainGreenAt.IsZero() {
		k.Cover = fmt.Sprintf("live through run %d, %s after merge", bt.ChainGreen,
			minutes(max(bt.ChainGreenAt.Sub(*bt.FinishedAt), 0)))
	}
	k.WaitingOn, k.SlotWait, k.MergeBehind, k.HeldFor = bt.WaitingOn, bt.SlotWait, bt.MergeBehind, bt.HeldFor
	k.HoldLine = bt.HoldView.Line
	// "blocked by n" counts the dependencies that are not done yet;
	// WaitingOn already says so for a card in waiting-deps, where the
	// face shows that chip and never both.
	var refs []string
	for _, d := range bt.Dependencies {
		if d.Issue > 0 {
			refs = append(refs, "#"+strconv.FormatInt(d.Issue, 10))
		}
	}
	if len(refs) > 0 && bt.WaitingOn == "" {
		k.Blocked, k.BlockedOn = len(refs), strings.Join(refs, ", ")
	}
	// a merged card's row may still name gate merge-queue
	k.QueueAhead, k.MergeBehind = bt.QueueAhead, bt.MergeBehind
	if n := len(bt.Helpers); n > 0 {
		h := bt.Helpers[n-1].Task
		k.Helper = &h
	}
	if v.humanLane(t) {
		k.Question = bt.Question
		if bt.Question != nil {
			k.Title = bt.Question.Ask
		}
		return k
	}
	if t.PlanCard() {
		var planRepo string
		k.PlanIssue, k.PlanName, planRepo = c.planIssue(ctx, v.byID, t.Parent)
		if k.PlanIssue > 0 {
			k.PlanURL = fmt.Sprintf("%s/%s/issues/%d", strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/"), planRepo, k.PlanIssue)
		}
	}
	// a retry that stopped before its replacement got an issue is finished by the next
	// a card cut into a successor is that card's to answer: RetryCard refuses it
	replacement, retried := v.byID[factoryd.RetryID(t.ID)]
	_, cut := v.byID[factoryd.SuccessorID(t.ID)]
	if t.Status == "failed" && (t.PlanCard() && t.PlanKey != "" || retried && replacement.PlanCard() && replacement.IssueNumber == 0) &&
		!cut && v.approves(t.Repository) {
		k.MayRetry = true
	}
	k.MayReplan = factoryd.MayReplan(t, v.planTrack[t.Repository]) && v.approves(t.Repository)
	k.MayExtend = v.approves(t.Repository) && extendable(t)
	if v.escalated(t) {
		k.Reason, k.Question = cardReason(bt.Escalation), bt.Question
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
	if e.Kind == factoryd.EventIssueClosedWithoutWriteAccess {
		return fmt.Sprintf("Ignored the close by %s: %s cannot write the repository.", e.Actor, e.Actor)
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
	each(func(k *card) {
		ids = append(ids, k.ID)
		if k.Helper != nil {
			ids = append(ids, k.Helper.ID)
		}
	})
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
	// a queued one or one that waits for a human's answer; a card parked
	// for a slot owes no work while it waits. A helper task at work on the
	// card is the card's activity.
	setActivity := func(k *card) {
		lastAt := func(at time.Time) time.Time {
			last := []time.Time{at, k.stepAt}
			if h := k.Helper; h != nil {
				last = append(last, h.CreatedAt, newest[h.ID].At)
			}
			return slices.MaxFunc(last, time.Time.Compare)
		}
		ring := func(d time.Duration) bool { return d >= factoryd.StaleWindow }
		live := !factoryd.Final(k.Status) && k.Status != "queued" && k.Status != "waiting-human" && k.Status != "waiting-poster" && k.MergeBehind == 0 && k.HeldFor == 0
		reason := func() string {
			q, _, _ := strings.Cut(strings.TrimPrefix(k.Reason, factoryd.AgentAsks), "\n")
			return q
		}
		// a human card's title is its ask
		titled := k.Kind == "human" && k.Question != nil
		if e, ok := newest[k.ID]; ok {
			k.Activity = activityLine(e)
			if why, _ := e.Detail["reason"].(string); e.Kind == factoryd.EventTaskEnded && k.Status == "failed" {
				k.Failure = failureClause(why)
				if s, ok := v.byID[factoryd.SuccessorID(k.ID)]; ok && s.IssueNumber > 0 {
					k.Failure = fmt.Sprintf("closed into successor #%d", s.IssueNumber)
				}
			}
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
			switch {
			case titled:
			case k.Question != nil && strings.HasPrefix(k.Status, "waiting"):
				k.Activity, k.WaitingForActivity = askline(k.Question.Ask), true
			case k.Reason != "" && strings.HasPrefix(k.Status, "waiting"):
				k.Activity = reason()
				k.WaitingForActivity = true
			}
			return
		}
		k.Stale = live && ring(time.Since(lastAt(k.CreatedAt)))
		if k.Stale {
			k.StaleText = "no event yet, " + minutes(time.Since(lastAt(k.CreatedAt))) + " on the card"
		}
		switch {
		case titled:
		case k.Question != nil:
			k.Activity = askline(k.Question.Ask)
		case k.Reason != "":
			k.Activity = reason()
		}
	}
	c.faceTimelines(ctx, each)
	each(setActivity)
	c.callTotals(ctx, each)
	each(func(k *card) { k.Fine = fineStates(*k) })
	setForms := func(k *card) {
		ok, err := c.writesRepo(ctx, v.user, k.Repository)
		if err != nil && !v.unchecked {
			v.unchecked = true
			log.Printf("console: may %s write %s: %v", v.user, k.Repository, err)
		}
		if k.MayWriteSummary = ok; ok {
			k.CSRF = v.csrf
		}
		k.Filter = v.filter
	}
	each(setForms)
}

// failureClause is a failure's reason as one clause after "failed: ":
// its first sentence, "" when that is no short clause or names a card by
// its task id.
func failureClause(reason string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	first, _, _ = strings.Cut(first, ". ")
	first = strings.TrimRight(first, ".")
	if first == "" || utf8.RuneCountInString(first) > 80 || strings.ContainsAny(first, ":;") || taskIDWord.MatchString(first) {
		return ""
	}
	r, size := utf8.DecodeRuneInString(first)
	if rest := first[size:]; rest == "" || !unicode.IsUpper(rune(rest[0])) {
		first = string(unicode.ToLower(r)) + rest
	}
	return first
}

var taskIDWord = regexp.MustCompile(`\bt-[0-9a-f]{6,}`)

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
	return commas.Sprintf("%d prompt · %d cached · %d output tokens · %s EUR modeled",
		t.Prompt, t.Cached, t.Output, textutil.EUR(t.ModeledEUR))
}

// shownProject is the board's shown project under f among all, the
// projects agents work on — f.Project's, else the first the viewer
// writes — with projects, the top bar's select choices, every project the
// viewer writes and the shown one, and writable saying each project by
// name. A project whose writers could not be checked is offered, its
// buttons hidden, rather than an empty board; a member sees only a
// project they write or read, the factory's writers every
// board, read-only where they do not write; errNoProject is the refusal.
// The whole board and a page of finished cards show the same one.
func (c *Console) shownProject(ctx context.Context, r *http.Request, v *boardView, all []factoryd.Project, f boardFilter) (shown factoryd.Project, projects []string, writable map[string]bool, err error) {
	writable = map[string]bool{}
	for _, p := range all {
		writes, werr := c.writesRepo(ctx, v.user, p.Repository)
		if werr != nil && !v.unchecked {
			v.unchecked = true
			log.Printf("console: may %s write %s: %v", v.user, p.Repository, werr)
			v.problems = append(v.problems, "the project's writers could not be checked: its buttons are hidden")
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
	// a member sees only the projects they write or read; the
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
// human sees it in the four columns. act is the actor whose board read this is — the
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
	v.writes = d.Writes
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
	d.Chat = c.cfg.Chat != nil && c.cfg.Operator != nil && shown.Name != ""
	if d.Chat {
		if row, ok, err := c.cfg.Chat.ChatIntake(ctx, shown.Repository); err != nil {
			log.Printf("console: the intake of %s: %v", shown.Name, err)
		} else if ok {
			d.IntakeClosed, d.IntakeBy, d.IntakeAt = row.Closed, row.By, row.At
		}
	}
	v.filter, d.boardFilter, d.Query = f, f, f.query()
	clear := f
	clear.Q = ""
	d.ClearQuery = clear.query()
	for i, title := range columnTitles {
		d.Columns = append(d.Columns, column{Title: title, Holds: strings.Join(columnHolds[i], " ")})
	}
	d.Columns[colDone].Finished = "done"

	tasks, err := c.cfg.Tasks.BoardTasks(ctx, shown.Repository)
	if err != nil {
		return d, fmt.Errorf("tasks: %w", err)
	}
	for _, bt := range tasks {
		v.byID[bt.ID] = bt.Task
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
	var needs []need
	cs, err := c.caseSetOf(ctx, shown.Repository, tasks...)
	if err != nil {
		log.Printf("console: the cases of %s: %v", shown.Name, err)
		v.problems = append(v.problems, "the cases waiting on you could not be read")
	}
	for _, bt := range c.askingCards(ctx, cs, tasks) {
		v.byID[bt.ID] = bt.Task
		tasks = append(tasks, bt)
	}
	v.cases = cs
	inView := casesInView(r)
	// BoardTasks and readFinished read shown.Repository's cards only, so
	// every byID lookup below stays right: a plan parent not among them
	// (finished, another project's) is read by planIssue's own read, a
	// failed card's replacement keeps the failed card's repository, and a
	// pending approval's task lives in its project, which here is shown's
	for _, bt := range tasks {
		t := bt.Task
		// factoryd's own human card works with the factory agent until its
		// face asks a human
		human, col := v.humanLane(t) && t.Status != "waiting-agent", fiveColumn(t)
		// a done plan that waits on its failed card holds its case at the
		// human: its row of Needs you carries the case
		holds := cs.holds(t)
		k := c.boardCard(ctx, v, bt)
		// the status line counts the project's cards, whatever the accent;
		// a card that holds a case at the human counts once, as its row,
		// and a plan that waits for its answers counts as its approval
		switch {
		case holds:
			d.Status.NeedYou++
		case human:
			if !factoryd.Final(t.Status) {
				d.Status.NeedYou++
			}
		case c.working(ctx, v, k):
			d.Status.Working++
		case col == colProgress && t.Status == "running" && c.timelineFace(ctx, t.ID).provider:
			d.Status.Waiting++
		case col == colProgress && t.Status != "in-review":
			d.Status.Working++
		case col == colNeedsYou && !v.planned[t.ID]:
			d.Status.NeedYou++
		}
		// a card that holds a case at the human is one row of Needs you, its
		// case in view, and yields its column to it as a merge card does
		var row need
		if holds {
			row = c.caseNeed(ctx, v, act, bt, k, cs, inView[t.ID])
		}
		switch {
		case human:
			if factoryd.Final(t.Status) {
				break
			}
			if holds {
				needs = append(needs, row)
				break
			}
			needs = append(needs, need{Since: t.CreatedAt, Card: k})
		case holds:
			needs = append(needs, row)
		case col == colNeedsYou && v.planned[t.ID] && !planFaced(v, cs, t.ID):
			// a plan whose questions climb their rungs works, as a merge
			// card in review does while its case climbs
			d.Columns[colProgress].Cards = append(d.Columns[colProgress].Cards, k)
		case col == colNeedsYou:
			// a plan that waits for its answers needs its approval card
			if v.planned[t.ID] {
				break
			}
			needs = append(needs, need{Since: waitingSince(bt), Card: k})
		case col >= 0:
			d.Columns[col].Cards = append(d.Columns[col].Cards, k)
		}
	}
	for _, a := range v.pending {
		if a.Project != shown.Repository {
			continue
		}
		// the card's row holds its requests
		if cs.holds(v.byID[a.TaskID]) {
			continue
		}
		// a merge's, a plan's or a plan's criteria request shows through its
		// case alone, whose pages the card's row carries
		if waitsForFace(a) {
			continue
		}
		k := c.approvalCard(ctx, a, act, v.byID, v.csrf, d.Approver, true)
		k.Filter, k.Writer = f, d.Writes
		d.Status.NeedYou++
		needs = append(needs, need{Since: a.CreatedAt, Approval: &k})
	}
	slices.SortStableFunc(needs, func(a, b need) int { return a.Since.Compare(b.Since) })
	d.Columns[colNeedsYou].Needs = needs
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
	})
	c.setFaces(ctx, v, &d)
	d.whole = d.Columns
	d.Columns = find(d.Columns, f.Q)
	arrange(&d, f)
	if shown.Name != "" {
		c.statusLine(ctx, v, f, shown, &d)
		if c.cfg.Routines != nil {
			c.placeGhosts(ctx, v, &d, shown.Repository)
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
	q := factoryd.FinishedQuery{Repository: repository, Statuses: factoryd.FinalStatuses, Since: time.Now().Add(-finishedSince), Oldest: f.Done.Order == "oldest"}
	if f.whole() {
		all, _, err := c.cfg.Tasks.FinishedTasks(ctx, q)
		return all, err
	}
	var out []factoryd.BoardTask
	for i := range d.Columns {
		col := &d.Columns[i]
		if col.Finished == "" || f.Col != "" && col.Finished != f.Col {
			continue
		}
		q.After, q.Limit = f.After, max(finishedPerPage, f.Shown[col.Finished])
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

// backstopWait bounds the board's read of LiteLLM's backstop: a slow
// LiteLLM leaves the panel its problem line, not the board waiting.
const backstopWait = 2 * time.Second

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
	now := c.clock()
	e, err := c.cfg.Costs.Enforced(ctx, now.UTC())
	if err != nil {
		log.Printf("console: the enforced spend: %v", err)
		d.Problems = append(d.Problems, "the spend against the caps could not be read")
	}
	if s.Spend, s.CapEUR, s.FactoryCap, err = c.monthFigures(ctx, v.policy, p, e, now); err != nil {
		log.Printf("console: the spend of %s this month: %v", p.Repository, err)
		d.Problems = append(d.Problems, "the project's spend this month could not be read")
	}
	if v.policyOK {
		s.DayCapEUR = v.policy.Document.Budget.DayEUR
		s.PremiumCapEUR = v.policy.Document.Budget.PremiumMonthEUR
		if s.Pool = c.ProjectPool(ctx, v.policy, p, now); !s.Pool.Readable {
			d.Problems = append(d.Problems, "the month's pool could not be read")
		}
	}
	s.TodayEUR, s.DayHit = e.Day, policy.CapHit(e.Day, s.DayCapEUR, e.DayHit)
	s.DayReset, s.MonthReset = resetLineOf(e.DayReset, e.DayAll, "today", now), resetLineOf(s.Spend.Reset, s.Spend.AllEUR, "this month", now)
	if s.PremiumCapEUR > 0 {
		s.PremiumEUR, s.PremiumHit = e.Premium, policy.CapHit(e.Premium, s.PremiumCapEUR, e.PremiumHit)
	}
	s.Approver = v.approves(p.Repository)
	if c.cfg.Backstop != nil {
		bctx, cancel := context.WithTimeout(ctx, backstopWait)
		b, err := c.cfg.Backstop(bctx)
		cancel()
		if err != nil {
			log.Printf("console: LiteLLM's backstop: %v", err)
			d.Problems = append(d.Problems, "LiteLLM's backstop could not be read")
		} else {
			s.Backstop, s.BackstopHit = &b, b.SpendUSD >= b.MaxUSD
		}
	}
	if c.cfg.Caps != nil {
		press := func(action, label, cap string, capEUR float64, hit bool, shown *policy.CapReset) *capPress {
			cp := &capPress{Action: action, Label: label, Words: resetWords(cap, capEUR, now, s.Backstop), CapEUR: capEUR, Hit: hit, CSRF: d.CSRF, Filter: f}
			if shown != nil {
				cp.Seen = shown.ID
			}
			return cp
		}
		if d.Factory && s.DayCapEUR > 0 {
			s.DayPress = press("/caps/day/reset", "Reset today's count", policy.CapDay, s.DayCapEUR, s.DayHit, e.DayReset)
		}
		switch {
		case s.CapEUR <= 0:
		case s.FactoryCap && d.Factory:
			s.MonthPress = press("/caps/month/reset", "Reset this month's count", policy.CapMonth, s.CapEUR, s.Spend.Hit, s.Spend.Reset)
		case !s.FactoryCap && s.Approver:
			s.MonthPress = press("/projects/"+p.Name+"/cap/reset", "Reset the project's month count", policy.CapProject, s.CapEUR, s.Spend.Hit, s.Spend.Reset)
		}
	}
	if stop, err := c.chatStopNow(ctx, p); err != nil {
		log.Printf("console: the chat stop of %s: %v", p.Repository, err)
		d.Problems = append(d.Problems, "the judgment budget could not be read")
	} else if stop.stopped {
		s.ChatSpent, s.ChatTask = true, stop.task.ID
	}
	if c.cfg.Hold != nil {
		if h, err := c.cfg.Hold(ctx, p.Repository); err != nil {
			log.Printf("console: the production hold of %s: %v", p.Repository, err)
			d.Problems = append(d.Problems, "the production hold could not be read")
		} else if h.Held() {
			s.Hold = &holdLine{For: coarse(time.Since(h.Since)), Reason: factoryd.HoldReason(h.Groups)}
			for _, g := range h.Groups {
				merged, total := g.Merged()
				s.Hold.Groups = append(s.Hold.Groups, fmt.Sprintf("%s · %d of %d merged", factoryd.PlanWords(g.Name, g.Issue), merged, total))
			}
			if h.DeployNow != "" && c.cfg.MainCommit != nil {
				switch head, err := c.cfg.MainCommit(ctx, p.Repository); {
				case err != nil:
					log.Printf("console: the head of %s: %v", p.Repository, err)
					d.Problems = append(d.Problems, "main's head could not be read")
				case head == h.DeployNow:
					s.Hold.Released = &releaseStanding{Commit: factoryd.ShortSHA(h.DeployNow), Age: coarse(time.Since(h.DeployAt))}
				}
			}
		}
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
// the foot of Queued (C16, as the 2026-10-17 ruling
// has it: needs-you holds only what needs a human's action now, so every
// routine's preview queues like all scheduled work, and the routine's
// work reaches needs-you only as its filed card, through the card flow),
// in the five columns and in the wide lane's queue group alike:
// the dated ones by their next run, then the merge-counted ones by the
// merges they lack, then the rest. The board's routines choice hides
// those whose next run is further away than its period, or all, and each
// lane counts what it hides. The strip's cell names the first held run,
// else the soonest under an hour away, whatever the choice.
func (c *Console) placeGhosts(ctx context.Context, v *boardView, d *boardData, repository string) {
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
	if len(views) == 0 {
		return
	}
	d.HasRoutines = true
	all := d.boardFilter
	all.Routines = "all"
	d.AllRoutines = all.query()
	for _, rv := range views {
		if d.Routines == "none" || d.Routines == "within" && rv.Next.Sub(now) > d.period() {
			d.Columns[colQueued].GhostsHidden++
			continue
		}
		d.Columns[colQueued].Ghosts = append(d.Columns[colQueued].Ghosts, c.routineFace(ctx, v, repository, rv, now, d.Writes))
	}
}

// filesWords are the kinds of work a routine files, as its ghost says them.
var filesWords = map[string]string{policy.FilesHuman: "human card", policy.FilesAgent: "agent card",
	policy.FilesPullRequest: "pull request", policy.FilesReport: "report"}

// routineFace is routine rv's face at now (C16, C46), which a writer may
// open: one paragraph of its title and the why line its file holds, and a
// foot of its schedule, its next run and its last. The next names the
// run's time, how soon it comes and what it files; a merge-counted
// routine's the merges it lacks, an event's only what it files, as the
// schedule names the event. A pending skip leads it, the run it
// names being the one after the skipped. The last names a held run's
// words, the open card running, or the newest run by the card it filed.
// It holds no button: its answers are its opened card's.
func (c *Console) routineFace(ctx context.Context, v *boardView, repository string, rv factoryd.RoutineView, now time.Time, open bool) faceView {
	rt := rv.Routine
	foot := &routineFoot{Key: rv.Project + "/" + rt.Name, Title: oneLine(rt.Title), Schedule: rt.Words(), Soon: rv.Soon, Held: rv.Held}
	f := faceView{Open: open, Routine: foot}
	f.title(c.linkRefs(ctx, v, repository, rt.Title), rt.Title)
	if rt.Why != "" {
		f.Why = c.linkRefs(ctx, v, repository, rt.Why)
	}
	when, in, kind := nextWords(rv, now)
	switch {
	case in != "":
		foot.Next = when + " · in " + in + " · " + kind
	case when != "":
		foot.Next = when
	default:
		foot.Next = kind
	}
	switch {
	case rv.State.SkipNext && when == "":
		foot.Next = "skipped once · " + kind
	case rv.State.SkipNext:
		foot.Next = "skipped once · next " + foot.Next
	}
	var last string
	switch l := runWords(rv.Last); {
	case rv.Held:
		last = rv.Last.Note
	case rv.Open != nil && rv.Open.Status == "running":
		foot.Running, last = true, fmt.Sprintf("#%d running", rv.Open.IssueNumber)
	case rv.Last.Issue > 0:
		last = fmt.Sprintf("last run #%d", rv.Last.Issue)
	case l != "":
		last = "last " + l
	}
	if last != "" {
		foot.Last = c.linkRefs(ctx, v, repository, last)
	}
	return f
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

// monthFigures is the status's month spend and cap at now: the project
// file's budget.month_eur against the project's own spend, else the
// policy's month_eur against the enforced month spend e counts — the
// factory's whole ledger with its holds — so the meter reads what the cap
// stops. The third return says the cap is the factory's, not a project
// file's.
func (c *Console) monthFigures(ctx context.Context, v policy.Version, p factoryd.Project, e policy.Enforced, now time.Time) (factoryd.ProjectSpend, float64, bool, error) {
	if _, eur := v.ProjectCap(p.Repository); !math.IsInf(eur, 1) {
		spend, err := c.cfg.Chat.ProjectMonthSpend(ctx, p.Repository, now)
		spend.Hit = policy.CapHit(spend.EUR, eur, spend.Hit)
		return spend, eur, false, err
	}
	return factoryd.ProjectSpend{EUR: e.Month, AllEUR: e.MonthAll, Hit: policy.CapHit(e.Month, v.Document.Budget.MonthEUR, e.MonthHit), Reset: e.MonthReset},
		v.Document.Budget.MonthEUR, true, nil
}

// spendWords is spend against the month cap as the project agent reads
// it at now: under the factory's cap the whole enforced ledger, without
// the project's own split, which does not sum to it. Once a reset
// restarted the cap's count, the spend this month is the month's whole and
// the count since the reset (CapResetWords) stands apart.
func spendWords(spend factoryd.ProjectSpend, capEUR float64, factory bool, now time.Time) string {
	cards := fmt.Sprintf("the cards %s EUR, the chat %s EUR", textutil.EUR(spend.EUR-spend.ChatEUR), textutil.EUR(spend.ChatEUR))
	if spend.Reset == nil {
		head := fmt.Sprintf("spend this month %s of %s EUR", textutil.EUR(spend.EUR), textutil.EUR(capEUR))
		if spend.Hit {
			head += " (cap hit)"
		}
		if factory {
			return head + ", the factory's whole ledger"
		}
		return head + ": " + cards
	}
	whole, count := "", textutil.EUR(spend.EUR)+" EUR"
	if factory {
		whole = ", the factory's whole ledger"
	} else {
		count += ", " + cards
	}
	if spend.Hit {
		count += ", cap hit"
	}
	return fmt.Sprintf("spend this month %s EUR%s; counted against the %s EUR cap since the %s: %s", textutil.EUR(spend.AllEUR), whole,
		textutil.EUR(capEUR), factoryd.CapResetWords(*spend.Reset, now), count)
}

// resetWords is the confirm of a Reset of cap at now, capEUR the cap: what
// its count becomes, the most that may still run before its period turns
// and what stays, and LiteLLM's backstop, b, when it leaves less.
func resetWords(cap string, capEUR float64, now time.Time, b *factoryd.Backstop) string {
	eur := textutil.EUR(capEUR)
	decision, count, before, stays := policy.RefusedMonthCap, "This month's count", "", "The month cap stays "+eur+" EUR; the day cap still holds."
	switch cap {
	case policy.CapDay:
		decision, count, before, stays = policy.RefusedDayCap, "Today's count", "00:00 UTC", "The day cap stays "+eur+" EUR; the month cap still holds."
	case policy.CapProject:
		count, stays = "The project's month count", "The project's month cap stays "+eur+" EUR; the factory's caps still hold."
	}
	turn := factoryd.CapTurn(decision, now)
	if before == "" {
		before = turn.Format("2 January") + " 00:00 UTC"
	}
	w := fmt.Sprintf("%s starts again from 0.00 EUR. Up to %s EUR more may be spent before %s, in %s. %s", count, eur, before, minutes(turn.Sub(now)), stays)
	if b != nil && b.MaxUSD-b.SpendUSD < capEUR {
		w += fmt.Sprintf(" LiteLLM's backstop leaves %s USD this month.", textutil.EUR(max(b.MaxUSD-b.SpendUSD, 0)))
	}
	return w
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
	case "supervisor", "checkin":
		// a check-in joins the column only when the runner wrote it beside
		// a detector or guidance; a review check-in keeps its plain row
		_, detected := m["detector"]
		_, guided := m["guidance"]
		if e.Step == "checkin" && !detected && !guided {
			break
		}
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
	bt, ok, err := c.cfg.Tasks.BoardTask(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	t := bt.Task
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
	force, _ := c.inForce(r.Context())
	chain, err := c.chainOf(r.Context(), bt, force, time.Now())
	if err != nil {
		log.Printf("console: the chain of %s: %v", t.ID, err)
	}
	v := &boardView{user: current(r).user, csrf: current(r).csrf, byID: map[string]factoryd.Task{t.ID: t}}
	c.render(w, "timeline.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf, "Factory": c.factoryWriter(r),
		"Card": c.card(t), "Face": c.faceOf(r.Context(), v, need{Card: c.card(t)}, false), "Entries": entries, "Gates": gates, "Longest": longest, "Chain": chain})
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
	Project, Title, Why, Summary, Body, Track, Priority, Size string
	// Refused is the filing door's refusal of the brief, "" for none
	Refused string
}

func (c *Console) newTaskForm(w http.ResponseWriter, r *http.Request) {
	c.renderNewTask(w, r, newTask{Track: policy.TrackDirect}, nil)
}

// renderNewTask answers the New task form: the projects agents work on,
// those whose process has a plan track and the priority labels the first's repository offers. A
// first submit whose text is flagged, or whose brief the filing door
// refuses, answers the form again, the form's values kept and the
// findings or the refusal shown above it.
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
	var plan []string
	for _, p := range ps {
		if p.Tracks.Plan != nil {
			plan = append(plan, p.Name)
		}
	}
	c.render(w, "new.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf, "Factory": true,
		"Projects": ps, "Priorities": offered, "PlanProjects": strings.Join(plan, " "), "Form": form, "Flags": flags})
}

// createTask files the issue as the human (doFileCard). Text the
// sanitiser or the provider would change (roadmap 125), or a brief the
// filing door refuses, is not filed at once: the form answers the findings
// or the refusal with everything the human wrote kept, and only its File
// as written button files the text as it stands, because a human may mean
// it; the door then files it marked unchecked.
func (c *Console) createTask(w http.ResponseWriter, r *http.Request) {
	form := newTask{Project: r.PostFormValue("project"), Title: r.PostFormValue("title"), Why: r.PostFormValue("why"), Summary: r.PostFormValue("summary"),
		Body: r.PostFormValue("body"), Track: r.PostFormValue("track"), Priority: r.PostFormValue("priority"), Size: r.PostFormValue("size")}
	try := factoryd.BriefTries
	if r.PostFormValue("as_written") == "" {
		if flags := filingFlags(form.Title, form.Body); len(flags) > 0 {
			c.renderNewTask(w, r, form, flags)
			return
		}
		try = 1
	}
	_, _, err := c.doFileCard(r.Context(), button(r), form.Project, factoryd.BriefText{Title: form.Title, Why: form.Why, Summary: form.Summary, Body: form.Body}, try,
		form.Track, "", form.Priority, form.Size, "", "", "", 0, 0, 0)
	var no *briefRefusal
	if errors.As(err, &no) {
		form.Refused = no.msg
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		c.renderNewTask(w, r, form, nil)
		return
	}
	if err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/?"+url.Values{"project": {form.Project}}.Encode(), http.StatusSeeOther)
}

// answer posts the human's answer to an escalation (doAnswerValue): a
// human card's choice, or the human's words, which went to the chat
// (answerInChatTurn), where the human lands.
func (c *Console) answer(w http.ResponseWriter, r *http.Request) {
	if err := c.answersLiveAsk(r.Context(), r.PathValue("id"), r.PostFormValue("case")); err != nil {
		c.fail(w, r, err)
		return
	}
	note, err := c.doAnswerValue(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("answer"), r.PostFormValue("value"), r.PostFormValue("head"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	if note == answerInChatNote {
		chat := "/projects/" + url.PathEscape(r.PostFormValue("project")) + "/chat"
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", chat)
			return
		}
		http.Redirect(w, r, chat, http.StatusSeeOther)
		return
	}
	c.pressed(w, r, nil)
}

// resolve answers POST /tasks/{id}/resolve: the one send of an answer of
// the opened card. The form posts the answer its button names, with that
// answer's fields ("<answer>.<field>"), the note among them, and the case
// it answers; doPressCase makes the one resolution and Act runs it once.
// A form that names no case presses one of the card's own actions
// (doCardAnswer).
func (c *Console) resolve(w http.ResponseWriter, r *http.Request) {
	id, action, named := r.PathValue("id"), r.PostFormValue("action"), r.PostFormValue("case_id")
	field := func(name string) string { return strings.Join(r.PostForm[action+"."+name], "\n") }
	if named == "" {
		if a, _, _ := strings.Cut(action, "/"); a == factoryd.AnswerHold {
			// the press waits for the live session to take its stop; the
			// server's WriteTimeout would cut the answer after a minute
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(factoryd.HoldAskWait + time.Minute)); err != nil {
				log.Printf("console: a hold press keeps the server's write deadline: %v", err)
			}
		}
		_, err := c.doCardAnswer(r.Context(), button(r), id, action, field)
		roomPressed(w, action, err)
		c.pressed(w, r, err)
		return
	}
	caseID, err := strconv.ParseInt(named, 10, 64)
	if err != nil || caseID <= 0 {
		c.fail(w, r, refuse(http.StatusBadRequest, "The press names no ask of this card. Reload the board and press the answer again."))
		return
	}
	if err := c.answersLiveAsk(r.Context(), id, named); err != nil {
		c.fail(w, r, err)
		return
	}
	answer, ok := factoryd.AnswerOf(action)
	if !ok {
		c.fail(w, r, refuse(http.StatusBadRequest, "That is no answer of this card. Press one of the card's answers."))
		return
	}
	values := map[string]string{}
	for _, f := range answer.Fields {
		values[f.Name] = field(f.Name)
	}
	if answer.Action == factoryd.ActExtend {
		for _, k := range append([]string{factoryd.MoneyTouched}, factoryd.RoomGuard...) {
			values[k] = field(k)
		}
	}
	_, err = c.doPressCase(r.Context(), button(r), id, caseID, answer, values)
	roomPressed(w, action, err)
	c.pressed(w, r, err)
}

// roomPressed has a refused press of a room ask its readout to read again
// (room-refused), so the figures it shows and its guard follow the card as
// it is now.
func roomPressed(w http.ResponseWriter, action string, err error) {
	if err != nil && action == factoryd.ActExtend {
		w.Header().Set("HX-Trigger", "room-refused")
	}
}

// answersLiveAsk refuses an answer from a form that names a case other than
// the card's live ask, the case an answer on the card ends; a form that
// names none is the chat's or a script's, and goes through.
func (c *Console) answersLiveAsk(ctx context.Context, id, named string) error {
	if named == "" {
		return nil
	}
	shown, err := strconv.ParseInt(named, 10, 64)
	if err != nil {
		return refuse(http.StatusBadRequest, "The press names no ask of this card. Reload the board and press the answer again.")
	}
	live, err := c.cfg.NewestAskCase(ctx, id)
	if err != nil {
		log.Printf("console: the cases of %s: %v", id, err)
		return refuse(http.StatusServiceUnavailable, "The card's asks could not be read, so nothing was answered. Try again.")
	}
	if live.ID != shown {
		// a merge's, a plan's criteria and a monitor's flag are answered by
		// their own number, beside the card's live ask
		if k, err := c.cfg.CaseByID(ctx, shown); err == nil && k.TaskID == id && !factoryd.AnsweredOnCard(k.Kind) {
			return nil
		}
	}
	switch {
	case live.ID == 0:
		return refuse(http.StatusConflict, "This ask is no longer open, and the card asks nothing now, so nothing was answered. Reload the board.")
	case live.ID != shown:
		return refuse(http.StatusConflict, "This ask is no longer open, and the card asks something else now, so nothing was answered. Reload the board to see it.")
	}
	return nil
}

// chatFund raises the project's chat task's monthly budget (doChatFund),
// the approver's answer to the chat's budget stop.
func (c *Console) chatFund(w http.ResponseWriter, r *http.Request) {
	eur, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("eur")), 64)
	if err != nil {
		c.fail(w, r, refuse(http.StatusBadRequest, "an amount in EUR above zero is required"))
		return
	}
	if _, _, err := c.doChatFund(r.Context(), button(r), r.PathValue("name"), eur, r.PostFormValue("reason"), ""); err != nil {
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
	http.Error(w, "Forgejo refused to "+what+": "+forgejoCause(err)+". Try again in a moment.", http.StatusBadGateway)
}

// writableCard is the card the path names, built as the board shows it
// under the URL's filter (?project, ?q, the menus); an unknown card is
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
		for _, id := range []string{factoryd.RetryID(bt.ID), factoryd.SuccessorID(bt.ID)} {
			if rt, ok, err := c.cfg.Tasks.Task(ctx, id); err == nil && ok {
				v.byID[rt.ID] = rt
			}
		}
	}
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	return bt, k, v, true
}

// cardFragment answers GET /cards/{id} to a writer of the card's project,
// 404 to anyone else: the one card as the board renders it, its forms
// carrying the filter (?project, ?q, the menus) of the board it is
// swapped into.
func (c *Console) cardFragment(w http.ResponseWriter, r *http.Request) {
	bt, k, v, ok := c.writableCard(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// the five columns stand a human's open card in Needs you
	needs := fiveColumn(bt.Task) == colNeedsYou || bt.Kind == "human" && !factoryd.Final(bt.Status)
	n := need{Since: waitingSince(bt), Card: k}
	if bt.Kind == "human" {
		n.Since = bt.CreatedAt
	}
	if cs, err := c.caseSetOf(ctx, bt.Repository, bt); err != nil {
		log.Printf("console: the cases of %s: %v", bt.ID, err)
	} else {
		v.cases = cs
		if cs.holds(bt.Task) {
			all, _ := c.coderProjects(ctx)
			if i := slices.IndexFunc(all, func(p factoryd.Project) bool { return p.Repository == bt.Repository }); i >= 0 {
				v.filter.Project = all[i].Name
			}
			v.writes, _ = c.writesRepo(ctx, v.user, bt.Repository)
			n, needs = c.caseNeed(ctx, v, button(r), bt, k, cs, casesInView(r)[bt.ID]), true
		}
	}
	c.render(w, "face", c.faceOf(ctx, v, n, needs))
}

// drawerTab is a tab of the card drawer: its ?tab= and its title.
type drawerTab struct{ Name, Title string }

// drawerTabs are the card drawer's tabs: Session the worker's live
// session, the default of a running card, Overview of any other, Receipt
// the stored receipt of a merged task.
var drawerTabs = []drawerTab{{"overview", "Overview"}, {"chat", "Session"}, {"activity", "Activity"}, {"files", "Files"}, {"receipt", "Receipt"}}

// drawerData is the card drawer on one tab. Overview: the opened card.
// Activity: the timeline page's gates and steps. Files: the pull
// request's changed files. Chat: the newest session's messages.
type drawerData struct {
	Card      card
	Tab       string
	Tabs      []drawerTab
	Opened    *openedView
	Gates     []gateRow
	Longest   int
	Entries   []timelineEntry
	Files     []prFile
	MoreFiles bool // Files are the first filesPages pages
	Session   *sessionRef
	Said      []said
	Problem   string // what the tab could not read
	// Chain is the chain behind the card's change as the Chain section
	// reads it (C38), nil while the chain holds nothing to say.
	Chain *chainReadout
	// Receipt is the merged task's stored receipt (roadmap 66) as the
	// drawer's receipt tab shows it: its facts, and the model's
	// assessments in their own labelled section after them; nil for a
	// task with no stored receipt, the tab's empty state.
	Receipt *receiptView
}

// The receipt tab's view of a stored receipt (factoryd.MergeReceipt):
// every fact of it carried as the drawer shows it, and the model's
// assessments, each under its source's label, apart from the facts.
type receiptView struct {
	TaskID, Repository            string
	Issue, PR                     int64
	Base, Head, MergeCommit       string
	DiffSHA256                    string
	PolicyVersion                 int64
	CostEUR                       float64
	Gates, Decisions, Assessments []receiptRow
}

// receiptRow is one row of the receipt tab: its label — a gate's name, a
// decision's kind, an assessment's source with its criterion — and the
// words the drawer shows for it.
type receiptRow struct {
	Label, By, Command, Exit, Words, Note, Draw, At string
}

// receiptViewOf is the drawer's view of the task's stored receipt: its
// facts one each, every gate run its command, exit code and verdict, the
// cost, and each assessment under its source's label.
func receiptViewOf(r factoryd.MergeReceipt) *receiptView {
	v := &receiptView{TaskID: r.Facts.TaskID, Repository: r.Facts.Repository, Issue: r.Facts.Issue, PR: r.Facts.PR,
		Base: r.Facts.Base, Head: r.Facts.Head, MergeCommit: r.Facts.MergeCommit, DiffSHA256: r.Facts.DiffSHA256,
		PolicyVersion: r.Facts.PolicyVersion, CostEUR: r.Facts.CostEUR}
	for _, g := range r.Facts.Gates {
		exit, command := "—", "—"
		if g.Exit != nil {
			exit = strconv.Itoa(*g.Exit)
		}
		if g.Command != "" {
			command = g.Command
		}
		v.Gates = append(v.Gates, receiptRow{Label: g.Gate, Command: command, Exit: exit, Words: g.Verdict})
	}
	for _, d := range r.Facts.Decisions {
		at := ""
		if d.At != nil {
			at = d.At.UTC().Format("01-02 15:04Z")
		}
		v.Decisions = append(v.Decisions, receiptRow{Label: d.Kind, By: d.By, Words: d.Decision, Note: d.Note, Draw: d.Draw, At: at})
	}
	for _, a := range r.Assessments {
		label := a.Source
		if a.Criterion != "" {
			label += " · " + a.Criterion
		}
		v.Assessments = append(v.Assessments, receiptRow{Label: label, Words: a.Verdict, Note: a.Note, At: a.Evidence})
	}
	return v
}

// chainReadout is the Chain section of a merged or done card's Activity
// tab and timeline (C38): the chain behind its change in the drawer's
// section grammar — the summary's hint the live fact, one row per fact,
// the section opening itself while the chain is red or stalled.
type chainReadout struct {
	Hint string
	Rows []chainRow
	Open bool
}

// chainRow is one row of the Chain readout: its fact with its clock and
// its age, its verdict, and the action that acts on it.
type chainRow struct {
	Why     string // the row's label: carry run, required, head of main
	Ref     string // the row's fact: a run's number, a context's name
	RefURL  string // the fact's link: a run on the forge, a card's id
	Card    bool   // the fact's link opens a card of this board
	At, Age string // the fact's clock and its age, "" to leave them out
	Verdict string // the fact's verdict word
	Hue     string // the verdict's colour class
	Act     string // the action that acts on the row, a card of this board
	ActID   string // the action card's id
}

// chainVerdict is a chain row's verdict word and hue: green for a
// success, amber for a wait, red for a failure.
func chainVerdict(status string) (word, hue string) {
	switch status {
	case "success":
		return "green", "st-merged"
	case "pending", "stalled":
		return status, "st-waiting"
	case "failure", "cancelled", "skipped":
		return status, "st-failed"
	}
	return status, "st-other"
}

// chainOf is the Chain section of card bt's drawer and timeline (C38):
// for a merged card the chain holds, its carry run with its verdict, the
// project's required contexts, the head of main and the chain's actors —
// the repair card of a red head, the re-run and the stall card of a
// stall; for a done card the chain covered, the cover run. nil while the
// chain holds nothing to say: no pull request, a roll that carries it, a
// change no run has picked up, a done card no green covers.
func (c *Console) chainOf(ctx context.Context, bt factoryd.BoardTask, force policy.Version, now time.Time) (*chainReadout, error) {
	if bt.PRNumber == 0 || bt.Status == "merged" && bt.RollID != "" || bt.Status != "merged" && bt.Status != "done" {
		return nil, nil
	}
	f, err := c.cfg.Tasks.ChainFacts(ctx, bt.Repository, bt.PRNumber)
	if err != nil {
		return nil, err
	}
	if bt.Status == "done" {
		if f.GreenRun == 0 || f.GreenRun < f.CarryRun || f.GreenAt.IsZero() || bt.FinishedAt == nil {
			return nil, nil
		}
		after := max(f.GreenAt.Sub(*bt.FinishedAt), 0)
		return &chainReadout{
			Hint: fmt.Sprintf("cover run %d · %s from merge", f.GreenRun, minutes(after)),
			Rows: []chainRow{{Why: "cover run", Ref: fmt.Sprintf("run %d", f.GreenRun), RefURL: c.runURL(bt.Repository, f.GreenRun),
				Age: minutes(after), Verdict: "live", Hue: "st-done"}},
		}, nil
	}
	if f.CarryRun == 0 {
		return nil, nil // the chain has not picked the change up
	}
	clock, age := func(t time.Time) string { return t.UTC().Format("15:04") }, func(t time.Time) string { return minutes(max(now.Sub(t), 0)) }
	word, hue := chainVerdict(f.CarryStatus)
	carry := chainRow{Why: "carry run", Ref: fmt.Sprintf("run %d", f.CarryRun), RefURL: c.runURL(bt.Repository, f.CarryRun),
		At: clock(f.CarryAt), Age: age(f.CarryAt), Verdict: word, Hue: hue}
	rows := []chainRow{carry}
	hint := fmt.Sprintf("run %d %s · %s", f.CarryRun, word, age(f.CarryAt))
	open := hue == "st-failed" || f.CarryStatus == "stalled"
	if _, cfg, ok := force.ProjectOf(bt.Repository); ok {
		if requires := cfg.Chain.Resolved().Requires; len(requires) > 0 {
			rows = append(rows, chainRow{Why: "required", Ref: strings.Join(requires, " "), Verdict: word, Hue: hue})
		}
	}
	if f.HeadRun > 0 {
		headWord, headHue := chainVerdict(f.HeadStatus)
		rows = append(rows, chainRow{Why: "head of main", Ref: fmt.Sprintf("run %d", f.HeadRun),
			RefURL: c.runURL(bt.Repository, f.HeadRun), At: clock(f.HeadAt), Age: age(f.HeadAt), Verdict: headWord, Hue: headHue})
		open = open || f.HeadStatus == "stalled"
	}
	switch {
	case f.Repair != nil && !factoryd.Final(f.Repair.Status):
		rows = append(rows, chainRow{Why: "repair card", Ref: fmt.Sprintf("#%d", f.Repair.IssueNumber), RefURL: f.Repair.ID,
			Card: true, Age: age(f.Repair.CreatedAt), Verdict: f.Repair.Status, Hue: statusClass(f.Repair.Status)})
		hint = fmt.Sprintf("run %d red · repair card #%d", f.CarryRun, f.Repair.IssueNumber)
	case f.Stall != nil && !factoryd.Final(f.Stall.Status) && (f.CarryStatus == "stalled" || f.HeadStatus == "stalled"):
		carry.Act, carry.ActID = fmt.Sprintf("stall card #%d", f.Stall.IssueNumber), f.Stall.ID
		hint = fmt.Sprintf("run %d stalled %s · stall card #%d", f.CarryRun, age(f.CarryAt), f.Stall.IssueNumber)
	case !f.RerunAt.IsZero() && f.HeadRun > 0:
		rows = append(rows, chainRow{Why: "re-run", Ref: fmt.Sprintf("run %d", f.HeadRun),
			RefURL: c.runURL(bt.Repository, f.HeadRun), Age: age(f.RerunAt), Verdict: "re-run", Hue: "st-waiting"})
	}
	rows[0] = carry
	return &chainReadout{Hint: hint, Rows: rows, Open: open}, nil
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

// said is a message of a session in the drawer's Chat: the agent's text
// between its tool calls, or a member's steer. Key is what the stream's
// copy of it carries, so the live one is not added twice.
type said struct {
	Key, Who, Text string
	Agent          bool
	At             time.Time
}

// cardDrawer answers GET /cards/{id}/drawer?tab=overview|activity|files|chat|receipt
// to a writer of the card's project, 403 to anyone else.
func (c *Console) cardDrawer(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab != "" && !slices.ContainsFunc(drawerTabs, func(t drawerTab) bool { return t.Name == tab }) {
		http.Error(w, "tab must be overview, chat, activity, files or receipt", http.StatusBadRequest)
		return
	}
	c.drawer(w, r, tab)
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
// inbox's message of it (factoryd.PokeOf) with its choices, the row the
// thread's answer buttons settle. A monitor's ask is not the card's.
func (c *Console) seedCardAsk(ctx context.Context, bt factoryd.BoardTask, who string, n int64) {
	events, err := c.cfg.Events.TaskEvents(ctx, bt.ID, startEvents)
	if err != nil {
		log.Printf("console: the events of %s: %v", bt.ID, err)
		return
	}
	var ask *factoryd.Event
	for i := range events { // newest first
		// a monitor's ask sits beside the card's run and is answered by its
		// case's number: the thread is the card's own ask's
		if events[i].Kind == factoryd.EventQuestionAsked && events[i].Detail["case_kind"] != string(factoryd.CaseMonitorFlag) {
			ask = &events[i]
			break
		}
	}
	if ask == nil {
		return
	}
	if s := factoryd.SettleOf(factoryd.ChatMessage{Kind: factoryd.MessageEscalation,
		BirthKind: factoryd.EventQuestionAsked, At: ask.At}, events, bt.Status, false); !s.Open {
		return
	}
	m, ok := factoryd.PokeOf(*ask, bt.Task, factoryd.Task{})
	if !ok {
		return
	}
	m.Role, m.Conversation = "user", n
	if _, err := c.cfg.Chat.AddChatMessage(ctx, bt.Repository, who, m); err != nil {
		log.Printf("console: the card thread of %s: %v", bt.ID, err)
	}
}

// drawer renders the card drawer on tab, "" for the card's default.
func (c *Console) drawer(w http.ResponseWriter, r *http.Request, tab string) {
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
	d := drawerData{Card: k, Tab: tab, Tabs: drawerTabs}
	// the drawer's head carries the title as filed, a human card's too
	if title := factoryd.CardTitle(bt.Task); title != "" {
		d.Card.Title = title
	}
	problem := func(what, action string, err error) {
		log.Printf("console: the drawer of %s could not read %s: %v", bt.ID, what, err)
		d.Problem = "The " + what + " could not be read: " + errText(err) + ". " + action
	}
	switch tab {
	case "overview":
		steps, err := c.readTimeline(ctx, bt.ID)
		if err != nil {
			log.Printf("console: the timeline of %s: %v", bt.ID, err)
		}
		d.Opened = c.opened(ctx, r, v, bt, k, steps, problem)
	case "activity":
		steps, err := c.readTimeline(ctx, bt.ID)
		if err != nil {
			problem("timeline", "Reload the board.", err)
			break
		}
		for _, m := range steps {
			d.Entries = append(d.Entries, entry(m))
		}
		d.Gates, d.Longest = gateRows(steps, bt.Task, time.Now())
		if d.Chain, err = c.chainOf(ctx, bt, v.policy, time.Now()); err != nil {
			problem("chain", "Reload the board.", err)
		}
	case "files":
		if bt.PRNumber == 0 {
			break
		}
		var err error
		if d.Files, d.MoreFiles, err = c.prFiles(r, bt.Repository, bt.PRNumber); err != nil {
			problem("pull request's files", "Reload the board.", err)
		}
	case "receipt":
		if c.cfg.Receipts != nil {
			rec, ok, err := c.cfg.Receipts.MergeReceipt(ctx, bt.ID)
			switch {
			case err != nil:
				problem("receipt", "Reload the board.", err)
			case ok:
				d.Receipt = receiptViewOf(rec)
			}
		}
	case "chat":
		var err error
		var no *refusal
		switch d.Session, d.Said, err = c.sessionChat(r, bt.Task); {
		case errors.As(err, &no):
			d.Problem = factoryd.ForHuman(no.msg)
		case err != nil:
			problem("session", "Reload the board.", err)
		}
	}
	c.render(w, "drawer.html", d)
}

// sessionChat is card k's newest session, nil before its first, and its
// messages in time order: the agent's text of each model call in the
// transcript, and the steers among the card's events from the session's
// start to its end. A transcript refused unread (a prover's) is the
// refusal.
func (c *Console) sessionChat(r *http.Request, k factoryd.Task) (*sessionRef, []said, error) {
	id := k.ID
	steps, err := c.readTimeline(r.Context(), id)
	all := sessionsOf(steps)
	if err != nil || len(all) == 0 {
		return nil, nil, err
	}
	s := sessionAt(all, 0)
	body, err := (&turn{c: c, r: r}).transcript(k, s)
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
