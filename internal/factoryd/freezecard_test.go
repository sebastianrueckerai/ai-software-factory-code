package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// freezeStore is the fakeStore with the freeze watcher's reads: the
// gateway's class_frozen and class_resumed rows and the cursor table.
type freezeStore struct {
	*fakeStore
	rows         []FreezeEvent
	storedCursor int64
	// failFace fails a face's write
	failFace bool
	// fault counts the store's writes and fails one; nil counts none
	fault *freezeFault
}

// freezeFault numbers the store's writes and Forgejo's calls of a run and
// fails the one numbered at, once: the fault dimension of the state table.
type freezeFault struct {
	mu    sync.Mutex
	at    int
	ops   []string
	fired bool
	// paused counts nothing: a human's own writes and calls
	paused bool
}

// fail counts op and reports whether it is the one to fail.
func (f *freezeFault) fail(op string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paused {
		return false
	}
	f.ops = append(f.ops, op)
	if len(f.ops) == f.at {
		f.fired = true
		return true
	}
	return false
}

// write counts one store write, a whole method as a transaction is, and
// fails it before it changes anything when it is the fault's.
func (s *freezeStore) write(op string) error {
	if s.fault.fail(op) {
		return errors.New("the store is away")
	}
	return nil
}

func (s *freezeStore) SetObjective(ctx context.Context, id, objective string) error {
	if s.failFace {
		return errors.New("the store is away")
	}
	if err := s.write("SetObjective"); err != nil {
		return err
	}
	return s.fakeStore.SetObjective(ctx, id, objective)
}

func (s *freezeStore) EndFreezeCard(ctx context.Context, t Task, face, comment string) (bool, error) {
	if err := s.write("EndFreezeCard"); err != nil {
		return false, err
	}
	return s.fakeStore.EndFreezeCard(ctx, t, face, comment)
}

func (s *freezeStore) ReopenFreezeCard(ctx context.Context, t Task, face, comment string) error {
	if err := s.write("ReopenFreezeCard"); err != nil {
		return err
	}
	return s.fakeStore.ReopenFreezeCard(ctx, t, face, comment)
}

func (s *freezeStore) SettleFreezeReopen(ctx context.Context, taskID string, issue int64) error {
	if err := s.write("SettleFreezeReopen"); err != nil {
		return err
	}
	return s.fakeStore.SettleFreezeReopen(ctx, taskID, issue)
}

func (s *freezeStore) TransitionTask(ctx context.Context, id string, from []string, to string) error {
	if err := s.write("TransitionTask to " + to); err != nil {
		return err
	}
	return s.fakeStore.TransitionTask(ctx, id, from, to)
}

func (s *freezeStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	if err := s.write("InsertIntakeEvent " + e.Kind); err != nil {
		return 0, err
	}
	return s.fakeStore.InsertIntakeEvent(ctx, e)
}

func (s *freezeStore) ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error {
	if err := s.write("ConfirmFactoryClose"); err != nil {
		return err
	}
	return s.fakeStore.ConfirmFactoryClose(ctx, taskID, issue)
}

func (s *freezeStore) CreateTask(ctx context.Context, t Task) error {
	if err := s.write("CreateTask"); err != nil {
		return err
	}
	return s.fakeStore.CreateTask(ctx, t)
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

func (s *freezeStore) StandingFreezes(ctx context.Context) ([]FreezeEvent, error) {
	armed := map[string][]FreezeEvent{}
	for _, ev := range s.rows {
		switch {
		case ev.ID > s.storedCursor:
		case ev.Kind == "class_resumed":
			delete(armed, ev.Class)
		case ev.Kind == "class_frozen":
			armed[ev.Class] = append(armed[ev.Class], ev)
		}
	}
	var out []FreezeEvent
	for _, arms := range armed {
		newest := slices.MaxFunc(arms, func(a, b FreezeEvent) int { return cmp.Compare(a.ID, b.ID) })
		newest.TaskID = ""
		seen := map[string]bool{}
		for _, arm := range arms {
			if !seen[arm.Repository] {
				seen[arm.Repository] = true
				ev := newest
				ev.Repository = arm.Repository
				out = append(out, ev)
			}
		}
	}
	slices.SortFunc(out, func(a, b FreezeEvent) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Repository, b.Repository))
	})
	return out, nil
}

