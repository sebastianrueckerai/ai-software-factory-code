package acceptance

// The freeze card's acceptance proof at the compiled factoryd: the real
// FreezeCard watcher over a real PGStore beside the suite's throwaway
// Postgres, its Forgejo the fake this file stands, reading the gateway's
// class_frozen and class_resumed rows off the shared events table. One
// hold files one card the moment it arms, whatever the reset's distance;
// a re-arm updates the same card; a self-resume closes it five minutes
// later on its own; a retry press closes it at once. The card holds no
// work slot and asks nothing until its freeze outgrows
// waits.provider_limit; then it alone asks, once.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
const plFreezeRepo = "sf/code"

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
		case r.Method == http.MethodGet && rest == "issues":
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

// plFreezeEvent inserts one gateway event row of the proofs' repository
// on the shared events table, the record the watcher reads, written now.
func plFreezeEvent(t *testing.T, db *sql.DB, kind, class, reset, words string) {
	t.Helper()
	plFreezeEventAt(t, db, plFreezeRepo, time.Now(), kind, class, reset, words)
}

// plFreezeEventAt inserts one gateway event row of repository, written at
// at.
func plFreezeEventAt(t *testing.T, db *sql.DB, repository string, at time.Time, kind, class, reset, words string) {
	t.Helper()
	detail, err := json.Marshal(map[string]string{"class": class, "reset": reset, "words": words})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail)
		VALUES ($1, $2, '', $3, 'gateway', 'gateway', $4)`, at, repository, kind, detail); err != nil {
		t.Fatal(err)
	}
}

// plFreezeWatcher wires the real watcher over the real store and the fake
// Forgejo, under a policy version that runs the class big on zai and whose
// waits.provider_limit is limit: the wait past which the class's card
// speaks once.
func plFreezeWatcher(s *factoryd.PGStore, forge *factoryd.Forgejo, limit time.Duration) *factoryd.FreezeCard {
	version := policy.Version{ID: 1, Commit: "fz-1", Tree: policy.Tree{Document: policy.Document{
		Waits:          policy.Waits{ProviderLimit: limit},
		Classes:        map[string]policy.Class{"big": {Provider: "zai"}},
		ProviderLimits: map[string]policy.ProviderLimit{"zai": {TZ: "Asia/Singapore"}}}}}
	return &factoryd.FreezeCard{Store: s, Forgejo: forge, Current: func() policy.Version { return version }}
}

// plFreezeWorkCards files the nine running cards of the class big the
// freeze covers, through the production writer.
func plFreezeWorkCards(ctx context.Context, t *testing.T, s *factoryd.PGStore, prefix string) {
	t.Helper()
	for i := 0; i < 9; i++ {
		card := factoryd.Task{ID: fmt.Sprintf("%s-%d", prefix, i), Kind: "agent", Track: policy.TrackDirect, Status: "running",
			Repository: plFreezeRepo, Objective: "the freeze's work", RequiredCapabilities: []string{"big"}}
		if err := s.CreateTask(ctx, card); err != nil {
			t.Fatal(err)
		}
	}
}

// plFreezeCard reads the class's freeze card off the store, the waiting
// one first; ok is false while none stands.
func plFreezeCard(ctx context.Context, t *testing.T, s *factoryd.PGStore) (factoryd.Task, bool) {
	t.Helper()
	for _, status := range []string{"waiting-human", "done"} {
		cards, err := s.ListTasksByStatus(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cards {
			if c.Source == factoryd.SourceFreezeCard {
				return c, true
			}
		}
	}
	return factoryd.Task{}, false
}

// plFreezeCards counts the store's freeze-card rows.
func plFreezeCards(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE source = $1`, factoryd.SourceFreezeCard).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestFreezeCardSpeaksForTheClass: one hold files one card the moment it
// arms — naming the words, the countdown and the count, holding no work
// slot and asking nothing; a re-arm updates the same card, no second one;
// a self-resume leaves the card five minutes, the held time on its face,
// and then it closes on its own and wakes the nine frozen cards once; a
// retry press closes it at once. A dated refusal before the reset the
// pressed card held reopens it, and a second press closes it with its own
// comment; a freeze three days later, past that reset, files a new card.
// The five-minute self-resume close is slept for real.
// [one_freeze_card_per_class]
func TestFreezeCardSpeaksForTheClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, db := accReceiptStore(t, "freeze_card_class")
	forge, fj := newAccFreezeForgejo(t)
	w := plFreezeWatcher(s, forge, 24*time.Hour)
	var woke []string
	w.Signal = func(_ context.Context, id string) error { woke = append(woke, id); return nil }
	plFreezeWorkCards(ctx, t, s, "fz-work")

	// the hold arms on zai's five-hour window: the card files at once,
	// its countdown read off the watcher's clock half a minute short of
	// five hours later
	reset := time.Now().UTC().Add(5*time.Hour + 30*time.Second).Truncate(time.Second)
	plFreezeEvent(t, db, "class_frozen", "big", reset.Format(time.RFC3339), plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card, ok := plFreezeCard(ctx, t, s)
	if !ok || card.Status != "waiting-human" || card.Kind != "human" || card.AdmittedAt != nil {
		t.Fatalf("the freeze card = %+v, want a waiting human card that holds no slot", card)
	}
	if n := plFreezeCards(t, db); n != 1 || len(fj.issues) != 1 {
		t.Fatalf("%d freeze cards on %d issues, want the one card", n, len(fj.issues))
	}
	for _, want := range []string{"Class: big", plFreezeWords,
		"The class resets at " + reset.Format(time.RFC3339) + " (in 5h 00m)", "9 cards of the class wait"} {
		if !strings.Contains(fj.issues[0], want) {
			t.Errorf("the card's face lacks %q:\n%s", want, fj.issues[0])
		}
	}
	// no ask anywhere: the card informs, the work cards ask nothing
	if asks := len(fj.comments); asks != 0 {
		t.Fatalf("%d comments filed, want none: the card asks nothing", asks)
	}

	// a re-arm with a later reset updates the same card, no second card
	later := reset.Add(4 * time.Hour)
	plFreezeEvent(t, db, "class_frozen", "big", later.Format(time.RFC3339), plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if n := plFreezeCards(t, db); n != 1 || len(fj.issues) != 1 {
		t.Fatalf("the re-arm left %d freeze cards on %d issues, want the one card", n, len(fj.issues))
	}
	updated, _ := plFreezeCard(ctx, t, s)
	if want := "The class resets at " + later.Format(time.RFC3339) + " (in 9h 00m)"; !strings.Contains(updated.Objective, want) {
		t.Errorf("the re-arm left the face stale, want %q:\n%s", want, updated.Objective)
	}

	// a self-resume: the held time on the face, then five minutes, then
	// the card closes on its own
	plFreezeEvent(t, db, "class_resumed", "big", later.Format(time.RFC3339), "")
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, _ := plFreezeCard(ctx, t, s)
	if resumed.Status != "waiting-human" || !strings.Contains(resumed.Objective, "the freeze held 0h 00m") {
		t.Fatalf("after the resume the card stands %s:\n%s", resumed.Status, resumed.Objective)
	}
	time.Sleep(5*time.Minute + 10*time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	closed, _, err := s.Task(ctx, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "done" {
		t.Fatalf("after the resume hold the card = %+v, want %s done", closed, card.ID)
	}
	if got := fj.patches[card.IssueNumber]; got != "closed" {
		t.Errorf("issue #%d's last patch = %q, want closed", card.IssueNumber, got)
	}
	// the resume held five minutes: the class works, and its nine frozen
	// cards wake once, not at their reset
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Sort(woke); len(woke) != 9 || len(slices.Compact(slices.Clone(woke))) != 9 || woke[0] != "fz-work-0" || woke[8] != "fz-work-8" {
		t.Errorf("the resume that held woke %q, want the nine frozen cards once each", woke)
	}

	// a retry press closes the class's open card at once, no five-minute
	// grace: a new hold files a card of its own (the self-closed episode
	// ended), the press closes it with no wait
	plFreezeEvent(t, db, "class_frozen", "big", later.Add(24*time.Hour).Format(time.RFC3339), plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, ok := plFreezeCard(ctx, t, s)
	if !ok || reopened.Status != "waiting-human" || reopened.ID == card.ID || len(fj.issues) != 2 {
		t.Fatalf("the new hold's card = %+v on %d issues, want a second waiting card", reopened, len(fj.issues))
	}
	if err := w.CloseOnRetry(ctx, plFreezeRepo, "big", "sebastian"); err != nil {
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
	pressSaid := fmt.Sprintf("#issues/%d sebastian's retry press cleared the big class's hold: the freeze card closes at once.", reopened.IssueNumber)
	if !slices.Contains(fj.comments, pressSaid) {
		t.Errorf("the issue's comments %q lack the press's close naming who pressed: %q", fj.comments, pressSaid)
	}
	// a dated refusal before the reset the pressed card held is the same
	// episode: it reopens the card, and a second press closes it again,
	// with its own comment
	plFreezeEvent(t, db, "class_frozen", "big", later.Add(25*time.Hour).Format(time.RFC3339), plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _, err := s.Task(ctx, reopened.ID); err != nil || again.Status != "waiting-human" || plFreezeCards(t, db) != 2 || len(fj.issues) != 2 {
		t.Fatalf("the refusal after the press left the card %s (%v) over %d issues, want it reopened", again.Status, err, len(fj.issues))
	}
	if err := w.CloseOnRetry(ctx, plFreezeRepo, "big", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if n := len(slices.DeleteFunc(slices.Clone(fj.comments), func(c string) bool { return c != pressSaid })); n != 2 || fj.patches[reopened.IssueNumber] != "closed" {
		t.Errorf("after the second press issue #%d holds %d press closes (%q) and is %q, want its own second close", reopened.IssueNumber, n, fj.comments, fj.patches[reopened.IssueNumber])
	}
	// a second press is quiet: no open card stands, no second close
	comments := len(fj.comments)
	if err := w.CloseOnRetry(ctx, plFreezeRepo, "big", "sebastian"); err != nil {
		t.Fatalf("the second press: %v", err)
	}
	if len(fj.comments) != comments {
		t.Errorf("the second press commented %q, want nothing", fj.comments[comments:])
	}
	// a freeze three days on lies past the reset the pressed card held: a
	// new episode, a new card, the pressed one left done and unsaid
	threeDays := time.Now().UTC().Add(72 * time.Hour)
	plFreezeEventAt(t, db, plFreezeRepo, threeDays, "class_frozen", "big", threeDays.Add(5*time.Hour).Format(time.RFC3339), plFreezeWords)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if old, _, err := s.Task(ctx, reopened.ID); err != nil || old.Status != "done" || plFreezeCards(t, db) != 3 || len(fj.issues) != 3 || len(fj.comments) != comments {
		t.Fatalf("the freeze three days on left the pressed card %s (%v), %d cards on %d issues, comments %q; want a new card",
			old.Status, err, plFreezeCards(t, db), len(fj.issues), fj.comments[comments:])
	}
}

// TestFreezeCardAsksAtProviderLimit: a freeze held past
// waits.provider_limit speaks once — the card alone asks add a fallback,
// switch the model, keep waiting — and the work cards ask nothing; a
// second pass stays quiet. The proof's policy names provider_limit at
// its floor of one minute, so the ask costs a minute of wall clock.
// [one_freeze_card_per_class]
func TestFreezeCardAsksAtProviderLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, db := accReceiptStore(t, "freeze_card_ask")
	forge, fj := newAccFreezeForgejo(t)
	w := plFreezeWatcher(s, forge, time.Minute)
	plFreezeWorkCards(ctx, t, s, "fz-ask")

	// the hold arms: the card files at once and asks nothing
	reset := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Second)
	plFreezeEvent(t, db, "class_frozen", "big", reset.Format(time.RFC3339), plFreezeWords)
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
	time.Sleep(70 * time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if asks := len(fj.comments); asks != 1 || !strings.Contains(fj.comments[0], "The provider's limit has held the big class since ") ||
		!strings.Contains(fj.comments[0], ", for more than 1 minute.") {
		t.Fatalf("comments %q, want the one provider-limit ask", fj.comments)
	}
	for _, want := range []string{"Add a fallback", "switch the class's model", "keep waiting", "The work cards ask nothing"} {
		if !strings.Contains(fj.comments[0], want) {
			t.Errorf("the ask lacks %q: %s", want, fj.comments[0])
		}
	}
	if !strings.HasPrefix(fj.comments[0], fmt.Sprintf("#issues/%d ", card.IssueNumber)) {
		t.Errorf("the ask %q is not on the card's issue #%d: the work cards ask nothing", fj.comments[0], card.IssueNumber)
	}
	asked, _ := plFreezeCard(ctx, t, s)
	if !strings.Contains(asked.Objective, "The provider's limit has held the big class since ") || !strings.Contains(asked.Objective, ", for more than 1 minute.") {
		t.Errorf("the card's face lacks the ask:\n%s", asked.Objective)
	}

	// a second pass asks nothing again
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if asks := len(fj.comments); asks != 1 {
		t.Fatalf("%d comments after the second pass, want the one ask still", asks)
	}
}
