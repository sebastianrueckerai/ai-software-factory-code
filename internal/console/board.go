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
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// eur2 is money to the cent, as a card's face reads spend against budget.
var funcs = template.FuncMap{
	"eur4": func(v float64) string { return fmt.Sprintf("%.4f", v) },
	"eur2": func(v float64) string { return fmt.Sprintf("\u20ac%.2f", v) },
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

// stateColumns follow the lane columns, in board order; closing counts as
// running.
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
	// Checks is "n/n" from the task's newest checks step, "" where the
	// card has no timeline with one.
	Checks string
	// NearBudget is spend at 80 percent of the budget or more.
	NearBudget bool
	// Stale is a card without an event for the stale window: an amber
	// ring, and these words on its activity line.
	Stale     bool
	StaleText string
	// Children roll up an accepted plan: n running, n done, n waiting.
	Children string
	// Blocked is how many cards this one waits on; BlockedOn names their
	// issues in its title attribute ("#3, #5").
	Blocked   int
	BlockedOn string
	// Accent is the card's colour label (one of factoryd.Accents), ""
	// when none: no chip, no edge. It never changes a state rendering.
	Accent string
	// MayWriteSummary is whether the viewer writes the task's project, so
	// the expanded view carries its forms with a CSRF token for them;
	// CSRF is that token (an escalation copy keeps it for its answer
	// form, which reads the page's own token).
	MayWriteSummary bool
	CSRF            string
	// FilterProject and FilterAccent are the board's filter the card's
	// forms carry back so an action keeps the view; Accents are the
	// choices its accent select offers.
	boardFilter
	FilterProject, FilterAccent string
	Accents                     []string
}

// titleClause is the face's title from the objective: up to the first
// full stop that ends a sentence — one followed by a space, or one at the
// very end — or 80 characters when there is none. A dot inside a file name
// or a version ("internal/console/board.go", "v1.2") ends no sentence, so
// the clause keeps it.
func titleClause(objective string) string {
	s := strings.TrimSpace(objective)
	if i := strings.Index(s, ". "); i >= 0 {
		s = strings.TrimSpace(s[:i])
	} else if strings.HasSuffix(s, ".") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "."))
	}
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

// statusLabels are the pill's display text per status.
var statusLabels = map[string]string{
	"running": "running", "closing": "closing", "queued": "queued",
	"waiting-human": "waiting", "waiting-poster": "waiting",
	"waiting-deps": "waiting", "in-review": "in review",
	"done": "done", "failed": "failed", "closed": "closed",
	"backlog": "backlog", "plan": "plan",
}

// StatusLabel is the card's status as display text for the pill.
func (k card) StatusLabel() string { return cmp.Or(statusLabels[k.Status], k.Status) }

// nearBudgetShare is the share of its budget from which a card's spend is
// shown prominent with the warning sign and the words "near its budget".
const nearBudgetShare = 0.8

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
		var passed int
		for _, j := range jobs {
			job, _ := j.(map[string]any)
			if st, _ := job["status"].(string); st == "success" {
				passed++
			}
		}
		if len(jobs) == 0 {
			return ""
		}
		return fmt.Sprintf("%d/%d", passed, len(jobs))
	}
	return ""
}

