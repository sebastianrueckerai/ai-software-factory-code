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
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// AgentAsk is an agent's escalate (Kind AskEscalation: the
// question, why, and what the agent would do, and the answers
// it offers as Choices), request_info (AskInfo: the question
// for the issue's author, what it tried, what it needs),
// request_tool (AskTool: the tool the sandbox lacks and what for) or
// request_cut (AskCut: why the card is too big, and the Parts it
// proposes).
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
	Parts          []string `json:"parts,omitempty"`
}

// AgentAsk kinds. AskReplan is a person's stop of the running session
// for the card's replan (Reason: the text for the planner); only factoryd
// sends it, so the endpoint refuses it.
const (
	AskEscalation = "escalation"
	AskInfo       = "info_request"
	AskTool       = "missing_tool"
	AskCut        = "cut"
	AskReplan     = "replan"
)

// maxTool and maxNeed bound a tool request's tool and need, in
// characters, as the gateway does.
const (
	maxTool = 80
	maxNeed = 600
)

// validate checks the fields of its kind, as the gateway does.
func (a AgentAsk) validate() error {
	switch a.Kind {
	case AskTool:
		if n, m := utf8.RuneCountInString(a.Tool), utf8.RuneCountInString(a.Need); n == 0 || n > maxTool || m == 0 || m > maxNeed ||
			strings.ContainsFunc(a.Tool+a.Need, unicode.IsControl) {
			return fmt.Errorf("tool (1 to %d characters) and need (1 to %d characters) are required, each on one line without control characters", maxTool, maxNeed)
		}
		return nil
	case AskCut:
		if refusal := policy.CutAskRefusal(a.Reason, a.Parts); refusal != "" {
			return errors.New(refusal)
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
		return fmt.Errorf("kind %q: want %s, %s, %s or %s", a.Kind, AskEscalation, AskInfo, AskTool, AskCut)
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
// request's text, a tool request as a run from before the tool park
// asks a human, or a person's replan's own reason.
func (a AgentAsk) reason() string {
	switch a.Kind {
	case AskReplan:
		return a.Reason
	case AskInfo:
		return agentAsksAuthor + a.Question + "\nTried: " + a.Tried + "\nNeeds: " + a.Needs
	case AskTool:
		return AgentAsks + "a sandbox with " + a.Tool + "\nWhy: " + a.Need
	}
	return AgentAsks + a.Question + "\nWhy: " + a.Reason + "\nRecommendation: " + a.Recommendation
}

// IntakeAgentEscalation is the intake event of an agent's escalate,
// IntakeAgentInfoRequest of its request_info, IntakeAgentToolRequest of its
// request_tool, IntakeAgentCut of its request_cut: recorded, never
// signalled (the session's supervision acts on them).
const (
	IntakeAgentEscalation  = "agent_escalation"
	IntakeAgentInfoRequest = "agent_info_request"
	IntakeAgentToolRequest = "agent_tool_request"
	IntakeAgentCut         = "agent_cut"
)

// IntakeEscalation records an escalation's issue comment (its CommentID)
// and reason: human answers are the trusted issue comments newer than it.
// IntakeInfoRequest records an information request's comment: the issue
// author's comments newer than it answer it.
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
// information request, never an escalation's answer.
const IntakePosterComment = "poster_comment"

// IntakeFactoryClosed records, before factoryd closes a task's issue as the
// bot (CommentID the issue number), that the close is its own: only such a
// bot close ends a task (review-task73 m8).
const IntakeFactoryClosed = "factory_closed"

// IntakeUntrustedClose records a close of a task's issue by a user who
// cannot write the repository, keyed by the close's time (CommentID, in
// microseconds since the epoch): never signalled, and recorded once per
// close so the heal pass leaves it alone.
const IntakeUntrustedClose = "untrusted_close"

// IntakeHandover claims a card's hand-over to the factory (the go
// action): one per card, so of two racing hands only the first files.
const IntakeHandover = "handover"

// botCloseRecorded reports whether events hold factoryd's record of
// closing t's issue itself.
func botCloseRecorded(events []IntakeEvent, t Task) bool {
	return slices.ContainsFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeFactoryClosed && e.CommentID == t.IssueNumber })
}

// DependencyEdges are the Forgejo calls that read and remove an issue's
// dependencies: Forgejo itself, or a human's session.
type DependencyEdges interface {
	IssueDependencies(ctx context.Context, owner, repo string, number int64) ([]IssueDependency, error)
	RemoveIssueDependency(ctx context.Context, repo Repo, number int64, dep IssueDependency) error
}

// DropOpenDependencies removes the open issues issue number of repo depends
// on, which Forgejo refuses to close it over, and answers their numbers.
// The edges other issues hold on it stay. A repository with issue
// dependencies switched off answers 404: none.
func DropOpenDependencies(ctx context.Context, e DependencyEdges, repo Repo, number int64) ([]int64, error) {
	deps, err := e.IssueDependencies(ctx, repo.Owner, repo.Name, number)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var freed []int64
	for _, d := range deps {
		if d.State != "open" {
			continue
		}
		if err := e.RemoveIssueDependency(ctx, repo, number, d); err != nil {
			return nil, &DependencyRefusal{Dep: d, Err: err}
		}
		freed = append(freed, d.Number)
	}
	return freed, nil
}

// DependencyRefusal is the removal of dependency Dep that failed.
type DependencyRefusal struct {
	Dep IssueDependency
	Err error
}

func (e *DependencyRefusal) Error() string {
	return fmt.Sprintf("remove its dependency on %s/%s#%d: %v", e.Dep.Repo.Owner, e.Dep.Repo.Name, e.Dep.Number, e.Err)
}

func (e *DependencyRefusal) Unwrap() error { return e.Err }

// NoLongerWaits is the close note's sentence naming the dependencies a close
// removed: "It no longer waits on #1659 and #1541."
func NoLongerWaits(numbers []int64) string {
	names := make([]string, len(numbers))
	for i, n := range numbers {
		names[i] = fmt.Sprintf("#%d", n)
	}
	return "It no longer waits on " + textutil.AndList(names) + "."
}

// closeIssueAsFactory closes t's issue as the bot, the one way factoryd
// closes a card's issue. It first drops the issue's dependencies on open
// issues (an ending card waits on nothing, and Forgejo refuses the close
// while one stands; the store's edges order the cards, the issues only
// mirror them); a failed read or removal returns before anything is
// written. It records the close as its own with comment
// (IntakeFactoryClosed), the sentence naming what it dropped added to it, so
// an empty comment posts that sentence alone, then posts comment unless it
// is "", closes the issue and confirms the record. A later step that fails
// returns, the record left unconfirmed for the next call or the heal; only a
// comment Forgejo refuses (forgejoTerminal) is logged, and the close goes
// on. A call that finds the record posts the recorded comment, not its own,
// where the issue lacks it (CommentOnce): one close, one comment. The
// sentence names what this call dropped: a retry after a failed record or a
// partly done removal finds those edges gone and does not name them, and the
// issue's timeline in Forgejo shows each removal.
func closeIssueAsFactory(ctx context.Context, store interface {
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
	ConfirmFactoryClose(context.Context, string, int64) error
}, forgejo *Forgejo, t Task, comment string) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	freed, err := DropOpenDependencies(ctx, forgejo, repo, t.IssueNumber)
	if err != nil {
		return err
	}
	if len(freed) > 0 {
		comment = strings.TrimSpace(comment + "\n\n" + NoLongerWaits(freed))
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
// of an agent's escalate, request_info, request_tool or
// request_cut (behind the gateway's bearer): the task must be running and
// its session take the ask, which is then recorded as an intake event; the
// session stops as on a supervisor's escalate: work in progress pushed,
// the question on the issue, and the human's (or for request_info the
// issue author's) answer resumes it, or for request_tool the image's
// arrival. A cut does not stop the session: its check-in judges it.
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
		for i := range ask.Parts {
			ask.Parts[i] = strings.TrimSpace(ask.Parts[i])
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
			ask.Choices = nil // no human picks: the issue's author answers on Forgejo, a tool request waits for its image, the check-in judges a cut
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
			switch {
			case errors.Is(err, ErrNoLiveSession):
				status = http.StatusConflict
			case errors.Is(err, ErrNoImageWait), errors.Is(err, ErrNoCut):
				status = http.StatusUnprocessableEntity
			}
			http.Error(w, err.Error(), status)
			return
		}
		kind, body := IntakeAgentEscalation, ask.Question+"\n\nWhy: "+ask.Reason+"\n\nRecommendation: "+ask.Recommendation
		asked := cmp.Or(ask.Question, ask.Tool)
		switch ask.Kind {
		case AskInfo:
			kind, body = IntakeAgentInfoRequest, ask.Question+"\n\nTried: "+ask.Tried+"\n\nNeeds: "+ask.Needs
		case AskTool:
			kind, body = IntakeAgentToolRequest, ask.Tool+"\n\nNeed: "+ask.Need
		case AskCut:
			kind, body = IntakeAgentCut, ask.Reason+"\n\nParts:\n- "+strings.Join(ask.Parts, "\n- ")
			asked = fmt.Sprintf("%d parts: %s", len(ask.Parts), ask.Reason)
		}
		log.Printf("task %s: the agent asked (%s): %s", t.ID, ask.Kind, textutil.Truncate(asked, 200))
		// the session already took it: a failed record is logged, not undone
		if _, err := store.InsertIntakeEvent(context.WithoutCancel(r.Context()), IntakeEvent{
			TaskID: t.ID, Kind: kind, Author: "agent", Body: body, Choices: ask.Choices,
		}); err != nil {
			log.Printf("task %s: record the agent escalation: %v", t.ID, err)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
