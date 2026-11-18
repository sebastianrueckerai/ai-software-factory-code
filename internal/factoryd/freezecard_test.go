package factoryd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// freezeStore is the fakeStore with the freeze watcher's reads: the
// gateway's class_frozen and class_resumed rows and the cursor table.
type freezeStore struct {
	*fakeStore
	rows         []FreezeEvent
	storedCursor int64
}

func (s *freezeStore) FreezeEvents(ctx context.Context) ([]FreezeEvent, int64, error) {
	var out []FreezeEvent
	newest := s.storedCursor
	for _, ev := range s.rows {
		if ev.ID > s.storedCursor {
			out = append(out, ev)
			newest = max(newest, ev.ID)
		}
	}
	return out, newest, nil
}

func (s *freezeStore) SetFreezeCursor(ctx context.Context, cursor int64) error {
	s.storedCursor = cursor
	return nil
}

// ListTasksByStatus refuses: the freeze watcher reads its cards by their
// source (FreezeCards), never by a scan of every card in a status.
func (s *freezeStore) ListTasksByStatus(context.Context, string) ([]Task, error) {
	return nil, errors.New("the freeze watcher scanned every card in a status: it reads its cards by their source, FreezeCards")
}

// ListFrozenCards lists the fake's running and queued cards whose classes
// hold class, in every repository, in id order, as the store's read does.
func (s *freezeStore) ListFrozenCards(ctx context.Context, class string) ([]Task, error) {
	var out []Task
	for _, id := range slices.Sorted(maps.Keys(s.tasks)) {
		if t := s.tasks[id]; slices.Contains(t.RequiredCapabilities, class) && slices.Contains([]string{"running", "queued"}, t.Status) {
			out = append(out, t)
		}
	}
	return out, nil
}

// freezeWatcher is a watcher with a fixed clock over store, under a policy
// whose big and small classes name the zai provider and whose plain class
// names none.
func freezeWatcher(store *freezeStore, forgejo *Forgejo, now *time.Time) *FreezeCard {
	doc := policy.Document{Waits: policy.Waits{ProviderLimit: 24 * time.Hour},
		Classes:        map[string]policy.Class{"big": {Provider: "zai"}, "small": {Provider: "zai"}, "plain": {}},
		ProviderLimits: map[string]policy.ProviderLimit{"zai": {TZ: "Asia/Singapore"}}}
	return &FreezeCard{Store: store, Forgejo: forgejo, now: func() time.Time { return *now },
		Current: func() policy.Version { return policy.Version{Tree: policy.Tree{Document: doc}} }}
}