func (s *freezeStore) SetFreezeCursor(ctx context.Context, cursor int64) error {
	if err := s.write("SetFreezeCursor"); err != nil {
		return err
	}
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
	store.fakeStore.now = func() time.Time { return *now }
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
		if !strings.Contains(c, "past waits.provider_limit") {
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
		if strings.Contains(c, "past waits.provider_limit") {
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
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "past waits.provider_limit") {
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

// Hand-derived from a press whose one write the store refuses: nothing
// changed, so the card stays open on its frozen face and no issue call is
// made, the press answers the failure, and the press's class_resumed
// starts the card's five-minute close: at 12:16 the card and its issue
// close.
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
	store.fault = &freezeFault{at: 1}
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err == nil {
		t.Fatal("the press whose write the store refused answered no error")
	}
	if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || !strings.Contains(c.Objective, "The class resets at "+reset+" (in 5h 00m)") ||
		len(fj.closed) != 0 || len(fj.comments) != 0 {
		t.Fatalf("after the refused write the card stands %s, closed issues %v, comments %q:\n%s", c.Status, fj.closed, fj.comments, c.Objective)
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
	if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || commentsSaying(fj, "#1 factoryd: the hold re-armed after the retry, to the reset "+reset+". 1 card of the class waits.") != 1 {
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
// #1 at 12:10, and the dated refusal at 12:20 opens the card in the store
// and meets a Forgejo that fails the issue's reopen. The pass ends past
// the row with the card waiting, its issue still closed and the reopen
// owed; the next pass opens the issue, with one re-arm comment, and owes
// nothing.
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
	if err := w.Check(ctx); err != nil || store.storedCursor != 2 {
		t.Fatalf("the pass whose reopen Forgejo failed = %v at cursor %d, want it to end past the row", err, store.storedCursor)
	}
	said := "#1 factoryd: the hold re-armed after the retry, to the reset 2026-11-21T18:00:00Z. 0 cards of the class wait."
	owed, err := store.OwedFreezeReopens(ctx)
	if c := freezeCardOn(t, store, 1); err != nil || c.Status != "waiting-human" || len(owed) != 1 || owed[0].CommentID != 1 || fj.states[1] != "closed" || commentsSaying(fj, said) != 0 {
		t.Fatalf("after the failed reopen the card stands %s, its issue %s, owed %+v, %v; want it waiting, the issue closed and its reopen owed", c.Status, fj.states[1], owed, err)
	}
	now = base.Add(21 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatalf("the next pass = %v, want the reopen done", err)
	}
	owed, err = store.OwedFreezeReopens(ctx)
	if c := freezeCardOn(t, store, 1); err != nil || c.Status != "waiting-human" || len(owed) != 0 || fj.states[1] != "open" || commentsSaying(fj, said) != 1 {
		t.Fatalf("the next pass left the card %s, its issue %s, owed %+v, comments %q", c.Status, fj.states[1], owed, fj.comments)
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
		if strings.Contains(c, "past waits.provider_limit") {
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
// error and the cursor past both rows. #1 is given up, its refusal on its
// face and on its row, and the arm in hand files big's replacement #2 in
// the same pass, before small's card #3. Big's next arm at 12:30 updates
// #2.
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
	given, replacement := freezeCardOn(t, store, 1), freezeCardOn(t, store, 2)
	if store.storedCursor != 3 || given.Status != "done" ||
		!strings.Contains(given.Objective, "Forgejo refused this card's issue #1 for good") || strings.Contains(given.Objective, freezePressedSentence) {
		t.Fatalf("after the pass: cursor %d, #1 %s:\n%s", store.storedCursor, given.Status, given.Objective)
	}
	if replacement.Status != "waiting-human" || !objectiveHasClassLine(replacement.Objective, "big") || !strings.Contains(replacement.Objective, "in 4h 40m") ||
		freezeCardOn(t, store, 3).Status != "waiting-human" || !objectiveHasClassLine(freezeCardOn(t, store, 3).Objective, "small") {
		t.Fatalf("after the pass: big's replacement #2 %s:\n%s", replacement.Status, replacement.Objective)
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
	if c := freezeCardOn(t, store, 2); len(fj.issues) != 3 || c.Status != "waiting-human" || !strings.Contains(c.Objective, "in 5h 30m") {
		t.Fatalf("big's next arm left %d issues, #2 %s:\n%s", len(fj.issues), c.Status, c.Objective)
	}
}

// Hand-derived from two open cards whose issues were deleted: big's #1,
// whose class resumed at 13:00, and small's #2, frozen since 12:00. At
// 13:00 the next day big's close meets the 404 and so does small's
// 24-hour ask. Neither stops the pass: both cards end done. Big's close
// is the store's one write, so #1 ends under its resumed face, and
// Forgejo's refusal of the gone issue is logged; big resumed, so its card
// has no successor. Small's ask needs its issue: #2 ends with the refusal
// on its face and its row. Small is still frozen: its replacement #3 files at once with the words
// and the reset, keeps the episode's clock and makes the ask that was due,
// once.
func TestFreezeCardRefusedIssueEndsTheCard(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	w.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: store.fakeStore, Store: store.fakeStore}
	store.rows = append(store.rows,
		FreezeEvent{ID: 1, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "big", Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"},
		FreezeEvent{ID: 2, Repository: "sf/acme", At: base, Kind: "class_frozen", Class: "small", Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	fj.deleted = map[int64]bool{1: true, 2: true}
	now = base.Add(time.Hour)
	store.rows = append(store.rows, FreezeEvent{ID: 3, Repository: "sf/acme", At: now, Kind: "class_resumed", Class: "big", Reset: base.Add(5 * time.Hour).Format(time.RFC3339)})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = base.Add(25 * time.Hour)
	for range 2 {
		if err := w.Check(ctx); err != nil {
			t.Fatalf("the pass whose close and ask Forgejo refused = %v, want it to end", err)
		}
	}
	for n, refused := range map[int64]bool{1: false, 2: true} {
		c := freezeCardOn(t, store, n)
		if c.Status != "done" || strings.Contains(c.Objective, "Forgejo refused this card's issue #"+fmt.Sprint(n)+" for good") != refused ||
			slices.ContainsFunc(store.events, func(e Event) bool { return e.Kind == EventFreezeIssueRefused && e.TaskID == c.ID }) != refused ||
			strings.Contains(c.Objective, freezeResumedSentence) == refused {
			t.Fatalf("#%d stands %s, want it done and the refusal on it %v; events %+v:\n%s", n, c.Status, refused, store.events, c.Objective)
		}
	}
	asks := 0
	for _, c := range fj.comments {
		if strings.HasPrefix(c, "#3 factoryd: The freeze of the small class has held since 2026-11-21T12:00:00Z, past waits.provider_limit") {
			asks++
		}
	}
	if c := freezeCardOn(t, store, 3); len(fj.issues) != 3 || c.Status != "waiting-human" || !objectiveHasClassLine(c.Objective, "small") ||
		!strings.Contains(c.Objective, "free quota exhausted") || !strings.Contains(c.Objective, "The class resets at 2026-11-22T18:00:00Z (in 5h 00m)") ||
		!strings.Contains(c.Objective, "Add a fallback") || asks != 1 {
		t.Fatalf("small's replacement on %d issues stands %s, asked %d times:\n%s", len(fj.issues), c.Status, asks, c.Objective)
	}
}

// Hand-derived from a press-closed card whose bot loses its rights: #1 of
// the big class, closed by the press at 12:10, after which Forgejo answers
// 403 on it. At 12:20 one pass reads big's arm, which opens #1 in the
// store and whose issue reopen meets the 403, and small's first arm: the
// pass ends with no error and the cursor past both, small's #2 files, and
// #1 waits under its holding face with the reopen owed, its issue closed
// and no refusal recorded. The pass at 12:25 meets the 403 again and
// still owes it. Once the credential is fixed, the 12:30 pass opens #1's
// issue with one re-arm comment and no new card. A class that resumes
// before the fix puts #1 on its resumed face, to close itself at 12:31,
// and the 12:30 pass opens the issue of the card that still waits.
func TestFreezeCardForbiddenReopenIsTriedAgain(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, resumes := range map[string]bool{"the credential is fixed": false, "the class resumes before the fix": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			w.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: store.fakeStore, Store: store.fakeStore}
			arm := func(id int64, class string) FreezeEvent {
				return FreezeEvent{ID: id, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: class, Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"}
			}
			pass := func(at time.Duration, rows ...FreezeEvent) {
				t.Helper()
				now = base.Add(at)
				for _, r := range rows {
					r.At = now
					store.rows = append(store.rows, r)
				}
				if err := w.Check(ctx); err != nil {
					t.Fatalf("the pass at %s = %v, want it to end", now.Format("15:04"), err)
				}
			}
			pass(0, arm(1, "big"))
			now = base.Add(10 * time.Minute)
			if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
				t.Fatal(err)
			}
			fj.forbidden = map[int64]bool{1: true}
			for _, at := range []time.Duration{20 * time.Minute, 25 * time.Minute} {
				if at == 20*time.Minute {
					pass(at, arm(2, "big"), arm(3, "small"))
				} else {
					pass(at)
				}
				kept := freezeCardOn(t, store, 1)
				owed, err := store.OwedFreezeReopens(ctx)
				if err != nil || store.storedCursor != 3 || kept.Status != "waiting-human" || strings.Contains(kept.Objective, freezePressedSentence) || len(fj.issues) != 2 ||
					len(owed) != 1 || owed[0].TaskID != kept.ID || fj.states[1] != "closed" ||
					freezeCardOn(t, store, 2).Status != "waiting-human" || slices.ContainsFunc(store.events, func(e Event) bool { return e.Kind == EventFreezeIssueRefused }) {
					t.Fatalf("at %s: cursor %d, %d issues, owed %+v, %v, issue #1 %s, #1 %s:\n%s", now.Format("15:04"), store.storedCursor, len(fj.issues), owed, err, fj.states[1], kept.Status, kept.Objective)
				}
			}
			if resumes {
				pass(26*time.Minute, FreezeEvent{ID: 4, Repository: "sf/acme", Kind: "class_resumed", Class: "big", Reset: base.Add(5 * time.Hour).Format(time.RFC3339)})
			}
			fj.forbidden = nil
			pass(30 * time.Minute)
			said := commentsSaying(fj, "#1 factoryd: the hold re-armed after the retry, to the reset 2026-11-21T17:00:00Z. 0 cards of the class wait.")
			owed, err := store.OwedFreezeReopens(ctx)
			if c := freezeCardOn(t, store, 1); err != nil || c.Status != "waiting-human" || strings.Contains(c.Objective, freezeResumedSentence) != resumes ||
				said != 1 || len(owed) != 0 || fj.states[1] != "open" || len(fj.issues) != 2 {
				t.Fatalf("after the fix #1 stands %s, its issue %s, with %d re-arm comments over %d issues, owed %+v, %v; want it waiting, on the resumed face %v, its issue open with one comment:\n%s",
					c.Status, fj.states[1], said, len(fj.issues), owed, err, resumes, c.Objective)
			}
		})
	}
}

// Hand-derived from filings Forgejo refuses. A 404 on the issue's
// creation means the repository is gone or out of the bot's reach: the
// pass ends with no card and logs what happened and what a human fixes,
// naming no card, and the class's next arm files it once Forgejo takes it.
// A 403 keeps the arm, so the pass after the fix files the card with no
// new arm.
func TestFreezeCardFilingRefusal(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			var said bytes.Buffer
			log.SetOutput(&said)
			fj.fileStatus = status
			store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			err := w.Check(ctx)
			log.SetOutput(os.Stderr) // under the log's lock: no write reaches said past here
			if err != nil || len(fj.issues) != 0 {
				t.Fatalf("the refused filing's pass = %v over %d issues, want it to end with none", err, len(fj.issues))
			}
			fj.fileStatus = 0
			now = base.Add(time.Minute)
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if status == http.StatusForbidden {
				if len(fj.issues) != 1 {
					t.Fatalf("after the fix %d issues, want the kept arm's card", len(fj.issues))
				}
				return
			}
			if text := said.String(); len(fj.issues) != 0 || strings.Contains(text, "#0") ||
				!strings.Contains(text, "Forgejo refuses for good to file the big class's freeze card on sf/acme") ||
				!strings.Contains(text, "a human restores the repository or the bot's access to it, and the class's next arm files the card") {
				t.Fatalf("after the 404: %d issues, log:\n%s", len(fj.issues), text)
			}
			store.rows = append(store.rows, FreezeEvent{ID: 2, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			if err := w.Check(ctx); err != nil || len(fj.issues) != 1 {
				t.Fatalf("the next arm = %v over %d issues, want the card filed", err, len(fj.issues))
			}
		})
	}
}

// Hand-derived from four ends of three freeze cards, each card counted
// once in factory.task.outcome. The press closes big's #1, the watcher
// moving the row (1). A refusal inside its reset opens #1 again, and its
// close five minutes after a self-resume counts nothing more (0). The
// press closes small's #2 with the webhook of that close moving the row
// first, which counts it there (0 here); a refusal opens #2 again and a
// human's close ends it through the webhook, which counts nothing more
// (0). The webhook ends plain's #3, which never ended before (1).
func TestFreezeCardCountsItsEndOnce(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, _ := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	m, reader := testMetrics(t)
	w.Metrics = m
	in := &Intake{Store: store.fakeStore, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Metrics: m}
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	row := func(kind, class string, at time.Duration) {
		t.Helper()
		now = base.Add(at)
		store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: kind, Class: class, Reset: reset, Words: "free quota exhausted"})
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	counted := func() float64 {
		n := 0.0
		for _, c := range sums(t, reader, "factory.task.outcome") {
			n += c
		}
		return n
	}
	row("class_frozen", "big", 0)
	row("class_frozen", "small", 0)
	row("class_frozen", "plain", 0)
	for _, class := range []string{"big", "small"} {
		store.webhookOnClose = class == "small"
		if err := w.CloseOnRetry(ctx, "sf/acme", class, "sebastian"); err != nil {
			t.Fatal(err)
		}
	}
	store.webhookOnClose = false
	if got := counted(); got != 2 {
		t.Fatalf("after the presses the watcher counted %v ends, want 2: each press moved its row", got)
	}
	row("class_frozen", "big", 10*time.Minute)
	row("class_frozen", "small", 10*time.Minute)
	row("class_resumed", "big", 20*time.Minute)
	now = base.Add(26 * time.Minute)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c1, c2 := freezeCardOn(t, store, 1), freezeCardOn(t, store, 2); c1.Status != "done" || c2.Status != "waiting-human" || counted() != 2 {
		t.Fatalf("#1 %s, #2 %s, %v ends counted; want #1 done, #2 open again and the two presses", c1.Status, c2.Status, counted())
	}
	for _, end := range []struct {
		issue int64
		want  float64
	}{{2, 2}, {3, 3}} {
		if err := in.finishHumanTask(ctx, freezeCardOn(t, store, end.issue), "done", "", "sebastian", nil); err != nil {
			t.Fatal(err)
		}
		if c := freezeCardOn(t, store, end.issue); c.Status != "done" || counted() != end.want {
			t.Fatalf("#%d %s after the webhook's end, %v ends counted, want done and %v", end.issue, c.Status, counted(), end.want)
		}
	}
	if got := counted(); got != 3 {
		t.Fatalf("%v ends counted over three cards, want 3: big's press, small's press and plain's close", got)
	}
}

// freezeAsks counts the provider-limit ask on the face of the freeze card
// on issue number and in that issue's comments.
func freezeAsks(t *testing.T, store *freezeStore, fj *alertForgejo, number int64) (face, comments int) {
	t.Helper()
	const ask = "past waits.provider_limit"
	for _, c := range fj.comments {
		if strings.HasPrefix(c, fmt.Sprintf("#%d factoryd: The freeze of the ", number)) && strings.Contains(c, ask) {
			comments++
		}
	}
	return strings.Count(freezeCardOn(t, store, number).Objective, ask), comments
}

// Hand-derived from a 401 that a restart cures, the bot's password being
// read once at start. Big's arm at 12:00 meets the 401 on its filing: no
// card, the cursor past the row. The watcher built anew at 12:10 over the
// same store, Forgejo taking the bot again, files the card on its first
// pass with no new row, once: the pass after files nothing. A watcher
// built anew once more files nothing either. A freeze whose reset passed
// before the restart files nothing.
func TestFreezeCardKeptArmSurvivesARestart(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, restart := range map[string]time.Duration{"the reset is ahead": 10 * time.Minute, "the reset passed": 6 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			fj.fileStatus = http.StatusUnauthorized
			store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			if err := w.Check(ctx); err != nil || len(fj.issues) != 0 || store.storedCursor != 1 {
				t.Fatalf("the refused filing's pass = %v over %d issues at cursor %d, want it to end with none at 1", err, len(fj.issues), store.storedCursor)
			}
			fj.fileStatus = 0
			want := 1
			if restart > 5*time.Hour {
				want = 0
			}
			for i, at := range []time.Duration{restart, restart + time.Minute, restart + 2*time.Minute} {
				now = base.Add(at)
				if i != 1 {
					w = freezeWatcher(store, forgejo, &now)
				}
				if err := w.Check(ctx); err != nil || len(fj.issues) != want {
					t.Fatalf("the pass at %s = %v over %d issues, want %d", now.Format("15:04"), err, len(fj.issues), want)
				}
			}
			if want == 1 {
				if c := freezeCardOn(t, store, 1); c.Status != "waiting-human" || !objectiveHasClassLine(c.Objective, "big") ||
					!strings.Contains(c.Objective, "free quota exhausted") || !strings.Contains(c.Objective, "The class resets at 2026-11-21T17:00:00Z (in 4h 50m)") {
					t.Fatalf("the kept arm's card stands %s:\n%s", c.Status, c.Objective)
				}
			}
		})
	}
}

// Hand-derived from a reopen a restart cures: the press closed big's #1
// at 12:10, and the dated refusal at 12:20 opens #1 in the store and
// meets a 401 on the issue's reopen: #1 waits, its issue closed and the
// reopen owed, the cursor past the row. The watcher built anew at 12:30,
// Forgejo taking the bot again, opens the issue on its first pass with
// one re-arm comment and no second card; the pass after adds nothing.
func TestFreezeCardKeptReopenSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	arm := func(id int64) {
		t.Helper()
		store.rows = append(store.rows, FreezeEvent{ID: id, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
			Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	arm(1)
	now = base.Add(10 * time.Minute)
	if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	fj.refuse = map[int64]int{1: http.StatusUnauthorized}
	now = base.Add(20 * time.Minute)
	arm(2)
	owed, err := store.OwedFreezeReopens(ctx)
	if c := freezeCardOn(t, store, 1); err != nil || c.Status != "waiting-human" || store.storedCursor != 2 || len(owed) != 1 || fj.states[1] != "closed" {
		t.Fatalf("after the refused reopen #1 stands %s at cursor %d, its issue %s, owed %+v, %v; want it waiting at 2, the issue closed and its reopen owed", c.Status, store.storedCursor, fj.states[1], owed, err)
	}
	fj.refuse = nil
	w = freezeWatcher(store, forgejo, &now)
	for _, at := range []time.Duration{30 * time.Minute, 31 * time.Minute} {
		now = base.Add(at)
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		said := commentsSaying(fj, "#1 factoryd: the hold re-armed after the retry, to the reset 2026-11-21T17:00:00Z. 0 cards of the class wait.")
		owed, err := store.OwedFreezeReopens(ctx)
		if c := freezeCardOn(t, store, 1); err != nil || c.Status != "waiting-human" || said != 1 || len(fj.issues) != 1 || len(owed) != 0 || fj.states[1] != "open" {
			t.Fatalf("at %s #1 stands %s, its issue %s, with %d re-arm comments over %d issues, owed %+v, %v; want it open again with one over one", now.Format("15:04"), c.Status, fj.states[1], said, len(fj.issues), owed, err)
		}
	}
}

// Hand-derived from an ask Forgejo refuses: small's #1, frozen at 12:00
// with a reset 30 hours out, owes its ask from 12:00 the next day. The
// passes at 13:00, 13:01 and 13:02 that day meet a 401 on the comment:
// the card is kept, no ask on its face or its issue. With the restart, a
// watcher built anew over the same store makes the pass at 13:03, Forgejo
// taking the bot again; without, the same watcher does. Either way the
// ask stands once on the face and once on the issue, and the passes after
// add none.
func TestFreezeCardKeptAskIsMadeOnce(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, restart := range map[string]bool{"the same watcher": false, "a restarted watcher": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "small",
				Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			fj.refuse = map[int64]int{1: http.StatusUnauthorized}
			pass := func(at time.Duration, wantFace, wantComments int) {
				t.Helper()
				now = base.Add(at)
				if err := w.Check(ctx); err != nil {
					t.Fatalf("the pass at %s = %v, want it to end", now.Format("15:04"), err)
				}
				if face, comments := freezeAsks(t, store, fj, 1); face != wantFace || comments != wantComments || len(fj.issues) != 1 {
					t.Fatalf("at %s the ask stands %d times on the face and %d on the issue over %d issues, want %d and %d on the one card:\n%s",
						now.Format("15:04"), face, comments, len(fj.issues), wantFace, wantComments, freezeCardOn(t, store, 1).Objective)
				}
			}
			for _, at := range []time.Duration{25 * time.Hour, 25*time.Hour + time.Minute, 25*time.Hour + 2*time.Minute} {
				pass(at, 0, 0)
			}
			fj.refuse = nil
			if restart {
				w = freezeWatcher(store, forgejo, &now)
			}
			pass(25*time.Hour+3*time.Minute, 1, 1)
			pass(25*time.Hour+4*time.Minute, 1, 1)
			w = freezeWatcher(store, forgejo, &now)
			pass(25*time.Hour+5*time.Minute, 1, 1)
		})
	}
}

// Hand-derived from a replacement whose filing fails. Small's #1, frozen
// at 12:00 with a reset 30 hours out, is deleted; its ask at 13:00 the
// next day meets the 404, the card is given up, and the replacement's
// filing fails too.
//   - A 404: nothing is kept.
//   - A 403, then the class resumes at 14:00: the kept filing is dropped.
//   - A 500 that lasts past the reset: each pass keeps the filing and
//     ends, until the reset at 18:00, after which the filing is dropped.
//
// In all three a new freeze 72 hours after the first files #2 on its own
// clock: no ask at once, the ask 24 hours later.
//   - A 500 that ends: the pass at 13:02 files #2 on the episode's clock,
//     and it makes the ask that was due, once.
func TestFreezeCardFailedReplacement(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, status := range map[string]int{"a 404": http.StatusNotFound, "a 403 and a resume": http.StatusForbidden,
		"a 500 past the reset": http.StatusInternalServerError, "a 500 that ends": http.StatusInternalServerError} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			row := func(kind string, reset time.Duration) {
				store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: kind, Class: "small",
					Reset: base.Add(reset).Format(time.RFC3339), Words: "free quota exhausted"})
			}
			pass := func(at time.Duration) {
				t.Helper()
				now = base.Add(at)
				if err := w.Check(ctx); err != nil {
					t.Fatalf("the pass at %s = %v, want it to end", now.Format("2006-01-02 15:04"), err)
				}
			}
			row("class_frozen", 30*time.Hour)
			pass(0)
			fj.deleted = map[int64]bool{1: true}
			fj.fileStatus = status
			pass(25 * time.Hour)
			pass(25*time.Hour + time.Minute)
			if c := freezeCardOn(t, store, 1); c.Status != "done" || len(fj.issues) != 1 {
				t.Fatalf("after the failed replacement #1 stands %s over %d issues, want it given up and no other", c.Status, len(fj.issues))
			}
			if name == "a 500 that ends" {
				fj.fileStatus = 0
				pass(25*time.Hour + 2*time.Minute)
				pass(25*time.Hour + 3*time.Minute)
				if face, comments := freezeAsks(t, store, fj, 2); face != 1 || comments != 1 || len(fj.issues) != 2 {
					t.Fatalf("the replacement over %d issues asks %d times on its face and %d on its issue, want #2 with the ask that was due, once", len(fj.issues), face, comments)
				}
				return
			}
			if status == http.StatusForbidden {
				now = base.Add(26 * time.Hour)
				row("class_resumed", 30*time.Hour)
			}
			pass(26 * time.Hour)
			pass(31 * time.Hour)
			fj.fileStatus = 0
			pass(31*time.Hour + time.Minute)
			if len(fj.issues) != 1 {
				t.Fatalf("%d issues once Forgejo files again, want no card for the freeze that ended", len(fj.issues))
			}
			now = base.Add(72 * time.Hour)
			row("class_frozen", 100*time.Hour)
			pass(72 * time.Hour)
			pass(72*time.Hour + time.Minute)
			if face, comments := freezeAsks(t, store, fj, 2); face != 0 || comments != 0 {
				t.Fatalf("the new freeze's card asks at once, %d times on its face and %d on its issue:\n%s", face, comments, freezeCardOn(t, store, 2).Objective)
			}
			pass(96*time.Hour + time.Minute)
			if face, comments := freezeAsks(t, store, fj, 2); face != 1 || comments != 1 {
				t.Fatalf("24 hours into the new freeze its card asks %d times on its face and %d on its issue, want once", face, comments)
			}
		})
	}
}

