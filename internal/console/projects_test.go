package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type fakeProjects struct {
	rows []factoryd.Project
}

func (f *fakeProjects) Projects(context.Context) ([]factoryd.Project, error) { return f.rows, nil }

// projectRows are the factory's code (seed, coder), the demo app (factory,
// registered), the infra repository (seed, agents none) and a row whose
// file is gone.
func projectRows() []factoryd.Project {
	return []factoryd.Project{
		{Name: "ai-software-factory-code", Repository: "sf/code", MergeRule: "human", Objects: "seed", Agents: "coder", File: true,
			Process: "maintenance", Lanes: testProcesses()["maintenance"].Lanes,
			Namespaces: []string{"factory"}, Tokens: policy.SeedTokens{Read: policy.SecretCIRegistrar, Actions: policy.SecretCIRegistrar, Merge: policy.SecretMerge}},
		{Name: "ai-software-factory-infra", Repository: "sf/infra", MergeRule: "human", Objects: "seed", Agents: "none", File: true,
			Tokens: policy.SeedTokens{Read: policy.SecretConfig}},
		{Name: "demo-app", Repository: "demo-app/demo-app", MergeRule: "auto", Objects: "factory", Agents: "coder", File: true,
			Process: "maintenance", Lanes: testProcesses()["maintenance"].Lanes,
			Namespaces:   []string{"demo-staging", "demo-prod"},
			Environments: []factoryd.Environment{{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"}},
			Quota:        policy.Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"}, ReadTokenHash: "h", GitToken: "secret-git-token", MergeToken: "secret-merge-token"},
		{Name: "gone", PullToken: "secret-pull-token"},
	}
}

// Ruling 179, 183: /projects shows every project read-only, each linked to
// its file at the version in force, with no form; its envelope is the
// file's, its use the registry's.
func TestProjectsPage(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.resources.standing = map[string]cloudres.StandingUse{"demo-app/demo-app": {Count: 1, MonthlyEUR: 5.99}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ck)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects = %d: %s", rec.Code, body)
	}
	const at = "https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory/projects/"
	for _, want := range []string{
		`<h3 id="project-demo-app">demo-app</h3>`, `Settings from <a href="` + at + `demo-app.yaml">projects/demo-app.yaml</a>.`,
		"Repository: demo-app/demo-app", "Objects: factory", "Agents: coder", "Merge rule: auto",
		"Notify: sebastian (the policy's approvers)", "Namespaces: demo-staging, demo-prod",
		"Quota per namespace: 2 CPU, 4Gi memory, 10Gi storage",
		"Task block: 30.00 EUR a month, types cx23, cx33, 3 servers at once, leases up to 8 h",
		"Standing block: 12.00 EUR a month, types cx23, 2 servers", "Standing now: 1 servers, 5.9900 EUR a month",
		"demo-staging: deploy/staging at demo-staging.aisoftwarefactory.world",
		"hostname: demo-staging.aisoftwarefactory.world", // the gateway YAML
		"Forgejo: organisation, repository, CI user and tokens made",
		`<h3 id="project-ai-software-factory-code">ai-software-factory-code</h3>`, `href="` + at + `ai-software-factory-code.yaml"`,
		"Objects: seed: the infra repository and the seed make them, factoryd reads and reports",
		"Tokens: read forgejo-ci-registrar, Actions forgejo-ci-registrar, merge forgejo-merge",
		"Agents: none: every issue goes to the human lane", "Tokens: read forgejo-config",
		"No file projects/gone.yaml in the configuration: nothing acts on this project",
		`<a href="/projects/new">New project</a>`,
		`<a href="/projects">Projects</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/projects lacks %q", want)
		}
	}
	for _, leak := range []string{"secret-git-token", "secret-merge-token", "secret-pull-token", ">h<", "<form method=\"post\" action=\"/projects"} {
		if strings.Contains(body, leak) {
			t.Errorf("/projects shows %q", leak)
		}
	}
}

// The settings edits are gone: the file is the one place, and a new
// project's file goes to infra as a pull request (POST /projects).
func TestProjectsHaveNoEdits(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	for _, path := range []string{"/projects/demo-app/settings", "/projects/demo-app"} {
		if rec := r.do(post(path, url.Values{"csrf": {s.csrf}, "merge_rule": {"auto"}}), ck); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want no such route", path, rec.Code)
		}
	}
}

// infraFake is the infra repository sf/infra in front of the rig's
// Forgejo: a proxy that answers infra's contents, pull request and merge
// calls and passes every other call on.
type infraFake struct {
	mu          sync.Mutex
	pull        map[string]any // GET pulls/main/project-linkshort; nil: 404
	pullAuth    []string
	contents    []recorded
	pulls       []recorded
	merges      []recorded
	mergeStatus int
	mergeBody   string
}

type recorded struct {
	auth string
	body map[string]any
}

func withInfra(t *testing.T, r *rig) *infraFake {
	t.Helper()
	f := &infraFake{mergeStatus: http.StatusOK}
	next, err := url.Parse(r.fg.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(next)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rec := func() recorded {
			var body map[string]any
			json.NewDecoder(req.Body).Decode(&body)
			return recorded{auth: req.Header.Get("Authorization"), body: body}
		}
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/repos/sf/infra/contents":
			f.contents = append(f.contents, rec())
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"commit":{"sha":"c0ffee"}}`))
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/repos/sf/infra/pulls":
			f.pulls = append(f.pulls, rec())
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"number":12}`))
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/repos/sf/infra/pulls/main/project-linkshort":
			f.pullAuth = append(f.pullAuth, req.Header.Get("Authorization"))
			if f.pull == nil {
				http.Error(w, `{"message":"pull request does not exist"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(f.pull)
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/repos/sf/infra/pulls/12/merge":
			f.merges = append(f.merges, rec())
			w.WriteHeader(f.mergeStatus)
			w.Write([]byte(f.mergeBody))
		case strings.HasPrefix(req.URL.Path, "/api/v1/repos/sf/infra/"):
			t.Errorf("unexpected infra call %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		default:
			proxy.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	r.c.cfg.ForgejoURL = srv.URL
	r.c.cfg.InfraRepo = factoryd.Repo{Owner: "sf", Name: "infra"}
	r.c.cfg.InfraToken = "config-token"
	return f
}

func (f *infraFake) set(pull map[string]any) {
	f.mu.Lock()
	f.pull = pull
	f.mu.Unlock()
}

// inForce is the version in force of the rig's ledger with projects/<name>.yaml
// added when the name is listed.
type inForce struct {
	base Policies
	mu   sync.Mutex
	add  []string
}

func (p *inForce) CurrentPolicy(ctx context.Context) (policy.Version, bool, error) {
	v, ok, err := p.base.CurrentPolicy(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	projects := maps.Clone(v.Tree.Projects)
	for _, name := range p.add {
		projects[name] = policy.ProjectConfig{Repository: name + "/" + name, MergeRule: "human", Objects: "factory", Agents: "coder", Process: "spec"}
	}
	v.Tree.Projects = projects
	return v, ok, err
}

func (p *inForce) load(name string) {
	p.mu.Lock()
	p.add = append(p.add, name)
	p.mu.Unlock()
}

// linkshortFile is section 5's file for the form linkshort, spec, ts-go,
// human, filed by sebastian on day.
func linkshortFile(day string) string {
	return "# yaml-language-server: $schema=../schema/project.schema.json\n" +
		"# Filed from the console by sebastian on " + day + ".\n" +
		"repository: linkshort/linkshort\n" +
		"merge_rule: human\n" +
		"objects: factory\n" +
		"agents: coder\n" +
		"process: spec\n" +
		"template: ts-go\n" +
		"namespaces: [linkshort-staging]\n" +
		"environments:\n" +
		"  - {namespace: linkshort-staging, path: deploy/staging}\n"
}

func linkshortForm(csrf string) url.Values {
	return url.Values{"csrf": {csrf}, "name": {"linkshort"}, "process": {"spec"}, "template": {"ts-go"}, "merge_rule": {"human"}}
}

// The form offers the tree's processes and templates to every console
// user; a file the loader's check refuses is not filed, and its lines show
// under the form with the human's choices kept.
func TestNewProjectRefusedByCheck(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	var checked []string
	r.c.cfg.CheckProject = func(_ context.Context, name string, file []byte) ([]string, error) {
		checked = append(checked, name, string(file))
		return []string{`projects/linkshort.yaml: process "spec": no file processes/spec.yaml (processes: maintenance)`,
			`projects/linkshort.yaml: template "ts-go": no directory templates/ts-go/ (templates: none)`}, nil
	}
	ck, s := r.loggedIn(t)
	form := r.do(httptest.NewRequest(http.MethodGet, "/projects/new", nil), ck)
	body := form.Body.String()
	for _, want := range []string{`<form method="post" action="/projects"`, `name="name"`, `pattern="[a-z0-9][a-z0-9\-]{0,35}[a-z0-9]"`,
		`<option value="maintenance">maintenance</option>`, `<option value="" selected>none</option>`,
		`<option value="human" selected>human</option>`, `<option value="auto">auto</option>`} {
		if form.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("GET /projects/new = %d, lacks %q", form.Code, want)
		}
	}
	today := time.Now().UTC().Format(time.DateOnly)
	rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
	body = rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /projects = %d, want 422: %s", rec.Code, body)
	}
	if len(checked) != 2 || checked[0] != "linkshort" || checked[1] != linkshortFile(today) {
		t.Fatalf("checked %q, want linkshort and\n%s", checked, linkshortFile(today))
	}
	for _, want := range []string{`projects/linkshort.yaml: process &#34;spec&#34;: no file processes/spec.yaml (processes: maintenance)`,
		`projects/linkshort.yaml: template &#34;ts-go&#34;: no directory templates/ts-go/ (templates: none)`,
		`value="linkshort"`, `<form method="post" action="/projects"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the refused form lacks %q", want)
		}
	}
	for form, line := range map[string]string{
		"name=L":               "name: a DNS label of 2 to 37 lowercase letters, digits and dashes",
		"name=-x":              "name: a DNS label of 2 to 37 lowercase letters, digits and dashes",
		"merge_rule=sometimes": "merge rule: human or auto",
		"process=":             "process: one of the configuration's processes",
		"template=../x":        "template: one of the configuration's templates, or none",
	} {
		k, v, _ := strings.Cut(form, "=")
		f := linkshortForm(s.csrf)
		f.Set(k, v)
		checked = nil
		rec := r.do(post("/projects", f), ck)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), html.EscapeString(line)) || checked != nil {
			t.Errorf("%s: %d, checked %v, want 422 with %q", form, rec.Code, checked != nil, line)
		}
	}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.contents)+len(infra.pulls) != 0 {
		t.Errorf("a refused file was filed: %d contents, %d pulls", len(infra.contents), len(infra.pulls))
	}
}

// A file that passes goes to infra as the human: one create in a new
// branch project-<name> off main, the pull request, then the panel.
// Filing is spend: another writer gets 403 and nothing is filed.
func TestNewProjectOpensPullRequestAsHuman(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	today := time.Now().UTC().Format(time.DateOnly)
	rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/linkshort/status" {
		t.Fatalf("POST /projects = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	infra.mu.Lock()
	contents, pulls := infra.contents, infra.pulls
	infra.mu.Unlock()
	wantContents := map[string]any{"branch": "main", "new_branch": "project-linkshort",
		"message": "Project linkshort: add its file (console, sebastian)",
		"files": []any{map[string]any{"operation": "create", "path": "factory/projects/linkshort.yaml",
			"content": base64.StdEncoding.EncodeToString([]byte(linkshortFile(today)))}}}
	if len(contents) != 1 || contents[0].auth != "Bearer access-1" || !reflect.DeepEqual(contents[0].body, wantContents) {
		t.Fatalf("contents calls %v, want one as the human with %v", contents, wantContents)
	}
	wantPull := map[string]any{"head": "project-linkshort", "base": "main", "title": "Project linkshort"}
	if len(pulls) != 1 || pulls[0].auth != "Bearer access-1" || !reflect.DeepEqual(pulls[0].body, wantPull) {
		t.Fatalf("pull calls %v, want one as the human with %v", pulls, wantPull)
	}

	other := newRig(t)
	otherInfra := withInfra(t, other)
	other.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) {
		t.Error("a non-approver's file was checked")
		return nil, nil
	}
	other.fg.user, other.writers.users = "alice", []string{"sebastian", "alice"}
	ock, os := other.loggedIn(t)
	if rec := other.do(post("/projects", linkshortForm(os.csrf)), ock); rec.Code != http.StatusForbidden {
		t.Errorf("POST /projects by a non-approver = %d, want 403", rec.Code)
	}
	if rec := other.do(httptest.NewRequest(http.MethodGet, "/projects/new", nil), ock); rec.Code != http.StatusOK {
		t.Errorf("GET /projects/new by a writer = %d, want 200", rec.Code)
	}
	otherInfra.mu.Lock()
	defer otherInfra.mu.Unlock()
	if len(otherInfra.contents)+len(otherInfra.pulls) != 0 {
		t.Error("a non-approver's file was filed")
	}
}

var stepRE = regexp.MustCompile(`<li class="step (\w+)" title="([^"]*)"><span class="name">([^<]*)</span>`)

// steps reads the panel's steps: name, state, tooltip.
func steps(body string) (names, states, tips []string) {
	for _, m := range stepRE.FindAllStringSubmatch(body, -1) {
		states, tips, names = append(states, m[1]), append(tips, m[2]), append(names, m[3])
	}
	return names, states, tips
}

type statusRig struct {
	*rig
	infra   *infraFake
	force   *inForce
	main    bool
	mainErr error
	ck      *http.Cookie
	csrf    string
}

func newStatusRig(t *testing.T) *statusRig {
	r := newRig(t)
	s := &statusRig{rig: r, infra: withInfra(t, r), force: &inForce{base: r.ledger}}
	r.c.cfg.Policies = s.force
	r.c.cfg.MainExists = func(_ context.Context, repo factoryd.Repo) (bool, error) {
		if repo != (factoryd.Repo{Owner: "linkshort", Name: "linkshort"}) {
			t.Errorf("MainExists(%v)", repo)
		}
		return s.main, s.mainErr
	}
	s.ck, _ = r.loggedIn(t)
	s.csrf = r.c.sessions[s.ck.Value].csrf
	return s
}

func (s *statusRig) row(p factoryd.Project) {
	s.projects.rows = append(projectRows(), p)
}

func (s *statusRig) status(t *testing.T) string {
	t.Helper()
	rec := s.do(httptest.NewRequest(http.MethodGet, "/projects/linkshort/status", nil), s.ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects/linkshort/status = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

var openPull = map[string]any{"number": 12, "state": "open", "merged": false, "html_url": "https://git.example/sf/infra/pulls/12",
	"user": map[string]any{"login": "sebastian"}}

// Each step's state and detail come from the source section 5's table
// names: the pull request read with factoryd's config token, the version
// in force, the project row and main, the registration and merge kept in
// memory, and ready_at.
func TestStatusStepsFromSources(t *testing.T) {
	minted := factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort", Objects: "factory", Agents: "coder", File: true, GitToken: "g"}
	ready := minted
	ready.ReadyAt = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		setup  func(s *statusRig)
		states []string
		detail []string
		absent []string
	}{
		{"nothing", func(s *statusRig) {}, []string{"waiting", "waiting", "waiting", "waiting"},
			[]string{"No pull request from branch project-linkshort into main yet."}, []string{`action="/projects/linkshort/merge"`, `type="password"`}},
		{"pull request open", func(s *statusRig) { s.infra.set(openPull) }, []string{"done", "waiting", "waiting", "waiting"},
			[]string{`<a href="https://git.example/sf/infra/pulls/12">#12, filed by sebastian</a>`, "Waiting for an approver to merge #12.",
				`action="/projects/linkshort/merge"`, `hx-post="/projects/linkshort/merge"`, `>Merge</button>`}, []string{`type="password"`}},
		{"pull request closed", func(s *statusRig) {
			s.infra.set(map[string]any{"number": 12, "state": "closed", "merged": false, "html_url": "https://git.example/sf/infra/pulls/12", "user": map[string]any{"login": "sebastian"}})
		}, []string{"failed", "waiting", "waiting", "waiting"}, []string{"#12 was closed without merging."}, []string{`action="/projects/linkshort/merge"`}},
		{"pull request unreadable", func(s *statusRig) { s.infra.set(map[string]any{"number": "twelve"}) }, []string{"failed", "waiting", "waiting", "waiting"},
			[]string{"Cannot read the pull request; the factoryd log says why."}, nil},
		{"merge refused", func(s *statusRig) {
			s.infra.set(openPull)
			s.c.runs["linkshort"] = &projectRun{merge: "Does not have enough approvals"}
		}, []string{"done", "failed", "waiting", "waiting"}, []string{"Forgejo refused the merge: Does not have enough approvals", `>Merge</button>`}, nil},
		{"merged, not loaded", func(s *statusRig) {
			s.infra.set(map[string]any{"number": 12, "state": "closed", "merged": true, "html_url": "https://git.example/sf/infra/pulls/12", "user": map[string]any{"login": "sebastian"}})
		}, []string{"done", "running", "waiting", "waiting"}, []string{"Merged; the factory loads it within 30 seconds."}, []string{`action="/projects/linkshort/merge"`}},
		{"loaded", func(s *statusRig) { s.force.load("linkshort") }, []string{"done", "done", "waiting", "waiting"},
			[]string{"projects/linkshort.yaml is in the configuration version 4.", "Loaded in version 4 (commit c9d98bb0aa11).",
				`type="password" autocomplete="off"`, `>Create repository</button>`, `hx-post="/projects/linkshort/register"`, `hx-preserve="true"`}, nil},
		{"registering", func(s *statusRig) {
			s.force.load("linkshort")
			s.c.runs["linkshort"] = &projectRun{registering: "sebastian"}
		}, []string{"done", "done", "running", "waiting"}, []string{"Registering with the login of sebastian."}, []string{`type="password"`}},
		{"registration failed", func(s *statusRig) {
			s.force.load("linkshort")
			s.c.runs["linkshort"] = &projectRun{regErr: "organisation linkshort: forgejo: status 401"}
		}, []string{"done", "done", "failed", "waiting"}, []string{"Registration failed: organisation linkshort: forgejo: status 401", `type="password"`}, nil},
		{"minted, main missing", func(s *statusRig) { s.force.load("linkshort"); s.row(minted) }, []string{"done", "done", "running", "waiting"},
			[]string{"Organisation, repository, CI user and tokens made; waiting for main&#39;s first commit."}, []string{`type="password"`}},
		{"main unreadable", func(s *statusRig) { s.force.load("linkshort"); s.row(minted); s.mainErr = errors.New("boom") }, []string{"done", "done", "failed", "waiting"},
			[]string{"Cannot read the repository&#39;s main; the factoryd log says why."}, nil},
		{"created, reconciling", func(s *statusRig) { s.force.load("linkshort"); s.row(minted); s.main = true }, []string{"done", "done", "done", "running"},
			[]string{"linkshort/linkshort with main.", "Waiting for a reconcile that finds every object in place; one runs at least hourly."}, []string{`type="password"`}},
		{"reconcile failed", func(s *statusRig) {
			s.force.load("linkshort")
			s.row(minted)
			s.main = true
			s.c.runs["linkshort"] = &projectRun{regErr: "webhook: forgejo: status 500"}
		}, []string{"done", "done", "done", "failed"}, []string{"The reconcile failed: webhook: forgejo: status 500"}, nil},
		{"ready", func(s *statusRig) { s.force.load("linkshort"); s.row(ready); s.main = true }, []string{"done", "done", "done", "done"},
			[]string{`<a href="/board?project=linkshort">Ready since 2026-10-02 09:30 UTC: open the board</a>`}, []string{`type="password"`, "hx-trigger"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStatusRig(t)
			c.setup(s)
			body := s.status(t)
			names, states, tips := steps(body)
			if want := []string{"Pull request opened", "Approved and merged", "Repository created", "Board ready"}; !slices.Equal(names, want) {
				t.Fatalf("steps %q, want %q", names, want)
			}
			if !slices.Equal(states, c.states) {
				t.Errorf("states %q, want %q", states, c.states)
			}
			for i, tip := range []string{
				"The project&#39;s file as a pull request on the infra repository, filed as you; the factory checked it as its loader will.",
				"An approver merges the file into infra&#39;s main; the factory loads it within 30 seconds.",
				"The Forgejo site admin&#39;s login registers the project: organisation, repository from the template, CI user, tokens. The password is used once and never stored.",
				"Webhook, labels, bot fork, deploy branch, namespaces, network policy and the Flux Kustomization are in place.",
			} {
				if tips[i] != tip {
					t.Errorf("tooltip %d = %q, want %q", i, tips[i], tip)
				}
			}
			for _, want := range c.detail {
				if !strings.Contains(body, want) {
					t.Errorf("the panel lacks %q", want)
				}
			}
			for _, not := range c.absent {
				if strings.Contains(body, not) {
					t.Errorf("the panel shows %q", not)
				}
			}
			s.infra.mu.Lock()
			defer s.infra.mu.Unlock()
			for _, auth := range s.infra.pullAuth {
				if auth != "Bearer config-token" {
					t.Errorf("the pull request was read with %q, want factoryd's config token", auth)
				}
			}
		})
	}
}

// The panel polls every 3 s while any step is not done and stops at the
// final render; htmx asks for the panel alone.
func TestStatusStopsPolling(t *testing.T) {
	s := newStatusRig(t)
	s.infra.set(openPull)
	const poll = `hx-get="/projects/linkshort/status" hx-trigger="every 3s" hx-swap="outerHTML"`
	if body := s.status(t); !strings.Contains(body, poll) || !strings.Contains(body, "<html") {
		t.Fatalf("an unfinished panel does not poll:\n%s", body)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/linkshort/status", nil)
	req.Header.Set("HX-Request", "true")
	part := s.do(req, s.ck).Body.String()
	if !strings.HasPrefix(part, `<section id="status"`) || strings.Contains(part, "<html") || !strings.Contains(part, poll) {
		t.Fatalf("htmx's request got:\n%s", part)
	}
	s.force.load("linkshort")
	s.row(factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort", File: true, GitToken: "g", ReadyAt: time.Now()})
	s.main = true
	if body := s.status(t); strings.Contains(body, "hx-trigger") || strings.Contains(body, "hx-get") {
		t.Fatalf("the final panel still polls:\n%s", body)
	}
}

// The Merge button merges as the human with section 5's body; Forgejo's
// refusal shows on the step.
func TestMergeAsHumanShowsRefusal(t *testing.T) {
	s := newStatusRig(t)
	s.infra.set(openPull)
	s.infra.mergeStatus, s.infra.mergeBody = http.StatusMethodNotAllowed, `{"message":"Does not have enough approvals","url":"x"}`
	req := post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	rec := s.do(req, s.ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Forgejo refused the merge: Does not have enough approvals") {
		t.Fatalf("merge refused: %d\n%s", rec.Code, rec.Body.String())
	}
	s.infra.mu.Lock()
	merges := s.infra.merges
	s.infra.mu.Unlock()
	want := map[string]any{"Do": "merge", "delete_branch_after_merge": true}
	if len(merges) != 1 || merges[0].auth != "Bearer access-1" || !reflect.DeepEqual(merges[0].body, want) {
		t.Fatalf("merges %v, want one as the human with %v", merges, want)
	}
	s.infra.mergeStatus, s.infra.mergeBody = http.StatusOK, ""
	if rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("merge = %d", rec.Code)
	}
	if _, states, _ := steps(s.status(t)); states[1] != "waiting" {
		t.Errorf("after a merge Forgejo took, step 2 = %s (the pull request still reads open in the fake)", states[1])
	}
}

// The registration's password lives on the goroutine's stack: after the
// call nothing the console keeps, and nothing it logs, holds it.
func TestRegisterPasswordNotKept(t *testing.T) {
	s := newStatusRig(t)
	s.force.load("linkshort")
	const password = "correct-horse-battery-staple"
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	done := make(chan factoryd.AdminAuth, 1)
	s.c.cfg.Register = func(_ context.Context, name string, a factoryd.AdminAuth) error {
		done <- a
		return errors.New("organisation linkshort: forgejo: status 401")
	}
	rec := s.do(post("/projects/linkshort/register", url.Values{"csrf": {s.csrf}, "password": {password}}), s.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	if a := <-done; a != (factoryd.AdminAuth{User: "sebastian", Password: password}) {
		t.Fatalf("Register got %+v", a)
	}
	waitRun(t, s.c, "linkshort")
	s.c.mu.Lock()
	kept := fmt.Sprintf("%+v %+v", *s.c.runs["linkshort"], s.c.cfg)
	for _, se := range s.c.sessions {
		kept += fmt.Sprintf(" %+v", se)
	}
	s.c.mu.Unlock()
	if strings.Contains(kept, password) || !strings.Contains(kept, "status 401") {
		t.Errorf("the console's state after the call: %s", kept)
	}
	if strings.Contains(logs.String(), password) || strings.Contains(s.status(t), password) {
		t.Error("the password was logged or rendered")
	}
	if rec := s.do(post("/projects/linkshort/register", url.Values{"csrf": {s.csrf}}), s.ck); rec.Code != http.StatusBadRequest {
		t.Errorf("register without a password = %d, want 400", rec.Code)
	}
}

// A closed tab does not cut a half-made registration: the call runs on a
// context detached from the request.
func TestRegisterSurvivesClosedRequest(t *testing.T) {
	s := newStatusRig(t)
	s.force.load("linkshort")
	release, ended := make(chan struct{}), make(chan error, 1)
	s.c.cfg.Register = func(ctx context.Context, name string, a factoryd.AdminAuth) error {
		<-release
		ended <- ctx.Err()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	rec := s.do(post("/projects/linkshort/register", url.Values{"csrf": {s.csrf}, "password": {"pw"}}).WithContext(ctx), s.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("register = %d", rec.Code)
	}
	cancel()
	if _, states, _ := steps(s.status(t)); states[2] != "running" {
		t.Errorf("while registering, step 3 = %s", states[2])
	}
	if rec := s.do(post("/projects/linkshort/register", url.Values{"csrf": {s.csrf}, "password": {"pw"}}), s.ck); rec.Code != http.StatusConflict {
		t.Errorf("a second registration while one runs = %d, want 409", rec.Code)
	}
	close(release)
	if err := <-ended; err != nil {
		t.Fatalf("the registration's context ended with the request: %v", err)
	}
	waitRun(t, s.c, "linkshort")
	if _, states, _ := steps(s.status(t)); states[2] != "waiting" {
		t.Errorf("after a registration without error and no tokens yet, step 3 = %s", states[2])
	}
}

func waitRun(t *testing.T, c *Console, name string) {
	t.Helper()
	for range 200 {
		c.mu.Lock()
		running := c.runs[name].registering != ""
		c.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the registration did not end")
}
