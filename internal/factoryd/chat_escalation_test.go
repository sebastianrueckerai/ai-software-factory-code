package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The chat's escalation: a human-lane card in Needs you as the bot, its
// question fenced and its choices listed in the body, and the
// question_asked row the inbox's poke reads (roadmap 239).
func TestEscalateToHuman(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store, rows := newFakeStore(), &fakeEventRecorder{}
	tl := &Timeline{Store: store, Events: rows}
	got, err := EscalateToHuman(ctx, store, f, tl, Repo{Owner: "sf", Name: "r"}, HumanQuestion{
		Ask:            "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria. Write them?",
		Why:            "the plan needs its acceptance criteria before the build can start",
		Info:           "item 12's section stops at the goal sentence; the options below both satisfy it",
		Recommendation: "Write the criteria, read off the roadmap's section",
		Choices: []QuestionChoice{
			{Value: "Write the criteria", Label: "Write the criteria"},
			{Value: "Park", Label: "Park"},
		},
	}, "sebastian", Task{Source: SourceChat})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "human" || got.Status != "waiting-human" || got.Source != SourceChat || got.IssueNumber != 40 || got.Repository != "sf/r" ||
		!strings.HasPrefix(got.Objective, "needs you: Nothing on the roadmap is ready. Item 12: no acceptance criteria. Write them?\n\n") ||
		!strings.Contains(got.Objective, "Why: the plan needs its acceptance criteria before the build can start\n\n") ||
		!strings.Contains(got.Objective, "Info: item 12's section stops at the goal sentence; the options below both satisfy it\n\n") ||
		!strings.Contains(got.Objective, "Recommendation: Write the criteria, read off the roadmap's section\n\n") ||
		!strings.Contains(got.Objective, "Choices:\n- Write the criteria\n- Park\n") || !strings.Contains(got.Objective, "asked in sebastian's chat") {
		t.Fatalf("the card = %+v", got)
	}
	if !slices.Equal(*labels, []string{"human", "state/waiting-human"}) {
		t.Fatalf("labels = %v", *labels)
	}
	want := []Event{{Repository: "sf/r", TaskID: got.ID, IssueNumber: 40, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaChat,
		Detail: map[string]any{"question": "Nothing on the roadmap is ready. Item 12: no acceptance criteria. Write them?",
			"ask":    "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria. Write them?",
			"why":    "the plan needs its acceptance criteria before the build can start",
			"info":   "item 12's section stops at the goal sentence; the options below both satisfy it",
			"source": "agent", "by": "sebastian",
			"recommendation": "Write the criteria, read off the roadmap's section",
			"choices":        []string{"Write the criteria", "Park"}}}}
	if !reflect.DeepEqual(rows.rows, want) {
		t.Fatalf("rows = %+v,\nwant %+v", rows.rows, want)
	}
	// the poke reads the row as the store answers it: the detail through JSON
	stored := rows.rows[0]
	b, _ := json.Marshal(stored.Detail)
	stored.Detail = nil
	if err := json.Unmarshal(b, &stored.Detail); err != nil {
		t.Fatal(err)
	}
	if m, ok := pokeOf(stored, got); !ok || m.Kind != MessageEscalation ||
		m.Content != "#40 needs you: Nothing on the roadmap is ready. Item 12: no acceptance criteria. Write them?" || !slices.Equal(m.Choices, []string{"Write the criteria", "Park"}) {
		t.Fatalf("its poke = %+v", m)
	}
	// the ask's durable anchor, the same row the runner's escalations
	// write, so the board and drawer read one truth
	events, err := store.IntakeEvents(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	question := HumanQuestion{
		Ask:            "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria. Write them?",
		Why:            "the plan needs its acceptance criteria before the build can start",
		Info:           "item 12's section stops at the goal sentence; the options below both satisfy it",
		Recommendation: "Write the criteria, read off the roadmap's section",
		Choices: []QuestionChoice{
			{Value: "Write the criteria", Label: "Write the criteria"},
			{Value: "Park", Label: "Park"},
		},
	}
	if len(events) != 1 || events[0].Kind != IntakeEscalation || events[0].Author != "factoryd" ||
		events[0].Body != question.Ask || events[0].CommentID != 0 || events[0].Question == nil ||
		!reflect.DeepEqual(*events[0].Question, question) {
		t.Fatalf("intake events = %+v, want one escalation row carrying the question", events)
	}
}

// An escalation factoryd files itself, as the stall card: the card wears
// its source and commit, the issue says factoryd asks and never the chat
// agent, and its question_asked row is factoryd's, so the wake reads it
// as no agent's own doing.
func TestEscalateToHumanOfFactoryd(t *testing.T) {
	ctx := context.Background()
	f, _ := fileStub(t)
	store, rows := newFakeStore(), &fakeEventRecorder{}
	tl := &Timeline{Store: store, Events: rows}
	got, err := EscalateToHuman(ctx, store, f, tl, Repo{Owner: "sf", Name: "r"}, HumanQuestion{
		Ask: "The chain is pending. Re-run it?", Why: "pending past the bound", Info: "a re-run moves it",
		Choices: []QuestionChoice{{Value: AnswerRerunAgain, Label: "Re-run again"}, {Value: AnswerDismiss, Label: "Dismiss"}},
	}, "the chain poller", Task{Source: SourceChainStall, SHA: "c2c2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceChainStall || got.SHA != "c2c2" || !strings.Contains(got.Objective, "\nfactoryd asks (the chain poller). ") ||
		strings.Contains(got.Objective, "agent asked") {
		t.Fatalf("the card = %+v", got)
	}
	if len(rows.rows) != 1 || rows.rows[0].Via != ViaFactoryd || rows.rows[0].Detail["source"] != SourceChainStall || rows.rows[0].Detail["by"] != "the chain poller" {
		t.Fatalf("rows = %+v, want factoryd's question_asked row", rows.rows)
	}
}
