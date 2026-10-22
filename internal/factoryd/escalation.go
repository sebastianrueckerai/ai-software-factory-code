package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// EscalationStore is what the escalation endpoint reads and records.
type EscalationStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
}

// SessionEscalator hands an escalation to a task's running session; nil
// means the session took it.
type SessionEscalator interface {
	EscalateSession(ctx context.Context, taskID string, ask AgentAsk) error
}

// AgentAsk is an agent's escalate_to_human (Kind AskEscalation: the
// question, why, and what the agent would do, ruling 149, and the answers
// it offers as Choices, ruling 256), request_info (AskInfo: the question
// for the issue's author, what it tried, what it needs, ruling 150) or
// request_tool (AskTool: the tool the sandbox lacks and what for).
type AgentAsk struct {
	Kind           string   `json:"kind"`
	Question       string   `json:"question"`
	Reason         string   `json:"reason"`
	Recommendation string   `json:"recommendation"`
	Choices        []string `json:"choices,omitempty"`
	Tried          string   `json:"tried"`
	Needs          string   `json:"needs"`
	Tool           string   `json:"tool,omitempty"`
	Need           string   `json:"need,omitempty"`
}

// AgentAsk kinds.
const (
	AskEscalation = "escalation"
	AskInfo       = "info_request"
	AskTool       = "missing_tool"
)

// maxTool and maxNeed bound a tool request's tool and need, in
// characters, as the gateway does.
const (
	maxTool = 80
	maxNeed = 600
)

// validate checks the fields of its kind, as the gateway does.
func (a AgentAsk) validate() error {
	if a.Kind == AskTool {
		if n, m := utf8.RuneCountInString(a.Tool), utf8.RuneCountInString(a.Need); n == 0 || n > maxTool || m == 0 || m > maxNeed {
			return fmt.Errorf("tool (1 to %d characters) and need (1 to %d characters) are required", maxTool, maxNeed)
		}
		return nil
	}
	fields := map[string]int{"question": len(a.Question)}
	switch a.Kind {
	case AskEscalation:
		fields["reason"], fields["recommendation"] = len(a.Reason), len(a.Recommendation)
	case AskInfo:
		fields["tried"], fields["needs"] = len(a.Tried), len(a.Needs)
	default:
		return fmt.Errorf("kind %q: want %s, %s or %s", a.Kind, AskEscalation, AskInfo, AskTool)
	}
	for name, n := range fields {
		limit := maxEscalationText
		if name == "recommendation" {
			limit = maxRecommendation
		}
		if n == 0 || n > limit {
			return fmt.Errorf("%s is required, at most %d bytes", name, limit)
		}
	}
	return nil
}

// reason is the ask as the workflow carries it: an escalation's reason
// (its first line the question, escalationQuestion), the information
// request's text, or a tool request as a run from before the tool park
// asks a human.
func (a AgentAsk) reason() string {
	switch a.Kind {
	case AskInfo:
		return agentAsksAuthor + a.Question + "\nTried: " + a.Tried + "\nNeeds: " + a.Needs
	case AskTool:
		return AgentAsks + "a sandbox with " + a.Tool + "\nWhy: " + a.Need
	}
	return AgentAsks + a.Question + "\nWhy: " + a.Reason + "\nRecommendation: " + a.Recommendation
}

// IntakeAgentEscalation is the intake event of an agent's escalate_to_human,
// IntakeAgentInfoRequest of its request_info, IntakeAgentToolRequest of its
// request_tool: recorded, never signalled (the session's supervision acts
// on them).
const (
	IntakeAgentEscalation  = "agent_escalation"
	IntakeAgentInfoRequest = "agent_info_request"
	IntakeAgentToolRequest = "agent_tool_request"
)

// IntakeEscalation records an escalation's issue comment (its CommentID)
// and reason: human answers are the trusted issue comments newer than it.
// IntakeInfoRequest records an information request's comment: the issue
// author's comments newer than it answer it (ruling 150).
const (
	IntakeEscalation  = "escalation"
	IntakeInfoRequest = "info_request"
)

// IntakeEscalationWithdrawn records the withdrawal of an escalation
// (versionBesideLadder): CommentID the withdrawal's comment, Withdraws
// the escalation's, Body why. Comments after the withdrawal's answer the
// escalation no more (answerOf).
const IntakeEscalationWithdrawn = "escalation_withdrawn"

// IntakePosterComment is a comment on the issue by its author when they
// cannot write the repository: signalled as information for an
// information request, never an escalation's answer (ruling 150).
const IntakePosterComment = "poster_comment"

// IntakeFactoryClosed records, before factoryd closes a task's issue as the
// bot (CommentID the issue number), that the close is its own: only such a
// bot close ends a task (review-task73 m8).
const IntakeFactoryClosed = "factory_closed"

// IntakeHandover claims a card's hand-over to the factory (the go
// action): one per card, so of two racing hands only the first files.
const IntakeHandover = "handover"

// botCloseRecorded reports whether events hold factoryd's record of
// closing t's issue itself.
func botCloseRecorded(events []IntakeEvent, t Task) bool {
	return slices.ContainsFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeFactoryClosed && e.CommentID == t.IssueNumber })
}

