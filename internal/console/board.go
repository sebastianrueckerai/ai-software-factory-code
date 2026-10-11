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

// fiveColumns are the project screen's five columns: a view over task
// states, never a new state. Each maps one or more statuses into a
// column; the lane is a chip on the card, and the fine state (waiting on
// the author, failed, closed, retrying, stale) a second chip.
var fiveColumns = []struct {
	Title    string
	Statuses []string
}{
	{"Needs you", []string{"waiting-human"}},
	{"Queued", []string{"backlog", "queued", "waiting-deps"}},
	{"Working", []string{"running", "closing"}},
	{"Verifying", []string{"in-review"}},
	{"Done", []string{"done", "failed", "closed"}},
}

// stateColumns follow the lane columns, in board order; closing counts as
// running. Kept for the wide view.
var stateColumns = []struct{ Title, Status string }{
	{"Running", "running"}, {"Waiting on a human", "waiting-human"}, {"Waiting on the author", "waiting-poster"},
	{"In review", "in-review"}, {"Done", "done"}, {"Failed", "failed"}, {"Closed", "closed"},
}

type card struct {
	factoryd.Task
	IssueURL string
	PRURL    string
	Reason   string // an escalation's reason
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
	// MayWriteSummary is whether the viewer writes the task's project, so
	// the expanded view carries the summary and accent forms; CSRF is the
	// session's token for them.
	MayWriteSummary bool
	CSRF            string
	// FilterProject and FilterAccent are the board's filter the card's
	// forms carry back so an action keeps the view.
	FilterProject, FilterAccent string
	// Place is where the board puts the card (boardCard).
	Place string
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

// checksTTL is how long a card's checks count is kept: the renders, polls
// and card swaps within it read the card's timeline once.
const checksTTL = 30 * time.Second

// checks is a remembered checks count and when it was read.
type checks struct {
	count string
	at    time.Time
}

// faceTimelines reads the newest checks step onto every card that can
// have one: a card with a pull request, not yet final, at most once per
// checksTTL. A timeline that cannot be read keeps the card without the
// chip — the object store answers no body for a task that never wrote a
// timeline, and reading is best-effort here, never a reason to fail the
// board.
func (c *Console) faceTimelines(ctx context.Context, everyCard func(func(*card))) {
	everyCard(func(k *card) {
		if k.PRNumber == 0 || factoryd.Final(k.Status) {
			return
		}
		c.mu.Lock()
		got, ok := c.checks[k.ID]
		c.mu.Unlock()
		if !ok || time.Since(got.at) >= checksTTL {
			got = checks{at: time.Now()}
			if steps, err := c.readTimeline(ctx, k.ID); err != nil {
				log.Printf("console: timeline of %s for its checks: %v", k.ID, err)
			} else {
				got.count = newestChecksCount(steps)
			}
			c.mu.Lock()
			for id, old := range c.checks {
				if time.Since(old.at) >= checksTTL {
					delete(c.checks, id)
				}
			}
			if c.checks == nil {
				c.checks = map[string]checks{}
			}
			c.checks[k.ID] = got
			c.mu.Unlock()
		}
		k.Checks = got.count
	})
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
// rides on the form from the card's FilterProject and FilterAccent.
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
}

// boardFilter is what the board was asked to show: the project and the
// accent; every form on it carries it back, so an action keeps the view.
type boardFilter struct {
	Project, Accent string
}

// formFilter is the board's filter a form carried back.
func formFilter(r *http.Request) boardFilter {
	return boardFilter{Project: r.PostFormValue("project"), Accent: r.PostFormValue("accent")}
}

// query is the filter as a URL query, empty when it names nothing.
func (f boardFilter) query() string {
	q := url.Values{}
	if f.Project != "" {
		q.Set("project", f.Project)
	}
	if f.Accent != "" {
		q.Set("accent", f.Accent)
	}
	if q.Encode() == "" {
		return ""
	}
	return "?" + q.Encode()
}

// statusLine is the project screen's quantitative summary above the board:
// the project's state word, counts of working and needing-you cards, and
// spend against the month cap.
type statusLine struct {
	State      string // "working", "waiting on you", or "idle"
	Working    int
	NeedYou    int
	SpendEUR   float64
	MonthCap   float64
	ProjectURL string // link to /costs
	NeedYouURL string // link to the Needs you column
}

type boardData struct {
	User  string
	CSRF  string
	Filed string
	Note  string // what the human's last action did
	// Project and Accent are the board's filter, which every form on it
	// carries back so an action keeps the view.
	boardFilter
	Query       string // the filter as a URL query for the poll's hx-get
	Projects    []string
	Status      statusLine
	Columns     []column
	HumanTasks  []card
	Escalations []card
	Approvals   []approvalCard
	Problems    []string
	Approver    bool   // may authorize spend (ruling 128)
	View        string // "" (default five columns) or "wide" (lanes first)
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
	project := r.URL.Query().Get("project")
	accent := r.URL.Query().Get("accent")
	view := r.URL.Query().Get("view")
	if accent != "" && !factoryd.IsAccent(accent) {
		http.Error(w, "accent must be one of "+strings.Join(factoryd.Accents, ", "), http.StatusBadRequest)
		return
	}
	if view != "" && view != "wide" {
		http.Error(w, "view must be wide or empty", http.StatusBadRequest)
		return
	}
	d, err := c.boardOf(r, project, accent, view)
	switch {
	case errors.Is(err, errNoProject):
		http.Error(w, "no project "+project+" takes agent tasks", http.StatusNotFound)
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
	view := r.PostFormValue("view")
	if r.Header.Get("HX-Request") == "" && status == http.StatusOK {
		q := f.query()
		if view != "" {
			if q == "" {
				q = "?view=" + view
			} else {
				q += "&view=" + view
			}
		}
		http.Redirect(w, r, "/"+q, http.StatusSeeOther)
		return
	}
	d, err := c.boardOf(r, f.Project, f.Accent, view)
	if errors.Is(err, errNoProject) {
		d, err = c.boardOf(r, "", f.Accent, view)
	}
	if err != nil {
		log.Printf("console: board: %v", err)
		http.Error(w, note+" The board could not be read; reload it.", http.StatusInternalServerError)
		return
	}
	d.Note = note
	for i, a := range d.Approvals {
		if p, ok := problems[a.TaskID]; ok {
			d.Approvals[i].Problem = p
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

// boardView is what the cards of one render share: the viewer, whether
// they approve, the board's filter (which every card's forms carry back),
// the projects agents work on by repository, the tasks read so far by id,
// the pending approval requests and the tasks they card (planned: by a
// plan's request). boardOf fills it for the whole board, cardFragment for
// one card; problems are what the board says it could not do.
type boardView struct {
	user, csrf      string
	approver        bool
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
	pol, _ := c.inForce(ctx)
	v.approver = slices.Contains(pol.Document.Approvers, s.user)
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

// mayReady: the viewer, an approver, may move t's cards into ready, as
// Start plan and Retry do.
func (c *Console) mayReady(ctx context.Context, v *boardView, t factoryd.Task) bool {
	if !v.approver {
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
// where the board puts it: the script swaps a card whose place changed
// with the whole board.
func (c *Console) boardCard(ctx context.Context, v *boardView, bt factoryd.BoardTask) card {
	t := bt.Task
	k := c.card(t)
	k.Place = fmt.Sprintf("%s %s %s %t %t %d", t.Status, t.Lane, t.Accent, v.carded[t.ID], v.planned[t.ID], bt.EscalationAlert)
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
			why, err := moveRefusal(ctx, t, name, proc, lane, v.user, c.mayMoveCached)
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
		k.Reason, k.EscalationAlert = bt.Escalation, bt.EscalationAlert
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
// checks count, and for a writer of the card's project the summary and
// accent forms with the board's filter. each visits every card once.
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
	setForms := func(k *card) {
		ok, err := c.mayMoveCached(ctx, k.Repository, v.user, policy.MovedByWriters)
		if err != nil && !v.unchecked {
			v.unchecked = true
			log.Printf("console: may %s write %s: %v", v.user, k.Repository, err)
		}
		if k.MayWriteSummary = ok; ok {
			k.CSRF = v.csrf
		}
		k.FilterProject, k.FilterAccent = v.filter.Project, v.filter.Accent
	}
	each(setForms)
}

// boardOf is the board of project ("" every project) and accent ("" every
// accent) as the session's human sees it. When view is "wide", the lanes
// of the shown projects' processes come first (backlog, plan, ready),
// then the state columns. Otherwise (the default), the five columns map
// task states into Needs you, Queued, Working, Verifying, Done.
func (c *Console) boardOf(r *http.Request, project, accent, view string) (boardData, error) {
	ctx := r.Context()
	f := boardFilter{Project: project, Accent: accent}
	v, all, err := c.viewOf(r, f)
	d := boardData{User: v.user, CSRF: v.csrf, boardFilter: f, Query: f.query(), Approver: v.approver, View: view}
	if err != nil {
		return d, err
	}
	for _, p := range all {
		d.Projects = append(d.Projects, p.Name)
	}
	shown := all
	if project != "" {
		i := slices.IndexFunc(all, func(p factoryd.Project) bool { return p.Name == project })
		if i < 0 {
			return d, errNoProject
		}
		shown = all[i : i+1]
	}
	inView := func(repository string) bool { return project == "" || repository == shown[0].Repository }

	// Five columns only when viewing one project and not explicitly wide
	wide := view == "wide" || project == ""
	byLane, byStatus := map[string]int{}, map[string]int{}
	if wide {
		lanes := map[string]bool{}
		for _, p := range shown {
			for _, l := range p.Lanes {
				lanes[l.Name] = true
			}
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
		for i, col := range fiveColumns {
			byStatus[col.Title] = i
			d.Columns = append(d.Columns, column{Title: col.Title})
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
	var working, needYou int
	for _, bt := range tasks {
		t := bt.Task
		// the accent filter narrows the board to one accent; a card with
		// none is in no accent's board, and an accent never changes how a
		// state renders.
		if !inView(t.Repository) || accent != "" && t.Accent != accent {
			continue
		}
		k := c.boardCard(ctx, v, bt)
		if v.humanLane(t) {
			if !factoryd.Final(t.Status) {
				d.HumanTasks = append(d.HumanTasks, k)
			}
			continue
		}

		if wide {
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
		} else {
			colIdx := fiveColumnForStatus(t.Status, t.Author, v.user)
			if colIdx >= 0 && colIdx < len(d.Columns) {
				d.Columns[colIdx].Cards = append(d.Columns[colIdx].Cards, k)
			}
		}

		// Count for status line
		if t.Status == "running" || t.Status == "closing" {
			working++
		}
		if t.Status == "waiting-human" || (t.Status == "waiting-poster" && t.Author == v.user) {
			needYou++
		}

		if t.Status == "waiting-human" && v.planned[t.ID] {
			escalations[t.ID] = bt
		} else if v.escalated(t) {
			d.Escalations = append(d.Escalations, k)
		}
	}
	for _, a := range v.pending {
		if inView(a.Project) {
			k := c.approvalCard(ctx, a, v.byID, v.csrf, d.Approver)
			k.FilterProject, k.FilterAccent = project, accent
			// an escalation from before this revision's request (a cap a
			// writer granted) no longer holds
			if e := escalations[a.TaskID]; a.Kind == factoryd.ApprovalPlan && e.EscalatedAt.After(a.CreatedAt) {
				k.Escalation = e.Escalation
			}
			d.Approvals = append(d.Approvals, k)
		}
	}
	c.finishCards(ctx, v, func(fn func(*card)) {
		for i := range d.Columns {
			for j := range d.Columns[i].Cards {
				fn(&d.Columns[i].Cards[j])
			}
		}
		for i := range d.HumanTasks {
			fn(&d.HumanTasks[i])
		}
		for i := range d.Escalations {
			fn(&d.Escalations[i])
		}
	})

	// Compute status line for the project screen
	if !wide && len(shown) == 1 {
		d.Status = c.computeStatusLine(ctx, shown[0], working, needYou)
	}

	d.Problems = append(d.Problems, v.problems...)
	return d, nil
}

// fiveColumnForStatus maps a task's status into one of the five columns.
// For waiting-poster, it goes to "Needs you" when the viewer is the author,
// otherwise to "Queued".
func fiveColumnForStatus(status, author, viewer string) int {
	switch status {
	case "waiting-human":
		return 0 // Needs you
	case "waiting-poster":
		if author == viewer {
			return 0 // Needs you
		}
		return 1 // Queued
	case "backlog", "queued", "waiting-deps":
		return 1 // Queued
	case "running", "closing":
		return 2 // Working
	case "in-review":
		return 3 // Verifying
	case "done", "failed", "closed":
		return 4 // Done
	}
	return -1
}

// computeStatusLine builds the project screen's quantitative summary.
func (c *Console) computeStatusLine(ctx context.Context, p factoryd.Project, working, needYou int) statusLine {
	sl := statusLine{
		Working:    working,
		NeedYou:    needYou,
		ProjectURL: "/costs",
		NeedYouURL: "#col-0",
	}

	// Determine state word
	if working > 0 {
		sl.State = "working"
	} else if needYou > 0 {
		sl.State = "waiting on you"
	} else {
		sl.State = "idle"
	}

	// Get spend from ledger for this UTC month
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	costs, err := c.cfg.Costs.Costs(ctx, from, now)
	if err != nil {
		log.Printf("console: status line costs: %v", err)
	} else {
		// Sum today's spend for the project
		today := now.Format(policy.DateLayout)
		for _, day := range costs.Days {
			if day.Day == today {
				sl.SpendEUR = day.Total
				break
			}
		}
	}

	// Get month cap from policy or project file
	pol, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	if err != nil || !ok {
		log.Printf("console: status line policy: loaded %v, %v", ok, err)
	} else {
		_, cap := pol.Tree.ProjectCap(p.Repository)
		if !math.IsInf(cap, 1) {
			sl.MonthCap = cap
		} else {
			sl.MonthCap = pol.Document.Budget.MonthEUR
		}
	}

	return sl
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
	c.render(w, "timeline.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf,
		"Card": c.card(t), "Entries": entries, "Gates": gates, "Longest": longest})
}

// labels lists repo's labels as member s, all pages.
func (c *Console) labels(ctx context.Context, s *session, repo factoryd.Repo) (map[string]int64, error) {
	asUser := func(ctx context.Context, method, path string, _ []byte, want int) ([]byte, error) {
		var raw json.RawMessage
		err := c.as(ctx, s, method, path, nil, want, &raw)
		return raw, err
	}
	ls, err := factoryd.GetPages[factoryd.RepoLabel](ctx, asUser,
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
	c.render(w, "new.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf,
		"Projects": ps, "Priorities": offered, "Lanes": lanes})
}

// createTask files the issue as the human (doFileCard).
func (c *Console) createTask(w http.ResponseWriter, r *http.Request) {
	_, issue, err := c.doFileCard(r.Context(), button(r), r.PostFormValue("project"), r.PostFormValue("title"),
		r.PostFormValue("body"), r.PostFormValue("lane"), r.PostFormValue("priority"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/?filed="+strconv.FormatInt(issue, 10), http.StatusSeeOther)
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

// cardFragment answers GET /cards/{id} to a writer of the card's project,
// 404 to anyone else: the one card as the board renders it, its forms
// carrying the filter (?project, ?accent) of the board it is swapped into.
func (c *Console) cardFragment(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	bt, ok, err := c.cfg.Tasks.BoardTask(ctx, id)
	switch {
	case err != nil:
		log.Printf("console: card %s: %v", id, err)
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}
	may, err := c.mayMoveCached(ctx, bt.Repository, current(r).user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", current(r).user, bt.Repository, err)
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return
	case !may:
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	v, _, err := c.viewOf(r, boardFilter{Project: q.Get("project"), Accent: q.Get("accent")})
	if err != nil {
		log.Printf("console: card %s: %v", id, err)
		http.Error(w, "cannot read the card", http.StatusInternalServerError)
		return
	}
	v.byID[bt.ID] = bt.Task
	if bt.Status == "failed" {
		if rt, ok, err := c.cfg.Tasks.Task(ctx, factoryd.RetryID(bt.ID)); err == nil && ok {
			v.byID[rt.ID] = rt
		}
	}
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	c.render(w, "card", k)
}

// drawerData is the card drawer's content for one tab.
type drawerData struct {
	User     string
	CSRF     string
	Card     card
	Tab      string // "overview", "activity", or "files"
	Entries  []timelineEntry
	Gates    []gateRow
	Longest  int
	Files    []prFile
	NoPR     bool
	FilesErr string
}

// prFile is a pull request's changed file from Forgejo's API.
type prFile struct {
	Name      string
	Additions int
	Deletions int
	Status    string
}

// cardDrawer answers GET /cards/{id}/drawer?tab=... for writers of the
// card's project (403 otherwise, 404 for an unknown card), with three tabs:
// Overview, Activity, Files.
func (c *Console) cardDrawer(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "overview"
	}
	if tab != "overview" && tab != "activity" && tab != "files" {
		http.Error(w, "tab must be overview, activity or files", http.StatusBadRequest)
		return
	}

	bt, ok, err := c.cfg.Tasks.BoardTask(ctx, id)
	switch {
	case err != nil:
		log.Printf("console: drawer %s: %v", id, err)
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}

	may, err := c.mayMoveCached(ctx, bt.Repository, current(r).user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", current(r).user, bt.Repository, err)
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return
	case !may:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	v, _, err := c.viewOf(r, boardFilter{Project: q.Get("project"), Accent: q.Get("accent")})
	if err != nil {
		log.Printf("console: drawer %s: %v", id, err)
		http.Error(w, "cannot read the card", http.StatusInternalServerError)
		return
	}
	v.byID[bt.ID] = bt.Task

	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })

	d := drawerData{
		User: current(r).user,
		CSRF: current(r).csrf,
		Card: k,
		Tab:  tab,
	}

	switch tab {
	case "overview":
		// The card already has all the expanded content
	case "activity":
		steps, err := c.readTimeline(ctx, bt.ID)
		if err != nil {
			log.Printf("console: drawer %s timeline: %v", id, err)
		} else {
			for _, m := range steps {
				d.Entries = append(d.Entries, entry(m))
			}
			d.Gates, d.Longest = gateRows(steps, bt.Task, time.Now())
		}
	case "files":
		if bt.PRNumber == 0 {
			d.NoPR = true
		} else {
			files, err := c.prFiles(r, bt.Repository, bt.PRNumber)
			if err != nil {
				log.Printf("console: drawer %s files: %v", id, err)
				d.FilesErr = "could not read the pull request's files"
			} else {
				d.Files = files
			}
		}
	}

	c.render(w, "drawer.html", d)
}

// prFiles reads the pull request's changed files from Forgejo's API.
func (c *Console) prFiles(r *http.Request, repository string, pr int64) ([]prFile, error) {
	repo, err := factoryd.SplitRepo(repository)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/files", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), pr)
	var raw []map[string]any
	if err := c.asUser(r, http.MethodGet, path, nil, http.StatusOK, &raw); err != nil {
		return nil, err
	}
	var out []prFile
	for _, f := range raw {
		name, _ := f["filename"].(string)
		additions, _ := f["additions"].(float64)
		deletions, _ := f["deletions"].(float64)
		status, _ := f["status"].(string)
		out = append(out, prFile{
			Name:      name,
			Additions: int(additions),
			Deletions: int(deletions),
			Status:    status,
		})
	}
	return out, nil
}
