package pg

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The fixtures the last group shares with factoryd's own test files and
// cannot reach across the package line: copies, kept verbatim except where
// they reach the surface.

// forgejoHub mints, lists and revokes like the live admin API: minted
// tokens show up in the list until they are revoked.
type forgejoHub struct {
	mu          sync.Mutex
	tokens      []TokenRef // live list, emptied by revocation
	mintedNames []string   // everything ever minted
	revoked     []int64
	nextID      int64
	calls       []string
	branchHead  string            // the task branch's head; empty = no branch yet
	committers  map[string]string // commit → its committer email; unlisted commits are a worker's
	parents     map[string]string // commit → its first parent; unlisted commits are root commits
	mainHead    string            // the task repository's main head
	comments    []string
	failWrites  int    // write-token mints to answer 502 before succeeding
	prFiles     string // a pull request's files, as Forgejo lists them
	// pulls are GET /pulls/{n} bodies, unlisted: 404; pullStatus answers
	// a number with its status instead
	pulls      map[int64]string
	pullStatus map[int64]int
	// branches are the heads of the fork's branches by name, which answer
	// before branchHead
	branches map[string]string
	// files are the raw files by "<ref>:<path>", unlisted: 404
	files map[string]string
	// statuses are combined-status bodies by commit, unlisted: no check
	// reported; statusReads are the commits read, in order
	statuses    map[string]string
	statusReads []string
	// mergeBases are the merge bases with main of the fork's commits by
	// commit, each one main reaches; the compare of an unlisted commit
	// fails
	mergeBases map[string]string
}

func (h *forgejoHub) minted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.mintedNames...)
}

