package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
		`in Git</a>`, `href="https://git.example/sf/infra/src/branch/main/factory/projects"`,
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

// The settings edits and the registration form are gone: the file is the
// one place.
func TestProjectsHaveNoEdits(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	for _, path := range []string{"/projects", "/projects/demo-app/settings"} {
		if rec := r.do(post(path, url.Values{"csrf": {s.csrf}, "merge_rule": {"auto"}}), ck); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want no such route", path, rec.Code)
		}
	}
}
