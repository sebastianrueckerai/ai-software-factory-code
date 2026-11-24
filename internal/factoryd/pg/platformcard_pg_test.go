package pg

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Hand-derived: PlatformCards lists the platform cards of every repository
// in any status, newest first, and no card of another source.
// FacePlatformCard writes the open card's face and what its ask knows
// together, and on a card of another source, or on none, it writes nothing
// and answers ErrNotFound. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestPlatformCardFaceIsOneWritePG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_platformcard"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ask := factoryd.HumanAsk{Q: factoryd.HumanQuestion{Ask: "The platform keeps ending the factory's work. Check the nodes.", Info: "the first face"},
		By: "factoryd", Asker: factoryd.SourcePlatformDeaths, Via: ViaFactoryd}
	old := Task{ID: "pd-old", Kind: "human", Status: "done", Track: policy.TrackDirect, Source: factoryd.SourcePlatformDeaths,
		Repository: "sf/other", IssueNumber: 3, Objective: "the old face", CreatedAt: base}
	if err := s.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		card Task
		ask  factoryd.HumanAsk
	}{
		{Task{ID: "pd-new", Track: policy.TrackDirect, Source: factoryd.SourcePlatformDeaths, Repository: "sf/acme", IssueNumber: 1,
			Objective: "the first face", CreatedAt: base.Add(time.Hour)}, ask},
		{Task{ID: "hu", Track: policy.TrackDirect, Source: "chat", Repository: "sf/acme", IssueNumber: 2, Objective: "the first face",
			CreatedAt: base.Add(2 * time.Hour)}, factoryd.HumanAsk{Q: ask.Q, By: "sebastian", Asker: "agent", Via: factoryd.ViaChat}},
	} {
		if _, created, err := s.CreateHumanCard(ctx, c.card, c.ask); err != nil || !created {
			t.Fatalf("CreateHumanCard %s = %v, %v", c.card.ID, created, err)
		}
	}
	cards, err := s.PlatformCards(ctx)
	ids := []string{}
	for _, c := range cards {
		ids = append(ids, c.ID+"/"+c.Status)
	}
	if err != nil || !slices.Equal(ids, []string{"pd-new/waiting-human", "pd-old/done"}) {
		t.Fatalf("PlatformCards = %v, %v; want pd-new waiting on the human, then pd-old done", ids, err)
	}
	state := func(id string) (face, info string, asks int) {
		t.Helper()
		card, ok, err := s.Task(ctx, id)
		if err != nil || !ok {
			t.Fatalf("card %s = %v, %v", id, ok, err)
		}
		if err := s.DB().QueryRowContext(ctx, `SELECT COALESCE((SELECT detail->>'info' FROM events WHERE task_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1), '')`,
			id, EventQuestionAsked).Scan(&info); err != nil {
			t.Fatal(err)
		}
		return card.Objective, info, freezeAskRows(t, s, id)
	}
	said := factoryd.Moments{{At: base.Add(2 * time.Hour), Layout: factoryd.LayoutDay}}
	if err := s.FacePlatformCard(ctx, "pd-new", "the second face, at 23 Nov 12:00 UTC", "what the second face says, at 23 Nov 12:00 UTC", said); err != nil {
		t.Fatal(err)
	}
	if face, info, asks := state("pd-new"); face != "the second face, at 23 Nov 12:00 UTC" || info != "what the second face says, at 23 Nov 12:00 UTC" || asks != 1 {
		t.Fatalf("after FacePlatformCard the card stands under %q and its %d asks know %q; want the second face on both, and no second ask", face, asks, info)
	}
	// the face's instant stands beside it on the card and on its ask
	var asked []byte
	if err := s.DB().QueryRowContext(ctx, `SELECT detail->'moments' FROM events WHERE task_id = 'pd-new' AND kind = $1 ORDER BY id DESC LIMIT 1`, EventQuestionAsked).Scan(&asked); err != nil {
		t.Fatal(err)
	}
	if card, _, _ := s.Task(ctx, "pd-new"); !reflect.DeepEqual(card.Moments, said) || string(asked) != `[{"at": "2026-11-23T12:00:00Z", "layout": "2 Jan 15:04"}]` {
		t.Fatalf("the card's moments %+v and its ask's %s, want %+v on both", card.Moments, asked, said)
	}
	for _, id := range []string{"hu", "pd-gone"} {
		if err := s.FacePlatformCard(ctx, id, "a face", "what it says", nil); !errors.Is(err, factoryd.ErrNotFound) {
			t.Fatalf("FacePlatformCard on %s = %v, want ErrNotFound", id, err)
		}
	}
	if face, info, asks := state("hu"); face != "the first face" || info != "the first face" || asks != 1 {
		t.Fatalf("the chat's card stands under %q with %d asks that know %q, want it untouched", face, asks, info)
	}
}