// closeIssueAsFactory closes t's issue as the bot, the one way factoryd
// closes a card's issue: it records the close as its own with comment
// (IntakeFactoryClosed), posts comment unless it is "", closes the issue
// and confirms the record. A step that fails returns, the record left
// unconfirmed for the next call or the heal; only a comment Forgejo
// refuses (forgejoTerminal) is logged, and the close goes on. A call that
// finds the record posts the recorded comment, not its own, where the
// issue lacks it (CommentOnce): one close, one comment.
func closeIssueAsFactory(ctx context.Context, store interface {
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
	ConfirmFactoryClose(context.Context, string, int64) error
}, forgejo *Forgejo, t Task, comment string) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	id, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: t.IssueNumber, Body: comment})
	if err != nil {
		return err
	}
	if id == 0 {
		events, err := store.IntakeEvents(ctx, t.ID)
		if err != nil {
			return err
		}
		comment = ""
		if i := slices.IndexFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeFactoryClosed && e.CommentID == t.IssueNumber }); i >= 0 {
			comment = events[i].Body
		}
	}
	switch {
	case comment == "":
	case id > 0:
		_, err = forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, comment)
	default:
		err = forgejo.CommentOnce(ctx, repo.Owner, repo.Name, t.IssueNumber, comment)
	}
	if forgejoTerminal(err) {
		log.Printf("task %s: issue #%d closes without its comment, which Forgejo refused: %v", t.ID, t.IssueNumber, err)
	} else if err != nil {
		return err
	}
	if err := forgejo.CloseIssue(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil {
		return err
	}
	return store.ConfirmFactoryClose(ctx, t.ID, t.IssueNumber)
}

// maxEscalationText bounds the question, the reason and an information
// request's fields, maxRecommendation the recommendation, as the gateway
// does.
const (
	maxEscalationText = 4000
	maxRecommendation = 2000
)

// MaxChoices bounds the answers an agent may offer, MaxChoice the
// characters of one.
const (
	MaxChoices = 5
	MaxChoice  = 60
)

// offeredChoices are the agent's choices as a card offers them, each
// trimmed: 2 to 5, none empty or over 60 characters. Any other list is
// logged and dropped, never refused: the question stands without it.
func offeredChoices(taskID string, choices []string) []string {
	if len(choices) == 0 {
		return nil
	}
	out := make([]string, len(choices))
	for i, c := range choices {
		out[i] = strings.TrimSpace(c)
	}
	if len(out) < 2 || len(out) > MaxChoices || slices.ContainsFunc(out, func(c string) bool { return c == "" || utf8.RuneCountInString(c) > MaxChoice }) {
		log.Printf("task %s: the agent's %d choices are dropped, the card offers 2 to %d of 1 to %d characters: %s",
			taskID, len(choices), MaxChoices, MaxChoice, textutil.Truncate(fmt.Sprintf("%q", choices), 400))
		return nil
	}
	return out
}

// EscalationHandler is POST /internal/escalations, the gateway's hand-over
// of an agent's escalate_to_human, request_info or request_tool (behind
// the gateway's bearer): the task must be running and its session take the
// ask, which is then recorded as an intake event; the session stops as on
// a supervisor's escalate: work in progress pushed, the question on the
// issue, and the human's (or for request_info the issue author's) answer
// resumes it, or for request_tool the image's arrival.
func EscalationHandler(store EscalationStore, sessions SessionEscalator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TaskID string `json:"task_id"`
			AgentAsk
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		ask := in.AgentAsk
		for _, f := range []*string{&ask.Question, &ask.Reason, &ask.Recommendation, &ask.Tried, &ask.Needs, &ask.Tool, &ask.Need} {
			*f = strings.TrimSpace(*f)
		}
		if in.TaskID == "" {
			http.Error(w, "task_id is required", http.StatusBadRequest)
			return
		}
		if err := ask.validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if ask.Kind == AskEscalation {
			ask.Choices = offeredChoices(in.TaskID, ask.Choices)
		} else {
			ask.Choices = nil // no human picks: the issue's author answers on Forgejo, a tool request waits for its image
		}
		t, ok, err := store.Task(r.Context(), in.TaskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no task "+in.TaskID, http.StatusNotFound)
			return
		}
		if t.Status != "running" {
			http.Error(w, "task "+in.TaskID+" is "+t.Status, http.StatusConflict)
			return
		}
		if err := sessions.EscalateSession(r.Context(), t.ID, ask); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrNoLiveSession) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		kind, body := IntakeAgentEscalation, ask.Question+"\n\nWhy: "+ask.Reason+"\n\nRecommendation: "+ask.Recommendation
		switch ask.Kind {
		case AskInfo:
			kind, body = IntakeAgentInfoRequest, ask.Question+"\n\nTried: "+ask.Tried+"\n\nNeeds: "+ask.Needs
		case AskTool:
			kind, body = IntakeAgentToolRequest, ask.Tool+"\n\nNeed: "+ask.Need
		}
		log.Printf("task %s: the agent asked (%s): %s", t.ID, ask.Kind, textutil.Truncate(cmp.Or(ask.Question, ask.Tool), 200))
		// the session already took it: a failed record is logged, not undone
		if _, err := store.InsertIntakeEvent(context.WithoutCancel(r.Context()), IntakeEvent{
			TaskID: t.ID, Kind: kind, Author: "agent", Body: body, Choices: ask.Choices,
		}); err != nil {
			log.Printf("task %s: record the agent escalation: %v", t.ID, err)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