// faceTimelines reads the newest checks step onto every card that has a
// session to have one: each agent card not yet final, at most once per
// render. A timeline that cannot be read keeps the card without the chip —
// the object store answers no body for a task that never wrote a timeline,
// and reading is best-effort here, never a reason to fail the board.
func (c *Console) faceTimelines(ctx context.Context, everyCard func(func(*card))) {
	read := map[string]string{}
	set := func(k *card) {
		if k.Kind != "agent" || factoryd.Final(k.Status) {
			return
		}
		if _, ok := read[k.ID]; !ok {
			steps, err := c.readTimeline(ctx, k.ID)
			if err != nil {
				log.Printf("console: timeline of %s for its checks: %v", k.ID, err)
				read[k.ID] = ""
			} else {
				read[k.ID] = newestChecksCount(steps)
			}
		}
		k.Checks = read[k.ID]
	}
	everyCard(set)
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
// rides on the form in the template, read from the page's data.
type move struct {
	Lane, Title string
	CSRF        string
}

// retry is a failed plan card's Retry button.
type retry struct {
	CSRF string
}

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

type boardData struct {
	User  string
	CSRF  string
	Filed string
	Note  string // what the human's last action did
	// Project and Accent are the board's filter, which every form on it
	// carries back so an action keeps the view; Accents are the choices
	// its select offers.
	boardFilter
	Query       string // the filter as a URL query for the poll's hx-get
	Accents     []string
	Projects    []string
	Columns     []column
	HumanTasks  []card
	Escalations []card
	Approvals   []approvalCard
	Problems    []string
	Approver    bool // may authorize spend (ruling 128)
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
	k.Accent = t.Accent
	k.Title = cmp.Or(t.HumanSummary, func() string {
		if t.PlanKey != "" {
			return t.PlanKey
		}
		return titleClause(t.Objective)
	}())
	if t.BudgetEUR > 0 {
		k.NearBudget = t.SpentEUR >= nearBudgetShare*t.BudgetEUR
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
	if accent != "" && !factoryd.IsAccent(accent) {
		http.Error(w, "accent must be one of "+strings.Join(factoryd.Accents, ", "), http.StatusBadRequest)
		return
	}
	d, err := c.boardOf(r, project, accent)
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
	project, accent := r.PostFormValue("project"), r.PostFormValue("accent")
	if r.Header.Get("HX-Request") == "" && status == http.StatusOK {
		back := "/"
		q := url.Values{}
		if project != "" {
			q.Set("project", project)
		}
		if accent != "" {
			q.Set("accent", accent)
		}
		if q.Encode() != "" {
			back += "?" + q.Encode()
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	d, err := c.boardOf(r, project, accent)
	if errors.Is(err, errNoProject) {
		d, err = c.boardOf(r, "", accent)
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

// boardOf is the board of project ("" every project) and accent ("" every
// accent) as the session's human sees it: the lanes of the shown projects'
// processes in the fixed order backlog, plan, ready, then the state
// columns.
func (c *Console) boardOf(r *http.Request, project, accent string) (boardData, error) {
	ctx, s := r.Context(), current(r)
	f := boardFilter{Project: project, Accent: accent}
	d := boardData{User: s.user, CSRF: s.csrf, boardFilter: f, Query: f.query(), Accents: factoryd.Accents}
	v, _ := c.inForce(r)
	d.Approver = slices.Contains(v.Document.Approvers, s.user)
	all, err := c.coderProjects(r)
	if err != nil {
		return d, fmt.Errorf("projects: %w", err)
	}
	byRepo := map[string]factoryd.Project{}
	for _, p := range all {
		d.Projects = append(d.Projects, p.Name)
		byRepo[p.Repository] = p
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
	lanes := map[string]bool{}
	for _, p := range shown {
		for _, l := range p.Lanes {
			lanes[l.Name] = true
		}
	}
	byLane, byStatus := map[string]int{}, map[string]int{}
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

	tasks, err := c.cfg.Tasks.BoardTasks(ctx, time.Now().Add(-finishedSince), boardLimit)
	if err != nil {
		return d, fmt.Errorf("tasks: %w", err)
	}
	if len(tasks) == boardLimit {
		d.Problems = append(d.Problems, fmt.Sprintf("the board shows the first %d tasks", boardLimit))
	}
	pending, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		d.Problems = append(d.Problems, "approval requests could not be read")
	}
	carded, planned := map[string]bool{}, map[string]bool{}
	for _, a := range pending {
		carded[a.TaskID] = true
		planned[a.TaskID] = planned[a.TaskID] || a.Kind == factoryd.ApprovalPlan
	}
	escalations := map[string]factoryd.BoardTask{}
	byID := map[string]factoryd.Task{}
	for _, bt := range tasks {
		byID[bt.ID] = bt.Task
	}
	unchecked := false
	// mayReady: the viewer, an approver, may move t's cards into ready,
	// as Start plan and Retry do
	mayReady := func(t factoryd.Task) bool {
		if !d.Approver {
			return false
		}
		_, proc := processOf(byRepo, t)
		ready, _ := proc.Lane(policy.LaneReady)
		ok, err := c.mayMoveCached(ctx, t.Repository, s.user, ready.MovedBy)
		if err != nil {
			log.Printf("console: may %s move %s's cards into ready: %v", s.user, t.Repository, err)
		}
		return ok
	}
	for _, bt := range tasks {
		t := bt.Task
		if !inView(t.Repository) {
			continue
		}
		// the accent filter narrows the board to one accent; a card with
		// none is in no accent's board, and an accent never changes how a
		// state renders.
		if accent != "" && t.Accent != accent {
			continue
		}
		k := c.card(t)
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
		// a roll task has no agent: the human lane until its card (ruling 178)
		if t.Kind == "human" || t.Source == factoryd.SourceRoll && !factoryd.Final(t.Status) && !carded[t.ID] {
			if !factoryd.Final(t.Status) {
				d.HumanTasks = append(d.HumanTasks, k)
			}
			continue
		}
		if t.Source == factoryd.SourcePlan && t.Parent != "" {
			k.PlanIssue = c.planIssue(ctx, byID, t.Parent)
		}
		i, ok := byStatus[t.Status]
		switch t.Status {
		case "backlog":
			i, ok = byLane[policy.LaneBacklog]
			name, proc := processOf(byRepo, t)
			for _, lane := range policy.LaneNames {
				why, err := moveRefusal(ctx, t, name, proc, lane, s.user, c.mayMoveCached)
				if err != nil && !unchecked {
					unchecked = true
					log.Printf("console: may %s move %s into %s: %v", s.user, t.ID, lane, err)
					d.Problems = append(d.Problems, "who may move a card could not be checked: the move buttons are hidden")
				}
				if err == nil && why == "" {
					k.Moves = append(k.Moves, move{Lane: lane, Title: laneTitles[lane], CSRF: s.csrf})
				}
			}
		case "queued", "waiting-deps":
			if i, ok = byLane[t.Lane]; !ok {
				i, ok = byLane[policy.LaneReady]
			}
		default:
			if t.Lane == policy.LanePlan || t.Lane == policy.LaneReady {
				k.Badge = t.Lane
			}
		}
		if t.Lane == policy.LanePlan && t.Status == "done" {
			if k.Plan, err = c.planCards(ctx, t, mayReady(t), s.csrf); err != nil {
				log.Printf("console: the cards of plan %s: %v", t.ID, err)
				d.Problems = append(d.Problems, "the cards of plan "+t.ID+" could not be read")
			} else {
				k.Children = planRollup(k.Plan.Cards)
			}
		}
		// a retry that stopped before its replacement got an issue is finished by the next
		replacement, retried := byID[factoryd.RetryID(t.ID)]
		if t.Status == "failed" && t.Source == factoryd.SourcePlan && t.Parent != "" && (t.PlanKey != "" || retried && replacement.IssueNumber == 0) &&
			mayReady(t) {
			k.Retry = &retry{CSRF: s.csrf}
		}
		if ok {
			d.Columns[i].Cards = append(d.Columns[i].Cards, k)
		}
		// a waiting plan's card is its escalation: Request changes answers it
		if t.Status == "waiting-human" && planned[t.ID] {
			escalations[t.ID] = bt
		} else if t.Status == "waiting-human" {
			k.Reason, k.EscalationAlert = bt.Escalation, bt.EscalationAlert
			d.Escalations = append(d.Escalations, k)
		}
	}
	for _, a := range pending {
		if inView(a.Project) {
			k := c.approvalCard(ctx, a, byID, s.csrf, d.Approver)
			k.Filter = project
			// an escalation from before this revision's request (a cap a
			// writer granted) no longer holds
			if e := escalations[a.TaskID]; a.Kind == factoryd.ApprovalPlan && e.EscalatedAt.After(a.CreatedAt) {
				k.Escalation = e.Escalation
			}
			d.Approvals = append(d.Approvals, k)
		}
	}
	var ids []string
	collect := func(ks []card) {
		for _, k := range ks {
			ids = append(ids, k.ID)
		}
	}
	for _, col := range d.Columns {
		collect(col.Cards)
	}
	collect(d.HumanTasks)
	collect(d.Escalations)
	// everyCard is each card of the board once, so a face's checks count,
	// its staleness ring and the summary editor can be filled per card.
	everyCard := func(fn func(*card)) {
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
	}
	newest := map[string]factoryd.Event{}
	if len(ids) > 0 && c.cfg.Events != nil {
		got, err := c.cfg.Events.NewestTaskEvents(ctx, ids)
		if err != nil {
			log.Printf("console: newest events: %v", err)
		} else {
			newest = got
		}
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
			k.Activity = fmt.Sprintf("%s by %s via %s at %s", e.Kind, e.Actor, e.Via, e.At.UTC().Format("15:04"))
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
			k.StaleText = ""
		}
	}
	everyCard(setActivity)
	c.faceTimelines(ctx, everyCard)
	// mayWrite is whether the viewer writes that repository, asked at most
	// once per repository per render through the move cache.
	mayWrite := func(repository string) bool {
		ok, err := c.mayMoveCached(ctx, repository, s.user, policy.MovedByWriters)
		if err != nil {
			log.Printf("console: may %s write %s: %v", s.user, repository, err)
			return false
		}
		return ok
	}
	writes := map[string]bool{}
	mayWriteOnce := func(repository string) bool {
		if _, ok := writes[repository]; !ok {
			writes[repository] = mayWrite(repository)
		}
		return writes[repository]
	}
	setForms := func(k *card) {
		k.MayWriteSummary = mayWriteOnce(k.Repository)
		if k.MayWriteSummary {
			k.CSRF = s.csrf
		}
		k.boardFilter, k.Accents = f, d.Accents
		k.FilterProject, k.FilterAccent = f.Project, f.Accent
		if k.Plan != nil {
			for i := range k.Plan.Cards {
				k.Plan.Cards[i].boardFilter = f
			}
		}
	}
	everyCard(setForms)
	return d, nil
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

// labels lists repo's labels as the human, all pages.
func (c *Console) labels(r *http.Request, repo factoryd.Repo) (map[string]int64, error) {
	asUser := func(_ context.Context, method, path string, _ []byte, want int) ([]byte, error) {
		var raw json.RawMessage
		err := c.asUser(r, method, path, nil, want, &raw)
		return raw, err
	}
	ls, err := factoryd.GetPages[factoryd.RepoLabel](r.Context(), asUser,
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
func (c *Console) coderProjects(r *http.Request) ([]factoryd.Project, error) {
	ps, err := c.cfg.Projects.Projects(r.Context())
	return slices.DeleteFunc(ps, func(p factoryd.Project) bool { return !p.Coder() }), err
}

func (c *Console) newTaskForm(w http.ResponseWriter, r *http.Request) {
	ps, err := c.coderProjects(r)
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	if len(ps) == 0 {
		http.Error(w, "no project takes agent tasks", http.StatusServiceUnavailable)
		return
	}
	labels, err := c.labels(r, ps[0].Repo())
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

// createTask files the issue as the human, labelled with the lane's label
// (backlog, plan or agent, or human for the human lane) and the priority
// when chosen; the webhook or the heal pass makes it a task.
func (c *Console) createTask(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.PostFormValue("title"))
	body := r.PostFormValue("body")
	lane := r.PostFormValue("lane")
	priority := r.PostFormValue("priority")
	switch {
	case title == "" || utf8.RuneCountInString(title) > 255:
		http.Error(w, "a title of 1 to 255 characters is required", http.StatusBadRequest)
		return
	case len(body) > 64<<10:
		http.Error(w, "the description is longer than 64 KiB", http.StatusBadRequest)
		return
	case laneLabels[lane] == "" && lane != factoryd.LaneHuman:
		http.Error(w, "lane must be backlog, plan, ready or human", http.StatusBadRequest)
		return
	case priority != "" && !slices.Contains(priorities, priority):
		http.Error(w, "unknown priority", http.StatusBadRequest)
		return
	}
	ps, err := c.coderProjects(r)
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	i := slices.IndexFunc(ps, func(p factoryd.Project) bool { return p.Name == r.PostFormValue("project") })
	if i < 0 {
		http.Error(w, "choose a project agents work on", http.StatusBadRequest)
		return
	}
	label := factoryd.LaneHuman
	if lane != factoryd.LaneHuman {
		if !slices.ContainsFunc(ps[i].Lanes, func(l policy.Lane) bool { return l.Name == lane }) {
			http.Error(w, "project "+ps[i].Name+" has no lane "+lane, http.StatusBadRequest)
			return
		}
		label = laneLabels[lane]
	}
	repo := ps[i].Repo()
	labels, err := c.labels(r, repo)
	if err != nil {
		c.forgejoFailed(w, r, "read the labels", err)
		return
	}
	var ids []int64
	for _, name := range []string{label, priority} {
		if name == "" {
			continue
		}
		id, ok := labels[name]
		if !ok {
			http.Error(w, "label "+name+" does not exist on "+repo.String(), http.StatusConflict)
			return
		}
		ids = append(ids, id)
	}
	var issue struct {
		Number int64 `json:"number"`
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	if err := c.asUser(r, http.MethodPost, path, map[string]any{"title": title, "body": body, "labels": ids},
		http.StatusCreated, &issue); err != nil {
		c.forgejoFailed(w, r, "file the issue", err)
		return
	}
	log.Printf("console: %s filed %s#%d (%s)", current(r).user, repo, issue.Number, lane)
	c.act(r, repo.String(), "", issue.Number, factoryd.EventCardFiled, map[string]any{"lane": lane, "title": title})
	http.Redirect(w, r, "/?filed="+strconv.FormatInt(issue.Number, 10), http.StatusSeeOther)
}

// answer posts the human's answer to an escalation as an issue comment
// under their own name; intake's human_comment path resumes the task.
func (c *Console) answer(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.PostFormValue("answer"))
	if text == "" || len(text) > 64<<10 {
		http.Error(w, "an answer of up to 64 KiB is required", http.StatusBadRequest)
		return
	}
	t, ok, err := c.cfg.Tasks.Task(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if t.Kind != "agent" || t.Status != "waiting-human" || t.IssueNumber == 0 {
		http.Error(w, "task "+t.ID+" is not waiting for an answer", http.StatusConflict)
		return
	}
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		log.Printf("console: task %s: %v", t.ID, err)
		http.Error(w, "the task names no repository", http.StatusInternalServerError)
		return
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), t.IssueNumber)
	var posted struct {
		ID int64 `json:"id"`
	}
	if err := c.asUser(r, http.MethodPost, path, map[string]string{"body": text}, http.StatusCreated, &posted); err != nil {
		c.forgejoFailed(w, r, "post the answer", err)
		return
	}
	log.Printf("console: %s answered the escalation of %s", current(r).user, t.ID)
	// One action, one row: the event names the comment this button posted,
	// so intake records no second question_answered when its webhook
	// delivers that very comment.
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventQuestionAnswered, commentDetail(posted.ID))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (c *Console) forgejoFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	log.Printf("console: %s could not %s: %v", current(r).user, what, err)
	if c.session(r) == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Error(w, "Forgejo refused to "+what+"; see the factoryd log", http.StatusBadGateway)
}
