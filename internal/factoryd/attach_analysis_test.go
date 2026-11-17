package factoryd

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// AttachAnalysis records the wake turn's findings on the card's open ask
// (the main agent concept): an analysis_attached row on the event spine,
// which the card's face shows beside the ask (CaseDesk). No inbox row
// repeats it, and the ask's own event stays what it was: the spine is
// append-only.

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

// seededRow is seedAsk's escalation row, as the Poker wrote it.
const seededRow = "#40 - Use a replica? needs you: Use a replica?\nWhy: the card cannot go on"

// deskAnalysis is the analysis the board reads beside case id's ask.
func deskAnalysis(t *testing.T, s *PGStore, repository string, id int64) string {
	t.Helper()
	d, err := s.CaseDesk(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	return d.Asks[id].Analysis
}

// An open ask takes the analysis on its card: the spine holds an
// analysis_attached row with the findings, the board reads the newest one
// beside the card's case and its face shows it after the agent's view,
// and the inbox's row stays as the Poker wrote it. A second attach is the
// one the card shows, while the spine keeps both.
func TestAttachAnalysisShowsOnTheCard(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	c, opened, err := s.OpenCase(t.Context(), Case{TaskID: "t-1", Repository: "sf/wake", Kind: CaseAgentEscalate, StateKey: "k1",
		Status: CaseAtHuman, Problem: "Use a replica?"})
	if err != nil || !opened {
		t.Fatalf("OpenCase = %v, %v", opened, err)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The replica adds 12 ms of lag, and the board reads it fine.", "Keep one replica", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	want := "The project agent found: The replica adds 12 ms of lag, and the board reads it fine.\n\nIt recommends: Keep one replica"
	if got := deskAnalysis(t, s, "sf/wake", c.ID); got != want {
		t.Fatalf("the card's analysis = %q, want %q", got, want)
	}
	ask := CaseAsk{Detail: map[string]any{"info": "the metrics say so"}, Analysis: want}
	if got := CaseFace(c, Task{ID: "t-1"}, ask).Info; got != "the metrics say so\n\n"+want {
		t.Fatalf("the face's info = %q", got)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); got != seededRow {
		t.Fatalf("the inbox's row = %q, want it as written: %q", got, seededRow)
	}
	rows := analyses(t, s, "t-1")
	if len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	e := rows[0]
	if e.Via != ViaChat || e.Actor != "sebastian" || e.Detail["source"] != "agent" ||
		e.Detail["found"] != "The replica adds 12 ms of lag, and the board reads it fine." ||
		e.Detail["recommendation"] != "Keep one replica" {
		t.Fatalf("the analysis row = %+v", e)
	}
	events, err := s.TaskEvents(t.Context(), "t-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if first := events[len(events)-1]; first.Kind != EventQuestionAsked || first.Detail["question"] != "Use a replica?" || first.Detail["found"] != nil {
		t.Fatalf("the ask's own event changed: %+v", first)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The writes queue behind the replica at the measured peak.", "", sebastiansAnalysis); err != nil {
		t.Fatal(err)
	}
	if got := deskAnalysis(t, s, "sf/wake", c.ID); got != "The project agent found: The writes queue behind the replica at the measured peak." {
		t.Fatalf("the second attach left the card at %q", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 2 {
		t.Fatalf("%d analysis rows after the second attach, want 2", len(rows))
	}
}

// A monitor's ask sits beside the card's own and is answered by its
// case's number: the card's own ask takes the analysis, though the
// monitor's is newer and open, and no row of the inbox changes.
func TestAttachAnalysisLeavesAMonitorsAskOut(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40, Kind: EventQuestionAsked, Actor: FactoryDecider,
		Via: ViaFactoryd, Detail: map[string]any{"question": "The monitor flagged this card: what should happen? Answer case 12 in the project chat.",
			"case_id": 12, "case_kind": string(CaseMonitorFlag)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); got != seededRow {
		t.Fatalf("the card's own ask's row = %q, want it as written", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
}

// A second ask after the first was answered takes the analysis: the
// answer settled the first ask alone.
func TestAttachAnalysisTakesTheAskAfterAnAnsweredOne(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	for _, e := range []Event{{Kind: EventQuestionAnswered, Detail: map[string]any{"answer": "No"}}, {Kind: EventQuestionAsked, Detail: map[string]any{"question": "Which port?"}}} {
		e.Repository, e.TaskID, e.IssueNumber, e.Actor, e.Via = "sf/wake", "t-1", 40, "sebastian", ViaChat
		if _, err := s.Record(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AttachAnalysis(t.Context(), "t-1", "Port 8080 is taken.", "", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 1 || rows[0].Detail["found"] != "Port 8080 is taken." {
		t.Fatalf("the analysis rows = %+v", rows)
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
	if got := askRow(t, s, "sf/wake", "t-1"); got != seededRow {
		t.Fatalf("the answered ask's row changed: %q, want %q", got, seededRow)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 0 {
		t.Fatalf("%d analysis rows on an answered ask, want 0", len(rows))
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
// request's case shows the findings on the card, and no inbox row changes.
func TestAttachAnalysisReachesTheOpenApprovalAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	seedApprovalAsk(t, s, "sf/wake", "t-1", 40)
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The replica adds 12 ms of lag, and the board reads it fine.", "Waive c1", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	d, err := s.CaseDesk(t.Context(), "sf/wake")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(d.Cases, func(c Case) bool { return c.Kind == CasePlanCriteria })
	if i < 0 {
		t.Fatalf("the desk's cases = %+v, want the criteria request's", d.Cases)
	}
	if got, want := d.Asks[d.Cases[i].ID].Analysis, "The project agent found: The replica adds 12 ms of lag, and the board reads it fine.\n\nIt recommends: Waive c1"; got != want {
		t.Fatalf("the request's card shows %q, want %q", got, want)
	}
	if got := rowContent(t, s, "sf/wake", "t-1", EventApprovalRequested); got != approvalAskRow {
		t.Fatalf("the request's row = %q, want it as written", got)
	}
	if got := rowContent(t, s, "sf/wake", "t-1", EventQuestionAsked); got != seededRow {
		t.Fatalf("the answered question's row = %q, want %q", got, seededRow)
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
		"The replica adds 12 ms of lag, and the board reads it fine.", "Waive c1", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
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

// A merge approval row is an ask like any: the spine records the analysis
// and the merge's card shows it, its inbox row as written. Once a human
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
	if got := rowContent(t, s, "sf/wake", "t-merge", EventApprovalRequested); got != mergeRow {
		t.Fatalf("the merge row = %q, want it as written", got)
	}
	d, err := s.CaseDesk(ctx, "sf/wake")
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(d.Cases, func(c Case) bool { return c.Kind == CaseMergeApproval }); i < 0 ||
		d.Asks[d.Cases[i].ID].Analysis != "The project agent found: The index covers the board's query.\n\nIt recommends: Approve it" {
		t.Fatalf("the desk = %+v, want the merge's case showing the analysis", d)
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