// Hand-derived against the plan's test 5: one hold on big with nine frozen
// cards files one card the moment it arms, whatever the reset's distance;
// it names the words, the countdown and the count, holds no slot and asks
// nothing. A re-arm updates the same card. A self-resume leaves it five
// minutes and then it closes on its own. Past waits.provider_limit the
// freeze card asks once and no work card asks. (The retry press's at-once
// close is the freeze-close-on-retry card's item.)
func TestFreezeCardOnePerClass(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	const words = "free quota exhausted, resets 2026-11-22 01:04:50"
	// nine cards of the big class, all waiting on the freeze
	for i := 0; i < 9; i++ {
		if err := store.CreateTask(ctx, Task{ID: string(rune('a' + i)), Kind: "agent", Track: policy.TrackDirect, Status: "running",
			Repository: repo.String(), RequiredCapabilities: []string{"big"}}); err != nil {
			t.Fatal(err)
		}
	}
	// the hold arms at 12:00, the reset five hours out: the card files at once
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(4*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: words})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 || len(store.tasks) != 10 {
		t.Fatalf("issues %d, tasks %d, want 1 and 10", len(fj.issues), len(store.tasks))
	}
	var card Task
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard {
			card = c
		}
	}
	if card.ID == "" || card.Kind != "human" || card.Status != "waiting-human" || card.AdmittedAt != nil {
		t.Fatalf("freeze card = %+v, want a waiting human card that holds no slot", card)
	}
	for _, want := range []string{"The provider limit freezes the big class", "Class: big", words, "in 4h 59m", "9 cards of the class wait"} {
		if !strings.Contains(fj.issues[0], want) {
			t.Errorf("the card's face lacks %q:\n%s", want, fj.issues[0])
		}
	}
	if got := fj.labels[card.IssueNumber]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	// no ask anywhere: the card informs, the work cards ask nothing
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("a question was asked: %+v", e)
		}
	}
	// a re-arm with a later reset updates the same card, no second card
	now = base.Add(time.Hour)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big", Reset: now.Add(8*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: words})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("the re-arm filed a second card: %d issues", len(fj.issues))
	}
	if updated, _, _ := store.Task(ctx, card.ID); !strings.Contains(updated.Objective, "in 8h 59m") {
		t.Errorf("the re-arm left the face stale:\n%s", updated.Objective)
	}
	// a self-resume: the held time on the face, then five minutes, then the
	// card closes on its own
	now = base.Add(90 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: repo.String(), At: now, Kind: "class_resumed",
		Class: "big", Reset: now.Format(time.RFC3339)})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, _, _ := store.Task(ctx, card.ID)
	if resumed.Status != "waiting-human" || !strings.Contains(resumed.Objective, "the freeze held 1h 30m") {
		t.Fatalf("after the resume the card =\n%s", resumed.Objective)
	}
	now = now.Add(4 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if t2, _, _ := store.Task(ctx, card.ID); t2.Status != "waiting-human" {
		t.Fatalf("the card closed %s early", now)
	}
	now = now.Add(2 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if t2, _, _ := store.Task(ctx, card.ID); t2.Status != "done" {
		t.Fatalf("the card stands %s, want closed five minutes after the resume", now)
	}
	if !slices.Contains(fj.closed, card.IssueNumber) {
		t.Errorf("issue #%d stands open", card.IssueNumber)
	}
	// a freeze held past waits.provider_limit: the freeze card alone speaks,
	// once, and no work card asks
	now = base.Add(2 * time.Hour)
	store.rows = append(store.rows, FreezeEvent{ID: 4, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big", Reset: now.Add(30 * time.Hour).Format(time.RFC3339), Words: words})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 2 {
		t.Fatalf("issues %d, want the new hold's card", len(fj.issues))
	}
	now = now.Add(25 * time.Hour)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	asks := 0
	for _, c := range fj.comments {
		if !strings.Contains(c, "has held past waits.provider_limit") {
			continue
		}
		asks++
		for _, want := range []string{"Add a fallback", "switch the class's model", "keep waiting"} {
			if !strings.Contains(c, want) {
				t.Errorf("the ask lacks %q: %s", want, c)
			}
		}
	}
	if asks != 1 {
		t.Fatalf("the freeze card asked %d times, want once", asks)
	}
	spoken, _, _ := store.Task(ctx, card2ID(store, repo))
	if !strings.Contains(spoken.Objective, "Add a fallback") {
		t.Errorf("the ask is not on the card's face:\n%s", spoken.Objective)
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("a question was asked: %+v", e)
		}
	}
}

// Hand-derived against the plan's retry-press tests: the press closes the
// card at once, with no five-minute grace, and matches the class's own
// card only; after the press a first success keeps it closed on
// class_resumed, and a first dated refusal re-arms the hold and updates
// the same card — no second card, no ask.
func TestFreezeCardCloseOnRetry(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	const words = "free quota exhausted, resets 2026-11-22 01:04:50"
	// the big hold arms: the card files
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(4*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: words})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	// a neighbouring class's card stands open beside it: the big press
	// leaves it, the whole-line Class match telling the classes apart
	neighbour := Task{ID: "nb", Kind: "human", Status: "waiting-human", RiskClass: "standard", Repository: repo.String(),
		Source: SourceFreezeCard, Author: "factory-agent",
		Objective: "The provider limit freezes the bigv2 class\n\nClass: bigv2\n\nbody"}
	if err := store.CreateTask(ctx, neighbour); err != nil {
		t.Fatal(err)
	}
	var card Task
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard && objectiveHasClassLine(c.Objective, "big") {
			card = c
		}
	}
	if card.ID == "" || card.ID == neighbour.ID {
		t.Fatal("the big class's freeze card did not file")
	}
	// the retry press: the card closes at once, no five-minute wait, and
	// only the big class's card closes
	if err := w.CloseOnRetry(ctx, repo.String(), "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	closed, _, _ := store.Task(ctx, card.ID)
	if closed.Status != "done" {
		t.Fatalf("after the press the card stands %s, want done at once", closed.Status)
	}
	if !slices.Contains(fj.closed, card.IssueNumber) {
		t.Errorf("issue #%d stands open after the press", card.IssueNumber)
	}
	if nb, _, _ := store.Task(ctx, neighbour.ID); nb.Status != "waiting-human" {
		t.Errorf("the press closed bigv2's card too: %s", nb.Status)
	}
	// a second press is quiet: no open card stands
	if err := w.CloseOnRetry(ctx, repo.String(), "big", "sebastian"); err != nil {
		t.Fatalf("the second press: %v", err)
	}
	if n := len(fj.closed); n != 1 {
		t.Errorf("%d issues closed, want the one", n)
	}
	// a first success after the press: the card stays closed on
	// class_resumed, no second card
	now = base.Add(time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: repo.String(), At: now, Kind: "class_resumed",
		Class: "big", Reset: now.Format(time.RFC3339)})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := store.Task(ctx, card.ID); kept.Status != "done" {
		t.Fatalf("the success reopened the card: %s", kept.Status)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("issues %d, want the one card", len(fj.issues))
	}
	// a first dated refusal after the press: the hold re-arms and the same
	// card updates — no second card, no ask
	now = base.Add(2 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big", Reset: now.Add(8*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: words})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("the refusal filed a second card: %d issues", len(fj.issues))
	}
	rearmed, _, _ := store.Task(ctx, card.ID)
	if rearmed.Status != "waiting-human" || !strings.Contains(rearmed.Objective, "in 8h 59m") {
		t.Fatalf("the re-arm left the card =\n%s", rearmed.Objective)
	}
	if !slices.ContainsFunc(fj.comments, func(c string) bool { return strings.Contains(c, "re-armed after the retry") }) {
		t.Errorf("the re-arm said nothing: comments %q", fj.comments)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("a question was asked: %+v", e)
		}
	}
	// and the re-armed card still closes five minutes after a resume
	now = base.Add(3 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 4, Repository: repo.String(), At: now, Kind: "class_resumed",
		Class: "big", Reset: now.Format(time.RFC3339)})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(freezeResumeHold)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if done, _, _ := store.Task(ctx, card.ID); done.Status != "done" {
		t.Fatalf("the re-armed card stands %s after its hold, want closed", done.Status)
	}
}

// Hand-derived against the plan's restart case: the press's close lives
// in the watcher's memory, so a factoryd restart between the press and
// the probe's answer must not lose it. A fresh watcher on the same store
// adopts the class's done card as closed — a first success keeps it done,
// a first dated refusal re-arms the same card, no second card, no ask —
// while a class whose card ended its episode on a self-resume still gets
// a new card for a new hold.
func TestFreezeCardCloseSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	const words = "free quota exhausted, resets 2026-11-22 01:04:50"
	// the hold arms: the card files
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(4*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: words})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	var card Task
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard && objectiveHasClassLine(c.Objective, "big") {
			card = c
		}
	}
	if card.ID == "" {
		t.Fatal("the big class's freeze card did not file")
	}
	// the retry press closes it at once, then factoryd restarts: a fresh
	// watcher over the same store, no memory of the press
	if err := w.CloseOnRetry(ctx, repo.String(), "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if closed, _, _ := store.Task(ctx, card.ID); closed.Status != "done" {
		t.Fatalf("after the press the card stands %s, want done", closed.Status)
	}
	w2 := freezeWatcher(store, forgejo, &now)
	// a first success after the restart: the card keeps closed
	now = base.Add(time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: repo.String(), At: now, Kind: "class_resumed",
		Class: "big", Reset: now.Format(time.RFC3339)})
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := store.Task(ctx, card.ID); kept.Status != "done" {
		t.Fatalf("the success after the restart reopened the card: %s", kept.Status)
	}
	// a first dated refusal after the restart: the same card re-arms —
	// no second card, no ask
	now = base.Add(2 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big", Reset: now.Add(8*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: words})
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("the refusal after the restart filed a second card: %d issues", len(fj.issues))
	}
	rearmed, _, _ := store.Task(ctx, card.ID)
	if rearmed.Status != "waiting-human" || !strings.Contains(rearmed.Objective, "in 8h 59m") {
		t.Fatalf("the re-arm after the restart left the card %s:\n%s", rearmed.Status, rearmed.Objective)
	}
	if len(fj.closed) != 2 {
		t.Errorf("%d issue patches, want the press's close and the re-arm's reopen", len(fj.closed))
	}
	if !slices.ContainsFunc(fj.comments, func(c string) bool { return strings.Contains(c, "re-armed after the retry") }) {
		t.Errorf("the re-arm after the restart said nothing: comments %q", fj.comments)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("a question was asked: %+v", e)
		}
	}
	// a self-closed card ended its episode: after another restart a new
	// hold files its own card, the done one is not adopted
	now = base.Add(3 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 4, Repository: repo.String(), At: now, Kind: "class_resumed",
		Class: "big", Reset: now.Format(time.RFC3339)})
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(freezeResumeHold)
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if done, _, _ := store.Task(ctx, card.ID); done.Status != "done" {
		t.Fatalf("the re-armed card stands %s after its hold, want closed", done.Status)
	}
	w3 := freezeWatcher(store, forgejo, &now)
	now = now.Add(time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 5, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big", Reset: now.Add(2 * time.Hour).Format(time.RFC3339), Words: words})
	if err := w3.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 2 {
		t.Fatalf("the new hold after a self-close reused the done card: %d issues", len(fj.issues))
	}
	fresh, _, _ := store.Task(ctx, card.ID)
	if fresh.Status != "done" || !strings.Contains(fresh.Objective, freezeResumedSentence) {
		t.Fatalf("the self-closed card was reopened: %s\n%s", fresh.Status, fresh.Objective)
	}
}

