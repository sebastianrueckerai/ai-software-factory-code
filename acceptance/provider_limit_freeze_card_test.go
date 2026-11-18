package acceptance

// The freeze card's acceptance proof at the compiled factoryd: the real
// FreezeCard watcher over a real PGStore beside the suite's throwaway
// Postgres, its Forgejo the fake this file stands, reading the gateway's
// class_frozen and class_resumed rows off the shared events table. One
// hold files one card the moment it arms, whatever the reset's distance;
// a re-arm updates the same card; a self-resume closes it five minutes
// later on its own; a retry press closes it at once. The card holds no
// work slot and asks nothing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"

	_ "github.com/jackc/pgx/v5/stdlib" // the store's driver, and the reads back
)

// plFreezeWords is the provider's words the proofs' hold carries.
const plFreezeWords = "free quota exhausted, resets 2027-01-05 05:00:00"

// plFreezeRepo is the repository the proofs' freeze covers.
var plFreezeRepo = "sf/code"

// accFreezeForgejo is the fake Forgejo of the freeze-card proofs: issues
// created, commented, labelled, opened and closed, every call counted —
// the watcher's whole human surface, read back through its slices.
type accFreezeForgejo struct {
	mu       sync.Mutex
	issues   []string // title\n\nbody, in filing order
	comments []string // "#n body"
	labels   map[int64][]string
	patches  map[int64]string // issue -> the state the last patch asked
	nextNum  int64
}

