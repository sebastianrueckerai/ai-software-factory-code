package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type fakeProjects struct {
	rows       []factoryd.Project
	reconciled []factoryd.Project
	calls      []string
}

func (f *fakeProjects) Projects(context.Context) ([]factoryd.Project, error) { return f.rows, nil }

func (f *fakeProjects) Project(_ context.Context, name string) (factoryd.Project, bool, error) {
	for _, p := range f.rows {
		if p.Name == name {
			return p, true, nil
		}
	}
	return factoryd.Project{}, false, nil
}

func (f *fakeProjects) CreateProject(_ context.Context, p factoryd.Project) error {
	for _, o := range f.rows {
		if o.Name == p.Name || o.Repository == p.Repository {
			return factoryd.ErrProjectExists
		}
	}
	f.calls = append(f.calls, "create "+p.Name)
	f.rows = append(f.rows, p)
	return nil
}

func (f *fakeProjects) SetProjectSettings(_ context.Context, name, rule string, notify []string, q policy.Quota) error {
	for i, p := range f.rows {
		if p.Name == name {
			f.rows[i].MergeRule, f.rows[i].Notify, f.rows[i].Quota = rule, notify, q
			f.calls = append(f.calls, "settings "+name)
			return nil
		}
	}
	return factoryd.ErrNotFound
}

func (f *fakeProjects) reconcile(_ context.Context, p factoryd.Project) error {
	f.reconciled = append(f.reconciled, p)
	return nil
}

