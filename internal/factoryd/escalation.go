package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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
	// AdmitAsk passes an escalation's or an information request's
	// question and reason on card t through the filing door as an ask: a
	// *BriefRefusal goes back to the agent.
	AdmitAsk(ctx context.Context, t Task, ask AgentAsk) error
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
	// Choice is the number from 1 of the choice the agent recommends, 0
	// for none.
	Choice int      `json:"choice,omitempty"`
	Tried  string   `json:"tried"`
	Needs  string   `json:"needs"`
	Tool   string   `json:"tool,omitempty"`
	Need   string   `json:"need,omitempty"`
	Parts  []string `json:"parts,omitempty"`
}

// AgentAsk kinds. AskReplan is a person's stop of the running session
// for the card's replan (Reason: the text for the planner), AskHold a
// human's stop of it for the card's hold; only factoryd sends them, so
// the endpoint refuses them.
const (
	AskEscalation = "escalation"
	AskInfo       = "info_request"
	AskTool       = "missing_tool"
	AskCut        = "cut"
	AskReplan     = "replan"
	AskHold       = "hold"
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
	if faults := GroundsFaults(a.Recommendation); len(faults) > 0 {
		return fmt.Errorf("recommendation: %s", strings.Join(faults, " "))
	}
	return nil
}

// reason is the ask as the workflow carries it: an escalation's reason
// (its first line the question, escalationQuestion), the information
// request's text, a tool request as a run from before the tool park
// asks a human, or a person's replan's or a human's hold's own reason.
func (a AgentAsk) reason() string {
	switch a.Kind {
	case AskReplan, AskHold:
		return a.Reason
	case AskInfo:
		return InfoRequestReason(a.Question, a.Tried, a.Needs)
	case AskTool:
		return AgentAsks + "a sandbox with " + a.Tool + "\nWhy: " + a.Need
	}
	pick := ""
	if a.Choice >= 1 && a.Choice <= len(a.Choices) {
		pick = fmt.Sprintf("\nChoice: %d (%s)", a.Choice, a.Choices[a.Choice-1])
	}
	return AgentAsks + a.Question + pick + "\nWhy: " + a.Reason + "\nRecommendation: " + a.Recommendation
}

// InfoRequestReason is the reason of the agent's request for information:
// its question to the issue's author, what it tried and what it needs.
func InfoRequestReason(question, tried, needs string) string {
	return agentAsksAuthor + question + "\nTried: " + tried + "\nNeeds: " + needs
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

// IntakeInfoRequest records an information request's comment: the issue
// author's comments newer than it answer it.
const IntakeInfoRequest = "info_request"

// IntakePosterComment is a comment on the issue by its author when they
// cannot write the repository: signalled as information for an
// information request, never an escalation's answer.
const IntakePosterComment = "poster_comment"

// IntakeFactoryClosed records, before factoryd closes a task's issue as the
// bot (CommentID the issue number), that the close is its own: only such a
// bot close ends a task (review-task73 m8).
const IntakeFactoryClosed = "factory_closed"

// IntakeCloseAnswer records, before a close answer closes a card's issue
// or signals its run, why the answer closes it: Author the answerer, Body
// answerReason's words. The end at closed reads it (closedWhy), so an end
// the close drives ahead of the answer's own rows still says why.
const IntakeCloseAnswer = "close_answer"

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

// closeRecords are the reads that say why a card ended closed (closedWhy)
// and the one write that ends it closed.
type closeRecords interface {
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	EndClosed(ctx context.Context, id string, from []string, reason string) error
}

// recordCloseAnswer records by's close answer on card id, with note, before
// the close it makes (IntakeCloseAnswer), and answers the row's id, which
// a close that fails deletes.
func recordCloseAnswer(ctx context.Context, store interface {
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
}, id, by, note string) (int64, error) {
	return store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: id, Kind: IntakeCloseAnswer, Author: by, Body: answerReason(by, note)})
}

