package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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

// Projects answers a fresh slice, as the store does: coderProjects
// filters its answer in place.
func (f *fakeProjects) Projects(context.Context) ([]factoryd.Project, error) {
	return slices.Clone(f.rows), nil
}

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
		"Notify: sebastian (the project's approvers)", "Namespaces: demo-staging, demo-prod",
		"Quota per namespace: 2 CPU, 4Gi memory, 10Gi storage",
		"Task block: 30.00 EUR a month, types cx23, cx33, 3 servers at once, leases up to 8 h",
		"Standing block: 12.00 EUR a month, types cx23, 2 servers", "Standing now: 1 server, 5.9900 EUR a month",
		"demo-staging: deploy/staging at demo-staging.aisoftwarefactory.world",
		"hostname: demo-staging.aisoftwarefactory.world", // the gateway YAML
		"Forgejo: organisation, repository, CI user and tokens made",
		`<h3 id="project-ai-software-factory-code">ai-software-factory-code</h3>`, `href="` + at + `ai-software-factory-code.yaml"`,
		"Objects: seed: the infra repository and the seed make them, factoryd reads and reports",
		"Tokens: read forgejo-ci-registrar, Actions forgejo-ci-registrar, merge forgejo-merge",
		"Agents: none: every issue goes to the human lane", "Tokens: read forgejo-config",
		"No file projects/gone.yaml in the configuration: nothing acts on this project",
		`<a href="/projects/new">New project</a>`,
		`<a href="/projects" aria-current="page">Projects</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/projects lacks %q", want)
		}
	}
	settings := strings.ReplaceAll(body, `<form method="post" action="/projects/demo-app/export"`, "")
	for _, leak := range []string{"secret-git-token", "secret-merge-token", "secret-pull-token", ">h<", "<form method=\"post\" action=\"/projects"} {
		if strings.Contains(settings, leak) {
			t.Errorf("/projects shows %q", leak)
		}
	}
}

// Roadmap 257: the projects page and its tool show each environment of a
// factory project with the Ready condition of its Flux Kustomization:
// ready, not ready with Flux's message once Flux applied it (even when the
// deploy branch has since lost its path), not deployed yet while Flux never
// applied it (no green build wrote its path, or Flux's message), no
// Kustomization before Flux has one, and unreadable when the read fails. A
// project registration has not minted has no Kustomization yet and is not
// read.
func TestProjectsShowEachEnvironmentsDeployState(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, _ := r.loggedIn(t)
	factory := func(name string, minted bool, envs ...factoryd.Environment) factoryd.Project {
		p := factoryd.Project{Name: name, Repository: name + "/" + name, MergeRule: "auto", Objects: policy.ObjectsFactory, Agents: "coder",
			File: true, Environments: envs}
		if minted {
			p.GitToken = "g"
		}
		return p
	}
	r.projects.rows = []factoryd.Project{projectRows()[0],
		factory("demo-app", true, factoryd.Environment{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"},
			factoryd.Environment{Namespace: "demo-prod", Path: "deploy/prod"}),
		factory("broken", true, factoryd.Environment{Namespace: "broken-staging", Path: "deploy/staging"},
			factoryd.Environment{Namespace: "broken-prod", Path: "deploy/prod"}, factoryd.Environment{Namespace: "broken-new", Path: "deploy/new"},
			factoryd.Environment{Namespace: "broken-locked", Path: "deploy/locked"}, factoryd.Environment{Namespace: "broken-gone", Path: "deploy/gone"},
			factoryd.Environment{Namespace: "broken-first", Path: "deploy/first"}),
		factory("waiting", false, factoryd.Environment{Namespace: "waiting-staging", Path: "deploy/staging"}),
	}
	type ready struct {
		s   factoryd.FluxState
		err error
	}
	flux := map[string]ready{
		"demo-staging/demo-app": {factoryd.FluxState{Ready: "True", Message: "Applied revision: deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456",
			Applied: "deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"}, nil},
		"demo-prod/demo-app": {factoryd.FluxState{Ready: "False",
			Message: "kustomization path not found: stat /tmp/kustomization-103304189/deploy/prod: no such file or directory"}, nil},
		"broken-staging/broken": {factoryd.FluxState{Ready: "False", Message: "Deployment/broken-staging/app dry-run failed: no matches for kind Thing",
			Applied: "deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"}, nil},
		"broken-prod/broken":   {},
		"broken-new/broken":    {err: factoryd.ErrNotFound},
		"broken-locked/broken": {err: factoryd.ErrForbidden},
		"broken-gone/broken": {factoryd.FluxState{Ready: "False", Message: "kustomization path not found: stat /tmp/kustomization-103304189/deploy/gone: no such file or directory",
			Applied: "deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"}, nil},
		"broken-first/broken": {factoryd.FluxState{Ready: "False", Message: "Deployment/broken-first/app dry-run failed: no matches for kind Thing"}, nil},
	}
	var mu sync.Mutex
	var read []string
	r.c.cfg.Kustomization = func(_ context.Context, namespace, name string) (factoryd.FluxState, error) {
		mu.Lock()
		defer mu.Unlock()
		read = append(read, namespace+"/"+name)
		f, ok := flux[namespace+"/"+name]
		if !ok {
			t.Errorf("read the Kustomization %s/%s", namespace, name)
		}
		return f.s, f.err
	}
	readOnce := []string{"broken-first/broken", "broken-gone/broken", "broken-locked/broken", "broken-new/broken", "broken-prod/broken", "broken-staging/broken",
		"demo-prod/demo-app", "demo-staging/demo-app"}

	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects = %d: %s", rec.Code, rec.Body)
	}
	for _, want := range []string{
		"Environments: demo-staging: deploy/staging at demo-staging.aisoftwarefactory.world (ready); " +
			"demo-prod: deploy/prod (not deployed yet: no green build has written deploy/prod)</li>",
		"Environments: broken-staging: deploy/staging (not ready: Deployment/broken-staging/app dry-run failed: no matches for kind Thing); " +
			"broken-prod: deploy/prod (not deployed yet: Flux has not reconciled it yet); broken-new: deploy/new (no Flux Kustomization yet); " +
			"broken-locked: deploy/locked (deploy state unreadable); " +
			"broken-gone: deploy/gone (not ready: kustomization path not found: stat /tmp/kustomization-103304189/deploy/gone: no such file or directory); " +
			"broken-first: deploy/first (not deployed yet: Deployment/broken-first/app dry-run failed: no matches for kind Thing)</li>",
		"Environments: waiting-staging: deploy/staging (no Flux Kustomization yet)</li>",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/projects lacks %q", want)
		}
	}
	slices.Sort(read)
	equal(t, "the page's reads", read, readOnce)

	read = nil
	equal(t, "the tool's read", reads(t, r, g, "projects", `{}`), []string{"Projects (4):\n" +
		"ai-software-factory-code (sf/code): objects seed; process maintenance; merge rule human; made by the infra repository and the seed; GitHub export none\n" +
		"demo-app (demo-app/demo-app): objects factory; process none; merge rule auto; registered, not ready yet; GitHub export none; " +
		"environments demo-staging: deploy/staging at demo-staging.aisoftwarefactory.world (ready), " +
		"demo-prod: deploy/prod (not deployed yet: no green build has written deploy/prod)\n" +
		"broken (broken/broken): objects factory; process none; merge rule auto; registered, not ready yet; GitHub export none; " +
		"environments broken-staging: deploy/staging (not ready: Deployment/broken-staging/app dry-run failed: no matches for kind Thing), " +
		"broken-prod: deploy/prod (not deployed yet: Flux has not reconciled it yet), broken-new: deploy/new (no Flux Kustomization yet), " +
		"broken-locked: deploy/locked (deploy state unreadable), " +
		"broken-gone: deploy/gone (not ready: kustomization path not found: stat /tmp/kustomization-103304189/deploy/gone: no such file or directory), " +
		"broken-first: deploy/first (not deployed yet: Deployment/broken-first/app dry-run failed: no matches for kind Thing)\n" +
		"waiting (waiting/waiting): objects factory; process none; merge rule auto; not registered; GitHub export none; " +
		"environments waiting-staging: deploy/staging (no Flux Kustomization yet)"})
	slices.Sort(read)
	equal(t, "the tool's reads", read, readOnce)
}

// The Kustomizations are read at once, at most fluxReads at a time: each
// read waits until fluxReads reads have started, so reads one after
// another would wait out the deadline with one in flight.
func TestProjectsReadTheKustomizationsAtOnce(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	wide := factoryd.Project{Name: "wide", Repository: "wide/wide", MergeRule: "auto", Objects: policy.ObjectsFactory, Agents: "coder",
		File: true, GitToken: "g"}
	for i := range fluxReads + 2 {
		wide.Environments = append(wide.Environments, factoryd.Environment{Namespace: fmt.Sprintf("wide-%d", i), Path: "deploy/staging"})
	}
	r.projects.rows = []factoryd.Project{wide}
	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	all := make(chan struct{})
	var mu sync.Mutex
	started, inFlight, most := 0, 0, 0
	r.c.cfg.Kustomization = func(context.Context, string, string) (factoryd.FluxState, error) {
		mu.Lock()
		started, inFlight = started+1, inFlight+1
		most = max(most, inFlight)
		if started == fluxReads {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-deadline.Done():
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return factoryd.FluxState{Ready: "True"}, nil
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects = %d: %s", rec.Code, rec.Body)
	}
	equal(t, "the reads at once at most", most, fluxReads)
	equal(t, "the ready environments", strings.Count(rec.Body.String(), "deploy/staging (ready)"), fluxReads+2)
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
	pull        map[string]any // GET pulls/<its number>; nil: 404
	pullAuth    []string
	contents    []recorded
	pulls       []recorded
	pullsStatus int              // the pull request's opening; 0: 201
	open        []map[string]any // GET pulls?state=open
	// branches, when set, are infra's branches besides main with their
	// heads: a commit makes its new branch with a head of its own, a new
	// branch that exists is refused, a deleted one goes; unset, no branch
	// exists. branchReads are the reads' Authorization, branchDeletes the
	// deletions, branchStatus their answer, 0: 204.
	branches      map[string]string
	branchReads   []string
	branchDeletes []recorded
	branchStatus  int
	// contentsAfter answers a commit once its branch is made; 0: 201
	contentsAfter int
	// racers, when set, holds the first branch reads until all of them
	// came, so the filings behind them pass the branch check together
	racers *barrier
	// merges and closes are the calls on a pull request, its path theirs;
	// closeStatus answers a close, 0: 201
	merges, closes []recorded
	closeStatus    int
	mergeStatus    int
	mergeBody      string
	// mergeStatuses answers the merge calls in order, then mergeStatus.
	mergeStatuses []int
	// contentsStatus answers the contents calls; 0: 201
	contentsStatus int
	// files are main's files a GET contents reads, by path, and reads
	// the Authorization of each read
	files map[string]string
	reads []string
}

// barrier holds the first n callers of wait until all n came; later ones
// pass at once.
type barrier struct {
	mu  sync.Mutex
	n   int
	all chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, all: make(chan struct{})} }

func (b *barrier) wait() {
	b.mu.Lock()
	if b.n == 0 {
		b.mu.Unlock()
		return
	}
	if b.n--; b.n == 0 {
		close(b.all)
	}
	b.mu.Unlock()
	<-b.all
}

type recorded struct {
	auth, path string
	body       map[string]any
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
		racers := f.racers
		f.mu.Unlock()
		if racers != nil && req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/repos/sf/infra/branches/") {
			racers.wait()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		rec := func() recorded {
			var body map[string]any
			json.NewDecoder(req.Body).Decode(&body)
			return recorded{auth: req.Header.Get("Authorization"), path: req.URL.Path, body: body}
		}
		number, merge := strings.CutSuffix(strings.TrimPrefix(req.URL.Path, "/api/v1/repos/sf/infra/pulls/"), "/merge")
		numbered := strings.HasPrefix(req.URL.Path, "/api/v1/repos/sf/infra/pulls/") && strings.Trim(number, "0123456789") == ""
		switch {
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/repos/sf/infra/contents/") && f.files != nil:
			text, ok := f.files[strings.TrimPrefix(req.URL.Path, "/api/v1/repos/sf/infra/contents/")]
			if !ok {
				http.NotFound(w, req)
				return
			}
			f.reads = append(f.reads, req.Header.Get("Authorization"))
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(text)), "sha": "blob1"})
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/repos/sf/infra/contents":
			c := rec()
			f.contents = append(f.contents, c)
			if f.contentsStatus != 0 {
				w.WriteHeader(f.contentsStatus)
				return
			}
			sha := fmt.Sprintf("c0ffee%d", len(f.contents))
			if branch, _ := c.body["new_branch"].(string); f.branches != nil {
				if f.branches[branch] != "" {
					http.Error(w, `{"message":"branch already exists"}`, http.StatusUnprocessableEntity)
					return
				}
				f.branches[branch] = sha
			}
			if f.contentsAfter != 0 {
				w.WriteHeader(f.contentsAfter)
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": sha}})
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/repos/sf/infra/pulls":
			f.pulls = append(f.pulls, rec())
			if f.pullsStatus != 0 {
				w.WriteHeader(f.pullsStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"number":12}`))
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/repos/sf/infra/pulls" && req.URL.Query().Get("state") == "open":
			f.pullAuth = append(f.pullAuth, req.Header.Get("Authorization"))
			json.NewEncoder(w).Encode(append([]map[string]any{}, f.open...))
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/repos/sf/infra/branches/"):
			f.branchReads = append(f.branchReads, req.Header.Get("Authorization"))
			name := strings.TrimPrefix(req.URL.Path, "/api/v1/repos/sf/infra/branches/")
			if f.branches[name] == "" {
				http.Error(w, `{"message":"branch does not exist"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"name": name, "commit": map[string]string{"id": f.branches[name]}})
		case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/api/v1/repos/sf/infra/branches/"):
			f.branchDeletes = append(f.branchDeletes, rec())
			if f.branchStatus != 0 {
				w.WriteHeader(f.branchStatus)
				return
			}
			delete(f.branches, strings.TrimPrefix(req.URL.Path, "/api/v1/repos/sf/infra/branches/"))
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodGet && numbered && !merge:
			f.pullAuth = append(f.pullAuth, req.Header.Get("Authorization"))
			if f.pull == nil || fmt.Sprint(f.pull["number"]) != number {
				http.Error(w, `{"message":"pull request does not exist"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(f.pull)
		case req.Method == http.MethodPatch && numbered && !merge:
			f.closes = append(f.closes, rec())
			if f.closeStatus != 0 {
				w.WriteHeader(f.closeStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case req.Method == http.MethodPost && numbered && merge:
			f.merges = append(f.merges, rec())
			status := f.mergeStatus
			if len(f.mergeStatuses) > 0 {
				status, f.mergeStatuses = f.mergeStatuses[0], f.mergeStatuses[1:]
			}
			w.WriteHeader(status)
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
		"message": "Project linkshort: add its file\n\nFactory-Role: console\nRequested-by: sebastian\n",
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

var stepRE = regexp.MustCompile(`<li class="step (\w+)" tabindex="0" title="([^"]*)"><span class="name">([^<]*)</span>`)

// steps reads the panel's steps: name, state, tooltip.
func steps(body string) (names, states, tips []string) {
	for _, m := range stepRE.FindAllStringSubmatch(body, -1) {
		states, tips, names = append(states, m[1]), append(tips, m[2]), append(names, m[3])
	}
	return names, states, tips
}

type statusRig struct {
	*rig
	infra        *infraFake
	force        *inForce
	main         bool
	mainErr      error
	reconcileErr string
	ck           *http.Cookie
	csrf         string
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
	r.c.cfg.ReconcileError = func(name string) string {
		if name != "linkshort" {
			t.Errorf("ReconcileError(%q)", name)
		}
		return s.reconcileErr
	}
	ck, s2 := r.loggedIn(t)
	s.ck, s.csrf = ck, s2.csrf
	s.filed(t, 12)
	return s
}

// filed records linkshort's filing as pull request number, the event
// createProject writes.
func (r *rig) filed(t *testing.T, number int64) {
	t.Helper()
	if _, err := r.events.Record(context.Background(), factoryd.Event{Repository: "linkshort/linkshort", Kind: factoryd.EventProjectFiled,
		Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"name": "linkshort", "pr": number}}); err != nil {
		t.Fatal(err)
	}
}

// Each merge attempt waits MergeRetryPause on a 405; zero keeps tests fast.
func (s *statusRig) noMergePause() {
	pause := time.Duration(0)
	s.rig.c.cfg.MergeRetryPause = &pause
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
// in force, the project row and main, the merge kept in memory, the
// registration's last error, and ready_at.
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
		{"pull request unreadable", func(s *statusRig) { s.infra.set(map[string]any{"number": 12, "state": 12}) }, []string{"failed", "waiting", "waiting", "waiting"},
			[]string{"Cannot read the pull request; the factoryd log says why."}, nil},
		{"merge refused", func(s *statusRig) {
			s.infra.set(openPull)
			s.c.runs["linkshort"] = &projectRun{merge: "Does not have enough approvals"}
		}, []string{"done", "failed", "waiting", "waiting"}, []string{"Forgejo refused the merge: Does not have enough approvals", `>Merge</button>`}, nil},
		{"merged, not loaded", func(s *statusRig) {
			s.infra.set(map[string]any{"number": 12, "state": "closed", "merged": true, "html_url": "https://git.example/sf/infra/pulls/12", "user": map[string]any{"login": "sebastian"}})
		}, []string{"done", "running", "waiting", "waiting"}, []string{"Merged; the factory loads it within 30 seconds."}, []string{`action="/projects/linkshort/merge"`}},
		{"loaded", func(s *statusRig) { s.force.load("linkshort") }, []string{"done", "done", "running", "waiting"},
			[]string{"projects/linkshort.yaml is in the configuration version 4.", "Loaded in version 4 (commit c9d98bb0aa11).",
				"The reconcile registers it within a minute of the load."}, []string{`<form method="post" action="/projects/linkshort/`}},
		{"registration failed", func(s *statusRig) {
			s.force.load("linkshort")
			s.reconcileErr = "registration: organisation linkshort: forgejo: status 401"
		}, []string{"done", "done", "failed", "waiting"},
			[]string{"The reconcile failed: registration: organisation linkshort: forgejo: status 401. It tries again at least hourly."}, nil},
		{"minted, main missing", func(s *statusRig) { s.force.load("linkshort"); s.row(minted) }, []string{"done", "done", "running", "waiting"},
			[]string{"Organisation, repository, CI user and tokens made; waiting for main&#39;s first commit."}, []string{`type="password"`}},
		{"minted, seed failed", func(s *statusRig) {
			s.force.load("linkshort")
			s.row(minted)
			s.reconcileErr = "seed: forgejo: status 500"
		}, []string{"done", "done", "failed", "waiting"}, []string{"The reconcile failed: seed: forgejo: status 500. It tries again at least hourly."}, nil},
		{"main unreadable", func(s *statusRig) { s.force.load("linkshort"); s.row(minted); s.mainErr = errors.New("boom") }, []string{"done", "done", "failed", "waiting"},
			[]string{"Cannot read the repository&#39;s main; the factoryd log says why."}, nil},
		{"created, reconciling", func(s *statusRig) { s.force.load("linkshort"); s.row(minted); s.main = true }, []string{"done", "done", "done", "running"},
			[]string{"linkshort/linkshort with main.", "Waiting for a reconcile that finds every object in place; one runs at least hourly."}, []string{`type="password"`}},
		{"reconcile failed", func(s *statusRig) {
			s.force.load("linkshort")
			s.row(minted)
			s.main = true
			s.reconcileErr = "webhook: forgejo: status 500"
		}, []string{"done", "done", "done", "failed"}, []string{"The reconcile failed: webhook: forgejo: status 500. It tries again at least hourly."}, nil},
		{"ready despite an old error", func(s *statusRig) { s.force.load("linkshort"); s.row(ready); s.main = true; s.reconcileErr = "old" },
			[]string{"done", "done", "done", "done"}, nil, []string{"old"}},
		{"ready", func(s *statusRig) { s.force.load("linkshort"); s.row(ready); s.main = true }, []string{"done", "done", "done", "done"},
			[]string{`<a href="/board?project=linkshort">Ready since 2026-10-02 09:30 UTC: open the board</a>`}, []string{`type="password"`, "hx-trigger"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStatusRig(t)
			c.setup(s)
			body := s.status(t)
			if !strings.Contains(body, `<a href="/projects" aria-current="page">Projects</a>`) {
				t.Error("the status page's bar does not mark Projects current")
			}
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
				"The factory registers the project with the Forgejo site admin&#39;s login from OpenBao: organisation, repository from the template, CI user, tokens. No one types a password.",
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

// A failed filing deletes its branch project-<name> as the human, so the
// name can be filed again: with Forgejo refusing a branch that exists, the
// filing after a refused pull request makes its branch and opens its own.
func TestFailedFilingDeletesItsBranch(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	infra.mu.Lock()
	infra.branches, infra.pullsStatus = map[string]string{}, http.StatusUnprocessableEntity
	infra.mu.Unlock()
	if rec := r.do(post("/projects", linkshortForm(s.csrf)), ck); rec.Code != http.StatusBadGateway {
		t.Fatalf("a refused pull request: POST /projects = %d %q, want 502", rec.Code, rec.Body.String())
	}
	infra.mu.Lock()
	infra.pullsStatus = 0
	deletes := slices.Clone(infra.branchDeletes)
	infra.mu.Unlock()
	if len(deletes) != 1 || deletes[0].path != "/api/v1/repos/sf/infra/branches/project-linkshort" || deletes[0].auth != "Bearer access-1" {
		t.Errorf("branch deletes %+v, want project-linkshort deleted as the human", deletes)
	}
	if rec := r.do(post("/projects", linkshortForm(s.csrf)), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the second filing: POST /projects = %d %q, want 303", rec.Code, rec.Body.String())
	}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.contents) != 2 || len(infra.pulls) != 2 || infra.branches["project-linkshort"] == "" {
		t.Errorf("%d contents and %d pull calls, branches %v; want two each and project-linkshort", len(infra.contents), len(infra.pulls), infra.branches)
	}
}

// A filing deletes only a branch whose head is the commit it made. One
// that finds branch project-<name> already there, an earlier filing's,
// commits nothing and leaves it; one whose commit errs after Forgejo made
// the branch knows no commit of its own, so its undo, reading the branch
// with the config token, leaves it and says so.
func TestFilingLeavesABranchNotItsOwn(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	infra.mu.Lock()
	infra.branches = map[string]string{"project-linkshort": "earlier"}
	infra.mu.Unlock()
	rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "name: infra has branch project-linkshort from an earlier filing; delete it to file the name again") {
		t.Fatalf("an earlier filing's branch: POST /projects = %d:\n%s", rec.Code, body)
	}
	infra.mu.Lock()
	if len(infra.contents) != 0 || len(infra.branchDeletes) != 0 || infra.branches["project-linkshort"] != "earlier" {
		t.Errorf("an earlier filing's branch: %d commits, %d deletes, branches %v; want none, none and the branch kept",
			len(infra.contents), len(infra.branchDeletes), infra.branches)
	}
	infra.branches, infra.contentsAfter = map[string]string{}, http.StatusInternalServerError
	infra.mu.Unlock()
	rec = r.do(post("/projects", linkshortForm(s.csrf)), ck)
	if body := rec.Body.String(); rec.Code != http.StatusBadGateway ||
		!strings.Contains(body, "Forgejo refused to commit the project's file; branch project-linkshort is left: its head is not this filing's commit") {
		t.Fatalf("a failed commit: POST /projects = %d %q, want 502 saying the branch is left", rec.Code, body)
	}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.branchDeletes) != 0 || infra.branches["project-linkshort"] == "" {
		t.Errorf("a failed commit: branch deletes %+v, branches %v; want none and the branch kept", infra.branchDeletes, infra.branches)
	}
	if !slices.Equal(infra.branchReads, []string{"Bearer config-token", "Bearer config-token", "Bearer config-token"}) {
		t.Errorf("branch reads %q, want three with the config token", infra.branchReads)
	}
}

// Two filings of one name racing both pass the branch check. The one
// whose commit lands first opens its pull request; the other's commit is
// refused, and its undo finds the branch's head is not its commit, so it
// leaves the branch: the winning filing keeps its branch and its pull
// request.
func TestRacingFilingsKeepTheWinnersBranch(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	infra.mu.Lock()
	infra.branches, infra.racers = map[string]string{}, newBarrier(2)
	infra.mu.Unlock()
	codes, bodies := make([]int, 2), make([]string, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	wg.Wait()
	won, lost := 0, 1
	if codes[0] != http.StatusSeeOther {
		won, lost = 1, 0
	}
	if codes[won] != http.StatusSeeOther || codes[lost] != http.StatusBadGateway ||
		!strings.Contains(bodies[lost], "Forgejo refused to commit the project's file; branch project-linkshort is left: its head is not this filing's commit") {
		t.Errorf("racing filings answered %d %q and %d %q; want one 303 and one 502 leaving the branch", codes[0], bodies[0], codes[1], bodies[1])
	}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.contents) != 2 || len(infra.pulls) != 1 || len(infra.closes) != 0 || len(infra.branchDeletes) != 0 ||
		infra.branches["project-linkshort"] != "c0ffee1" {
		t.Errorf("%d commits, %d pull requests, closes %+v, branch deletes %+v, branches %v; want two, one, none, none and the first commit's branch",
			len(infra.contents), len(infra.pulls), infra.closes, infra.branchDeletes, infra.branches)
	}
	if pr, ok, err := r.events.FiledPR(context.Background(), "linkshort"); pr != 12 || !ok || err != nil {
		t.Errorf("FiledPR = %d, %v, %v; want the winner's #12", pr, ok, err)
	}
}

// A filing is several writes as the human, so it starts only on a token
// that outlasts them (filingTime): one that runs out sooner, though not
// within the minute every read refreshes at, is refreshed before the
// first write, and a refresh Forgejo refuses sends the human to /login
// with nothing written and the session ended.
func TestFilingRefreshesTheSessionFirst(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	r.setTokenExpiry(t, ck, time.Now().Add(2*time.Minute))
	if rec := r.do(post("/projects", linkshortForm(s.csrf)), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/linkshort/status" {
		t.Fatalf("POST /projects = %d to %q, want 303 to the panel", rec.Code, rec.Header().Get("Location"))
	}
	if g := r.fg.grants[len(r.fg.grants)-1]; g.Get("grant_type") != "refresh_token" {
		t.Errorf("last grant %v, want the refresh", g)
	}
	infra.mu.Lock()
	if len(infra.contents) != 1 || infra.contents[0].auth != "Bearer access-2" {
		t.Errorf("commits %+v, want one with the refreshed token", infra.contents)
	}
	infra.mu.Unlock()

	r.fg.refuse = true
	r.setTokenExpiry(t, ck, time.Now().Add(2*time.Minute))
	if rec := r.do(post("/projects", linkshortForm(s.csrf)), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("a refused refresh: POST /projects = %d to %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.contents) != 1 || len(r.sessions.rows) != 0 {
		t.Errorf("a refused refresh: %d commits, %d sessions; want the one before and none", len(infra.contents), len(r.sessions.rows))
	}
}

// A second filing of the name merges its own pull request: the panel and
// the Merge button read the pull request the last project_filed event
// records, by its number, not Forgejo's answer for branch
// project-linkshort, which may be the first filing's.
func TestMergeActsOnTheLastFiling(t *testing.T) {
	s := newStatusRig(t)
	s.noMergePause()
	s.filed(t, 13)
	s.infra.set(map[string]any{"number": 13, "state": "open", "merged": false, "html_url": "https://git.example/sf/infra/pulls/13",
		"user": map[string]any{"login": "sebastian"}})
	if body := s.status(t); !strings.Contains(body, `<a href="https://git.example/sf/infra/pulls/13">#13, filed by sebastian</a>`) {
		t.Errorf("the panel lacks #13:\n%s", body)
	}
	if rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("merge = %d: %s", rec.Code, rec.Body.String())
	}
	s.infra.mu.Lock()
	defer s.infra.mu.Unlock()
	if len(s.infra.merges) != 1 || s.infra.merges[0].path != "/api/v1/repos/sf/infra/pulls/13/merge" {
		t.Fatalf("merges %+v, want one of #13", s.infra.merges)
	}
}

// The Merge button merges as the human with section 5's body; Forgejo's
// refusal shows on the step and answers its own request, so a plain POST
// gets a non-2xx status instead of a 303 that shows nothing.
func TestMergeAsHumanShowsRefusal(t *testing.T) {
	s := newStatusRig(t)
	s.noMergePause()
	s.infra.set(openPull)
	s.infra.mergeStatus, s.infra.mergeBody = http.StatusUnprocessableEntity, `{"message":"Does not have enough approvals","url":"x"}`
	req := post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	rec := s.do(req, s.ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Forgejo refused the merge: Does not have enough approvals") {
		t.Fatalf("merge refused: %d\n%s", rec.Code, rec.Body.String())
	}
	if _, states, _ := steps(s.status(t)); states[1] != "failed" {
		t.Errorf("after a refusal step 2 = %s, want failed", states[1])
	}
	s.infra.mu.Lock()
	merges := s.infra.merges
	s.infra.mu.Unlock()
	want := map[string]any{"Do": "merge", "delete_branch_after_merge": true}
	if len(merges) != 1 || merges[0].auth != "Bearer access-1" || !reflect.DeepEqual(merges[0].body, want) {
		t.Fatalf("merges %v, want one as the human with %v", merges, want)
	}
	if rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "Forgejo refused the merge: Does not have enough approvals") {
		t.Errorf("a plain POST's refused merge = %d:\n%s", rec.Code, rec.Body.String())
	}
	s.infra.mergeStatus, s.infra.mergeBody = http.StatusOK, ""
	if rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("merge = %d", rec.Code)
	}
	if _, states, _ := steps(s.status(t)); states[1] != "waiting" {
		t.Errorf("after a merge Forgejo took, step 2 = %s (the pull request still reads open in the fake)", states[1])
	}
}

// A plain POST whose merge Forgejo refuses every time answers 409 with the
// refusal text, after the 405 retries ran out.
func TestMergeRefusalPlainPostGets409(t *testing.T) {
	s := newStatusRig(t)
	s.noMergePause()
	s.infra.set(openPull)
	s.infra.mergeStatus, s.infra.mergeBody = http.StatusMethodNotAllowed, `{"message":"Please try again later.","url":"x"}`
	rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Forgejo refused the merge: Please try again later.") {
		t.Fatalf("a plain POST's refused merge = %d:\n%s", rec.Code, rec.Body.String())
	}
	s.infra.mu.Lock()
	attempts := len(s.infra.merges)
	s.infra.mu.Unlock()
	if attempts != mergeRetryTries+1 {
		t.Errorf("%d merge calls, want %d with the 405 retries", attempts, mergeRetryTries+1)
	}
	if run := s.c.runs["linkshort"]; run == nil || run.merge != "Please try again later." {
		t.Errorf("the refusal was not kept for the panel: %+v", run)
	}
}

// A 405 while Forgejo re-checks mergeability becomes a merge on a retry.
func TestMergeRetriesAfter405(t *testing.T) {
	s := newStatusRig(t)
	s.noMergePause()
	s.infra.set(openPull)
	s.infra.mergeStatuses = []int{http.StatusMethodNotAllowed, http.StatusOK}
	s.infra.mergeBody = `{"message":"Please try again later.","url":"x"}`
	rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("merge after a 405 = %d:\n%s", rec.Code, rec.Body.String())
	}
	s.infra.mu.Lock()
	attempts := len(s.infra.merges)
	s.infra.mu.Unlock()
	if attempts != 2 {
		t.Errorf("%d merge calls, want the 405 and the retry", attempts)
	}
	if run := s.c.runs["linkshort"]; run == nil || run.merge != "" {
		t.Errorf("a merged project keeps a refusal: %+v", run)
	}
}

