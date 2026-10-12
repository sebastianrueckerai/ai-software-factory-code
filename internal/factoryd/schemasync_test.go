package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// schemaForgejo is a scripted Forgejo for the schema pass: main of
// sf/ai-software-factory-infra, the bot's fork of it with its branches, the
// pull requests and their comments. It answers each credential as Forgejo
// does: factory-config's token reads sf's repository, the bot (a reader of
// it) opens pull requests, closes its own and comments, and owns its
// fork; any other credential sees neither, org sf being private.
type schemaForgejo struct {
	mu       sync.Mutex
	main     map[string]string
	branches map[string]map[string]string // the fork's, by name; nil: no fork
	prs      []*schemaPR
	comments []string
	writes   []string
}

type schemaPR struct {
	number                                     int64
	head, headRepo, title, body, poster, state string
}

const (
	schemaUp   = "/api/v1/repos/sf/ai-software-factory-infra"
	schemaFork = "/api/v1/repos/factory-agent/ai-software-factory-infra"
)

func (s *schemaForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		by := "nobody"
		if u, pw, ok := r.BasicAuth(); ok && u == "factory-agent" && pw == "bot-pw" {
			by = "bot"
		} else if r.Header.Get("Authorization") == "token config-tok" {
			by = "config"
		}
		p, q := r.URL.Path, r.URL.Query()
		if r.Method != http.MethodGet {
			s.writes = append(s.writes, r.Method+" "+p+" by "+by)
		}
		if by == "nobody" || by == "config" && (r.Method != http.MethodGet || !strings.HasPrefix(p, schemaUp+"/")) {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		pr := func() *schemaPR {
			var n int64
			fmt.Sscanf(strings.TrimPrefix(strings.TrimPrefix(p, schemaUp+"/pulls/"), schemaUp+"/issues/"), "%d", &n)
			return s.prs[n-1]
		}
		switch {
		case r.Method == http.MethodGet && p == schemaUp+"/branches/main":
			fmt.Fprint(w, `{"commit":{"id":"c1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, schemaUp+"/raw/") && q.Get("ref") == "c1":
			c, ok := s.main[strings.TrimPrefix(p, schemaUp+"/raw/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, c)
		case r.Method == http.MethodGet && p == schemaUp+"/pulls" && q.Get("state") == "open":
			out := []map[string]any{}
			for _, pr := range s.prs {
				if pr.state == "open" && q.Get("page") == "1" {
					out = append(out, map[string]any{"number": pr.number, "title": pr.title, "state": pr.state,
						"head": map[string]any{"ref": pr.head, "repo": map[string]string{"full_name": pr.headRepo}}})
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && p == schemaUp+"/pulls":
			head, ok := strings.CutPrefix(body["head"].(string), "factory-agent:")
			if !ok || s.branches[head] == nil || body["base"] != "main" {
				t.Errorf("pull request %v", body)
			}
			s.prs = append(s.prs, &schemaPR{number: int64(len(s.prs) + 1), head: head, headRepo: "factory-agent/ai-software-factory-infra",
				title: body["title"].(string), body: body["body"].(string), poster: by, state: "open"})
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d}`, len(s.prs))
		case r.Method == http.MethodPatch && strings.HasPrefix(p, schemaUp+"/pulls/") && body["state"] == "closed":
			if pr().poster != by {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			pr().state = "closed"
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && strings.HasPrefix(p, schemaUp+"/issues/") && strings.HasSuffix(p, "/comments"):
			s.comments = append(s.comments, fmt.Sprintf("#%d by %s: %s", pr().number, by, body["body"]))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && p == schemaFork && s.branches == nil:
			http.NotFound(w, r)
		case r.Method == http.MethodGet && p == schemaFork || r.Method == http.MethodPost && p == schemaUp+"/forks":
			if s.branches == nil {
				s.branches = map[string]map[string]string{"main": maps.Clone(s.main)}
				w.WriteHeader(http.StatusAccepted)
			}
			fmt.Fprint(w, `{"fork":true,"private":true,"parent":{"full_name":"sf/ai-software-factory-infra"}}`)
		case p == schemaFork+"/sync_fork" && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"allowed":true,"commits_behind":%d}`, len(changed(s.main, s.branches["main"])))
		case p == schemaFork+"/sync_fork" && r.Method == http.MethodPost:
			s.branches["main"] = maps.Clone(s.main)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, schemaFork+"/branches/"):
			name := strings.TrimPrefix(p, schemaFork+"/branches/")
			switch {
			case s.branches[name] == nil:
				http.NotFound(w, r)
			case r.Method == http.MethodDelete:
				delete(s.branches, name)
				w.WriteHeader(http.StatusNoContent)
			default:
				fmt.Fprint(w, `{"commit":{"id":"b"}}`)
			}
		case r.Method == http.MethodGet && strings.HasPrefix(p, schemaFork+"/contents/") && s.branches[q.Get("ref")] != nil:
			c, ok := s.branches[q.Get("ref")][strings.TrimPrefix(p, schemaFork+"/contents/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(c)), "encoding": "base64", "sha": blobOf(c)})
		case r.Method == http.MethodPost && p == schemaFork+"/contents" && s.branches[body["branch"].(string)] != nil:
			b := maps.Clone(s.branches[body["branch"].(string)])
			s.branches[body["new_branch"].(string)] = b
			for _, op := range body["files"].([]any) {
				op := op.(map[string]any)
				path, content := op["path"].(string), ""
				if c, err := base64.StdEncoding.DecodeString(op["content"].(string)); err == nil {
					content = string(c)
				}
				if old, exists := b[path]; op["operation"] == "create" == exists || exists && op["sha"] != blobOf(old) {
					t.Errorf("%v of %s with sha %v, exists %v", op["operation"], path, op["sha"], exists)
				}
				b[path] = content
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"commit":{"sha":"c2"}}`)
		default:
			t.Errorf("unexpected %s %s by %s", r.Method, r.URL, by)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// schemaRig is sf/ai-software-factory-infra with main's factory/ holding
// this binary's schemas, changed by change, and the copier of build
// 0123456789ab as factoryd wires it.
func schemaRig(t *testing.T, change func(main map[string]string)) (*schemaForgejo, *SchemaCopier) {
	schemas, err := policy.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	main := map[string]string{"factory/policy.yaml": "version: 1\n"}
	for name, b := range schemas {
		main["factory/schema/"+name] = string(b)
	}
	change(main)
	fj := &schemaForgejo{main: main}
	srv := fj.server(t)
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", ProjectsToken: "projects-tok", HTTP: srv.Client()}
	config := &ForgejoConfig{Forgejo: f, Token: "config-tok", Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Branch: "main", Dir: "factory"}
	return fj, &SchemaCopier{Forgejo: f, Config: config, Build: "0123456789ab"}
}

// branch is the fork's branch name, nil when it has none.
func (s *schemaForgejo) branch(name string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.branches[name])
}

func (s *schemaForgejo) state() ([]schemaPR, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prs []schemaPR
	for _, pr := range s.prs {
		prs = append(prs, *pr)
	}
	return prs, s.comments, s.writes
}

// a stale and a missing copy: one pull request by the bot from its fork's
// factory/schema-0123456789ab, writing exactly those two files whole; the
// next boot opens none, and after a person closes it the boot after cuts
// the branch afresh and opens it again
func TestSchemaCopierOpensOnePullRequest(t *testing.T) {
	fj, c := schemaRig(t, func(main map[string]string) {
		main["factory/schema/policy.schema.json"] = "{}\n"
		delete(main, "factory/schema/process.schema.json")
	})
	ctx := context.Background()
	if err := c.SyncOnce(ctx, 1, 0); err != nil {
		t.Fatal(err)
	}
	schemas, _ := policy.Schemas()
	branch := fj.branch("factory/schema-0123456789ab")
	if got, want := changed(fj.main, branch), []string{"factory/schema/policy.schema.json", "factory/schema/process.schema.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the branch changes %v, want %v", got, want)
	}
	for _, name := range []string{"policy.schema.json", "process.schema.json"} {
		if branch["factory/schema/"+name] != string(schemas[name]) {
			t.Errorf("%s is not written whole", name)
		}
	}
	body := "Main's copies of these schemas differ from what factoryd build 0123456789ab generates; this pull request writes them whole:\n\n" +
		"- `factory/schema/policy.schema.json`: differs\n" +
		"- `factory/schema/process.schema.json`: missing\n\n" +
		"factoryd opens it when it starts and never merges it, an approver does. `factoryd config check` only warns about a stale copy.\n"
	want := []schemaPR{{number: 1, head: "factory/schema-0123456789ab", headRepo: "factory-agent/ai-software-factory-infra",
		title: "Schema copies for 0123456789ab", body: body, poster: "bot", state: "open"}}
	writes := []string{"POST " + schemaUp + "/forks by bot", "POST " + schemaFork + "/contents by bot", "POST " + schemaUp + "/pulls by bot"}
	if prs, comments, got := fj.state(); !reflect.DeepEqual(prs, want) || len(comments) != 0 || !reflect.DeepEqual(got, writes) {
		t.Fatalf("pull requests %+v, comments %q, writes %q;\nwant %+v, none, %q", prs, comments, got, want, writes)
	}

	if err := c.SyncOnce(ctx, 1, 0); err != nil {
		t.Fatal(err)
	}
	if prs, _, got := fj.state(); len(prs) != 1 || !reflect.DeepEqual(got, writes) {
		t.Fatalf("second boot: pull requests %+v, writes %q; want the one, no write", prs, got)
	}

	fj.mu.Lock()
	fj.prs[0].state = "closed"
	fj.branches["factory/schema-0123456789ab"] = maps.Clone(fj.main) // as if main had taken it
	fj.mu.Unlock()
	if err := c.SyncOnce(ctx, 1, 0); err != nil {
		t.Fatal(err)
	}
	want = append(want, want[0])
	want[0].state, want[1].number = "closed", 2
	writes = append(writes, "DELETE "+schemaFork+"/branches/factory/schema-0123456789ab by bot", "POST "+schemaFork+"/contents by bot", "POST "+schemaUp+"/pulls by bot")
	if prs, _, got := fj.state(); !reflect.DeepEqual(prs, want) || !reflect.DeepEqual(got, writes) {
		t.Fatalf("after a close: pull requests %+v, writes %q;\nwant %+v, %q", prs, got, want, writes)
	}
	if got := changed(fj.main, fj.branch("factory/schema-0123456789ab")); !reflect.DeepEqual(got, []string{"factory/schema/policy.schema.json", "factory/schema/process.schema.json"}) {
		t.Fatalf("the cut afresh branch changes %v", got)
	}
}

