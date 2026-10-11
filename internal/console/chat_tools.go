package console

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// toolInput holds every tool's arguments; each tool reads its own.
type toolInput struct {
	TaskID    string `json:"task_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body,omitempty"`
	Lane      string `json:"lane,omitempty"`
	Text      string `json:"text,omitempty"`
	Note      string `json:"note,omitempty"`
	Action    string `json:"action,omitempty"`
	Revision  int    `json:"revision,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Accent    string `json:"accent,omitempty"`
	Login     string `json:"login,omitempty"`
	N         int    `json:"n,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Path      string `json:"path,omitempty"`
	WithDiff  bool   `json:"with_diff,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Username  string `json:"username,omitempty"`
	Token     string `json:"token,omitempty"`
	From      int    `json:"from,omitempty"`
	To        int    `json:"to,omitempty"`
	Query     string `json:"query,omitempty"`
	Size      string `json:"size,omitempty"`
	DependsOn string `json:"depends_on,omitempty"`
	Remove    bool   `json:"remove,omitempty"`
	// undo marks an undo's run, which only chatUndo sets: unexported, no
	// arguments carry it
	undo bool
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
type chatTool struct {
	name, title, about string
	params             map[string]any // the arguments' JSON Schema properties
	required           []string
	write              bool
	internal           bool // an undo's inverse only: the agent is not offered it
	fits               string
	run                func(t *turn, in toolInput) (string, *inverse, error)
}

func (tool chatTool) def() factoryd.OperatorTool {
	params := map[string]any{"type": "object", "properties": tool.params}
	if len(tool.required) > 0 {
		params["required"] = tool.required
	}
	return factoryd.OperatorTool{Name: tool.name, Description: tool.about, Parameters: params}
}

func str(about string) map[string]any { return map[string]any{"type": "string", "description": about} }

func stepParam(about string) map[string]any {
	return map[string]any{"type": "integer", "description": about, "minimum": 1}
}

func enum(about string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": about, "enum": values}
}

var (
	taskParam = str("the card's task id, t-...")
	nParam    = map[string]any{"type": "integer", "description": "how many, 1 to 100 (default 20)", "minimum": 1, "maximum": 100}
	textParam = str("the text, as the member writes it")
	sessionN  = stepParam("the session's number on the card, 1 the first (default the newest)")
)

const sessionFits = "a card with a session"

// chatTools are the tools the agent is offered, in policy.ChatTools'
// order: steer only where factoryd can reach a session, github_export
// where it keeps exports.
func (c *Console) chatTools() []chatTool {
	return slices.DeleteFunc(slices.Clone(tools), func(t chatTool) bool {
		return t.internal || t.name == "steer" && c.cfg.Steer == nil || t.name == "github_export" && c.cfg.Mirrors == nil
	})
}

func (c *Console) chatTool(name string) (chatTool, bool) {
	i := slices.IndexFunc(c.chatTools(), func(t chatTool) bool { return t.name == name })
	if i < 0 {
		return chatTool{}, false
	}
	return c.chatTools()[i], true
}

// card is the tool's card, refused unless it is the chat project's.
func (t *turn) card(in toolInput) (factoryd.Task, error) {
	return t.c.cardIn(t.r.Context(), t.a, in.TaskID)
}

// describe is one line saying what a proposal of tool with in would do;
// a card of another project refuses it.
func (t *turn) describe(tool chatTool, in toolInput) (string, error) {
	parts := []string{tool.title}
	if in.TaskID != "" {
		k, err := t.card(in)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("#%d %s %q", k.IssueNumber, k.ID, cardTitle(k)))
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
		parts = append(parts, fmt.Sprintf("%s #%d %s %q", edge, d.IssueNumber, d.ID, cardTitle(d)))
	}
	for _, f := range []struct{ key, v string }{{"title", in.Title}, {"body", in.Body}, {"lane", in.Lane}, {"size", in.Size}, {"action", in.Action},
		{"summary", in.Summary}, {"accent", in.Accent}, {"text", in.Text}, {"note", in.Note}, {"remote", in.Remote}, {"login", in.Login}} {
		if f.v != "" {
			parts = append(parts, f.key+" "+textutil.Truncate(f.v, 120))
		}
	}
	if in.Revision > 0 {
		parts = append(parts, fmt.Sprintf("revision %d", in.Revision))
	}
	if tool.name == "answer" {
		bt, _, err := t.c.cfg.Tasks.BoardTask(t.r.Context(), in.TaskID)
		if err != nil {
			return "", err
		}
		if len(bt.Choices) > 0 {
			parts = append(parts, "choices "+numbered(bt.Choices))
		}
	}
	return sanitize.Inbound(strings.Join(parts, ", ")), nil
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

// cardTitle is the face's title of t: the summary a writer set, the plan
// key of a planned card, else the objective's first clause.
func cardTitle(t factoryd.Task) string {
	if t.HumanSummary != "" {
		return t.HumanSummary
	}
	if t.PlanKey != "" {
		return t.PlanKey
	}
	return titleClause(t.Objective)
}

// cardLine is t on one line for the agent.
func cardLine(t factoryd.Task) string {
	s := fmt.Sprintf("#%d %s %q %s (%s)", t.IssueNumber, t.ID, cardTitle(t), t.Status, factoryd.StatusMeaning(t.Status))
	if t.Gate != "" {
		s += fmt.Sprintf(", gate %s (%s)", t.Gate, factoryd.GateMeaning(t.Gate))
	}
	if t.BudgetEUR > 0 {
		s += fmt.Sprintf(", €%.2f of €%.2f", t.SpentEUR, t.BudgetEUR)
	}
	return s
}

// chatColumnCards bounds the cards of one column the agent reads.
const chatColumnCards = 25

// boardText is project p's board in the wide view as the session's member
// sees it, for the agent: each column with its cards, the human lane, the
// escalations and the approval requests waiting.
func (c *Console) boardText(r *http.Request, p factoryd.Project) (string, boardData, error) {
	d, err := c.boardOf(r, boardFilter{Project: p.Name, View: viewWide})
	if errors.Is(err, errNoProject) {
		return "Project " + p.Name + " takes no agent cards: it has no board.", d, nil
	}
	if err != nil {
		return "", d, err
	}
	var b strings.Builder
	list := func(title string, cards []card) {
		fmt.Fprintf(&b, "%s (%d):", title, len(cards))
		for i, k := range cards {
			if i == chatColumnCards {
				fmt.Fprintf(&b, "\n  and %d more", len(cards)-i)
				break
			}
			b.WriteString("\n  " + cardLine(k.Task))
			if k.Held {
				b.WriteString(", " + heldWords)
			}
			if k.Provider {
				b.WriteString(", " + providerWords)
			}
		}
		b.WriteString("\n")
	}
	for _, col := range d.Columns {
		list(col.Title, col.Cards)
	}
	list("Human lane", d.HumanTasks)
	escalated := make([]card, 0, len(d.Escalations))
	for _, n := range d.Escalations {
		escalated = append(escalated, n.Card)
	}
	list("Escalated, waiting on an answer", escalated)
	fmt.Fprintf(&b, "Approval requests waiting (%d):", len(d.Approvals))
	for _, a := range d.Approvals {
		fmt.Fprintf(&b, "\n  %s request %d of %s", a.Kind, a.ID, a.TaskID)
		if a.Issue > 0 {
			fmt.Fprintf(&b, " (#%d)", a.Issue)
		}
		if a.PRNumber > 0 {
			fmt.Fprintf(&b, ", pull request #%d", a.PRNumber)
		}
	}
	for _, problem := range d.Problems {
		b.WriteString("\nNote: " + problem)
	}
	return b.String(), d, nil
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

// tools are the project agent's tools in policy.ChatTools' order.
var tools = []chatTool{
	{name: "system", title: "Read the manual",
		about: "The factory's manual, rendered from the code: the card states, the gates in order, the tools with the states each fits, this project's process and budgets, the chat's OK list; read it when a state or a refusal is unclear.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.systemText()
			return s, nil, err
		}},
	{name: "board", title: "Read the board",
		about: "The project's board as the member sees it: each column with its cards (issue, task id, title, status, gate, spend of budget), the human lane, the escalated cards and the approval requests waiting.",
		fits:  "any card",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, _, err := t.c.boardText(t.r, t.p)
			return s, nil, err
		}},
	{name: "card", title: "Read a card", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "One card in full: its state, gate, spend of budget, lane, issue and sessions of its cap; the cards it depends on and that depend on it; the question it waits on with its choices; its pull request with its head and state, and its waiting requests; which of the reads pr, plan and session holds its reviews, audit, checks, plan and sessions; its objective and the agent's summary.",
		fits:  "any card",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.cardText(in); return s, nil, err }},
	{name: "history", title: "Read the history", params: map[string]any{"n": nParam},
		about: "The project's newest events, newest first: what was done to which card, by whom and through what (button, chat, factoryd, webhook).",
		fits:  "any card",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			events, err := t.c.cfg.Events.ProjectEvents(t.r.Context(), t.p.Repository, 0, in.limit())
			return eventLines(events), nil, err
		}},
	{name: "card_history", title: "Read a card's history", params: map[string]any{"task_id": taskParam, "n": nParam}, required: []string{"task_id"},
		about: "One card's events, newest first.",
		fits:  "any card",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			k, err := t.card(in)
			if err != nil {
				return "", nil, err
			}
			events, err := t.c.cfg.Events.TaskEvents(t.r.Context(), k.ID, in.limit())
			return eventLines(events), nil, err
		}},
	{name: "costs", title: "Read the costs",
		about: "The project's spend this UTC month against its month cap, the cards' and the chat's apart.",
		fits:  "any card",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.c.spendLine(t.r.Context(), t.p)
			return "The project's " + s + ".", nil, err
		}},
	{name: "diff", title: "Read a pull request's diff", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "The diff of a card's pull request, read from Forgejo as the member; its first 20 KB.",
		fits:  "a card with a pull request",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.diff(in); return s, nil, err }},
	{name: "plan", title: "Read a plan", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "A plan's newest revision: its number and sha256 (which accept_plan takes), its summary, each card's key, role, title, size, dependencies and issue once filed, and each filed card's state; for a card of a plan, its plan's.",
		fits:  "a plan card or a card of a plan",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.planText(in); return s, nil, err }},
	{name: "pr", title: "Read a pull request",
		params:   map[string]any{"task_id": taskParam, "with_diff": map[string]any{"type": "boolean", "description": "add the diff's first 20 KB"}},
		required: []string{"task_id"},
		about:    "A card's pull request: its newest checks' jobs with their verdicts and run times, its newest model review with its findings and the earlier reviews' verdicts, its audit with its findings, and its changed files with their additions and deletions, read from Forgejo as the member.",
		fits:     "a card with a pull request",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.prText(in); return s, nil, err }},
	{name: "file", title: "Read a file",
		params:   map[string]any{"ref": str("a branch, tag or commit"), "path": str("the file's path in the repository")},
		required: []string{"ref", "path"},
		about:    "A file of the project's repository at a branch, tag or commit, read from Forgejo as the member; its first 64 KB, a binary file refused.",
		fits:     "the project's repository",
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.fileText(in); return s, nil, err }},
	{name: "session", title: "Read a session", params: map[string]any{"task_id": taskParam, "n": sessionN}, required: []string{"task_id"},
		about: "One session of a card, the newest unless n numbers one: its model, attempt and outcome, its duration, the agent's closing summary, the supervisor's trips and the messages it got, the files it changed, its diff stat, its spend and its step count.",
		fits:  sessionFits,
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.sessionText(in); return s, nil, err }},
	{name: "session_steps", title: "List a session's steps",
		params: map[string]any{"task_id": taskParam, "n": sessionN, "from": stepParam("the first step's number (default 1)"),
			"to": stepParam("the last step's number (default from + 99, at most 100 steps)")},
		required: []string{"task_id"},
		about:    "A session's transcript as numbered steps, one line each: the time since the session's start, the kind or tool, the duration and a summary without content; the first 100 unless from and to name others.",
		fits:     sessionFits,
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.stepsText(in); return s, nil, err }},
	{name: "session_step", title: "Read a session's steps",
		params: map[string]any{"task_id": taskParam, "n": sessionN, "from": stepParam("the first step's number"),
			"to": stepParam("the last step's number (default from, at most 20 steps)")},
		required: []string{"task_id", "from"},
		about:    "Steps of a session in full, from from to to: a tool call's arguments and error, a model call's text, a command's output, a checkpoint's diff stat; each step cut at 8 KiB, the answer at 32 KiB.",
		fits:     sessionFits,
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.stepText(in); return s, nil, err }},
	{name: "session_search", title: "Search a card's sessions",
		params: map[string]any{"task_id": taskParam, "query": str("the text to find, 2 to 200 characters, any case"),
			"n": stepParam("the session's number on the card (default every session)")},
		required: []string{"task_id", "query"},
		about:    "Search the transcripts of a card's sessions, or of session n, for a text: each hit's session, step and kind with the matching line and the line before and after it; at most 20 hits.",
		fits:     sessionFits,
		run:      func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.searchText(in); return s, nil, err }},
	{name: "new_card", title: "File a card", write: true,
		params: map[string]any{"title": str("one line, at most 255 characters"), "body": str("the description, Markdown"),
			"lane": enum("where the card starts: backlog to wait, plan for a spec to plan, ready for an agent to work on now, human for a human", "backlog", "plan", "ready", "human"),
			"size": enum("the card's size, which sets its budget; none for the default budget", policy.CardSizes...)},
		required: []string{"title", "lane"},
		about:    "File a new card on the project as the member: an issue with the lane's label, which the factory takes in.",
		fits:     "a lane of the project",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			repo, issue, err := t.c.doFileCard(t.r.Context(), t.a, t.p.Name, in.Title, in.Body, in.Lane, "", in.Size)
			return fmt.Sprintf("Filed %s#%d in lane %s.", repo, issue, in.Lane), nil, err
		}},
	{name: "move_card", title: "Move a card", write: true,
		params: map[string]any{"task_id": taskParam, "lane": enum("the starting lane", policy.LanePlan, policy.LaneReady)}, required: []string{"task_id", "lane"},
		about: "Move a backlog card into a starting lane of its process, as the member's label on its issue.",
		fits:  "a card in the backlog",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doMoveCard(t.r.Context(), t.a, in.TaskID, in.Lane)
			return note, &inverse{"unmove_card", toolInput{TaskID: in.TaskID, Lane: in.Lane}}, err
		}},
	{name: "answer", title: "Answer a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Answer the question a card waits on (it needs you), with one of the choices the card offers word for word or in free text: a comment on its issue as the member, which resumes it.",
		fits:     "a card waiting on a human with a question or at its session cap",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doAnswer(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "comment", title: "Comment on a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Comment on a card's issue as the member.",
		fits:     "any card",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			posted, note, err := t.c.doComment(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, &inverse{"delete_comment", toolInput{TaskID: in.TaskID, N: int(posted)}}, err
		}},
	{name: "steer", title: "Message a running card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Send a message to a running card's agent session, marked as the member's; the agent reads it next.",
		fits:     "a card running a session",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doSteer(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "retry", title: "Retry a card", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "File a failed card of a plan again and start the replacement in lane ready (an approver's action).",
		fits:  "a failed plan card",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRetry(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "start_plan", title: "Start a plan", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Start an accepted plan: its cards still in the backlog go to lane ready (an approver's action).",
		fits:  "an accepted plan",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doStartPlan(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "accept_plan", title: "Accept a plan", write: true,
		params: map[string]any{"task_id": taskParam, "revision": map[string]any{"type": "integer", "description": "the plan revision the member saw", "minimum": 1},
			"sha256": str("that revision's sha256, from the plan tool")},
		required: []string{"task_id", "revision", "sha256"},
		about:    "Accept a plan's revision, which factoryd then files as cards (an approver's action).",
		fits:     "a plan awaiting acceptance",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doAcceptPlan(t.r.Context(), t.a, in.TaskID, in.Revision, in.SHA256)
			return note, nil, err
		}},
	{name: "request_changes", title: "Ask for changes to a plan", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Ask the planner for changes to a plan: a comment on its issue as the member.",
		fits:     "a plan awaiting acceptance",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRequestChanges(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "reject_plan", title: "Reject a plan", write: true, params: map[string]any{"task_id": taskParam, "note": str("why")},
		required: []string{"task_id", "note"},
		about:    "Reject a plan: its task ends failed with the reason.",
		fits:     "a plan awaiting acceptance",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRejectPlan(t.r.Context(), t.a, in.TaskID, in.Note)
			return note, nil, err
		}},
	{name: "approve_merge", title: "Approve a merge", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Approve a card's waiting merge request: the member's approving review on its pull request, then factoryd merges it (an approver's action).",
		fits:  "a pending merge request",
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
			"action": enum("request_changes sends the card back to its agent, reject closes the pull request and fails the card", "request_changes", "reject")},
		required: []string{"task_id", "note", "action"},
		about:    "Deny a card's waiting merge request with the member's reason.",
		fits:     "a pending merge request",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			q, err := t.mergeRequest(in)
			if err != nil {
				return "", nil, err
			}
			note, err := t.c.doDeny(t.r.Context(), t.a, q.ID, in.Action, in.Note)
			return note, nil, err
		}},
	{name: "set_summary", title: "Set a card's summary", write: true,
		params:   map[string]any{"task_id": taskParam, "summary": str("one line of up to 255 characters; empty restores the generated title")},
		required: []string{"task_id", "summary"},
		about:    "Set the one-line summary a card's face shows instead of its generated title.",
		fits:     "any card",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			was, err := t.c.doSetSummary(t.r.Context(), t.a, in.TaskID, in.Summary)
			return in.setNote("summary", was.IssueNumber, in.Summary), &inverse{"set_summary", toolInput{TaskID: in.TaskID, Summary: was.HumanSummary}}, err
		}},
	{name: "set_accent", title: "Set a card's accent", write: true,
		params:   map[string]any{"task_id": taskParam, "accent": enum("the colour; empty removes it", append([]string{""}, factoryd.Accents...)...)},
		required: []string{"task_id", "accent"},
		about:    "Set a card's colour label, which shows as a chip and its left edge.",
		fits:     "any card",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			was, err := t.c.doSetAccent(t.r.Context(), t.a, in.TaskID, in.Accent)
			return in.setNote("accent", was.IssueNumber, in.Accent), &inverse{"set_accent", toolInput{TaskID: in.TaskID, Accent: was.Accent}}, err
		}},
	{name: "invite", title: "Invite a member", write: true,
		params: map[string]any{"login": str("the Forgejo user name")}, required: []string{"login"},
		about: "Make a Forgejo user a member of the project: a write collaborator of its repository, whom the console admits within a minute (an approver's action).",
		fits:  "an approver of the project",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doInvite(t.r.Context(), t.a, in.Login)
			return note, nil, err
		}},
	{name: "github_export", title: "Export to GitHub", write: true,
		params: map[string]any{"remote": str("the GitHub repository, owner/name"), "username": str("the GitHub login the token belongs to"),
			"token": str("a GitHub token that may push to the repository; the member gives it")},
		required: []string{"remote", "username", "token"},
		about:    "Make Forgejo push the project's repository to a GitHub repository on every commit and every 8 h, in place of the project's last export (an approver's action). The token is never shown or stored in the conversation.",
		fits:     "a factory project, for an approver",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doGithubExport(t.r.Context(), t.a, t.p, in.Remote, in.Username, in.Token)
			return note, nil, err
		}},
	{name: "close", title: "Close a card", write: true,
		params: map[string]any{"task_id": taskParam, "note": str("why, commented on the issue before it closes; optional")}, required: []string{"task_id"},
		about: "Close a card's issue as the member, the note commented first when given; the factory ends the card.",
		fits:  "a card that is not final",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doClose(t.r.Context(), t.a, in.TaskID, in.Note)
			return note, nil, err
		}},
	{name: "edit", title: "Edit a card", write: true,
		params: map[string]any{"task_id": taskParam, "title": str("the new title, at most 255 characters; empty keeps it"),
			"body": str("the new description in Markdown, replacing the old; empty keeps it")},
		required: []string{"task_id"},
		about:    "Edit a card's issue as the member, its title, its description or both; the card's next session reads the edit.",
		fits:     "a card not in a session",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doEdit(t.r.Context(), t.a, in.TaskID, in.Title, in.Body)
			return note, nil, err
		}},
	{name: "grant_session", title: "Grant a session", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Grant a card at its session cap one more session: the member's answer to its findings, a comment on its issue, which resumes it.",
		fits:     "a card at its session cap",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doGrantSession(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "set_dependency", title: "Set a dependency", write: true,
		params: map[string]any{"task_id": taskParam, "depends_on": str("the task id of the card it waits for"),
			"remove": map[string]any{"type": "boolean", "description": "true removes the dependency"}},
		required: []string{"task_id", "depends_on"},
		about:    "Make a card that has not started wait for another card of the project until that one is done, or with remove no longer; its issue shows the dependency.",
		fits:     "two cards of which the first has not started",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doSetDependency(t.r.Context(), t.a, in.TaskID, in.DependsOn, in.Remove)
			return note, nil, err
		}},
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

// offered is chatTools, set in init: tools' system entry reads it, which
// a direct reference would make an initialization cycle.
var offered func(*Console) []chatTool

func init() {
	offered = (*Console).chatTools
}

func (t *turn) systemText() (string, error) {
	v, ok := t.c.inForce(t.r.Context())
	if !ok {
		return "", refuse(http.StatusInternalServerError, "no policy in force")
	}
	var b strings.Builder
	b.WriteString("Signals and members file cards; a card runs agent sessions in a sandbox; its pull request passes the gates (checks, policy, review, audit, approval), merges and deploys; a member steers through the chat, and every tool here is one of the console's actions.\n\n")
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
	b.WriteString("\nTools:\n")
	confirmSet := make(map[string]bool)
	for _, c := range t.cfg.Confirm {
		confirmSet[c] = true
	}
	for _, tool := range offered(t.c) {
		line := "  " + tool.name
		if confirmSet[tool.name] {
			line += ", needs OK"
		}
		if tool.fits != "" {
			line += ", " + tool.fits
		}
		b.WriteString(line + "\n")
	}
	confirm := "none"
	if len(t.cfg.Confirm) > 0 {
		confirm = strings.Join(t.cfg.Confirm, ", ")
	}
	fmt.Fprintf(&b, "\nTools needing OK: %s\n", confirm)
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
	budget := v.Tree.Document.Budget
	fmt.Fprintf(&b, "  task budget: €%.2f default, €%.2f maximum\n", budget.TaskDefaultEUR, budget.TaskMaxEUR)
	if _, eur := v.ProjectCap(t.p.Repository); !math.IsInf(eur, 1) {
		fmt.Fprintf(&b, "  project month cap: €%.2f\n", eur)
	}
	fmt.Fprintf(&b, "  sessions per card: %d\n", v.Tree.Document.Limits.Sessions)
	fmt.Fprintf(&b, "  agent time: %s\n", v.Tree.Document.Limits.AgentTime)
	fmt.Fprintf(&b, "\nChat bounds:\n  rounds per turn: %d\n  message size: %d KiB\n", chatRounds, chatMessageMax>>10)
	return b.String(), nil
}