// No render of the new project's panel asks for a password or offers a
// registration: the reconcile registers (ruling 219); the old route is gone.
func TestNewProjectPanelHasNoPassword(t *testing.T) {
	minted := factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort", Objects: "factory", Agents: "coder", File: true, GitToken: "g"}
	for _, setup := range []func(s *statusRig){
		func(s *statusRig) {},
		func(s *statusRig) { s.infra.set(openPull) },
		func(s *statusRig) { s.force.load("linkshort") },
		func(s *statusRig) { s.force.load("linkshort"); s.reconcileErr = "boom" },
		func(s *statusRig) { s.force.load("linkshort"); s.row(minted) },
		func(s *statusRig) { s.force.load("linkshort"); s.row(minted); s.main = true },
	} {
		s := newStatusRig(t)
		setup(s)
		req := httptest.NewRequest(http.MethodGet, "/projects/linkshort/status", nil)
		req.Header.Set("HX-Request", "true")
		for _, body := range []string{s.status(t), s.do(req, s.ck).Body.String()} {
			if strings.Contains(body, `type="password"`) || strings.Contains(body, "/register") || strings.Contains(body, "Create repository") {
				t.Errorf("the panel offers a registration:\n%s", body)
			}
		}
		if rec := s.do(post("/projects/linkshort/register", url.Values{"csrf": {s.csrf}, "password": {"pw"}}), s.ck); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST /projects/linkshort/register = %d", rec.Code)
		}
	}
}

