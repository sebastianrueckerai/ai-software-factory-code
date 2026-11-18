package factoryd

import (
	"context"
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

// FrozenCards counts the fake's waiting cards of the class, as the store's
// read does.
func (s *freezeStore) FrozenCards(ctx context.Context, repository, class string) (int, error) {
	n := 0
	for _, t := range s.tasks {
		if t.Repository == repository && slices.Contains(t.RequiredCapabilities, class) && slices.Contains([]string{"running", "queued"}, t.Status) {
			n++
		}
	}
	return n, nil
}

// freezeWatcher is a watcher with a fixed clock over store.
func freezeWatcher(store *freezeStore, forgejo *Forgejo, now *time.Time) *FreezeCard {
	return &FreezeCard{Store: store, Forgejo: forgejo, now: func() time.Time { return *now },
		Current: func() policy.Version {
			return policy.Version{Tree: policy.Tree{Document: policy.Document{Waits: policy.Waits{ProviderLimit: 24 * time.Hour}}}}
		}}
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
	if err := w.CloseOnRetry(ctx, repo.String(), "big"); err != nil {
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
	if err := w.CloseOnRetry(ctx, repo.String(), "big"); err != nil {
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
	if err := w.CloseOnRetry(ctx, repo.String(), "big"); err != nil {
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