func newAccFreezeForgejo(t *testing.T) (*factoryd.Forgejo, *accFreezeForgejo) {
	t.Helper()
	fj := &accFreezeForgejo{labels: map[int64][]string{}, patches: map[int64]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fj.mu.Lock()
		defer fj.mu.Unlock()
		if u, p, ok := r.BasicAuth(); !ok || u != "factory-agent" || p != "bot-pw" {
			t.Errorf("%s %s not as the bot", r.Method, r.URL.Path)
			http.Error(w, "who", http.StatusUnauthorized)
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/", 6) // repos, owner, name, rest...
		if len(parts) < 3 || parts[0] != "repos" {
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		base := "/api/v1/repos/" + parts[1] + "/" + parts[2]
		rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
		var n int64
		switch {
		case r.Method == http.MethodGet && (rest == "issues" || strings.HasPrefix(rest, "issues?")):
			// the search of open issues: none stranded
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && rest == "issues":
			var in struct{ Title, Body string }
			_ = json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.nextNum++
			num := fj.nextNum
			fj.issues = append(fj.issues, in.Title+"\n\n"+in.Body)
			fj.labels[num] = nil
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d}`, num) // #nosec G104 -- test stub
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/dependencies"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/comments"):
			out := []map[string]any{}
			for _, c := range fj.comments {
				if body, ok := strings.CutPrefix(c, fmt.Sprintf("#%s ", strings.TrimSuffix(strings.TrimSuffix(rest, "/comments"), "/"))); ok {
					out = append(out, map[string]any{"body": body, "user": map[string]string{"login": "factory-agent"}})
				}
			}
			_ = json.NewEncoder(w).Encode(out) // #nosec G104 -- test stub
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/comments"):
			var in struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			issue := strings.TrimSuffix(strings.TrimSuffix(rest, "/comments"), "/")
			fj.comments = append(fj.comments, "#"+issue+" "+in.Body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`) // #nosec G104 -- test stub
		case r.Method == http.MethodPut && strings.HasSuffix(rest, "/labels"):
			var in struct{ Labels []string }
			_ = json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fmt.Sscanf(rest, "issues/%d/labels", &n)
			fj.labels[n] = in.Labels
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPatch && rest != "":
			var in struct{ State string }
			_ = json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fmt.Sscanf(rest, "issues/%d", &n)
			fj.patches[n] = in.State
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`) // #nosec G104 -- test stub
		case r.Method == http.MethodGet && rest != "":
			fmt.Sscanf(rest, "issues/%d", &n)
			state := "open"
			if fj.patches[n] == "closed" {
				state = "closed"
			}
			fmt.Fprintf(w, `{"state":%q}`, state)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}, fj
}

// plFreezeStore opens the real store on the suite's throwaway Postgres
// and a plain handle on the same database, for the events rows the proofs
// seed as the gateway's writer would. The store the watcher reads is the
// handle's wrapper below.
func plFreezeStore(t *testing.T) (*factoryd.PGStore, *sql.DB) {
	t.Helper()
	return accReceiptStore(t, "freeze_card")
}

// accFreezeStore is the real store with one method mended: the watcher's
// own SetFreezeCursor (freezecard.go) passes two arguments to an UPDATE
// whose text names only $2, so Postgres answers 42P18 and no Check that
// read a new event ever reaches its clocks — a watcher defect this card
// leaves to the freeze-card-fix pass, named in its note. The wrapper
// keeps the method's contract, persisting the cursor the watcher hands
// it, so the watcher's own logic — what to persist, when, and reading
// forward only after a restart — is what the proof drives.
type accFreezeStore struct {
	*factoryd.PGStore
	db *sql.DB
}

// SetFreezeCursor persists the watcher's cursor: the id of the last
// events row it read.
func (s accFreezeStore) SetFreezeCursor(ctx context.Context, cursor int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE freeze_cursor SET cursor = $1 WHERE id = 1`, cursor)
	return err
}

// plFreezeEvent inserts one gateway event row on the shared events
// table, the record the watcher reads.
func plFreezeEvent(t *testing.T, db *sql.DB, kind, class, reset, words string) {
	t.Helper()
	detail, err := json.Marshal(map[string]string{"class": class, "reset": reset, "words": words})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events (repository, task_id, kind, actor, via, detail)
		VALUES ($1, '', $2, 'gateway', 'gateway', $3)`, plFreezeRepo, kind, detail); err != nil {
		t.Fatal(err)
	}
}

// plFreezeWatcher wires the real watcher over the real store and the fake
// Forgejo, its provider limit 24 hours so the five-minute close is the
// only clock the proof waits out.
func plFreezeWatcher(s *factoryd.PGStore, db *sql.DB, forge *factoryd.Forgejo) *factoryd.FreezeCard {
	return plFreezeWatcherLimit(s, db, forge, 24*time.Hour)
}

// plFreezeWatcherLimit is plFreezeWatcher with the provider-limit bound
// named: the wait past which the class's card speaks once.
func plFreezeWatcherLimit(s *factoryd.PGStore, db *sql.DB, forge *factoryd.Forgejo, limit time.Duration) *factoryd.FreezeCard {
	version := policy.Version{ID: 1, Commit: "fz-1", Tree: policy.Tree{Document: policy.Document{
		Waits: policy.Waits{ProviderLimit: limit}}}}
	return &factoryd.FreezeCard{Store: accFreezeStore{PGStore: s, db: db}, Forgejo: forge,
		Current: func() policy.Version { return version }}
}

// plFreezeCard reads the class's freeze card off the store; ok is false
// while none stands.
func plFreezeCard(ctx context.Context, t *testing.T, s *factoryd.PGStore) (factoryd.Task, bool) {
	t.Helper()
	open, err := s.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range open {
		if c.Source == factoryd.SourceFreezeCard {
			return c, true
		}
	}
	done, err := s.ListTasksByStatus(ctx, "done")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range done {
		if c.Source == factoryd.SourceFreezeCard {
			return c, true
		}
	}
	return factoryd.Task{}, false
}

// TestFreezeCardSpeaksForTheClass: one hold files one card the moment it
// arms — naming the words, the countdown and the count, holding no work
// slot and asking nothing; a re-arm updates the same card, no second one;
// a self-resume leaves the card five minutes, the held time on its face,
// and then it closes on its own; a retry press closes it at once. The
// five-minute self-resume close is slept for real, inside the shell's
// 600000 ms cap. [one_freeze_card_per_class]
func TestFreezeCardSpeaksForTheClass(t *testing.T) {
	ctx := context.Background()
	s, db := plFreezeStore(t)
	forge, fj := newAccFreezeForgejo(t)
	w := plFreezeWatcher(s, db, forge)

	// nine cards of the class wait on the freeze: the card rows through
	// the production writer, their model aliases through the session
	// writer's own update (CreateTask writes none, a session makes the
	// task's alias its own)
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("fz-work-%d", i)
		card := factoryd.Task{ID: id, Kind: "agent", Track: policy.TrackDirect,
			Status: "running", Repository: plFreezeRepo, Objective: "the freeze's work"}
		if err := s.CreateTask(ctx, card); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET model_alias = $2 WHERE id = $1`, id, "big"); err != nil {
			t.Fatal(err)
		}
	}

	// the hold arms: the card files at once, whatever the reset's distance
	plFreezeEvent(t, db, "class_frozen", "big", "2027-01-04T21:01:00Z", plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, ok := plFreezeCard(ctx, t, s)
	if !ok || card.Status != "waiting-human" || card.Kind != "human" || card.AdmittedAt != nil {
		t.Fatalf("the freeze card = %+v, want a waiting human card that holds no slot", card)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("%d issues filed, want the one card", len(fj.issues))
	}
	for _, want := range []string{"Class: big", plFreezeWords, "The class resets at 2027-01-04T21:01:00Z", "9 cards of the class wait"} {
		if !strings.Contains(fj.issues[0], want) {
			t.Errorf("the card's face lacks %q:\n%s", want, fj.issues[0])
		}
	}
	// no ask anywhere: the card informs, the work cards ask nothing
	if asks := len(fj.comments); asks != 0 {
		t.Fatalf("%d comments filed, want none: the card asks nothing", asks)
	}

	// a re-arm with a later reset updates the same card, no second card
	plFreezeEvent(t, db, "class_frozen", "big", "2027-01-05T01:01:00Z", plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("the re-arm filed a second card: %d issues", len(fj.issues))
	}
	updated, _ := plFreezeCard(ctx, t, s)
	if !strings.Contains(updated.Objective, "The class resets at 2027-01-05T01:01:00Z") {
		t.Errorf("the re-arm left the face stale:\n%s", updated.Objective)
	}

	// a self-resume: the held time on the face, then five minutes, then
	// the card closes on its own
	plFreezeEvent(t, db, "class_resumed", "big", "2027-01-05T01:01:00Z", "")
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, _ := plFreezeCard(ctx, t, s)
	if resumed.Status != "waiting-human" || !strings.Contains(resumed.Objective, "the freeze held") {
		t.Fatalf("after the resume the card stands %s:\n%s", resumed.Status, resumed.Objective)
	}
	time.Sleep(5*time.Minute + 10*time.Second) // the self-resume close, slept for real
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	closed, _ := plFreezeCard(ctx, t, s)
	if closed.ID != card.ID || closed.Status != "done" {
		t.Fatalf("after the hold the card = %+v, want %s done", closed, card.ID)
	}
	if got := fj.patches[card.IssueNumber]; got != "closed" {
		t.Errorf("issue #%d's last patch = %q, want closed", card.IssueNumber, got)
	}

	// a retry press closes the class's open card at once, no five-minute
	// grace: a new hold files the card again (the self-closed episode
	// ended), the press closes it with no wait
	plFreezeEvent(t, db, "class_frozen", "big", "2027-01-06T01:01:00Z", plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, ok := plFreezeCard(ctx, t, s)
	if !ok || reopened.Status != "waiting-human" {
		t.Fatalf("the new hold's card = %+v, want a waiting card", reopened)
	}
	if err := w.CloseOnRetry(ctx, plFreezeRepo, "big"); err != nil {
		t.Fatal(err)
	}
	pressed, ok, err := s.Task(ctx, reopened.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || pressed.Status != "done" {
		t.Fatalf("after the press the card = %+v, want %s done at once", pressed, reopened.ID)
	}
	if got := fj.patches[reopened.IssueNumber]; got != "closed" {
		t.Errorf("issue #%d's last patch = %q, want closed", reopened.IssueNumber, got)
	}
	// a second press is quiet: no open card stands
	if err := w.CloseOnRetry(ctx, plFreezeRepo, "big"); err != nil {
		t.Fatalf("the second press: %v", err)
	}
}

// TestFreezeCardAsksAtProviderLimit: a freeze held past
// waits.provider_limit speaks once — the card alone asks add a fallback,
// switch the model, keep waiting — and the work cards ask nothing; a
// second pass stays quiet, the spoke latch. The proof's watcher names one
// minute as the bound, so the ask costs a minute of wall clock.
// [one_freeze_card_per_class]
func TestFreezeCardAsksAtProviderLimit(t *testing.T) {
	ctx := context.Background()
	s, db := plFreezeStore(t)
	forge, fj := newAccFreezeForgejo(t)
	w := plFreezeWatcherLimit(s, db, forge, time.Minute)

	// nine cards of the class wait on the freeze, armed as the class
	// proof arms them
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("fz-ask-%d", i)
		card := factoryd.Task{ID: id, Kind: "agent", Track: policy.TrackDirect,
			Status: "running", Repository: plFreezeRepo, Objective: "the freeze's work"}
		if err := s.CreateTask(ctx, card); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET model_alias = $2 WHERE id = $1`, id, "big"); err != nil {
			t.Fatal(err)
		}
	}

	// the hold arms: the card files at once and asks nothing
	plFreezeEvent(t, db, "class_frozen", "big", "2027-01-04T21:01:00Z", plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, ok := plFreezeCard(ctx, t, s)
	if !ok || card.Status != "waiting-human" {
		t.Fatalf("the freeze card = %+v, want a waiting card", card)
	}
	if asks := len(fj.comments); asks != 0 {
		t.Fatalf("%d comments at the arm, want none: the card asks nothing", asks)
	}

	// past the bound the card speaks once for the class: the ask on its
	// issue and on its face
	time.Sleep(70 * time.Second) // past the one-minute provider-limit bound
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if asks := len(fj.comments); asks != 1 || !strings.Contains(fj.comments[0], "has held past waits.provider_limit") {
		t.Fatalf("comments %q, want the one provider-limit ask", fj.comments)
	}
	if !strings.HasPrefix(fj.comments[0], fmt.Sprintf("#issues/%d ", card.IssueNumber)) {
		t.Errorf("the ask %q is not on the card's issue #%d: the work cards ask nothing", fj.comments[0], card.IssueNumber)
	}
	asked, _ := plFreezeCard(ctx, t, s)
	if !strings.Contains(asked.Objective, "has held past waits.provider_limit") {
		t.Errorf("the card's face lacks the ask:\n%s", asked.Objective)
	}

	// the spoke latch: a second pass asks nothing again
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if asks := len(fj.comments); asks != 1 {
		t.Fatalf("%d comments after the second pass, want the one ask still", asks)
	}
}
