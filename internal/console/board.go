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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	"utc":         func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
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

// The project screen's five columns (ruling 255), a view over the task
// states and never a state of their own; the lane and the fine state are
// chips on the card.
const (
	colNeedsYou = iota
	colQueued
	colWorking
	colVerifying
	colDone
)

var fiveTitles = []string{"Needs you", "Queued", "Working", "Verifying", "Done"}

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
	case "running", "closing":
		return colWorking
	case "in-review":
		return colVerifying
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
	// Total is the task's time so far, or in all once it finished; GateAge
	// how long it has been in its gate, which GateText names as the card
	// reads it.
	Total, GateAge     string
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
	// clause.
	Title string
	// StatusClass is the pill's class, one hue per state.
	StatusClass string
	// Checks is "n/n" from the task's newest checks step, "" on a card
	// without a pull request, a final one, or one whose timeline holds none.
	Checks string
	// NearBudget is spend at 80 percent of the budget or more.
	NearBudget bool
	// Stale is a card without an event for the stale window: an amber
	// ring, and these words on its activity line.
	Stale     bool
	StaleText string
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

// titleClause is the face's title from the objective, on one line: up to
// the first full stop that ends a sentence — one followed by whitespace,
// or one at the very end — and at most 80 characters. A dot inside a file
// name or a version ("internal/console/board.go", "v1.2") ends no
// sentence, so the clause keeps it.
func titleClause(objective string) string {
	words := strings.Fields(objective)
	for i, w := range words {
		if strings.HasSuffix(w, ".") {
			words[i] = strings.TrimSuffix(w, ".")
			words = words[:i+1]
			break
		}
	}
	s := strings.TrimSpace(strings.Join(words, " "))
	if r := []rune(s); len(r) > 80 {
		s = strings.TrimRight(string(r[:80]), " ,;:")
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

// staleAfter bounds how long a card may hold no event before its face
// wears the amber ring.
const staleAfter = 30 * time.Minute

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

// checksTTL is how long what a card's face reads from its timeline is
// kept: the renders, polls and card swaps within it read it once.
const checksTTL = 30 * time.Second

// checks is a card's remembered checks count, retry, hold and wait on
// the provider, and when they were read.
type checks struct {
	count                    string
	retrying, held, provider bool
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
	Cards []card
	// Needs is the five columns' Needs you: what the wide view's human
	// lane holds and every card that needs the viewer, oldest first.
	Needs []need
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
}

// viewWide is the board's wide view: the lanes as columns first, then the
// task states, as the board was before the five columns.
const viewWide = "wide"

// boardFilter is what the board was asked to show: the project, the
// accent and the view; every form on it carries it back, so an action
// keeps the view.
type boardFilter struct {
	Project, Accent, View string
}

// formFilter is the board's filter a form carried back.
func formFilter(r *http.Request) boardFilter {
	return boardFilter{Project: r.PostFormValue("project"), Accent: r.PostFormValue("accent"), View: r.PostFormValue("view")}
}

// urlFilter is the board's filter in the URL's query.
func urlFilter(r *http.Request) boardFilter {
	q := r.URL.Query()
	return boardFilter{Project: q.Get("project"), Accent: q.Get("accent"), View: q.Get("view")}
}

func (f boardFilter) wide() bool { return f.View == viewWide }

// query is the filter as a URL query, empty when it names nothing.
func (f boardFilter) query() string {
	q := url.Values{}
	if f.Project != "" {
		q.Set("project", f.Project)
	}
	if f.Accent != "" {
		q.Set("accent", f.Accent)
	}
	if f.View != "" {
		q.Set("view", f.View)
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
	GitHub                    string
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
	// Project, Accent and View are the board's filter, which every form
	// on it carries back so an action keeps the view.
	boardFilter
	Query       string   // the filter as a URL query for the poll's hx-get
	Writes      bool     // the viewer writes the shown project: its stream and drawer are theirs
	Chat        bool     // the project agent's panel shows beside the five columns, its input to a writer
	ToggleQuery string   // the filter with the other view
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

// minutes is a duration in whole minutes, or hours and minutes.
func minutes(d time.Duration) string {
	m := int(d.Minutes())
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
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
	if f.Accent != "" && !factoryd.IsAccent(f.Accent) {
		http.Error(w, "accent must be one of "+strings.Join(factoryd.Accents, ", "), http.StatusBadRequest)
		return
	}
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
		name = "board"
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
		name = "board"
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
	filter          boardFilter
	byRepo          map[string]factoryd.Project
	byID            map[string]factoryd.Task
	pending         []factoryd.ApprovalRequest
	carded, planned map[string]bool
	unchecked       bool // a may-move check failed, logged once per render
	problems        []string
}

// viewOf reads what every card of a render shares, and the projects agents
// work on, by name.
func (c *Console) viewOf(r *http.Request, f boardFilter) (*boardView, []factoryd.Project, error) {
	ctx, s := r.Context(), current(r)
	v := &boardView{user: s.user, csrf: s.csrf, filter: f, byRepo: map[string]factoryd.Project{},
		byID: map[string]factoryd.Task{}, carded: map[string]bool{}, planned: map[string]bool{}}
	v.policy, _ = c.inForce(ctx)
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
	}
	return v, all, nil
}

// humanLane: t has no agent, so its card is in the human lane: a human
// task, or a roll task until its card (ruling 178).
func (v *boardView) humanLane(t factoryd.Task) bool {
	return t.Kind == "human" || t.Source == factoryd.SourceRoll && !factoryd.Final(t.Status) && !v.carded[t.ID]
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
	// the five columns are no lanes: a card wears its lane, unless its
	// status says the same word
	if !v.filter.wide() {
		k.Badge = t.Lane
		if t.Lane == t.Status {
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
	// newest event is older than half an hour wears it, one that never had
	// an event its own age — comes only on a card that is running its
	// work: never a final one, never one in the backlog or the plan lane.
	setActivity := func(k *card) {
		ring := func(d time.Duration) bool { return d >= staleAfter }
		live := !factoryd.Final(k.Status) && k.Status != "backlog" && k.Status != "plan"
		reason := func() string {
			q, _, _ := strings.Cut(strings.TrimPrefix(k.Reason, factoryd.AgentAsks), "\n")
			return q
		}
		if e, ok := newest[k.ID]; ok {
			k.Activity = activityLine(e)
			k.Stale = live && ring(time.Since(e.At))
			if k.Stale {
				k.StaleText = "nothing for over " + minutes(time.Since(e.At))
			}
			if k.Reason != "" && strings.HasPrefix(k.Status, "waiting") {
				k.Activity = reason()
				k.WaitingForActivity = true
			}
			return
		}
		k.Stale = live && ring(time.Since(k.CreatedAt))
		if k.Stale {
			k.StaleText = "no event yet, " + minutes(time.Since(k.CreatedAt)) + " on the card"
		}
		if k.Reason != "" {
			k.Activity = reason()
		}
	}
	each(setActivity)
	c.faceTimelines(ctx, each)
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

// boardOf is the board of one project, f.Project or else the first the
// viewer writes, narrowed to f.Accent ("" every accent), as the session's
// human sees it. The five columns are the default, the human lane folded
// into Needs you; the wide view puts the lanes of the project's process
// first in the fixed order backlog, plan, ready, then the state columns,
// under the human lane.
func (c *Console) boardOf(r *http.Request, f boardFilter) (boardData, error) {
	ctx := r.Context()
	v, all, err := c.viewOf(r, f)
	d := boardData{User: v.user, CSRF: v.csrf, Factory: c.factoryWriter(r)}
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
		for _, title := range fiveTitles {
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
		case col == colWorking && t.Status == "running" && c.timelineFace(ctx, t.ID).provider:
			d.Status.Waiting++
		case col == colWorking:
			d.Status.Working++
		case col == colNeedsYou && !v.planned[t.ID]:
			d.Status.NeedYou++
		}
		if t.Status == "waiting-human" && v.planned[t.ID] {
			escalations[t.ID] = bt
		}
		// the accent filter narrows the board to one accent; a card with
		// none is in no accent's board, and an accent never changes how a
		// state renders.
		if f.Accent != "" && t.Accent != f.Accent {
			continue
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
	if shown.Name != "" {
		c.statusLine(ctx, v, shown, &d)
		d.Pulse = c.pulseOf(r, shown)
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
	if err := c.writes(r.Context(), button(r), t.Repository, "task "+t.ID); err != nil {
		c.fail(w, r, err)
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
		r.PostFormValue("body"), r.PostFormValue("lane"), r.PostFormValue("priority"), r.PostFormValue("size"))
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

func (c *Console) forgejoFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	log.Printf("console: %s could not %s: %v", current(r).user, what, err)
	if c.session(r) == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Error(w, "Forgejo refused to "+what+"; see the factoryd log", http.StatusBadGateway)
}

// writableCard is the card the path names, built as the board shows it
// under the URL's filter (?project, ?accent, ?view), for a writer of its
// project; anyone else gets refused and an unknown card 404. false: the
// answer is written.
func (c *Console) writableCard(w http.ResponseWriter, r *http.Request, refused int) (factoryd.BoardTask, card, *boardView, bool) {
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
	may, err := c.mayMoveCached(ctx, bt.Repository, current(r).user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", current(r).user, bt.Repository, err)
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return bt, card{}, nil, false
	case !may && refused == http.StatusNotFound:
		http.NotFound(w, r)
		return bt, card{}, nil, false
	case !may:
		http.Error(w, "only the project's writers open its cards", refused)
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
// carrying the filter (?project, ?accent, ?view) of the board it is
// swapped into.
func (c *Console) cardFragment(w http.ResponseWriter, r *http.Request) {
	if _, k, _, ok := c.writableCard(w, r, http.StatusNotFound); ok {
		c.render(w, "card", k)
	}
}

// drawerTab is a tab of the card drawer: its ?tab= and its title.
type drawerTab struct{ Name, Title string }

// drawerTabs are the card drawer's tabs, the first its default.
var drawerTabs = []drawerTab{{"overview", "Overview"}, {"activity", "Activity"}, {"files", "Files"}}

// drawerData is the card drawer on one tab, under the card's face.
// Overview: the objective as its headline, a planned card's provenance
// line and its body, the plan's table, a pending merge's reviews and
// audit, and the related cards. Activity: the timeline page's gates and
// steps. Files: the pull request's changed files.
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
	MoreFiles                  bool   // Files are the first filesPages pages
	Problem                    string // what the tab could not read
}

// related is a card the drawer's card relates to, Why, in its live state.
type related struct {
	Why  string
	Card card
}

// cardDrawer answers GET /cards/{id}/drawer?tab=overview|activity|files
// to a writer of the card's project, 403 to anyone else.
func (c *Console) cardDrawer(w http.ResponseWriter, r *http.Request) {
	tab := cmp.Or(r.URL.Query().Get("tab"), drawerTabs[0].Name)
	if !slices.ContainsFunc(drawerTabs, func(t drawerTab) bool { return t.Name == tab }) {
		http.Error(w, "tab must be overview, activity or files", http.StatusBadRequest)
		return
	}
	bt, k, v, ok := c.writableCard(w, r, http.StatusForbidden)
	if !ok {
		return
	}
	ctx := r.Context()
	d := drawerData{Card: k, Tab: tab, Tabs: drawerTabs}
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
				d.Merge = &m
			}
		}
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
	}
	c.render(w, "drawer.html", d)
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
