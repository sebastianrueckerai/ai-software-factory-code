package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/manual"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// toolInput holds every tool's arguments; each tool reads its own.
type toolInput struct {
	TaskID     string   `json:"task_id,omitempty"`
	Title      string   `json:"title,omitempty"`
	Body       string   `json:"body,omitempty"`
	Lane       string   `json:"lane,omitempty"`
	Text       string   `json:"text,omitempty"`
	Question   int      `json:"question,omitempty"`
	Note       string   `json:"note,omitempty"`
	Minutes    int      `json:"minutes,omitempty"`
	Action     string   `json:"action,omitempty"`
	Revision   int      `json:"revision,omitempty"`
	SHA256     string   `json:"sha256,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	Accent     string   `json:"accent,omitempty"`
	Login      string   `json:"login,omitempty"`
	N          int      `json:"n,omitempty"`
	Thread     int64    `json:"thread,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Path       string   `json:"path,omitempty"`
	SHA        string   `json:"sha,omitempty"`
	WithDiff   bool     `json:"with_diff,omitempty"`
	Remote     string   `json:"remote,omitempty"`
	From       int      `json:"from,omitempty"`
	To         int      `json:"to,omitempty"`
	Query      string   `json:"query,omitempty"`
	Size       string   `json:"size,omitempty"`
	Rung       string   `json:"rung,omitempty"`
	RungReason string   `json:"rung_reason,omitempty"`
	Acceptance []string `json:"acceptance,omitempty"`
	By         string   `json:"by,omitempty"`
	FromDate   string   `json:"from_date,omitempty"`
	ToDate     string   `json:"to_date,omitempty"`
	State      string   `json:"state,omitempty"`
	ID         int64    `json:"id,omitempty"`
	Verdict    string   `json:"verdict,omitempty"`
	DependsOn  string   `json:"depends_on,omitempty"`
	Remove     bool     `json:"remove,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Autonomy   string   `json:"autonomy,omitempty"`
	Content    string   `json:"content,omitempty"`
	BacklogCap int      `json:"backlog_cap,omitempty"`
	// Old and New are a document edit's passage and its replacement,
	// BaseSHA256 the hash the document was read with; Find narrows a
	// document read to the lines that hold it, Regex makes Find a pattern,
	// From and To read a line range.
	// Ask and Choices are an escalation's question and answers, and the
	// ask tool's question too; Why and Info the question format's why and
	// what-would-change-your-mind, Recommendation its suggested answer,
	// all the writer's own.
	Ask            string   `json:"ask,omitempty"`
	Why            string   `json:"why,omitempty"`
	Info           string   `json:"info,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Choices        []string `json:"choices,omitempty"`
	// Found is the analysis the attach_analysis tool records inside an
	// ask.
	Found      string `json:"found,omitempty"`
	Old        string `json:"old,omitempty"`
	New        string `json:"new,omitempty"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
	Find       string `json:"find,omitempty"`
	Regex      bool   `json:"regex,omitempty"`
	// Answers are every open question of a plan revision, answered at
	// once by the answer tool.
	Answers []PlanAnswerInput `json:"answers,omitempty"`
	// Name names a routine; the routine tools' other arguments are its
	// keys, BriefText its brief's text and SkipUndo a Skip once's undo.
	Name      string           `json:"name,omitempty"`
	Schedule  []policy.Trigger `json:"schedule,omitempty"`
	Files     string           `json:"files,omitempty"`
	Skill     string           `json:"skill,omitempty"`
	BriefText string           `json:"brief_text,omitempty"`
	WhileOpen string           `json:"while_open,omitempty"`
	Missed    string           `json:"missed,omitempty"`
	Tell      string           `json:"tell,omitempty"`
	SkipUndo  bool             `json:"undo,omitempty"`
	// The view tools' arguments (282), one per control of the screen
	// board_view works; Find is the find's words, a call that names it
	// with empty text clearing them.
	View         string `json:"view,omitempty"`
	Queued       string `json:"queued,omitempty"`
	QueuedOnly   string `json:"queued_only,omitempty"`
	Progress     string `json:"progress,omitempty"`
	ProgressOnly string `json:"progress_only,omitempty"`
	Done         string `json:"done,omitempty"`
	DoneOnly     string `json:"done_only,omitempty"`
	Routines     string `json:"routines,omitempty"`
	Within       int    `json:"within,omitempty"`
	Unit         string `json:"unit,omitempty"`
	Card         string `json:"card,omitempty"`
	Tab          string `json:"tab,omitempty"`
	Theme        string `json:"theme,omitempty"`
	// undo marks an undo's run, which only chatUndo sets: unexported, no
	// arguments carry it
	undo bool
}

// PlanAnswerInput is one answer of the answer tool's answers list: the
// question's number on the plan card and its text.
type PlanAnswerInput struct {
	Question int    `json:"question"`
	Text     string `json:"text"`
}

// setNote is a set tool's result on issue: field is set, or on an undo
// back to v.
func (in toolInput) setNote(field string, issue int64, v string) string {
	if in.undo {
		return fmt.Sprintf("The %s of #%d is back to %q.", field, issue, v)
	}
	return fmt.Sprintf("The %s of #%d is set.", field, issue)
}

// inverse is an applied action's undo: the tool of tools, internal ones
// included, that undoes it, with its arguments.
type inverse struct {
	tool string
	args toolInput
}

// chatTool is one of the project agent's tools: a read, or a console
// action (write) it runs through the action's own function as the member.
// Its about is one short sentence, what it reads or does; the system read
// carries the depth.
type chatTool struct {
	name, title, about string
	params             map[string]any // the arguments' JSON Schema properties
	required           []string
	write              bool
	priced             bool // one model call over the conversation's own attachment: its result stays in replay, re-running re-spends
	internal           bool // an undo's inverse only: the agent is not offered it
	run                func(t *turn, in toolInput) (string, *inverse, error)
}

// def is the tool's schema, its sentence marked (needs OK) when the
// chat's confirm names it.
func (tool chatTool) def(confirm bool) factoryd.OperatorTool {
	params := map[string]any{"type": "object", "properties": tool.params}
	if len(tool.required) > 0 {
		params["required"] = tool.required
	}
	about := tool.about
	if confirm {
		about = strings.TrimSuffix(about, ".") + " (needs OK)."
	}
	return factoryd.OperatorTool{Name: tool.name, Description: about, Parameters: params}
}

func str(about string) map[string]any { return map[string]any{"type": "string", "description": about} }

func stepParam(about string) map[string]any {
	return map[string]any{"type": "integer", "description": about, "minimum": 1}
}

func enum(about string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": about, "enum": values}
}

// fallback is parameter p with its default v, which a call that leaves p
// out or empty takes.
func fallback(v any, p map[string]any) map[string]any {
	p["default"] = v
	return p
}

var (
	taskParam   = str("the card's task id or issue number")
	routineName = str("the routine's name: lowercase letters, digits and dashes")
	nParam      = fallback(20, map[string]any{"type": "integer", "description": "how many", "minimum": 1, "maximum": 100})
	textParam   = str("the text")
	sessionN    = stepParam("the session's number")
	docPath     = str("the file's path under docs/")
)

// chatTools are the tools the agent is offered, in policy.ChatTools'
// order: steer only where factoryd can reach a session, github_export
// where it keeps exports, github_sync where it syncs them, the routine
// tools where it runs routines.
func (c *Console) chatTools() []chatTool {
	return slices.DeleteFunc(slices.Clone(tools), func(t chatTool) bool {
		return t.internal || t.name == "steer" && c.cfg.Steer == nil || t.name == "github_export" && c.cfg.Mirrors == nil ||
			t.name == "github_sync" && c.cfg.GitHubSync == nil || strings.HasPrefix(t.name, "routine") && c.cfg.Routines == nil
	})
}

// offered are the schemas of the chat's tools the turn's member holds the
// access table's roles for: the model is offered no tool the dispatch
// would refuse them.
func (t *turn) offered() []factoryd.OperatorTool {
	var defs []factoryd.OperatorTool
	for _, tool := range t.c.chatTools() {
		if t.offers(tool.name) {
			defs = append(defs, tool.def(slices.Contains(t.cfg.Confirm, tool.name)))
		}
	}
	return defs
}

func (c *Console) chatTool(name string) (chatTool, bool) {
	i := slices.IndexFunc(c.chatTools(), func(t chatTool) bool { return t.name == name })
	if i < 0 {
		return chatTool{}, false
	}
	return c.chatTools()[i], true
}

// isRead is true for the name of a read tool: its result is the project
// as it was at the call.
func isRead(name string) bool {
	i := slices.IndexFunc(tools, func(t chatTool) bool { return t.name == name })
	return i >= 0 && !tools[i].write
}

// isPriced is true for the name of a tool whose every call spends a
// model look over the conversation's own attachment: its answer never
// goes stale, and running it again re-spends.
func isPriced(name string) bool {
	i := slices.IndexFunc(tools, func(t chatTool) bool { return t.name == name })
	return i >= 0 && tools[i].priced
}

// card is the tool's card, refused unless it is the chat project's.
func (t *turn) card(in toolInput) (factoryd.Task, error) {
	return t.c.cardIn(t.r.Context(), t.a, in.TaskID)
}

// normalized is a call's arguments as the tools read them: a number given
// for a text parameter, an issue number say, as its text, and each
// parameter with a default that the call leaves out or empty set to it.
func (tool chatTool) normalized(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(cmp.Or(args, "{}")), &m) != nil || m == nil {
		return args
	}
	changed := false
	for k, p := range tool.params {
		p, _ := p.(map[string]any)
		v, set := m[k]
		if n, ok := v.(float64); ok && p["type"] == "string" {
			m[k], changed = strconv.FormatFloat(n, 'f', -1, 64), true
		}
		if d, ok := p["default"]; ok && (!set || v == nil || v == "") {
			m[k], changed = d, true
		}
	}
	if !changed {
		return args
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}

// byTaskID names in's cards by their task ids: a card named by its issue
// number, "#5" or 5, is the chat project's card of that issue.
func (t *turn) byTaskID(in *toolInput) error {
	for _, id := range []*string{&in.TaskID, &in.DependsOn, &in.Card} {
		n, err := strconv.ParseInt(strings.TrimPrefix(*id, "#"), 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		k, ok, err := t.c.cfg.Tasks.TaskByIssue(t.r.Context(), t.p.Repository, n)
		switch {
		case err != nil:
			log.Printf("console: card #%d of %s: %v", n, t.p.Name, err)
			return refuse(http.StatusInternalServerError, "cannot read the task")
		case !ok:
			return refuse(http.StatusNotFound, fmt.Sprintf("Project %s has no card #%d: the board read lists its cards.", t.p.Name, n))
		}
		*id = k.ID
	}
	return nil
}

// describe is one line saying what a proposal of tool with in would do,
// in pinned to the plan revision an accept_plan names, an approve_merge's
// with its warning; a card of another project refuses it, and so do an
// approve_merge or a deny_merge without a pending merge request and a
// github_export the console would refuse.
func (t *turn) describe(tool chatTool, in *toolInput) (string, error) {
	if tool.name == "routine_write" || tool.name == "routine_delete" {
		return t.describeRoutine(tool, *in)
	}
	if tool.name == "github_export" {
		if _, err := t.exportToken(); err != nil {
			return "", err
		}
	}
	parts := []string{tool.title}
	var k factoryd.Task
	if in.TaskID != "" {
		var err error
		if k, err = t.card(*in); err != nil {
			return "", err
		}
		if tool.name == "accept_plan" {
			if err := t.pinPlan(in); err != nil {
				return "", err
			}
		}
		parts = append(parts, fmt.Sprintf("#%d %s %q", k.IssueNumber, k.ID, factoryd.CardTitle(k)))
	}
	if in.DependsOn != "" {
		d, err := t.card(toolInput{TaskID: in.DependsOn})
		if err != nil {
			return "", err
		}
		edge := "depends on"
		if in.Remove {
			edge = "no longer depends on"
		}
		parts = append(parts, fmt.Sprintf("%s #%d %s %q", edge, d.IssueNumber, d.ID, factoryd.CardTitle(d)))
	}
	for _, f := range []struct{ key, v string }{{"name", in.Name}, {"title", in.Title}, {"body", in.Body}, {"lane", in.Lane}, {"size", in.Size}, {"action", in.Action},
		{"summary", in.Summary}, {"accent", in.Accent}, {"text", in.Text}, {"note", in.Note}, {"remote", in.Remote}, {"login", in.Login}, {"mode", in.Mode},
		{"verdict", in.Verdict}, {"autonomy", in.Autonomy}, {"path", in.Path}, {"old", in.Old}, {"new", in.New}, {"content", in.Content}} {
		if f.v != "" {
			parts = append(parts, f.key+" "+textutil.Truncate(f.v, 120))
		}
	}
	if len(in.Acceptance) > 0 {
		items := make([]string, len(in.Acceptance))
		for i, a := range in.Acceptance {
			items[i] = textutil.Truncate(a, 120)
		}
		parts = append(parts, "acceptance "+strings.Join(items, "; "))
	}
	if tool.name == "routine_skip" && in.SkipUndo {
		parts = append(parts, "undo the pending skip")
	}
	if tool.name == "set_autonomy" {
		if in.BacklogCap > 0 {
			parts = append(parts, fmt.Sprintf("backlog cap %d", in.BacklogCap))
		} else {
			v, _ := t.c.inForce(t.r.Context())
			parts = append(parts, fmt.Sprintf("backlog cap %d stays", v.Tree.BacklogCapOf(t.p.Repository)))
		}
	}
	if tool.name == "tell_card" {
		branch, k, _, err := t.tellRoute(*in)
		if err != nil {
			return "", err
		}
		return sanitize.Inbound(toldLine(strings.Join(parts, ", "), branch, k)), nil
	}
	if in.Revision > 0 {
		rev := fmt.Sprintf("revision %d", in.Revision)
		if in.SHA256 != "" {
			rev += " sha " + textutil.Truncate(in.SHA256, 7)
		}
		parts = append(parts, rev)
	}
	if tool.name == "answer" {
		bt, _, err := t.c.cfg.Tasks.BoardTask(t.r.Context(), in.TaskID)
		if err != nil {
			return "", err
		}
		if len(bt.Choices) > 0 {
			parts = append(parts, "choices "+numbered(bt.Choices))
		}
		if in.Question > 0 && in.Revision > 0 {
			qs, err := t.c.cfg.Tasks.PlanQuestions(t.r.Context(), k.ID, in.Revision)
			if err != nil {
				return "", err
			}
			if in.Question <= len(qs) {
				parts = append(parts, "question "+factoryd.QuestionLine(in.Question, qs[in.Question-1]))
			}
		}
		if len(in.Answers) > 0 {
			lines := make([]string, 0, len(in.Answers))
			for _, ans := range in.Answers {
				lines = append(lines, fmt.Sprintf("question %d: %s", ans.Question, textutil.Truncate(strings.Join(strings.Fields(ans.Text), " "), 120)))
			}
			parts = append(parts, strings.Join(lines, ", "))
		}
	}
	desc := strings.Join(parts, ", ")
	if tool.name == "deny_merge" {
		if _, err := t.mergeRequest(*in); err != nil {
			return "", err
		}
	}
	if tool.name == "approve_merge" {
		if _, err := t.mergeRequest(*in); err != nil {
			return "", err
		}
		steps, err := t.c.readTimeline(t.r.Context(), k.ID)
		if err != nil {
			return "", err
		}
		desc += mergeWarning(steps)
	}
	if tool.name == "alert_verdict" {
		al, ok, err := t.c.cfg.Alerts.AlertByID(t.r.Context(), in.ID)
		switch {
		case err != nil:
			log.Printf("console: alert %d: %v", in.ID, err)
			return "", refuse(http.StatusInternalServerError, "cannot read the alert")
		case !ok:
			return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no alert %d.", in.ID))
		}
		desc += fmt.Sprintf(", alert %d %q", al.ID, alertTitle(al))
	}
	if tool.name == "promote_resource" || tool.name == "release_resource" {
		res, ok, err := t.c.cfg.Resources.Commission(t.r.Context(), in.ID)
		switch {
		case err != nil:
			log.Printf("console: resource %d: %v", in.ID, err)
			return "", refuse(http.StatusInternalServerError, "cannot read the resource registry")
		case !ok:
			return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no commissioned resource %d.", in.ID))
		}
		desc += ", " + res.Kind + " " + strconv.FormatInt(res.ID, 10) + " of " + res.Repo
	}
	return sanitize.Inbound(desc), nil
}

// mergeShown bounds the review findings a merge's warning names.
const mergeShown = 5

// mergeWarning is the warning a merge proposal ends with, after a card's
// timeline steps: the newest review's findings, each on one line, and the
// checks of the newest run neither green nor skipped, or the run's state
// when it is not green and names no such check; "" for none.
func mergeWarning(steps []map[string]any) string {
	var w strings.Builder
	if m := newest(steps, "model_review"); m != nil {
		findings, _ := m["findings"].([]any)
		var shown []string
		for i, f := range findings {
			if i == mergeShown {
				shown = append(shown, fmt.Sprintf("and %d more", len(findings)-i))
				break
			}
			shown = append(shown, textutil.Truncate(strings.Join(strings.Fields(fmt.Sprint(f)), " "), 200))
		}
		if len(shown) > 0 {
			fmt.Fprintf(&w, " The review has %s: %s.", textutil.Plural(len(findings), "open finding", "open findings"), strings.Join(shown, "; "))
		}
	}
	if m := newest(steps, "checks"); m != nil {
		var failing []string
		jobs, _ := m["jobs"].([]any)
		for _, j := range jobs {
			job, _ := j.(map[string]any)
			if status, _ := job["status"].(string); status != "success" && status != "skipped" {
				name, _ := job["name"].(string)
				failing = append(failing, name)
			}
		}
		state, _ := m["state"].(string)
		switch {
		case len(failing) > 0:
			w.WriteString(" Failing checks: " + strings.Join(failing, ", ") + ".")
		case state != "" && state != "success":
			w.WriteString(" Checks: " + state + ".")
		}
	}
	if w.Len() == 0 {
		return ""
	}
	return ". Warning:" + w.String()
}

// exportToken is the project's export with the login and token the
// projects page's form stored, which the chat's export pushes with:
// refused where the console refuses the export and without a token,
// naming the form.
func (t *turn) exportToken() (factoryd.Mirror, error) {
	if err := t.c.exportable(t.r.Context(), t.a, t.p); err != nil {
		return factoryd.Mirror{}, err
	}
	m, ok, err := t.c.cfg.Mirrors.MirrorToken(t.r.Context(), t.p.Name, factoryd.MirrorExport)
	switch {
	case err != nil:
		log.Printf("console: the export of %s: %v", t.p.Name, err)
		return m, refuse(http.StatusInternalServerError, "cannot read the project's export")
	case !ok || m.Token == "":
		return m, refuse(http.StatusConflict, t.p.Name+" has no GitHub token for an export. "+t.c.exportForm(t.p.Name))
	}
	return m, nil
}

// pinPlan sets the plan revision in names, the newest for none, and its
// sha256 unless in names one; a card with no such revision keeps in.
func (t *turn) pinPlan(in *toolInput) error {
	if in.Revision > 0 && in.SHA256 != "" {
		return nil
	}
	k, err := t.card(*in)
	if err != nil {
		return err
	}
	rev, ok, err := t.c.cfg.Tasks.PlanRevision(t.r.Context(), k.ID, in.Revision)
	if ok {
		in.Revision = rev.Revision
		if in.SHA256 == "" {
			in.SHA256 = rev.SHA256
		}
	}
	return err
}

// numbered is a question's choices on one line, each with its number, so
// the member may answer "the second".
func numbered(choices []string) string {
	parts := make([]string, len(choices))
	for i, c := range choices {
		parts[i] = fmt.Sprintf("%d. %s", i+1, c)
	}
	return strings.Join(parts, "; ")
}

// planQuestions are the questions of plan card t's newest revision; none
// for a card that is no plan.
func (c *Console) planQuestions(ctx context.Context, t factoryd.Task) ([]factoryd.PlanQuestion, error) {
	if t.Lane != policy.LanePlan {
		return nil, nil
	}
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, t.ID, 0)
	if err != nil || !ok {
		return nil, err
	}
	return c.cfg.Tasks.PlanQuestions(ctx, t.ID, rev.Revision)
}

// answer answers the question card in waits on: a plan's open questions by
// their revision and numbers, all of them in one call when in names a list
// of answers, which a newer revision refuses; any other question with in's
// text.
func (t *turn) answer(in toolInput) (string, error) {
	ctx := t.r.Context()
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	qs, err := t.c.planQuestions(ctx, k)
	if err != nil {
		return "", err
	}
	open := slices.ContainsFunc(qs, func(q factoryd.PlanQuestion) bool { return q.Answer == "" })
	switch {
	case len(in.Answers) > 0 && !open:
		return "", wrongState(k, t.c.capFactsOf(ctx, k), "an open plan question")
	case len(in.Answers) > 0 || open:
		if in.Revision == 0 {
			return "", refuse(http.StatusBadRequest, "A plan's question is answered with its revision (revision), from the plan tool.")
		}
		if len(in.Answers) > 0 {
			return t.c.doAnswerPlanQuestions(ctx, t.a, in.TaskID, in.Revision, in.Answers)
		}
		return t.c.doAnswerPlanQuestion(ctx, t.a, in.TaskID, in.Revision, in.Question, in.Text)
	case in.Question > 0:
		return "", wrongState(k, t.c.capFactsOf(ctx, k), "an open plan question")
	}
	return t.c.doAnswer(ctx, t.a, in.TaskID, in.Text)
}

// cardLine is t on one line for the agent.
func cardLine(t factoryd.Task) string {
	s := fmt.Sprintf("#%d %s %q %s (%s)", t.IssueNumber, t.ID, factoryd.CardTitle(t), t.Status, factoryd.StatusMeaning(t.Status))
	if t.Gate != "" {
		s += fmt.Sprintf(", gate %s (%s)", t.Gate, factoryd.GateMeaning(t.Gate))
	}
	if t.BudgetEUR > 0 {
		s += fmt.Sprintf(", €%.2f of €%.2f", t.SpentEUR, t.BudgetEUR)
	}
	return s
}

// boardListed bounds each list of the board the agent reads: a column's
// cards, the approval requests and the notes.
const boardListed = 25

// boardText is project p's board in the wide view as the session's member
// sees it, for the agent: each column with its cards, the human lane, the
// escalations and the approval requests waiting. a is the chat's actor,
// whose member its approval cards read a healed head's checks as.
func (c *Console) boardText(r *http.Request, p factoryd.Project, a actor) (string, boardData, error) {
	d, err := c.boardOf(r, boardFilter{Project: p.Name, View: viewWide}, a)
	if errors.Is(err, errNoProject) {
		return "Project " + p.Name + " takes no agent cards: it has no board.", d, nil
	}
	if err != nil {
		return "", d, err
	}
	return boardWords(d), d, nil
}

// boardWords is board d's text for the agent, each list bounded at
// boardListed: the open cards apart from the finished ones, so no count
// of open cards takes in a card that ended.
func boardWords(d boardData) string {
	var b strings.Builder
	// listed writes up to boardListed of the whole's lines and counts the rest
	listed := func(prefix string, lines []string, whole int) {
		shown := min(len(lines), boardListed)
		for _, l := range lines[:shown] {
			b.WriteString(prefix + l)
		}
		if whole > shown {
			fmt.Fprintf(&b, "%sand %d more", prefix, whole-shown)
		}
	}
	list := func(title string, whole int, cards []card) {
		fmt.Fprintf(&b, "%s (%d):", title, whole)
		lines := make([]string, len(cards))
		for i, k := range cards {
			lines[i] = cardLine(k.Task)
			if k.Held {
				lines[i] += ", " + heldWords
			}
			if k.Provider {
				lines[i] += ", " + providerWords
			}
		}
		listed("\n  ", lines, whole)
		b.WriteString("\n")
	}
	b.WriteString("Open cards:\n")
	for _, col := range d.Columns {
		if col.Finished == "" {
			list(col.Title, len(col.Cards), col.Cards)
		}
	}
	list("Human lane", len(d.HumanTasks), d.HumanTasks)
	escalated := make([]card, 0, len(d.Escalations))
	for _, n := range d.Escalations {
		escalated = append(escalated, n.Card)
	}
	list("Escalated, waiting on an answer", len(escalated), escalated)
	if i := slices.IndexFunc(d.Columns, func(col column) bool { return col.Finished != "" }); i >= 0 {
		fmt.Fprintf(&b, "Finished cards, ended in the last %d days and open no more:\n", finishedSince/(24*time.Hour))
		for _, col := range d.Columns[i:] {
			if col.Finished != "" {
				list(col.Title, col.Count(), col.Cards)
			}
		}
	}
	fmt.Fprintf(&b, "Approval requests waiting (%d):", len(d.Approvals))
	requests := make([]string, len(d.Approvals))
	for i, a := range d.Approvals {
		requests[i] = fmt.Sprintf("%s request %d of %s", a.Kind, a.ID, a.TaskID)
		if a.Issue > 0 {
			requests[i] += fmt.Sprintf(" (#%d)", a.Issue)
		}
		if a.PRNumber > 0 {
			requests[i] += fmt.Sprintf(", pull request #%d", a.PRNumber)
		}
	}
	listed("\n  ", requests, len(requests))
	listed("\nNote: ", d.Problems, len(d.Problems))
	return b.String()
}

// limit is in's n, 20 when unset, at most 100.
func (in toolInput) limit() int {
	if in.N <= 0 {
		return 20
	}
	return min(in.N, 100)
}

// mergeRequest is the pending merge approval request of card id in the
// chat's project.
func (t *turn) mergeRequest(in toolInput) (factoryd.ApprovalRequest, error) {
	k, err := t.card(in)
	if err != nil {
		return factoryd.ApprovalRequest{}, err
	}
	all, err := t.c.cfg.Approvals.ApprovalRequests(t.r.Context(), "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		return factoryd.ApprovalRequest{}, refuse(http.StatusInternalServerError, "cannot read the approval requests")
	}
	i := slices.IndexFunc(all, func(q factoryd.ApprovalRequest) bool {
		return q.TaskID == k.ID && q.Kind == factoryd.ApprovalMerge && q.Project == t.p.Repository
	})
	if i < 0 {
		return factoryd.ApprovalRequest{}, wrongState(k, t.c.capFactsOf(t.r.Context(), k), "a pending merge request")
	}
	return all[i], nil
}

func eventLines(events []factoryd.Event) string {
	if len(events) == 0 {
		return "No events."
	}
	lines := make([]string, len(events))
	for i, e := range events {
		lines[i] = eventLine(e)
	}
	return strings.Join(lines, "\n")
}

// orWords joins words the way a sentence lists them: "a, b or c".
func orWords(ws ...string) string {
	if len(ws) < 2 {
		return strings.Join(ws, "")
	}
	return strings.Join(ws[:len(ws)-1], ", ") + " or " + ws[len(ws)-1]
}

// orderWords are a menu's order words for the view tool, newest naming
// the default, as its refusal and schema name them.
func orderWords(m menuSpec) []string {
	ws := []string{"newest"}
	for _, o := range m.Orders {
		if o.Value != "" {
			ws = append(ws, o.Value)
		}
	}
	return ws
}

// showWords are a menu's show words for the view tool, all naming the
// default; queued's also takes plan:n, as its menu offers per plan (C13).
func showWords(m menuSpec) []string {
	ws := []string{"all"}
	for _, o := range m.Onlys {
		if o.Value != "" {
			ws = append(ws, o.Value)
		}
	}
	if m.Name == "queued" {
		ws = append(ws, "plan:n")
	}
	return ws
}

// takeOrder reads the call's order word for one menu: "" leaves the
// page's choice, newest clears it, a word the menu does not carry is
// refused naming the words it does, for the tool works the menu and a
// word it does not offer names no control.
func takeOrder(m menuSpec, w string) (set bool, v string, err error) {
	if w == "" {
		return false, "", nil
	}
	if w == "newest" {
		return true, "", nil
	}
	for _, o := range m.Orders {
		if o.Value == w {
			return true, w, nil
		}
	}
	return false, "", refuse(http.StatusBadRequest, fmt.Sprintf("The %s order must be %s.", m.Name, orWords(orderWords(m)...)))
}

// takeShow reads the call's show word for one menu, all naming the
// default and queued's taking plan:n for the plan with that number.
func takeShow(m menuSpec, w string) (set bool, v string, err error) {
	if w == "" {
		return false, "", nil
	}
	if m.Name == "queued" && strings.HasPrefix(w, "plan:") {
		if _, e := strconv.ParseInt(strings.TrimPrefix(w, "plan:"), 10, 64); e == nil {
			return true, w, nil
		}
		return false, "", refuse(http.StatusBadRequest, fmt.Sprintf("The %s show must be %s.", m.Name, orWords(showWords(m)...)))
	}
	if w == "all" {
		return true, "", nil
	}
	for _, o := range m.Onlys {
		if o.Value == w {
			return true, w, nil
		}
	}
	return false, "", refuse(http.StatusBadRequest, fmt.Sprintf("The %s show must be %s.", m.Name, orWords(showWords(m)...)))
}

// pageFilter is the board's filter the member's page stands on (282):
// the view field the chat's POST carries, read as the URL's query is,
// under the chat's own project. Only the wide view is a view the board
// carries, so no other view word rides along.
func (t *turn) pageFilter() boardFilter {
	v, _ := url.ParseQuery(strings.TrimPrefix(t.pageQuery, "?"))
	f := boardFilter{Project: t.p.Name, Q: strings.TrimSpace(v.Get("q"))}
	if v.Get("view") == viewWide {
		f.View = viewWide
	}
	readCols(&f, v.Get)
	return f
}

// viewNote says what the board now shows, in the words of its controls,
// for the view tools' stored answer (282).
func (f boardFilter) viewNote() string {
	var parts []string
	if f.View == viewWide {
		parts = append(parts, "the wide view")
	}
	if f.Q != "" {
		parts = append(parts, fmt.Sprintf("find %q", f.Q))
	}
	for _, m := range colMenus {
		c := f.col(m.Name)
		var ws []string
		for _, o := range m.Orders {
			if o.Value != "" && o.Value == c.Order {
				ws = append(ws, o.Label)
			}
		}
		for _, o := range m.Onlys {
			if o.Value != "" && o.Value == c.Only {
				ws = append(ws, o.Label)
			}
		}
		if strings.HasPrefix(c.Only, "plan:") {
			ws = append(ws, "from "+c.Only)
		}
		if len(ws) > 0 {
			parts = append(parts, columnTitles[m.Col]+" "+strings.Join(ws, " and "))
		}
	}
	switch f.Routines {
	case "none":
		parts = append(parts, "no routines")
	case "within":
		parts = append(parts, "routines within "+f.Period())
	}
	if len(parts) == 0 {
		return "The board now shows every card, the default view."
	}
	return "The board now shows " + strings.Join(parts, ", ") + "."
}

// tabNames are the drawer's tabs by name, from drawerTabs itself.
var tabNames = func() (ns []string) {
	for _, d := range drawerTabs {
		ns = append(ns, d.Name)
	}
	return ns
}()

// setView is the board view and reset tools' run (282): the page's view
// and the call's words merge into one filter whose query the member's
// screen alone then holds, as a click holds it, nothing stored. Reset
// instead clears every control to the board's default and closes the
// drawer, leaving the member's theme theirs.
func (t *turn) setView(in toolInput, reset bool) (string, *inverse, error) {
	var f boardFilter
	var e map[string]any
	if reset {
		f = boardFilter{Project: t.p.Name}
		e = map[string]any{"card": ""}
	} else {
		f = t.pageFilter()
		e = map[string]any{}
		for _, m := range colMenus {
			var order, only string
			switch m.Name {
			case "queued":
				order, only = in.Queued, in.QueuedOnly
			case "progress":
				order, only = in.Progress, in.ProgressOnly
			case "done":
				order, only = in.Done, in.DoneOnly
			}
			c := f.col(m.Name)
			if set, v, err := takeOrder(m, order); err != nil {
				return "", nil, err
			} else if set {
				c.Order = v
			}
			if set, v, err := takeShow(m, only); err != nil {
				return "", nil, err
			} else if set {
				c.Only = v
			}
			f.setCol(m.Name, c)
		}
		if t.present["find"] {
			f.Q = strings.TrimSpace(in.Find)
		}
		switch in.View {
		case "":
		case "wide":
			f.View = viewWide
		case "none":
			f.View = ""
		default:
			return "", nil, refuse(http.StatusBadRequest, "The view must be wide or none.")
		}
		switch in.Routines {
		case "":
		case "all":
			f.Routines = ""
		case "none", "within":
			f.Routines = in.Routines
		default:
			return "", nil, refuse(http.StatusBadRequest, "The routines must be all, within or none.")
		}
		if in.Within != 0 || t.present["within"] {
			if in.Within < 1 || in.Within > 999 {
				return "", nil, refuse(http.StatusBadRequest, "The period must be 1 to 999.")
			}
			if f.Routines != "within" {
				return "", nil, refuse(http.StatusBadRequest, "The within and unit words need routines within.")
			}
			f.Within = in.Within
		}
		if in.Unit != "" {
			if _, ok := periodUnits[in.Unit]; !ok {
				return "", nil, refuse(http.StatusBadRequest, "The unit must be days or hours.")
			}
			if f.Routines != "within" {
				return "", nil, refuse(http.StatusBadRequest, "The within and unit words need routines within.")
			}
			f.Unit = in.Unit
		}
		// the routines control's own defaults, which the page query's
		// reader gives a routines=within it stands on
		if f.Routines == "within" {
			if f.Within < 1 {
				f.Within = 1
			}
			if f.Unit == "" {
				f.Unit = "days"
			}
		}
		if in.Tab != "" {
			if !slices.Contains(tabNames, in.Tab) {
				return "", nil, refuse(http.StatusBadRequest, "The tab must be "+orWords(tabNames...)+".")
			}
			if in.Card == "" {
				return "", nil, refuse(http.StatusBadRequest, "The tab needs the card it opens.")
			}
			e["tab"] = in.Tab
		}
		if in.Card != "" {
			task, err := t.c.cardIn(t.r.Context(), t.a, in.Card)
			if err != nil {
				return "", nil, err
			}
			e["card"] = task.ID
		}
		switch in.Theme {
		case "":
		case "dark", "light":
			e["theme"] = in.Theme
		default:
			return "", nil, refuse(http.StatusBadRequest, "The theme must be dark or light.")
		}
	}
	e["query"] = f.query()
	t.viewEvent = e
	if reset {
		return "The board is back to its default view and the drawer is closed.", nil, nil
	}
	note := f.viewNote()
	if card, ok := e["card"].(string); ok {
		note = strings.TrimSuffix(note, ".") + ", the card " + card + " open in the drawer."
	}
	if th, ok := e["theme"].(string); ok {
		note = strings.TrimSuffix(note, ".") + ", the " + th + " theme."
	}
	return note, nil, nil
}

// boardViewParams are the board view tool's controls (282), one per
// control of the screen, the menus' words read from the menus themselves.
func boardViewParams() map[string]any {
	p := map[string]any{
		"view":     enum("the wide view: wide or none", "wide", "none"),
		"find":     str("the find's words, empty clears"),
		"routines": enum("the routines: all, within or none", "all", "within", "none"),
		"within":   map[string]any{"type": "integer", "description": "the period's length, 1 to 999", "minimum": 1, "maximum": 999},
		"unit":     enum("the period's unit: days or hours", "days", "hours"),
		"card":     str("the card to open in the drawer"),
		"tab":      enum("the card's tab: "+orWords(tabNames...), tabNames...),
		"theme":    enum("the theme: dark or light", "dark", "light"),
	}
	for _, m := range colMenus {
		title := strings.ToLower(columnTitles[m.Col][:1]) + columnTitles[m.Col][1:]
		p[m.Name] = enum(title+" order: "+orWords(orderWords(m)...), orderWords(m)...)
		p[m.Name+"_only"] = enum(title+" show: "+orWords(showWords(m)...), showWords(m)...)
	}
	return p
}

// tools are the project agent's tools in policy.ChatTools' order.
var tools = []chatTool{
	{name: "system", title: "Read the manual",
		about: "The factory's manual: states, gates, how the tools work, autonomy, the process and its limits.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.systemText()
			return s, nil, err
		}},
	{name: "board", title: "Read the board",
		about: "The project's board: each column's cards, the human lane, the escalations and the approval requests.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, _, err := t.c.boardText(t.r, t.p, t.a)
			return s, nil, err
		}},
	{name: "card", title: "Read a card", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "One card in full, its model calls included, with pointers to its plan, pull request and sessions.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.cardText(in); return s, nil, err }},
	{name: "history", title: "Read the history", params: map[string]any{"n": nParam},
		about: "The project's events, newest first: what was done to which card, by whom and how.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			events, err := t.c.cfg.Events.ProjectEvents(t.r.Context(), t.p.Repository, 0, in.limit())
			return eventLines(events), nil, err
		}},
	{name: "card_history", title: "Read a card's history", params: map[string]any{"task_id": taskParam, "n": nParam}, required: []string{"task_id"},
		about: "One card's events, newest first.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			k, err := t.card(in)
			if err != nil {
				return "", nil, err
			}
			events, err := t.c.cfg.Events.TaskEvents(t.r.Context(), k.ID, in.limit())
			return eventLines(events), nil, err
		}},
	{name: "costs", title: "Read the costs",
		params: map[string]any{"by": enum("the whole ledger by day, card or tier", "day", "card", "tier"),
			"from_date": str("the first UTC day, YYYY-MM-DD (default the month's first)"),
			"to_date":   str("the last UTC day, YYYY-MM-DD (default today)")},
		about: "The project's spend this month against its cap, or the whole ledger by day, card or tier.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			if in.By != "" {
				s, err := t.c.costsByText(t.r.Context(), t.a, in.By, in.FromDate, in.ToDate)
				return s, nil, err
			}
			s, err := t.c.spendLine(t.r.Context(), t.p)
			return "The project's " + s + ".", nil, err
		}},
	{name: "diff", title: "Read a pull request's diff", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "A card's pull request diff, its first 20 KB.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.diff(in); return s, nil, err }},
	{name: "plan", title: "Read a plan", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "The newest revision of a card's plan: its sha256, questions and cards.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.planText(in); return s, nil, err }},
	{name: "pr", title: "Read a pull request",
		params:   map[string]any{"task_id": taskParam, "with_diff": map[string]any{"type": "boolean", "description": "add the diff"}},
		required: []string{"task_id"},
		about:    "A card's pull request: its checks, reviews, audit and changed files.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.prText(in); return s, nil, err }},
	{name: "file", title: "Read a file",
		params:   map[string]any{"ref": str("a branch, tag or commit"), "path": str("the file's path")},
		required: []string{"ref", "path"},
		about:    "A file of the project's repository at a branch, tag or commit, its first 64 KB.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.fileText(in); return s, nil, err }},
	{name: "session", title: "Read a session", params: map[string]any{"task_id": taskParam, "n": sessionN}, required: []string{"task_id"},
		about: "One session of a card: its outcome, summary, supervisor trips, model calls and changed files.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.sessionText(in); return s, nil, err }},
	{name: "session_steps", title: "List a session's steps",
		params: map[string]any{"task_id": taskParam, "n": sessionN, "from": fallback(1, stepParam("the first step")),
			"to": stepParam("the last step (default and most: from + 99)")},
		required: []string{"task_id"},
		about:    "A session's steps, one line each.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.stepsText(in); return s, nil, err }},
	{name: "session_step", title: "Read a session's steps",
		params: map[string]any{"task_id": taskParam, "n": sessionN, "from": fallback(1, stepParam("the first step")),
			"to": stepParam("the last step (default from, most from + 19)")},
		required: []string{"task_id"},
		about:    "A session's steps in full.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.stepText(in); return s, nil, err }},
	{name: "session_search", title: "Search a card's sessions",
		params: map[string]any{"task_id": taskParam, "query": str("the text to find, any case"),
			"n": stepParam("one session's number (default all)")},
		required: []string{"task_id", "query"},
		about:    "Search a card's sessions for a text, at most 20 hits.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.searchText(in); return s, nil, err }},
	{name: "ci", title: "Read CI",
		params: map[string]any{"ref": fallback("main", str("a branch")), "sha": str("a commit (default the ref's head)")},
		about:  "The CI result of main, a branch or a commit.",
		run:    func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.ciText(in); return s, nil, err }},
	{name: "alerts", title: "Read the alerts",
		params: map[string]any{"state": fallback("open", enum("open: without a verdict, all: every alert", "open", "all"))},
		about:  "The factory's alerts, newest first.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.c.alertsText(t.r.Context(), t.a, cmp.Or(in.State, "open"))
			return s, nil, err
		}},
	{name: "resources", title: "Read the resources",
		about: "The commissioned cloud resources by project.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.c.resourcesText(t.r.Context(), t.a)
			return s, nil, err
		}},
	{name: "settings", title: "Read the settings",
		about: "Every project's settings.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.c.projectsText(t.r.Context(), t.a)
			return s, nil, err
		}},
	{name: "policy", title: "Read the policy",
		about: "The policy in force: its limits, budgets, risk classes and approvers.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.c.policyText(t.r.Context(), t.a)
			return s, nil, err
		}},
	{name: "alert_verdict", title: "Judge an alert", write: true,
		params:   map[string]any{"id": map[string]any{"type": "integer", "description": "the alert's id", "minimum": 1}, "verdict": enum("real pointed at a problem, noise did not", factoryd.VerdictReal, factoryd.VerdictNoise)},
		required: []string{"id", "verdict"},
		about:    "Judge an open escalation alert real or noise.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doAlertVerdict(t.r.Context(), t.a, in.ID, in.Verdict)
			return note, nil, err
		}},
	{name: "promote_resource", title: "Make a resource standing", write: true,
		params: map[string]any{"id": map[string]any{"type": "integer", "description": "the resource's id", "minimum": 1}}, required: []string{"id"},
		about: "Make a leased resource standing, owned by its project.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doPromoteResource(t.r.Context(), t.a, in.ID)
			return note, nil, err
		}},
	{name: "release_resource", title: "Release a resource", write: true,
		params: map[string]any{"id": map[string]any{"type": "integer", "description": "the resource's id", "minimum": 1}}, required: []string{"id"},
		about: "End a resource's lease now, so the collector deletes it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doReleaseResource(t.r.Context(), t.a, in.ID)
			return note, nil, err
		}},
	{name: "new_card", title: "File a card", write: true,
		params: map[string]any{"title": str("one line"), "body": str("the description, Markdown"),
			"acceptance": map[string]any{"type": "array", "minItems": 1, "maxItems": factoryd.MaxCardAcceptance,
				"items":       map[string]any{"type": "string"},
				"description": "the acceptance tests, one line each"},
			"lane":        fallback(policy.LaneBacklog, enum("where the card starts", "backlog", "plan", "ready", "human")),
			"size":        enum("the card's size, which sets its budget", policy.CardSizes...),
			"rung":        enum("the model rung it starts on", factoryd.RungCheap, factoryd.RungFrontier),
			"rung_reason": str("why that rung, one line")},
		required: []string{"title"},
		about:    "File a new card on the project.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			body := in.Body
			if len(in.Acceptance) > 0 {
				body += "\n\nAcceptance:"
				for _, a := range in.Acceptance {
					body += "\n- " + a
				}
			}
			if flags := filingFlags(in.Title, body); len(flags) > 0 {
				return "", nil, refuse(http.StatusBadRequest, flaggedCard(flags))
			}
			repo, issue, err := t.c.doFileCard(t.r.Context(), t.a, t.p.Name, in.Title, body, in.Lane, "", in.Size, in.Rung, in.RungReason)
			return fmt.Sprintf("Filed %s#%d in lane %s.", repo, issue, in.Lane), nil, err
		}},
	{name: "move_card", title: "Move a card", write: true,
		params: map[string]any{"task_id": taskParam, "lane": enum("the starting lane", policy.LanePlan, policy.LaneReady)}, required: []string{"task_id", "lane"},
		about: "Move a backlog card into a starting lane.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doMoveCard(t.r.Context(), t.a, in.TaskID, in.Lane)
			return note, &inverse{"unmove_card", toolInput{TaskID: in.TaskID, Lane: in.Lane}}, err
		}},
	{name: "answer", title: "Answer a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam,
		"question": map[string]any{"type": "integer", "description": "a plan question's number", "minimum": 1},
		"revision": map[string]any{"type": "integer", "description": "the plan's revision, for a plan's questions", "minimum": 1},
		"answers": map[string]any{"type": "array", "description": "every open question's answer at once",
			"items": map[string]any{"type": "object", "properties": map[string]any{
				"question": map[string]any{"type": "integer", "description": "the question's number", "minimum": 1},
				"text":     map[string]any{"type": "string", "description": "its answer"}},
				"required": []string{"question", "text"}}}},
		required: []string{"task_id"},
		about:    "Answer the question a card waits on, or every open question of a plan at once.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.answer(in)
			return note, nil, err
		}},
	{name: "comment", title: "Comment on a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Comment on a card's issue.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			posted, note, err := t.c.doComment(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, &inverse{"delete_comment", toolInput{TaskID: in.TaskID, N: int(posted)}}, err
		}},
	{name: "tell_card", title: "Tell a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Tell a card one text, routed by its state to its session, its merge request or its question.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.runTellCard(in)
			return note, nil, err
		}},
	{name: "steer", title: "Message a running card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Send a running card's session a message.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doSteer(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "retry", title: "Retry a card", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "File a failed plan card again, its replacement in lane ready.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRetry(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "start_plan", title: "Start a plan", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Start an accepted plan, its backlog cards into lane ready.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doStartPlan(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "accept_plan", title: "Accept a plan", write: true,
		params: map[string]any{"task_id": taskParam, "revision": map[string]any{"type": "integer", "description": "the revision the member saw (default the newest)", "minimum": 1},
			"sha256": str("its sha256 (default the revision's)")},
		required: []string{"task_id"},
		about:    "Accept a plan's revision, which factoryd files as cards.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			if err := t.pinPlan(&in); err != nil {
				return "", nil, err
			}
			note, err := t.c.doAcceptPlan(t.r.Context(), t.a, in.TaskID, in.Revision, in.SHA256)
			return note, nil, err
		}},
	{name: "request_changes", title: "Ask for changes to a plan", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Ask the planner for changes to a plan.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRequestChanges(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "reject_plan", title: "Reject a plan", write: true, params: map[string]any{"task_id": taskParam, "note": str("why")},
		required: []string{"task_id", "note"},
		about:    "Reject a plan, its task failed with the reason.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRejectPlan(t.r.Context(), t.a, in.TaskID, in.Note)
			return note, nil, err
		}},
	{name: "approve_merge", title: "Approve a merge", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Approve a card's pending merge request, which factoryd then merges.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			q, err := t.mergeRequest(in)
			if err != nil {
				return "", nil, err
			}
			note, err := t.c.doApprove(t.r.Context(), t.a, q.ID)
			return note, nil, err
		}},
	{name: "deny_merge", title: "Deny a merge", write: true,
		params: map[string]any{"task_id": taskParam, "note": str("why"),
			"action": fallback("request_changes", enum("request_changes returns it to its agent, reject fails it", "request_changes", "reject"))},
		required: []string{"task_id", "note"},
		about:    "Deny a card's merge request with a reason, or close a pull request factoryd opened with it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			q, err := t.mergeRequest(in)
			if err != nil {
				return "", nil, err
			}
			note, err := t.c.doDeny(t.r.Context(), t.a, q.ID, in.Action, in.Note)
			return note, nil, err
		}},
	{name: "rerun_checks", title: "Re-run a pull request's checks", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Run the red checks of a pull request factoryd opened again, from its pending merge request.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			q, err := t.mergeRequest(in)
			if err != nil {
				return "", nil, err
			}
			note, err := t.c.doRerun(t.r.Context(), t.a, q.ID)
			return note, nil, err
		}},
	{name: "set_summary", title: "Set a card's summary", write: true,
		params:   map[string]any{"task_id": taskParam, "summary": str("one line, empty restores the generated title")},
		required: []string{"task_id"},
		about:    "Set the one-line summary a card's face shows instead of its generated title.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			was, err := t.c.doSetSummary(t.r.Context(), t.a, in.TaskID, in.Summary)
			return in.setNote("summary", was.IssueNumber, in.Summary), &inverse{"set_summary", toolInput{TaskID: in.TaskID, Summary: was.HumanSummary}}, err
		}},
	{name: "set_accent", title: "Set a card's accent", write: true,
		params:   map[string]any{"task_id": taskParam, "accent": enum("the colour, empty removes it", append([]string{""}, factoryd.Accents...)...)},
		required: []string{"task_id"},
		about:    "Set a card's colour label.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			was, err := t.c.doSetAccent(t.r.Context(), t.a, in.TaskID, in.Accent)
			return in.setNote("accent", was.IssueNumber, in.Accent), &inverse{"set_accent", toolInput{TaskID: in.TaskID, Accent: was.Accent}}, err
		}},
	{name: "invite", title: "Invite a member", write: true,
		params: map[string]any{"login": str("the Forgejo user name")}, required: []string{"login"},
		about: "Make a Forgejo user a writer of the project's repository.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doInvite(t.r.Context(), t.a, in.Login)
			return note, nil, err
		}},
	{name: "github_export", title: "Export to GitHub", write: true,
		params: map[string]any{"remote": str("the GitHub repository, owner/name"),
			"mode": fallback(factoryd.ModeMirror, enum("the export's mode", factoryd.ModeMirror, factoryd.ModePullRequests))},
		required: []string{"remote"},
		about:    "Export the project's repository to GitHub, in place of its last export.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			m, err := t.exportToken()
			if err != nil {
				return "", nil, err
			}
			note, err := t.c.doGithubExport(t.r.Context(), t.a, t.p, in.Remote, m.Username, m.Token, in.Mode)
			return note, nil, err
		}},
	{name: "github_sync", title: "Sync from GitHub", write: true, params: map[string]any{},
		about: "Bring the GitHub export's moved default branch back as a pull request to main.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doGithubSync(t.r.Context(), t.a, t.p)
			return note, nil, err
		}},
	{name: "close", title: "Close a card", write: true,
		params: map[string]any{"task_id": taskParam, "note": str("why, commented first")}, required: []string{"task_id"},
		about: "Close a card's issue, which ends the card.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doClose(t.r.Context(), t.a, in.TaskID, in.Note)
			return note, nil, err
		}},
	{name: "edit", title: "Edit a card", write: true,
		params: map[string]any{"task_id": taskParam, "title": str("the new title, empty keeps it"),
			"body": str("the new description, empty keeps it")},
		required: []string{"task_id"},
		about:    "Edit a card's issue title or description.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doEdit(t.r.Context(), t.a, in.TaskID, in.Title, in.Body)
			return note, nil, err
		}},
	{name: "grant_session", title: "Grant a session", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Grant a card at its session cap one more session, with an answer to its findings.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doGrantSession(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "set_dependency", title: "Set a dependency", write: true,
		params: map[string]any{"task_id": taskParam, "depends_on": str("the card it waits for, by task id or issue number"),
			"remove": map[string]any{"type": "boolean", "description": "removes the dependency"}},
		required: []string{"task_id", "depends_on"},
		about:    "Make an unstarted card wait for another until that one is done.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doSetDependency(t.r.Context(), t.a, in.TaskID, in.DependsOn, in.Remove)
			return note, &inverse{"set_dependency", toolInput{TaskID: in.TaskID, DependsOn: in.DependsOn, Remove: !in.Remove}}, err
		}},
	{name: "split_card", title: "Split a card", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Have a planner split a card's large pull request into 2 or 3 cards.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doSplit(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "read_doc", title: "Read a document",
		params: map[string]any{"path": docPath, "find": str("the lines holding this text, case-insensitive"),
			"from":  stepParam("the first line to read"),
			"to":    stepParam("the last line to read"),
			"regex": map[string]any{"type": "boolean", "description": "find is a regular expression"}},
		required: []string{"path"},
		about:    "A document of the project's document repository with its base_sha256 and line count.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.c.readDoc(t.r.Context(), t.cfg.DocRepo, in.Path, in.Find, in.From, in.To, in.Regex)
			return s, nil, err
		}},
	{name: "write_doc", title: "Write a document", write: true,
		params: map[string]any{"path": docPath,
			"old":         str("the passage to replace, as read"),
			"new":         str("the passage that replaces old, empty removes it"),
			"content":     str("a new file's content"),
			"base_sha256": str("read_doc's base_sha256")},
		required: []string{"path"},
		about:    "Commit an edit of one passage, or a new file, to the project's document repository.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doWriteDoc(t.r.Context(), t.a, t.cfg.DocRepo, t.p.Repository, in.Path, docEdit{Old: in.Old, New: in.New, Content: in.Content, Base: in.BaseSHA256})
			return note, nil, err
		}},
	{name: "set_autonomy", title: "Set the autonomy level", write: true,
		params: map[string]any{"autonomy": enum("the autonomy state", policy.AutonomyStates...),
			"backlog_cap": map[string]any{"type": "integer", "description": "how many cards may wait unstarted, unset keeps it", "minimum": 1}},
		required: []string{"autonomy"},
		about:    "Set the project's autonomy state and backlog cap through a pull request on its project file.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			if err := t.c.doSetAutonomy(t.r.Context(), t.p.Name, in.Autonomy, in.BacklogCap, t.a); err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("The project runs at %s now%s.", in.Autonomy, capNote(in.BacklogCap)), nil, nil
		}},
	{name: "routines", title: "Read the routines",
		about: "The project's routines: each one's schedule, next run, last run and open card.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.routinesText()
			return s, nil, err
		}},
	{name: "routine_write", title: "Write a routine", write: true,
		params: map[string]any{"name": routineName, "title": str("the title of the card each run files"),
			"schedule": map[string]any{"type": "array", "description": "the triggers, any one of which starts a run", "minItems": 1,
				"items": map[string]any{"type": "object", "description": "exactly one of cron, merges or on", "properties": map[string]any{
					"cron":   str("five fields in the factory's zone, day and weekday ANDed"),
					"merges": map[string]any{"type": "integer", "description": "a run every n merged cards of the project", "minimum": 1},
					"on":     enum("a run on each such event", policy.RoutineEvents...)}}},
			"files":      enum("what a run files", policy.FilesHuman, policy.FilesAgent, policy.FilesReport),
			"skill":      str("the skill an agent routine's card runs, unset keeps it"),
			"brief_text": str("each filed card's body, Markdown, unset keeps it"),
			"while_open": enum("what a run does while the last card is open, unset keeps it", policy.WhileOpenHold, policy.WhileOpenFile),
			"missed":     enum("what a run does when the factory was down, unset keeps it", policy.MissedValues...),
			"tell":       enum("whether a filing tells the inbox, unset keeps it", policy.TellValues...)},
		required: []string{"name", "title", "schedule", "files"},
		about:    "Create or change one of the project's routines through a pull request on its project file.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.writeRoutine(in)
			return s, nil, err
		}},
	{name: "routine_delete", title: "Delete a routine", write: true,
		params: map[string]any{"name": routineName}, required: []string{"name"},
		about: "Delete one of the project's routines through a pull request on its project file.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.deleteRoutine(in)
			return s, nil, err
		}},
	{name: "routine_run", title: "Run a routine now", write: true,
		params: map[string]any{"name": routineName}, required: []string{"name"},
		about: "Run one of the project's routines now, as the board's Run now does.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRoutineRun(t.r.Context(), t.a, t.p.Name, in.Name)
			return note, nil, err
		}},
	{name: "routine_skip", title: "Skip a routine's next run", write: true,
		params:   map[string]any{"name": routineName, "undo": map[string]any{"type": "boolean", "description": "clears a pending skip"}},
		required: []string{"name"},
		about:    "Skip the next run of one of the project's routines, or undo a pending skip.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRoutineSkip(t.r.Context(), t.a, t.p.Name, in.Name, in.SkipUndo)
			return note, nil, err
		}},
	{name: "notes_read", title: "Read the scratchpad",
		about: "The project's scratchpad, which every thread of the chat shares.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.notesText()
			return s, nil, err
		}},
	{name: "notes_write", title: "Write the scratchpad", write: true,
		params: map[string]any{"text": str("a note to append"),
			"old": str("a passage to replace"), "new": str("the passage that replaces old, empty removes it")},
		about: "Append a note to the scratchpad, or replace one passage of it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.writeNotes(in.Text, in.Old, in.New)
			return s, nil, err
		}},
	{name: "escalate", title: "Escalate to the members", write: true,
		params: map[string]any{"ask": str("the title: one sentence, 200 bytes at most, with numbers"),
			"why":            str("why the card cannot go on without a person's answer"),
			"info":           str("the facts a reader needs, with what would change your mind"),
			"recommendation": str("the answer you would pick and where you read it"),
			"choices": map[string]any{"type": "array", "minItems": 2, "maxItems": factoryd.MaxChoices, "items": map[string]any{"type": "string", "maxLength": factoryd.MaxChoice},
				"description": "answers the members may press, each once"}},
		required: []string{"ask", "why", "info", "recommendation", "choices"},
		about:    "Ask the members for a decision, its why and its info with it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.escalate(in.Ask, in.Why, in.Info, in.Recommendation, in.Choices)
			return s, nil, err
		}},
	{name: "ask", title: "Ask the member",
		params: map[string]any{"ask": str("the question, with its state and its numbers"),
			"choices": map[string]any{"type": "array", "minItems": 2, "maxItems": factoryd.MaxChoices, "items": map[string]any{"type": "string", "maxLength": factoryd.MaxChoice},
				"description": "the answers the member may press"}},
		required: []string{"ask", "choices"},
		about:    "Ask this conversation's member a question, answered by a press or their next message.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.askMember(in.Ask, in.Choices)
			return s, nil, err
		}},
	{name: "attach_analysis", title: "Attach an analysis to an ask", write: true,
		params: map[string]any{"task_id": taskParam,
			"found":          str("what you learned about the ask, the facts with their numbers"),
			"recommendation": str("the answer you would pick and where you read it")},
		required: []string{"task_id", "found"},
		about:    "Record what you found inside a card's open ask, for the human who reads it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.attachAnalysis(in)
			return s, nil, err
		}},
	{name: "active_threads", title: "Read the project's threads",
		about: "The project's active threads, every member's with its owner named, then the scratchpad's tail.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.activeThreads()
			return s, nil, err
		}},
	{name: "chat_search", title: "Search the project's threads",
		params: map[string]any{"query": str("the text to find, any case"),
			"thread": map[string]any{"type": "integer", "description": "one of your own thread's numbers to search only it", "minimum": 1}},
		required: []string{"query"},
		about:    "Search every thread of the project, every member's, for a text, at most 20 hits.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.chatSearch(in); return s, nil, err }},
	{name: "board_view", title: "Board view",
		about:  "Works the board's view controls as a click does: menus, find, routines, card, theme.",
		params: boardViewParams(),
		run:    func(t *turn, in toolInput) (string, *inverse, error) { return t.setView(in, false) },
	},
	{name: "board_view_reset", title: "Board view reset",
		about: "Clears the board's view controls to the default and closes the drawer.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { return t.setView(toolInput{}, true) },
	},
	{name: "watch_card", title: "Watch a card",
		params:   map[string]any{"task_id": taskParam},
		required: []string{"task_id"},
		about:    "Watch a card: its moves wake this thread.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.watchCard(in); return s, nil, err }},
	{name: "unwatch_card", title: "End a watch",
		params:   map[string]any{"task_id": taskParam},
		required: []string{"task_id"},
		about:    "End this thread's watch on a card.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.unwatchCard(in); return s, nil, err }},
	{name: "set_timer", title: "Set a timer",
		params: map[string]any{"minutes": map[string]any{"type": "integer", "description": "the minutes from now, 1 to 1440", "minimum": 1, "maximum": 1440},
			"note":    str("the note the wake carries, 1 to 200 characters"),
			"task_id": str("the card this timer dies with, its task id or issue number")},
		required: []string{"minutes", "note"},
		about:    "Set a one-shot timer that wakes this thread with its note.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.setTimer(in); return s, nil, err }},
	{name: "unmove_card", write: true, internal: true,
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doUnmoveCard(t.r.Context(), t.a, in.TaskID, in.Lane)
			return note, nil, err
		}},
	{name: "delete_comment", write: true, internal: true,
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doDeleteComment(t.r.Context(), t.a, in.TaskID, int64(in.N))
			return note, nil, err
		}},
	{name: "analyze_image", title: "Look at an image",
		params: map[string]any{"name": str("the image's name, img1 as its row shows"),
			"ask": map[string]any{"type": "string", "maxLength": 2000, "description": "what to look for (default what the image shows)"}},
		required: []string{"name"},
		priced:   true,
		about:    "One look at an image a member sent: the vision model's answer to a question about it.",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.lookImage(in); return s, nil, err }},
	{name: "read_attachment", title: "Read a sent document",
		params: map[string]any{"name": str("the document's name, doc1 as its row shows"), "find": str("the lines holding this text, case-insensitive"),
			"from":  stepParam("the first line to read"),
			"to":    stepParam("the last line to read"),
			"regex": map[string]any{"type": "boolean", "description": "find is a regular expression"}},
		required: []string{"name"},
		about:    "A document a member sent, with its base_sha256 and line count.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.readAttachment(in)
			return s, nil, err
		}},
	{name: "summarize_attachment", title: "Summarise a sent document",
		params:   map[string]any{"name": str("the document's name, doc1 as its row shows"), "ask": str("what the summary answers (default what the document says)")},
		required: []string{"name"},
		priced:   true,
		about:    "One model call over a document a member sent: its answer to a question about it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			s, err := t.summarizeAttachment(in)
			return s, nil, err
		}},
}

// diffLimit bounds the diff the agent reads.
const diffLimit = 20 << 10

// diff is the diff of card in's pull request, read as the member.
func (t *turn) diff(in toolInput) (string, error) {
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	if k.PRNumber == 0 {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d has no pull request.", k.IssueNumber))
	}
	repo, err := factoryd.SplitRepo(k.Repository)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "the task names no repository")
	}
	var raw []byte
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d.diff", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), k.PRNumber)
	if err := t.c.as(t.r.Context(), t.a.s, http.MethodGet, path, nil, http.StatusOK, &raw); err != nil {
		return "", &forgejoRefusal{"read the pull request's diff", err}
	}
	if len(raw) > diffLimit {
		return string(raw[:diffLimit]) + fmt.Sprintf("\n… the diff goes on for %s more bytes", answered(raw, len(raw)-diffLimit)), nil
	}
	return string(raw), nil
}

// toolNotes are what the tools' sentences leave to the manual, each
// stated once.
var toolNotes = `  diff, pr, file and ci read Forgejo; alerts, resources, settings and policy read what their console pages show.
  card is a card's state in one call: status, gate, spend, sessions, model calls per alias, dependencies, the question it waits on, its pull request and requests, and which of pr, plan and session holds the rest.
  A card's sessions are numbered from 1, the first; n names one, the newest by default.
  new_card's lanes: backlog waits, plan is a spec to plan, ready starts an agent now, human is a person's. A plan or ready card needs 1 to 10 acceptance tests, one line each, in acceptance. Its rung: cheap for routine, well-specified work, frontier for a large change, a plan or factoryd's own package; none derives it from the size and the text.
  github_export's mode mirror pushes every branch and tag; pull_requests opens a GitHub pull request per merged card.
  tell_card routes by the card's state: to its running session as a message (steer), at its pending merge request as changes requested (deny_merge), to the question it waits on as the answer (answer).
  answer takes a plan's open questions with the plan's revision, every one at once in answers; any other question takes text, one of its choices word for word or free.
  write_doc commits straight to master: an edit of one passage against read_doc's base_sha256, or a new file.
  The scratchpad steers nothing: what becomes direction moves into the roadmap with write_doc.
  ask questions this conversation's member, answered by a press or their next message, and ends the turn.
  escalate asks the members who are not in this conversation; a choice or a reply in their inbox answers it.
  attach_analysis records what you found inside a card's open ask; the members read it with the question.
  analyze_image looks at an image a member sent, once per question; cite what you saw, and look again rather than guess. A member's image is spend they approved; the chat's monthly budget still bounds the looks.
  read_attachment reads the thread's documents in chunks, never the whole file: the head first, then from and to, or find.
  for a long document, summarize_attachment first and read_attachment's ranges only where its answer points.
`

func (t *turn) systemText() (string, error) {
	v, ok := t.c.inForce(t.r.Context())
	if !ok {
		return "", refuse(http.StatusInternalServerError, "no policy in force")
	}
	var b strings.Builder
	b.WriteString(manual.Loop + "; a member steers through the chat, and every tool here is one of the console's actions.\n\n")
	b.WriteString("Card states:\n")
	for _, s := range factoryd.Statuses {
		fmt.Fprintf(&b, "  %s: %s\n", s, factoryd.StatusMeaning(s))
	}
	b.WriteString("\nGates:\n")
	proc, hasProc := v.Tree.Processes[t.p.Process]
	gateOrder := []string{factoryd.GateIntake, factoryd.GateSession}
	if hasProc {
		for _, s := range proc.Stages {
			gateOrder = append(gateOrder, s.Name)
		}
	}
	gateOrder = append(gateOrder, factoryd.GateHuman)
	for _, g := range gateOrder {
		fmt.Fprintf(&b, "  %s: %s\n", g, factoryd.GateMeaning(g))
	}
	b.WriteString("\nThe tools:\n" + toolNotes)
	b.WriteString("\nAutonomy states:\n")
	for _, s := range policy.AutonomyStates {
		fmt.Fprintf(&b, "  %s: %s\n", s, policy.AutonomyMeaning[s])
	}
	b.WriteString("\nProcess")
	if t.p.Process != "" {
		fmt.Fprintf(&b, " %s", t.p.Process)
	}
	b.WriteString(":\n")
	if hasProc {
		for _, l := range proc.Lanes {
			line := fmt.Sprintf("  lane %s", l.Name)
			if l.MovedBy != "" {
				line += ", moved by " + l.MovedBy
			}
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "  merge rule: %s\n", t.p.MergeRule)
	}
	fmt.Fprintf(&b, "  approvers: %s\n", strings.Join(v.ApproversOf(t.p.Repository), ", "))
	if _, eur := v.ProjectCap(t.p.Repository); !math.IsInf(eur, 1) {
		fmt.Fprintf(&b, "  project month cap: €%.2f\n", eur)
	}
	b.WriteString(limitLines(v.Tree.Document))
	fmt.Fprintf(&b, "\nChat bounds:\n  message size: %d KiB\n  model calls: as many as the task's budget holds\n", chatMessageMax>>10)
	b.WriteString("\nThe writing guide, for every word a person reads:\n" + writingguide.Guide)
	return b.String(), nil
}

// limitLines are the policy's task budget and limits, one indented line
// each, as the system and policy reads show them.
func limitLines(d policy.Document) string {
	l := d.Limits
	checkIn := "none"
	if l.CheckIn > 0 {
		checkIn = l.CheckIn.String()
	}
	after, nudges := l.Nudge()
	return fmt.Sprintf("  task budget: €%.2f default, €%.2f maximum\n  sessions per card: %d\n  agent time: %s\n  check-in: %s\n"+
		"  supervisor calls per session: %d\n  supervisor timeout: %s\n  returns before the stronger model: %d\n  steps per session: %d\n"+
		"  identical calls before a nudge: %d, nudges: %d\n  identical calls that trip the supervisor: %d\n",
		d.Budget.TaskDefaultEUR, d.Budget.TaskMaxEUR, l.Sessions, l.AgentTime, checkIn,
		l.SupervisorCalls, l.SupervisorTimeout, l.StrongerAfter, l.StepCap(), after, nudges, l.SameCallAfter())
}