func projectRows() []factoryd.Project {
	return []factoryd.Project{
		{Name: "ai-software-factory-code", Repository: "sf/code", MergeRule: "human", Namespaces: []string{"factory"}},
		{Name: "demo-app", Repository: "sf/demo-app", MergeRule: "auto", Managed: true, Namespaces: []string{"demo-staging", "demo-prod"},
			Environments: []factoryd.Environment{{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"}},
			Quota:        policy.Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"}, ReadTokenHash: "h", CIToken: "secret-ci-token"},
	}
}

func TestProjectsPage(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.projects.rows = projectRows()
	r.resources.envs = []cloudres.EnvelopeRecord{{Project: "sf/demo-app", SetBy: "sebastian",
		Envelope: policy.Envelope{MonthlyEUR: 30, ServerTypes: []string{"cx23"}, MaxCount: 3, MaxLeaseHours: 8,
			Standing: policy.Standing{MonthlyEUR: 12, ServerTypes: []string{"cx23"}, MaxCount: 1}},
		StandingUse: cloudres.StandingUse{Count: 1, MonthlyEUR: 5.99}}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ck)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects = %d: %s", rec.Code, body)
	}
	for _, want := range []string{
		`<h3 id="project-demo-app">demo-app</h3>`, "sf/demo-app", "Merge rule: auto",
		"Notify: sebastian (the policy's approvers)", "Namespaces: demo-staging, demo-prod",
		"Quota per namespace: 2 CPU, 4Gi memory, 10Gi storage",
		"Task block: 30.00 EUR a month, types cx23, 3 servers at once, leases up to 8 h",
		"Standing block: 12.00 EUR a month, types cx23, 1 servers",
		"Standing now: 1 servers, 5.9900 EUR a month.",
		"demo-staging: deploy/staging at demo-staging.aisoftwarefactory.world",
		"hostname: demo-staging.aisoftwarefactory.world", // the gateway YAML
		"CI token: minted", `<h3 id="project-ai-software-factory-code">ai-software-factory-code</h3>`,
		"Unmanaged: its objects are the infra repository's.",
		// the code repository has no envelope row: the policy's default
		"Task block: 10.00 EUR a month, types cx23, 2 servers at once, leases up to 4 h",
		`action="/projects/demo-app/settings"`, `action="/projects"`, `name="csrf" value="` + s.csrf + `"`,
		`<a href="/projects">Projects</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/projects lacks %q", want)
		}
	}
	if strings.Contains(body, "secret-ci-token") || strings.Contains(body, ">h<") {
		t.Fatal("/projects shows a token")
	}
}

func TestRegisterProjectFromTheConsole(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	form := url.Values{"csrf": {s.csrf}, "name": {"demo-app"}, "repository": {"sf/demo-app"}, "merge_rule": {"auto"},
		"namespaces": {"demo-staging, demo-prod"}, "environments": {"demo-staging=deploy/staging\r\ndemo-prod=deploy/prod"},
		"hostnames": {"demo-staging=demo-staging.aisoftwarefactory.world\ndemo-prod=demo.aisoftwarefactory.world"},
		"secrets": {"demo-write-token/token"}}
	rec := r.do(post("/projects", form), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects?registered=demo-app" {
		t.Fatalf("register = %d %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	want := factoryd.Project{Name: "demo-app", Repository: "sf/demo-app", MergeRule: "auto", Managed: true,
		Namespaces: []string{"demo-staging", "demo-prod"},
		Environments: []factoryd.Environment{
			{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"},
			{Namespace: "demo-prod", Path: "deploy/prod", Hostname: "demo.aisoftwarefactory.world"}},
		Quota:   policy.DefaultProject.Quota,
		Secrets: []string{"demo-write-token/token"}}
	if len(r.projects.rows) != 1 || !reflect.DeepEqual(r.projects.rows[0], want) || len(r.projects.reconciled) != 1 {
		t.Fatalf("registered %+v, reconciled %d", r.projects.rows, len(r.projects.reconciled))
	}
	if rec := r.do(post("/projects", form), ck); rec.Code != http.StatusConflict {
		t.Fatalf("a second registration = %d", rec.Code)
	}
	for name, change := range map[string][2]string{
		"bad name":       {"name", "Demo"},
		"system ns":      {"namespaces", "kube-system"},
		"env outside":    {"environments", "other=deploy/x"},
		"bad env":        {"environments", "demo-staging"},
		"merge rule":     {"merge_rule", "sometimes"},
		"host of no env": {"hostnames", "nope=x.example.com"},
	} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("name", "other")
		f.Set("repository", "sf/other")
		f.Set(change[0], change[1])
		n := len(r.projects.calls)
		if rec := r.do(post("/projects", f), ck); rec.Code != http.StatusBadRequest || len(r.projects.calls) != n {
			t.Errorf("%s: %d, want 400 and nothing stored: %s", name, rec.Code, rec.Body)
		}
	}
}

func TestProjectSettingsReconcile(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.projects.rows = projectRows()
	form := url.Values{"csrf": {s.csrf}, "merge_rule": {"human"}, "notify": {"alice bob"},
		"quota_cpu": {"1500m"}, "quota_memory": {"3Gi"}, "quota_storage": {"20Gi"}}
	rec := r.do(post("/projects/demo-app/settings", form), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects?saved=demo-app" {
		t.Fatalf("settings = %d: %s", rec.Code, rec.Body)
	}
	got := r.projects.rows[1]
	if got.MergeRule != "human" || !reflect.DeepEqual(got.Notify, []string{"alice", "bob"}) ||
		got.Quota != (policy.Quota{CPU: "1500m", Memory: "3Gi", Storage: "20Gi"}) {
		t.Fatalf("saved %+v", got)
	}
	// the ResourceQuota follows at once
	if len(r.projects.reconciled) != 1 || r.projects.reconciled[0].Quota.CPU != "1500m" {
		t.Fatalf("reconciled %+v", r.projects.reconciled)
	}
	for name, change := range map[string][2]string{"quota": {"quota_cpu", "lots"}, "rule": {"merge_rule", "x"}, "notify": {"notify", "a;b"}} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set(change[0], change[1])
		if rec := r.do(post("/projects/demo-app/settings", f), ck); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if rec := r.do(post("/projects/nope/settings", form), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project = %d", rec.Code)
	}
}

// Ruling 128: registration and settings are spend; other writers see the
// page without forms and get 403.
func TestProjectsNeedAnApprover(t *testing.T) {
	r := newRig(t)
	r.fg.user, r.writers.users = "alice", []string{"sebastian", "alice"}
	ck, s := r.loggedIn(t)
	r.projects.rows = projectRows()
	page := r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ck).Body.String()
	if strings.Contains(page, `action="/projects"`) || strings.Contains(page, "/settings\"") || !strings.Contains(page, "demo-app") {
		t.Fatal("a non-approver gets the forms")
	}
	for _, path := range []string{"/projects", "/projects/demo-app/settings"} {
		if rec := r.do(post(path, url.Values{"csrf": {s.csrf}, "name": {"x"}}), ck); rec.Code != http.StatusForbidden {
			t.Errorf("%s by a non-approver = %d", path, rec.Code)
		}
	}
	if len(r.projects.calls) != 0 {
		t.Fatalf("calls %v", r.projects.calls)
	}
}