// Hand-derived from a close Forgejo refuses: big's #1 resumed at 12:00,
// and from 12:05 its issue answers 401. The pass at 12:05 ends the card
// in the store's one write and wakes the class's two running cards, in
// both repositories; its issue stays open, the close recorded and not
// confirmed. The passes at 12:06 and 12:07 wake none again. Once Forgejo
// takes the bot, the heal pass at 12:08 posts the close's one comment,
// closes the issue and confirms the record.
func TestFreezeCardRefusedCloseEndsTheCardAndWakesOnce(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base.Add(-time.Hour)
	w := freezeWatcher(store, forgejo, &now)
	var woke []string
	w.Signal = func(_ context.Context, id string) error { woke = append(woke, id); return nil }
	for _, c := range []Task{{ID: "t1", Status: "running", Repository: "sf/acme"}, {ID: "t2", Status: "running", Repository: "sf/other"}} {
		c.Kind, c.Track, c.RequiredCapabilities = "agent", policy.TrackDirect, []string{"big"}
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	store.rows = append(store.rows,
		FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big", Reset: reset, Words: "free quota exhausted"},
		FreezeEvent{ID: 2, Repository: "sf/acme", At: base, Kind: "class_resumed", Class: "big", Reset: reset, Words: "free quota exhausted"})
	fj.refuse = map[int64]int{1: http.StatusUnauthorized}
	for _, step := range []struct {
		at     time.Duration
		woken  int
		status string
	}{{0, 0, "waiting-human"}, {5 * time.Minute, 2, "done"}, {6 * time.Minute, 2, "done"}, {7 * time.Minute, 2, "done"}} {
		now = base.Add(step.at)
		if err := w.Check(ctx); err != nil {
			t.Fatalf("the pass at %s = %v, want it to end", now.Format("15:04"), err)
		}
		if c := freezeCardOn(t, store, 1); len(woke) != step.woken || c.Status != step.status {
			t.Fatalf("at %s: woke %q, #1 %s; want %d wakes and %s", now.Format("15:04"), woke, c.Status, step.woken, step.status)
		}
	}
	if slices.Sort(woke); !slices.Equal(woke, []string{"t1", "t2"}) {
		t.Fatalf("woke %q, want each running card once", woke)
	}
	said := "#1 factoryd: The freeze of the big class ended: the freeze held 1h 00m, and the card closes."
	if half, err := store.UnconfirmedFactoryCloses(ctx, 0, 10); err != nil || len(half) != 1 || fj.states[1] == "closed" || commentsSaying(fj, said) != 0 {
		t.Fatalf("while Forgejo refuses: unconfirmed closes %+v, %v, issue #1 %q, comments %q; want the one close unconfirmed and the issue open", half, err, fj.states[1], fj.comments)
	}
	fj.refuse = nil
	now = base.Add(8 * time.Minute)
	in := &Intake{Store: store.fakeStore, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent"}
	for range 2 {
		if err := in.HealFactoryCloses(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if half, err := store.UnconfirmedFactoryCloses(ctx, 0, 10); err != nil || len(half) != 0 || fj.states[1] != "closed" || commentsSaying(fj, said) != 1 || len(woke) != 2 {
		t.Fatalf("after the heal: unconfirmed closes %+v, %v, issue #1 %q, comments %q, woke %q; want the issue closed with the one comment", half, err, fj.states[1], fj.comments, woke)
	}
}

// Hand-derived from the statuses Forgejo answers a filing with. A 401 and
// a 403 are the bot's credential or its rights. A 400 and a 423 are not:
// the line names the status and that Forgejo will not take the request. A
// 422 is given up, as a 404 is, but no repository or access is missing:
// the line says Forgejo cannot process the filing. Each line names the
// state and what follows.
func TestFreezeCardRefusalNamesItsStatus(t *testing.T) {
	const credential, restores = "a human fixes the bot's credential or its rights", "a human restores the repository or the bot's access to it"
	for status, want := range map[int]string{
		http.StatusUnauthorized:        credential + ", and each pass tries the card again",
		http.StatusForbidden:           credential + ", and each pass tries the card again",
		http.StatusBadRequest:          "Forgejo will not take the request with status 400: a human reads its answer above and restores what it names, and each pass tries the card again",
		http.StatusLocked:              "Forgejo will not take the request with status 423: a human reads its answer above and restores what it names, and each pass tries the card again",
		http.StatusUnprocessableEntity: "Forgejo cannot process the filing as factoryd sends it, which the answer above explains, and the class's next arm tries it again",
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
			w := freezeWatcher(store, forgejo, &now)
			var said bytes.Buffer
			log.SetOutput(&said)
			fj.fileStatus = status
			store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: now.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
			err := w.Check(context.Background())
			log.SetOutput(os.Stderr) // under the log's lock: no write reaches said past here
			text := said.String()
			if err != nil || !strings.Contains(text, want) || !strings.Contains(text, fmt.Sprintf("status %d", status)) ||
				(!strings.Contains(want, credential) && strings.Contains(text, credential)) || strings.Contains(text, restores) {
				t.Fatalf("the pass = %v, want a line saying %q, log:\n%s", err, want, text)
			}
		})
	}
}

// Hand-derived from a card that holds two freezes. Big's #1 freezes at
// 12:00 with a reset five days out and asks at 13:00 the next day, naming
// the freeze since 12:00. The press closes it at 14:00, and the dated
// refusal at 14:02 opens it again: a new freeze, since 14:02, with no ask.
// With the restart a watcher built anew makes the passes from 15:00 on. At
// 15:00 nothing is asked: the new freeze is 58 minutes old. At 15:00 the
// day after, 24 hours and 58 minutes in, the card asks for its second
// freeze, a second comment that names 14:02, and the face holds that ask
// alone. The pass after adds none.
func TestFreezeCardReopenedCardAsksForItsOwnFreeze(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, restart := range map[string]bool{"the same watcher": false, "a restarted watcher": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &freezeStore{fakeStore: newFakeStore()}
			forgejo, fj := newAlertForgejo(t)
			now := base
			w := freezeWatcher(store, forgejo, &now)
			pass := func(at time.Duration, arm bool, wantFace, wantComments int) {
				t.Helper()
				now = base.Add(at)
				if arm {
					store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
						Reset: base.Add(120 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
				}
				if err := w.Check(ctx); err != nil {
					t.Fatal(err)
				}
				if face, comments := freezeAsks(t, store, fj, 1); face != wantFace || comments != wantComments || len(fj.issues) != 1 {
					t.Fatalf("at %s the ask stands %d times on the face and %d on the issue over %d issues, want %d and %d on the one card:\n%s",
						now.Format("2006-01-02 15:04"), face, comments, len(fj.issues), wantFace, wantComments, freezeCardOn(t, store, 1).Objective)
				}
			}
			pass(0, true, 0, 0)
			pass(25*time.Hour, false, 1, 1)
			now = base.Add(26 * time.Hour)
			if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
				t.Fatal(err)
			}
			pass(26*time.Hour+2*time.Minute, true, 0, 1)
			if restart {
				w = freezeWatcher(store, forgejo, &now)
			}
			pass(27*time.Hour, false, 0, 1)
			pass(51*time.Hour, false, 1, 2)
			pass(51*time.Hour+time.Minute, false, 1, 2)
			for _, since := range []string{"2026-11-21T12:00:00Z", "2026-11-22T14:02:00Z"} {
				if n := commentsSaying(fj, "#1 factoryd: The freeze of the big class has held since "+since+
					", past waits.provider_limit (24h0m0s). Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing."); n != 1 {
					t.Fatalf("%d asks name the freeze since %s, want one: %q", n, since, fj.comments)
				}
			}
			if face := freezeCardOn(t, store, 1).Objective; !strings.Contains(face, "The freeze started at 2026-11-22T14:02:00Z.") || !strings.Contains(face, "has held since 2026-11-22T14:02:00Z,") {
				t.Fatalf("the face names another freeze than the one since 14:02:\n%s", face)
			}
		})
	}
}