// endClosed ends card id closed from one of from for reason, the reason of
// the path that ends it. A reason that says no more than that the card's
// issue closed, or none, gives way to the card's records (closedWhy), closer
// the close's author where the path knows one, bot the factory's account;
// with no record it stands, "issue closed" for none.
func endClosed(ctx context.Context, store closeRecords, id string, from []string, reason, closer, bot string) error {
	if reason == "" || strings.HasPrefix(reason, ReasonIssueClosed) {
		why, err := closedWhy(ctx, store, id, closer, bot)
		if err != nil {
			return err
		}
		reason = cmp.Or(why, reason, ReasonIssueClosed)
	}
	return store.EndClosed(ctx, id, from, reason)
}

// closedWhy is why card id closed by its records, "" for none: its newest
// close answer (IntakeCloseAnswer), else the factory's newest close record
// (closeBodyReason), else the close of its issue in Forgejo by closer, or
// with closer "" by the author of its newest issue_closed row, unless the
// factory's account bot closed it.
func closedWhy(ctx context.Context, store closeRecords, id, closer, bot string) (string, error) {
	rows, err := store.IntakeEvents(ctx, id)
	if err != nil {
		return "", err
	}
	var answer, record *IntakeEvent
	for i, e := range slices.Backward(rows) {
		switch {
		case e.Kind == IntakeCloseAnswer && answer == nil:
			answer = &rows[i]
		case e.Kind == IntakeFactoryClosed && record == nil:
			record = &rows[i]
		case e.Kind == "issue_closed" && closer == "":
			closer = e.Author
		}
	}
	if answer != nil {
		return answer.Body, nil
	}
	if record != nil {
		if why := closeBodyReason(record.Body); why != "" {
			return why, nil
		}
	}
	if closer == "" || closer == bot {
		return "", nil
	}
	return "closed in Forgejo by " + closer, nil
}

// answerReason is why a close answer by actor ended a card: "closed by"
// who answered, and the first sentence of note. A note that names its
// closer already, a plan's close words (PlanCloseWords), stands as it is.
func answerReason(actor, note string) string {
	note = leadSentence(note)
	switch {
	case strings.HasPrefix(note, "closed by "):
		return note
	case note == "":
		return "closed by " + DeciderWords(actor)
	}
	return "closed by " + DeciderWords(actor) + ": " + note
}

// oldCloseLead is the lead an older factoryd put on its close records:
// "factoryd: ", then "task <id> closed: " where it named the card.
var oldCloseLead = regexp.MustCompile(`^factoryd: (?:task \S+ closed: )?`)

// closeBodyReason is why the factory's close record with body ended its
// card: the body's first sentence, an older record's lead taken off. A
// body that holds only the sentence a close adds on the dependencies it
// dropped (NoLongerWaits) says none.
func closeBodyReason(body string) string {
	why := oldCloseLead.ReplaceAllString(leadSentence(body), "")
	if strings.HasPrefix(why, "It no longer waits on ") {
		return ""
	}
	return why
}

