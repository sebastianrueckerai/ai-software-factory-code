package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	TaskID   string `json:"task_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	Lane     string `json:"lane,omitempty"`
	Text     string `json:"text,omitempty"`
	Note     string `json:"note,omitempty"`
	Action   string `json:"action,omitempty"`
	Revision int    `json:"revision,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Accent   string `json:"accent,omitempty"`
	N        int    `json:"n,omitempty"`
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

func enum(about string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": about, "enum": values}
}

var (
	taskParam = str("the card's task id, t-...")
	nParam    = map[string]any{"type": "integer", "description": "how many, 1 to 100 (default 20)", "minimum": 1, "maximum": 100}
	textParam = str("the text, as the member writes it")
)

// chatTools are the tools the agent is offered, in policy.ChatTools'
// order: steer only where factoryd can reach a session.
func (c *Console) chatTools() []chatTool {
	return slices.DeleteFunc(slices.Clone(tools), func(t chatTool) bool { return t.internal || t.name == "steer" && c.cfg.Steer == nil })
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
	for _, f := range []struct{ key, v string }{{"title", in.Title}, {"body", in.Body}, {"lane", in.Lane}, {"action", in.Action},
		{"summary", in.Summary}, {"accent", in.Accent}, {"text", in.Text}, {"note", in.Note}} {
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
	s := fmt.Sprintf("#%d %s %q %s", t.IssueNumber, t.ID, cardTitle(t), t.Status)
	if t.Gate != "" {
		s += ", gate " + t.Gate
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
		return factoryd.ApprovalRequest{}, refuse(http.StatusConflict, fmt.Sprintf("Card #%d has no merge request waiting.", k.IssueNumber))
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
	{name: "board", title: "Read the board",
		about: "The project's board as the member sees it: each column with its cards (issue, task id, title, status, gate, spend of budget), the human lane, the escalated cards and the approval requests waiting.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, _, err := t.c.boardText(t.r, t.p)
			return s, nil, err
		}},
	{name: "card", title: "Read a card", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "One card in detail: status, lane, gate, spend, its issue and pull request, the objective, the agent's summary, the question it waits on with the choices it offers, its merge request's reviews, its plan's revision and cards, its plan.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.cardText(in); return s, nil, err }},
	{name: "history", title: "Read the history", params: map[string]any{"n": nParam},
		about: "The project's newest events, newest first: what was done to which card, by whom and through what (button, chat, factoryd, webhook).",
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
		about: "The project's spend this UTC month against its month cap, the cards' and the chat's apart.",
		run: func(t *turn, _ toolInput) (string, *inverse, error) {
			s, err := t.c.spendLine(t.r.Context(), t.p)
			return "The project's " + s + ".", nil, err
		}},
	{name: "diff", title: "Read a pull request's diff", params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "The diff of a card's pull request, read from Forgejo as the member; its first 20 KB.",
		run:   func(t *turn, in toolInput) (string, *inverse, error) { s, err := t.diff(in); return s, nil, err }},
	{name: "new_card", title: "File a card", write: true,
		params: map[string]any{"title": str("one line, at most 255 characters"), "body": str("the description, Markdown"),
			"lane": enum("where the card starts: backlog to wait, plan for a spec to plan, ready for an agent to work on now, human for a human", "backlog", "plan", "ready", "human")},
		required: []string{"title", "lane"},
		about:    "File a new card on the project as the member: an issue with the lane's label, which the factory takes in.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			repo, issue, err := t.c.doFileCard(t.r.Context(), t.a, t.p.Name, in.Title, in.Body, in.Lane, "", "")
			return fmt.Sprintf("Filed %s#%d in lane %s.", repo, issue, in.Lane), nil, err
		}},
	{name: "move_card", title: "Move a card", write: true,
		params: map[string]any{"task_id": taskParam, "lane": enum("the starting lane", policy.LanePlan, policy.LaneReady)}, required: []string{"task_id", "lane"},
		about: "Move a backlog card into a starting lane of its process, as the member's label on its issue.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doMoveCard(t.r.Context(), t.a, in.TaskID, in.Lane)
			return note, &inverse{"unmove_card", toolInput{TaskID: in.TaskID, Lane: in.Lane}}, err
		}},
	{name: "answer", title: "Answer a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Answer the question a card waits on (it needs you), with one of the choices the card offers word for word or in free text: a comment on its issue as the member, which resumes it.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doAnswer(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "comment", title: "Comment on a card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Comment on a card's issue as the member.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			posted, note, err := t.c.doComment(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, &inverse{"delete_comment", toolInput{TaskID: in.TaskID, N: int(posted)}}, err
		}},
	{name: "steer", title: "Message a running card", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Send a message to a running card's agent session, marked as the member's; the agent reads it next.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doSteer(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "retry", title: "Retry a card", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "File a failed card of a plan again and start the replacement in lane ready (an approver's action).",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRetry(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "start_plan", title: "Start a plan", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Start an accepted plan: its cards still in the backlog go to lane ready (an approver's action).",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doStartPlan(t.r.Context(), t.a, in.TaskID)
			return note, nil, err
		}},
	{name: "accept_plan", title: "Accept a plan", write: true,
		params: map[string]any{"task_id": taskParam, "revision": map[string]any{"type": "integer", "description": "the plan revision the member saw", "minimum": 1},
			"sha256": str("that revision's sha256, from the card tool")},
		required: []string{"task_id", "revision", "sha256"},
		about:    "Accept a plan's revision, which factoryd then files as cards (an approver's action).",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doAcceptPlan(t.r.Context(), t.a, in.TaskID, in.Revision, in.SHA256)
			return note, nil, err
		}},
	{name: "request_changes", title: "Ask for changes to a plan", write: true, params: map[string]any{"task_id": taskParam, "text": textParam},
		required: []string{"task_id", "text"},
		about:    "Ask the planner for changes to a plan: a comment on its issue as the member.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRequestChanges(t.r.Context(), t.a, in.TaskID, in.Text)
			return note, nil, err
		}},
	{name: "reject_plan", title: "Reject a plan", write: true, params: map[string]any{"task_id": taskParam, "note": str("why")},
		required: []string{"task_id", "note"},
		about:    "Reject a plan: its task ends failed with the reason.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			note, err := t.c.doRejectPlan(t.r.Context(), t.a, in.TaskID, in.Note)
			return note, nil, err
		}},
	{name: "approve_merge", title: "Approve a merge", write: true, params: map[string]any{"task_id": taskParam}, required: []string{"task_id"},
		about: "Approve a card's waiting merge request: the member's approving review on its pull request, then factoryd merges it (an approver's action).",
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
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			was, err := t.c.doSetSummary(t.r.Context(), t.a, in.TaskID, in.Summary)
			return in.setNote("summary", was.IssueNumber, in.Summary), &inverse{"set_summary", toolInput{TaskID: in.TaskID, Summary: was.HumanSummary}}, err
		}},
	{name: "set_accent", title: "Set a card's accent", write: true,
		params:   map[string]any{"task_id": taskParam, "accent": enum("the colour; empty removes it", append([]string{""}, factoryd.Accents...)...)},
		required: []string{"task_id", "accent"},
		about:    "Set a card's colour label, which shows as a chip and its left edge.",
		run: func(t *turn, in toolInput) (string, *inverse, error) {
			was, err := t.c.doSetAccent(t.r.Context(), t.a, in.TaskID, in.Accent)
			return in.setNote("accent", was.IssueNumber, in.Accent), &inverse{"set_accent", toolInput{TaskID: in.TaskID, Accent: was.Accent}}, err
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

// cardText is the card tool's answer.
func (t *turn) cardText(in toolInput) (string, error) {
	ctx := t.r.Context()
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	bt, _, err := t.c.cfg.Tasks.BoardTask(ctx, k.ID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(cardLine(k))
	if k.Lane != "" {
		fmt.Fprintf(&b, "\nlane %s of process %s", k.Lane, k.Process)
	}
	if k.Accent != "" {
		b.WriteString(", accent " + k.Accent)
	}
	base := strings.TrimSuffix(t.c.cfg.ForgejoPublicURL, "/") + "/" + k.Repository
	if k.IssueNumber > 0 {
		fmt.Fprintf(&b, "\nissue %s/issues/%d", base, k.IssueNumber)
	}
	if k.PRNumber > 0 {
		fmt.Fprintf(&b, "\npull request %s/pulls/%d", base, k.PRNumber)
	}
	for _, f := range []struct{ name, v string }{{"waiting on", bt.WaitingOn}, {"depends on", bt.Dependencies},
		{"the question it waits on", bt.Escalation}, {"the choices it offers", numbered(bt.Choices)}, {"objective", k.Objective}, {"the agent's summary", k.AgentSummary}} {
		if f.v != "" {
			fmt.Fprintf(&b, "\n%s: %s", f.name, textutil.Truncate(f.v, 2000))
		}
	}
	if k.Parent != "" {
		if p, ok, err := t.c.cfg.Tasks.Task(ctx, k.Parent); err == nil && ok && p.Repository == t.p.Repository {
			b.WriteString("\npart of plan " + cardLine(p))
		}
	}
	if k.Lane == policy.LanePlan {
		if rev, ok, err := t.c.cfg.Tasks.PlanRevision(ctx, k.ID, 0); err == nil && ok {
			fmt.Fprintf(&b, "\nplan revision %d, sha256 %s", rev.Revision, rev.SHA256)
		}
		cards, err := t.c.cfg.Tasks.PlanCards(ctx, k.ID)
		if err != nil {
			return "", err
		}
		for _, card := range cards {
			b.WriteString("\n  plan card " + cardLine(card))
		}
	}
	all, err := t.c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		return "", err
	}
	for _, q := range all {
		if q.TaskID != k.ID || q.Project != t.p.Repository {
			continue
		}
		fmt.Fprintf(&b, "\n%s request %d waiting", q.Kind, q.ID)
		var spec factoryd.MergeSpec
		if q.Kind == factoryd.ApprovalMerge && json.Unmarshal(q.Spec, &spec) == nil {
			fmt.Fprintf(&b, " for pull request #%d, risk %s; the model review: %s, %s", q.PRNumber, spec.RiskClass, spec.Review.Verdict, spec.Review.Summary)
			if len(spec.WhyHuman) > 0 {
				b.WriteString("; a human decides because " + strings.Join(spec.WhyHuman, ", "))
			}
			if spec.Recommendation != "" {
				b.WriteString("; recommendation: " + spec.Recommendation)
			}
		}
	}
	return b.String(), nil
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
		return string(raw[:diffLimit]) + fmt.Sprintf("\n… the diff goes on for %d more bytes", len(raw)-diffLimit), nil
	}
	return string(raw), nil
}