// Hand-derived from a replacement that waits on a restart: small's #1,
// frozen at 12:00 with a reset 30 hours out, is deleted, its ask at 13:00
// the next day meets the 404 and the card is given up, its face keeping
// the reset and the freeze's start. The replacement's filing meets a 401.
// A watcher built anew at 13:10, Forgejo taking the bot again, files #2
// for the standing freeze on the clock of the given-up card, and it makes
// the ask that was due, naming the freeze since 12:00, once.
func TestFreezeCardReplacementKeepsItsClockOverARestart(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "small",
		Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	fj.deleted, fj.fileStatus = map[int64]bool{1: true}, http.StatusUnauthorized
	now = base.Add(25 * time.Hour)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c := freezeCardOn(t, store, 1); c.Status != "done" || len(fj.issues) != 1 ||
		!strings.Contains(c.Objective, "The class resets at 2026-11-22T18:00:00Z (the reset its freeze held).\nThe freeze started at 2026-11-21T12:00:00Z.") {
		t.Fatalf("the given-up #1 stands %s over %d issues:\n%s", c.Status, len(fj.issues), c.Objective)
	}
	fj.fileStatus = 0
	w = freezeWatcher(store, forgejo, &now)
	for _, at := range []time.Duration{25*time.Hour + 10*time.Minute, 25*time.Hour + 11*time.Minute} {
		now = base.Add(at)
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	face, comments := freezeAsks(t, store, fj, 2)
	if c := freezeCardOn(t, store, 2); len(fj.issues) != 2 || face != 1 || comments != 1 || !strings.Contains(c.Objective, "has held since 2026-11-21T12:00:00Z,") {
		t.Fatalf("the replacement over %d issues asks %d times on its face and %d on its issue, want #2 with the ask since 12:00, once:\n%s", len(fj.issues), face, comments, c.Objective)
	}
}

// Hand-derived from a freeze card a human closed by hand: big's #1,
// frozen at 12:00 to the reset 17:00, whose issue a human closes at 12:30,
// the webhook ending the card with its frozen face. Watchers built anew at
// 13:00 and at 14:00 file nothing: the human closed the card of this
// reset. A dated refusal at 18:00 names the reset 23:00, a new freeze,
// and files #2.
func TestFreezeCardClosedByAHumanStaysClosedOverARestart(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	arm := func(reset time.Duration) {
		store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
			Reset: base.Add(reset).Format(time.RFC3339), Words: "free quota exhausted"})
	}
	arm(5 * time.Hour)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionTask(ctx, freezeCardOn(t, store, 1).ID, []string{"waiting-human"}, "done"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{time.Hour, 2 * time.Hour} {
		now = base.Add(at)
		w = freezeWatcher(store, forgejo, &now)
		if err := w.Check(ctx); err != nil || len(fj.issues) != 1 {
			t.Fatalf("the restarted pass at %s = %v over %d issues, want the closed card left alone", now.Format("15:04"), err, len(fj.issues))
		}
	}
	now = base.Add(6 * time.Hour)
	arm(11 * time.Hour)
	if err := w.Check(ctx); err != nil || len(fj.issues) != 2 || freezeCardOn(t, store, 2).Status != "waiting-human" {
		t.Fatalf("the new freeze's pass = %v over %d issues, want #2 filed", err, len(fj.issues))
	}
}

