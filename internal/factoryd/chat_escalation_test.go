package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A card with an ID files under it, and its labels follow human.
func TestEscalateToHumanKeepsTheCardsIDAndLabels(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store := newFakeStore()
	got, err := EscalateToHuman(ctx, store, f, Repo{Owner: "sf", Name: "r"},
		HumanQuestion{Ask: "Handle it?", Why: "w", Info: "i"}, "factoryd", Task{ID: "t-given", Source: SourceAlert}, "bug")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t-given" || store.tasks["t-given"].IssueNumber != 40 || !slices.Equal(*labels, []string{"human", "bug", "state/waiting-human"}) {
		t.Fatalf("card %+v, tasks %v, labels %v", got, store.tasks, *labels)
	}
}

// The chat's escalation: a human-lane card in Needs you as the bot, its
// question fenced and its choices listed in the body, and the
// question_asked row the inbox's poke reads (roadmap 239).
func TestEscalateToHuman(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store := newFakeStore()
	const askText = "Nothing on the roadmap is ready. Item 12: no acceptance criteria. Write them?"
	wantBody := "```\nNothing on the roadmap is ready.\nItem 12: no acceptance criteria. Write them?\n```\n\n" +
		"Why: the plan needs its acceptance criteria before the build can start\n\n" +
		"Info: item 12's section stops at the goal sentence; the options below both satisfy it\n\n" +
		"Recommendation: Write the criteria, read off the roadmap's section\n\n" +
		"Choices:\n- Write the criteria\n- Park\n\n" +
		"The project agent asked in sebastian's chat. Answer in the chat's inbox, or close this issue once decided.\n\nfiled by factoryd"
	got, err := EscalateToHuman(ctx, store, f, Repo{Owner: "sf", Name: "r"}, HumanQuestion{
		Ask:            "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria. Write them?",
		Why:            "the plan needs its acceptance criteria before the build can start",
		Info:           "item 12's section stops at the goal sentence; the options below both satisfy it",
		Recommendation: "Write the criteria, read off the roadmap's section",
		Source:         "the project agent",
		Choices: []QuestionChoice{
			{Value: "Write the criteria", Label: "Write the criteria"},
			{Value: "Park", Label: "Park"},
		},
	}, "sebastian", Task{Source: SourceChat})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "human" || got.Status != "waiting-human" || got.Source != SourceChat || got.IssueNumber != 40 || got.Repository != "sf/r" ||
		got.Objective != askText+"\n\n"+wantBody {
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
			"source": "agent", "by": "sebastian", "case_id": int64(1),
			"recommendation": "Write the criteria, read off the roadmap's section", "recommended_by": "the project agent",
			"choices": []string{"Write the criteria", "Park"},
			"offers":  []QuestionChoice{{Value: "Write the criteria", Label: "Write the criteria"}, {Value: "Park", Label: "Park"}}}}}
	if !reflect.DeepEqual(store.events, want) {
		t.Fatalf("rows = %+v,\nwant %+v", store.events, want)
	}
	// the poke reads the row as the store answers it: the detail through JSON
	stored := store.events[0]
	b, _ := json.Marshal(stored.Detail)
	stored.Detail = nil
	if err := json.Unmarshal(b, &stored.Detail); err != nil {
		t.Fatal(err)
	}
	if m, ok := pokeOf(stored, got); !ok || m.Kind != MessageEscalation ||
		m.Content != "#40 - Nothing on the roadmap is ready. Item 12: no acceptance criteria. Write them? needs you: Nothing on the roadmap is ready. Item 12: no acceptance criteria. Write them?\nWhy: the plan needs its acceptance criteria before the build can start" || !slices.Equal(m.Choices, []string{"Write the criteria", "Park"}) {
		t.Fatalf("its poke = %+v", m)
	}
	// the ask's durable anchor, the same row the runner's escalations
	// write, so the board and drawer read one truth
	if events, err := store.IntakeEvents(ctx, got.ID); err != nil || len(events) != 0 {
		t.Fatalf("intake events = %+v, %v; want none: the ask is its question_asked row", events, err)
	}
	question := HumanQuestion{
		Ask:            "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria. Write them?",
		Why:            "the plan needs its acceptance criteria before the build can start",
		Info:           "item 12's section stops at the goal sentence; the options below both satisfy it",
		Recommendation: "Write the criteria, read off the roadmap's section",
		Source:         "the project agent",
		Choices: []QuestionChoice{
			{Value: "Write the criteria", Label: "Write the criteria"},
			{Value: "Park", Label: "Park"},
		},
	}
	if q := questionOfAsk(stored.Detail); q == nil || !reflect.DeepEqual(*q, question) {
		t.Fatalf("the row's question = %+v, want %+v", q, question)
	}
}

// An escalation factoryd files itself, as the stall card: the card wears
// its source and commit, the issue says factoryd asks and never the chat
// agent, and its question_asked row is factoryd's, so the wake reads it
// as no agent's own doing.
func TestEscalateToHumanOfFactoryd(t *testing.T) {
	ctx := context.Background()
	f, _ := fileStub(t)
	store := newFakeStore()
	got, err := EscalateToHuman(ctx, store, f, Repo{Owner: "sf", Name: "r"}, HumanQuestion{
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
	if len(store.events) != 1 || store.events[0].Via != ViaFactoryd || store.events[0].Detail["source"] != SourceChainStall || store.events[0].Detail["by"] != "the chain poller" {
		t.Fatalf("rows = %+v, want factoryd's question_asked row", store.events)
	}
}

// A caller's long ask is cut as a guard: the title stays within MaxAsk
// bytes and says it was cut.
func TestEscalateToHumanGuardsTheAsk(t *testing.T) {
	f, _ := fileStub(t)
	store := newFakeStore()

	got, err := EscalateToHuman(context.Background(), store, f, Repo{Owner: "sf", Name: "r"},
		HumanQuestion{Ask: strings.Repeat("a", 300), Why: "w", Info: "i"}, "sebastian", Task{Source: SourceChat})
	if err != nil {
		t.Fatal(err)
	}
	title, _, _ := strings.Cut(got.Objective, "\n\n")
	if want := strings.Repeat("a", 197) + "…"; title != want || len(title) != 200 {
		t.Fatalf("title = %q (%d bytes), want 197 a's and …", title, len(title))
	}
}

// recordAsk writes the ask row of card t as a human card is born with it,
// for the tests that seed an ask on a card they made themselves.
func recordAsk(ctx context.Context, tl *Timeline, t Task, q HumanQuestion, by, asker, via, decider string, facts map[string]any) error {
	return tl.WriteRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, via, HumanAsk{Q: q, By: by, Asker: asker, Via: via, Decider: decider, Facts: facts}.Event(t).Detail)
}