// card2ID is the second freeze card's id, the one the new hold filed.
func card2ID(store *freezeStore, repo Repo) string {
	for _, t := range store.tasks {
		if t.Source == SourceFreezeCard && t.Repository == repo.String() && t.Status == "waiting-human" {
			return t.ID
		}
	}
	return ""
}

// findCard matches the whole Class line: a class_frozen for big-pro
// leaves big's card alone and updates big-pro's own, even across a
// restart whose watcher adopts the open cards afresh.
func TestFreezeCardClassMatchIsWholeLine(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	// big's hold arms and files its card
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(4 * time.Hour).Format(time.RFC3339), Words: "big frozen"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("issues %d, want one", len(fj.issues))
	}
	// the row's stamp the later watchers read frozenAt from
	big, _, _ := store.Task(ctx, cardOfClass(store, repo, "big"))
	big.CreatedAt = base
	store.tasks[big.ID] = big
	// big-pro's hold arms: a fresh watcher adopts the open cards afresh,
	// and big's card must not be mistaken for big-pro's
	now = base.Add(time.Minute)
	w2 := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big-pro", Reset: now.Add(4 * time.Hour).Format(time.RFC3339), Words: "big-pro frozen"})
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 2 {
		t.Fatalf("issues %d, want big-pro's own card beside big's", len(fj.issues))
	}
	big, _, _ = store.Task(ctx, cardOfClass(store, repo, "big"))
	bigPro, _, _ := store.Task(ctx, cardOfClass(store, repo, "big-pro"))
	if !strings.Contains(big.Objective, "Class: big\n") || strings.Contains(big.Objective, "big-pro") {
		t.Fatalf("big's card was touched by big-pro's hold:\n%s", big.Objective)
	}
	if !strings.Contains(bigPro.Objective, "Class: big-pro\n") {
		t.Fatalf("big-pro's card lacks its class:\n%s", bigPro.Objective)
	}
	// big-pro's hold re-arms with a later reset: its own card updates, big's stays stale
	now = base.Add(2 * time.Minute)
	w3 := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: repo.String(), At: now, Kind: "class_frozen",
		Class: "big-pro", Reset: now.Add(8*time.Hour + 59*time.Minute).Format(time.RFC3339), Words: "big-pro frozen"})
	if err := w3.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 2 {
		t.Fatalf("issues %d, want no third card", len(fj.issues))
	}
	if updated, _, _ := store.Task(ctx, bigPro.ID); !strings.Contains(updated.Objective, "in 8h 59m") {
		t.Errorf("big-pro's face stayed stale:\n%s", updated.Objective)
	}
	if again, _, _ := store.Task(ctx, big.ID); strings.Contains(again.Objective, "in 8h 59m") || again.Objective != big.Objective {
		t.Errorf("big's card changed under big-pro's re-arm:\n%s", again.Objective)
	}
}

// cardOfClass is the open freeze card a class's hold filed.
func cardOfClass(store *freezeStore, repo Repo, class string) string {
	marker := "Class: " + class + "\n"
	for _, t := range store.tasks {
		if t.Source == SourceFreezeCard && t.Repository == repo.String() && strings.Contains(t.Objective, marker) {
			return t.ID
		}
	}
	return ""
}

// A restart between the persisted resume cursor and the five-minute
// close: a fresh watcher over the same store reads the close stamp off
// the resumed card's face and closes the card at its five-minute mark.
func TestFreezeCardRestartAfterResumeClosesIt(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(4 * time.Hour).Format(time.RFC3339), Words: "big frozen"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, _, _ := store.Task(ctx, cardOfClass(store, repo, "big"))
	// the row's stamp the restarted watcher reads frozenAt from
	card.CreatedAt = base
	store.tasks[card.ID] = card
	// the class resumes on its own: the face names the close stamp
	now = base.Add(90 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: repo.String(), At: now, Kind: "class_resumed",
		Class: "big", Reset: now.Format(time.RFC3339)})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, _, _ := store.Task(ctx, card.ID)
	if !strings.Contains(resumed.Objective, "The card closes itself at "+stamp(now.Add(freezeResumeHold))) {
		t.Fatalf("the resumed face lacks its close stamp:\n%s", resumed.Objective)
	}
	// the restart: a fresh watcher, six minutes after the resume
	now = now.Add(6 * time.Minute)
	w2 := freezeWatcher(store, forgejo, &now)
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if t2, _, _ := store.Task(ctx, card.ID); t2.Status != "done" {
		t.Fatalf("the card stands %s after the restart, want closed at its five-minute mark", t2.Status)
	}
	if !slices.Contains(fj.closed, card.IssueNumber) {
		t.Errorf("issue #%d stands open", card.IssueNumber)
	}
}