// Hand-derived from a Forgejo that fails one class's filing: small's arm
// at 12:00 meets a 500 beside big's, which files #1. The pass ends with
// the cursor past both rows. At 12:05 the 500 still stands and big's
// class_resumed arrives: the pass reads it, and #1 takes the resumed
// face. At 12:10 the 500 still stands: #1 closes. Once Forgejo answers,
// the 12:11 pass files small's #2 with no new row.
func TestFreezeCardFailedFilingStopsNoOtherClass(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	reset := base.Add(5 * time.Hour).Format(time.RFC3339)
	pass := func(at time.Duration, rows ...FreezeEvent) {
		t.Helper()
		now = base.Add(at)
		for _, r := range rows {
			r.ID, r.Repository, r.At, r.Reset, r.Words = int64(len(store.rows)+1), "sf/acme", now, reset, "free quota exhausted"
			store.rows = append(store.rows, r)
		}
		if err := w.Check(ctx); err != nil || store.storedCursor != int64(len(store.rows)) {
			t.Fatalf("the pass at %s = %v at cursor %d, want it to end past row %d", now.Format("15:04"), err, store.storedCursor, len(store.rows))
		}
	}
	pass(0, FreezeEvent{Kind: "class_frozen", Class: "big"})
	fj.fileStatus = http.StatusInternalServerError
	pass(time.Minute, FreezeEvent{Kind: "class_frozen", Class: "small"})
	pass(5*time.Minute, FreezeEvent{Kind: "class_resumed", Class: "big"})
	if c := freezeCardOn(t, store, 1); !strings.Contains(c.Objective, freezeResumedSentence) {
		t.Fatalf("big's resume stayed unread behind small's failed filing:\n%s", c.Objective)
	}
	pass(10 * time.Minute)
	if c := freezeCardOn(t, store, 1); c.Status != "done" || len(fj.issues) != 1 {
		t.Fatalf("at 12:10 #1 stands %s over %d issues, want it closed and small's filing still kept", c.Status, len(fj.issues))
	}
	fj.fileStatus = 0
	pass(11 * time.Minute)
	if c := freezeCardOn(t, store, 2); c.Status != "waiting-human" || !objectiveHasClassLine(c.Objective, "small") || len(fj.issues) != 2 {
		t.Fatalf("once Forgejo answers, #2 stands %s over %d issues, want small's card", c.Status, len(fj.issues))
	}
}

