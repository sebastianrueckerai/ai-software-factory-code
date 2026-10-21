package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeKeyLiteLLM is LiteLLM's key endpoints as removal reads them: the list
// with every key's task_id metadata, and the delete.
type fakeKeyLiteLLM struct {
	mu      sync.Mutex
	keys    []liteKey
	deleted []string
}

func (l *fakeKeyLiteLLM) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk-master" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list"):
			json.NewEncoder(w).Encode(keyList{Keys: l.keys, TotalPages: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/key/delete":
			var body struct{ Keys []string }
			json.NewDecoder(r.Body).Decode(&body)
			l.deleted = append(l.deleted, body.Keys...)
			l.keys = slices.DeleteFunc(l.keys, func(k liteKey) bool { return slices.Contains(body.Keys, k.Token) })
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (l *fakeKeyLiteLLM) holds(token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.keys, func(k liteKey) bool { return k.Token == token })
}

// removalRig is a registered demo-app (register's own tests' fakes) whose
// file has left the configuration, ready for its removal: the GitHub
// export, the packages, both forks, the mirror rows, the cards and their
// LiteLLM keys are what its life left.
type removalRig struct {
	registerRig
	litellm *fakeKeyLiteLLM
	rem     *Remover
}

func newRemovalRig(t *testing.T) *removalRig {
	t.Helper()
	rig := newRegisterRig(t)
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("register: %v\n%s", err, rig.logs)
	}
	// the file went from the configuration, and factoryd loaded the new
	// version: the row keeps its tokens, its file flag is gone
	p := rig.store.m["demo-app"]
	p.File = false
	rig.store.m["demo-app"] = p
	rig.fj.mirrors = []map[string]any{{"remote_name": "github", "remote_address": "https://github.com/example/demo-app.git"}}
	rig.fj.packages = []map[string]string{
		{"type": "container", "name": "demo-app", "version": "1.0.0"},
		{"type": "container", "name": "demo-app", "version": "1.0.1"},
	}
	rig.fj.forks = map[string]bool{"factory-agent": true, "factory-projects": true}
	rig.store.mirrors = map[string]bool{"demo-app": true}
	rig.kube.labels = map[string]map[string]string{
		"demo-staging": {ProjectLabel: "demo-app"}, "demo-prod": {ProjectLabel: "demo-app"}}
	rig.store.tasks = []Task{
		{ID: "t-done-1", Repository: "demo-app/demo-app", Status: "done"},
		{ID: "t-done-2", Repository: "demo-app/demo-app", Status: "closed"},
	}
	rig.store.history = map[string]int{"events": 12, "ledger": 34, "audit": 5, "chat": 2}
	litellm := &fakeKeyLiteLLM{keys: []liteKey{
		{Token: "sk-done-1", Metadata: map[string]any{"task_id": "t-done-1"}},
		{Token: "sk-done-2", Metadata: map[string]any{"task_id": "t-done-2"}},
		{Token: "sk-other", Metadata: map[string]any{"task_id": "t-of-another-project"}},
	}}
	srv := litellm.server(t)
	rem := &Remover{Forgejo: rig.reg.Forgejo, Kube: rig.kube, Store: rig.store, OpenBao: rig.kv,
		LiteLLM:      &LiteLLMAdmin{BaseURL: srv.URL, MasterKey: "sk-master", HTTP: srv.Client()},
		ProjectsUser: "factory-projects"}
	return &removalRig{registerRig: *rig, litellm: litellm, rem: rem}
}