// A restart mid-freeze: a fresh watcher over the same store re-seeds the
// class from the card's row and face, so past waits.provider_limit the
// one-time ask fires exactly once.
func TestFreezeCardRestartMidFreezeSpeaksOnce(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "big frozen"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, _, _ := store.Task(ctx, cardOfClass(store, repo, "big"))
	// the row's stamp the restart reads frozenAt from
	card.CreatedAt = base
	store.tasks[card.ID] = card
	// the restart: a fresh watcher a day later, no events behind the cursor
	now = base.Add(25 * time.Hour)
	w2 := freezeWatcher(store, forgejo, &now)
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	asks := 0
	for _, c := range fj.comments {
		if strings.Contains(c, "has held past waits.provider_limit") {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("the freeze card asked %d times after the restart, want once", asks)
	}
	spoken, _, _ := store.Task(ctx, card.ID)
	if !strings.Contains(spoken.Objective, "Add a fallback") {
		t.Errorf("the ask is not on the card's face:\n%s", spoken.Objective)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("a question was asked: %+v", e)
		}
	}
}

// A restart mid-freeze adopts the reset on the card's face: the class_frozen
// a restarted gateway writes when its first call meets the reset the card
// already holds is the same freeze, so the fresh watcher neither rewrites
// the face nor comments a re-arm. The provider's words, which the face
// shows first, name other resets in the face's own wording, mid-line and
// on a line of their own; the face's reset line is read. A new reset still
// re-arms the card.
func TestFreezeCardRestartKeepsTheResetOnItsFace(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	const words = "quota exhausted. The class resets at 2026-11-21T12:30:00Z (in 0h 30m).\nThe class resets at 2026-11-21T13:00:00Z (in 1h 00m)."
	arm := func(reset time.Time) {
		store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: "class_frozen",
			Class: "big", Reset: reset.Format(time.RFC3339), Words: words})
	}
	arm(base.Add(5 * time.Hour))
	if err := freezeWatcher(store, forgejo, &now).Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, _, _ := store.Task(ctx, cardOfClass(store, Repo{Owner: "sf", Name: "acme"}, "big"))
	// the row's stamp the restart reads frozenAt from
	card.CreatedAt = base
	store.tasks[card.ID] = card
	now = base.Add(10 * time.Minute)
	w := freezeWatcher(store, forgejo, &now)
	arm(base.Add(5 * time.Hour))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(ctx, card.ID); got.Objective != card.Objective || len(fj.comments) != 0 {
		t.Fatalf("the same reset after the restart rewrote the face or commented: comments %q, face:\n%s", fj.comments, got.Objective)
	}
	arm(base.Add(6 * time.Hour))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(ctx, card.ID); !strings.Contains(got.Objective, "in 5h 50m") || len(fj.comments) != 1 ||
		!strings.Contains(fj.comments[0], "the hold re-armed, to the reset 2026-11-21T18:00:00Z") {
		t.Fatalf("the new reset after the restart: comments %q, face:\n%s", fj.comments, got.Objective)
	}
}

// An arm whose repository the watcher holds in memory without its card
// adopts the class's open card off the store, and the reset on its face
// with it: an arm that repeats that reset re-arms nothing, so the face
// stands and its issue takes no comment.
func TestFreezeCardArmAdoptsTheResetOfTheOpenCard(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	arm := func() {
		store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: "class_frozen",
			Class: "big", Reset: reset, Words: "free quota exhausted"})
	}
	arm()
	if err := freezeWatcher(store, forgejo, &now).Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, _, _ := store.Task(ctx, cardOfClass(store, Repo{Owner: "sf", Name: "acme"}, "big"))
	card.CreatedAt = base
	store.tasks[card.ID] = card
	now = base.Add(10 * time.Minute)
	w := freezeWatcher(store, forgejo, &now)
	w.classes = map[string]*freezeClass{"sf/acme/big": {repo: "sf/acme", class: "big"}}
	arm()
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(ctx, card.ID); got.Objective != card.Objective || len(fj.comments) != 0 || w.classes["sf/acme/big"].taskID != card.ID {
		t.Fatalf("the adopting arm: memory %+v, comments %q, face:\n%s", w.classes["sf/acme/big"], fj.comments, got.Objective)
	}
}

// The gateway's hold is the class's in every repository, and its lapse
// writes one class_resumed, on the repository of the call that found it
// lapsed. Two repositories hold an open card of the big class; the one
// class_resumed on sf/acme resumes both: the held time on each face, then
// five minutes, then each closes on its own.
func TestFreezeCardSelfResumeClosesEveryRepositorysCard(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	const words = "free quota exhausted, resets 2026-11-22 01:04:50"
	for i, repo := range []string{"sf/acme", "sf/other"} {
		store.rows = append(store.rows, FreezeEvent{ID: int64(i + 1), Repository: repo, At: base, Kind: "class_frozen",
			Class: "big", Reset: base.Add(time.Duration(5+i) * time.Hour).Format(time.RFC3339), Words: words})
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cards := map[string]Task{}
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard {
			cards[c.Repository] = c
		}
	}
	if len(cards) != 2 || len(fj.issues) != 2 {
		t.Fatalf("freeze cards = %+v over %d issues, want one in each repository", cards, len(fj.issues))
	}
	now = base.Add(90 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_resumed",
		Class: "big", Reset: base.Add(6 * time.Hour).Format(time.RFC3339)})
	stand := func(want string) {
		t.Helper()
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		for repo, c := range cards {
			got, _, _ := store.Task(ctx, c.ID)
			if got.Status != want || !strings.Contains(got.Objective, "the freeze held 1h 30m") {
				t.Fatalf("at %s %s's card stands %s, want %s on the resumed face:\n%s", now, repo, got.Status, want, got.Objective)
			}
		}
	}
	stand("waiting-human")
	now = now.Add(4 * time.Minute)
	stand("waiting-human")
	now = now.Add(2 * time.Minute)
	stand("done")
	for repo, c := range cards {
		if !slices.Contains(fj.closed, c.IssueNumber) {
			t.Errorf("%s's issue #%d stands open", repo, c.IssueNumber)
		}
	}
}