// Hand-derived from the record of a close the reopen takes off its issue:
// the press closes big's #1, its close recorded and confirmed, and a
// dated refusal opens it again. The record stays confirmed, as the
// store's does, so no heal pass reads it as a close left half done.
func TestFreezeCardForgottenCloseStaysConfirmed(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, _ := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	for i, at := range []time.Duration{0, 20 * time.Minute} {
		now = base.Add(at)
		store.rows = append(store.rows, FreezeEvent{ID: int64(i + 1), Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
			Reset: base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			now = base.Add(10 * time.Minute)
			if err := w.CloseOnRetry(ctx, "sf/acme", "big", "sebastian"); err != nil {
				t.Fatal(err)
			}
		}
	}
	open, err := store.UnconfirmedFactoryCloses(ctx, 0, 10)
	if c := freezeCardOn(t, store, 1); err != nil || c.Status != "waiting-human" || len(open) != 0 {
		t.Fatalf("after the reopen #1 stands %s with the unconfirmed closes %+v, %v; want it open and none", c.Status, open, err)
	}
}

// freezeTable is one class under a watcher that can be built anew over
// the same store: the rig TestFreezeCardStateTable walks. One running work
// card of the big class waits in sf/acme, the card a resume that held
// wakes.
type freezeTable struct {
	t       *testing.T
	store   *freezeStore
	forgejo *Forgejo
	fj      *alertForgejo
	now     time.Time
	w       *FreezeCard
	in      *Intake
	metrics *telemetry.Metrics
	reader  *sdkmetric.ManualReader
	wakes   []string

	// A fault run: fault fails one write or call, a pass's or a press's
	// error is then the fault's and not the test's. restartOnFault builds
	// the watcher anew right after the pass or the press the fault fired
	// in. A failed press is made again, at once or, with pressLater,
	// after the next pass.
	fault                      *freezeFault
	restartOnFault, pressLater bool
	restarted, pressOwed       bool
	pressFailed                bool
}

func newFreezeTable(t *testing.T) *freezeTable {
	t.Helper()
	forgejo, fj := newAlertForgejo(t)
	fj.listOpen = true
	h := &freezeTable{t: t, store: &freezeStore{fakeStore: newFakeStore()}, forgejo: forgejo, fj: fj, now: freezeTableBase}
	h.metrics, h.reader = testMetrics(t)
	h.in = &Intake{Store: h.store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Metrics: h.metrics}
	h.store.tasks["work-big"] = Task{ID: "work-big", Kind: "agent", Status: "running", Repository: "sf/acme", RequiredCapabilities: []string{"big"}}
	h.restart()
	return h
}

var freezeTableBase = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func (h *freezeTable) at(d time.Duration) { h.now = freezeTableBase.Add(d) }

func (h *freezeTable) restart() {
	h.w = freezeWatcher(h.store, h.forgejo, &h.now)
	h.w.Metrics = h.metrics
	h.w.Signal = func(_ context.Context, id string) error {
		h.wakes = append(h.wakes, id)
		return nil
	}
}

// row writes the gateway's row of kind for the big class, its reset days
// after 12:00, at the rig's time.
func (h *freezeTable) row(kind string, days int) { h.rowIn("sf/acme", kind, days) }

func (h *freezeTable) rowIn(repo, kind string, days int) {
	h.store.rows = append(h.store.rows, FreezeEvent{ID: int64(len(h.store.rows) + 1), Repository: repo, At: h.now, Kind: kind, Class: "big",
		Reset: freezeTableBase.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
}

// arm starts the fault run that fails the write or call numbered failAt;
// 0 fails none and counts them.
func (h *freezeTable) arm(failAt int) {
	h.fault = &freezeFault{at: failAt}
	h.store.fault = h.fault
	h.fj.mu.Lock()
	h.fj.fault = func(r *http.Request) bool {
		_, call, _ := strings.Cut(r.URL.Path, "/issues")
		return h.fault.fail(strings.ReplaceAll(r.Method+" issues"+call, "/", " "))
	}
	h.fj.mu.Unlock()
}

// afterFault builds the watcher anew once, right after the pass or the
// press the fault fired in, on a run that restarts.
func (h *freezeTable) afterFault() {
	if h.restartOnFault && !h.restarted && h.fault != nil && h.fault.fired {
		h.restarted = true
		h.restart()
	}
}

func (h *freezeTable) pass() {
	h.t.Helper()
	if err := h.w.Check(context.Background()); err != nil && (h.fault == nil || h.fault.at == 0) {
		h.t.Fatalf("the pass at %s = %v, want it to end", h.now.Format("2006-01-02 15:04"), err)
	}
	h.afterFault()
	if h.pressOwed {
		h.pressOwed = false
		h.closeOnRetry()
	}
}

// press is the retry press as RetryFreeze makes it: only on a card that
// holds, the gateway clears the hold and writes the class_resumed row,
// then the card closes. A press that fails is made again, as a human
// presses again: the gateway, its hold cleared, writes no second row.
func (h *freezeTable) press() {
	h.t.Helper()
	if !h.holds() {
		return
	}
	h.row("class_resumed", 5)
	if h.closeOnRetry() {
		return
	}
	h.pressFailed = true
	h.afterFault()
	if h.pressLater {
		h.pressOwed = true
		return
	}
	h.closeOnRetry()
}

// holds reports whether a freeze card stands that takes the press.
func (h *freezeTable) holds() bool {
	h.t.Helper()
	open, err := h.store.FreezeCards(context.Background(), "waiting-human")
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.ContainsFunc(open, FreezeCardHolds)
}

// closeOnRetry is the press's close of the card, on a card that holds:
// false when it failed on a fault run.
func (h *freezeTable) closeOnRetry() bool {
	h.t.Helper()
	if !h.holds() {
		return true
	}
	err := h.w.CloseOnRetry(context.Background(), "sf/acme", "big", "sebastian")
	if err != nil && (h.fault == nil || h.fault.at == 0) {
		h.t.Fatal(err)
	}
	return err == nil
}

// heal is the heal pass's finish of every close factoryd recorded and did
// not confirm (HealFactoryCloses), without its two-minute wait.
func (h *freezeTable) heal() {
	h.t.Helper()
	ctx := context.Background()
	rows, err := h.store.UnconfirmedFactoryCloses(ctx, -time.Hour, 50)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, row := range rows {
		if err := h.in.healFactoryClose(ctx, row); err != nil && (h.fault == nil || h.fault.at == 0) {
			h.t.Fatalf("the heal of the close of #%d = %v", row.CommentID, err)
		}
	}
}

// settle runs passes, each with the heal's, until two in a row change
// nothing a human or a later freeze can see.
func (h *freezeTable) settle() {
	h.t.Helper()
	prev, same := "", 0
	for range 10 {
		h.pass()
		h.heal()
		if got := h.end(); got == prev {
			same++
		} else {
			prev, same = got, 0
		}
		if same == 2 {
			return
		}
	}
	h.t.Fatalf("ten passes did not settle the cards, which stand:\n%s", prev)
}

// humanClose is a human's close of the open card on issue number through
// the console: recorded as factoryd's in the human's words, then the
// webhook of that close ends the card.
func (h *freezeTable) humanClose(number int64) {
	h.t.Helper()
	ctx := context.Background()
	c := freezeCardOn(h.t, h.store, number)
	if c.Status != "waiting-human" {
		return
	}
	if h.fault != nil {
		h.fault.mu.Lock()
		h.fault.paused = true
		h.fault.mu.Unlock()
		defer func() {
			h.fault.mu.Lock()
			h.fault.paused = false
			h.fault.mu.Unlock()
		}()
	}
	if err := closeIssueAsFactory(ctx, h.store.fakeStore, h.forgejo, c, "sebastian closed this card."); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.in.issueClosed(ctx, c, "factory-agent"); err != nil {
		h.t.Fatal(err)
	}
}

var freezeCountdownText = regexp.MustCompile(`\(in \d+h \d\dm\)`)

// end describes everything a human or a later freeze can see: each freeze
// card's row and face, its close records and owed reopens, each issue open
// or closed, every comment, the ends counted and the wakes sent. A face's
// countdown is the minute of its write and is left out. An issue is named
// by its repository and its place there, as the fake numbers across
// repositories.
func (h *freezeTable) end() string {
	h.t.Helper()
	ctx := context.Background()
	h.fj.mu.Lock()
	defer h.fj.mu.Unlock()
	name, per := map[int64]string{}, map[string]int{}
	var issues, cards, comments []string
	for i, repo := range h.fj.repos {
		per[repo]++
		n := int64(i + 1)
		name[n] = fmt.Sprintf("%s#%d", repo, per[repo])
		issues = append(issues, fmt.Sprintf("issue %s: %s\n", name[n], cmp.Or(h.fj.states[n], "open")))
	}
	for _, c := range h.store.tasks {
		if c.Source != SourceFreezeCard {
			continue
		}
		d := fmt.Sprintf("card %s: %s\n%s\n", name[c.IssueNumber], c.Status, freezeCountdownText.ReplaceAllString(c.Objective, "(in …)"))
		events, err := h.store.IntakeEvents(ctx, c.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, e := range events {
			d += fmt.Sprintf("  %s on %d, confirmed %v: %s\n", e.Kind, e.CommentID, h.store.confirmedCloses[closeKey(e)], e.Body)
		}
		cards = append(cards, d)
	}
	for _, c := range h.fj.comments {
		var n int64
		fmt.Sscanf(c, "#%d ", &n) // #nosec G104 -- test stub
		comments = append(comments, "comment "+name[n]+strings.TrimPrefix(c, fmt.Sprintf("#%d", n))+"\n")
	}
	slices.Sort(issues)
	slices.Sort(cards)
	slices.Sort(comments)
	return fmt.Sprintf("%s%s%sends counted: %v\nwakes: %v\n", strings.Join(issues, ""), strings.Join(cards, ""), strings.Join(comments, ""),
		h.ends(), slices.Sorted(slices.Values(h.wakes)))
}

// ends is how many freeze cards' ends the run counted.
func (h *freezeTable) ends() float64 {
	h.t.Helper()
	n := 0.0
	for _, c := range sums(h.t, h.reader, "factory.task.outcome") {
		n += c
	}
	return n
}

// reached is the short form of end a named exception asserts: the cards,
// the open issues, and how many comments, ends and wakes.
func (h *freezeTable) reached() string {
	h.t.Helper()
	var open []string
	for i := range h.fj.issues {
		if h.fj.states[int64(i+1)] != "closed" {
			open = append(open, fmt.Sprintf("#%d", i+1))
		}
	}
	return fmt.Sprintf("%s | open %s | %d comments, %v ends, %d wakes", h.cards(), strings.Join(open, " "), len(h.fj.comments), h.ends(), len(h.wakes))
}

// freezeFaultException is a fault the cards cannot converge from to the
// end of the run without it: why, in one line, and what the run reaches
// (reached).
type freezeFaultException struct{ why, reaches string }

// freezeFaults is the fault dimension of scenario: build makes the state
// and event applies what happens to it. It runs once without a fault and
// counts the store's writes and Forgejo's calls, then once for each with
// that one failing once, without a restart and with one right after the
// pass or the press it failed in; a press that failed is made again, at
// once and after the next pass. Every run must settle at the end of the
// run without a fault, but those except names, by "<the failed write or
// call>[/restart][/pressed again after a pass]", which must reach what
// they name. It answers the run without a fault, settled.
func freezeFaults(t *testing.T, build, event func(h *freezeTable), except map[string]freezeFaultException) *freezeTable {
	t.Helper()
	run := func(t *testing.T, failAt int, restart, pressLater bool) *freezeTable {
		t.Helper()
		h := newFreezeTable(t)
		build(h)
		h.restartOnFault, h.pressLater = restart, pressLater
		h.arm(failAt)
		event(h)
		h.now = h.now.Add(time.Minute)
		h.settle()
		return h
	}
	base := run(t, 0, false, false)
	if t.Failed() {
		return base
	}
	want, seen := base.end(), map[string]int{}
	used := map[string]bool{}
	for i, op := range base.fault.ops {
		seen[op]++
		if seen[op] > 1 {
			op = fmt.Sprintf("%s (%d)", op, seen[op])
		}
		for _, restart := range []bool{false, true} {
			for _, pressLater := range []bool{false, true} {
				name := op
				if restart {
					name += "/restart"
				}
				if pressLater {
					name += "/pressed again after a pass"
				}
				var h *freezeTable
				t.Run("fault "+name, func(t *testing.T) {
					h = run(t, i+1, restart, pressLater)
					if e, named := except[name]; named {
						used[name] = true
						if got := h.reached(); got != e.reaches {
							t.Fatalf("the cards reach %q, want %q (%s)", got, e.reaches, e.why)
						}
						if h.end() == want {
							t.Fatalf("the cards reach the end of the run without a fault, so this is no exception (%s)", e.why)
						}
						return
					}
					if got := h.end(); got != want {
						t.Fatalf("%s failed once and the cards settle at:\n%s\nwant the end of the run without a fault:\n%s", op, got, want)
					}
				})
				if h == nil || !h.pressFailed {
					break // no press failed: pressing again later is no other run
				}
			}
		}
	}
	for name := range except {
		if !used[name] {
			t.Errorf("the exception %q names no fault of this run", name)
		}
	}
	return base
}

// cards describes every freeze card by its issue: its row, its face, the
// reset on its face (R1 five days after 12:00, R2 six) and its ask.
func (h *freezeTable) cards() string {
	h.t.Helper()
	all := map[int64]string{}
	for _, c := range h.store.tasks {
		if c.Source != SourceFreezeCard {
			continue
		}
		face := "frozen"
		for kind, tell := range map[string]string{"resumed": freezeResumedSentence, "pressed": freezePressedSentence, "refused": freezeRefusedSentence} {
			if strings.Contains(c.Objective, tell) {
				face = kind
			}
		}
		d := fmt.Sprintf("#%d %s %s", c.IssueNumber, strings.TrimSuffix(c.Status, "-human"), face)
		switch freezeFaceReset(c.Objective) {
		case freezeTableBase.Add(120 * time.Hour).Format(time.RFC3339):
			d += " R1"
		case freezeTableBase.Add(144 * time.Hour).Format(time.RFC3339):
			d += " R2"
		}
		if strings.Contains(c.Objective, "past waits.provider_limit") {
			d += " asked"
		}
		if events, err := h.store.IntakeEvents(context.Background(), c.ID); err != nil {
			h.t.Fatal(err)
		} else if c.Status == "waiting-human" && botCloseRecorded(events, c) {
			d += " +close record"
		}
		all[c.IssueNumber] = d
	}
	var out []string
	for _, n := range slices.Sorted(maps.Keys(all)) {
		out = append(out, all[n])
	}
	return strings.Join(out, "; ")
}

// Hand-derived: the state table of one class in one repository, every
// card state under every event, each cell also with the watcher built
// anew between the state and the event. The big class freezes at 12:00 to
// the reset R1, five days out; R2 is six days out. Each state is built by
// 13:58 at the latest and each event lands at 14:00, the two ticks at
// 14:05 and at 13:00 the next day. One more pass a minute later must
// change nothing. A cell names the cards the store then holds and the
// comments the event posted. Each cell then runs its fault dimension
// (freezeFaults), with the exceptions freezeTableExceptions names. In the
// pending filing state Forgejo answers
// the filing 401 through the event and answers again for the pass after
// it. The refusal deletes the issues that stand (a filing answers 404) and
// passes at 13:00 the next day.
func TestFreezeCardStateTable(t *testing.T) {
	type cell struct {
		cards    string
		comments int
	}
	ctx := context.Background()
	const frozen1, resumed1, pressed1, closed1, refused1, human1 = "#1 waiting frozen R1", "#1 waiting resumed R1", "#1 done pressed R1", "#1 done resumed R1", "#1 done refused", "#1 done frozen R1"
	states := []struct {
		name  string
		build func(h *freezeTable)
		start string
		cells map[string]cell
	}{
		{"none", func(h *freezeTable) {}, "", map[string]cell{
			"class_frozen same reset": {frozen1, 0}, "class_frozen new reset": {"#1 waiting frozen R2", 0}, "class_resumed": {"", 0}, "press": {"", 0},
			"tick past 5 minutes": {"", 0}, "tick past the limit": {"", 0}, "restart": {"", 0}, "refusal": {"", 0}}},
		{"open frozen", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
		}, frozen1, map[string]cell{
			"class_frozen same reset": {frozen1, 0}, "class_frozen new reset": {"#1 waiting frozen R2", 1}, "class_resumed": {resumed1, 0}, "press": {pressed1, 1},
			"tick past 5 minutes": {frozen1, 0}, "tick past the limit": {frozen1 + " asked", 1}, "restart": {frozen1, 0},
			"refusal": {"#1 done refused R1; #2 waiting frozen R1 asked", 1}}},
		{"open resumed", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
			h.at(118 * time.Minute)
			h.row("class_resumed", 5)
			h.pass()
		}, resumed1, map[string]cell{
			"class_frozen same reset": {frozen1, 0}, "class_frozen new reset": {"#1 waiting frozen R2", 1}, "class_resumed": {resumed1, 0}, "press": {resumed1, 0},
			"tick past 5 minutes": {closed1, 1}, "tick past the limit": {closed1, 1}, "restart": {resumed1, 0},
			// the card's close is the store's one write; Forgejo, its issue
			// gone, takes neither the comment nor the close
			"refusal": {closed1, 0}}},
		{"done by press", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
			h.at(118 * time.Minute)
			h.press()
			h.pass()
		}, pressed1, map[string]cell{
			"class_frozen same reset": {frozen1, 1}, "class_frozen new reset": {"#1 waiting frozen R2", 1}, "class_resumed": {pressed1, 0}, "press": {pressed1, 0},
			"tick past 5 minutes": {pressed1, 0}, "tick past the limit": {pressed1, 0}, "restart": {pressed1, 0}, "refusal": {pressed1, 0}}},
		{"done by self-resume close", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
			h.at(time.Hour)
			h.row("class_resumed", 5)
			h.pass()
			h.at(66 * time.Minute)
			h.pass()
		}, closed1, map[string]cell{
			"class_frozen same reset": {closed1 + "; #2 waiting frozen R1", 0}, "class_frozen new reset": {closed1 + "; #2 waiting frozen R2", 0}, "class_resumed": {closed1, 0},
			"press": {closed1, 0}, "tick past 5 minutes": {closed1, 0}, "tick past the limit": {closed1, 0}, "restart": {closed1, 0}, "refusal": {closed1, 0}}},
		{"done by give-up", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
			// a re-arm meets the deleted issue and no replacement can be
			// filed; then the class resumes
			h.fj.deleted, h.fj.fileStatus = map[int64]bool{1: true}, http.StatusNotFound
			h.at(time.Hour)
			h.row("class_frozen", 6)
			h.pass()
			h.at(66 * time.Minute)
			h.row("class_resumed", 6)
			h.pass()
			h.fj.fileStatus = 0
		}, refused1, map[string]cell{
			"class_frozen same reset": {refused1 + "; #2 waiting frozen R1", 0}, "class_frozen new reset": {refused1 + "; #2 waiting frozen R2", 0}, "class_resumed": {refused1, 0},
			"press": {refused1, 0}, "tick past 5 minutes": {refused1, 0}, "tick past the limit": {refused1, 0}, "restart": {refused1, 0}, "refusal": {refused1, 0}}},
		{"done by a human", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
			h.at(118 * time.Minute)
			// the console's close: recorded as factoryd's, in the human's words
			c := freezeCardOn(h.t, h.store, 1)
			if err := closeIssueAsFactory(ctx, h.store, h.forgejo, c, "sebastian closed this card."); err != nil {
				h.t.Fatal(err)
			}
			if err := h.store.TransitionTask(ctx, c.ID, []string{"waiting-human"}, "done"); err != nil {
				h.t.Fatal(err)
			}
		}, human1, map[string]cell{
			"class_frozen same reset": {human1, 0}, "class_frozen new reset": {human1 + "; #2 waiting frozen R2", 0}, "class_resumed": {human1, 0}, "press": {human1, 0},
			"tick past 5 minutes": {human1, 0}, "tick past the limit": {human1, 0}, "restart": {human1, 0}, "refusal": {human1, 0}}},
		{"pending filing", func(h *freezeTable) {
			h.fj.fileStatus = http.StatusUnauthorized
			h.row("class_frozen", 5)
			h.pass()
		}, "", map[string]cell{
			"class_frozen same reset": {frozen1, 0}, "class_frozen new reset": {"#1 waiting frozen R2", 0}, "class_resumed": {"", 0}, "press": {frozen1, 0},
			"tick past 5 minutes": {frozen1, 0}, "tick past the limit": {frozen1 + " asked", 1}, "restart": {frozen1, 0}, "refusal": {"", 0}}},
		{"open frozen, a human's close in flight", func(h *freezeTable) {
			h.row("class_frozen", 5)
			h.pass()
			h.at(118 * time.Minute)
			// the console's close is recorded and the issue closed; the
			// webhook that ends the card has not landed
			if err := closeIssueAsFactory(ctx, h.store, h.forgejo, freezeCardOn(h.t, h.store, 1), "sebastian closed this card."); err != nil {
				h.t.Fatal(err)
			}
		}, frozen1 + " +close record", map[string]cell{
			"class_frozen same reset": {frozen1 + " +close record", 0}, "class_frozen new reset": {"#1 waiting frozen R2 +close record", 1},
			"class_resumed": {resumed1 + " +close record", 0},
			// the press leaves the card to the human's close, and the
			// gateway's class_resumed starts its five minutes
			"press":               {resumed1 + " +close record", 0},
			"tick past 5 minutes": {frozen1 + " +close record", 0}, "tick past the limit": {frozen1 + " asked +close record", 1},
			"restart": {frozen1 + " +close record", 0},
			// the given-up card ends whatever close is in flight: its issue
			// is gone
			"refusal": {"#1 done refused R1; #2 waiting frozen R1 asked", 1}}},
	}
	events := []struct {
		name  string
		apply func(h *freezeTable)
	}{
		{"class_frozen same reset", func(h *freezeTable) { h.row("class_frozen", 5); h.pass() }},
		{"class_frozen new reset", func(h *freezeTable) { h.row("class_frozen", 6); h.pass() }},
		{"class_resumed", func(h *freezeTable) { h.row("class_resumed", 5); h.pass() }},
		{"press", func(h *freezeTable) { h.press(); h.pass() }},
		{"tick past 5 minutes", func(h *freezeTable) { h.at(125 * time.Minute); h.pass() }},
		{"tick past the limit", func(h *freezeTable) { h.at(25 * time.Hour); h.pass() }},
		{"restart", func(h *freezeTable) { h.restart(); h.pass() }},
		{"refusal", func(h *freezeTable) {
			h.fj.deleted = map[int64]bool{}
			for n := range h.fj.issues {
				h.fj.deleted[int64(n+1)] = true
			}
			if h.fj.fileStatus != 0 {
				h.fj.fileStatus = http.StatusNotFound
			}
			h.at(25 * time.Hour)
			h.pass()
		}},
	}
	for _, state := range states {
		for _, event := range events {
			for _, restarted := range []bool{false, true} {
				name := state.name + "/" + event.name
				if restarted {
					name += "/after a restart"
				}
				t.Run(name, func(t *testing.T) {
					said := 0
					h := freezeFaults(t, func(h *freezeTable) {
						state.build(h)
						if got := h.cards(); got != state.start {
							h.t.Fatalf("the state stands %q, want %q", got, state.start)
						}
						if restarted {
							h.restart()
						}
						said = len(h.fj.comments)
						h.at(2 * time.Hour)
					}, func(h *freezeTable) {
						event.apply(h)
						if h.fj.fileStatus == http.StatusUnauthorized {
							h.fj.fileStatus = 0
						}
					}, freezeTableExceptions[name])
					want := state.cells[event.name]
					if got := h.cards(); got != want.cards || len(h.fj.comments)-said != want.comments {
						t.Fatalf("%q with %d comments %q, want %q with %d", got, len(h.fj.comments)-said, h.fj.comments[said:], want.cards, want.comments)
					}
				})
			}
		}
	}
}

