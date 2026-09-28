package factoryd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// forgejoState is a scripted Forgejo holding one organization's
// repository and what registration writes to it.
type forgejoState struct {
	mu         sync.Mutex
	repos      map[string]bool // full name -> exists
	fork       map[string]any
	hooks      []map[string]any
	collabs    map[string]string
	protection map[string]any
	labels     []string
	secrets    map[string]string
	writes     []string // "METHOD path by <auth>"
}

func newForgejoState() *forgejoState {
	return &forgejoState{repos: map[string]bool{}, collabs: map[string]string{}, secrets: map[string]string{}}
}

func (s *forgejoState) server(t *testing.T) *httptest.Server {
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
		page := r.URL.Query().Get("page")
		list := func(v any) {
			if page != "" && page != "1" {
				io.WriteString(w, "[]")
				return
			}
			json.NewEncoder(w).Encode(v)
		}
		const repo = "/api/v1/repos/sf/demo-app"
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == repo:
			if !s.repos["sf/demo-app"] {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, `{"permissions":{"admin":true}}`)
		case r.Method == http.MethodPost && p == "/api/v1/orgs/sf/repos":
			if body["name"] != "demo-app" || body["private"] != true {
				t.Errorf("create repo %v", body)
			}
			s.repos["sf/demo-app"] = true
			w.WriteHeader(http.StatusCreated)
		case p == repo+"/hooks" && r.Method == http.MethodGet:
			list(s.hooks)
		case p == repo+"/hooks" && r.Method == http.MethodPost:
			body["id"] = 1
			delete(body["config"].(map[string]any), "secret")
			s.hooks = append(s.hooks, body)
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(p, repo+"/collaborators/") && strings.HasSuffix(p, "/permission"):
			user := strings.TrimSuffix(strings.TrimPrefix(p, repo+"/collaborators/"), "/permission")
			if s.collabs[user] == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"permission":%q}`, s.collabs[user])
		case strings.HasPrefix(p, repo+"/collaborators/") && r.Method == http.MethodPut:
			s.collabs[strings.TrimPrefix(p, repo+"/collaborators/")] = body["permission"].(string)
			w.WriteHeader(http.StatusNoContent)
		case p == repo+"/branch_protections/main" && r.Method == http.MethodGet:
			if s.protection == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(s.protection)
		case p == repo+"/branch_protections" && r.Method == http.MethodPost:
			s.protection = body
			w.WriteHeader(http.StatusCreated)
		case p == repo+"/branch_protections/main" && r.Method == http.MethodPatch:
			s.protection = body
			json.NewEncoder(w).Encode(body)
		case p == "/api/v1/repos/factory-agent/demo-app" && r.Method == http.MethodGet:
			if s.fork == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(s.fork)
		case p == repo+"/forks" && r.Method == http.MethodPost:
			s.fork = map[string]any{"fork": true, "private": true, "has_actions": false, "parent": map[string]string{"full_name": "sf/demo-app"}}
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(s.fork)
		case p == repo+"/labels" && r.Method == http.MethodGet:
			var out []map[string]any
			for i, l := range s.labels {
				out = append(out, map[string]any{"id": i + 1, "name": l})
			}
			list(out)
		case p == repo+"/labels" && r.Method == http.MethodPost:
			s.labels = append(s.labels, body["name"].(string))
			w.WriteHeader(http.StatusCreated)
		case p == repo+"/actions/secrets" && r.Method == http.MethodGet:
			var out []map[string]string
			for _, n := range slices.Sorted(func(yield func(string) bool) {
				for k := range s.secrets {
					if !yield(k) {
						return
					}
				}
			}) {
				out = append(out, map[string]string{"name": n})
			}
			list(out)
		case strings.HasPrefix(p, repo+"/actions/secrets/") && r.Method == http.MethodPut:
			s.secrets[strings.TrimPrefix(p, repo+"/actions/secrets/")] = body["data"].(string)
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeProjectKube keeps applied objects by path; an apply of the same
// object changes nothing.
type fakeProjectKube struct {
	mu      sync.Mutex
	objects map[string]string
	secrets map[string]map[string]string
	deploys map[string][]DeploymentState
	changes []string
}

func newFakeProjectKube() *fakeProjectKube {
	pull := `{"auths":{"git.aisoftwarefactory.world":{"username":"sebastian","password":"registry-pat","auth":"x"}}}`
	return &fakeProjectKube{objects: map[string]string{},
		secrets: map[string]map[string]string{"factory/forgejo-registry": {".dockerconfigjson": pull}}}
}

func (k *fakeProjectKube) Apply(_ context.Context, path string, obj map[string]any) (Change, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, _ := json.Marshal(obj)
	old, ok := k.objects[path]
	k.objects[path] = string(b)
	c := Created
	switch {
	case ok && old == string(b):
		return Unchanged, nil
	case ok:
		c = Updated
	}
	k.changes = append(k.changes, string(c)+" "+path)
	if strings.Contains(path, "/secrets/") {
		ns := strings.Split(path, "/")[4]
		name := path[strings.LastIndex(path, "/")+1:]
		data := map[string]string{}
		for key, v := range obj["data"].(map[string]string) {
			raw, _ := base64.StdEncoding.DecodeString(v)
			data[key] = string(raw)
		}
		k.secrets[ns+"/"+name] = data
	}
	return c, nil
}

func (k *fakeProjectKube) GetSecretData(_ context.Context, ns, name string) (map[string]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d, ok := k.secrets[ns+"/"+name]; ok {
		return d, nil
	}
	return nil, ErrNotFound
}

func (k *fakeProjectKube) Deployments(_ context.Context, ns string) ([]DeploymentState, error) {
	return k.deploys[ns], nil
}

func (k *fakeProjectKube) object(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(k.objects[path]), &out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

type memProjects struct {
	mu sync.Mutex
	m  map[string]Project
}

func (s *memProjects) Projects(context.Context) ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Project
	for _, p := range s.m {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Project) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *memProjects) Project(_ context.Context, name string) (Project, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[name]
	return p, ok, nil
}

func (s *memProjects) CreateProject(_ context.Context, p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.m {
		if o.Name == p.Name || o.Repository == p.Repository {
			return ErrProjectExists
		}
	}
	s.m[p.Name] = p
	return nil
}

func (s *memProjects) SetProjectSettings(_ context.Context, name, rule string, notify []string, q policy.Quota) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[name]
	if !ok {
		return ErrNotFound
	}
	p.MergeRule, p.Notify, p.Quota = rule, notify, q
	s.m[name] = p
	return nil
}

func (s *memProjects) SetProjectTokens(_ context.Context, name string, t ProjectTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[name]
	if t.ReadTokenHash != "" {
		p.ReadTokenHash = t.ReadTokenHash
	}
	if t.CIToken != "" {
		p.CIToken = t.CIToken
	}
	if t.PullToken != "" {
		p.PullToken = t.PullToken
	}
	s.m[name] = p
	return nil
}

type registerRig struct {
	fj    *forgejoState
	kube  *fakeProjectKube
	store *memProjects
	reg   *Registrar
	logs  *bytes.Buffer
}

func newRegisterRig(t *testing.T) *registerRig {
	fj := newForgejoState()
	srv := fj.server(t)
	kube := newFakeProjectKube()
	store := &memProjects{m: map[string]Project{}}
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", ProjectsToken: "projects-tok", HTTP: srv.Client()}
	reg := &Registrar{Forgejo: f, Kube: kube, Store: store,
		Policy:     func() policy.Document { return policy.Document{Approvers: []string{"sebastian"}} },
		WebhookURL: "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo", WebhookSecret: "hook-secret",
		FactoryNamespace: "factory", PullSecret: "forgejo-registry", GitURL: "https://git.aisoftwarefactory.world",
		RegistryHost: "git.aisoftwarefactory.world", MergeUser: "factory-merge", CIUser: "factory-ci", ProjectsUser: "factory-projects"}
	return &registerRig{fj: fj, kube: kube, store: store, reg: reg, logs: captureLog(t)}
}

// Registration against a Forgejo and a cluster that have nothing yet
// creates every object once; the second run changes nothing and says so.
func TestRegistrationIsIdempotent(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p := demoProject()
	p.CIToken, p.PullToken = "ci-tok", "pull-tok"
	rig.store.m[p.Name] = p
	if err := rig.reg.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	first := rig.logs.String()
	for _, want := range []string{
		"project demo-app: repository sf/demo-app: created", "project demo-app: webhook: created",
		"project demo-app: collaborator factory-agent: created", "project demo-app: collaborator factory-merge: created",
		"project demo-app: protection main: created", "project demo-app: fork factory-agent/demo-app: ready",
		"project demo-app: actions secret REGISTRY_USER: created", "project demo-app: actions secret REGISTRY_PASSWORD: created",
		"project demo-app: actions secret FACTORY_READ_TOKEN: created",
		"project demo-app: namespace demo-staging: created", "project demo-app: demo-staging rolebinding factoryd: created",
		"project demo-app: demo-staging resourcequota: created", "project demo-app: demo-prod limitrange: created",
		"project demo-app: demo-prod serviceaccount flux: created", "project demo-app: demo-prod rolebinding flux: created",
		"project demo-app: demo-staging secret forgejo-registry: created", "project demo-app: demo-prod secret demo-write-token/token: created",
		"project demo-app: flux gitrepository: created", "project demo-app: demo-staging flux kustomization: created",
		"project demo-app: demo-prod flux kustomization: created",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first run lacks %q", want)
		}
	}
	writes := len(rig.fj.writes)
	kubeChanges := len(rig.kube.changes)
	stored := rig.store.m[p.Name]
	demoToken := rig.kube.secrets["demo-staging/demo-write-token"]["token"]

	rig.logs.Reset()
	if err := rig.reg.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	second := rig.logs.String()
	for _, line := range strings.Split(strings.TrimSpace(second), "\n") {
		if !strings.HasSuffix(line, ": no change") && !strings.HasSuffix(line, ": ready") && !strings.HasSuffix(line, ": ensured") {
			t.Errorf("second run changed something: %s", line)
		}
	}
	if strings.Count(second, "\n") != strings.Count(first, "\n") {
		t.Errorf("second run logged %d lines, the first %d", strings.Count(second, "\n"), strings.Count(first, "\n"))
	}
	if len(rig.fj.writes) != writes || len(rig.kube.changes) != kubeChanges {
		t.Fatalf("second run wrote: forgejo %v, kube %v", rig.fj.writes[writes:], rig.kube.changes[kubeChanges:])
	}
	if rig.store.m[p.Name].ReadTokenHash != stored.ReadTokenHash || rig.kube.secrets["demo-staging/demo-write-token"]["token"] != demoToken {
		t.Fatal("second run replaced a token")
	}
}

// What the first run wrote, object by object.
func TestRegistrationObjects(t *testing.T) {
	rig := newRegisterRig(t)
	p := demoProject()
	p.CIToken, p.PullToken = "ci-tok", "pull-tok"
	rig.store.m[p.Name] = p
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	fj := rig.fj
	// Actions secrets with the project's CI token; everything else of the
	// repository with the projects token; the fork and labels as the bot
	for _, w := range fj.writes {
		switch {
		case strings.Contains(w, "/actions/secrets/"):
			if !strings.HasSuffix(w, "by token ci-tok") {
				t.Errorf("%s: want the CI token", w)
			}
		case strings.Contains(w, "/forks") || strings.Contains(w, "/labels"):
			if !strings.HasSuffix(w, "by basic factory-agent") {
				t.Errorf("%s: want the bot", w)
			}
		case !strings.HasSuffix(w, "by token projects-tok"):
			t.Errorf("%s: want the projects token", w)
		}
	}
	if fj.secrets["REGISTRY_USER"] != "sebastian" || fj.secrets["REGISTRY_PASSWORD"] != "registry-pat" {
		t.Errorf("registry secrets = %q / %q", fj.secrets["REGISTRY_USER"], fj.secrets["REGISTRY_PASSWORD"])
	}
	if tok := fj.secrets["FACTORY_READ_TOKEN"]; !rig.store.m["demo-app"].ReadTokenMatches(tok) {
		t.Error("the stored hash does not match the FACTORY_READ_TOKEN set")
	}
	hook := fj.hooks[0]
	if hook["type"] != "forgejo" || hook["active"] != true || hook["config"].(map[string]any)["url"] != rig.reg.WebhookURL ||
		hook["config"].(map[string]any)["content_type"] != "json" || len(hook["events"].([]any)) != len(ProjectHookEvents) {
		t.Errorf("hook = %v", hook)
	}
	if fj.collabs["factory-agent"] != "write" || fj.collabs["factory-merge"] != "write" {
		t.Errorf("collaborators = %v", fj.collabs)
	}
	// auto: no required approval; the push whitelist off for the job token
	want := map[string]any{"rule_name": "main", "enable_push": true, "enable_push_whitelist": false, "push_whitelist_usernames": []any{},
		"enable_merge_whitelist": true, "merge_whitelist_usernames": []any{"sebastian", "factory-merge"},
		"enable_approvals_whitelist": true, "approvals_whitelist_username": []any{"sebastian"}, "required_approvals": float64(0),
		"apply_to_admins": true, "block_on_rejected_reviews": true, "dismiss_stale_approvals": true, "enable_status_check": false,
		"protected_file_patterns": "", "unprotected_file_patterns": ""}
	if !reflect.DeepEqual(fj.protection, want) {
		t.Errorf("protection = %v", fj.protection)
	}
	if len(fj.labels) != len(BootLabels) {
		t.Errorf("labels = %v", fj.labels)
	}

	k := rig.kube
	ns := k.object(t, "/api/v1/namespaces/demo-staging")
	if got := ns["metadata"].(map[string]any)["labels"]; !reflect.DeepEqual(got, map[string]any{"factory/project": "demo-app", "pod-security.kubernetes.io/enforce": "restricted"}) {
		t.Errorf("namespace labels = %v", got)
	}
	quota := k.object(t, "/api/v1/namespaces/demo-prod/resourcequotas/project")
	if got := quota["spec"].(map[string]any)["hard"]; !reflect.DeepEqual(got, map[string]any{"requests.cpu": "2", "limits.cpu": "2",
		"requests.memory": "4Gi", "limits.memory": "4Gi", "requests.storage": "10Gi"}) {
		t.Errorf("quota = %v", got)
	}
	flux := k.object(t, "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-staging/rolebindings/flux")
	if flux["roleRef"].(map[string]any)["name"] != "factory-project-deployer" ||
		!reflect.DeepEqual(flux["subjects"], []any{map[string]any{"kind": "ServiceAccount", "name": "flux", "namespace": "demo-staging"}}) {
		t.Errorf("flux binding = %v", flux)
	}
	own := k.object(t, "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-staging/rolebindings/factoryd")
	if own["roleRef"].(map[string]any)["name"] != "factoryd-project" ||
		!reflect.DeepEqual(own["subjects"], []any{map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": "factory"}}) {
		t.Errorf("factoryd binding = %v", own)
	}
	src := k.object(t, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/demo-app")
	if !reflect.DeepEqual(src["spec"], map[string]any{"interval": "1m", "url": "https://git.aisoftwarefactory.world/sf/demo-app",
		"ref": map[string]any{"branch": "main"}, "secretRef": map[string]any{"name": "forgejo-pat"}}) {
		t.Errorf("gitrepository = %v", src["spec"])
	}
	ks := k.object(t, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-prod/kustomizations/demo-app")
	if !reflect.DeepEqual(ks["spec"], map[string]any{"interval": "1m", "path": "./deploy/prod", "prune": true,
		"sourceRef":          map[string]any{"kind": "GitRepository", "name": "demo-app", "namespace": "flux-system"},
		"serviceAccountName": "flux", "targetNamespace": "demo-prod"}) {
		t.Errorf("kustomization = %v", ks["spec"])
	}
	pull := k.secrets["demo-prod/forgejo-registry"][".dockerconfigjson"]
	if pull != `{"auths":{"git.aisoftwarefactory.world":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("factory-ci:pull-tok"))+`","password":"pull-tok","username":"factory-ci"}}}` {
		t.Errorf("pull secret = %s", pull)
	}
	if tok := k.secrets["demo-staging/demo-write-token"]["token"]; len(tok) != 64 || tok == k.secrets["demo-prod/demo-write-token"]["token"] {
		t.Errorf("generated secrets: %q, %q", tok, k.secrets["demo-prod/demo-write-token"]["token"])
	}
}