// an older build's schema pull request is closed once the new one stands,
// with a comment naming it; a person's pull request stays open
func TestSchemaCopierClosesOlderPullRequest(t *testing.T) {
	fj, c := schemaRig(t, func(main map[string]string) { main["factory/schema/policy.schema.json"] = "{}\n" })
	fj.branches = map[string]map[string]string{"main": maps.Clone(fj.main), "factory/schema-aaaaaaaaaaaa": maps.Clone(fj.main)}
	fj.prs = []*schemaPR{
		{number: 1, head: "factory/schema-aaaaaaaaaaaa", headRepo: "factory-agent/ai-software-factory-infra", title: "Schema copies for aaaaaaaaaaaa", poster: "bot", state: "open"},
		{number: 2, head: "factory/schema-mine", headRepo: "sf/ai-software-factory-infra", title: "Schema copies by hand", poster: "sebastian", state: "open"},
	}
	for range 2 {
		if err := c.SyncOnce(context.Background(), 1, 0); err != nil {
			t.Fatal(err)
		}
	}
	prs, comments, writes := fj.state()
	if prs[0].state != "closed" || prs[1].state != "open" || len(prs) != 3 || prs[2].title != "Schema copies for 0123456789ab" || prs[2].state != "open" {
		t.Fatalf("pull requests %+v", prs)
	}
	if want := []string{"#1 by bot: Closed by factoryd: #3 replaces it."}; !reflect.DeepEqual(comments, want) {
		t.Errorf("comments %q, want %q", comments, want)
	}
	if want := []string{"POST " + schemaFork + "/contents by bot", "POST " + schemaUp + "/pulls by bot",
		"PATCH " + schemaUp + "/pulls/1 by bot", "POST " + schemaUp + "/issues/1/comments by bot"}; !reflect.DeepEqual(writes, want) {
		t.Errorf("writes %q, want %q", writes, want)
	}
}

