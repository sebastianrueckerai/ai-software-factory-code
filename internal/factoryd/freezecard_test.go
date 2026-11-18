package factoryd

import (
	"context"
	"errors"
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
	for _, want := range []string{"Model calls of the big class wait for the provider's limit to reset", "Class: big", words, "in 4h 59m", "9 cards of the class wait"} {
		if !strings.Contains(fj.issues[0], want) {
			t.Errorf("the card's face lacks %q:\n%s", want, fj.issues[0])
		}
	}
	if got := fj.labels[card.IssueNumber]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	// the card asks once, with its filing, and the work cards ask nothing
	if asks := freezeAsks(t, store); asks[card.ID] != 1 || len(asks) != 1 {
		t.Fatalf("the asks = %v, want the freeze card's one", asks)
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
		if !strings.Contains(c, "The provider's limit has held the ") {
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
	freezeAsks(t, store)
}

// CloseFreezeCard closes the class's open freeze card's row, as the
// store's close does: the whole-line Class marker matches the class's
// own card only.
func (s *freezeStore) CloseFreezeCard(ctx context.Context, repository, class string) (Task, bool, error) {
	for id, t := range s.tasks {
		if t.Repository != repository || t.Source != SourceFreezeCard || t.Status != "waiting-human" {
			continue
		}
		if !objectiveHasClassLine(t.Objective, class) {
			continue
		}
		t.Status = "done"
		s.tasks[id] = t
		return t, true, nil
	}
	return Task{}, false, nil
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
		Objective: "Model calls of the bigv2 class wait for the provider's limit to reset\n\nClass: bigv2\n\nbody"}
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
	if !slices.ContainsFunc(fj.comments, func(c string) bool { return strings.Contains(c, "holds the class again after the retry") }) {
		t.Errorf("the re-arm said nothing: comments %q", fj.comments)
	}
	// the reopened card stands in Needs you again, so it asks again
	if asks := freezeAsks(t, store); asks[card.ID] != 2 {
		t.Fatalf("the asks = %v, want the reopened card's second", asks)
	}
	// the withdrawn ask keeps what it knew: only the new ask holds the new reset
	var infos []string
	for _, e := range store.events {
		if e.TaskID == card.ID && e.Kind == EventQuestionAsked {
			infos = append(infos, e.Detail["info"].(string))
		}
	}
	if !strings.Contains(infos[0], "in 4h 59m") || strings.Contains(infos[0], "in 8h 59m") || !strings.Contains(infos[1], "in 8h 59m") {
		t.Fatalf("the first ask knows:\n%s\nand the second:\n%s\nwant the first reset in the first and the new one in the second alone", infos[0], infos[1])
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
	if !slices.ContainsFunc(fj.comments, func(c string) bool { return strings.Contains(c, "holds the class again after the retry") }) {
		t.Errorf("the re-arm after the restart said nothing: comments %q", fj.comments)
	}
	if asks := freezeAsks(t, store); asks[card.ID] != 2 {
		t.Fatalf("the asks = %v, want the reopened card's second", asks)
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
		if strings.Contains(c, "The provider's limit has held the ") {
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
	freezeAsks(t, store)
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
		!strings.Contains(fj.comments[0], "The provider's limit holds the class longer, until 2026-11-21T18:00:00Z.") {
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
// card the press closed, with the frozen face. Inside a resume's five
// minutes it cancels the close. On an open, frozen card it is a true
// duplicate and writes nothing.
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
			if got.Status != "waiting-human" || !strings.Contains(got.Objective, "in 3h 58m") || strings.Contains(got.Objective, "closes itself at") ||
				len(fj.comments) != comments+1 || len(fj.issues) != 1 {
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

// freezeAsks counts the ask rows of each freeze card, and fails on an ask
// of any other card: the work cards ask nothing.
func freezeAsks(t *testing.T, store *freezeStore) map[string]int {
	t.Helper()
	asks := map[string]int{}
	for _, e := range store.events {
		if e.Kind != EventQuestionAsked {
			continue
		}
		if e.Detail["source"] != SourceFreezeCard || !slices.Equal(e.Detail["choices"].([]string), []string{"Retry now", "Noted", "Not now"}) {
			t.Fatalf("a question was asked that is no freeze card's: %+v", e)
		}
		asks[e.TaskID]++
	}
	return asks
}

// mainsFreezeFace is the face of a freeze card as the factoryd before the
// card asked wrote it, words the provider's, count the cards it counted.
func mainsFreezeFace(words string, count string) string {
	return "The provider limit freezes the big class\n\n" +
		"Class: big\n\n" +
		"The provider limit freezes the class: every call of the class is answered with the named refusal until the reset.\n\n" +
		"The provider said:\n\n```\n" + words + "\n```\n\n" +
		"The class resets at 2026-11-21T17:00:00Z (in 6h 00m). " + count + " cards of the class wait.\n\n" +
		"This card holds no work slot and asks nothing. It updates on a re-arm, and closes itself five minutes after the class resumes."
}

// A freeze card an older factoryd filed says in its title and its face that
// it asks nothing, and the board answers a card only through its case. The
// watcher that adopts it writes the face as it writes every freeze card's,
// with the class, the reset and the provider's words of the old face and
// the count of now, and asks once with that face; the next watcher, after a
// restart, finds the case and asks no second time.
func TestFreezeCardAdoptedWithNoCaseAsks(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store.tasks["t-old-freeze"] = Task{ID: "t-old-freeze", Repository: "sf/acme", IssueNumber: 41, Kind: "human", Status: "waiting-human",
		Source: SourceFreezeCard, Objective: mainsFreezeFace("quota exhausted", "3"), CreatedAt: now.Add(-time.Hour)}
	for _, id := range []string{"w1", "w2"} {
		store.tasks[id] = Task{ID: id, Kind: "agent", Status: "running", Repository: "sf/acme", RequiredCapabilities: []string{"big"}}
	}
	for range 2 {
		if err := freezeWatcher(store, forgejo, &now).Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	open, _ := store.OpenCasesOf(ctx, "t-old-freeze")
	if len(open) != 1 {
		t.Fatalf("the adopted card holds %d live cases, want 1: %+v", len(open), open)
	}
	var asked []Event
	for _, e := range store.events {
		if e.TaskID == "t-old-freeze" && e.Kind == EventQuestionAsked {
			asked = append(asked, e)
		}
	}
	const face = "Class: big\n\n" +
		"The provider limit freezes the class: every call of the class is answered with the named refusal until the reset.\n\n" +
		"The provider said:\n\n```\nquota exhausted\n```\n\n" +
		"The class resets at 2026-11-21T17:00:00Z (in 5h 00m). 2 cards of the class wait.\n\n" +
		"This card holds no work slot. It updates when the provider names a new reset, and closes itself five minutes after the class resumes."
	if len(asked) != 1 || asked[0].Detail["info"] != face ||
		asked[0].Detail["ask"] != "Model calls of the big class wait for the provider's limit to reset" {
		t.Fatalf("the asks: %+v; want one with the face\n%s", asked, face)
	}
	if got := store.tasks["t-old-freeze"].Objective; got != "Model calls of the big class wait for the provider's limit to reset\n\n"+face {
		t.Errorf("the adopted card's face:\n%s", got)
	}
	if len(fj.comments) != 0 {
		t.Errorf("the adoption commented: %q", fj.comments)
	}
}

// An older factoryd's card that spoke its one-time sentence keeps one such
// sentence on the face the adoption writes, and speaks no second time.
func TestFreezeCardAdoptedAfterItSpokeStaysQuiet(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	now := time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)
	store.tasks["t-old-freeze"] = Task{ID: "t-old-freeze", Repository: "sf/acme", IssueNumber: 41, Kind: "human", Status: "waiting-human",
		Source: SourceFreezeCard, CreatedAt: now.Add(-25 * time.Hour),
		Objective: strings.Replace(mainsFreezeFace("quota exhausted", "3"), "2026-11-21T17:00:00Z", "2026-11-22T17:00:00Z", 1) +
			"\n\nThe freeze of the big class has held past waits.provider_limit (24h0m0s). " +
			"Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing."}
	for range 2 {
		if err := freezeWatcher(store, forgejo, &now).Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := store.tasks["t-old-freeze"].Objective
	if !strings.HasSuffix(got, "\n\nThe provider's limit has held the big class for more than 24 hours. "+
		"Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing.") ||
		strings.Count(got, "Add a fallback") != 1 || strings.Contains(got, "waits.provider_limit") {
		t.Errorf("the adopted card's face:\n%s", got)
	}
	if len(fj.comments) != 0 {
		t.Errorf("the adopted card spoke again: %q", fj.comments)
	}
}

// A watcher that starts after the card spoke reads that off the face: each
// restart leaves the one comment and the one paragraph.
func TestFreezeCardRestartAfterItSpokeStaysQuiet(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "big frozen"})
	if err := freezeWatcher(store, forgejo, &now).Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, _, _ := store.Task(ctx, cardOfClass(store, repo, "big"))
	card.CreatedAt = base
	store.tasks[card.ID] = card
	now = base.Add(25 * time.Hour)
	for range 3 {
		if err := freezeWatcher(store, forgejo, &now).Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	const said = "The provider's limit has held the big class for more than 24 hours."
	comments := 0
	for _, c := range fj.comments {
		comments += strings.Count(c, said)
	}
	spoken, _, _ := store.Task(ctx, card.ID)
	if comments != 1 || strings.Count(spoken.Objective, said) != 1 {
		t.Fatalf("after two restarts: %d comments, want 1, and the face:\n%s", comments, spoken.Objective)
	}
}

// What the freeze card's ask knows is the card's face at each rewrite: the
// re-arm's reset, the one-time sentence and the resume. The re-arm's
// comment names the reset as the face does.
func TestFreezeCardAskKnowsTheFaceAsItStands(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	repo := Repo{Owner: "sf", Name: "acme"}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	info := func() string {
		t.Helper()
		var got string
		for _, e := range store.events {
			if e.Kind == EventQuestionAsked {
				got, _ = e.Detail["info"].(string)
			}
		}
		return got
	}
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: "2026-11-21T17:00:00Z", Words: "big frozen"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := info(); !strings.Contains(got, "The class resets at 2026-11-21T17:00:00Z (in 5h 00m).") {
		t.Fatalf("the filed ask knows:\n%s", got)
	}
	store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: repo.String(), At: base, Kind: "class_frozen",
		Class: "big", Reset: "2026-11-23T05:00:00+08:00", Words: "big frozen"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := info(); !strings.Contains(got, "The class resets at 2026-11-22T21:00:00Z (in 33h 00m).") || strings.Contains(got, "17:00") {
		t.Fatalf("after the re-arm the ask knows:\n%s", got)
	}
	if len(fj.comments) != 1 || fj.comments[0] != "#1 The provider's limit holds the class longer, until 2026-11-22T21:00:00Z. 0 cards of the class wait." {
		t.Fatalf("the re-arm's comment: %q", fj.comments)
	}
	now = base.Add(25 * time.Hour)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, _, _ := store.Task(ctx, cardOfClass(store, repo, "big"))
	if got := info(); !strings.HasSuffix(got, "The provider's limit has held the big class for more than 24 hours. "+
		"Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing.") ||
		card.Objective != "Model calls of the big class wait for the provider's limit to reset\n\n"+got {
		t.Fatalf("after the card spoke the ask knows:\n%s\nand the face:\n%s", got, card.Objective)
	}
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: repo.String(), At: now, Kind: "class_resumed", Class: "big"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := info(); got != "Class: big\n\nThe class resumed on its own at 2026-11-22T13:00:00Z: the freeze held 25h 00m. The card closes itself at 2026-11-22T13:05:00Z." {
		t.Fatalf("after the resume the ask knows:\n%s", got)
	}
}
