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
	got, err := EscalateToHuman(ctx, store, f, tl, Repo{Owner: "sf", Name: "r"}, "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria.",
		[]string{"Write the criteria", "Park"}, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "human" || got.Status != "waiting-human" || got.Source != SourceChat || got.IssueNumber != 40 || got.Repository != "sf/r" ||
		!strings.HasPrefix(got.Objective, "needs you: Nothing on the roadmap is ready. Item 12: no acceptance criteria.\n\n") ||
		!strings.Contains(got.Objective, "Choices:\n- Write the criteria\n- Park\n") || !strings.Contains(got.Objective, "asked in sebastian's chat") {
		t.Fatalf("the card = %+v", got)
	}
	if !slices.Equal(*labels, []string{"human", "state/waiting-human"}) {
		t.Fatalf("labels = %v", *labels)
	}
	want := []Event{{Repository: "sf/r", TaskID: got.ID, IssueNumber: 40, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaChat,
		Detail: map[string]any{"question": "Nothing on the roadmap is ready.\nItem 12: no acceptance criteria.", "source": "agent", "by": "sebastian",
			"choices": []string{"Write the criteria", "Park"}}}}
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
		m.Content != "#40 needs you: Nothing on the roadmap is ready.\nItem 12: no acceptance criteria." || !slices.Equal(m.Choices, []string{"Write the criteria", "Park"}) {
		t.Fatalf("its poke = %+v", m)
	}
}