// twoRepoCards files the big class's open card in sf/acme and in sf/other
// at base: two arms, the second with the later reset base+6h, which every
// open card of the class takes.
func twoRepoCards(t *testing.T, store *freezeStore, w *FreezeCard, base time.Time) map[string]Task {
	t.Helper()
	for i, repo := range []string{"sf/acme", "sf/other"} {
		store.rows = append(store.rows, FreezeEvent{ID: int64(i + 1), Repository: repo, At: base, Kind: "class_frozen",
			Class: "big", Reset: base.Add(time.Duration(5+i) * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	}
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	cards := map[string]Task{}
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard {
			cards[c.Repository] = c
		}
	}
	if len(cards) != 2 {
		t.Fatalf("freeze cards = %+v, want one in each repository", cards)
	}
	return cards
}

// The class resumes at 13:30, which starts both repositories' five-minute
// closes; at 13:32, inside them, one of sf/acme's cards meets a dated
// refusal and the class re-arms to 22:00. The arm re-arms both cards: each
// face holds the new reset (in 8h 28m) again, and neither closes at 13:35,
// nor after a restart of the watcher.
func TestFreezeCardReArmCancelsEveryRepositorysPendingClose(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	cards := twoRepoCards(t, store, w, base)
	now = base.Add(90 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_resumed", Class: "big"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(92 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 4, Repository: "sf/acme", At: now, Kind: "class_frozen",
		Class: "big", Reset: base.Add(10 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	stand := func(w *FreezeCard) {
		t.Helper()
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		for repo, c := range cards {
			got, _, _ := store.Task(ctx, c.ID)
			if got.Status != "waiting-human" || !strings.Contains(got.Objective, "in 8h 28m") || strings.Contains(got.Objective, "closes itself at") {
				t.Fatalf("at %s %s's card stands %s, want waiting on the re-armed face:\n%s", now, repo, got.Status, got.Objective)
			}
		}
	}
	stand(w)
	now = base.Add(97 * time.Minute)
	stand(w)
	now = base.Add(100 * time.Minute)
	stand(freezeWatcher(store, forgejo, &now))
	if len(fj.closed) != 0 {
		t.Fatalf("closed issues %v, want none while the class is frozen", fj.closed)
	}
}

// failOnce fails the first face write of card id, as a store outage does.
type failOnce struct {
	*freezeStore
	id     string
	failed bool
}

func (s *failOnce) SetObjective(ctx context.Context, id, objective string) error {
	if id == s.id && !s.failed {
		s.failed = true
		return errors.New("the store is down")
	}
	return s.freezeStore.SetObjective(ctx, id, objective)
}

// The class_resumed row of 13:30 reaches the watcher at 13:31, and sf/other's
// face write fails once; the watcher reads the row again at 13:33. Both
// cards then hold the same resume: held 1h 30m, closing at 13:35, the row's
// own time plus five minutes, and both close then.
func TestFreezeCardResumeStampsEveryCardFromTheRow(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	cards := twoRepoCards(t, store, w, base)
	f := &failOnce{freezeStore: store, id: cards["sf/other"].ID}
	w.Store = f
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: base.Add(90 * time.Minute), Kind: "class_resumed", Class: "big"})
	now = base.Add(91 * time.Minute)
	if err := w.Check(ctx); err == nil || !f.failed {
		t.Fatalf("Check = %v, want sf/other's failed write", err)
	}
	now = base.Add(93 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want := "the freeze held 1h 30m. The card closes itself at " + stamp(base.Add(95*time.Minute))
	for repo, c := range cards {
		if got, _, _ := store.Task(ctx, c.ID); !strings.Contains(got.Objective, want) {
			t.Fatalf("%s's face, want %q:\n%s", repo, want, got.Objective)
		}
	}
	now = base.Add(95 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	for repo, c := range cards {
		if got, _, _ := store.Task(ctx, c.ID); got.Status != "done" || !slices.Contains(fj.closed, c.IssueNumber) {
			t.Fatalf("%s's card stands %s at 13:35, want closed", repo, got.Status)
		}
	}
}

// An arm whose reset the card already holds re-arms it whenever the card
// does not stand open on that freeze. Right after an early press the
// provider still limits to the same moment: that class_frozen reopens the
// card the press closed, with the frozen face, and says so. Inside a
// resume's five minutes — the recheck lapse's resume and its probe — it
// cancels the close and brings the frozen face back, unsaid. On an open,
// frozen card it is a true duplicate and writes nothing.
func TestFreezeCardSameResetArm(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	for name, between := range map[string]func(ctx context.Context, store *freezeStore, w *FreezeCard, now *time.Time) error{
		"after the press": func(ctx context.Context, store *freezeStore, w *FreezeCard, now *time.Time) error {
			if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
				return err
			}
			store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: *now, Kind: "class_resumed", Class: "big", Reset: reset})
			return w.Check(ctx)
		},
		"inside a resume's close": func(ctx context.Context, store *freezeStore, w *FreezeCard, now *time.Time) error {
			store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: *now, Kind: "class_resumed", Class: "big", Reset: reset})
			return w.Check(ctx)
		},
		"a duplicate": nil,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			arm := func(id int64) {
				store.rows = append(store.rows, FreezeEvent{ID: id, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"})
				if err := w.Check(ctx); err != nil {
					t.Fatal(err)
				}
			}
			arm(1)
			if len(store.tasks) != 1 {
				t.Fatalf("tasks = %+v, want the one freeze card", store.tasks)
			}
			var card Task
			for _, c := range store.tasks {
				card = c
			}
			now = base.Add(time.Hour)
			if between != nil {
				if err := between(ctx, store, w, &now); err != nil {
					t.Fatal(err)
				}
			}
			before, _, _ := store.Task(ctx, card.ID)
			comments := len(fj.comments)
			now = base.Add(time.Hour + 2*time.Minute)
			arm(3)
			got, _, _ := store.Task(ctx, card.ID)
			if between == nil {
				if got.Objective != before.Objective || len(fj.comments) != comments || len(fj.issues) != 1 {
					t.Fatalf("the duplicate wrote: face changed %t, comments %d -> %d, issues %d", got.Objective != before.Objective, comments, len(fj.comments), len(fj.issues))
				}
				return
			}
			said := comments + 1
			if name == "inside a resume's close" {
				said = comments
			}
			if got.Status != "waiting-human" || !strings.Contains(got.Objective, "in 3h 58m") || strings.Contains(got.Objective, "closes itself at") ||
				len(fj.comments) != said || len(fj.issues) != 1 {
				t.Fatalf("the card stands %s after the same-reset arm over %d issues, comments %d -> %d:\n%s", got.Status, len(fj.issues), comments, len(fj.comments), got.Objective)
			}
			now = base.Add(time.Hour + 6*time.Minute)
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if still, _, _ := store.Task(ctx, card.ID); still.Status != "waiting-human" {
				t.Fatalf("the card stands %s six minutes on, want it holding the freeze", still.Status)
			}
		})
	}
}