// The refusals of step 1, each naming the state and what to do, and none
// of them touching anything. The factory's own code project is refused
// like any other whose file is the configuration in force: its file is
// the configuration's repository itself (ruling 179 names it in no
// source).
func TestRemoveRefuses(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		file   bool // the configuration in force holds projects/demo-app.yaml
		open   string
		want   []string
		object string
	}{
		{name: "a file still in force", file: true,
			want: []string{"projects/demo-app.yaml is in the configuration in force", "remove the file from the configuration repository", "the hourly reconcile registers the project again"}},
		{name: "the factory's own project", object: "ai-software-factory-code",
			want: []string{"projects/ai-software-factory-code.yaml is in the configuration in force", "the hourly reconcile registers the project again"}},
		{name: "an open card", open: "t-open-1",
			want: []string{"1 card of demo-app/demo-app still open (t-open-1)", "finish or close them first"}},
	} {
		rig := newRemovalRig(t)
		writes, deletes := len(rig.fj.writes), len(rig.kube.deletes)
		if c.name != "the factory's own project" && c.file {
			p := rig.store.m["demo-app"]
			p.File = true
			rig.store.m["demo-app"] = p
		}
		if c.open != "" {
			rig.store.tasks = append(rig.store.tasks, Task{ID: c.open, Repository: "demo-app/demo-app", Status: "running"})
		}
		object := c.object
		if object == "" {
			object = "demo-app"
		} else {
			// the factory's own project's file is in the tree: the row
			// says so, as the store's join of files and rows does
			p := demoProject()
			p.Name, p.Repository = object, object+"/"+object
			p.File = true
			rig.store.m[object] = p
		}
		_, err := rig.rem.Remove(ctx, object, false)
		if err == nil {
			t.Fatalf("%s: removed", c.name)
		}
		for _, want := range c.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %q, wants %q", c.name, err, want)
			}
		}
		if n := len(rig.fj.writes); n != writes {
			t.Errorf("%s: wrote %v", c.name, rig.fj.writes[writes:])
		}
		if n := len(rig.kube.deletes); n != deletes {
			t.Errorf("%s: deleted %v", c.name, rig.kube.deletes[deletes:])
		}
		if _, ok, _ := rig.store.Project(ctx, "demo-app"); !ok {
			t.Errorf("%s: the row went", c.name)
		}
		if !rig.litellm.holds("sk-done-1") {
			t.Errorf("%s: the LiteLLM keys went", c.name)
		}
	}
}

// A registered project's removal, register's inverse: every step in the
// removal's fixed order, each naming its object and its result, and a
// second run after a clean one touches nothing and fails nothing.
func TestRemovesWhatRegisterMade(t *testing.T) {
	rig := newRemovalRig(t)
	ctx := context.Background()
	lines, err := rig.rem.Remove(ctx, "demo-app", false)
	if err != nil {
		t.Fatalf("remove: %v\n%s", err, strings.Join(lines, "\n"))
	}
	want := []string{
		"project demo-app: github export https://github.com/example/demo-app.git: deleted",
		"project demo-app: demo-prod flux kustomization: deleted",
		"project demo-app: demo-staging flux kustomization: deleted",
		"project demo-app: namespace demo-prod: deleted",
		"project demo-app: namespace demo-staging: deleted",
		"project demo-app: flux gitrepository: deleted",
		"project demo-app: flux secret demo-app-git: deleted",
		"project demo-app: packages: deleted 2",
		"project demo-app: repository demo-app/demo-app: deleted",
		"project demo-app: fork factory-agent/demo-app: deleted",
		"project demo-app: fork factory-projects/demo-app: deleted",
		"project demo-app: organisation demo-app: deleted",
		"project demo-app: user ci-demo-app: deleted",
		"project demo-app: tokens of factory-projects: deleted 1",
		"project demo-app: tokens of factory-merge: deleted 1",
		"project demo-app: tokens of factory-gate: deleted 1",
		"project demo-app: tokens of factory-chat: deleted 1",
		"project demo-app: litellm keys: deleted 2",
		"project demo-app: row: deleted",
		"project demo-app: project_mirrors rows: deleted 1",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// the cluster: the Kustomizations before their namespaces, then the
	// namespaces, the GitRepository and its secret
	if got := rig.kube.deletes; !slices.Equal(got, []string{
		"/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-prod/kustomizations/demo-app",
		"/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-staging/kustomizations/demo-app",
		"/api/v1/namespaces/demo-prod",
		"/api/v1/namespaces/demo-staging",
		"/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/demo-app",
		"/api/v1/namespaces/flux-system/secrets/demo-app-git",
	}) {
		t.Fatalf("deletes out of order: %v", got)
	}
	if rig.fj.repo || !rig.fj.orgGone || !rig.fj.ciUserGone || len(rig.fj.mirrors) != 0 || len(rig.fj.packages) != 0 ||
		rig.fj.forks["factory-agent"] || rig.fj.forks["factory-projects"] || rig.fj.fork != nil {
		t.Fatalf("forgejo state: repo %v org %v ci %v mirrors %v packages %v forks %v fork %v",
			rig.fj.repo, rig.fj.orgGone, rig.fj.ciUserGone, rig.fj.mirrors, rig.fj.packages, rig.fj.forks, rig.fj.fork)
	}
	for _, un := range [4]string{"factory-projects", "factory-merge", "factory-gate", "factory-chat"} {
		for id, tok := range rig.fj.tokens {
			if tok[0] == un {
				t.Errorf("token %d of %s (%s) lives", id, un, tok[1])
			}
		}
	}
	if rig.litellm.holds("sk-done-1") || rig.litellm.holds("sk-done-2") || !rig.litellm.holds("sk-other") {
		t.Fatal("the wrong LiteLLM keys went, or another project's did")
	}
	if slices.Contains(rig.litellm.deleted, "sk-other") {
		t.Fatal("another project's key deleted")
	}
	if _, ok, _ := rig.store.Project(ctx, "demo-app"); ok {
		t.Fatal("the row lives")
	}
	if rig.store.mirrors["demo-app"] {
		t.Fatal("the mirror rows live")
	}

	// a second run makes nothing anew: its only writes are the gone
	// checks' deletes
	writes := len(rig.fj.writes)
	lines, err = rig.rem.Remove(ctx, "demo-app", false)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if !strings.HasSuffix(line, ": "+string(Gone)) && !strings.Contains(line, ": no push mirror") {
			t.Errorf("second run: %s", line)
		}
	}
	for _, w := range rig.fj.writes[writes:] {
		if !strings.HasPrefix(w, "DELETE ") {
			t.Errorf("second run wrote %s", w)
		}
	}
}

