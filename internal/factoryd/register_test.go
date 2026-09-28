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

	"golang.org/x/crypto/ssh"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// forgejoState is a scripted Forgejo holding what registration makes: the
// organisation demo-app owned by factory-projects, its repository
// demo-app/demo-app, the CI user in team ci, tokens, and everything the
// projects token keeps on the repository. Every write records its path
// and credential.
type forgejoState struct {
	mu            sync.Mutex
	org, repo     bool
	orgOwner      string
	users         map[string]bool
	team          []string // ci team members; nil: no team
	fork          map[string]any
	hooks         []map[string]any
	hookSecret    string
	collabs       map[string]string
	protection    map[string]any
	keys          []map[string]any
	labels        []string
	secrets       map[string]string
	tokens        map[int64][2]string // id -> user, name
	nextToken     int64
	minted        []string
	writes        []string
	projectsAdmin bool // factory-projects sees the repository as admin
	emails        map[string]string
	teamUnits     map[string]string
}

func newForgejoState() *forgejoState {
	return &forgejoState{users: map[string]bool{"factory-projects": true, "factory-merge": true, "factory-agent": true, "sebastian": true},
		collabs: map[string]string{}, secrets: map[string]string{}, tokens: map[int64][2]string{}, projectsAdmin: true}
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
		admin := by == "basic sebastian"
		if r.Method != http.MethodGet {
			s.writes = append(s.writes, r.Method+" "+r.URL.Path+" by "+by)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		list := func(v any) {
			if p := r.URL.Query().Get("page"); p != "" && p != "1" {
				io.WriteString(w, "[]")
				return
			}
			json.NewEncoder(w).Encode(v)
		}
		const repo = "/api/v1/repos/demo-app/demo-app"
		p := r.URL.Path
		switch {
		// the site admin's
		case admin && r.Method == http.MethodGet && p == "/api/v1/orgs/demo-app":
			if !s.org {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, `{"username":"demo-app"}`)
		case admin && r.Method == http.MethodPost && p == "/api/v1/admin/users/factory-projects/orgs":
			if body["username"] != "demo-app" || body["visibility"] != "private" {
				t.Errorf("create org %v", body)
			}
			s.org, s.orgOwner = true, "factory-projects"
			w.WriteHeader(http.StatusCreated)
		case admin && p == "/api/v1/users/factory-projects/orgs/demo-app/permissions":
			fmt.Fprintf(w, `{"is_owner":%v}`, s.orgOwner == "factory-projects")
		case admin && r.Method == http.MethodGet && p == repo:
			if !s.repo {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, `{}`)
		case admin && r.Method == http.MethodPost && p == "/api/v1/orgs/demo-app/repos":
			if body["name"] != "demo-app" || body["private"] != true || body["default_branch"] != "main" {
				t.Errorf("create repo %v", body)
			}
			s.repo = true
			w.WriteHeader(http.StatusCreated)
		case admin && r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/users/") && strings.Count(p, "/") == 4:
			user := strings.TrimPrefix(p, "/api/v1/users/")
			if !s.users[user] {
				http.NotFound(w, r)
				return
			}
			email := user + "@factory.local"
			if e, ok := s.emails[user]; ok {
				email = e
			}
			fmt.Fprintf(w, `{"login":%q,"email":%q}`, user, email)
		case admin && r.Method == http.MethodPost && p == "/api/v1/admin/users":
			if body["must_change_password"] != false || len(body["password"].(string)) < 32 {
				t.Errorf("create user %v", body["username"])
			}
			s.users[body["username"].(string)] = true
			w.WriteHeader(http.StatusCreated)
		case admin && r.Method == http.MethodGet && p == "/api/v1/orgs/demo-app/teams":
			if s.team == nil {
				list([]any{map[string]any{"id": 1, "name": "Owners"}})
				return
			}
			list([]any{map[string]any{"id": 1, "name": "Owners"}, map[string]any{"id": 7, "name": "ci", "includes_all_repositories": false,
				"units_map": s.teamUnits}})
		case admin && r.Method == http.MethodPost && p == "/api/v1/orgs/demo-app/teams":
			if !reflect.DeepEqual(body["units_map"], map[string]any{"repo.packages": "write"}) || body["includes_all_repositories"] != false {
				t.Errorf("create team %v", body)
			}
			s.team = []string{}
			s.teamUnits = map[string]string{"repo.code": "none", "repo.packages": "write"}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":7}`)
		case admin && strings.HasPrefix(p, "/api/v1/teams/7/members/"):
			user := strings.TrimPrefix(p, "/api/v1/teams/7/members/")
			if r.Method == http.MethodPut {
				s.team = append(s.team, user)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !slices.Contains(s.team, user) {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, `{}`)
		case admin && strings.HasSuffix(p, "/tokens") || admin && strings.Contains(p, "/tokens/"):
			user := strings.Split(p, "/")[4]
			if r.URL.Query().Get("sudo") != user {
				t.Errorf("token call without sudo=%s: %s", user, r.URL)
			}
			switch r.Method {
			case http.MethodGet:
				var out []map[string]any
				for id, un := range s.tokens {
					if un[0] == user {
						out = append(out, map[string]any{"id": id, "name": un[1]})
					}
				}
				list(nonNil(out))
			case http.MethodDelete:
				var id int64
				fmt.Sscanf(p[strings.LastIndex(p, "/")+1:], "%d", &id)
				delete(s.tokens, id)
				w.WriteHeader(http.StatusNoContent)
			case http.MethodPost:
				s.nextToken++
				s.tokens[s.nextToken] = [2]string{user, body["name"].(string)}
				s.minted = append(s.minted, fmt.Sprintf("%s %v %v %v", user, body["name"], body["scopes"], body["repositories"]))
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"id":%d,"sha1":"%s-%s"}`, s.nextToken, user, body["name"])
			}
		// the projects token's (and the bot's fork and labels)
		case r.Method == http.MethodGet && p == repo:
			if !s.repo || !s.projectsAdmin {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, `{"permissions":{"admin":true}}`)
		case p == repo+"/hooks" && r.Method == http.MethodGet:
			list(s.hooks)
		case p == repo+"/hooks" && r.Method == http.MethodPost:
			body["id"] = 1
			s.hookSecret = body["config"].(map[string]any)["secret"].(string)
			delete(body["config"].(map[string]any), "secret")
			s.hooks = append(s.hooks, body)
			w.WriteHeader(http.StatusCreated)
		case p == repo+"/hooks/1" && r.Method == http.MethodPatch:
			s.hookSecret = body["config"].(map[string]any)["secret"].(string)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{}`)
		case strings.HasPrefix(p, repo+"/collaborators/") && strings.HasSuffix(p, "/permission"):
			user := strings.TrimSuffix(strings.TrimPrefix(p, repo+"/collaborators/"), "/permission")
			perm := s.collabs[user]
			switch {
			case user == "sebastian": // a site admin reads as admin, collaborator or not
				perm = "admin"
			case perm == "":
				perm = "none"
			}
			fmt.Fprintf(w, `{"permission":%q}`, perm)
		case strings.HasPrefix(p, repo+"/collaborators/") && r.Method == http.MethodGet:
			if s.collabs[strings.TrimPrefix(p, repo+"/collaborators/")] == "" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
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
		case p == repo+"/keys" && r.Method == http.MethodGet:
			list(nonNil(s.keys))
		case p == repo+"/keys" && r.Method == http.MethodPost:
			body["id"] = len(s.keys) + 1
			s.keys = append(s.keys, body)
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(p, repo+"/keys/") && r.Method == http.MethodDelete:
			s.keys = slices.DeleteFunc(s.keys, func(k map[string]any) bool { return fmt.Sprint(k["id"]) == p[len(repo+"/keys/"):] })
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/v1/repos/factory-agent/demo-app" && r.Method == http.MethodGet:
			if s.fork == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(s.fork)
		case p == repo+"/forks" && r.Method == http.MethodPost:
			s.fork = map[string]any{"fork": true, "private": true, "has_actions": false, "parent": map[string]string{"full_name": "demo-app/demo-app"}}
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
			t.Errorf("unexpected %s %s by %s", r.Method, r.URL, by)
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
	return &fakeProjectKube{objects: map[string]string{}, secrets: map[string]map[string]string{}}
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
		if o.Name == p.Name || o.Repository == p.Repository || slices.ContainsFunc(o.Namespaces, func(ns string) bool { return slices.Contains(p.Namespaces, ns) }) {
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
	for _, f := range []struct{ from, to *string }{{&t.ReadTokenHash, &p.ReadTokenHash}, {&t.HookSecretHash, &p.HookSecretHash},
		{&t.PullToken, &p.PullToken}, {&t.GitToken, &p.GitToken}, {&t.MergeToken, &p.MergeToken}} {
		if *f.from != "" {
			*f.to = *f.from
		}
	}
	s.m[name] = p
	return nil
}

func (s *memProjects) ClaimReadToken(_ context.Context, name, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[name]
	if p.ReadTokenHash != "" {
		return false, nil
	}
	p.ReadTokenHash = hash
	s.m[name] = p
	return true, nil
}

func (s *memProjects) ForgetReadToken(_ context.Context, name, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.m[name]; p.ReadTokenHash == hash {
		p.ReadTokenHash = ""
		s.m[name] = p
	}
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
		RegistryHost: "git.aisoftwarefactory.world", MergeUser: "factory-merge", ProjectsUser: "factory-projects"}
	return &registerRig{fj: fj, kube: kube, store: store, reg: reg, logs: captureLog(t)}
}

var admin = AdminAuth{User: "sebastian", Password: "admin-pw"}

// register-project on an empty Forgejo and cluster: the admin makes the
// organisation (owned by factory-projects), the repository, the CI user in
// team ci and every token; the projects token keeps the rest. A second run
// writes nothing and logs only "no change" (ruling 164).
func TestRegisterIsIdempotent(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, demoProject(), admin, false)
	if err != nil {
		t.Fatal(err)
	}
	first := rig.logs.String()
	for _, want := range []string{
		"row: created", "organisation demo-app: created", "repository demo-app/demo-app: created", "user ci-demo-app: created",
		"team ci: created", "token pull of ci-demo-app: minted", "token git-demo-app of factory-projects: minted",
		"token project-demo-app of factory-merge: minted", "token registry of ci-demo-app: minted",
		"webhook: created", "collaborator factory-agent: created", "collaborator factory-merge: no change", "collaborator sebastian: created",
		"protection main: created", "fork factory-agent/demo-app: ready", "deploy key: created",
		"actions secret FACTORY_READ_TOKEN: created", "actions secret REGISTRY_USER: no change",
		"namespace demo-staging: created", "demo-staging networkpolicy: created", "demo-prod resourcequota: created",
		"demo-staging secret forgejo-registry: created", "demo-prod secret demo-write-token/token: created",
		"flux secret demo-app-git: created", "flux gitrepository: created", "demo-prod flux kustomization: created",
	} {
		if !strings.Contains(first, "project demo-app: "+want) {
			t.Errorf("first run lacks %q", want)
		}
	}
	if p.PullToken != "ci-demo-app-pull" || p.GitToken != "factory-projects-git-demo-app" || p.MergeToken != "factory-merge-project-demo-app" {
		t.Fatalf("tokens = %q %q %q", p.PullToken, p.GitToken, p.MergeToken)
	}
	want := []string{"ci-demo-app pull [read:package] <nil>",
		"factory-projects git-demo-app [read:repository] [map[name:demo-app owner:demo-app]]",
		"factory-merge project-demo-app [write:repository] [map[name:demo-app owner:demo-app]]",
		"ci-demo-app registry [write:package] <nil>"}
	if !reflect.DeepEqual(rig.fj.minted, want) {
		t.Fatalf("minted %q", rig.fj.minted)
	}
	writes, changes := len(rig.fj.writes), len(rig.kube.changes)
	stored := rig.store.m["demo-app"]

	rig.logs.Reset()
	if _, err := rig.reg.Register(ctx, demoProject(), admin, false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(rig.logs.String()), "\n") {
		if !strings.HasSuffix(line, ": no change") && !strings.HasSuffix(line, ": ready") && !strings.HasSuffix(line, ": ensured") &&
			!strings.HasSuffix(line, ": no change (its settings are the console's)") {
			t.Errorf("second run: %s", line)
		}
	}
	if len(rig.fj.writes) != writes || len(rig.kube.changes) != changes {
		t.Fatalf("second run wrote: forgejo %v, kube %v", rig.fj.writes[writes:], rig.kube.changes[changes:])
	}
	if !reflect.DeepEqual(rig.store.m["demo-app"], stored) {
		t.Fatal("second run changed the row")
	}
	other := demoProject()
	other.Namespaces = append(other.Namespaces, "demo-extra")
	if _, err := rig.reg.Register(ctx, other, admin, false); err == nil || !strings.Contains(err.Error(), "registered with other") {
		t.Fatalf("a changed registration = %v", err)
	}
}

// What registration wrote, and with which credential.
func TestRegistrationObjects(t *testing.T) {
	rig := newRegisterRig(t)
	if _, err := rig.reg.Register(context.Background(), demoProject(), admin, false); err != nil {
		t.Fatal(err)
	}
	fj := rig.fj
	for _, w := range fj.writes {
		switch {
		case strings.Contains(w, "/admin/") || strings.Contains(w, "/tokens") || strings.Contains(w, "/teams") ||
			strings.HasPrefix(w, "POST /api/v1/orgs/demo-app/repos") || w == "PUT /api/v1/repos/demo-app/demo-app/collaborators/factory-merge by basic sebastian":
			if !strings.HasSuffix(w, "by basic sebastian") {
				t.Errorf("%s: want the admin", w)
			}
		case strings.Contains(w, "/forks") || strings.Contains(w, "/labels"):
			if !strings.HasSuffix(w, "by basic factory-agent") {
				t.Errorf("%s: want the bot", w)
			}
		case !strings.HasSuffix(w, "by token projects-tok"):
			t.Errorf("%s: want the projects token", w)
		}
	}
	if !slices.Equal(fj.team, []string{"ci-demo-app"}) || fj.orgOwner != "factory-projects" {
		t.Errorf("team %v, owner %q", fj.team, fj.orgOwner)
	}
	if fj.secrets["REGISTRY_USER"] != "ci-demo-app" || fj.secrets["REGISTRY_PASSWORD"] != "ci-demo-app-registry" {
		t.Errorf("registry secrets = %q / %q", fj.secrets["REGISTRY_USER"], fj.secrets["REGISTRY_PASSWORD"])
	}
	if tok := fj.secrets["FACTORY_READ_TOKEN"]; !rig.store.m["demo-app"].ReadTokenMatches(tok) {
		t.Error("the stored hash does not match the FACTORY_READ_TOKEN set")
	}
	// the deploy key: one write key, its private half only in DEPLOY_KEY
	if len(fj.keys) != 1 || fj.keys[0]["title"] != "factory-roll" || fj.keys[0]["read_only"] != false {
		t.Fatalf("keys = %v", fj.keys)
	}
	signer, err := ssh.ParsePrivateKey([]byte(fj.secrets["DEPLOY_KEY"]))
	if err != nil {
		t.Fatalf("DEPLOY_KEY: %v", err)
	}
	if got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); !strings.HasPrefix(fj.keys[0]["key"].(string), got) {
		t.Fatalf("the key added is not DEPLOY_KEY's: %s vs %s", fj.keys[0]["key"], got)
	}
	hook := fj.hooks[0]
	if hook["type"] != "forgejo" || hook["config"].(map[string]any)["url"] != rig.reg.WebhookURL || fj.hookSecret != "hook-secret" ||
		len(hook["events"].([]any)) != len(ProjectHookEvents) {
		t.Errorf("hook = %v", hook)
	}
	if fj.collabs["factory-agent"] != "write" || fj.collabs["factory-merge"] != "write" || fj.collabs["sebastian"] != "write" {
		t.Errorf("collaborators = %v", fj.collabs)
	}
	// auto: no required approval; pushes only by the approvers and the deploy key
	want := map[string]any{"rule_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []any{"sebastian"},
		"push_whitelist_deploy_keys": true, "enable_merge_whitelist": true, "merge_whitelist_usernames": []any{"sebastian", "factory-merge"},
		"enable_approvals_whitelist": true, "approvals_whitelist_username": []any{"sebastian"}, "required_approvals": float64(0),
		"apply_to_admins": true, "block_on_rejected_reviews": true, "dismiss_stale_approvals": true, "enable_status_check": false,
		"protected_file_patterns": ".forgejo/**;.gitea/**;.github/**", "unprotected_file_patterns": ""}
	if !reflect.DeepEqual(fj.protection, want) {
		t.Errorf("protection = %v", fj.protection)
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
	cnp := k.object(t, "/apis/cilium.io/v2/namespaces/demo-prod/ciliumnetworkpolicies/project")
	b, _ := json.Marshal(cnp["spec"])
	for _, want := range []string{`"endpointSelector":{}`, `"fromEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"demo-prod"}}]`,
		`"fromEntities":["ingress"]`, `"toEntities":["world"],"toPorts":[{"ports":[{"port":"443","protocol":"TCP"}]}]`,
		`"k8s:k8s-app":"kube-dns"`, `"port":"4318"`, `"toEntities":["kube-apiserver"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("network policy lacks %s: %s", want, b)
		}
	}
	flux := k.object(t, "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-staging/rolebindings/flux")
	if flux["roleRef"].(map[string]any)["name"] != "factory-project-deployer" ||
		!reflect.DeepEqual(flux["subjects"], []any{map[string]any{"kind": "ServiceAccount", "name": "flux", "namespace": "demo-staging"}}) {
		t.Errorf("flux binding = %v", flux)
	}
	src := k.object(t, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/demo-app")
	if !reflect.DeepEqual(src["spec"], map[string]any{"interval": "1m", "url": "https://git.aisoftwarefactory.world/demo-app/demo-app",
		"ref": map[string]any{"branch": "main"}, "secretRef": map[string]any{"name": "demo-app-git"}}) {
		t.Errorf("gitrepository = %v", src["spec"])
	}
	if got := k.secrets["flux-system/demo-app-git"]; !reflect.DeepEqual(got, map[string]string{"username": "factory-projects", "password": "factory-projects-git-demo-app"}) {
		t.Errorf("flux secret = %v", got)
	}
	ks := k.object(t, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-prod/kustomizations/demo-app")
	if !reflect.DeepEqual(ks["spec"], map[string]any{"interval": "1m", "path": "./deploy/prod", "prune": true,
		"sourceRef":          map[string]any{"kind": "GitRepository", "name": "demo-app", "namespace": "flux-system"},
		"serviceAccountName": "flux", "targetNamespace": "demo-prod"}) {
		t.Errorf("kustomization = %v", ks["spec"])
	}
	pull := k.secrets["demo-prod/forgejo-registry"][".dockerconfigjson"]
	if pull != `{"auths":{"git.aisoftwarefactory.world":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("ci-demo-app:ci-demo-app-pull"))+`","password":"ci-demo-app-pull","username":"ci-demo-app"}}}` {
		t.Errorf("pull secret = %s", pull)
	}
	if tok := k.secrets["demo-staging/demo-write-token"]["token"]; len(tok) != 64 || tok == k.secrets["demo-prod/demo-write-token"]["token"] {
		t.Errorf("generated secrets: %q, %q", tok, k.secrets["demo-prod/demo-write-token"]["token"])
	}
}

// A project the console recorded waits for register-project in Forgejo;
// its cluster objects come at once, its Flux source once the tokens exist.
func TestConsoleRegisteredProjectWaits(t *testing.T) {
	rig := newRegisterRig(t)
	p := demoProject()
	rig.store.m[p.Name] = p
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	logs := rig.logs.String()
	for _, want := range []string{"repository demo-app/demo-app: " + string(Waiting), "demo-staging secret forgejo-registry: " + string(Waiting),
		"flux source: " + string(Waiting), "namespace demo-prod: created"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if len(rig.fj.writes) != 0 {
		t.Fatalf("wrote %v", rig.fj.writes)
	}
}

// A merge rule change reaches main's protection, a quota change the
// ResourceQuotas, a new webhook secret (a re-seed) the hook, a new
// approver the collaborators and whitelists.
func TestRegistrationFollowsSettings(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, demoProject(), admin, false)
	if err != nil {
		t.Fatal(err)
	}
	rig.store.SetProjectSettings(ctx, p.Name, "human", nil, policy.Quota{CPU: "1", Memory: "2Gi", Storage: "5Gi"})
	rig.reg.WebhookSecret = "re-seeded"
	rig.fj.users["alice"] = true
	rig.reg.Policy = func() policy.Document { return policy.Document{Approvers: []string{"sebastian", "alice"}} }
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, rig.store.m[p.Name]); err != nil {
		t.Fatal(err)
	}
	got := rig.logs.String()
	for _, want := range []string{"protection main: updated", "demo-staging resourcequota: updated", "demo-prod resourcequota: updated",
		"webhook: updated", "collaborator alice: created", "demo-staging limitrange: no change", "deploy key: no change"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if rig.fj.protection["required_approvals"] != float64(1) || rig.fj.hookSecret != "re-seeded" ||
		!reflect.DeepEqual(rig.fj.protection["push_whitelist_usernames"], []any{"sebastian", "alice"}) {
		t.Errorf("protection %v, hook secret %q", rig.fj.protection, rig.fj.hookSecret)
	}
	if rig.store.m[p.Name].HookSecretHash != HashToken("re-seeded") {
		t.Error("the new hook secret's hash was not stored")
	}
}

// A deploy key or DEPLOY_KEY that went missing is made anew, both halves.
func TestDeployKeyHeals(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, demoProject(), admin, false)
	if err != nil {
		t.Fatal(err)
	}
	old := rig.fj.secrets["DEPLOY_KEY"]
	delete(rig.fj.secrets, "DEPLOY_KEY")
	rig.logs.Reset()
	rig.reg.Reconcile(ctx, rig.store.m[p.Name])
	if !strings.Contains(rig.logs.String(), "deploy key: updated") || len(rig.fj.keys) != 1 || rig.fj.secrets["DEPLOY_KEY"] == old {
		t.Fatalf("keys %v, log:\n%s", rig.fj.keys, rig.logs)
	}
}

// Two reconciles racing on a missing FACTORY_READ_TOKEN: only the one that
// stores its hash sets the secret, so they never disagree (m5).
func TestReadTokenRace(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, demoProject(), admin, false)
	if err != nil {
		t.Fatal(err)
	}
	delete(rig.fj.secrets, "FACTORY_READ_TOKEN")
	stale := rig.store.m[p.Name]
	// the other process forgot the old hash and claimed its own first
	rig.store.ForgetReadToken(ctx, p.Name, stale.ReadTokenHash)
	rig.store.ClaimReadToken(ctx, p.Name, HashToken("theirs"))
	rig.fj.secrets["FACTORY_READ_TOKEN"] = "theirs"
	run := &reconcileRun{r: rig.reg, p: stale}
	c, err := run.readToken(ctx, nil)
	if err != nil || c != "set by another reconcile" || rig.fj.secrets["FACTORY_READ_TOKEN"] != "theirs" ||
		!rig.store.m[p.Name].ReadTokenMatches("theirs") {
		t.Fatalf("= %q, %v; secret %q", c, err, rig.fj.secrets["FACTORY_READ_TOKEN"])
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

// An organisation of that name that factory-projects does not own is
// someone else's: registration stops before anything is written.
func TestRegisterRefusesAForeignOrganisation(t *testing.T) {
	rig := newRegisterRig(t)
	rig.fj.org, rig.fj.orgOwner = true, "mallory"
	_, err := rig.reg.Register(context.Background(), demoProject(), admin, false)
	if err == nil || !strings.Contains(err.Error(), "factory-projects does not own it") {
		t.Fatalf("err = %v", err)
	}
	if len(rig.fj.writes) != 0 {
		t.Fatalf("wrote %v", rig.fj.writes)
	}
}

// --rotate mints every token again and rewrites the registry secrets.
func TestRegisterRotates(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	if _, err := rig.reg.Register(ctx, demoProject(), admin, false); err != nil {
		t.Fatal(err)
	}
	n := len(rig.fj.minted)
	if _, err := rig.reg.Register(ctx, demoProject(), admin, true); err != nil {
		t.Fatal(err)
	}
	if len(rig.fj.minted)-n != 4 || len(rig.fj.tokens) != 4 {
		t.Fatalf("rotate minted %d, %d tokens live (old ones must be revoked)", len(rig.fj.minted)-n, len(rig.fj.tokens))
	}
}

// review-task65 n2: registration adopts only the CI user and team it made.
func TestRegisterRefusesForeignCIUserOrTeam(t *testing.T) {
	rig := newRegisterRig(t)
	rig.fj.users["ci-demo-app"] = true
	rig.fj.emails = map[string]string{"ci-demo-app": "mallory@example.com"}
	if _, err := rig.reg.Register(context.Background(), demoProject(), admin, false); err == nil ||
		!strings.Contains(err.Error(), "a user ci-demo-app exists that registration did not make") {
		t.Fatalf("foreign user = %v", err)
	}
	if len(rig.fj.minted) != 0 {
		t.Fatalf("minted for a foreign user: %v", rig.fj.minted)
	}

	rig = newRegisterRig(t)
	rig.fj.team = []string{}
	rig.fj.teamUnits = map[string]string{"repo.code": "write", "repo.packages": "write"}
	if _, err := rig.reg.Register(context.Background(), demoProject(), admin, false); err == nil ||
		!strings.Contains(err.Error(), "a team ci exists in demo-app with more than package write") {
		t.Fatalf("foreign team = %v", err)
	}
	if len(rig.fj.minted) != 0 || len(rig.fj.team) != 0 {
		t.Fatalf("used a foreign team: minted %v, members %v", rig.fj.minted, rig.fj.team)
	}
}
