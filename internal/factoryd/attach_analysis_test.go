package factoryd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

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
		Repository: repository, IssueNumber: issue, Objective: question}); err != nil {
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
		Content: fmt.Sprintf("#%d needs you: %s", issue, question), Choices: []string{"Yes", "No"}}); err != nil {
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

// An open ask takes the analysis: its escalation row carries it after the
// question, the spine holds an analysis_attached row with the findings,
// and a second attach replaces the row's analysis while the spine keeps
// both.
func TestAttachAnalysisEnrichesOpenAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if err := s.AttachAnalysis(t.Context(), "sf/wake", "t-1",
		"The replica adds 12 ms of lag; the board reads it fine.", "Keep one replica", "sebastian"); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := askRow(t, s, "sf/wake", "t-1"),
		"#40 needs you: Use a replica?\nAnalysis: The replica adds 12 ms of lag; the board reads it fine.\nRecommendation: Keep one replica"; got != want {
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
	if err := s.AttachAnalysis(t.Context(), "sf/wake", "t-1",
		"The writes queue behind the replica at the measured peak.", "", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); !strings.Contains(got, "queue behind the replica") || strings.Contains(got, "12 ms") {
		t.Fatalf("the second attach left the row at %q", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 2 {
		t.Fatalf("%d analysis rows after the second attach, want 2", len(rows))
	}
}

// An ask a settling event answered takes no analysis: AskSettledError with
// the word and the actor, the row unchanged, no analysis row on the spine.
func TestAttachAnalysisRefusesAnsweredAsk(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat, Detail: map[string]any{"answer": "No"}}); err != nil {
		t.Fatal(err)
	}
	err := s.AttachAnalysis(t.Context(), "sf/wake", "t-1", "The replica adds 12 ms of lag.", "", "sebastian")
	var se *AskSettledError
	if !errors.As(err, &se) || se.Word != "answered" || se.By != "sebastian" {
		t.Fatalf("the refusal = %v, want answered by sebastian", err)
	}
	if got, want := askRow(t, s, "sf/wake", "t-1"), "#40 needs you: Use a replica?"; got != want {
		t.Fatalf("the answered ask's row changed: %q, want %q", got, want)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 0 {
		t.Fatalf("%d analysis rows on an answered ask, want 0", len(rows))
	}
}
