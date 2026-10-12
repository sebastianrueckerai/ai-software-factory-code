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
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// templateForgejo is a scripted Forgejo for the template pass: main of
// demo-app/demo-app, factory-projects' fork of it with its branches, the
// pull requests and their comments. Every write records its call and
// credential, every contents call the paths it wrote.
type templateForgejo struct {
	mu       sync.Mutex
	main     map[string]string
	branches map[string]map[string]string // the fork's, by name; nil: no fork
	base     map[string]string            // main's files the branch last took
	prs      []*templatePR
	closes   int
	comments []string
	written  []string
	writes   []string
}

type templatePR struct {
	number            int64
	head, state, body string
	merged            bool
	closed            int // its place in the order of closes
}

// closePR closes pull request n as a person on Forgejo does, merged or not.
func (s *templateForgejo) closePR(n int64, merged bool) {
	s.closes++
	s.prs[n-1].state, s.prs[n-1].merged, s.prs[n-1].closed = "closed", merged, s.closes
}

// conflicts reports whether main and branch b changed a path differently
// since the branch last took main.
func (s *templateForgejo) conflicts(b map[string]string) bool {
	theirs := changed(s.base, b)
	return slices.ContainsFunc(changed(s.base, s.main), func(f string) bool { return slices.Contains(theirs, f) && s.main[f] != b[f] })
}

func blobOf(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))[:12] }

// changed are the paths whose content differs between a and b.
func changed(a, b map[string]string) []string {
	var out []string
	for _, p := range slices.Compact(slices.Sorted(slices.Values(slices.Concat(slices.Collect(maps.Keys(a)), slices.Collect(maps.Keys(b)))))) {
		va, ina := a[p]
		vb, inb := b[p]
		if va != vb || ina != inb {
			out = append(out, p)
		}
	}
	return out
}