// A failure in step 4 stops the run there; a second run finishes the
// rest, the steps before reporting what a former run already took.
func TestRemoveFinishesAfterAStep4Failure(t *testing.T) {
	rig := newRemovalRig(t)
	ctx := context.Background()
	rig.fj.failDelete = "/api/v1/orgs/demo-app"
	lines, err := rig.rem.Remove(ctx, "demo-app", false)
	if err == nil || !strings.Contains(err.Error(), "organisation demo-app") {
		t.Fatalf("err = %v\n%s", err, strings.Join(lines, "\n"))
	}
	if slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "user ci-demo-app") }) {
		t.Fatalf("the steps after the failure ran:\n%s", strings.Join(lines, "\n"))
	}
	// step 4 stopped at the organisation: the repository, the forks and
	// the packages went, the organisation did not, and nothing after it
	if rig.fj.repo || rig.fj.orgGone || rig.fj.ciUserGone {
		t.Fatalf("partial state: repo %v orgGone %v ciUserGone %v", rig.fj.repo, rig.fj.orgGone, rig.fj.ciUserGone)
	}
	if _, ok, _ := rig.store.Project(ctx, "demo-app"); !ok {
		t.Fatal("the row went though step 4 failed")
	}
	if !rig.litellm.holds("sk-done-1") || rig.litellm.deleted != nil {
		t.Fatal("the LiteLLM keys went though step 4 failed")
	}

	rig.fj.failDelete = ""
	rig.logs.Reset()
	lines, err = rig.rem.Remove(ctx, "demo-app", false)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		"project demo-app: github export: no change: no push mirror",
		"project demo-app: flux gitrepository: already gone",
		"project demo-app: organisation demo-app: deleted",
		"project demo-app: user ci-demo-app: deleted",
		"project demo-app: tokens of factory-projects: deleted 1",
		"project demo-app: litellm keys: deleted 2",
		"project demo-app: row: deleted",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("second run lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if rig.litellm.holds("sk-done-1") || !rig.litellm.holds("sk-other") {
		t.Fatal("the second run deleted the wrong keys")
	}
	if _, ok, _ := rig.store.Project(ctx, "demo-app"); ok {
		t.Fatal("the row lives after the second run")
	}
}