// freezeTableExceptions names, by cell, the faults of the state table that
// cannot converge to the end of the run without them.
var freezeTableExceptions = func() map[string]map[string]freezeFaultException {
	// the press's one write failed and changed nothing; the pass before the
	// second press read the gateway's class_resumed, so the card took the
	// resumed face, which takes no press and closes itself at 14:05
	lost := freezeFaultException{"the press's write failed and a pass read the class_resumed before the second press: the card closes itself five minutes after the resume",
		"#1 waiting resumed R1 | open #1 | 0 comments, 0 ends, 0 wakes"}
	cell := map[string]freezeFaultException{"EndFreezeCard/pressed again after a pass": lost, "EndFreezeCard/restart/pressed again after a pass": lost}
	return map[string]map[string]freezeFaultException{"open frozen/press": cell, "open frozen/press/after a restart": cell}
}()

// Hand-derived from a human's close the watcher lives through: big's #1,
// frozen at 12:00 to the reset 17:00 the next day, whose issue a human
// closes at 12:30. The same watcher reads a dated refusal at 13:00 that
// names a new reset: #1 keeps its face and gets no comment, and #2 files
// for the new freeze. At 13:00 the next day nothing asks on #1; #2 asks
// once at 13:01, 24 hours into its own freeze.
func TestFreezeCardClosedByAHumanUnderTheWatcher(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	pass := func(at, reset time.Duration) {
		t.Helper()
		now = base.Add(at)
		if reset != 0 {
			store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
				Reset: base.Add(reset).Format(time.RFC3339), Words: "free quota exhausted"})
		}
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pass(0, 29*time.Hour)
	face := freezeCardOn(t, store, 1).Objective
	if err := store.TransitionTask(ctx, freezeCardOn(t, store, 1).ID, []string{"waiting-human"}, "done"); err != nil {
		t.Fatal(err)
	}
	pass(time.Hour, 48*time.Hour)
	pass(25*time.Hour, 0)
	pass(25*time.Hour+time.Minute, 0)
	asks1, said1 := freezeAsks(t, store, fj, 1)
	asks2, said2 := freezeAsks(t, store, fj, 2)
	if c := freezeCardOn(t, store, 1); c.Status != "done" || c.Objective != face || asks1+said1 != 0 || len(fj.issues) != 2 || asks2 != 1 || said2 != 1 || len(fj.comments) != 1 {
		t.Fatalf("#1 stands %s over %d issues with the comments %q, #2 asks %d times; want #1 as the human left it and one ask on #2:\n%s", c.Status, len(fj.issues), fj.comments, said2, c.Objective)
	}
}

