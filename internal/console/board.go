package console

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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
)

var funcs = template.FuncMap{
	"eur4": func(v float64) string { return fmt.Sprintf("%.4f", v) },
	// lease renders minutes as whole hours when they are
	"lease": func(minutes int) string {
		if minutes%60 == 0 {
			return fmt.Sprintf("%d h", minutes/60)
		}
		return fmt.Sprintf("%d min", minutes)
	},
	"utc":         func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
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

// columns are the agent lane, in board order; closing counts as running.
var columns = []struct{ Title, Status string }{
	{"Suggested", "suggested"}, {"Queued", "queued"}, {"Running", "running"}, {"Waiting on a human", "waiting-human"},
	{"Waiting on the author", "waiting-poster"}, {"In review", "in-review"}, {"Done", "done"}, {"Failed", "failed"},
	{"Closed", "closed"},
}

type card struct {
	factoryd.Task
	IssueURL string
	PRURL    string
	Reason   string // an escalation's reason
	// Total is the task's time so far, or in all once it finished; GateAge
	// how long it has been in its gate.
	Total, GateAge  string
	EscalationAlert int64 // the escalation's open alert
}

type column struct {
	Title string
	Cards []card
}

type boardData struct {
	User        string
	CSRF        string
	Filed       string
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
		k.GateAge = minutes(time.Since(*t.GateSince))
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

// board renders the whole page, or only the board for htmx's poll.
func (c *Console) board(w http.ResponseWriter, r *http.Request) {
	s := current(r)
	tasks, err := c.cfg.Tasks.BoardTasks(r.Context(), time.Now().Add(-finishedSince), boardLimit)
	if err != nil {
		log.Printf("console: board tasks: %v", err)
		http.Error(w, "cannot read tasks", http.StatusInternalServerError)
		return
	}
	d := boardData{User: s.user, CSRF: s.csrf, Filed: r.URL.Query().Get("filed"), Approver: c.approver(r)}
	if len(tasks) == boardLimit {
		d.Problems = append(d.Problems, fmt.Sprintf("the board shows the first %d tasks", boardLimit))
	}
	byStatus := map[string]int{}
	for i, col := range columns {
		d.Columns = append(d.Columns, column{Title: col.Title})
		byStatus[col.Status] = i
	}
	byStatus["closing"] = byStatus["running"]
	byID := map[string]factoryd.Task{}
	for _, bt := range tasks {
		t := bt.Task
		byID[t.ID] = t
		k := c.card(t)
		if t.Kind == "human" {
			if !factoryd.Final(t.Status) {
				d.HumanTasks = append(d.HumanTasks, k)
			}
			continue
		}
		if i, ok := byStatus[t.Status]; ok {
			d.Columns[i].Cards = append(d.Columns[i].Cards, k)
		}
		if t.Status == "waiting-human" {
			k.Reason, k.EscalationAlert = bt.Escalation, bt.EscalationAlert
			d.Escalations = append(d.Escalations, k)
		}
	}
	pending, err := c.cfg.Approvals.ApprovalRequests(r.Context(), "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		d.Problems = append(d.Problems, "approval requests could not be read")
	}
	for _, a := range pending {
		d.Approvals = append(d.Approvals, c.approvalCard(a, byID, s.csrf, d.Approver))
	}
	name := "page.html"
	if r.URL.Path == "/board" {
		name = "board"
	}
	c.render(w, name, d)
}

type timelineEntry struct {
	Time       string
	Step       string
	Detail     string
	Reason     string
	Supervisor supervised // the session or the gate supervisor's decision
}

// readTimeline reads the task's timeline steps, oldest first.
func (c *Console) readTimeline(r *http.Request, taskID string) ([]map[string]any, error) {
	body, err := c.cfg.Timelines.Get(r.Context(), "sessions/"+taskID+"/timeline.jsonl")
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
	steps, err := c.readTimeline(r, t.ID)
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

// labels lists the repository's labels as the human, all pages.
func (c *Console) labels(r *http.Request) (map[string]int64, error) {
	asUser := func(_ context.Context, method, path string, _ []byte, want int) ([]byte, error) {
		var raw json.RawMessage
		err := c.asUser(r, method, path, nil, want, &raw)
		return raw, err
	}
	ls, err := factoryd.GetPages[factoryd.RepoLabel](r.Context(), asUser,
		fmt.Sprintf("/api/v1/repos/%s/%s/labels?", url.PathEscape(c.cfg.Repo.Owner), url.PathEscape(c.cfg.Repo.Name)))
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

func (c *Console) newTaskForm(w http.ResponseWriter, r *http.Request) {
	labels, err := c.labels(r)
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
	c.render(w, "new.html", map[string]any{"User": current(r).user, "CSRF": current(r).csrf,
		"Repo": c.cfg.Repo.String(), "Priorities": offered})
}

// createTask files the issue as the human, labelled with the lane (and the
// priority when chosen); the webhook or the heal pass makes it a task.
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
	case lane != "agent" && lane != "human":
		http.Error(w, "lane must be agent or human", http.StatusBadRequest)
		return
	case priority != "" && !slices.Contains(priorities, priority):
		http.Error(w, "unknown priority", http.StatusBadRequest)
		return
	}
	labels, err := c.labels(r)
	if err != nil {
		c.forgejoFailed(w, r, "read the labels", err)
		return
	}
	var ids []int64
	for _, name := range []string{lane, priority} {
		if name == "" {
			continue
		}
		id, ok := labels[name]
		if !ok {
			http.Error(w, "label "+name+" does not exist on "+c.cfg.Repo.String(), http.StatusConflict)
			return
		}
		ids = append(ids, id)
	}
	var issue struct {
		Number int64 `json:"number"`
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues", url.PathEscape(c.cfg.Repo.Owner), url.PathEscape(c.cfg.Repo.Name))
	if err := c.asUser(r, http.MethodPost, path, map[string]any{"title": title, "body": body, "labels": ids},
		http.StatusCreated, &issue); err != nil {
		c.forgejoFailed(w, r, "file the issue", err)
		return
	}
	log.Printf("console: %s filed %s#%d (%s)", current(r).user, c.cfg.Repo, issue.Number, lane)
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
	if err := c.asUser(r, http.MethodPost, path, map[string]string{"body": text}, http.StatusCreated, nil); err != nil {
		c.forgejoFailed(w, r, "post the answer", err)
		return
	}
	log.Printf("console: %s answered the escalation of %s", current(r).user, t.ID)
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