// freezeCardOn is the freeze card of issue number in the store.
func freezeCardOn(t *testing.T, store *freezeStore, number int64) Task {
	t.Helper()
	for _, c := range store.tasks {
		if c.Source == SourceFreezeCard && c.IssueNumber == number {
			return c
		}
	}
	t.Fatalf("no freeze card on issue #%d", number)
	return Task{}
}

// commentsSaying counts the comments of fj that are exactly said.
func commentsSaying(fj *alertForgejo, said string) int {
	n := 0
	for _, c := range fj.comments {
		if c == said {
			n++
		}
	}
	return n
}

// Hand-derived from the press's episode: the press at 13:00 closes the
// card whose face holds the reset 17:00. A dated refusal three days later
// lies past that reset: a new episode, which files a new card and leaves
// the pressed one done and unsaid — in the watcher that pressed and, by
// the card's face, in a fresh one after a restart.
func TestFreezeCardPressEpisodeEndsAtItsReset(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, restart := range map[string]bool{"in the watcher that pressed": false, "after a restart": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big",
				Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			now = base.Add(time.Hour)
			if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
				t.Fatal(err)
			}
			pressed := freezeCardOn(t, store, 1)
			if pressed.Status != "done" || !strings.Contains(pressed.Objective, "The class resets at 2026-11-21T17:00:00Z (the hold's reset)") {
				t.Fatalf("the pressed card stands %s:\n%s", pressed.Status, pressed.Objective)
			}
			if restart {
				w = freezeWatcher(store, forgejo, &now)
			}
			comments := len(fj.comments)
			now = base.Add(72 * time.Hour)
			store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: now.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if old := freezeCardOn(t, store, 1); old.Status != "done" || old.Objective != pressed.Objective || len(fj.comments) != comments || len(fj.issues) != 2 {
				t.Fatalf("the freeze three days on left the pressed card %s over %d issues, comments %q:\n%s", old.Status, len(fj.issues), fj.comments[comments:], old.Objective)
			}
			if fresh := freezeCardOn(t, store, 2); fresh.Status != "waiting-human" || !strings.Contains(fresh.Objective, "in 5h 00m") {
				t.Fatalf("the new episode's card stands %s:\n%s", fresh.Status, fresh.Objective)
			}
		})
	}
}