// Hand-derived from two freezes with the same reset: small's #1, frozen
// at 12:00 to the reset 18:00 the next day, is deleted and given up at its
// ask at 13:00 the next day, and #2 replaces it on #1's clock. The class
// resumes at 14:00, which ends #1's freeze and takes the reset and the
// start off its face; #2 closes at 14:05. A refusal at 15:00 names the
// same reset: #3 files for a new freeze, on its own clock, and asks
// nothing at 15:01.
func TestFreezeCardGivenUpClockEndsWithItsFreeze(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	pass := func(at time.Duration, kind string) {
		t.Helper()
		now = base.Add(at)
		if kind != "" {
			store.rows = append(store.rows, FreezeEvent{ID: int64(len(store.rows) + 1), Repository: "sf/acme", At: now, Kind: kind, Class: "small",
				Reset: base.Add(30 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
		}
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pass(0, "class_frozen")
	fj.deleted = map[int64]bool{1: true}
	pass(25*time.Hour, "")
	pass(25*time.Hour+time.Minute, "")
	if face, said := freezeAsks(t, store, fj, 2); face != 1 || said != 1 || !strings.Contains(freezeCardOn(t, store, 1).Objective, "The freeze started at 2026-11-21T12:00:00Z.") {
		t.Fatalf("the replacement asks %d times on its face and %d on its issue, #1:\n%s", face, said, freezeCardOn(t, store, 1).Objective)
	}
	pass(26*time.Hour, "class_resumed")
	pass(26*time.Hour+5*time.Minute, "")
	if c1, c2 := freezeCardOn(t, store, 1), freezeCardOn(t, store, 2); c2.Status != "done" || strings.Contains(c1.Objective, "The class resets at") || strings.Contains(c1.Objective, freezeStarted) ||
		!strings.Contains(c1.Objective, freezeRefusedSentence) {
		t.Fatalf("after the resume #2 stands %s and #1 keeps a clock:\n%s", c2.Status, c1.Objective)
	}
	pass(27*time.Hour, "class_frozen")
	pass(27*time.Hour+time.Minute, "")
	if face, said := freezeAsks(t, store, fj, 3); face != 0 || said != 0 || !strings.Contains(freezeCardOn(t, store, 3).Objective, "The freeze started at 2026-11-22T15:00:00Z.") {
		t.Fatalf("the new freeze's #3 asks %d times on its face and %d on its issue, want its own clock:\n%s", face, said, freezeCardOn(t, store, 3).Objective)
	}
}

// Hand-derived from a filing Forgejo takes a day late: big's arm at 12:00
// meets a 401 until 13:00 the next day. The card filed then names the
// freeze since 12:00, the arm's own time, and makes the ask that was due
// on the same pass, once.
func TestFreezeCardKeptFilingKeepsTheArmsClock(t *testing.T) {
	ctx := context.Background()
	store := &freezeStore{fakeStore: newFakeStore()}
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	now := base
	w := freezeWatcher(store, forgejo, &now)
	fj.fileStatus = http.StatusUnauthorized
	store.rows = append(store.rows, FreezeEvent{ID: 1, Repository: "sf/acme", At: now, Kind: "class_frozen", Class: "big",
		Reset: base.Add(120 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	for _, at := range []time.Duration{0, 25 * time.Hour, 25*time.Hour + time.Minute} {
		now = base.Add(at)
		if at == 25*time.Hour {
			fj.fileStatus = 0
		}
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	face, said := freezeAsks(t, store, fj, 1)
	if c := freezeCardOn(t, store, 1); face != 1 || said != 1 || !strings.Contains(c.Objective, "The freeze started at 2026-11-21T12:00:00Z.") {
		t.Fatalf("the card filed a day late asks %d times on its face and %d on its issue, want the freeze since 12:00 and one ask:\n%s", face, said, c.Objective)
	}
}

// freezeProbe is a watcher on a clock over one store, the big class armed
// at 12:00 to a reset five hours out and its card #1 filed: the start of
// the probes below.
type freezeProbe struct {
	t       *testing.T
	store   *freezeStore
	forgejo *Forgejo
	fj      *alertForgejo
	base    time.Time
	now     time.Time
	w       *FreezeCard
}

func newFreezeProbe(t *testing.T) *freezeProbe {
	t.Helper()
	forgejo, fj := newAlertForgejo(t)
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	p := &freezeProbe{t: t, store: &freezeStore{fakeStore: newFakeStore()}, forgejo: forgejo, fj: fj, base: base, now: base}
	p.restart()
	p.pass(0, true)
	return p
}

func (p *freezeProbe) restart() { p.w = freezeWatcher(p.store, p.forgejo, &p.now) }

// pass runs the watcher's pass at at past 12:00, after a new class_frozen
// row of the same reset when arm is set.
func (p *freezeProbe) pass(at time.Duration, arm bool) {
	p.t.Helper()
	p.now = p.base.Add(at)
	if arm {
		p.store.rows = append(p.store.rows, FreezeEvent{ID: int64(len(p.store.rows) + 1), Repository: "sf/acme", At: p.now, Kind: "class_frozen", Class: "big",
			Reset: p.base.Add(5 * time.Hour).Format(time.RFC3339), Words: "free quota exhausted"})
	}
	if err := p.w.Check(context.Background()); err != nil {
		p.t.Fatal(err)
	}
}

// close closes #1's issue as factoryd's recorded close at at, in said, and
// ends the card when the webhook of that close lands.
func (p *freezeProbe) close(at time.Duration, said string, webhook bool) {
	p.t.Helper()
	p.now = p.base.Add(at)
	card := freezeCardOn(p.t, p.store, 1)
	if err := closeIssueAsFactory(context.Background(), p.store, p.forgejo, card, said); err != nil {
		p.t.Fatal(err)
	}
	if webhook {
		if err := p.store.TransitionTask(context.Background(), card.ID, []string{"waiting-human"}, "done"); err != nil {
			p.t.Fatal(err)
		}
	}
}

// Hand-derived from a press whose face write failed: the press's close of
// #1 is recorded at 12:10 and its webhook ends the card, which keeps the
// holding face. The hold never armed again. A watcher built anew passes at
// 12:11 and 12:12: the record's time, after the arm of 12:00, tells that
// the press answered the standing freeze, so #1 stays done with no re-arm
// comment.
func TestFreezeCardPressAnsweredByItsRecord(t *testing.T) {
	p := newFreezeProbe(t)
	p.close(10*time.Minute, "factoryd: sebastian's retry press cleared the big class's hold: the freeze card closes at once.", true)
	said := len(p.fj.comments)
	p.restart()
	p.pass(11*time.Minute, false)
	p.pass(12*time.Minute, false)
	if c := freezeCardOn(t, p.store, 1); c.Status != "done" || len(p.fj.comments) != said || len(p.fj.issues) != 1 {
		t.Fatalf("#1 stands %s over %d issues with the comments %q, want it done as the press left it", c.Status, len(p.fj.issues), p.fj.comments[said:])
	}
}

// Hand-derived from a human's close in flight: the console records the
// close of #1 and closes its issue at 12:10, and the row waits on the
// webhook. A watcher built anew passes at 12:11: the record, newer than
// the freeze of 12:00, stays on the issue, so the webhook's close counts
// as factoryd's recorded one. The card then ends, and the passes at 12:12
// and, by another new watcher, 12:13 leave it as the human closed it.
func TestFreezeCardHumanCloseInFlightKeepsItsRecord(t *testing.T) {
	ctx := context.Background()
	p := newFreezeProbe(t)
	p.close(10*time.Minute, "sebastian closed this card.", false)
	p.restart()
	p.pass(11*time.Minute, false)
	card := freezeCardOn(t, p.store, 1)
	counts, err := closeCounts(ctx, p.store, "factory-agent", nil, card, "factory-agent")
	if err != nil || !counts || card.Status != "waiting-human" {
		t.Fatalf("the webhook's close of #1, which stands %s, counts: %t, %v; want the console's record still on the issue", card.Status, counts, err)
	}
	if err := p.store.TransitionTask(ctx, card.ID, []string{"waiting-human"}, "done"); err != nil {
		t.Fatal(err)
	}
	said := len(p.fj.comments)
	p.pass(12*time.Minute, false)
	p.restart()
	p.pass(13*time.Minute, false)
	if c := freezeCardOn(t, p.store, 1); c.Status != "done" || c.Objective != card.Objective || len(p.fj.comments) != said || len(p.fj.issues) != 1 {
		t.Fatalf("#1 stands %s over %d issues with the comments %q, want it as the human closed it:\n%s", c.Status, len(p.fj.issues), p.fj.comments[said:], c.Objective)
	}
}

// pressedCard builds the big class's card #1, frozen at 12:00 to the
// reset five days out and closed by the retry press at 13:58.
func pressedCard(h *freezeTable) {
	h.row("class_frozen", 5)
	h.pass()
	h.at(118 * time.Minute)
	h.press()
	h.pass()
	h.at(2 * time.Hour)
}

// Hand-derived from a reopen the class's resume follows in one pass: #1,
// which the press closed at 13:58, meets at 14:00 a dated refusal of its
// reset and then the class's resume, both read by one pass. Without a
// fault #1 opens again, with the re-arm's comment, and takes the resumed
// face: it waits, its issue open, to close itself at 14:05. Every write
// and call of that pass failing once, with a restart too, must end there,
// but the reopen's own write: it changes nothing, so #1 stays closed.
func TestFreezeCardFaultsOfAReopenTheResumeFollows(t *testing.T) {
	h := freezeFaults(t, pressedCard, func(h *freezeTable) {
		h.row("class_frozen", 5)
		h.row("class_resumed", 5)
		h.pass()
	}, func() map[string]freezeFaultException {
		// the reopen's one write failed and changed nothing, and the resume
		// in the same pass ended the freeze: no arm stands to open #1 again
		kept := freezeFaultException{"the reopen's write failed and the class resumed in the same pass: #1 stays as the press closed it",
			"#1 done pressed R1 | open  | 1 comments, 1 ends, 0 wakes"}
		return map[string]freezeFaultException{"ReopenFreezeCard": kept, "ReopenFreezeCard/restart": kept}
	}())
	if got, want := h.reached(), "#1 waiting resumed R1 | open #1 | 2 comments, 1 ends, 0 wakes"; got != want {
		t.Fatalf("the cards reach %q, want %q", got, want)
	}
}

// Hand-derived from a press and a re-arm in one second and a human's close
// after them: #1 is pressed closed at 14:00 and a dated refusal of the
// same second opens it again; the pass after it changes nothing. Then a
// human closes #1 through the console. Without a fault #1 ends done under
// its holding face, its issue closed, with three comments: the press's,
// the re-arm's and the human's. No later pass opens it again, whichever
// write or call of the reopen failed once, with a restart too.
func TestFreezeCardFaultsOfAReopenAHumanThenCloses(t *testing.T) {
	h := freezeFaults(t, func(h *freezeTable) {
		h.row("class_frozen", 5)
		h.pass()
		h.at(2 * time.Hour)
		h.press()
	}, func(h *freezeTable) {
		h.row("class_frozen", 5)
		h.pass()
		h.pass()
		h.humanClose(1)
	}, nil)
	if got, want := h.reached(), "#1 done frozen R1 | open  | 3 comments, 1 ends, 0 wakes"; got != want {
		t.Fatalf("the cards reach %q, want %q", got, want)
	}
}

// Hand-derived from one class armed by two repositories: sf/acme's dated
// refusal and then sf/other's, of one reset, read by one pass. Without a
// fault each repository holds its own open card. A filing Forgejo did not
// take is made again by the next pass, and by a watcher built anew, in
// either repository.
func TestFreezeCardFaultsOfTwoRepositoriesFilings(t *testing.T) {
	h := freezeFaults(t, func(h *freezeTable) {}, func(h *freezeTable) {
		h.rowIn("sf/acme", "class_frozen", 5)
		h.rowIn("sf/other", "class_frozen", 5)
		h.pass()
	}, nil)
	if got, want := h.reached(), "#1 waiting frozen R1; #2 waiting frozen R1 | open #1 #2 | 0 comments, 0 ends, 0 wakes"; got != want {
		t.Fatalf("the cards reach %q, want %q", got, want)
	}
}