func (h *forgejoHub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			var body struct {
				Name         string
				Scopes       []string
				Repositories []Repo
			}
			json.NewDecoder(r.Body).Decode(&body)
			if slices.Contains(body.Scopes, "write:repository") && h.failWrites > 0 {
				h.failWrites--
				h.calls = append(h.calls, fmt.Sprintf("mint %s %s failed", body.Name, strings.Join(body.Scopes, ",")))
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			h.nextID++
			h.tokens = append(h.tokens, TokenRef{ID: h.nextID, Name: body.Name})
			h.mintedNames = append(h.mintedNames, body.Name)
			repos := []string{}
			for _, repo := range body.Repositories {
				repos = append(repos, repo.String())
			}
			h.calls = append(h.calls, fmt.Sprintf("mint %s %s %s", body.Name, strings.Join(body.Scopes, ","), strings.Join(repos, ",")))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"sha1":"tok-%s"}`, h.nextID, body.Name)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/raw/"):
			_, file, _ := strings.Cut(r.URL.Path, "/raw/")
			body, ok := h.files[r.URL.Query().Get("ref")+":"+file]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, sha, _ := strings.Cut(r.URL.Path, "/commits/")
			sha = strings.TrimSuffix(sha, "/status")
			h.statusReads = append(h.statusReads, sha)
			io.WriteString(w, cmp.Or(h.statuses[sha], statusBody("")))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/") && strings.Count(r.URL.Path, "/") == 5:
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") && strings.Count(r.URL.Path, "/") == 5:
			h.calls = append(h.calls, "fork")
			name := path.Base(r.URL.Path)
			fmt.Fprintf(w, `{"full_name":"factory-agent/%s","fork":true,"private":true,"parent":{"full_name":"sf/%s"}}`, name, name)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			h.calls = append(h.calls, "list")
			list, _ := json.Marshal(h.tokens)
			w.Write(list)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			var list []map[string]any
			for i, c := range h.comments {
				list = append(list, map[string]any{"id": 501 + i, "body": c, "user": map[string]string{"login": "factory-agent"}})
			}
			b, _ := json.Marshal(list)
			w.Write(b)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			h.comments = append(h.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 500+len(h.comments))
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/git/commits/"):
			_, sha, _ := strings.Cut(r.URL.Path, "/git/commits/")
			parents := []map[string]string{}
			if p := h.parents[sha]; p != "" {
				parents = append(parents, map[string]string{"sha": p})
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"committer": map[string]string{"email": cmp.Or(h.committers[sha], "factory-agent@factory.local")}}, "parents": parents})
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/branches/"):
			if strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/") && strings.HasSuffix(r.URL.Path, "/branches/main") {
				fmt.Fprintf(w, `{"commit":{"id":%q}}`, h.mainHead)
				return
			}
			// task branches live on the fork only
			_, name, _ := strings.Cut(r.URL.Path, "/branches/")
			if head, ok := h.branches[name]; ok && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") {
				fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
				return
			}
			if h.branchHead == "" || !strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, h.branchHead)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/repo/compare/main...factory-agent:"):
			head := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/repo/compare/main...factory-agent:")
			switch base, ok := h.mergeBases[head]; {
			case !ok:
				http.Error(w, "failed", http.StatusInternalServerError)
			case base == head:
				io.WriteString(w, `{"total_commits":0,"commits":[]}`)
			default:
				fmt.Fprintf(w, `{"total_commits":1,"commits":[{"sha":%q,"parents":[{"sha":%q}]}]}`, head, base)
			}
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/sf/repo/commits" && r.URL.Query().Get("not") == "main":
			// main reaches every merge base: none lies past main
			io.WriteString(w, `[]`)
		case r.Method == "DELETE":
			var id int64
			fmt.Sscanf(r.URL.Path, "/api/v1/users/factory-agent/tokens/%d", &id)
			kept := h.tokens[:0]
			for _, tok := range h.tokens {
				if tok.ID != id {
					kept = append(kept, tok)
				}
			}
			h.tokens = kept
			h.revoked = append(h.revoked, id)
			h.calls = append(h.calls, fmt.Sprintf("revoke %d", id))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && path.Base(path.Dir(r.URL.Path)) == "pulls":
			h.calls = append(h.calls, "pull "+path.Base(r.URL.Path))
			var n int64
			fmt.Sscanf(path.Base(r.URL.Path), "%d", &n)
			body, ok := h.pulls[n]
			switch {
			case h.pullStatus[n] != 0:
				w.WriteHeader(h.pullStatus[n])
			case !ok:
				http.NotFound(w, r)
			default:
				io.WriteString(w, body)
			}
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/files") && h.prFiles != "":
			io.WriteString(w, h.prFiles)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// heldStore opens the store with task t-held filed: a budget of 5.00
// with 4.50 spent. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func heldStore(t *testing.T) *factoryd.PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := factoryd.OpenStore(ctx, schematest.DSN(t, "pg_held"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.PgDB().Close() })
	if _, err := s.PgDB().ExecContext(ctx, `TRUNCATE tasks, holds CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-held", Kind: "agent", Track: "direct", Repository: "sf/r", Status: "running",
		BudgetEUR: 5, SpentEUR: 4.5}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return s
}

// putHold takes a hold of eur on task id, expiring after the interval
// expires (negative: already expired).
func putHold(t *testing.T, s *factoryd.PGStore, id string, eur float64, expires string) {
	t.Helper()
	if _, err := s.PgDB().ExecContext(context.Background(), `INSERT INTO holds (task_id, kind, ref, eur, expires_at)
		VALUES ($1, 'model', 'small', $2, now() + $3::interval)`, id, eur, expires); err != nil {
		t.Fatalf("hold: %v", err)
	}
}

type busyLine struct {
	busy   map[string]bool
	landed map[string][]int64
}

func (l *busyLine) Behind(_, login string, _ int64) (bool, func(...int64)) {
	return l.busy[login], func(ids ...int64) {
		if l.busy[login] {
			l.landed[login] = append(l.landed[login], ids...)
		}
	}
}

func statusBody(state string, statuses ...[4]string) string {
	list := []map[string]string{}
	for _, s := range statuses {
		list = append(list, map[string]string{"status": s[0], "context": s[1], "target_url": s[2], "description": s[3]})
	}
	b, _ := json.Marshal(map[string]any{"state": state, "statuses": list})
	return string(b)
}
