package factoryd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/openbao"
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
	protection    map[string]map[string]any // branch -> its protection
	branches      map[string]string         // branch -> head
	keys          []map[string]any
	labels        []string
	secrets       map[string]string
	tokens        map[int64][2]string // id -> user, name
	nextToken     int64
	minted        []string
	writes        []string
	hookReads     []string // the credential of each hooks list
	projectsAdmin bool     // factory-projects sees the repository as admin
	emails        map[string]string
	teamUnits     map[string]string
	seeds         []seedCommit
	orgBasic      string // the basic auth, user:password, of the organisation's create

	files         map[string]bool // main's paths
	migrated      []map[string]any
	migrateFail   string        // a migrate's 500 answer; "": it works
	migrateGate   chan struct{} // a migrate answers once it closes, the fake's lock free meanwhile; nil: at once
	source        []string      // the migrated repository's paths; none: it is empty
	sourceBranch  string        // and its default branch
	defaultBranch string
}

// seedCommit is one contents call: its branch, message, credential and
// each file's operation and decoded content by path.
type seedCommit struct {
	branch, message, by string
	ops                 map[string]string
	files               map[string]string
}

func newForgejoState() *forgejoState {
	return &forgejoState{users: map[string]bool{"factory-projects": true, "factory-merge": true, "factory-agent": true, "sebastian": true},
		collabs: map[string]string{}, protection: map[string]map[string]any{}, branches: map[string]string{}, secrets: map[string]string{}, tokens: map[int64][2]string{}, projectsAdmin: true,
		files: map[string]bool{}}
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
		case p == "/api/v1/repos/other/other": // a second project, not made yet
			http.NotFound(w, r)
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
			u, pw, _ := r.BasicAuth()
			s.orgBasic = u + ":" + pw
			w.WriteHeader(http.StatusCreated)
		case admin && p == "/api/v1/users/factory-projects/orgs/demo-app/permissions":
			fmt.Fprintf(w, `{"is_owner":%v}`, s.orgOwner == "factory-projects")
		case admin && r.Method == http.MethodGet && p == repo:
			if !s.repo {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"empty":%t}`, len(s.branches) == 0)
		case admin && r.Method == http.MethodPost && p == "/api/v1/orgs/demo-app/repos":
			if body["name"] != "demo-app" || body["private"] != true || body["default_branch"] != "main" || body["auto_init"] != nil {
				t.Errorf("create repo %v", body)
			}
			s.repo = true
			w.WriteHeader(http.StatusCreated)
		case admin && r.Method == http.MethodPost && p == "/api/v1/repos/migrate":
			s.migrated = append(s.migrated, body)
			if gate := s.migrateGate; gate != nil {
				s.mu.Unlock()
				<-gate
				s.mu.Lock()
			}
			if s.migrateFail != "" {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, s.migrateFail)
				return
			}
			s.repo, s.defaultBranch = true, s.sourceBranch
			for _, f := range s.source {
				s.files[f], s.branches[s.sourceBranch] = true, "1mp0rt"
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
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
			fmt.Fprintf(w, `{"permissions":{"admin":true},"default_branch":%q,"empty":%t}`, s.defaultBranch, len(s.branches) == 0)
		case p == repo && r.Method == http.MethodPatch:
			s.defaultBranch = body["default_branch"].(string)
			io.WriteString(w, `{}`)
		case strings.HasPrefix(p, repo+"/contents/") && r.Method == http.MethodGet:
			path := strings.TrimPrefix(p, repo+"/contents/")
			dir := slices.ContainsFunc(slices.Collect(maps.Keys(s.files)), func(f string) bool { return strings.HasPrefix(f, path+"/") })
			if !s.files[path] && !dir || r.URL.Query().Get("ref") != "main" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, `{}`)
		case p == repo+"/hooks" && r.Method == http.MethodGet:
			s.hookReads = append(s.hookReads, by)
			if by == "token revoked-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if by != "token projects-tok" && by != "token registrar-tok" { // an admin's token only
				w.WriteHeader(http.StatusForbidden)
				return
			}
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
		case p == repo+"/collaborators" && r.Method == http.MethodGet:
			var out []map[string]string
			for _, u := range slices.Sorted(maps.Keys(s.collabs)) {
				out = append(out, map[string]string{"login": u})
			}
			list(nonNil(out))
		case strings.HasPrefix(p, repo+"/collaborators/") && r.Method == http.MethodDelete:
			delete(s.collabs, strings.TrimPrefix(p, repo+"/collaborators/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, repo+"/collaborators/") && r.Method == http.MethodPut:
			s.collabs[strings.TrimPrefix(p, repo+"/collaborators/")] = body["permission"].(string)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, repo+"/branch_protections/") && r.Method == http.MethodGet:
			b, ok := s.protection[strings.TrimPrefix(p, repo+"/branch_protections/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(b)
		case p == repo+"/branch_protections" && r.Method == http.MethodPost:
			s.protection[body["rule_name"].(string)] = body
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(p, repo+"/branch_protections/") && r.Method == http.MethodPatch:
			name := strings.TrimPrefix(p, repo+"/branch_protections/")
			body["rule_name"] = name
			s.protection[name] = body
			json.NewEncoder(w).Encode(body)
		case strings.HasPrefix(p, repo+"/branches/") && r.Method == http.MethodGet:
			head, ok := s.branches[strings.TrimPrefix(p, repo+"/branches/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
		case p == repo+"/branches" && r.Method == http.MethodPost:
			head, ok := s.branches[body["old_branch_name"].(string)]
			if _, covered := s.protection[body["new_branch_name"].(string)]; covered { // Forgejo 15: its pre-receive hook refuses
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			s.branches[body["new_branch_name"].(string)] = head
			w.WriteHeader(http.StatusCreated)
		case p == repo+"/contents" && r.Method == http.MethodPost:
			c := seedCommit{branch: body["branch"].(string), message: body["message"].(string), by: by, ops: map[string]string{}, files: map[string]string{}}
			for _, f := range body["files"].([]any) {
				f := f.(map[string]any)
				b, _ := base64.StdEncoding.DecodeString(f["content"].(string))
				c.ops[f["path"].(string)], c.files[f["path"].(string)] = f["operation"].(string), string(b)
			}
			_, has := s.branches[c.branch]
			if has && s.protection[c.branch] != nil || slices.ContainsFunc(slices.Collect(maps.Keys(c.files)), func(f string) bool { return s.files[f] }) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			for f := range c.files {
				s.files[f] = true
			}
			s.seeds = append(s.seeds, c)
			s.branches[c.branch] = "5eed"
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"commit":{"sha":"5eed"}}`)
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
		case strings.HasPrefix(p, repo+"/actions/secrets/") && r.Method == http.MethodPut &&
			strings.HasPrefix(strings.TrimPrefix(p, repo+"/actions/secrets/"), "FORGEJO_"):
			// Forgejo reserves the prefixes FORGEJO_, GITEA_ and GITHUB_
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"invalid secret name"}`)
		case strings.HasPrefix(p, repo+"/actions/secrets/") && r.Method == http.MethodPut && body["data"] == "":
			// CreateOrUpdateSecretOption.Data is binding:"Required"
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `[{"fieldNames":["Data"],"classification":"RequiredError","message":"Required"}]`)
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
	labels  map[string]map[string]string // a namespace the infra repository made -> its labels
	objects map[string]string
	secrets map[string]map[string]string
	meta    map[string]secretMeta
	deploys map[string][]DeploymentState
	changes []string
	fail    string // an apply whose path holds it fails
}

func newFakeProjectKube() *fakeProjectKube {
	return &fakeProjectKube{objects: map[string]string{}, secrets: map[string]map[string]string{}, meta: map[string]secretMeta{}}
}

// secretMeta is an applied secret's type and labels; a secret set by hand
// in secrets has none, an unlabelled Opaque one.
type secretMeta struct {
	typ    string
	labels map[string]string
}

func (k *fakeProjectKube) Apply(_ context.Context, path string, obj map[string]any) (Change, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != "" && strings.Contains(path, k.fail) {
		return "", errors.New("the API server refused")
	}
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
		typ, _ := obj["type"].(string)
		if typ == "" {
			typ = "Opaque"
		}
		labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]string)
		k.meta[ns+"/"+name] = secretMeta{typ, labels}
	}
	return c, nil
}

// SecretsByLabels answers ns's secrets in no order, as a map ranges.
func (k *fakeProjectKube) SecretsByLabels(_ context.Context, ns string, labels map[string]string) ([]KubeSecret, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []KubeSecret
	for key, data := range k.secrets {
		in, name, _ := strings.Cut(key, "/")
		m, ok := k.meta[key]
		if !ok {
			m.typ = "Opaque"
		}
		if in == ns && !slices.ContainsFunc(slices.Collect(maps.Keys(labels)), func(l string) bool { return m.labels[l] != labels[l] }) {
			out = append(out, KubeSecret{Name: name, Type: m.typ, Data: maps.Clone(data)})
		}
	}
	return out, nil
}

// putSecret applies a secret of type typ with labels and data, as Flux or
// a hand would.
func (k *fakeProjectKube) putSecret(t *testing.T, ns, name, typ string, labels, data map[string]string) {
	t.Helper()
	b64 := map[string]string{}
	for key, v := range data {
		b64[key] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	if _, err := k.Apply(context.Background(), "/api/v1/namespaces/"+ns+"/secrets/"+name, map[string]any{"type": typ,
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels}, "data": b64}); err != nil {
		t.Fatal(err)
	}
}

func (k *fakeProjectKube) GetSecretData(_ context.Context, ns, name string) (map[string]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d, ok := k.secrets[ns+"/"+name]; ok {
		return d, nil
	}
	return nil, ErrNotFound
}

func (k *fakeProjectKube) NamespaceLabel(_ context.Context, name, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	labels, ok := k.labels[name]
	if !ok {
		return "", ErrNotFound
	}
	return labels[key], nil
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

// memProjects holds the joined projects by name (as their files say) and
// which of them have a row.
type memProjects struct {
	mu   sync.Mutex
	m    map[string]Project
	rows map[string]bool
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

// EnsureProjectRows gives a row to each name that has none.
func (s *memProjects) EnsureProjectRows(_ context.Context, names []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]bool{}
	}
	var created []string
	for _, n := range names {
		if !s.rows[n] {
			s.rows[n] = true
			created = append(created, n)
		}
	}
	return created, nil
}

func (s *memProjects) SetProjectTokens(_ context.Context, name string, t ProjectTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[name]
	for _, f := range []struct{ from, to *string }{{&t.ReadTokenHash, &p.ReadTokenHash}, {&t.HookSecretHash, &p.HookSecretHash}, {&t.HostKeyHash, &p.HostKeyHash}, {&t.StagingHash, &p.StagingHash},
		{&t.PullToken, &p.PullToken}, {&t.GitToken, &p.GitToken}, {&t.MergeToken, &p.MergeToken}} {
		if *f.from != "" {
			*f.to = *f.from
		}
	}
	s.m[name] = p
	return nil
}

func (s *memProjects) SetProjectReady(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.m[name]; p.ReadyAt.IsZero() {
		p.ReadyAt = time.Now()
		s.m[name] = p
	}
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

// fakeKV is OpenBao's kv-v2 as the Registrar reads it.
type fakeKV struct {
	mu    sync.Mutex
	data  map[string]string
	err   error
	reads int
}

func (k *fakeKV) KV(_ context.Context, path string) (map[string]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reads++
	if path != AdminSecret {
		return nil, fmt.Errorf("no secret at %s", path)
	}
	return maps.Clone(k.data), k.err
}

type registerRig struct {
	kv    *fakeKV
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
	store := &memProjects{m: map[string]Project{"demo-app": demoProject()}}
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", ProjectsToken: "projects-tok", HTTP: srv.Client()}
	kv := &fakeKV{data: map[string]string{"username": "sebastian", "password": "admin-pw"}}
	reg := &Registrar{Forgejo: f, Kube: kube, Store: store, OpenBao: kv,
		Policy:     approversTree("sebastian"),
		WebhookURL: "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo", WebhookSecret: "hook-secret",
		FactoryNamespace: "factory", PullSecret: "forgejo-registry", GitURL: "https://git.aisoftwarefactory.world",
		RegistryHost: "git.aisoftwarefactory.world", MergeUser: "factory-merge", ProjectsUser: "factory-projects"}
	return &registerRig{kv: kv, fj: fj, kube: kube, store: store, reg: reg, logs: captureLog(t)}
}

// register-project on an empty Forgejo and cluster: the admin makes the
// organisation (owned by factory-projects), the repository, the CI user in
// team ci and every token; the projects token keeps the rest. A second run
// writes nothing and logs only "no change" (ruling 164).
func TestRegisterIsIdempotent(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, "demo-app", false)
	if err != nil {
		t.Fatal(err)
	}
	first := rig.logs.String()
	for _, want := range []string{
		"row: created", "organisation demo-app: created", "repository demo-app/demo-app: created", "user ci-demo-app: created",
		"team ci: created", "token pull of ci-demo-app: minted", "token git-demo-app of factory-projects: minted",
		"token project-demo-app of factory-merge: minted", "token registry of ci-demo-app: minted",
		"webhook: created", "collaborator factory-agent: created", "collaborator factory-merge: no change", "collaborator sebastian: created",
		"protection main: created", "branch deploy: waiting: main has no commit yet", "protection deploy: waiting: main has no commit yet",
		"fork factory-agent/demo-app: ready", "deploy key: created", "actions secret FACTORY_READ_TOKEN: created", "actions secret REGISTRY_USER: no change",
		"actions secrets STAGING_URL, STAGING_SECRETS: created",
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

	rig.fj.branches["main"] = "c0ffee"
	rig.logs.Reset()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: branch deploy: created\n") || rig.fj.branches["deploy"] != "c0ffee" ||
		!strings.Contains(rig.logs.String(), "project demo-app: protection deploy: created\n") {
		t.Fatalf("deploy after main's first push: %v\n%s", rig.fj.branches, rig.logs)
	}
	writes, changes := len(rig.fj.writes), len(rig.kube.changes)
	stored := rig.store.m["demo-app"]

	rig.logs.Reset()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(rig.logs.String()), "\n") {
		if !strings.HasSuffix(line, ": no change") && !strings.HasSuffix(line, ": ready") {
			t.Errorf("second run: %s", line)
		}
	}
	if len(rig.fj.writes) != writes || len(rig.kube.changes) != changes {
		t.Fatalf("second run wrote: forgejo %v, kube %v", rig.fj.writes[writes:], rig.kube.changes[changes:])
	}
	if !reflect.DeepEqual(rig.store.m["demo-app"], stored) {
		t.Fatal("second run changed the row")
	}
	for name, p := range map[string]Project{"infra": {Name: "infra", Repository: "sf/infra", Objects: "seed", File: true},
		"gone": {Name: "gone", Repository: "gone/gone", Objects: "factory"}} {
		rig.store.m[name] = p
		if _, err := rig.reg.Register(ctx, name, false); err == nil || !strings.Contains(err.Error(), "registration makes a factory project's objects") {
			t.Fatalf("registering %s = %v", name, err)
		}
	}
	if _, err := rig.reg.Register(ctx, "nothing", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("registering a project no file names = %v", err)
	}
}

// What registration wrote, and with which credential.
func TestRegistrationObjects(t *testing.T) {
	rig := newRegisterRig(t)
	rig.fj.branches["main"] = "c0ffee"
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatal(err)
	}
	fj, k := rig.fj, rig.kube
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
	// auto: no required approval; pushes to main by factory-merge and the
	// approvers, to deploy by the deploy key alone, never touching workflows
	want := map[string]map[string]any{
		"main": {"rule_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []any{"sebastian", "factory-merge"},
			"push_whitelist_deploy_keys": false, "enable_merge_whitelist": true, "merge_whitelist_usernames": []any{"sebastian", "factory-merge"},
			"enable_approvals_whitelist": true, "approvals_whitelist_username": []any{"sebastian"}, "required_approvals": float64(0),
			"apply_to_admins": true, "block_on_rejected_reviews": true, "dismiss_stale_approvals": true, "enable_status_check": false,
			"protected_file_patterns": "", "unprotected_file_patterns": ""},
		"deploy": {"rule_name": "deploy", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []any{},
			"push_whitelist_deploy_keys": true, "enable_merge_whitelist": true, "merge_whitelist_usernames": []any{},
			"enable_approvals_whitelist": false, "approvals_whitelist_username": []any{}, "required_approvals": float64(0),
			"apply_to_admins": true, "block_on_rejected_reviews": false, "dismiss_stale_approvals": false, "enable_status_check": false,
			"protected_file_patterns": ".forgejo/**;.gitea/**;.github/**", "unprotected_file_patterns": ""},
	}
	if !reflect.DeepEqual(fj.protection, want) {
		t.Errorf("protection = %v", fj.protection)
	}

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
		`"fromEntities":["ingress"]`,
		`"k8s:k8s-app":"kube-dns"`, `"port":"4318"`, `"toEntities":["kube-apiserver"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("network policy lacks %s: %s", want, b)
		}
	}
	// backlog 20: a file without egress reaches nothing outside the cluster
	for _, not := range []string{"world", "toFQDNs", "rules"} {
		if strings.Contains(string(b), not) {
			t.Errorf("network policy has %s: %s", not, b)
		}
	}
	flux := k.object(t, "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-staging/rolebindings/flux")
	if flux["roleRef"].(map[string]any)["name"] != "factory-project-deployer" ||
		!reflect.DeepEqual(flux["subjects"], []any{map[string]any{"kind": "ServiceAccount", "name": "flux", "namespace": "demo-staging"}}) {
		t.Errorf("flux binding = %v", flux)
	}
	src := k.object(t, "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/demo-app")
	if !reflect.DeepEqual(src["spec"], map[string]any{"interval": "1m", "url": "https://git.aisoftwarefactory.world/demo-app/demo-app",
		"ref": map[string]any{"branch": "deploy"}, "secretRef": map[string]any{"name": "demo-app-git"}}) {
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

// A factory project's file whose registration cannot read the Forgejo
// admin's login from OpenBao: the registration fails and says why, its
// Forgejo objects wait, its cluster objects come at once, its Flux source
// once the tokens exist.
func TestProjectFileWaitsForRegistration(t *testing.T) {
	rig := newRegisterRig(t)
	rig.kv.err = errors.New("openbao kv factory/forgejo-admin: openbao answered 403: permission denied")
	p := demoProject()
	rig.store.m[p.Name] = p
	err := rig.reg.Reconcile(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "registration: the Forgejo admin's login: openbao kv factory/forgejo-admin: openbao answered 403") {
		t.Fatalf("err = %v", err)
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
	if got := rig.reg.ReconcileError("demo-app"); !strings.HasPrefix(got, "registration: the Forgejo admin's login: openbao kv factory/forgejo-admin") {
		t.Fatalf("ReconcileError = %q", got)
	}
	rig.kv.err = nil
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := rig.reg.ReconcileError("demo-app"); got != "" {
		t.Fatalf("after a reconcile that worked, ReconcileError = %q", got)
	}
}

// Registration reads the Forgejo site admin's login from OpenBao's kv-v2
// factory/forgejo-admin through the OpenBao client and uses it as basic
// auth on the site admin's calls.
func TestAdminFromOpenBao(t *testing.T) {
	var reads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/auth/kubernetes/login":
			io.WriteString(w, `{"auth":{"client_token":"tok-1","lease_duration":3600}}`)
		case "GET /v1/factory/data/forgejo-admin":
			reads = append(reads, r.Header.Get("X-Vault-Token"))
			io.WriteString(w, `{"data":{"data":{"username":"sebastian","password":"pw"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig := newRegisterRig(t)
	rig.reg.OpenBao = &openbao.Client{BaseURL: srv.URL, AuthRole: "factoryd", TokenFile: tokenFile, HTTP: srv.Client()}
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if rig.fj.orgBasic != "sebastian:pw" || !slices.Equal(reads, []string{"tok-1"}) {
		t.Fatalf("organisation created with %q; kv reads %v", rig.fj.orgBasic, reads)
	}
}

// A reconcile of a factory project whose row has no tokens registers it
// first: organisation, repository, CI user, team, the row's three tokens
// (and the registry token into its Actions secrets), then the Forgejo
// steps from the seed on, with the minted tokens in the cluster's.
func TestReconcileRegistersUnregisteredProject(t *testing.T) {
	rig := newRegisterRig(t)
	p := seededProject()
	rig.store.m[p.Name] = p
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	logs := rig.logs.String()
	order := []string{"organisation demo-app: created", "repository demo-app/demo-app: created", "user ci-demo-app: created", "team ci: created",
		"token pull of ci-demo-app: minted", "token git-demo-app of factory-projects: minted", "token project-demo-app of factory-merge: minted",
		"seed: created 5eed from template ts-go", "demo-staging secret forgejo-registry: created", "flux gitrepository: created"}
	at := -1
	for _, want := range order {
		i := strings.Index(logs, "project demo-app: "+want)
		if i < at {
			t.Fatalf("%q missing or out of order:\n%s", want, logs)
		}
		at = i
	}
	row := rig.store.m["demo-app"]
	if row.PullToken != "ci-demo-app-pull" || row.GitToken != "factory-projects-git-demo-app" || row.MergeToken != "factory-merge-project-demo-app" {
		t.Fatalf("row tokens %q %q %q", row.PullToken, row.GitToken, row.MergeToken)
	}
	if len(rig.fj.minted) != 4 || rig.kv.reads != 1 {
		t.Fatalf("minted %v, kv reads %d", rig.fj.minted, rig.kv.reads)
	}
}

// Two reconciles of one copy read before the registration stored its
// tokens: the one queued behind the registering one reads the row under
// the lock and mints nothing again.
func TestQueuedReconcileDoesNotMintAgain(t *testing.T) {
	rig := newRegisterRig(t)
	stale := demoProject()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rig.reg.Reconcile(context.Background(), stale) // #nosec G104 -- the counts below judge it
		}()
	}
	wg.Wait()
	if len(rig.fj.minted) != 4 || len(rig.fj.tokens) != 4 || rig.kv.reads != 1 {
		t.Fatalf("minted %v, %d tokens live, kv reads %d; want 4, 4, 1", rig.fj.minted, len(rig.fj.tokens), rig.kv.reads)
	}
}

// A factory project's last reconcile error stays until a clean reconcile:
// the console shows it while the project is not ready.
func TestReconcileErrorKeptUntilClean(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	rig.kube.fail = "demo-prod"
	if err := rig.reg.Reconcile(ctx, demoProject()); err == nil {
		t.Fatal("the failing namespace passed")
	}
	if got := rig.reg.ReconcileError("demo-app"); !strings.Contains(got, "demo-prod") || !strings.Contains(got, "the API server refused") {
		t.Fatalf("ReconcileError = %q", got)
	}
	rig.kube.fail = ""
	rig.fj.branches["main"] = "c0ffee"
	if err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"]); err != nil {
		t.Fatal(err)
	}
	if got := rig.reg.ReconcileError("demo-app"); got != "" {
		t.Fatalf("after a clean reconcile, ReconcileError = %q", got)
	}
}

// A registered project's reconcile, alone or in ReconcileAll, never reads
// the credential.
func TestRegisteredProjectReadsNoCredential(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	rig.kv.reads = 0
	stored := rig.store.m["demo-app"]
	if err := rig.reg.Reconcile(ctx, stored); err != nil {
		t.Fatal(err)
	}
	rig.reg.ReconcileAll(ctx)
	if rig.kv.reads != 0 {
		t.Fatalf("kv reads %d, want 0", rig.kv.reads)
	}
}

// The admin's password reaches no log line, no error and no kept state,
// on a registration that works and on ones that fail.
func TestAdminPasswordNeverLogged(t *testing.T) {
	const password = "pw-7f3a-never-logged"
	var said []string
	for _, c := range []struct {
		name  string
		setup func(rig *registerRig)
	}{
		{"works", func(*registerRig) {}},
		{"foreign organisation", func(rig *registerRig) { rig.fj.org, rig.fj.orgOwner = true, "mallory" }},
		{"no username", func(rig *registerRig) { delete(rig.kv.data, "username") }},
	} {
		rig := newRegisterRig(t)
		rig.kv.data["password"] = password
		c.setup(rig)
		_, err := rig.reg.Register(context.Background(), "demo-app", false)
		if c.name != "works" && err == nil {
			t.Fatalf("%s: registered", c.name)
		}
		said = append(said, c.name, rig.logs.String(), fmt.Sprint(err), rig.reg.ReconcileError("demo-app"), fmt.Sprintf("%+v", rig.store.m["demo-app"]))
	}
	if all := strings.Join(said, "\n"); strings.Contains(all, password) {
		t.Fatalf("the password was said:\n%s", all)
	}
}

// A merge rule change reaches main's protection, a quota change the
// ResourceQuotas, a new webhook secret (a re-seed) the hook, a new
// approver the collaborators and whitelists.
func TestRegistrationFollowsSettings(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, "demo-app", false)
	if err != nil {
		t.Fatal(err)
	}
	// the file changed: merge rule human, a smaller quota
	changed := rig.store.m[p.Name]
	changed.MergeRule, changed.Quota = "human", policy.Quota{CPU: "1", Memory: "2Gi", Storage: "5Gi"}
	rig.store.m[p.Name] = changed
	rig.reg.WebhookSecret = "re-seeded"
	rig.fj.users["alice"] = true
	rig.reg.Policy = approversTree("sebastian", "alice")
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
	main := rig.fj.protection["main"]
	if main["required_approvals"] != float64(1) || rig.fj.hookSecret != "re-seeded" ||
		!reflect.DeepEqual(main["push_whitelist_usernames"], []any{"sebastian", "alice", "factory-merge"}) {
		t.Errorf("protection %v, hook secret %q", main, rig.fj.hookSecret)
	}
	if rig.store.m[p.Name].HookSecretHash != HashToken("re-seeded") {
		t.Error("the new hook secret's hash was not stored")
	}
}

// approversTree is a configuration whose policy names approvers and whose
// project files name none.
func approversTree(approvers ...string) func() policy.Tree {
	return func() policy.Tree { return policy.Tree{Document: policy.Document{Approvers: approvers}} }
}

// demoApprovers is a configuration whose policy names sebastian and whose
// demo-app file names approvers (ruling 254).
func demoApprovers(approvers ...string) func() policy.Tree {
	return func() policy.Tree {
		return policy.Tree{Document: policy.Document{Approvers: []string{"sebastian"}},
			Projects: map[string]policy.ProjectConfig{"demo-app": {Repository: "demo-app/demo-app", Approvers: approvers}}}
	}
}

// A project file's approvers, not the policy's, become the project's
// collaborators and main's whitelists, so Forgejo takes their approving
// review for factory-merge's merge (ruling 254).
func TestRegistrationTakesTheProjectsApprovers(t *testing.T) {
	rig := newRegisterRig(t)
	rig.fj.users["bob"] = true
	rig.reg.Policy = demoApprovers("bob")
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatal(err)
	}
	main := rig.fj.protection["main"]
	if !reflect.DeepEqual(main["approvals_whitelist_username"], []any{"bob"}) || !reflect.DeepEqual(main["push_whitelist_usernames"], []any{"bob", "factory-merge"}) ||
		!reflect.DeepEqual(main["merge_whitelist_usernames"], []any{"bob", "factory-merge"}) {
		t.Errorf("protection main = %v, want bob whitelisted", main)
	}
	if rig.fj.collabs["bob"] != "write" || rig.fj.collabs["sebastian"] != "" {
		t.Errorf("collaborators = %v, want bob and not the policy's sebastian", rig.fj.collabs)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: collaborator bob: created\n") {
		t.Errorf("log lacks bob's collaboration:\n%s", rig.logs)
	}
}

// Backlog 24 (ruling 203), members (ruling 254): factoryd owns who
// administers a factory project's repository. A member (permission write)
// stays; a collaborator with admin who is not the bot, the merge user or
// an approver of the project leaves.
func TestReconcileRemovesOnlyAdminsItDoesNotKeep(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	rig.fj.users["bob"] = true
	rig.reg.Policy = demoApprovers("bob")
	p, err := rig.reg.Register(ctx, "demo-app", false)
	if err != nil {
		t.Fatal(err)
	}
	rig.fj.collabs = map[string]string{"factory-agent": "admin", "factory-merge": "admin", "bob": "write", "mallory": "admin", "dave": "write"}
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, rig.store.m[p.Name]); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"factory-agent": "admin", "factory-merge": "admin", "bob": "write", "dave": "write"}; !reflect.DeepEqual(rig.fj.collabs, want) {
		t.Fatalf("collaborators = %v, want %v", rig.fj.collabs, want)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: collaborators: removed mallory\n") {
		t.Errorf("log lacks mallory's removal:\n%s", rig.logs)
	}
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, rig.store.m[p.Name]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: collaborators: no change\n") {
		t.Fatalf("second pass:\n%s", rig.logs)
	}
}

// A deploy key or DEPLOY_KEY that went missing is made anew, both halves.
func TestDeployKeyHeals(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, "demo-app", false)
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

// Backlog 20 (ruling 203): a project file's egress hosts are all its
// namespaces reach outside the cluster, on 443 only, by name; Cilium
// learns a name's addresses from the DNS answers its proxy sees, so the
// DNS rule gets a visibility rule that still allows every lookup.
func TestProjectEgressReachesOnlyTheFilesHosts(t *testing.T) {
	rig := newRegisterRig(t)
	p := demoProject()
	p.Egress = []string{"api.example.com", "hooks.example.org"}
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cnp := rig.kube.object(t, "/apis/cilium.io/v2/namespaces/demo-staging/ciliumnetworkpolicies/project")
	b, _ := json.Marshal(cnp["spec"].(map[string]any)["egress"])
	for _, want := range []string{
		`{"toFQDNs":[{"matchName":"api.example.com"},{"matchName":"hooks.example.org"}],"toPorts":[{"ports":[{"port":"443","protocol":"TCP"}]}]}`,
		`"toPorts":[{"ports":[{"port":"53","protocol":"ANY"}],"rules":{"dns":[{"matchPattern":"*"}]}}]`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("egress lacks %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), "world") {
		t.Errorf("egress still reaches world: %s", b)
	}
}

// The staging namespace admits the project's own CI runners on 8080, the
// smoke test's way in; its other rules and prod's policy stay as they were.
func TestStagingAdmitsTheProjectsCIRunners(t *testing.T) {
	p := Project{Name: "uptime", Namespaces: []string{"uptime-staging", "uptime-prod"},
		Environments: []Environment{{Namespace: "uptime-staging", Path: "deploy/staging"}, {Namespace: "uptime-prod", Path: "deploy/prod"}}}
	const before = `{"egress":[{"toEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"uptime-staging"}}]},{"toEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s:k8s-app":"kube-dns"}}],"toPorts":[{"ports":[{"port":"53","protocol":"ANY"}]}]},{"toEntities":["kube-apiserver"]},{"toEndpoints":[{"matchLabels":{"app.kubernetes.io/component":"otel-collector","k8s:io.kubernetes.pod.namespace":"platform"}}],"toPorts":[{"ports":[{"port":"4318","protocol":"TCP"}]}]}],"endpointSelector":{},"ingress":[{"fromEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"uptime-staging"}}]},{"fromEntities":["ingress"]},{"fromEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"cnpg-system"}}],"toPorts":[{"ports":[{"port":"8000","protocol":"TCP"},{"port":"5432","protocol":"TCP"}]}]}]}`
	want := strings.TrimSuffix(before, "]}") + `,{"fromEndpoints":[{"matchLabels":{"app.kubernetes.io/name":"ci-runner","k8s:io.cilium.k8s.namespace.labels.factory.ai/ci-project":"uptime"}}],"toPorts":[{"ports":[{"port":"8080","protocol":"TCP"}]}]}]}`
	if got := string(mustJSON(ProjectNetworkPolicy(p, "uptime-staging"))); got != want {
		t.Fatalf("staging:\n%s\nwant\n%s", got, want)
	}
	if got, want := string(mustJSON(ProjectNetworkPolicy(p, "uptime-prod"))), strings.ReplaceAll(before, "uptime-staging", "uptime-prod"); got != want {
		t.Fatalf("prod:\n%s\nwant\n%s", got, want)
	}
}

// Registration mirrors the staging namespace's generated secrets into the
// Actions secret STAGING_SECRETS, KEY=value lines, and the staging
// Service's address into STAGING_URL; a reconcile finding both as it left
// them writes nothing, a changed value is written again, and no value
// reaches the log.
func TestStagingSecretsMirrored(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p := demoProject()
	p.Secrets = []string{"demo-admin/ADMIN_TOKEN"}
	rig.store.m[p.Name] = p
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	value := rig.kube.secrets["demo-staging/demo-admin"]["ADMIN_TOKEN"]
	if len(value) != 64 || value == rig.kube.secrets["demo-prod/demo-admin"]["ADMIN_TOKEN"] {
		t.Fatalf("generated staging %q, prod %q", value, rig.kube.secrets["demo-prod/demo-admin"]["ADMIN_TOKEN"])
	}
	if got := rig.fj.secrets["STAGING_SECRETS"]; got != "ADMIN_TOKEN='"+value+"'\n" {
		t.Fatalf("STAGING_SECRETS = %q", got)
	}
	if got := rig.fj.secrets["STAGING_URL"]; got != "http://demo-app.demo-staging.svc.cluster.local:8080" {
		t.Fatalf("STAGING_URL = %q", got)
	}
	staged := func() (n int) {
		for _, w := range rig.fj.writes {
			if strings.Contains(w, "/actions/secrets/STAGING_") {
				n++
			}
		}
		return n
	}
	if n := staged(); n != 2 {
		t.Fatalf("%d STAGING_ writes, want 2: %v", n, rig.fj.writes)
	}

	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	if n := staged(); n != 2 {
		t.Fatalf("an unchanged value written again: %v", rig.fj.writes)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: actions secrets STAGING_URL, STAGING_SECRETS: no change\n") {
		t.Fatalf("second run:\n%s", rig.logs)
	}

	rig.kube.secrets["demo-staging/demo-admin"]["ADMIN_TOKEN"] = "set-by-hand"
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	if got := rig.fj.secrets["STAGING_SECRETS"]; got != "ADMIN_TOKEN='set-by-hand'\n" || staged() != 4 ||
		!strings.Contains(rig.logs.String(), "project demo-app: actions secrets STAGING_URL, STAGING_SECRETS: updated\n") {
		t.Fatalf("after a changed value: %q, %d writes\n%s", got, staged(), rig.logs)
	}
	for _, v := range []string{value, "set-by-hand"} {
		if strings.Contains(rig.logs.String(), v) {
			t.Fatalf("the log holds the value %s:\n%s", v, rig.logs)
		}
	}
}

// The smoke test sources STAGING_SECRETS as shell: a value is quoted, and
// a key that is no shell name gets a comment line and no value.
func TestStagingSecretsQuoted(t *testing.T) {
	rig := newRegisterRig(t)
	p := demoProject()
	p.Secrets = []string{"demo-admin/ADMIN_TOKEN", "demo-db/db-password"}
	rig.store.m[p.Name] = p
	rig.kube.secrets["demo-staging/demo-admin"] = map[string]string{"ADMIN_TOKEN": "it's $x"}
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatal(err)
	}
	got := rig.fj.secrets["STAGING_SECRETS"]
	if want := "ADMIN_TOKEN='it'\\''s $x'\n# demo-db/db-password: db-password is not a shell variable name\n"; got != want {
		t.Fatalf("STAGING_SECRETS = %q, want %q", got, want)
	}
	db := rig.kube.secrets["demo-staging/demo-db"]["db-password"]
	if len(db) != 64 || strings.Contains(got, db) || strings.Contains(rig.logs.String(), db) {
		t.Fatalf("db-password %q: in the secret %v, in the log %v", db, strings.Contains(got, db), strings.Contains(rig.logs.String(), db))
	}
}

// Forgejo refuses an empty Actions secret, so a project without secrets
// gets one comment line, once.
func TestStagingSecretsWithoutSecrets(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p := demoProject()
	p.Secrets = nil
	rig.store.m[p.Name] = p
	for range 2 {
		if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
			t.Fatal(err)
		}
	}
	if got := rig.fj.secrets["STAGING_SECRETS"]; got != "# projects/demo-app.yaml names no secrets\n" {
		t.Fatalf("STAGING_SECRETS = %q", got)
	}
	if n := strings.Count(strings.Join(rig.fj.writes, "\n"), "/actions/secrets/STAGING_SECRETS by"); n != 1 {
		t.Fatalf("STAGING_SECRETS written %d times: %v", n, rig.fj.writes)
	}
}

// STAGING_SECRETS takes, after the file's entries, every key of each
// Opaque Secret Flux applied for the project's staging Kustomization, by
// Secret name and key; not a Secret of another type, of another
// Kustomization or without Flux's labels. The poll writes it again when a
// value changes or a Secret comes, and not when nothing changed or the
// project is not ready.
func TestStagingSecretsTakeFluxSecrets(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p := demoProject()
	p.Secrets, p.ReadyAt = []string{"demo-db/DB_PASSWORD"}, time.Now()
	rig.store.m[p.Name] = p
	flux := map[string]string{fluxNameLabel: "demo-app", fluxNamespaceLabel: "demo-staging"}
	rig.kube.putSecret(t, "demo-staging", "admin-token", "Opaque", flux, map[string]string{"ADMIN_TOKEN": "x"})
	rig.kube.putSecret(t, "demo-staging", "registry", "kubernetes.io/dockerconfigjson", flux, map[string]string{"REGISTRY_AUTH": "r"})
	rig.kube.putSecret(t, "demo-staging", "by-hand", "Opaque", nil, map[string]string{"BY_HAND": "h"})
	rig.kube.putSecret(t, "demo-staging", "other", "Opaque", map[string]string{fluxNameLabel: "other", fluxNamespaceLabel: "demo-staging"},
		map[string]string{"OTHER": "o"})
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	db := rig.kube.secrets["demo-staging/demo-db"]["DB_PASSWORD"]
	staged := func(want string, writes int) {
		t.Helper()
		got, n := rig.fj.secrets["STAGING_SECRETS"], strings.Count(strings.Join(rig.fj.writes, "\n"), "/actions/secrets/STAGING_SECRETS by")
		if got != want || n != writes {
			t.Fatalf("STAGING_SECRETS = %q, written %d times; want %q, %d\n%s", got, n, want, writes, rig.logs)
		}
	}
	if len(db) != 64 {
		t.Fatalf("generated DB_PASSWORD %q", db)
	}
	staged("DB_PASSWORD='"+db+"'\nADMIN_TOKEN='x'\n", 1)

	rig.kube.secrets["demo-staging/admin-token"]["ADMIN_TOKEN"] = "y"
	rig.reg.poll(ctx)
	staged("DB_PASSWORD='"+db+"'\nADMIN_TOKEN='y'\n", 2)
	if !strings.Contains(rig.logs.String(), "project demo-app: actions secrets STAGING_URL, STAGING_SECRETS: updated\n") {
		t.Fatalf("the poll's write is not logged:\n%s", rig.logs)
	}
	rig.reg.poll(ctx)
	staged("DB_PASSWORD='"+db+"'\nADMIN_TOKEN='y'\n", 2)

	rig.kube.putSecret(t, "demo-staging", "api-keys", "Opaque", flux, map[string]string{"B_KEY": "b", "A_KEY": "a", "bad-key": "z"})
	rig.reg.poll(ctx)
	staged("DB_PASSWORD='"+db+"'\nADMIN_TOKEN='y'\nA_KEY='a'\nB_KEY='b'\n# api-keys/bad-key: bad-key is not a shell variable name\n", 3)
	if strings.Contains(rig.logs.String(), db) {
		t.Fatalf("the log holds DB_PASSWORD's value:\n%s", rig.logs)
	}
	p = rig.store.m["demo-app"]
	p.ReadyAt = time.Time{}
	rig.store.m["demo-app"] = p
	rig.kube.secrets["demo-staging/admin-token"]["ADMIN_TOKEN"] = "z"
	rig.reg.poll(ctx)
	staged("DB_PASSWORD='"+db+"'\nADMIN_TOKEN='y'\nA_KEY='a'\nB_KEY='b'\n# api-keys/bad-key: bad-key is not a shell variable name\n", 3)
}

// A key the file's entries name and a Flux Secret holds too is written
// once, with the file's value; a file that names none takes the Flux
// Secret's line alone.
func TestStagingSecretsFileWins(t *testing.T) {
	for _, secrets := range [][]string{{"demo-admin/ADMIN_TOKEN"}, nil} {
		rig := newRegisterRig(t)
		p := demoProject()
		p.Secrets = secrets
		rig.store.m[p.Name] = p
		rig.kube.putSecret(t, "demo-staging", "admin-token", "Opaque", map[string]string{fluxNameLabel: "demo-app", fluxNamespaceLabel: "demo-staging"},
			map[string]string{"ADMIN_TOKEN": "x"})
		if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
			t.Fatalf("%v\n%s", err, rig.logs)
		}
		want := "ADMIN_TOKEN='x'\n"
		if v := rig.kube.secrets["demo-staging/demo-admin"]["ADMIN_TOKEN"]; secrets != nil {
			if len(v) != 64 {
				t.Fatalf("generated ADMIN_TOKEN %q", v)
			}
			want = "ADMIN_TOKEN='" + v + "'\n"
		}
		if got := rig.fj.secrets["STAGING_SECRETS"]; got != want {
			t.Fatalf("secrets %v: STAGING_SECRETS = %q, want %q", secrets, got, want)
		}
	}
}