// A button's request htmx sends swaps only a 2xx answer, so a refused
// click answers the panel with its reason; a poll never replaces the panel
// while a button's request runs.
func TestPanelButtonRefusalShowsOnPanel(t *testing.T) {
	s := newStatusRig(t)
	s.infra.set(openPull)
	page := s.status(t)
	for _, want := range []string{"Hover a step, or focus it, for what it does.", `hx-sync="closest section:replace"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	s.infra.set(nil)
	s.force.load("linkshort")
	hx := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := post(path, form)
		req.Header.Set("HX-Request", "true")
		return s.do(req, s.ck)
	}
	for _, c := range []struct {
		path string
		form url.Values
		want string
	}{
		{"/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}, "No open pull request from project-linkshort."},
	} {
		rec := hx(c.path, c.form)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.HasPrefix(body, `<section id="status"`) || !strings.Contains(body, `<p class="problem" role="alert">`+c.want+`</p>`) {
			t.Errorf("htmx POST %s = %d:\n%s", c.path, rec.Code, body)
		}
	}
	if rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck); rec.Code != http.StatusConflict {
		t.Errorf("a plain POST's refusal = %d, want 409", rec.Code)
	}
	s.infra.set(openPull)
	s.force.mu.Lock()
	s.force.add = nil
	s.force.mu.Unlock()
	s.infra.mergeStatus = http.StatusTeapot
	s.infra.mergeBody = "{"
	body := hx("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}).Body.String()
	if !strings.Contains(body, "Forgejo refused the merge: {") {
		t.Errorf("a refusal without a message:\n%s", body)
	}
}
