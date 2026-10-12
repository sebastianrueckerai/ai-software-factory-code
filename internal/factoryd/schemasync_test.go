package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// schemaForgejo is a scripted Forgejo for the schema pass: sf/infra with
// its main, factory/schema-* branches, pull requests and comments. Every
// write records its call and the credential that made it.
type schemaForgejo struct {
	mu        sync.Mutex
	repo      bool // the projects token sees sf/infra at all
	blind     bool // no credential reaches sf/infra: nothing is read
	main      map[string]string
	branches  map[string]map[string]string
	prs       []*schemaPR
	writes    []string
	comments  []string
	filesRead int
}

type schemaPR struct {
	number  int64
	title   string
	head    string
	state   string
	body    string
	closed  int
	created int
}

func schemaBlob(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))[:12] }

func (s *schemaForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	const repo = "/api/v1/repos/sf/infra"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		by := r.Header.Get("Authorization")
		if u, _, ok := r.BasicAuth(); ok {
			by = "basic " + u
		}
		p, q := r.URL.Path, r.URL.Query()
		if raw := strings.TrimPrefix(r.RequestURI, repo); strings.Contains(raw, "?") {
			p = repo + raw[:strings.Index(raw, "?")]
		}
		write := func() { s.writes = append(s.writes, r.Method+" "+p+" by "+by) }
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		pr := func(n int64) *schemaPR {
			for _, e := range s.prs {
				if e.number == n {
					return e
				}
			}
			return nil
		}
		number := func() int64 {
			var v int64
			fmt.Sscanf(path.Base(strings.TrimSuffix(p, "/comments")), "%d", &v)
			return v
		}
		switch {
		case !s.repo && strings.HasPrefix(p, repo):
			http.NotFound(w, r)
		case s.blind && strings.HasPrefix(p, repo):
			http.NotFound(w, r)
		case r.Method == http.MethodGet && p == repo:
			json.NewEncoder(w).Encode(map[string]any{"empty": false})
		case r.Method == http.MethodGet && p == repo+"/pulls" && q.Get("state") == "open":
			out := []map[string]any{}
			if q.Get("page") != "1" {
				json.NewEncoder(w).Encode(out) // past the rig's single page
				return
			}
			for _, e := range slices.SortedFunc(slices.Values(s.prs), func(a, b *schemaPR) int { return a.created - b.created }) {
				if e.state != "open" {
					continue
				}
				out = append(out, map[string]any{"number": e.number, "title": e.title, "state": e.state, "body": e.body,
					"head": map[string]any{"ref": e.head, "repo": map[string]string{"full_name": "sf/infra"}},
					"base": map[string]string{"ref": "main"}})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && p == repo+"/pulls":
			base, _ := strings.CutSuffix(body["base"].(string), "")
			if base != "main" {
				t.Errorf("pull request into %q, want main", base)
			}
			if s.branches[body["head"].(string)] == nil {
				t.Errorf("pull request from branch %v, which does not exist", body["head"])
			}
			e := &schemaPR{number: int64(len(s.prs) + 1), title: body["title"].(string), head: body["head"].(string),
				state: "open", body: body["body"].(string), created: len(s.prs) + 1}
			s.prs = append(s.prs, e)
			write()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"html_url":"u"}`, e.number)
		case r.Method == http.MethodPatch && strings.HasPrefix(p, repo+"/pulls/"):
			e := pr(number())
			if e == nil {
				http.NotFound(w, r)
				return
			}
			if body["state"] == "closed" && e.state == "open" {
				e.state = "closed"
				e.closed = len(s.prs)
			}
			write()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodPost && strings.HasPrefix(p, repo+"/issues/") && strings.HasSuffix(p, "/comments"):
			e := pr(number())
			if e == nil {
				http.NotFound(w, r)
				return
			}
			s.comments = append(s.comments, fmt.Sprintf("#%d by %s: %s", e.number, by, body["body"]))
			write()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]int{"id": 1})
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/git/trees/"):
			ref := strings.TrimPrefix(p, repo+"/git/trees/")
			files, known := s.main, ref == "deadbeef" || ref == "tree-factory" || ref == "sub-schema"
			if b, ok := s.branches[ref]; ok {
				files, known = b, ref != "sub-schema"
			}
			if !known {
				http.NotFound(w, r)
				return
			}
			recursive := q.Get("recursive") == "true"
			out := []map[string]any{}
			emit := func(name, content string) {
				out = append(out, map[string]any{"path": name, "type": "blob", "mode": "100644", "sha": schemaBlob(content)})
			}
			addTree := func(name, sha string) {
				out = append(out, map[string]any{"path": name, "type": "tree", "mode": "040000", "sha": sha})
			}
			switch {
			case ref == "deadbeef": // the commit's top level
				addTree("factory", "tree-factory")
				addTree("k8s", "tree-k8s")
			case ref == "tree-factory": // factory/, flat or recursive
				if recursive { // git lists a subtree's files under its name
					for _, name := range slices.Sorted(maps.Keys(files)) {
						rel, ok := strings.CutPrefix(name, "factory/"+policy.DirSchema+"/")
						if ok {
							emit(policy.DirSchema+"/"+rel, files[name])
						}
					}
				} else {
					addTree(policy.DirSchema, "sub-schema")
				}
				emit("policy.yaml", files["factory/policy.yaml"])
			case ref == "sub-schema": // schema/'s entries: main's copies
				for _, name := range slices.Sorted(maps.Keys(files)) {
					rel, ok := strings.CutPrefix(name, "factory/"+policy.DirSchema+"/")
					if ok {
						emit(policy.DirSchema+"/"+rel, files[name])
					}
				}
			case recursive && strings.HasSuffix(ref, "/"+policy.DirSchema): // a branch's schema/
				prefix := strings.TrimSuffix(ref, "/"+policy.DirSchema) + "/"
				for _, name := range slices.Sorted(maps.Keys(files)) {
					rel, ok := strings.CutPrefix(name, prefix+policy.DirSchema+"/")
					if ok {
						emit(policy.DirSchema+"/"+rel, files[name])
					}
				}
			default:
				t.Errorf("tree %s of the scripted Forgejo", ref)
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(out), "truncated": false, "tree": out})
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/raw/"):
			fileAt(w, r, s, strings.TrimPrefix(p, repo+"/raw/"))
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/contents/"):
			name := strings.TrimPrefix(p, repo+"/contents/")
			files := s.main
			if b, ok := s.branches[q.Get("ref")]; ok {
				files = b
			}
			content, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)),
				"encoding": "base64", "sha": schemaBlob(content)})
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/branches/"):
			name := strings.TrimPrefix(p, repo+"/branches/")
			if name == "main" {
				json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": "deadbeef"}})
				return
			}
			if s.branches[name] == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": "c"}})
		case r.Method == http.MethodDelete && strings.HasPrefix(p, repo+"/branches/"):
			name := strings.TrimPrefix(p, repo+"/branches/")
			if name == "main" {
				t.Errorf("delete of main")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if s.branches[name] == nil {
				http.NotFound(w, r)
				return
			}
			delete(s.branches, name)
			write()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && p == repo+"/branches":
			to, _ := body["new_branch_name"].(string)
			from, _ := body["old_branch_name"].(string)
			if s.branches[to] != nil {
				w.WriteHeader(http.StatusConflict) // Forgejo keeps an existing branch
				return
			}
			src, ok := map[string]map[string]string{"main": s.main}[from]
			if !ok {
				http.NotFound(w, r)
				return
			}
			cut := map[string]string{}
			for name, content := range src {
				if !strings.Contains(name, "/"+policy.DirSchema+"/") {
					cut[name] = content
				}
			}
			s.branches[to] = cut
			write()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": "c"}})
		case r.Method == http.MethodPost && p == repo+"/contents":
			branch, _ := body["branch"].(string)
			newBranch, _ := body["new_branch"].(string)
			if newBranch != "" {
				branch = newBranch
			}
			b, ok := s.branches[branch]
			if !ok {
				t.Errorf("commit onto %s, a branch the rig does not hold (new_branch %q)", branch, newBranch)
				w.WriteHeader(http.StatusConflict)
				return
			}
			for _, op := range body["files"].([]any) {
				o := op.(map[string]any)
				content, _ := base64.StdEncoding.DecodeString(o["content"].(string))
				have, exists := b[o["path"].(string)]
				switch o["operation"] {
				case "create":
					if exists {
						t.Errorf("create over an existing %s", o["path"])
					}
				case "update":
					if !exists || o["sha"] != schemaBlob(have) {
						t.Errorf("update of %s with sha %v, holds %v", o["path"], o["sha"], exists)
					}
				default:
					t.Errorf("operation %v", o["operation"])
				}
				b[o["path"].(string)] = string(content)
			}
			write()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c"}})
		default:
			t.Errorf("unexpected %s %s by %s", r.Method, r.URL, by)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fileAt answers raw/<path>: main's file, or the branch ref's.
func fileAt(w http.ResponseWriter, r *http.Request, s *schemaForgejo, name string) {
	files := s.main
	if b, ok := s.branches[r.URL.Query().Get("ref")]; ok {
		files = b
	}
	content, ok := files[path.Clean(name)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write([]byte(content)) // #nosec G104 -- the scripted answer
}

// schemaRig is sf/infra on a scripted Forgejo with main holding copies,
// and the copier reading it as factoryd does at boot.
func schemaRig(t *testing.T, build string, copies map[string]string) (*schemaForgejo, *SchemaCopier) {
	t.Helper()
	s := &schemaForgejo{repo: true, main: maps.Clone(copies), branches: map[string]map[string]string{}}
	srv := s.server(t)
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", ProjectsToken: "projects-tok", HTTP: srv.Client()}
	cfg := &ForgejoConfig{Forgejo: f, Token: "config-tok", Repo: Repo{Owner: "sf", Name: "infra"}, Branch: "main", Dir: "factory"}
	c := &SchemaCopier{Forgejo: f, Token: f.ProjectsToken, Repo: cfg.Repo, Config: cfg,
		Head: func() string { return "deadbeef" }, Build: func() string { return build }}
	return s, c
}

// generatedSchemas are this binary's schemas as main would hold them.
func generatedSchemas(t *testing.T) map[string]string {
	t.Helper()
	want, err := policy.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, b := range want {
		out["factory/"+policy.DirSchema+"/"+name] = string(b)
	}
	return out
}

// a stale copy opens exactly one pull request, titled for the build, from
// factory/schema-<id>, carrying only the changed files whole.
func TestSchemaCopierOpensOnePullRequest(t *testing.T) {
	build := "0123456789abcdef0123456789abcdef"
	copies := generatedSchemas(t)
	stale := "policy.schema.json"
	copies["factory/"+policy.DirSchema+"/"+stale] = "{}\n"
	s, c := schemaRig(t, build, copies)

	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	wantID := shortID(build)
	if len(s.prs) != 1 {
		t.Fatalf("%d pull requests (%+v), want one", len(s.prs), s.prs)
	}
	pr := s.prs[0]
	if pr.title != "Schema copies for "+wantID {
		t.Errorf("title = %q, want %q", pr.title, "Schema copies for "+wantID)
	}
	if pr.head != SchemaBranchPrefix+wantID {
		t.Errorf("branch = %q, want %q", pr.head, SchemaBranchPrefix+wantID)
	}
	wrote := s.branches[pr.head]
	if len(wrote) != 1 {
		t.Fatalf("the branch holds %v, want the one changed file", slices.Sorted(maps.Keys(wrote)))
	}
	want, err := policy.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if got := wrote["factory/schema/"+stale]; got != string(want[stale]) {
		t.Errorf("the branch's copy is not what this binary generates")
	}
	for _, line := range []string{"- `schema/" + stale + "` (differs)"} {
		if !strings.Contains(pr.body, line) {
			t.Errorf("body lacks %q:\n%s", line, pr.body)
		}
	}
	// the writes carry the projects token, and nothing else
	for _, w := range s.writes {
		if !strings.HasSuffix(w, " by token projects-tok") {
			t.Errorf("write %q, want it by the projects token", w)
		}
	}
	// a second start with the pull request open opens none
	before := len(s.prs)
	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	if len(s.prs) != before {
		t.Fatalf("%d pull requests after a second start, want %d", len(s.prs), before)
	}
}

// equal copies open nothing.
func TestSchemaCopierEqualCopiesOpenNothing(t *testing.T) {
	s, c := schemaRig(t, "aaaaaaaaaaaa", generatedSchemas(t))
	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(s.prs) != 0 || len(s.writes) != 0 {
		t.Fatalf("prs %+v, writes %v, want none", s.prs, s.writes)
	}
}

// a missing copy is written too, and the body says so.
func TestSchemaCopierWritesMissingCopy(t *testing.T) {
	copies := generatedSchemas(t)
	delete(copies, "factory/schema/process.schema.json")
	s, c := schemaRig(t, "bbbbbbbbbbbb", copies)
	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(s.prs) != 1 {
		t.Fatalf("%d pull requests, want one", len(s.prs))
	}
	wrote := s.branches[s.prs[0].head]
	if _, ok := wrote["factory/schema/process.schema.json"]; !ok {
		t.Fatalf("the branch lacks the missing file: %v", slices.Sorted(maps.Keys(wrote)))
	}
	if !strings.Contains(s.prs[0].body, "- `schema/process.schema.json` (missing)") {
		t.Errorf("body:\n%s", s.prs[0].body)
	}
}

// an older build's open schema pull request is closed with a comment
// naming the new one, and the new one is the only open request.
func TestSchemaCopierClosesOlderPullRequest(t *testing.T) {
	copies := generatedSchemas(t)
	copies["factory/schema/project.schema.json"] = "{\"old\": true}\n"
	s, c := schemaRig(t, "cccccccccccc", copies)
	old := SchemaBranchPrefix + "dddddddddddd"
	s.branches[old] = map[string]string{"factory/schema/project.schema.json": "{\"old\": true}\n"}
	s.prs = []*schemaPR{{number: 7, title: "Schema copies for dddddddddddd", head: old, state: "open",
		body: "- `schema/project.schema.json` (differs)\n", created: 1}}

	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if s.prs[0].state != "closed" {
		t.Fatalf("the older pull request is %s, want closed", s.prs[0].state)
	}
	if len(s.comments) != 1 {
		t.Fatalf("comments %v, want one", s.comments)
	}
	if want := "#7 by basic factory-agent"; !strings.HasPrefix(s.comments[0], want) {
		t.Errorf("comment %q, wants its author", s.comments[0])
	}
	if !strings.Contains(s.comments[0], "Schema copies for cccccccccccc") {
		t.Errorf("comment names no new request:\n%s", s.comments[0])
	}
	var open []*schemaPR
	for _, e := range s.prs {
		if e.state == "open" {
			open = append(open, e)
		}
	}
	if len(open) != 1 || open[0].title != "Schema copies for cccccccccccc" {
		t.Fatalf("open pull requests %+v, want exactly the new one", open)
	}
}

// a build that does not know its commit names its pull request by the hash
// of the generated content.
func TestSchemaCopierBuildIsContentHash(t *testing.T) {
	want, err := policy.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	id := policy.SchemaContentHash(want)
	copies := generatedSchemas(t)
	copies["factory/schema/chat.schema.json"] = ""
	s, c := schemaRig(t, id, copies)
	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(s.prs) != 1 || s.prs[0].title != "Schema copies for "+id || s.prs[0].head != SchemaBranchPrefix+id {
		t.Fatalf("pull requests %+v, want one titled and branched for %s", s.prs, id)
	}
}

// where the projects token reaches no repository, factoryd says so and
// files nothing.
func TestSchemaCopierUnreachableRepository(t *testing.T) {
	s, c := schemaRig(t, "eeeeeeeeeeee", generatedSchemas(t))
	s.repo = false
	if err := c.SyncOnce(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(s.prs) != 0 || len(s.writes) != 0 {
		t.Fatalf("prs %+v writes %v, want none", s.prs, s.writes)
	}
}