// Two reconciles racing on a missing FACTORY_READ_TOKEN: only the one that
// stores its hash sets the secret, so they never disagree (m5).
func TestReadTokenRace(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, "demo-app", false)
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

// A row without a file is left alone.
func TestReconcileSkipsARowWithoutAFile(t *testing.T) {
	rig := newRegisterRig(t)
	p := Project{Name: "gone", GitToken: "g"}
	if err := rig.reg.Reconcile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(rig.fj.writes) != 0 || len(rig.kube.changes) != 0 ||
		!strings.Contains(rig.logs.String(), "project gone: no file projects/gone.yaml in the configuration: nothing reconciled") {
		t.Fatalf("a row without a file touched: %v %v\n%s", rig.fj.writes, rig.kube.changes, rig.logs)
	}
}

// An organisation of that name that factory-projects does not own is
// someone else's: registration stops before anything is written.
func TestRegisterRefusesAForeignOrganisation(t *testing.T) {
	rig := newRegisterRig(t)
	rig.fj.org, rig.fj.orgOwner = true, "mallory"
	_, err := rig.reg.Register(context.Background(), "demo-app", false)
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
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	n := len(rig.fj.minted)
	if _, err := rig.reg.Register(ctx, "demo-app", true); err != nil {
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
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err == nil ||
		!strings.Contains(err.Error(), "a user ci-demo-app exists that registration did not make") {
		t.Fatalf("foreign user = %v", err)
	}
	if len(rig.fj.minted) != 0 {
		t.Fatalf("minted for a foreign user: %v", rig.fj.minted)
	}

	rig = newRegisterRig(t)
	rig.fj.team = []string{}
	rig.fj.teamUnits = map[string]string{"repo.code": "write", "repo.packages": "write"}
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err == nil ||
		!strings.Contains(err.Error(), "a team ci exists in demo-app with more than package write") {
		t.Fatalf("foreign team = %v", err)
	}
	if len(rig.fj.minted) != 0 || len(rig.fj.team) != 0 {
		t.Fatalf("used a foreign team: minted %v, members %v", rig.fj.minted, rig.fj.team)
	}
}

// A new project's first push to main makes deploy through the project's
// reconcile; later pushes, and repositories of no factory project, do
// nothing.
func TestMainPushedMakesDeploy(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rig.fj.hooks[0]["events"].([]any), any("push")) {
		t.Fatalf("webhook events %v lack push", rig.fj.hooks[0]["events"])
	}
	rig.fj.branches["main"] = "c0ffee"
	rig.logs.Reset()
	if err := rig.reg.MainPushed(ctx, "demo-app/demo-app"); err != nil {
		t.Fatal(err)
	}
	got := rig.logs.String()
	for _, want := range []string{"project demo-app: main pushed, no deploy branch yet: reconcile\n",
		"project demo-app: branch deploy: created\n", "project demo-app: protection deploy: created\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if rig.fj.branches["deploy"] != "c0ffee" {
		t.Fatalf("deploy = %q", rig.fj.branches["deploy"])
	}
	writes := len(rig.fj.writes)
	rig.logs.Reset()
	for _, repo := range []string{"demo-app/demo-app", "sf/ai-software-factory-code", "other/other"} {
		if err := rig.reg.MainPushed(ctx, repo); err != nil {
			t.Fatalf("%s: %v", repo, err)
		}
	}
	if rig.logs.Len() != 0 || len(rig.fj.writes) != writes {
		t.Fatalf("later pushes: log %q, writes %v", rig.logs, rig.fj.writes[writes:])
	}
}

