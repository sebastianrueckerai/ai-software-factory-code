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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

var funcs = template.FuncMap{
	"eur4": func(v float64) string { return fmt.Sprintf("%.4f", v) },
	// eur2 is money to the cent, as a card's face reads spend against budget
	"eur2": func(v float64) string { return fmt.Sprintf("\u20ac%.2f", v) },
	// accents are the colour labels a writer may put on a card
	"accents": func() []string { return factoryd.Accents },
	// lease renders minutes as whole hours when they are
	"lease": func(minutes int) string {
		if minutes%60 == 0 {
			return fmt.Sprintf("%d h", minutes/60)
		}
		return fmt.Sprintf("%d min", minutes)
	},
	"utc": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	// pct is spend of cap in whole percent, 0 without a cap; gaugeClass
	// is the gauge's class: warn from 80 percent, over past the cap
	"pct":    pct,
	"pct100": func(spent, cap float64) int { return min(pct(spent, cap), 100) },
	// initial is a member's avatar letter
	"initial": func(s string) string { return strings.ToUpper(string([]rune(s)[:min(1, len([]rune(s)))])) },
	// switch is whether the top bar shows the project select: the board
	// page's data has the projects, the other pages' has none
	"switch": func(d any) bool { b, ok := d.(boardData); return ok && len(b.Projects) > 0 },
	"gaugeClass": func(spent, cap float64) string {
		switch {
		case cap > 0 && spent > cap:
			return " over"
		case pct(spent, cap) >= 80:
			return " warn"
		}
		return ""
	},
	"inc":         func(i int) int { return i + 1 },
	"join":        strings.Join,
	"verdictForm": func(id int64, csrf, back string) verdictForm { return verdictForm{id, csrf, back} },
	// alertSupervisor is what the supervisor did on a machine alert; an
	// escalation's detail stays off the page
	"alertSupervisor": func(a factoryd.Alert) supervised {
		if a.Kind == factoryd.AlertEscalation {
			return supervised{}
		}
		return supervisedBy(a.Detail["action"], a.Detail["diagnosis"], a.Detail["recommendation"])
	},
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

// arrival orders the four columns' cards newest first (Sebastian,
// 2026-10-14): Queued by filing, In progress by admission, Done by
// finishing; a card without the time keeps its filing time, and the
// store's order holds among equals. Needs you stays oldest waiting first.
func arrival(cols []column) {
	at := func(k card) time.Time {
		switch {
		case slices.Contains([]string{"running", "closing", "in-review"}, k.Status) && k.AdmittedAt != nil:
			return *k.AdmittedAt
		case factoryd.Final(k.Status) && k.FinishedAt != nil:
			return *k.FinishedAt
		}
		return k.CreatedAt
	}
	for i := range cols {
		slices.SortStableFunc(cols[i].Cards, func(a, b card) int { return at(b).Compare(at(a)) })
	}
}

// find narrows the columns to the terms of q (C13): a word matches what
// the face says (the ref, the title, the state and stage words, the
// activity line, the chips, an item's headline), a key:value term a fact
// behind the face (plan:558, stage:verifying, status:failed, is:failed
// and the other exceptional states, by:sebastian, model:coding-cheap,
// pr:612), and every term must match. Each column keeps its whole count;
// the columns given stay as they were, for the status panel.
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

// titleCap bounds the face's title in runes; titleEllipsis ends a title
// cut to it.
const (
	titleCap      = 80
	titleEllipsis = "…"
)

// titleClause is the face's title from the objective, on one line: the
// objective's first paragraph, the title as the issue was filed, in full —
// a blank line ends the paragraph, a full stop never does. Only a title
// longer than titleCap runes is shortened, at a word boundary with an
// ellipsis, never mid-word. The objective is sanitised as a brief carries
// it, so a hidden comment in it shows on no face.
func titleClause(objective string) string {
	var words []string
	for _, line := range strings.Split(sanitize.Inbound(objective), "\n") {
		if strings.TrimSpace(line) == "" {
			if len(words) > 0 {
				break
			}
			continue
		}
		words = append(words, strings.Fields(line)...)
	}
	s := strings.Join(words, " ")
	if r := []rune(s); len(r) > titleCap {
		cut := r[:titleCap-len([]rune(titleEllipsis))]
		end := len(cut)
		for end > 0 && cut[end-1] != ' ' {
			end--
		}
		if end == 0 {
			end = len(cut) // one word longer than the cap: cut it
		}
		s = strings.TrimRight(string(cut[:end]), " ,;:") + titleEllipsis
	}
	return s
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
	Cards []card
	// Needs is the five columns' Needs you: what the wide view's human
	// lane holds and every card that needs the viewer, oldest first.
	Needs []need
}

// Count is the number the column's head shows: its needs, else its cards.
func (c column) Count() int {
	if len(c.Needs) > 0 {
		return len(c.Needs)
	}
	return len(c.Cards)
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

// anchor is where the item is on the page: an escalation's article, else
// the card's drawer.
func (n need) anchor() string {
	if n.Escalated {
		return "#need-" + n.Card.ID
	}
	return "#card=" + n.Card.ID
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
}

// formFilter is the board's filter a form carried back.
func formFilter(r *http.Request) boardFilter {
	return boardFilter{Project: r.PostFormValue("project"), View: r.PostFormValue("view"), Q: strings.TrimSpace(r.PostFormValue("q"))}
}

// urlFilter is the board's filter in the URL's query.
func urlFilter(r *http.Request) boardFilter {
	q := r.URL.Query()
	return boardFilter{Project: q.Get("project"), View: q.Get("view"), Q: strings.TrimSpace(q.Get("q"))}
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
	Project                   string
	State                     string
	Working, Waiting, NeedYou int
	Spend                     factoryd.ProjectSpend
	CapEUR                    float64
	// TodayEUR is the factory's spend this UTC day and DayCapEUR the
	// policy's day cap, as /costs shows them.
	TodayEUR, DayCapEUR float64
	GitHub              string
	GitHubURL           string // the export's remote on GitHub, "" when not exported
}

// waiting is "; n waiting on the provider", "" for none.
func (s statusLine) waiting() string {
	if s.Waiting == 0 {
		return ""
	}
	return fmt.Sprintf("; %d %s", s.Waiting, providerWords)
}

type boardData struct {
	User  string
	CSRF  string
	Filed string
	Note  string // what the human's last action did
	// Project and View are the board's filter, which every form
	// on it carries back so an action keeps the view.
	boardFilter
	Query       string   // the filter as a URL query for the poll's hx-get
	Writes      bool     // the viewer writes the shown project: its drawer and its stream's rows are theirs
	Chat        bool     // the project agent's panel shows beside the five columns, its input to a writer
	ToggleQuery string   // the filter with the other view
	ClearQuery  string   // the filter without the find, the ×'s link (C13)
	whole       []column // the columns before the find narrowed them, the status panel's
	Projects    []string // the projects the viewer writes, and the shown one
	Status      statusLine
	Pulse       *factoryd.Pulse // the shown project's newest pulse
	Columns     []column
	HumanTasks  []card
	Escalations []need
	Approvals   []approvalCard
	Problems    []string
	Approver    bool // may authorize the shown project's spend (rulings 128, 254)
	Factory     bool // writes the factory's own code: the layout links the factory-level pages
	// PolicyVersion, PolicyCommit and PolicyTreeURL are the configuration
	// version factoryd runs, as /policy shows it: its number and its
	// commit, linked to the commit's tree in Forgejo. All zero when the
	// policy could not be read, where the board shows no line.
	PolicyVersion int64
	PolicyCommit  string
	PolicyTreeURL string
	Ticker        []tick // the project's newest events along the foot
}

func pct(spent, cap float64) int {
	if cap <= 0 {
		return 0
	}
	return int(math.Round(spent / cap * 100))
}

// cols are the columns as read, before the find narrowed them: the
// status panel lists the project's cards, not the find's.
func (d boardData) cols() []column {
	if d.whole != nil {
		return d.whole
	}
	return d.Columns
}

// InProgress are the cards in progress for the status panel, with their
// stage, in the columns' order.
func (d boardData) InProgress() []card {
	var out []card
	for _, col := range d.cols() {
		for _, k := range col.Cards {
			if k.Stage != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// needLink is one Needs you item in the status panel: its headline and
// the anchor of the item in the column.
type needLink struct{ Words, Href string }

// NeedLinks are the Needs you items for the status panel: the first
// column's in the four columns; in the wide view, which keeps them in
// its own lists, the approvals, the escalations and the human tasks.
func (d boardData) NeedLinks() []needLink {
	var out []needLink
	if len(d.Columns) == 0 {
		return nil
	}
	needs := d.cols()[0].Needs
	if needs == nil {
		for i := range d.Approvals {
			needs = append(needs, need{Since: d.Approvals[i].CreatedAt, Approval: &d.Approvals[i]})
		}
		for _, n := range d.Escalations {
			needs = append(needs, n)
		}
		for _, k := range d.HumanTasks {
			needs = append(needs, need{Since: k.CreatedAt, Card: k})
		}
	}
	for _, n := range needs {
		switch {
		case n.Approval != nil && n.Approval.Kind == factoryd.ApprovalMerge:
			out = append(out, needLink{fmt.Sprintf("Merge PR #%d", n.Approval.PRNumber), fmt.Sprintf("#approval-%d", n.Approval.ID)})
		case n.Approval != nil && n.Approval.Kind == factoryd.ApprovalPlan:
			out = append(out, needLink{fmt.Sprintf("Plan #%d", n.Approval.Issue), fmt.Sprintf("#approval-%d", n.Approval.ID)})
		case n.Approval != nil:
			out = append(out, needLink{"Resource request", fmt.Sprintf("#approval-%d", n.Approval.ID)})
		case n.Card.IssueNumber > 0:
			out = append(out, needLink{fmt.Sprintf("#%d %s", n.Card.IssueNumber, n.Card.Title), n.anchor()})
		default:
			out = append(out, needLink{n.Card.Title, n.anchor()})
		}
	}
	return out
}

// tick is one item of the ticker along the foot: a project event with the
// card's words.
type tick struct {
	At, Glyph, Ref, Words, Class string
}

// tickerLimit bounds the events the ticker shows; tickerWindow is how far
// back they reach, and tickerQuiet how many the ticker keeps regardless
// of age when the window holds none.
const (
	tickerLimit  = 24
	tickerWindow = 24 * time.Hour
	tickerQuiet  = 5
)

// recentTicks keeps the rows of the last tickerWindow, newest first; a
// quiet project keeps its newest tickerQuiet rows, so the ticker never
// scrolls through old days beside today's events, and never stands empty.
func recentTicks(rows []factoryd.Event, now time.Time) []factoryd.Event {
	var recent []factoryd.Event
	for _, e := range rows {
		if now.Sub(e.At) <= tickerWindow {
			recent = append(recent, e)
		}
	}
	if len(recent) > 0 {
		return recent
	}
	return rows[:min(len(rows), tickerQuiet)]
}

// tickerOf is the project's newest events as ticks, newest first: a
// status row by its status word and glyph, any other row by its kind, the
// card's title when it is on the board.
func (c *Console) tickerOf(ctx context.Context, v *boardView, p factoryd.Project) []tick {
	rows, err := c.cfg.Events.ProjectEvents(ctx, p.Repository, 0, tickerLimit)
	if err != nil {
		log.Printf("console: the ticker of %s: %v", p.Repository, err)
		return nil
	}
	var out []tick
	for _, e := range recentTicks(rows, time.Now()) {
		k := tick{At: e.At.UTC().Format("15:04"), Glyph: "·", Ref: e.TaskID, Class: "event"}
		if e.IssueNumber > 0 {
			k.Ref = fmt.Sprintf("#%d", e.IssueNumber)
		}
		if t, ok := v.byID[e.TaskID]; ok {
			k.Words = c.card(t).Title
		}
		if e.Kind == factoryd.EventTaskStatus {
			status, _ := e.Detail["status"].(string)
			switch status {
			case "done":
				k.Glyph, k.Class = "✓", "done"
			case "failed":
				k.Glyph, k.Class = "✕", "failed"
			case "closed":
				k.Glyph, k.Class = "⊘", "closed"
			}
			if k.Words == "" {
				k.Words = status
			} else if k.Class == "event" {
				k.Words += " · " + status
			}
		} else if k.Words == "" {
			k.Words = e.Kind + " by " + e.Actor
		}
		out = append(out, k)
	}
	return out
}

// Final is a done, failed or closed card: a two-line row on the board.
func (k card) Final() bool { return factoryd.Final(k.Status) }

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

// boardLimit bounds the tasks one board render reads.
const boardLimit = 300

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
		return titleClause(t.Objective)
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
	f := urlFilter(r)
	if f.View != "" && !f.wide() {
		http.Error(w, "view must be "+viewWide+" or none", http.StatusBadRequest)
		return
	}
	d, err := c.boardOf(r, f)
	switch {
	case errors.Is(err, errNoProject):
		http.Error(w, "no project "+f.Project+" takes agent tasks", http.StatusNotFound)
		return
	case err != nil:
		log.Printf("console: board: %v", err)
		http.Error(w, "cannot read the board", http.StatusInternalServerError)
		return
	}
	d.Filed = r.URL.Query().Get("filed")
	name := "page.html"
	if r.URL.Path == "/board" {
		name = "board-fragment"
	}
	c.render(w, name, d)
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
	d, err := c.boardOf(r, f)
	if errors.Is(err, errNoProject) {
		f.Project = ""
		d, err = c.boardOf(r, f)
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
	merging         map[string]bool // a pending merge approval cards the task's merge decision
	unchecked       bool            // a may-move check failed, logged once per render
	problems        []string
}

// viewOf reads what every card of a render shares, and the projects agents
// work on, by name.
func (c *Console) viewOf(r *http.Request, f boardFilter) (*boardView, []factoryd.Project, error) {
	ctx, s := r.Context(), current(r)
	v := &boardView{user: s.user, csrf: s.csrf, filter: f, byRepo: map[string]factoryd.Project{},
		byID: map[string]factoryd.Task{}, carded: map[string]bool{}, planned: map[string]bool{}, merging: map[string]bool{}}
	v.policy, v.policyOK = c.inForce(ctx)
	all, err := c.coderProjects(ctx)
	if err != nil {
		return v, nil, fmt.Errorf("projects: %w", err)
	}
	for _, p := range all {
		v.byRepo[p.Repository] = p
	}
	if v.pending, err = c.cfg.Approvals.ApprovalRequests(ctx, "pending"); err != nil {
		log.Printf("console: approvals: %v", err)
		v.problems = append(v.problems, "approval requests could not be read")
	}
	for _, a := range v.pending {
		v.carded[a.TaskID] = true
		v.planned[a.TaskID] = v.planned[a.TaskID] || a.Kind == factoryd.ApprovalPlan
		v.merging[a.TaskID] = v.merging[a.TaskID] || a.Kind == factoryd.ApprovalMerge
	}
	return v, all, nil
}

// humanLane: t has no agent, so its card is in the human lane: a human
// task, or a roll task whose merge decision is its one remaining action —
// a pending merge approval or a waiting-human state (ruling 219); while
// its checks or review run the status mapping places it like any task.
func (v *boardView) humanLane(t factoryd.Task) bool {
	return t.Kind == "human" || t.Source == factoryd.SourceRoll && !factoryd.Final(t.Status) &&
		(t.Status == "waiting-human" || v.merging[t.ID])
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
		fiveColumn(t, v.user))
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
		k.Reason, k.Choices, k.EscalationAlert = bt.Escalation, bt.Choices, bt.EscalationAlert
		k.Capped = capFactsFrom(t, v.policy.Document.Limits.Sessions, bt.Escalation, v.pending).capped
		// the Fund action is offered where the answer path's floor refuses
		// the answer (ruling 322): the same card, to its project's approvers
		if v.approves(t.Repository) && c.unfundedAnswer(ctx, t) != "" {
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

// activityLine is event e on its card's activity line: what happened, by
// whom, which way and when; a task_status row names the status and gate
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
	return fmt.Sprintf("%s by %s via %s at %s", e.Kind, e.Actor, e.Via, at)
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
			if k.Reason != "" && strings.HasPrefix(k.Status, "waiting") {
				k.Activity = reason()
				k.WaitingForActivity = true
			}
			return
		}
		k.Stale = live && ring(time.Since(lastAt(k.CreatedAt)))
		if k.Stale {
			k.StaleText = "no event yet, " + minutes(time.Since(lastAt(k.CreatedAt))) + " on the card"
		}
		if k.Reason != "" {
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

// boardOf is the board of one project, f.Project or else the first the
// viewer writes, the project's cards, as the session's
// human sees it. The five columns are the default, the human lane folded
// into Needs you; the wide view puts the lanes of the project's process
// first in the fixed order backlog, plan, ready, then the state columns,
// under the human lane.
func (c *Console) boardOf(r *http.Request, f boardFilter) (boardData, error) {
	ctx := r.Context()
	v, all, err := c.viewOf(r, f)
	d := boardData{User: v.user, CSRF: v.csrf, Factory: c.factoryWriter(r)}
	if v.policyOK {
		d.PolicyVersion, d.PolicyCommit, d.PolicyTreeURL = v.policy.ID, v.policy.Commit, c.configTree(v.policy.Commit)
	}
	if err != nil {
		return d, err
	}
	var shown factoryd.Project
	writable := map[string]bool{}
	for _, p := range all {
		// a project whose writers could not be checked is offered, its
		// buttons hidden, rather than an empty board
		writes, err := c.mayMoveCached(ctx, p.Repository, v.user, policy.MovedByWriters)
		if err != nil && !v.unchecked {
			v.unchecked = true
			log.Printf("console: may %s write %s: %v", v.user, p.Repository, err)
			v.problems = append(v.problems, "who may move a card could not be checked: the move buttons are hidden")
		}
		writable[p.Name] = writes
		if writes || err != nil {
			d.Projects = append(d.Projects, p.Name)
			if f.Project == "" && shown.Name == "" {
				shown = p
			}
		}
		if p.Name == f.Project {
			shown = p
		}
	}
	switch {
	case f.Project != "" && shown.Name == "":
		return d, errNoProject
	case shown.Name == "":
		d.Problems = append(d.Problems, "You write none of the projects agents work on.")
	case !slices.Contains(d.Projects, shown.Name):
		d.Projects = append(d.Projects, shown.Name)
	}
	// a member sees only the projects they write or read (ruling 254); the
	// factory's writers see every board, read-only where they do not write
	if shown.Name != "" && !writable[shown.Name] && v.user != "" && !d.Factory {
		if reads, err := c.reads(ctx, current(r), shown); err != nil || !reads {
			return d, errNoProject
		}
	}
	f.Project, d.Writes, d.Approver = shown.Name, writable[shown.Name], v.approves(shown.Repository)
	d.Chat = c.cfg.Chat != nil && c.cfg.Operator != nil && shown.Name != "" && !f.wide()
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
			d.Columns = append(d.Columns, column{Title: col.Title})
		}
		byStatus["closing"] = byStatus["running"]
	} else {
		for _, title := range columnTitles {
			d.Columns = append(d.Columns, column{Title: title})
		}
	}

	tasks, err := c.cfg.Tasks.BoardTasks(ctx, time.Now().Add(-finishedSince), boardLimit)
	if err != nil {
		return d, fmt.Errorf("tasks: %w", err)
	}
	if len(tasks) == boardLimit {
		d.Problems = append(d.Problems, fmt.Sprintf("the board shows the first %d tasks", boardLimit))
	}
	for _, bt := range tasks {
		v.byID[bt.ID] = bt.Task
	}
	escalations := map[string]factoryd.BoardTask{}
	var needs []need
	escalation := func(k card) need {
		return need{Card: k, Escalated: true, CSRF: v.csrf, Approver: d.Factory && c.approver(r), Back: "/" + d.Query}
	}
	for _, bt := range tasks {
		t := bt.Task
		if t.Repository != shown.Repository {
			continue
		}
		human, col := v.humanLane(t), fiveColumn(t, v.user)
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
			d.Columns[col].Cards = append(d.Columns[col].Cards, k)
		}
	}
	if !wide {
		arrival(d.Columns)
	}
	for _, a := range v.pending {
		if a.Project != shown.Repository {
			continue
		}
		k := c.approvalCard(ctx, a, v.byID, v.csrf, d.Approver)
		k.Filter = f
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
	if shown.Name != "" {
		c.statusLine(ctx, v, shown, &d)
		d.Pulse = c.pulseOf(r, shown)
		d.Ticker = c.tickerOf(ctx, v, shown)
	}
	d.Problems = append(d.Problems, v.problems...)
	return d, nil
}

// statusLine completes the board's status line with the project's state
// word, its spend this UTC month (its chat's included) and its month cap:
// its file's budget.month_eur, else the policy's.
func (c *Console) statusLine(ctx context.Context, v *boardView, p factoryd.Project, d *boardData) {
	s := &d.Status
	s.Project = p.Name
	switch {
	case s.NeedYou > 0:
		s.State = "waiting on you"
	case s.Working > 0:
		s.State = "working"
	default:
		s.State = "idle"
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

func (c *Console) newTaskForm(w http.ResponseWriter, r *http.Request) {
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
		"Projects": ps, "Priorities": offered, "Lanes": lanes})
}

// createTask files the issue as the human (doFileCard).
func (c *Console) createTask(w http.ResponseWriter, r *http.Request) {
	_, issue, err := c.doFileCard(r.Context(), button(r), r.PostFormValue("project"), r.PostFormValue("title"),
		r.PostFormValue("body"), r.PostFormValue("lane"), r.PostFormValue("priority"), r.PostFormValue("size"), "", "")
	if err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/?"+url.Values{"project": {r.PostFormValue("project")}, "filed": {strconv.FormatInt(issue, 10)}}.Encode(), http.StatusSeeOther)
}

// answer posts the human's answer to an escalation (doAnswer).
func (c *Console) answer(w http.ResponseWriter, r *http.Request) {
	if _, err := c.doAnswer(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("answer")); err != nil {
		c.fail(w, r, err)
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

// drawerTabs are the card drawer's tabs: Chat the default of a running
// card, Overview of any other.
var drawerTabs = []drawerTab{{"overview", "Overview"}, {"chat", "Chat"}, {"activity", "Activity"}, {"files", "Files"}}

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
				m := c.approvalCard(ctx, a, v.byID, v.csrf, v.approves(a.Project))
				m.Filter, m.IDSuffix = k.Filter, "-d"
				d.Merge = &m
			}
		}
		if v.escalated(bt.Task) {
			d.Need = &need{Card: k, Escalated: true, CSRF: v.csrf, Approver: c.approver(r), Back: "/" + k.Filter.query(), Suffix: "-d"}
		}
		d.Runs = c.lifeOf(ctx, bt.Task)
		d.Life = lifeLine(d.Runs, bt.SpentEUR)
		var err error
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