// The fake's failDelete answers 500 for every delete the removal makes,
// not only the five objects of step 4: a push mirror (step 2) and a token
// (step 4's tail) fail their step, the steps after them stay, and a second
// run finishes the rest.
func TestRemoveFinishesAfterEarlierDeleteFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    string // the path whose delete answers 500
		object  string // the step the failure stops
		cluster int    // the cluster deletes the first run made
	}{
		{name: "a push mirror delete", fail: "/push_mirrors/", object: "github export https://github.com/example/demo-app.git", cluster: 0},
		{name: "a token delete", fail: "/users/factory-projects/tokens/", object: "tokens of factory-projects", cluster: 6},
	} {
		rig := newRemovalRig(t)
		ctx := context.Background()
		rig.fj.failDelete = tc.fail
		lines, err := rig.rem.Remove(ctx, "demo-app", false)
		if err == nil || !strings.Contains(err.Error(), tc.object) {
			t.Fatalf("%s: err = %v\n%s", tc.name, err, strings.Join(lines, "\n"))
		}
		if n := len(rig.kube.deletes); n != tc.cluster {
			t.Errorf("%s: the first run made %d cluster deletes, want %d", tc.name, n, tc.cluster)
		}
		if _, ok, _ := rig.store.Project(ctx, "demo-app"); !ok {
			t.Errorf("%s: the row went though the run failed", tc.name)
		}
		if !rig.litellm.holds("sk-done-1") {
			t.Errorf("%s: the LiteLLM keys went though the run failed", tc.name)
		}

		rig.fj.failDelete = ""
		lines, err = rig.rem.Remove(ctx, "demo-app", false)
		if err != nil {
			t.Fatalf("%s: second run: %v\n%s", tc.name, err, strings.Join(lines, "\n"))
		}
		if _, ok, _ := rig.store.Project(ctx, "demo-app"); ok {
			t.Errorf("%s: the row lives after the second run", tc.name)
		}
		if rig.litellm.holds("sk-done-1") || rig.litellm.holds("sk-done-2") || !rig.litellm.holds("sk-other") {
			t.Errorf("%s: the second run deleted the wrong keys", tc.name)
		}
		if rig.fj.repo || !rig.fj.orgGone || !rig.fj.ciUserGone || len(rig.fj.mirrors) != 0 || len(rig.fj.packages) != 0 {
			t.Errorf("%s: forgejo state after the second run: repo %v orgGone %v ciUserGone %v mirrors %v packages %v",
				tc.name, rig.fj.repo, rig.fj.orgGone, rig.fj.ciUserGone, rig.fj.mirrors, rig.fj.packages)
		}
	}
}