// Hand-derived from the shared hold: sf/acme and sf/other each hold the big
// class's card to the reset 17:00, and the press closes both. sf/acme's
// dated refusal at 13:00, to 18:00, lies inside both cards' reset: it
// reopens both repositories' cards, each with the re-arm's word and the new
// reset, and files none — in the watcher that pressed and, by the cards'
// faces, after a restart.
func TestFreezeCardArmReopensEveryPressClosedCard(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, restart := range map[string]bool{"in the watcher that pressed": false, "after a restart": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			for i, repo := range []string{"sf/acme", "sf/other"} {
				store.rows = append(store.rows, FreezeEvent{ID: int64(i + 1), Repository: repo, At: base, Kind: "class_frozen", Class: "big",
					Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			}
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if len(fj.issues) != 2 || !slices.Equal(fj.repos, []string{"sf/acme", "sf/other"}) {
				t.Fatalf("issues %q in %q, want one card in each repository", fj.issues, fj.repos)
			}
			now = base.Add(30 * time.Minute)
			for _, repo := range []string{"sf/acme", "sf/other"} {
				if err := w.CloseOnRetry(ctx, repo, "big", "sebastian"); err != nil {
					t.Fatal(err)
				}
			}
			if restart {
				w = freezeWatcher(store, forgejo, &now)
			}
			now = base.Add(time.Hour)
			store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: base.Add(6 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			for _, n := range []int64{1, 2} {
				c := freezeCardOn(t, store, n)
				said := fmt.Sprintf("#%d factoryd: the hold re-armed after the retry, to the reset 2026-11-21T18:00:00Z. 0 cards of the class wait.", n)
				if c.Status != "waiting-human" || !strings.Contains(c.Objective, "in 5h 00m") || commentsSaying(fj, said) != 1 {
					t.Fatalf("#%d stands %s after sf/acme's re-arm, comments %q:\n%s", n, c.Status, fj.comments, c.Objective)
				}
			}
			if len(fj.issues) != 2 {
				t.Fatalf("the re-arm filed a card: %d issues", len(fj.issues))
			}
		})
	}
}

// Hand-derived from a card that closes, reopens and closes again: each
// close is its own and says so. The press at 12:10 closes #1, a dated
// refusal at 12:20 reopens it, the same presser's second press at 12:30
// closes it again, with the same words: the issue carries the press's
// close twice. A refusal at 12:40 reopens it once more, the class resumes
// on its own at 12:50, and five minutes on its close says the freeze ended.
func TestFreezeCardEveryCloseSaysItsOwn(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	row := func(id int64, kind string, at time.Duration) {
		t.Helper()
		now = base.Add(at)
		store.rows = append(store.rows, FreezeEvent{ID: id, Repository: "sf/acme", At: now, Kind: kind, Class: "big",
			Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	press := func(at time.Duration) {
		t.Helper()
		now = base.Add(at)
		if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
			t.Fatal(err)
		}
	}
	row(1, "class_frozen", 0)
	press(10 * time.Minute)
	row(2, "class_frozen", 20*time.Minute)
	press(30 * time.Minute)
	pressSaid := "#1 factoryd: sebastian's retry press cleared the big class's hold: the freeze card closes at once."
	if n := commentsSaying(fj, pressSaid); n != 2 || freezeCardOn(t, store, 1).Status != "done" {
		t.Fatalf("after the second press the card stands %s with %d press closes: %q", freezeCardOn(t, store, 1).Status, n, fj.comments)
	}
	row(3, "class_frozen", 40*time.Minute)
	row(4, "class_resumed", 50*time.Minute)
	now = base.Add(56 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	endSaid := "#1 factoryd: The freeze of the big class ended: the freeze held 0h 10m, and the card closes."
	if c := freezeCardOn(t, store, 1); c.Status != "done" || commentsSaying(fj, endSaid) != 1 || len(fj.issues) != 1 {
		t.Fatalf("the self-resume's close left the card %s over %d issues, comments %q", c.Status, len(fj.issues), fj.comments)
	}
}

// Hand-derived from a long freeze's recheck lapse: the card asked once at
// 24 hours. The gateway's recheck lapse then writes class_resumed and its
// probe the same reset again: the frozen face comes back with the ask on
// it, and nothing is said, neither a re-arm nor a second ask.
func TestFreezeCardRecheckLapseStaysQuiet(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	reset := base.Add(30 * time.Hour).Format(time.RFC3339)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(25 * time.Hour)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "has held past waits.provider_limit") {
		t.Fatalf("comments %q, want the one ask", fj.comments)
	}
	store.rows = append(store.rows,
		FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_resumed", Class: "big", Reset: reset, Words: "free quota exhausted"},
		FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	c := freezeCardOn(t, store, 1)
	if c.Status != "waiting-human" || !strings.Contains(c.Objective, "in 5h 00m") || !strings.Contains(c.Objective, "Add a fallback") ||
		strings.Contains(c.Objective, freezeResumedSentence) || len(fj.comments) != 1 {
		t.Fatalf("after the lapse the card stands %s, comments %q:\n%s", c.Status, fj.comments, c.Objective)
	}
}

// Hand-derived from a press whose issue close Forgejo refuses: the card
// stays open on its frozen face, the press answers the failure, and the
// press's class_resumed starts the card's five-minute close: at 12:16 the
// card and its issue close.
func TestFreezeCardPressCloseFailureClosesLater(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(10 * time.Minute)
	fj.failClose = 1
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err == nil {
		t.Fatal("the press whose close Forgejo refused answered no error")
	}
	if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || !strings.Contains(c.Objective, "The class resets at "+reset+" (in 5h 00m)") {
		t.Fatalf("after the refused close the card stands %s:\n%s", c.Status, c.Objective)
	}
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_resumed", Class: "big", Reset: reset, Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(16 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c := freezeCardOn(t, store, 1); c.Status != "done" || !slices.Contains(fj.closed, int64(1)) {
		t.Fatalf("five minutes after the resume the card stands %s, closed issues %v", c.Status, fj.closed)
	}
}

// Hand-derived from the watcher meeting the press half way: the press's
// class_resumed lands and the watcher writes the resumed face before the
// press closes the card. The press writes its own face, with the reset 17:00
// the watcher held, so after a restart the probe's dated refusal before it
// reopens the same card: no second card.
func TestFreezeCardPressAfterAResumeKeepsItsEpisode(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(10 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_resumed", Class: "big", Reset: reset, Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	want := freezePressedBody("big", "sebastian", now, reset)
	if c := freezeCardOn(t, store, 1); c.Status != "done" || c.Objective != want {
		t.Fatalf("the pressed card stands %s:\n%s\nwant the press's face:\n%s", c.Status, c.Objective, want)
	}
	w = freezeWatcher(store, forgejo, &now)
	now = base.Add(12 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
		Reset: base.Add(6 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || len(fj.issues) != 1 {
		t.Fatalf("the refusal after the restart left the card %s over %d issues", c.Status, len(fj.issues))
	}
}

// Hand-derived from the webhook outrunning the watcher: each close's
// webhook moves the card to done before the watcher does. The press at
// 12:10 still succeeds and keeps its card closed in memory, so the
// same-reset refusal at 12:20 reopens it; the self-resume at 12:30 closes
// it at 12:36 with no error and wakes the frozen card t1.
func TestFreezeCardCloseOutrunByItsWebhook(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	store.webhookOnClose = true
	var woke []string
	w.Signal = func(_ context.Context, id string) error { woke = append(woke, id); return nil }
	if err := store.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Track: policy.TrackDirect, Status: "running", Repository: "sf/acme",
		RequiredCapabilities: []string{"big"}}); err != nil {
		t.Fatal(err)
	}
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	row := func(id int64, kind string, at time.Duration) {
		t.Helper()
		now = base.Add(at)
		store.rows = append(store.rows, FreezeEvent{ID: id, Repository: "sf/acme", At: now, Kind: kind, Class: "big", Reset: reset, Words: "free quota exhausted"})
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	row(1, "class_frozen", 0)
	now = base.Add(10 * time.Minute)
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
		t.Fatalf("the press the webhook outran = %v, want it done", err)
	}
	if c := freezeCardOn(t, store, 1); c.Status != "done" || !strings.Contains(c.Objective, freezePressedSentence) {
		t.Fatalf("the pressed card stands %s:\n%s", c.Status, c.Objective)
	}
	row(2, "class_frozen", 20*time.Minute)
	if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || commentsSaying(fj, "#1 factoryd: the hold re-armed after the retry, to the reset "+reset+". 1 cards of the class wait.") != 1 {
		t.Fatalf("the refusal inside the reset left the card %s, comments %q", c.Status, fj.comments)
	}
	row(3, "class_resumed", 30*time.Minute)
	now = base.Add(36 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatalf("the self-resume's close the webhook outran = %v, want it done", err)
	}
	if c := freezeCardOn(t, store, 1); c.Status != "done" || !slices.Equal(woke, []string{"t1"}) {
		t.Fatalf("after the resume held the card stands %s and the switch woke %q, want done and t1 woken", c.Status, woke)
	}
}

// Hand-derived from a self-resume at 12:00: sf/acme's card holds the big
// class's freeze over t1 and t2, running in sf/acme, t3, queued there, and
// t4, running in sf/other, whose calls the hold answered and which so has
// no card of its own. A resume that holds five minutes means the lapse's
// probe was served: at 12:05 the card closes and the running frozen cards
// of every repository wake, once, however often the watcher checks on. A
// class_frozen inside the five minutes, with the same reset or a new one,
// means the class froze again: nothing wakes.
func TestFreezeCardResumeThatHoldsWakesTheFrozenCards(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	for name, again := range map[string]string{"a resume that holds": "", "a same-reset arm inside the window": reset,
		"a new-reset arm inside the window": base.Add(6 * time.Hour).Format(time.RFC3339)} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, _ := newAlertForgejo(t)
			now := base.Add(-time.Hour)
			w := freezeWatcher(store, forgejo, &now)
			var woke []string
			w.Signal = func(_ context.Context, id string) error { woke = append(woke, id); return nil }
			for _, c := range []Task{{ID: "t1", Status: "running", Repository: "sf/acme"}, {ID: "t2", Status: "running", Repository: "sf/acme"},
				{ID: "t3", Status: "queued", Repository: "sf/acme"}, {ID: "t4", Status: "running", Repository: "sf/other"}} {
				c.Kind, c.Track, c.RequiredCapabilities = "agent", policy.TrackDirect, []string{"big"}
				if err := store.CreateTask(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			store.rows = append(store.rows,
				FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"},
				FreezeEvent{ID: 2, Repository: "sf/acme", At: base, Kind: "class_resumed", Class: "big", Reset: reset, Words: "free quota exhausted"})
			// each row lands on the pass at its own time: the resume's at
			// 12:00, the arm's at 12:02, three minutes into the close
			for _, at := range []time.Duration{0, 2 * time.Minute, 5 * time.Minute, 6 * time.Minute} {
				now = base.Add(at)
				if at == 2*time.Minute && again != "" {
					store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
						Reset: again, Words: "free quota exhausted"})
				}
				if err := w.Check(ctx); err != nil {
					t.Fatal(err)
				}
				if c := freezeCardOn(t, store, 1); at == 0 && !strings.Contains(c.Objective, freezeResumedSentence) {
					t.Fatalf("the resume's pass left the face:\n%s", c.Objective)
				}
			}
			want := []string(nil)
			if again == "" {
				want = []string{"t1", "t2", "t4"}
			}
			if slices.Sort(woke); !slices.Equal(woke, want) {
				t.Fatalf("woke %q, want %q", woke, want)
			}
		})
	}
}

// Hand-derived from a reopen whose Forgejo call fails: the press closed
// #1 at 12:10, and the dated refusal at 12:20 meets a refused reopen. The
// pass fails and leaves the card done; the next pass reopens it, with one
// re-arm comment, and the watcher runs on.
func TestFreezeCardFailedReopenTriesAgain(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big",
		Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(10 * time.Minute)
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	now = base.Add(20 * time.Minute)
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
		Reset: base.Add(6 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	fj.failClose = 1
	if err := w.Check(ctx); err == nil {
		t.Fatal("the pass whose reopen Forgejo refused answered no error")
	}
	if c := freezeCardOn(t, store, 1); c.Status != "done" {
		t.Fatalf("after the refused reopen the card stands %s, want done until its issue opens", c.Status)
	}
	now = base.Add(21 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatalf("the next pass = %v, want the reopen done", err)
	}
	said := "#1 factoryd: the hold re-armed after the retry, to the reset 2026-11-21T18:00:00Z. 0 cards of the class wait."
	if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || commentsSaying(fj, said) != 1 {
		t.Fatalf("the next pass left the card %s, comments %q", c.Status, fj.comments)
	}
}

// Hand-derived from a long freeze that re-arms: the card asked once at
// 24 hours; a re-arm at 26 hours to a later reset keeps the episode, so
// the card keeps its ask on its face and asks no second time.
func TestFreezeCardAsksOnceAcrossReArms(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big",
		Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	for i, at := range []time.Duration{0, 25 * time.Hour, 26 * time.Hour, 27 * time.Hour} {
		now = base.Add(at)
		if i == 2 {
			store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: base.Add(40 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
		}
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	asks := 0
	for _, c := range fj.comments {
		if strings.Contains(c, "has held past waits.provider_limit") {
			asks++
		}
	}
	if c := freezeCardOn(t, store, 1); asks != 1 || !strings.Contains(c.Objective, "in 14h 00m") || !strings.Contains(c.Objective, "Add a fallback") {
		t.Fatalf("the card asked %d times, comments %q:\n%s", asks, fj.comments, c.Objective)
	}
}

// Hand-derived from a press-closed card whose issue was deleted: #1 of the
// big class, closed by the press at 12:10, then deleted in Forgejo. At
// 12:20 one pass reads big's arm, whose reopen Forgejo answers 404, and
// small's first arm. The refusal stops nothing: the pass ends with no
// error and the cursor past both rows, small's card #2 files, and #1 is
// given up, its refusal on its face and on its row. Big's next arm at
// 12:30 files a new card, #3.
func TestFreezeCardRefusedReopenStopsNothing(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	w.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: store.fakeStore, Store: store.fakeStore}
	arm := func(id int64, class string, reset time.Duration) FreezeEvent {
		return FreezeEvent{ID: id, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: class, Reset: base.Add(reset).Format(time.RFC3339), Words: "free quota exhausted"}
	}
	store.rows = append(store.rows, arm(1, "big", 5*time.Hour))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(10 * time.Minute)
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	fj.deleted = map[int64]bool{1: true}
	now = base.Add(20 * time.Minute)
	store.rows = append(store.rows, arm(2, "big", 5*time.Hour), arm(3, "small", 5*time.Hour))
	if err := w.Check(ctx); err != nil {
		t.Fatalf("the pass with the refused reopen = %v, want it to end", err)
	}
	given := freezeCardOn(t, store, 1)
	if store.storedCursor != 3 || freezeCardOn(t, store, 2).Status != "waiting-human" || given.Status != "done" ||
		!strings.Contains(given.Objective, "Forgejo refused this card's issue #1 for good") || strings.Contains(given.Objective, freezePressedSentence) {
		t.Fatalf("after the pass: cursor %d, #1 %s:\n%s", store.storedCursor, given.Status, given.Objective)
	}
	if !slices.ContainsFunc(store.events, func(e Event) bool {
		return e.Kind == EventFreezeIssueRefused && e.TaskID == given.ID && e.Detail["class"] == "big" && e.Detail["issue"] == int64(1)
	}) {
		t.Fatalf("events %+v, want the refusal recorded on #1", store.events)
	}
	now = base.Add(30 * time.Minute)
	store.rows = append(store.rows, arm(4, "big", 6*time.Hour))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c := freezeCardOn(t, store, 3); len(fj.issues) != 3 || c.Status != "waiting-human" || !objectiveHasClassLine(c.Objective, "big") {
		t.Fatalf("big's next arm left %d issues, #3 %s:\n%s", len(fj.issues), c.Status, c.Objective)
	}
}