// equal copies open nothing and close the bot's open schema pull request
func TestSchemaCopierEqualCopies(t *testing.T) {
	fj, c := schemaRig(t, func(map[string]string) {})
	fj.prs = []*schemaPR{{number: 1, head: "factory/schema-aaaaaaaaaaaa", headRepo: "factory-agent/ai-software-factory-infra", title: "Schema copies for aaaaaaaaaaaa", poster: "bot", state: "open"}}
	if err := c.SyncOnce(context.Background(), 1, 0); err != nil {
		t.Fatal(err)
	}
	prs, comments, writes := fj.state()
	if len(prs) != 1 || prs[0].state != "closed" || fj.branch("main") != nil {
		t.Fatalf("pull requests %+v; want #1 closed, and no fork made", prs)
	}
	if want := []string{"#1 by bot: Closed by factoryd: the copies on main equal the schemas of factoryd 0123456789ab."}; !reflect.DeepEqual(comments, want) {
		t.Errorf("comments %q, want %q", comments, want)
	}
	if want := []string{"PATCH " + schemaUp + "/pulls/1 by bot", "POST " + schemaUp + "/issues/1/comments by bot"}; !reflect.DeepEqual(writes, want) {
		t.Errorf("writes %q, want %q", writes, want)
	}
}

// a credential without access to sf's repository, as the projects token
// has none, files nothing
func TestSchemaCopierWrongCredential(t *testing.T) {
	fj, c := schemaRig(t, func(main map[string]string) { main["factory/schema/policy.schema.json"] = "{}\n" })
	c.Forgejo = c.Forgejo.As("factory-projects", "projects-tok")
	if err := c.SyncOnce(context.Background(), 1, 0); err == nil {
		t.Fatal("the projects token filed the schema copies")
	}
	if prs, _, _ := fj.state(); len(prs) != 0 {
		t.Fatalf("pull requests %+v, want none", prs)
	}
}