// A merge rule change reaches main's protection; a quota change the
// ResourceQuota; the rest stays.
func TestRegistrationFollowsSettings(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p := demoProject()
	rig.store.m[p.Name] = p
	rig.reg.Reconcile(ctx, p)
	rig.store.SetProjectSettings(ctx, p.Name, "human", nil, policy.Quota{CPU: "1", Memory: "2Gi", Storage: "5Gi"})
	p = rig.store.m[p.Name]
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, p); err != nil {
		t.Fatal(err)
	}
	got := rig.logs.String()
	for _, want := range []string{"protection main: updated", "demo-staging resourcequota: updated", "demo-prod resourcequota: updated",
		"demo-staging limitrange: no change", "webhook: no change"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if rig.fj.protection["required_approvals"] != float64(1) {
		t.Errorf("human: required approvals = %v", rig.fj.protection["required_approvals"])
	}
	// no CI token yet: the secrets wait, nothing else does
	if !strings.Contains(got, "actions secrets: waiting: no CI token yet") || len(rig.fj.secrets) != 0 {
		t.Errorf("secrets without a CI token: %v", rig.fj.secrets)
	}
}

func TestRegistrationSkipsUnmanaged(t *testing.T) {
	rig := newRegisterRig(t)
	p := Project{Name: "ai-software-factory-code", Repository: "sf/ai-software-factory-code", Namespaces: []string{"factory"}}
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(rig.fj.writes) != 0 || len(rig.kube.changes) != 0 || !strings.Contains(rig.logs.String(), "unmanaged") {
		t.Fatalf("unmanaged project touched: %v %v", rig.fj.writes, rig.kube.changes)
	}
}

// A repository that exists but factory-projects cannot administer stops
// the Forgejo steps with the way out; the cluster steps still run.
func TestRegistrationNeedsAdminOnAnExistingRepository(t *testing.T) {
	rig := newRegisterRig(t)
	rig.fj.repos["sf/demo-app"] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"permissions":{"admin":false}}`)
	}))
	defer srv.Close()
	rig.reg.Forgejo.BaseURL = srv.URL
	err := rig.reg.Reconcile(context.Background(), demoProject())
	if err == nil || !strings.Contains(err.Error(), "factory-projects is no admin of it") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(rig.logs.String(), "namespace demo-staging: created") {
		t.Fatal("cluster steps skipped")
	}
}