// leadSentence is the first sentence of text's first paragraph, on one
// line.
func leadSentence(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n\n")
	return firstSentence(oneLine(first))
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
// returns, the record left unconfirmed for the next call, the heal or, for a
// freeze card, its watcher; only a
// comment Forgejo refuses (forgejoTerminal) is logged, and the close goes
// on. A call that finds the record posts the recorded comment, not its own,
// where the issue lacks it: one close, one comment, also for a card whose
// earlier close, off its issue since a reopen, said the same words
// (finishClose). The
// sentence names what this call dropped: a retry after a failed record or a
// partly done removal finds those edges gone and does not name them, and the
// issue's timeline in Forgejo shows each removal. A card with no issue gets
// the record alone, with no forge call.
func closeIssueAsFactory(ctx context.Context, store interface {
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
	ConfirmFactoryClose(context.Context, string, int64) error
}, forgejo *Forgejo, t Task, comment string) error {
	if t.IssueNumber == 0 {
		// a card with no issue, factoryd's own human card before its face:
		// the record alone, once, which the heal pass ends the card on
		events, err := store.IntakeEvents(ctx, t.ID)
		if err != nil || botCloseRecorded(events, t) {
			return err
		}
		_, err = store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeFactoryClosed, Author: "factoryd", Body: comment})
		return err
	}
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
	closes := 0
	if id == 0 {
		events, err := store.IntakeEvents(ctx, t.ID)
		if err != nil {
			return err
		}
		comment = ""
		if i := slices.IndexFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeFactoryClosed && e.CommentID == t.IssueNumber }); i >= 0 {
			comment = events[i].Body
		}
		for _, e := range events {
			if e.Kind == IntakeFactoryClosed && e.Body == comment {
				closes++
			}
		}
	}
	return finishClose(ctx, store, forgejo, t, comment, closes)
}

// finishClose is the Forgejo side of a close factoryd recorded: it posts
// comment unless it is "", closes the issue and confirms the record. The
// call that made the record passes closes 0 and posts a new comment. A
// later call passes how many closes the card recorded with these words,
// this one and those a reopen took off the issue, and posts the comment
// unless the issue holds that many (CommentTimes): each close has its own
// comment, and a call made again posts none twice. Only a comment Forgejo
// refuses (forgejoTerminal) is logged, and the close goes on.
func finishClose(ctx context.Context, store interface {
	ConfirmFactoryClose(context.Context, string, int64) error
}, forgejo *Forgejo, t Task, comment string, closes int) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	switch {
	case comment == "":
	case closes == 0:
		_, err = forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, comment)
	default:
		err = forgejo.CommentTimes(ctx, repo.Owner, repo.Name, t.IssueNumber, comment, closes)
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

// closeRow moves human card id's row from waiting-human to done and
// reports whether this call moved it; a row no longer waiting is closed
// already. After closeIssueAsFactory the webhook of that close,
// factoryd's own, ends the human card too (finishHumanTask) and may come
// first, its outcome counted there.
func closeRow(ctx context.Context, store interface {
	TransitionTask(ctx context.Context, id string, from []string, to string) error
}, id string) (bool, error) {
	err := store.TransitionTask(ctx, id, []string{"waiting-human"}, "done")
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// maxEscalationText bounds the question, the reason and an information
// request's fields, maxRecommendation the recommendation, as the gateway
// does.
const (
	maxEscalationText = 4000
	maxRecommendation = 2000
)

// MaxChoices bounds the answers an agent may offer; writingguide.MaxChoice
// bounds the characters of one.
const MaxChoices = 5

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
	if len(out) < 2 || len(out) > MaxChoices || slices.ContainsFunc(out, func(c string) bool { return c == "" || utf8.RuneCountInString(c) > writingguide.MaxChoice }) {
		log.Printf("task %s: the agent's %d choices are dropped, the card offers 2 to %d of 1 to %d characters: %s",
			taskID, len(choices), MaxChoices, writingguide.MaxChoice, textutil.Truncate(fmt.Sprintf("%q", choices), 400))
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
			switch {
			case ask.Choices == nil:
				ask.Choice = 0
			case ask.Choice < 0 || ask.Choice > len(ask.Choices):
				http.Error(w, fmt.Sprintf("choice %d names none of the %d choices", ask.Choice, len(ask.Choices)), http.StatusBadRequest)
				return
			}
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
		if ask.Kind == AskEscalation || ask.Kind == AskInfo {
			var no *BriefRefusal
			if err := sessions.AdmitAsk(r.Context(), t, ask); errors.As(err, &no) {
				refuseBrief(w, no)
				return
			} else if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
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
			TaskID: t.ID, Kind: kind, Author: "agent", Body: body,
		}); err != nil {
			log.Printf("task %s: record the agent escalation: %v", t.ID, err)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