// The removal keeps the project's history: its tasks, events, ledger,
// audit and chat rows stay, only the projects row and the mirror rows go.
func TestRemoveKeepsTheHistory(t *testing.T) {
	rig := newRemovalRig(t)
	ctx := context.Background()
	before := slices.Clone(rig.store.tasks)
	history := map[string]int{}
	for k, v := range rig.store.history {
		history[k] = v
	}
	if _, err := rig.rem.Remove(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	got, err := rig.store.ProjectTasks(ctx, "demo-app/demo-app")
	if err != nil || len(got) != len(before) {
		t.Fatalf("the tasks after the removal: %v, %v", got, err)
	}
	for i := range before {
		if got[i].ID != before[i].ID || got[i].Status != before[i].Status {
			t.Fatalf("task %d after the removal: %+v, want %+v", i, got[i], before[i])
		}
	}
	if !mapsEqual(rig.store.history, history) {
		t.Fatalf("the history rows after the removal: %v, want %v", rig.store.history, history)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// --dry-run lists the steps and their targets and acts on nothing.
func TestRemoveDryRun(t *testing.T) {
	rig := newRemovalRig(t)
	reads, writes, deletes := rig.kv.reads, len(rig.fj.writes), len(rig.kube.deletes)
	lines, err := rig.rem.Remove(context.Background(), "demo-app", true)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		"project demo-app: github export https://github.com/example/demo-app.git: would delete mirror github",
		"project demo-app: demo-prod flux kustomization: would delete /apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-prod/kustomizations/demo-app",
		"project demo-app: namespace demo-prod: would delete /api/v1/namespaces/demo-prod",
		"project demo-app: flux gitrepository: would delete /apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/demo-app",
		"project demo-app: packages: would delete demo-app's packages",
		"project demo-app: repository demo-app/demo-app: would delete",
		"project demo-app: fork factory-agent/demo-app: would delete",
		"project demo-app: fork factory-projects/demo-app: would delete",
		"project demo-app: organisation demo-app: would delete",
		"project demo-app: user ci-demo-app: would delete",
		"project demo-app: tokens of factory-projects: would delete git-demo-app",
		"project demo-app: tokens of factory-merge: would delete project-demo-app",
		"project demo-app: tokens of factory-gate: would delete project-demo-app",
		"project demo-app: tokens of factory-chat: would delete project-demo-app",
		"project demo-app: litellm keys: would delete the keys of 2 tasks",
		"project demo-app: row: would delete",
		"project demo-app: project_mirrors rows: would delete",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("dry run lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if len(rig.fj.writes) != writes || len(rig.kube.deletes) != deletes || !rig.fj.repo || rig.fj.orgGone {
		t.Fatalf("dry run acted: writes %v deletes %v repo %v orgGone %v", rig.fj.writes[writes:], rig.kube.deletes[deletes:], rig.fj.repo, rig.fj.orgGone)
	}
	if rig.litellm.deleted != nil {
		t.Fatalf("dry run deleted LiteLLM keys %v", rig.litellm.deleted)
	}
	if _, ok, _ := rig.store.Project(context.Background(), "demo-app"); !ok {
		t.Fatal("dry run deleted the row")
	}
	// no admin's login was read: a dry run needs none
	if n := rig.kv.reads - reads; n != 0 {
		t.Fatalf("dry run read OpenBao %d times", n)
	}
	// the ordering note: the Kustomization target names the step's own
	// object, the dry run names it before its namespace
	ks := slices.Index(lines, "project demo-app: demo-staging flux kustomization: would delete /apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-staging/kustomizations/demo-app")
	ns := slices.Index(lines, "project demo-app: namespace demo-staging: would delete /api/v1/namespaces/demo-staging")
	if ks < 0 || ns < 0 || ks > ns {
		t.Fatalf("the dry run's order: kustomization at %d, namespace at %d:\n%s", ks, ns, strings.Join(lines, "\n"))
	}
}

// LiteLLM's delete takes the key hashes of exactly the tasks named, and a
// task whose keys are gone already deletes none.
func TestDeleteTaskKeys(t *testing.T) {
	l := &fakeKeyLiteLLM{keys: []liteKey{
		{Token: "sk-a", Metadata: map[string]any{"task_id": "t-1"}},
		{Token: "sk-b", Metadata: map[string]any{"task_id": "t-1"}},
		{Token: "sk-c", Metadata: map[string]any{"task_id": "t-2"}},
	}}
	srv := l.server(t)
	admin := &LiteLLMAdmin{BaseURL: srv.URL, MasterKey: "sk-master", HTTP: srv.Client()}
	n, err := admin.DeleteTaskKeys(context.Background(), []string{"t-1"})
	if err != nil || n != 2 || l.holds("sk-a") || l.holds("sk-b") || !l.holds("sk-c") {
		t.Fatalf("= %d, %v; a %v b %v c %v", n, err, l.holds("sk-a"), l.holds("sk-b"), l.holds("sk-c"))
	}
	if n, err := admin.DeleteTaskKeys(context.Background(), []string{"t-1"}); err != nil || n != 0 {
		t.Fatalf("a second delete of t-1 = %d, %v", n, err)
	}
	if _, err := admin.DeleteTaskKeys(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