func (s *templateForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		by := r.Header.Get("Authorization")
		if u, _, ok := r.BasicAuth(); ok {
			by = "basic " + u
		}
		if r.Method != http.MethodGet {
			s.writes = append(s.writes, r.Method+" "+r.URL.Path+" by "+by)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		const up, fk = "/api/v1/repos/demo-app/demo-app", "/api/v1/repos/factory-projects/demo-app"
		p, q := r.URL.Path, r.URL.Query()
		branch := s.branches[q.Get("ref")]
		pr := func() *templatePR {
			var n int64
			fmt.Sscanf(strings.TrimPrefix(p, up+"/pulls/"), "%d", &n)
			fmt.Sscanf(strings.TrimPrefix(p, up+"/issues/"), "%d", &n)
			return s.prs[n-1]
		}
		file := func(files map[string]string, path string) {
			c, ok := files[path]
			dir := slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(f string) bool { return strings.HasPrefix(f, path+"/") })
			switch {
			case dir:
				json.NewEncoder(w).Encode([]any{})
			case !ok:
				http.NotFound(w, r)
			default:
				json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(c)), "encoding": "base64", "sha": blobOf(c)})
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, up+"/contents/") && q.Get("ref") == "main":
			file(s.main, strings.TrimPrefix(p, up+"/contents/"))
		case r.Method == http.MethodGet && strings.HasPrefix(p, fk+"/contents/") && branch != nil:
			file(branch, strings.TrimPrefix(p, fk+"/contents/"))
		case r.Method == http.MethodGet && p == up+"/pulls" &&
			(q.Get("state") == "open" || q.Get("state") == "closed" && q.Get("sort") == "recentclose" && q.Get("poster") == "factory-projects"):
			out := []map[string]any{}
			for _, pr := range slices.SortedFunc(slices.Values(s.prs), func(a, b *templatePR) int { return b.closed - a.closed }) {
				if pr.state == q.Get("state") && q.Get("page") == "1" {
					out = append(out, map[string]any{"number": pr.number, "state": pr.state, "body": pr.body, "merged": pr.merged,
						"mergeable": !s.conflicts(s.branches[pr.head]),
						"head":      map[string]any{"ref": pr.head, "repo": map[string]string{"full_name": "factory-projects/demo-app"}}})
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && p == up+"/pulls":
			head, _ := strings.CutPrefix(body["head"].(string), "factory-projects:")
			if body["base"] != "main" || s.branches[head] == nil || body["title"] != "Template update" {
				t.Errorf("pull request %v", body)
			}
			s.prs = append(s.prs, &templatePR{number: int64(len(s.prs) + 1), head: head, state: "open", body: body["body"].(string)})
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d}`, len(s.prs))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/files"):
			out := []map[string]string{}
			for _, f := range changed(s.base, s.branches[pr().head]) {
				if q.Get("page") == "1" {
					out = append(out, map[string]string{"filename": f})
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/update") && q.Get("style") == "merge":
			b := s.branches[pr().head]
			if s.conflicts(b) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			for _, f := range changed(s.base, s.main) {
				if c, ok := s.main[f]; ok {
					b[f] = c
				} else {
					delete(b, f)
				}
			}
			s.base = maps.Clone(s.main)
		case r.Method == http.MethodPatch && strings.HasPrefix(p, up+"/pulls/"):
			if v, ok := body["body"].(string); ok {
				pr().body = v
			}
			if body["state"] == "closed" && pr().state == "open" {
				s.closePR(pr().number, false)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodPost && strings.HasPrefix(p, up+"/issues/") && strings.HasSuffix(p, "/comments"):
			s.comments = append(s.comments, fmt.Sprintf("#%d by %s: %s", pr().number, by, body["body"]))
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]int{"id": 1})
		case r.Method == http.MethodGet && p == fk && s.branches == nil:
			http.NotFound(w, r)
		case r.Method == http.MethodGet && p == fk || r.Method == http.MethodPost && p == up+"/forks":
			if s.branches == nil {
				s.branches = map[string]map[string]string{"main": maps.Clone(s.main)}
				w.WriteHeader(http.StatusAccepted)
			}
			json.NewEncoder(w).Encode(map[string]any{"fork": true, "private": true, "parent": map[string]string{"full_name": "demo-app/demo-app"}})
		case p == fk+"/sync_fork" && r.Method == http.MethodGet:
			behind := 0
			if len(changed(s.main, s.branches["main"])) > 0 {
				behind = 1
			}
			fmt.Fprintf(w, `{"allowed":true,"commits_behind":%d}`, behind)
		case p == fk+"/sync_fork" && r.Method == http.MethodPost:
			s.branches["main"] = maps.Clone(s.main)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, fk+"/branches/"):
			name := strings.TrimPrefix(p, fk+"/branches/")
			if s.branches[name] == nil {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodDelete {
				delete(s.branches, name)
				for _, pr := range s.prs {
					if pr.head == name && pr.state == "open" {
						s.closePR(pr.number, false)
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": "b"}})
		case r.Method == http.MethodPost && p == fk+"/contents":
			from, _ := body["branch"].(string)
			b := s.branches[from]
			if nb, _ := body["new_branch"].(string); nb != "" {
				b = maps.Clone(b)
				s.branches[nb], s.base = b, maps.Clone(s.main)
			}
			for _, op := range body["files"].([]any) {
				op := op.(map[string]any)
				path, content := op["path"].(string), ""
				if c, err := base64.StdEncoding.DecodeString(op["content"].(string)); err == nil {
					content = string(c)
				}
				old, exists := b[path]
				if create := op["operation"] == "create"; create == exists || !create && op["sha"] != blobOf(old) {
					t.Errorf("%v of %s with sha %v, exists %v", op["operation"], path, op["sha"], exists)
				}
				if b[path] = content; op["operation"] == "delete" {
					delete(b, path)
				}
				if m, ok := s.main[path]; !strings.HasPrefix(path, ".forgejo/") && !strings.HasPrefix(path, "deploy/") && (m != b[path] || ok != (op["operation"] != "delete")) {
					t.Errorf("the pass wrote %s other than main has it", path)
				}
				s.written = append(s.written, path)
			}
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

// templateRig is demo-app, ready, on template ts-go, with main holding
// main's files, and a Registrar on the scripted Forgejo. A path outside
// .forgejo/ and deploy/ is only ever written back to main's.
func templateRig(t *testing.T, main map[string]string) (*templateForgejo, *Registrar, *memProjects) {
	fj := &templateForgejo{main: main}
	srv := fj.server(t)
	p := demoProject()
	p.Template, p.ReadyAt = "ts-go", time.Now()
	p.TemplateFiles = map[string]string{
		".forgejo/workflows/ci.yml":         "image: [[.Image]]\nsmoke: [[.Name]]-smoke\n",
		".forgejo/workflows/pr.yml":         "name: pr\n",
		"deploy/smoke.sh":                   "curl [[.Name]]\n",
		"deploy/base/kustomization.yaml":    "resources: []\n",
		"deploy/staging/kustomization.yaml": "namespace: [[index .Namespaces \"deploy/staging\"]]\n",
		"AGENTS.md":                         "# [[.Name]]\n",
		"cmd/server/main.go":                "package main\n",
	}
	store := &memProjects{m: map[string]Project{"demo-app": p}}
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", ProjectsToken: "projects-tok", HTTP: srv.Client()}
	reg := &Registrar{Forgejo: f, Store: store, RegistryHost: "git.aisoftwarefactory.world", ProjectsUser: "factory-projects"}
	return fj, reg, store
}

// the rendered files of templateRig's template
const (
	renderedCI    = "image: git.aisoftwarefactory.world/demo-app/demo-app\nsmoke: demo-app-smoke\n"
	renderedSmoke = "curl demo-app\n"
)

// a main seeded in Stage 15: its ci.yml has no smoke job, deploy/ no
// smoke.sh; its AGENTS.md and code are its own
func stage15Main() map[string]string {
	return map[string]string{
		".forgejo/workflows/ci.yml": "image: git.aisoftwarefactory.world/demo-app/demo-app\n", ".forgejo/workflows/pr.yml": "name: pr\n",
		"deploy/base/kustomization.yaml": "resources: []\n", "deploy/staging/kustomization.yaml": "namespace: demo-staging\n",
		"AGENTS.md": "# demo-app, our own words\n", "cmd/server/main.go": "package main // ours\n",
	}
}

// the pull request's change: the paths whose branch content differs from
// main's, with the branch's content
func (s *templateForgejo) proposed(branch string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, p := range changed(s.main, s.branches[branch]) {
		out[p] = s.branches[branch][p]
	}
	return out
}

func templateBody(lines ...string) string {
	return "Template ts-go, rendered for this project as registration seeds it, differs from main in these files:\n\n" + strings.Join(lines, "") +
		"\nfactoryd keeps this pull request to the template every hour and closes it once main equals it. It is no card: factoryd files no task for it and never merges it, an approver does.\n"
}

// A main without the smoke job gets one pull request from factory-projects'
// fork with exactly ci.yml and deploy/smoke.sh, by factory-projects; its
// own AGENTS.md and code stay out.
func TestTemplateUpdateOpensOnePullRequest(t *testing.T) {
	fj, reg, _ := templateRig(t, stage15Main())
	logs := captureLog(t)
	reg.TemplateUpdates(context.Background())
	want := map[string]string{".forgejo/workflows/ci.yml": renderedCI, "deploy/smoke.sh": renderedSmoke}
	if got := fj.proposed(TemplateUpdateBranch); !reflect.DeepEqual(got, want) {
		t.Errorf("proposed %q\nwant %q", got, want)
	}
	if !slices.Equal(fj.written, []string{".forgejo/workflows/ci.yml", "deploy/smoke.sh"}) {
		t.Errorf("written %v", fj.written)
	}
	if len(fj.prs) != 1 || fj.prs[0].head != TemplateUpdateBranch || fj.prs[0].state != "open" ||
		fj.prs[0].body != templateBody("- `.forgejo/workflows/ci.yml`: differs\n", "- `deploy/smoke.sh`: missing\n") {
		t.Errorf("pull requests %+v", fj.prs)
	}
	for _, w := range fj.writes {
		if !strings.HasSuffix(w, " by token projects-tok") {
			t.Errorf("write not by factory-projects: %s", w)
		}
	}
	if !strings.Contains(logs.String(), "project demo-app: template update: created pull request #1: .forgejo/workflows/ci.yml, deploy/smoke.sh\n") {
		t.Errorf("log:\n%s", logs)
	}
}

// A main without deploy/ gets the template's whole deploy/.
func TestTemplateUpdateAddsMissingDeploy(t *testing.T) {
	fj, reg, _ := templateRig(t, map[string]string{".forgejo/workflows/ci.yml": renderedCI, ".forgejo/workflows/pr.yml": "name: pr\n", "main.go": "package main\n"})
	reg.TemplateUpdates(context.Background())
	want := map[string]string{"deploy/smoke.sh": renderedSmoke, "deploy/base/kustomization.yaml": "resources: []\n", "deploy/staging/kustomization.yaml": "namespace: demo-staging\n"}
	if got := fj.proposed(TemplateUpdateBranch); !reflect.DeepEqual(got, want) {
		t.Errorf("proposed %q\nwant %q", got, want)
	}
	if len(fj.prs) != 1 || fj.prs[0].body != templateBody("- `deploy/base/kustomization.yaml`: deploy/ missing\n",
		"- `deploy/smoke.sh`: deploy/ missing\n", "- `deploy/staging/kustomization.yaml`: deploy/ missing\n") {
		t.Errorf("pull requests %+v", fj.prs)
	}
}

// A main equal to the render gets nothing: no fork, no write. Its own
// deploy/smoke.sh, which the card that owns deploy/ filled, stays its own.
func TestTemplateUpdateLeavesEqualMain(t *testing.T) {
	main := stage15Main()
	main[".forgejo/workflows/ci.yml"], main["deploy/smoke.sh"] = renderedCI, "curl demo-app && authenticated_write\n"
	fj, reg, _ := templateRig(t, main)
	logs := captureLog(t)
	reg.TemplateUpdates(context.Background())
	if len(fj.writes) != 0 || fj.branches != nil {
		t.Errorf("writes %v, fork %v", fj.writes, fj.branches)
	}
	if !strings.Contains(logs.String(), "project demo-app: template update: no change\n") {
		t.Errorf("log:\n%s", logs)
	}
}

// A second pass with the pull request open after the template changed and
// main moved merges main in and brings the branch to the new render: ci.yml
// back to main's, smoke.sh the new one, and what someone else put on the
// branch back to main's; no second pull request. A third pass writes
// nothing.
func TestTemplateUpdateKeepsItsPullRequest(t *testing.T) {
	fj, reg, store := templateRig(t, stage15Main())
	ctx := context.Background()
	reg.TemplateUpdates(ctx)
	p := store.m["demo-app"]
	p.TemplateFiles = maps.Clone(p.TemplateFiles)
	p.TemplateFiles[".forgejo/workflows/ci.yml"], p.TemplateFiles["deploy/smoke.sh"] = "image: [[.Image]]\n", "curl -f [[.Name]]\n"
	store.m["demo-app"] = p
	fj.main["cmd/server/main.go"] = "package main // ours, later\n"
	fj.branches[TemplateUpdateBranch]["README.md"], fj.branches[TemplateUpdateBranch]["AGENTS.md"] = "planted\n", "# planted\n"
	logs := captureLog(t)
	reg.TemplateUpdates(ctx)
	if got, want := fj.proposed(TemplateUpdateBranch), map[string]string{"deploy/smoke.sh": "curl -f demo-app\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("proposed %q\nwant %q", got, want)
	}
	if len(fj.prs) != 1 || fj.prs[0].state != "open" || fj.prs[0].body != templateBody("- `deploy/smoke.sh`: missing\n") {
		t.Errorf("pull requests %+v", fj.prs)
	}
	if !strings.Contains(logs.String(), "project demo-app: template update: updated pull request #1: .forgejo/workflows/ci.yml, AGENTS.md, deploy/smoke.sh, README.md\n") {
		t.Errorf("log:\n%s", logs)
	}
	writes := len(fj.writes)
	reg.TemplateUpdates(ctx)
	if len(fj.writes) != writes || len(fj.prs) != 1 {
		t.Errorf("a pass with nothing to change wrote %v", fj.writes[writes:])
	}
}

// Files on main that now equal the render close the open pull request:
// its branch goes, then the close, then the bot's comment, so a failed
// close repeats no comment.
func TestTemplateUpdateClosesWhenMainEquals(t *testing.T) {
	fj, reg, _ := templateRig(t, stage15Main())
	ctx := context.Background()
	reg.TemplateUpdates(ctx)
	fj.main[".forgejo/workflows/ci.yml"] = renderedCI
	fj.main["deploy/smoke.sh"] = renderedSmoke
	writes, logs := len(fj.writes), captureLog(t)
	reg.TemplateUpdates(ctx)
	if want := []string{"DELETE /api/v1/repos/factory-projects/demo-app/branches/factory/template-update by token projects-tok",
		"PATCH /api/v1/repos/demo-app/demo-app/pulls/1 by token projects-tok", "POST /api/v1/repos/demo-app/demo-app/issues/1/comments by basic factory-agent"}; !slices.Equal(fj.writes[writes:], want) {
		t.Errorf("writes %q\nwant %q", fj.writes[writes:], want)
	}
	if fj.prs[0].state != "closed" || fj.branches[TemplateUpdateBranch] != nil {
		t.Errorf("pull request %+v, branch %v", fj.prs[0], fj.branches[TemplateUpdateBranch])
	}
	if want := []string{"#1 by basic factory-agent: factoryd: closing this pull request: main's files equal the template's."}; !slices.Equal(fj.comments, want) {
		t.Errorf("comments %q", fj.comments)
	}
	if !strings.Contains(logs.String(), "project demo-app: template update: closed pull request #1: main's files equal the template's\n") {
		t.Errorf("log:\n%s", logs)
	}
}

// A main that changes a file the branch changes, the template unchanged,
// conflicts with it: the pass closes the pull request, and the next pass
// opens a new one from main. A branch a merged pull request left is
// deleted, and the pass after opens the new pull request.
func TestTemplateUpdateStartsAnew(t *testing.T) {
	fj, reg, store := templateRig(t, stage15Main())
	ctx := context.Background()
	reg.TemplateUpdates(ctx)
	fj.main[".forgejo/workflows/ci.yml"] = "image: by hand\n"
	reg.TemplateUpdates(ctx)
	if want := []string{"#1 by basic factory-agent: factoryd: closing this pull request: main conflicts with its branch; the next pass opens it anew from main."}; fj.prs[0].state != "closed" || len(fj.prs) != 1 || fj.branches[TemplateUpdateBranch] != nil || !slices.Equal(fj.comments, want) {
		t.Fatalf("pull requests %+v, comments %q", fj.prs, fj.comments)
	}
	reg.TemplateUpdates(ctx)
	if got, want := fj.proposed(TemplateUpdateBranch), map[string]string{".forgejo/workflows/ci.yml": renderedCI, "deploy/smoke.sh": renderedSmoke}; len(fj.prs) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("pull requests %+v, proposed %q", fj.prs, got)
	}
	// an approver merges #2; Forgejo keeps its branch
	for path, c := range fj.proposed(TemplateUpdateBranch) {
		fj.main[path] = c
	}
	fj.closePR(2, true)
	p := store.m["demo-app"]
	p.TemplateFiles = maps.Clone(p.TemplateFiles)
	p.TemplateFiles[".forgejo/workflows/ci.yml"] = "image: [[.Image]]\n"
	store.m["demo-app"] = p
	logs := captureLog(t)
	reg.TemplateUpdates(ctx)
	if len(fj.prs) != 2 || fj.branches[TemplateUpdateBranch] != nil ||
		!strings.Contains(logs.String(), "template update: waiting: deleted the branch factory-projects/demo-app:factory/template-update a closed pull request left; the next pass opens a new one\n") {
		t.Fatalf("pull requests %+v, branch %v\n%s", fj.prs, fj.branches[TemplateUpdateBranch], logs)
	}
	reg.TemplateUpdates(ctx)
	if got, want := fj.proposed(TemplateUpdateBranch), map[string]string{".forgejo/workflows/ci.yml": "image: git.aisoftwarefactory.world/demo-app/demo-app\n"}; len(fj.prs) != 3 || !reflect.DeepEqual(got, want) {
		t.Errorf("pull requests %+v, proposed %q", fj.prs, got)
	}
}

// Ruling 296: an approver's close stands while the branch holds the
// render; no pass deletes or reopens it. A change of the render deletes the
// branch, and the next pass opens a new pull request.
func TestTemplateUpdateRespectsAClose(t *testing.T) {
	fj, reg, store := templateRig(t, stage15Main())
	ctx := context.Background()
	reg.TemplateUpdates(ctx)
	fj.closePR(1, false)
	writes, logs := len(fj.writes), captureLog(t)
	reg.TemplateUpdates(ctx)
	reg.TemplateUpdates(ctx)
	if len(fj.writes) != writes || len(fj.prs) != 1 || fj.branches[TemplateUpdateBranch] == nil ||
		strings.Count(logs.String(), "project demo-app: template update: waiting: #1 closed unmerged; the template's next change reopens it\n") != 2 {
		t.Fatalf("writes %v, pull requests %+v\n%s", fj.writes[writes:], fj.prs, logs)
	}
	p := store.m["demo-app"]
	p.TemplateFiles = maps.Clone(p.TemplateFiles)
	p.TemplateFiles["deploy/smoke.sh"] = "curl -f [[.Name]]\n"
	store.m["demo-app"] = p
	reg.TemplateUpdates(ctx)
	if fj.branches[TemplateUpdateBranch] != nil || len(fj.prs) != 1 {
		t.Fatalf("branch %v, pull requests %+v", fj.branches[TemplateUpdateBranch], fj.prs)
	}
	reg.TemplateUpdates(ctx)
	if got, want := fj.proposed(TemplateUpdateBranch), map[string]string{".forgejo/workflows/ci.yml": renderedCI, "deploy/smoke.sh": "curl -f demo-app\n"}; len(fj.prs) != 2 || fj.prs[1].state != "open" || !reflect.DeepEqual(got, want) {
		t.Errorf("pull requests %+v, proposed %q", fj.prs, got)
	}
}

// The loop runs the template pass at a new configuration version, not
// before templateDelay otherwise.
func TestReconcileLoopRunsTheTemplatePassAtANewVersion(t *testing.T) {
	rig := newRegisterRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	rig.fj.branches["main"] = "c0ffee"
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	p := rig.store.m["demo-app"]
	p.Template = "gone" // no files: the pass says so without a call
	rig.store.m["demo-app"] = p
	rig.logs.Reset()
	var version atomic.Int64
	version.Store(7)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.reg.ReconcileLoop(ctx, version.Load, time.Millisecond, time.Hour)
	}()
	time.Sleep(50 * time.Millisecond)
	version.Store(8)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if n := strings.Count(rig.logs.String(), "project demo-app: template update: template \"gone\" is not in the configuration version in force\n"); n != 1 {
		t.Errorf("template passes = %d, want 1:\n%s", n, rig.logs)
	}
}