// Item 69: a Forgejo rebuilt without its git storage keeps deploy's
// protection but not the branch, and Forgejo refuses to create a branch
// its protection covers. That is no failure: roll.sh starts deploy on the
// next roll, and the steps after the branch (fork, labels, deploy key,
// FACTORY_READ_TOKEN, FACTORY_SSH_HOST_KEY) still run.
func TestReconcileGoesOnWhenDeployProtectionRefusesTheBranch(t *testing.T) {
	rig := newRegisterRig(t)
	key := "ssh-ed25519 AAAAone"
	rig.reg.HostKey = func(context.Context) (string, error) { return key, nil }
	rig.fj.branches["main"] = "c0ffee"
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, "demo-app", false)
	if err != nil {
		t.Fatal(err)
	}
	if rig.fj.protection["deploy"] == nil {
		t.Fatal("no deploy protection after the first run")
	}
	delete(rig.fj.branches, "deploy")
	key = "ssh-ed25519 AAAAtwo"
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, rig.store.m[p.Name]); err != nil {
		t.Fatalf("Reconcile = %v\n%s", err, rig.logs)
	}
	got := rig.logs.String()
	for _, want := range []string{
		"project demo-app: branch deploy: missing: its protection refuses the create; the next roll starts it\n",
		"project demo-app: protection deploy: no change\n",
		"project demo-app: fork factory-agent/demo-app: ready\n",
		"project demo-app: labels: no change\n",
		"project demo-app: deploy key: no change\n",
		"project demo-app: actions secret FACTORY_READ_TOKEN: no change\n",
		"project demo-app: actions secret FACTORY_SSH_HOST_KEY: updated\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if rig.fj.secrets["FACTORY_SSH_HOST_KEY"] != key {
		t.Fatalf("FACTORY_SSH_HOST_KEY = %q, want %q", rig.fj.secrets["FACTORY_SSH_HOST_KEY"], key)
	}
	// the protection step ran on deploy: the NoMain skip would log waiting
	if strings.Contains(got, "project demo-app: protection deploy: "+string(NoMain)+"\n") {
		t.Errorf("the protection step skipped deploy as NoMain:\n%s", got)
	}
}

