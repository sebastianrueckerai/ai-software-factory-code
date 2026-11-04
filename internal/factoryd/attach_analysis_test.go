package factoryd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// AttachAnalysis records the wake turn's findings inside the card's open
// ask (the main agent concept): an analysis_attached row on the event
// spine and the ask's escalation rows rewritten to carry them, so every
// member reads the analysis with the question. The ask's own event stays
// what it was: the spine is append-only.

// newAskStore opens the store and empties the rows an ask touches.
func newAskStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "attach"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	for _, table := range []string{"events", "tasks", "chat_messages", "chat_conversations"} {
		if _, err := s.db.ExecContext(t.Context(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return s
}

// seedAsk seeds card issue of repository with its question asked and the
// member's escalation row of it, as the Poker writes the row.
func seedAsk(t *testing.T, s *PGStore, repository, taskID string, issue int64, question string) {
	t.Helper()
	if err := s.CreateTask(t.Context(), Task{ID: taskID, Kind: "agent", Status: "waiting-human",
		Repository: repository, IssueNumber: issue, Objective: question, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(t.Context(), Event{Repository: repository, TaskID: taskID, IssueNumber: issue,
		Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": question, "why": "the card cannot go on", "info": "the metrics say so"}}); err != nil {
		t.Fatal(err)
	}
	conv, err := s.EnsureInbox(t.Context(), repository, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChatMessage(t.Context(), repository, "sebastian", ChatMessage{Role: "assistant",
		Conversation: conv, Kind: MessageEscalation, TaskID: taskID, BirthKind: EventQuestionAsked,
		Content: fmt.Sprintf("#%d - %s needs you: %s\nWhy: the card cannot go on", issue, question, question), Choices: []string{"Yes", "No"}}); err != nil {
		t.Fatal(err)
	}
}

// askRow reads the task's escalation row content.
func askRow(t *testing.T, s *PGStore, repository, taskID string) string {
	t.Helper()
	var content string
	if err := s.db.QueryRowContext(t.Context(), `SELECT content FROM chat_messages
		WHERE repository = $1 AND task_id = $2 AND kind = $3`, repository, taskID, MessageEscalation).Scan(&content); err != nil {
		t.Fatal(err)
	}
	return content
}

// analyses counts the task's analysis_attached rows.
func analyses(t *testing.T, s *PGStore, taskID string) []Event {
	t.Helper()
	events, err := s.TaskEvents(t.Context(), taskID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range events {
		if e.Kind == EventAnalysisAttached {
			out = append(out, e)
		}
	}
	return out
}

// sebastiansAnalysis is the analysis_attached row sebastian's chat writes.
var sebastiansAnalysis = Event{Repository: "sf/wake", Actor: "sebastian", Via: ViaChat}

// The row keeps the detail its caller gave, a chat token's stamp, beside
// the findings the store adds.
func TestAttachAnalysisKeepsItsEventsDetail(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	e := sebastiansAnalysis
	e.Detail = map[string]any{"token": map[string]any{"id": 7, "name": "laptop"}}
	if err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", e); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	rows := analyses(t, s, "t-1")
	if len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	got := rows[0]
	if token, _ := got.Detail["token"].(map[string]any); got.Actor != "sebastian" || got.IssueNumber != 40 || got.Detail["found"] != "The replica adds 12 ms of lag." ||
		got.Detail["source"] != "agent" || token["name"] != "laptop" || token["id"] != float64(7) {
		t.Fatalf("the analysis row = %+v", got)
	}
}

// An open ask takes the analysis: its escalation row carries it after the
// question, the spine holds an analysis_attached row with the findings,
// and a second attach replaces the row's analysis while the spine keeps
// both.
func TestAttachAnalysisEnrichesOpenAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The replica adds 12 ms of lag; the board reads it fine.", "Keep one replica", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := askRow(t, s, "sf/wake", "t-1"),
		"#40 - Use a replica? needs you: Use a replica?\nWhy: the card cannot go on\nAnalysis: The replica adds 12 ms of lag; the board reads it fine.\nRecommendation: Keep one replica"; got != want {
		t.Fatalf("the ask's row = %q, want %q", got, want)
	}
	rows := analyses(t, s, "t-1")
	if len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	e := rows[0]
	if e.Via != ViaChat || e.Actor != "sebastian" || e.Detail["source"] != "agent" ||
		e.Detail["found"] != "The replica adds 12 ms of lag; the board reads it fine." ||
		e.Detail["recommendation"] != "Keep one replica" {
		t.Fatalf("the analysis row = %+v", e)
	}
	events, err := s.TaskEvents(t.Context(), "t-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	ask := events[len(events)-1] // newest first, so the ask is the oldest
	if ask.Kind != EventQuestionAsked || ask.Detail["question"] != "Use a replica?" || ask.Detail["found"] != nil {
		t.Fatalf("the ask's own event changed: %+v", ask)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The writes queue behind the replica at the measured peak.", "", sebastiansAnalysis); err != nil {
		t.Fatal(err)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); !strings.Contains(got, "queue behind the replica") || strings.Contains(got, "12 ms") {
		t.Fatalf("the second attach left the row at %q", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 2 {
		t.Fatalf("%d analysis rows after the second attach, want 2", len(rows))
	}
}

// A session cap's stop takes the analysis too: its row is the budget stop's
// words with the why under them, and the analysis follows the why.
func TestAttachAnalysisEnrichesCapStop(t *testing.T) {
	s := newAskStore(t)
	if err := s.CreateTask(t.Context(), Task{ID: "t-1", Kind: "agent", Status: "waiting-human",
		Repository: "sf/wake", IssueNumber: 40, Objective: "Land the rounds", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Grant it one more session?", "why": "review findings went back to the agent", "source": "cap"}}); err != nil {
		t.Fatal(err)
	}
	conv, err := s.EnsureInbox(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	row := "#40 - Land the rounds is stopped by its budget: Grant it one more session?\nWhy: review findings went back to the agent"
	if _, err := s.AddChatMessage(t.Context(), "sf/wake", "sebastian", ChatMessage{Role: "assistant",
		Conversation: conv, Kind: MessageEscalation, TaskID: "t-1", BirthKind: EventQuestionAsked, Content: row}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1", "One finding is open.", "Grant one session", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := askRow(t, s, "sf/wake", "t-1"), row+"\nAnalysis: One finding is open.\nRecommendation: Grant one session"; got != want {
		t.Fatalf("the ask's row = %q, want %q", got, want)
	}
}

// An ask a settling event answered takes no analysis: AskSettledError with
// the ask's kind, the word and the actor, the row unchanged, no analysis
// row on the spine.
func TestAttachAnalysisRefusesAnsweredAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat, Detail: map[string]any{"answer": "No"}}); err != nil {
		t.Fatal(err)
	}
	err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", sebastiansAnalysis)
	var se *AskSettledError
	if !errors.As(err, &se) || se.Kind != "question" || se.Word != "answered" || se.By != "sebastian" {
		t.Fatalf("the refusal = %v, want the question answered by sebastian", err)
	}
	if got, want := askRow(t, s, "sf/wake", "t-1"), "#40 - Use a replica? needs you: Use a replica?\nWhy: the card cannot go on"; got != want {
		t.Fatalf("the answered ask's row changed: %q, want %q", got, want)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 0 {
		t.Fatalf("%d analysis rows on an answered ask, want 0", len(rows))
	}
}

// errHeldClose is the close failure the hooking driver below answers on the
// ask's rows: a failure database/sql would swallow at the walk's own end,
// because a driver that still holds a next result set leaves the rows open
// for the caller to close.
var errHeldClose = errors.New("the driver failed to close the ask's rows")

// hookedConnector opens the store's own pgx connections, so every query
// answers as it would, wrapped in a hookedConn.
type hookedConnector struct{ driver.Connector }

func (c *hookedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &hookedConn{conn}, nil
}

// hookedConn forwards the modern calls to the pgx connection it wraps; the
// ask's SELECT answers rows that fail to close.
type hookedConn struct{ driver.Conn }

func (c *hookedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *hookedConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

func (c *hookedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c *hookedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, "FROM chat_messages") && strings.Contains(q, "birth_kind = $4") {
		return &heldCloseRows{rows}, nil
	}
	return rows, nil
}

// heldCloseRows answers that a next result set waits, so the walk's end
// leaves the rows open and the caller's Close is their first: a close that
// fails with errHeldClose. On plain pgx rows the walk's own end has closed
// them already, and the caller's Close answers nil whatever the driver felt.
type heldCloseRows struct{ driver.Rows }

func (heldCloseRows) HasNextResultSet() bool { return true }

func (heldCloseRows) NextResultSet() error { return io.EOF }

func (r *heldCloseRows) Close() error {
	_ = r.Rows.Close() // the wrapped rows' close is best effort; the caller must see the test's failure
	return errHeldClose
}

// The ask's rows failing to close after their walk surfaces as the attach's
// own error: the ask's row and the spine stay as they were.
func TestAttachAnalysisReturnsItsRowsCloseError(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	conn, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(schematest.DSN(t, "attach"))
	if err != nil {
		t.Fatalf("the pgx connector: %v", err)
	}
	hooked := sql.OpenDB(&hookedConnector{conn})
	t.Cleanup(func() { hooked.Close() })
	s.db = hooked
	err = s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", sebastiansAnalysis)
	if !errors.Is(err, errHeldClose) {
		t.Fatalf("AttachAnalysis = %v, want the rows' close failure", err)
	}
	if got, want := askRow(t, s, "sf/wake", "t-1"), "#40 - Use a replica? needs you: Use a replica?\nWhy: the card cannot go on"; got != want {
		t.Fatalf("the ask's row changed: %q, want %q", got, want)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 0 {
		t.Fatalf("%d analysis rows, want 0", len(rows))
	}
}

// approvalAskRow is the criteria request's poke row, as the Poker writes
// it for card #40, whose title is "Use a replica?".
const approvalAskRow = `Plan #40 "Use a replica?" waits for you: its cards ran out with a criterion still unmet. Waive one with your reason, grant one more bounded card, or confirm the delivered cards satisfy them.`

// seedApprovalAsk seeds over seedAsk's card the criteria approval the gap
// check opens: the question answered, then the request's event, its
// pending row and its escalation row, as the Poker writes the row — the
// fault's shape, the answered question no longer the ask that waits.
func seedApprovalAsk(t *testing.T, s *PGStore, repository, taskID string, issue int64) {
	t.Helper()
	if _, err := s.Record(t.Context(), Event{Repository: repository, TaskID: taskID, IssueNumber: issue,
		Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat, Detail: map[string]any{"answer": "One replica"}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.OpenApproval(t.Context(), ApprovalRequest{Kind: ApprovalCriteria, TaskID: taskID,
		Project: repository, Spec: []byte(`{"task":"t-int"}`), Reason: "criteria not met with the plan's cards done: c1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(t.Context(), Event{Repository: repository, TaskID: taskID, IssueNumber: issue,
		Kind: EventApprovalRequested, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"approval": id, "kind": ApprovalCriteria, "criteria": []string{"c1"}}}); err != nil {
		t.Fatal(err)
	}
	conv, err := s.EnsureInbox(t.Context(), repository, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChatMessage(t.Context(), repository, "sebastian", ChatMessage{Role: "assistant",
		Conversation: conv, Kind: MessageEscalation, TaskID: taskID, BirthKind: EventApprovalRequested,
		Content: approvalAskRow}); err != nil {
		t.Fatal(err)
	}
}

// rowContent reads the task's escalation row content of birth kind birth.
func rowContent(t *testing.T, s *PGStore, repository, taskID, birth string) string {
	t.Helper()
	var content string
	if err := s.db.QueryRowContext(t.Context(), `SELECT content FROM chat_messages
		WHERE repository = $1 AND task_id = $2 AND kind = $3 AND birth_kind = $4`,
		repository, taskID, MessageEscalation, birth).Scan(&content); err != nil {
		t.Fatal(err)
	}
	return content
}

// The card's open approval ask takes the analysis a newer answered
// question used to block: the ask is addressed across both families, the
// request's row carries the findings with its own words, and the answered
// question's row keeps its question.
func TestAttachAnalysisReachesTheOpenApprovalAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	seedApprovalAsk(t, s, "sf/wake", "t-1", 40)
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The replica adds 12 ms of lag; the board reads it fine.", "Waive c1", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := rowContent(t, s, "sf/wake", "t-1", EventApprovalRequested),
		approvalAskRow+"\nAnalysis: The replica adds 12 ms of lag; the board reads it fine.\nRecommendation: Waive c1"; got != want {
		t.Fatalf("the request's row = %q, want %q", got, want)
	}
	if got, want := rowContent(t, s, "sf/wake", "t-1", EventQuestionAsked), "#40 - Use a replica? needs you: Use a replica?\nWhy: the card cannot go on"; got != want {
		t.Fatalf("the answered question's row = %q, want %q", got, want)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 1 || rows[0].Detail["found"] != "The replica adds 12 ms of lag; the board reads it fine." {
		t.Fatalf("the analysis rows = %+v", rows)
	}
}

// The card's open approval ask takes the analysis even behind a newer
// answered question: an answer on another venue is no word on a request.
func TestAttachAnalysisReachesTheApprovalAskBehindANewerAnswer(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	seedApprovalAsk(t, s, "sf/wake", "t-1", 40)
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat, Detail: map[string]any{"answer": "Use one replica"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The replica adds 12 ms of lag; the board reads it fine.", "Waive c1", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := rowContent(t, s, "sf/wake", "t-1", EventApprovalRequested),
		approvalAskRow+"\nAnalysis: The replica adds 12 ms of lag; the board reads it fine.\nRecommendation: Waive c1"; got != want {
		t.Fatalf("the request's row = %q, want %q", got, want)
	}
}

// With no ask of either family open, the refusal names the ask it
// examined — the criteria request, its state and its actor — and nothing
// is written.
func TestAttachAnalysisRefusalNamesTheAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	seedApprovalAsk(t, s, "sf/wake", "t-1", 40)
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventPlanChangesRequested, Actor: "sebastian", Via: ViaConsole, Detail: map[string]any{"changes": "one more card"}}); err != nil {
		t.Fatal(err)
	}
	err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", sebastiansAnalysis)
	var se *AskSettledError
	if !errors.As(err, &se) || se.Kind != ApprovalCriteria || se.Word != "rejected" || se.By != "sebastian" {
		t.Fatalf("the refusal = %v, want the criteria request rejected by sebastian", err)
	}
	if got := rowContent(t, s, "sf/wake", "t-1", EventApprovalRequested); got != approvalAskRow {
		t.Fatalf("the settled request's row changed: %q", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 0 {
		t.Fatalf("%d analysis rows on a settled ask, want 0", len(rows))
	}
}

// A merge approval row is an ask like any: the analysis rides its inbox
// row after the request's own line, and the spine records it. Once a human
// approved the merge the ask is settled: the refusal names the merge
// request approved by that human, and nothing more is written.
func TestAttachAnalysisReachesAMergeRow(t *testing.T) {
	s := newAskStore(t)
	ctx := t.Context()
	if err := s.CreateTask(ctx, Task{ID: "t-merge", Kind: "agent", Status: "in-review", Repository: "sf/wake", IssueNumber: 41,
		PRNumber: 9, Objective: "Add the index.", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t-merge", Project: "sf/wake", PRNumber: 9,
		Spec: []byte(`{"risk_class":"high","head_sha":"abc123"}`), Reason: "pull request #9 at abc123 is on the card (risk class high): a human decides the merge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/wake", TaskID: "t-merge", IssueNumber: 41, Kind: EventApprovalRequested, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"approval": id, "kind": ApprovalMerge, "pr": 9, "class": "high", "factory": false, "red_checks": false}}); err != nil {
		t.Fatal(err)
	}
	conv, err := s.EnsureInbox(ctx, "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	const mergeRow = "#41 waits for you: merge PR #9, high risk."
	if _, err := s.AddChatMessage(ctx, "sf/wake", "sebastian", ChatMessage{Role: "assistant", Conversation: conv, Kind: MessageEscalation,
		TaskID: "t-merge", BirthKind: EventApprovalRequested, Content: mergeRow, Choices: []string{"Approve"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAnalysis(ctx, "t-merge", "The index covers the board's query.", "Approve it", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := rowContent(t, s, "sf/wake", "t-merge", EventApprovalRequested),
		mergeRow+"\nAnalysis: The index covers the board's query.\nRecommendation: Approve it"; got != want {
		t.Fatalf("the merge row = %q, want %q", got, want)
	}
	if rows := analyses(t, s, "t-merge"); len(rows) != 1 || rows[0].IssueNumber != 41 ||
		rows[0].Detail["found"] != "The index covers the board's query." || rows[0].Detail["recommendation"] != "Approve it" {
		t.Fatalf("the analysis rows = %+v", rows)
	}

	if err := s.DecideApproval(ctx, id, ApprovalMerge, "approved", "sebastian", "Approved in the factory console."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/wake", TaskID: "t-merge", IssueNumber: 41, Kind: EventMergeApproved,
		Actor: "sebastian", Via: ViaConsole, Detail: map[string]any{"pr": 9}}); err != nil {
		t.Fatal(err)
	}
	err = s.AttachAnalysis(ctx, "t-merge", "The index is still fine.", "", sebastiansAnalysis)
	var se *AskSettledError
	if !errors.As(err, &se) || se.Kind != ApprovalMerge || se.Word != "approved" || se.By != "sebastian" {
		t.Fatalf("the refusal = %v, want the merge request approved by sebastian", err)
	}
	if rows := analyses(t, s, "t-merge"); len(rows) != 1 {
		t.Fatalf("%d analysis rows after the settled attach, want 1", len(rows))
	}
}
