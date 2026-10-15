package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// codeIssues is the code repository's issue API as the upstream card uses
// it: the open issues, a new issue (numbered from 71), its labels.
type codeIssues struct {
	mu     sync.Mutex
	issues []string // "title\n\nbody", the first numbered 71
	closed []int64
	labels map[int64][]string
}

func (c *codeIssues) srv(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/ai-software-factory-code/")
		switch {
		case r.Method == http.MethodGet && path == "issues":
			open := []map[string]any{}
			for i, is := range c.issues {
				if n := int64(71 + i); r.URL.Query().Get("page") == "1" && !slices.Contains(c.closed, n) {
					title, _, _ := strings.Cut(is, "\n\n")
					open = append(open, map[string]any{"number": n, "title": title, "user": map[string]string{"login": "factory-agent"}})
				}
			}
			json.NewEncoder(w).Encode(open) // #nosec G104 -- test stub
		case r.Method == http.MethodPost && path == "issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			c.issues = append(c.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 70 + len(c.issues)}) // #nosec G104 -- test stub
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/labels"):
			var in struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "issues/"), "/labels"), 10, 64)
			c.labels[n] = in.Labels
			json.NewEncoder(w).Encode([]any{}) // #nosec G104 -- test stub
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// The upstream card comes once a week on the code repository and never
// while one is open: a human card (an agent's sandbox reaches no
// registry or release page) naming the list file and every pin to bump.
// Hand-derived: no card yet files #71; while it is open nothing; closed
// six days after its filing nothing; closed and eight days old, #72.
func TestRollerFilesTheUpstreamCardWeekly(t *testing.T) {
	ctx := context.Background()
	issues := &codeIssues{labels: map[int64][]string{}}
	store := newFakeStore()
	r := &Roller{Forgejo: issues.srv(t), Store: store, Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}}
	if err := r.upstream(ctx); err != nil {
		t.Fatal(err)
	}
	if len(issues.issues) != 1 || len(store.tasks) != 1 {
		t.Fatalf("issues %d, tasks %d, want 1 and 1", len(issues.issues), len(store.tasks))
	}
	var card Task
	for _, card = range store.tasks {
	}
	if card.Kind != "human" || card.Status != "waiting-human" || card.Source != SourceUpstream || card.IssueNumber != 71 ||
		card.Repository != "sf/ai-software-factory-code" || card.Author != "factory-agent" {
		t.Fatalf("card %+v", card)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	title, body, _ := strings.Cut(issues.issues[0], "\n\n")
	if title != "Weekly bump of the pinned outside inputs" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"upstream-images.txt", "DEBIAN_SNAPSHOT", "POSTGRESQL_VERSION", "LIBPQ_VERSION", "POSTGRESQL_COMMON_VERSION",
		"apt-archive.postgresql.org", "go.mod's `toolchain`", "ADD --checksum", "buildkitSHA256", "scripts/upstream-images-test/test.sh",
		"chart", "index.yaml", "ts-go template", "Talos", "Cilium"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card names no %s:\n%s", want, body)
		}
	}
	if err := r.upstream(ctx); err != nil {
		t.Fatal(err)
	}
	if len(issues.issues) != 1 {
		t.Fatalf("a second card while #71 is open: %d issues", len(issues.issues))
	}
	card.Status = "done"
	card.CreatedAt = time.Now().Add(-6 * 24 * time.Hour)
	store.tasks[card.ID] = card
	issues.closed = append(issues.closed, 71)
	if err := r.upstream(ctx); err != nil {
		t.Fatal(err)
	}
	if len(issues.issues) != 1 {
		t.Fatalf("a card six days after the last: %d issues", len(issues.issues))
	}
	card.CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
	store.tasks[card.ID] = card
	if err := r.upstream(ctx); err != nil {
		t.Fatal(err)
	}
	if len(issues.issues) != 2 || len(store.tasks) != 2 {
		t.Fatalf("a week after the last: issues %d, tasks %d, want 2 and 2", len(issues.issues), len(store.tasks))
	}
}

// A card whose issue was filed and whose row then failed is taken on by
// the next poll, not filed again.
func TestRollerTakesOnTheUpstreamIssueWhoseRowFailed(t *testing.T) {
	ctx := context.Background()
	issues := &codeIssues{labels: map[int64][]string{}}
	store := newFakeStore()
	book := &failingTasks{fakeStore: store, failNext: 1}
	r := &Roller{Forgejo: issues.srv(t), Store: book, Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}}
	if err := r.upstream(ctx); err == nil {
		t.Fatal("a failed task row raised nothing")
	}
	if err := r.upstream(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.upstream(ctx); err != nil {
		t.Fatal(err)
	}
	if len(issues.issues) != 1 || len(store.tasks) != 1 {
		t.Fatalf("issues %d, tasks %d, want 1 and 1", len(issues.issues), len(store.tasks))
	}
	for _, card := range store.tasks {
		if card.IssueNumber != 71 || card.Source != SourceUpstream || card.Status != "waiting-human" {
			t.Fatalf("card %+v", card)
		}
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL), like TestPGStore.
func TestPGStoreNewestTaskOf(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	now := time.Now()
	for _, tk := range []Task{
		{ID: "old", Repository: "sf/code", Source: SourceUpstream, CreatedAt: now.Add(-14 * 24 * time.Hour)},
		{ID: "new", Repository: "sf/code", Source: SourceUpstream, CreatedAt: now.Add(-7 * 24 * time.Hour)},
		{ID: "other-source", Repository: "sf/code", Source: SourceRoll, CreatedAt: now},
		{ID: "other-repo", Repository: "sf/infra", Source: SourceUpstream, CreatedAt: now},
	} {
		tk.BudgetEUR, tk.Kind, tk.Status = 1, "human", "waiting-human"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	got, ok, err := s.NewestTaskOf(ctx, "sf/code", SourceUpstream)
	if err != nil || !ok || got.ID != "new" {
		t.Fatalf("NewestTaskOf = %s, %v, %v; want new", got.ID, ok, err)
	}
	if _, ok, err := s.NewestTaskOf(ctx, "sf/none", SourceUpstream); err != nil || ok {
		t.Fatalf("NewestTaskOf of a repository with none = %v, %v", ok, err)
	}
}