// Item 25: a hand edit in Forgejo (main's protection deleted) is set back
// by the next periodic reconcile, with no boot, console change or new
// policy version in between.
func TestReconcileLoopSetsBackAHandEdit(t *testing.T) {
	rig := newRegisterRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	rig.logs.Reset()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.reg.ReconcileLoop(ctx, func() int64 { return 7 }, time.Millisecond, 50*time.Millisecond)
	}()
	time.Sleep(100 * time.Millisecond)
	rig.fj.mu.Lock()
	delete(rig.fj.protection, "main")
	rig.fj.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if rig.fj.protection["main"] == nil {
		t.Fatalf("main's protection not set back:\n%s", rig.logs)
	}
	if n := strings.Count(rig.logs.String(), "project demo-app: protection main: created\n"); n != 1 {
		t.Fatalf("protection main created %d times, want once:\n%s", n, rig.logs)
	}
}

// Ruling 179: a seed project's reconcile keeps the labels and (agents
// coder) the bot's fork as the bot, reads the webhook with the project's
// read token and each namespace's factory/project label, reports them, and
// creates nothing else: no factory-projects call, no hook, no cluster
// object.
func TestSeedProjectIsReadAndReported(t *testing.T) {
	rig := newRegisterRig(t)
	rig.reg.Forgejo.Seeded = testSeeded
	rig.fj.repo = true
	rig.kube.labels = map[string]map[string]string{"demo-staging": {ProjectLabel: "demo-app", ObjectsLabel: "seed"},
		"demo-prod": {ProjectLabel: "other", ObjectsLabel: "seed"}, "demo-dev": {ProjectLabel: "demo-app"}, "demo-bare": {}}
	p := Project{Name: "demo-app", Repository: "demo-app/demo-app", MergeRule: "human", Objects: "seed", Agents: "coder", File: true,
		Namespaces: []string{"demo-staging", "demo-prod", "demo-dev", "demo-qa", "demo-bare"},
		Tokens:     policy.SeedTokens{Read: policy.SecretCIRegistrar, Actions: policy.SecretCIRegistrar, Merge: policy.SecretMerge}}
	ctx := context.Background()
	if err := rig.reg.Reconcile(ctx, p); err != nil {
		t.Fatal(err)
	}
	logs := rig.logs.String()
	for _, want := range []string{
		"project demo-app: labels: created agent, bug, human, improvement, noise, priority/p0,",
		"project demo-app: fork factory-agent/demo-app: ready\n",
		"project demo-app: webhook: missing: the seed makes it, factoryd never does\n",
		"project demo-app: namespace demo-staging labels factory/project, factory/objects: present\n",
		`project demo-app: namespace demo-prod labels factory/project, factory/objects: wrong: "other", "seed", want "demo-app", "seed"; the infra repository sets them` + "\n",
		`project demo-app: namespace demo-dev labels factory/project, factory/objects: wrong: "demo-app", "", want "demo-app", "seed"; the infra repository sets them` + "\n",
		"project demo-app: namespace demo-qa labels factory/project, factory/objects: missing: no namespace demo-qa; the infra repository makes it\n",
		"project demo-app: namespace demo-bare labels factory/project, factory/objects: missing: the infra repository sets them\n",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	for _, w := range rig.fj.writes {
		if !strings.HasSuffix(w, " by basic factory-agent") || !strings.Contains(w, "/labels") && !strings.Contains(w, "/forks") {
			t.Errorf("a seed project's reconcile wrote %s", w)
		}
	}
	if len(rig.fj.labels) != len(BootLabels) || rig.fj.fork == nil || len(rig.fj.hooks) != 0 || len(rig.kube.changes) != 0 {
		t.Fatalf("labels %d, fork %v, hooks %v, cluster %v", len(rig.fj.labels), rig.fj.fork, rig.fj.hooks, rig.kube.changes)
	}
	if !slices.Equal(rig.fj.hookReads, []string{"token registrar-tok"}) {
		t.Fatalf("hooks read with %v, want the project's read token", rig.fj.hookReads)
	}

	// the seed made the hook: present; the labels are there
	rig.fj.hooks = []map[string]any{{"id": 1, "active": true, "config": map[string]any{"url": rig.reg.WebhookURL}}}
	rig.logs.Reset()
	writes := len(rig.fj.writes)
	if err := rig.reg.Reconcile(ctx, p); err != nil {
		t.Fatal(err)
	}
	if logs := rig.logs.String(); !strings.Contains(logs, "project demo-app: webhook: present\n") || !strings.Contains(logs, "project demo-app: labels: no change\n") ||
		len(rig.fj.writes) != writes {
		t.Fatalf("second run wrote %v:\n%s", rig.fj.writes[writes:], logs)
	}

	// agents none: no fork; a read token without admin cannot see the
	// hooks: one line, no error, the reconcile ends clean
	p.Agents, p.Tokens = "none", policy.SeedTokens{Read: policy.SecretConfig}
	rig.fj.fork = nil
	rig.logs.Reset()
	err := rig.reg.Reconcile(ctx, p)
	if logs := rig.logs.String(); !strings.Contains(logs, "project demo-app: fork factory-agent/demo-app: none: agents none\n") || rig.fj.fork != nil ||
		err != nil || !strings.Contains(logs, "project demo-app: webhook: not checkable, no admin credential on demo-app/demo-app\n") {
		t.Fatalf("agents none: err %v, fork %v:\n%s", err, rig.fj.fork, logs)
	}
	if rig.fj.hookReads[len(rig.fj.hookReads)-1] != "token config-tok" {
		t.Fatalf("hooks read with %v", rig.fj.hookReads)
	}
	// m1: only a 403 is "not checkable"; a revoked token (401) is an error
	rig.reg.Forgejo.Seeded = map[string]string{policy.SecretConfig: "revoked-tok"}
	if err := rig.reg.Reconcile(ctx, p); err == nil || !strings.Contains(err.Error(), "webhook: cannot read the hooks with forgejo-config") {
		t.Fatalf("a revoked read token: %v", err)
	}
}

// ReconcileAll gives each file its row, logs it once, and passes a row
// without a file by.
func TestReconcileAllMakesTheRows(t *testing.T) {
	rig := newRegisterRig(t)
	rig.reg.Forgejo.Seeded = testSeeded
	rig.fj.repo = true
	rig.store.m["gone"] = Project{Name: "gone", GitToken: "g"}
	rig.store.rows = map[string]bool{"gone": true}
	ctx := context.Background()
	rig.reg.ReconcileAll(ctx)
	if !rig.store.rows["demo-app"] || !strings.Contains(rig.logs.String(), "project demo-app: row: created from projects/demo-app.yaml\n") ||
		!strings.Contains(rig.logs.String(), "project gone: no file projects/gone.yaml") {
		t.Fatalf("rows %v:\n%s", rig.store.rows, rig.logs)
	}
	rig.logs.Reset()
	rig.reg.ReconcileAll(ctx)
	if strings.Contains(rig.logs.String(), "row: created") {
		t.Fatalf("a second pass created a row again:\n%s", rig.logs)
	}
}
