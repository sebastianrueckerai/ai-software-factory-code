package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a mirror
// row answers its token only to the registrar's read; a second put
// resets its state and sync.
func TestPGStoreMirrors(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	if err := s.DeleteMirror(ctx, "demo-app", MirrorExport); err != nil {
		t.Fatal(err)
	}
	read := func(get func(context.Context, string, string) (Mirror, bool, error)) Mirror {
		t.Helper()
		m, ok, err := get(ctx, "demo-app", MirrorExport)
		if err != nil || !ok {
			t.Fatalf("read = %v, %v", ok, err)
		}
		m.LastUpdate = m.LastUpdate.UTC()
		return m
	}
	row := Mirror{Project: "demo-app", Direction: MirrorExport, Remote: "acme/app", Username: "u", Token: "ghp-s3cret", State: "created"}
	if err := s.PutMirror(ctx, row); err != nil {
		t.Fatal(err)
	}
	if got := read(s.MirrorToken); got != row {
		t.Fatalf("MirrorToken = %+v, want %+v", got, row)
	}
	row.Token = ""
	if got := read(s.Mirror); got != row {
		t.Fatalf("Mirror = %+v, want %+v without the token", got, row)
	}
	synced := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	if err := s.SetMirrorState(ctx, "demo-app", MirrorExport, "failed: auth", "auth", synced); err != nil {
		t.Fatal(err)
	}
	row.State, row.LastError, row.LastUpdate = "failed: auth", "auth", synced
	if got := read(s.Mirror); got != row {
		t.Fatalf("after SetMirrorState: %+v, want %+v", got, row)
	}
	again := Mirror{Project: "demo-app", Direction: MirrorExport, Remote: "acme/other", Username: "v", Token: "ghp-n3w", State: "created"}
	if err := s.PutMirror(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got := read(s.MirrorToken); got != again {
		t.Fatalf("after a second PutMirror: %+v, want %+v", got, again)
	}
	if err := s.DeleteMirror(ctx, "demo-app", MirrorExport); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Mirror(ctx, "demo-app", MirrorExport); err != nil || ok {
		t.Fatalf("Mirror after DeleteMirror = %v, %v; want none", ok, err)
	}
}

// mirrorForgejo answers a repository's push mirrors and records every
// call as "METHOD path auth body".
type mirrorForgejo struct {
	mu      sync.Mutex
	mirrors string // the GET answer's first page
	calls   []string
}

func (f *mirrorForgejo) server(t *testing.T) *Forgejo {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body)))
		switch r.URL.Query().Get("page") {
		case "1":
			io.WriteString(w, f.mirrors)
		case "":
			io.WriteString(w, "{}")
		default:
			io.WriteString(w, "[]")
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, ProjectsToken: "projects-tok", HTTP: srv.Client()}
}

const twoMirrors = `[{"remote_name":"remote_mirror_a","remote_address":"https://u:ghp-s3cret@github.com/acme/app.git","last_update":"2026-10-11T09:00:00Z","last_error":""},
	{"remote_name":"remote_mirror_b","remote_address":"https://github.com/acme/old.git","last_update":"1970-01-01T00:00:00Z","last_error":"remote: Invalid username or token."}]`

// The push mirror calls go through the projects token: the add posts the
// remote, its login, an 8 h interval and a sync on every commit; the list
// answers each address without its credentials and a never-synced mirror
// with no time.
func TestPushMirrorCalls(t *testing.T) {
	fj := &mirrorForgejo{mirrors: twoMirrors}
	f := fj.server(t)
	ctx, repo := context.Background(), Repo{"demo-app", "demo-app"}
	if err := errors.Join(f.AddPushMirror(ctx, repo, "https://github.com/acme/app.git", "u", "tok"), f.SyncPushMirrors(ctx, repo),
		f.DeletePushMirror(ctx, repo, "remote_mirror_b")); err != nil {
		t.Fatal(err)
	}
	got, err := f.PushMirrors(ctx, repo)
	want := []PushMirror{{RemoteName: "remote_mirror_a", RemoteAddress: "https://github.com/acme/app.git", LastUpdate: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)},
		{RemoteName: "remote_mirror_b", RemoteAddress: "https://github.com/acme/old.git", LastError: "remote: Invalid username or token."}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("PushMirrors = %+v, %v\nwant %+v", got, err, want)
	}
	const path = "/api/v1/repos/demo-app/demo-app/push_mirrors"
	wantCalls := []string{
		"POST " + path + ` token projects-tok {"interval":"8h","remote_address":"https://github.com/acme/app.git","remote_password":"tok","remote_username":"u","sync_on_commit":true}`,
		"POST /api/v1/repos/demo-app/demo-app/push_mirrors-sync token projects-tok",
		"DELETE " + path + "/remote_mirror_b token projects-tok",
		"GET " + path + " token projects-tok", "GET " + path + " token projects-tok"}
	if !slices.Equal(fj.calls, wantCalls) {
		t.Fatalf("calls\n%s\nwant\n%s", strings.Join(fj.calls, "\n"), strings.Join(wantCalls, "\n"))
	}
}

// memMirrors are mirror rows by project, recording each state set as
// "project direction: state | last error | last update".
type memMirrors struct {
	mu   sync.Mutex
	rows map[string]Mirror
	set  []string
}

func (m *memMirrors) MirrorToken(_ context.Context, project, direction string) (Mirror, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[project+" "+direction]
	return row, ok, nil
}

func (m *memMirrors) SetMirrorState(_ context.Context, project, direction, state, lastError string, lastUpdate time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := "never"
	if !lastUpdate.IsZero() {
		at = lastUpdate.UTC().Format(time.RFC3339)
	}
	m.set = append(m.set, project+" "+direction+": "+state+" | "+lastError+" | "+at)
	if row, ok := m.rows[project+" "+direction]; ok {
		row.State, row.LastError, row.LastUpdate = state, lastError, lastUpdate
		m.rows[project+" "+direction] = row
	}
	return nil
}

// DeleteStaleImports keeps every row: the rows' age is Postgres's, so
// TestPollDeletesStaleImports runs on it.
func (m *memMirrors) DeleteStaleImports(context.Context, []string, time.Time) ([]Mirror, error) {
	return nil, nil
}

func (m *memMirrors) SetMirrorToken(_ context.Context, project, direction, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[project+" "+direction]; ok {
		row.Token = token
		m.rows[project+" "+direction] = row
	}
	return nil
}

// The registrar reads the push mirror of each project with an export row,
// and only theirs, and records its state: synced with its age, failed
// with the first line of its error cut at 120 and the token taken out, or
// failed for a mirror Forgejo no longer holds; no log line holds the
// token.
func TestMirrorStates(t *testing.T) {
	logs := captureLog(t)
	last := time.Now().Add(-5*time.Minute - 10*time.Second).UTC().Truncate(time.Second)
	long := "fatal: Authentication failed for 'https://u:ghp-s3cret@github.com/acme/app.git/' " + strings.Repeat("x", 120) + "\nhint: more"
	errJSON, _ := json.Marshal(long)
	for _, c := range []struct {
		name, mirrors, want string
	}{
		{"synced", `[{"remote_name":"remote_mirror_old","remote_address":"https://github.com/acme/old.git","last_update":"2026-10-01T09:00:00Z","last_error":"x"},
			{"remote_name":"remote_mirror_a","remote_address":"https://github.com/acme/app.git","last_update":"` + last.Format(time.RFC3339) + `","last_error":""}]`,
			"demo-app export: synced 5 min ago |  | " + last.Format(time.RFC3339)},
		{"failed", `[{"remote_name":"remote_mirror_a","remote_address":"https://github.com/acme/app.git","last_update":"` + last.Format(time.RFC3339) + `","last_error":` + string(errJSON) + `}]`,
			"demo-app export: failed: " + textutil.Truncate("fatal: Authentication failed for 'https://u:[token]@github.com/acme/app.git/' "+strings.Repeat("x", 120), 120) +
				" | " + strings.ReplaceAll(long, "ghp-s3cret", "[token]") + " | " + last.Format(time.RFC3339)},
		{"never synced", `[{"remote_name":"remote_mirror_a","remote_address":"https://github.com/acme/app.git","last_update":"1970-01-01T00:00:00Z","last_error":""}]`,
			"demo-app export: created |  | never"},
		{"gone", `[]`, "demo-app export: failed: Forgejo holds no push mirror to acme/app |  | never"},
	} {
		fj := &mirrorForgejo{mirrors: c.mirrors}
		other := demoProject()
		other.Name, other.Repository = "linkshort", "linkshort/linkshort"
		mirrors := &memMirrors{rows: map[string]Mirror{"demo-app export": {Project: "demo-app", Direction: MirrorExport, Remote: "acme/app",
			Username: "u", Token: "ghp-s3cret", State: "created"}}}
		r := &Registrar{Forgejo: fj.server(t), Store: &memProjects{m: map[string]Project{"demo-app": demoProject(), "linkshort": other}}, Mirrors: mirrors}
		r.mirrorStates(context.Background())
		if !slices.Equal(mirrors.set, []string{c.want}) {
			t.Errorf("%s: states %q, want %q", c.name, mirrors.set, c.want)
		}
		const list = "GET /api/v1/repos/demo-app/demo-app/push_mirrors token projects-tok"
		if !slices.Equal(fj.calls, []string{list, list}) && !(c.mirrors == `[]` && slices.Equal(fj.calls, []string{list})) {
			t.Errorf("%s: calls %q, want demo-app's list only", c.name, fj.calls)
		}
	}
	if strings.Contains(logs.String(), "ghp-s3cret") {
		t.Fatalf("the token was logged:\n%s", logs)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): an import
// row may hold no token, a public source's or one the migrate used; an
// export row must hold one.
func TestPGStoreImportMirror(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	row := Mirror{Project: "linkshort", Direction: MirrorImport, Remote: "acme/app", Token: "ghp-s3cret", State: "pending"}
	if err := s.PutMirror(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMirrorToken(ctx, "linkshort", MirrorImport, ""); err != nil {
		t.Fatal(err)
	}
	row.Token = ""
	if got, ok, err := s.MirrorToken(ctx, "linkshort", MirrorImport); err != nil || !ok || got != row {
		t.Fatalf("after SetMirrorToken: %+v, %v, %v; want %+v", got, ok, err, row)
	}
	if err := s.PutMirror(ctx, Mirror{Project: "linkshort", Direction: MirrorImport, Remote: "acme/public", State: "pending"}); err != nil {
		t.Fatalf("an import without a token: %v", err)
	}
	if err := s.PutMirror(ctx, Mirror{Project: "linkshort", Direction: MirrorExport, Remote: "acme/app", Username: "u", State: "created"}); err == nil {
		t.Fatal("an export without a token was stored")
	}
	if err := s.DeleteMirror(ctx, "linkshort", MirrorImport); err != nil {
		t.Fatal(err)
	}
}

// importedProject is demo-app with a template holding the factory's
// workflows, roll script and conventions, a file under .forgejo/ that the
// GitHub repository has too, and files of its own.
func importedProject() Project {
	p := demoProject()
	p.Template = "ts-go"
	p.TemplateFiles = map[string]string{
		".forgejo/workflows/ci.yml":         "image: [[.Image]]\n",
		".forgejo/workflows/pr.yml":         "on: pull_request\n",
		".forgejo/roll.sh":                  "#!/bin/sh\n",
		".forgejo/CODEOWNERS":               "* @[[.Name]]\n",
		"AGENTS.md":                         "# [[.Name]]\n",
		"README.md":                         "# template\n",
		"deploy/staging/kustomization.yaml": "namespace: [[index .Namespaces \"deploy/staging\"]]\n",
	}
	return p
}

const importToken = "ghp_imp0rtT0ken"

// fakeGitHub answers GitHub's commits of a repository with status and
// body, recording each call as its path and query, and its Authorization.
type fakeGitHub struct {
	mu     sync.Mutex
	status int
	body   string
	calls  []string
}

// importRig is a registration of demo-app whose row imports acme/app,
// a GitHub repository with main, a CODEOWNERS and manifests of its own;
// GitHub shows its commits.
func importRig(t *testing.T) (*registerRig, *memMirrors) {
	rig := newRegisterRig(t)
	rig.store.m["demo-app"] = importedProject()
	mirrors := &memMirrors{rows: map[string]Mirror{"demo-app import": {Project: "demo-app", Direction: MirrorImport, Remote: "acme/app",
		Token: importToken, State: "pending"}}}
	rig.reg.Mirrors = mirrors
	rig.fj.source, rig.fj.sourceBranch = []string{"go.mod", "main.go", ".forgejo/CODEOWNERS", "deploy/base/kustomization.yaml"}, "main"
	gh := &fakeGitHub{status: http.StatusOK, body: `[{"sha":"1mp0rt"}]`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		gh.calls = append(gh.calls, r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		w.WriteHeader(gh.status)
		io.WriteString(w, gh.body)
	}))
	t.Cleanup(srv.Close)
	rig.reg.GitHubAPI, rig.gh = srv.URL, gh
	return rig, mirrors
}

const (
	githubCall = "/repos/acme/app/commits?per_page=1 Bearer " + importToken
	deleteRepo = "DELETE /api/v1/repos/demo-app/demo-app by basic sebastian"
	migrateAs  = "POST /api/v1/repos/migrate by basic sebastian"
)

var factoryWorkflows = map[string]string{".forgejo/workflows/ci.yml": "image: git.aisoftwarefactory.world/demo-app/demo-app\n",
	".forgejo/workflows/pr.yml": "on: pull_request\n", ".forgejo/roll.sh": "#!/bin/sh\n", "AGENTS.md": "# demo-app\n"}

// A project with an import row is made by Forgejo's migrate of the GitHub
// repository as the site admin, never by the create; the row forgets the
// token and says imported; the seed adds only the template's workflows,
// roll script and AGENTS.md the repository lacks, in one commit before
// main's protection. A later registration migrates and seeds nothing; no
// log line holds the token.
func TestRegisterImportsFromGitHub(t *testing.T) {
	rig, mirrors := importRig(t)
	ctx := context.Background()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	want := map[string]any{"clone_addr": "https://github.com/acme/app.git", "auth_token": importToken, "repo_owner": "demo-app",
		"repo_name": "demo-app", "service": "github", "private": true, "mirror": false}
	if len(rig.fj.migrated) != 1 || !reflect.DeepEqual(rig.fj.migrated[0], want) {
		t.Fatalf("migrates %v, want one with %v", rig.fj.migrated, want)
	}
	if !slices.Contains(rig.fj.writes, "POST /api/v1/repos/migrate by basic sebastian") || slices.Contains(rig.fj.writes, "POST /api/v1/orgs/demo-app/repos by basic sebastian") {
		t.Fatalf("writes %q: want the migrate as the admin and no create", rig.fj.writes)
	}
	if row := mirrors.rows["demo-app import"]; row.Token != "" || !strings.HasPrefix(row.State, "imported ") {
		t.Fatalf("import row after the migrate: token %q, state %q", row.Token, row.State)
	}
	if len(rig.fj.seeds) != 1 {
		t.Fatalf("contents calls %+v, want one", rig.fj.seeds)
	}
	c := rig.fj.seeds[0]
	if !reflect.DeepEqual(c.files, factoryWorkflows) || c.branch != "main" || c.message != "Factory workflows" || c.by != "token projects-tok" {
		t.Fatalf("commit %q on %q by %q with %q\nwant %q", c.message, c.branch, c.by, c.files, factoryWorkflows)
	}
	seed := slices.Index(rig.fj.writes, "POST /api/v1/repos/demo-app/demo-app/contents by token projects-tok")
	protect := slices.Index(rig.fj.writes, "POST /api/v1/repos/demo-app/demo-app/branch_protections by token projects-tok")
	if seed < 0 || protect < 0 || seed > protect {
		t.Fatalf("workflows at %d, first protection at %d: %v", seed, protect, rig.fj.writes)
	}
	for _, line := range []string{"repository demo-app/demo-app: imported from acme/app",
		"seed: created 5eed: .forgejo/roll.sh, .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml, AGENTS.md from template ts-go"} {
		if !strings.Contains(rig.logs.String(), "project demo-app: "+line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, rig.logs)
		}
	}
	if _, err := rig.reg.Register(ctx, "demo-app", true); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if len(rig.fj.migrated) != 1 || len(rig.fj.seeds) != 1 {
		t.Fatalf("a second registration migrated %d and seeded %d times", len(rig.fj.migrated), len(rig.fj.seeds))
	}
	if strings.Contains(rig.logs.String(), importToken) {
		t.Fatalf("the token was logged:\n%s", rig.logs)
	}
}

// A refused migrate leaves Forgejo's empty repository and no seed: the row
// keeps its token and the copy's start for the next try and says failed
// with the first line of the error, the token taken out, cut at 120; the
// registration error says the same and no log line holds the token. A
// reconcile while the repository is under 20 minutes old waits; once it
// is older, the reconcile deletes it as the admin and migrates again,
// asking GitHub nothing (ruling 289). A refusal for good copies again at
// most once a reconcile, each time 20 minutes after the last leftover.
func TestRegisterImportFailure(t *testing.T) {
	rig, mirrors := importRig(t)
	rig.fj.migrateFail = `{"message":"clone: https://oauth2:` + importToken + `@github.com/acme/app.git: authentication failed"}`
	ctx := context.Background()
	_, err := rig.reg.Register(ctx, "demo-app", false)
	const msg = `forgejo POST /api/v1/repos/migrate: status 500: {"message":"clone: https://oauth2:[token]@github.com/acme/app.git: authentication failed"}`
	if err == nil || !strings.Contains(err.Error(), "registration: repository demo-app/demo-app: "+msg) || rig.reg.ReconcileError("demo-app") != err.Error() {
		t.Fatalf("registration error %v, reconcile error %q", err, rig.reg.ReconcileError("demo-app"))
	}
	row := mirrors.rows["demo-app import"]
	const state = `failed: forgejo POST /api/v1/repos/migrate: status 500: {"message":"clone: https://oauth2:[token]@github.com/acme/app.git: authe…`
	if row.Token != importToken || row.State != state || row.LastError != msg || row.LastUpdate.IsZero() {
		t.Fatalf("import row: token kept %v, state %q, error %q, start %v\nwant state %q", row.Token == importToken, row.State, row.LastError, row.LastUpdate, state)
	}
	if !rig.fj.repo || len(rig.fj.branches) != 0 || len(rig.fj.seeds) != 0 || slices.Contains(rig.fj.writes, "POST /api/v1/orgs/demo-app/repos by basic sebastian") {
		t.Fatalf("after a refused migrate: repository %v, branches %v, seeds %d, writes %q", rig.fj.repo, rig.fj.branches, len(rig.fj.seeds), rig.fj.writes)
	}
	if strings.Contains(rig.logs.String(), importToken) || strings.Contains(err.Error(), importToken) {
		t.Fatalf("the token was logged:\n%s", rig.logs)
	}
	reconcile := func() error {
		err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"])
		rig.reg.copies.Wait()
		return err
	}
	deletes := func() int {
		return len(slices.DeleteFunc(slices.Clone(rig.fj.writes), func(w string) bool { return w != deleteRepo }))
	}
	afterDelete := func() []string {
		for i := len(rig.fj.writes) - 1; i >= 0; i-- {
			if rig.fj.writes[i] == deleteRepo {
				return rig.fj.writes[i:]
			}
		}
		return nil
	}
	for i, migrates := range []int{2, 3} {
		if err := reconcile(); err == nil || !strings.Contains(err.Error(), "may still be copying") || len(rig.fj.migrated) != migrates-1 || deletes() != i {
			t.Fatalf("under 20 minutes, round %d: error %v, migrates %d, deletes %d", i, err, len(rig.fj.migrated), deletes())
		}
		rig.fj.created = time.Now().Add(-21 * time.Minute)
		if i == 1 {
			rig.fj.migrateFail = ""
		}
		if err := reconcile(); err != nil {
			t.Fatalf("round %d: %v\n%s", i, err, rig.logs)
		}
		if len(rig.fj.migrated) != migrates || deletes() != i+1 || len(rig.gh.calls) != 0 || !slices.Contains(afterDelete(), migrateAs) {
			t.Fatalf("round %d: migrates %d, deletes %d, GitHub calls %q, writes %q", i, len(rig.fj.migrated), deletes(), rig.gh.calls, rig.fj.writes)
		}
	}
	if row := mirrors.rows["demo-app import"]; row.Token != "" || !strings.HasPrefix(row.State, "imported ") {
		t.Fatalf("the last retry: token %q, state %q", row.Token, row.State)
	}
}

// The delete rests on a second read just before it: a repository that no
// longer reads empty with the first read's creation (a push landed, or
// another process's migrate made it anew) is left alone, nothing copied,
// and the reconcile fails until the next one looks again.
func TestImportLeftoverChangedBeforeTheDelete(t *testing.T) {
	for name, change := range map[string]func(*forgejoState){
		"made anew": func(s *forgejoState) { s.created = time.Now() },
		"pushed":    func(s *forgejoState) { s.branches["main"] = "pu5h" },
	} {
		rig, mirrors := importRig(t)
		rig.fj.repo, rig.fj.created = true, time.Now().Add(-21*time.Minute)
		reads := 0
		rig.fj.repoRead = func() {
			if reads++; reads == 2 {
				change(rig.fj)
			}
		}
		want := mirrors.rows["demo-app import"]
		err := rig.reg.Reconcile(context.Background(), rig.store.m["demo-app"])
		if err == nil || !strings.Contains(err.Error(), "registration: repository demo-app/demo-app: the repository changed while registration read it") ||
			reads != 2 || slices.Contains(rig.fj.writes, deleteRepo) || len(rig.fj.migrated) != 0 || mirrors.rows["demo-app import"] != want {
			t.Fatalf("%s: error %v, reads %d, writes %q, migrates %d, import row %+v", name, err, reads, rig.fj.writes, len(rig.fj.migrated), mirrors.rows["demo-app import"])
		}
	}
}

// A GitHub repository whose default branch is not main gets main from it,
// as the default branch, before the workflows; an empty one is seeded
// whole from the template as a new project is.
func TestImportedMain(t *testing.T) {
	all := map[string]string{"README.md": "# template\n", "deploy/staging/kustomization.yaml": "namespace: demo-staging\n", ".forgejo/CODEOWNERS": "* @demo-app\n"}
	maps.Copy(all, factoryWorkflows)
	for _, c := range []struct {
		name, branch, message, head string
		source                      []string
		want                        map[string]string
	}{
		{"master", "master", "Factory workflows", "1mp0rt", []string{"go.mod", ".forgejo/CODEOWNERS", "deploy/base/kustomization.yaml"}, factoryWorkflows},
		{"empty", "main", "Seed from template ts-go (registration)", "5eed", nil, all},
	} {
		rig, _ := importRig(t)
		rig.fj.source, rig.fj.sourceBranch = c.source, c.branch
		if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, rig.logs)
		}
		if len(rig.fj.seeds) != 1 || !reflect.DeepEqual(rig.fj.seeds[0].files, c.want) || rig.fj.seeds[0].message != c.message || rig.fj.seeds[0].branch != "main" {
			t.Fatalf("%s: commits %+v, want %q with %q", c.name, rig.fj.seeds, c.message, c.want)
		}
		if rig.fj.defaultBranch != "main" || rig.fj.branches[c.branch] != c.head {
			t.Errorf("%s: default branch %q, branches %v", c.name, rig.fj.defaultBranch, rig.fj.branches)
		}
		patched := slices.Contains(rig.fj.writes, "PATCH /api/v1/repos/demo-app/demo-app by token projects-tok")
		if patched != (c.name == "master") {
			t.Errorf("%s: default branch set %v: %q", c.name, patched, rig.fj.writes)
		}
	}
}

// A migrate that outlived its caller left the repository: the next
// registration takes a non-empty one as the import, with no second
// migrate, and adds the workflows. An empty one made just now may still be
// copying: the registration fails, the row as it was, nothing seeded or
// protected.
func TestRegisterImportExists(t *testing.T) {
	rig, mirrors := importRig(t)
	rig.fj.repo, rig.fj.defaultBranch, rig.fj.branches["main"] = true, "main", "1mp0rt"
	for _, f := range rig.fj.source {
		rig.fj.files[f] = true
	}
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if len(rig.fj.migrated) != 0 || slices.Contains(rig.fj.writes, "POST /api/v1/orgs/demo-app/repos by basic sebastian") {
		t.Fatalf("migrates %v, writes %q: want neither migrate nor create", rig.fj.migrated, rig.fj.writes)
	}
	if row := mirrors.rows["demo-app import"]; row.Token != "" || !strings.HasPrefix(row.State, "imported ") {
		t.Fatalf("import row: token %q, state %q", row.Token, row.State)
	}
	if len(rig.fj.seeds) != 1 || !reflect.DeepEqual(rig.fj.seeds[0].files, factoryWorkflows) || rig.fj.seeds[0].message != "Factory workflows" || rig.fj.seeds[0].branch != "main" {
		t.Fatalf("commits %+v, want one \"Factory workflows\" with %q", rig.fj.seeds, factoryWorkflows)
	}

	rig, mirrors = importRig(t)
	rig.fj.repo, rig.fj.created = true, time.Now()
	_, err := rig.reg.Register(context.Background(), "demo-app", false)
	const msg = "registration: repository demo-app/demo-app: the repository exists, empty: a migrate of github.com/acme/app may still be copying; the next reconcile looks again"
	if err == nil || !strings.Contains(err.Error(), msg) || rig.reg.ReconcileError("demo-app") != err.Error() {
		t.Fatalf("registration error %v, reconcile error %q", err, rig.reg.ReconcileError("demo-app"))
	}
	want := Mirror{Project: "demo-app", Direction: MirrorImport, Remote: "acme/app", Token: importToken, State: "pending"}
	if row := mirrors.rows["demo-app import"]; row != want || len(mirrors.set) != 0 {
		t.Fatalf("import row %+v, states set %q; want %+v unchanged", row, mirrors.set, want)
	}
	if len(rig.fj.migrated) != 0 || len(rig.fj.seeds) != 0 || slices.Contains(rig.fj.writes, "POST /api/v1/orgs/demo-app/repos by basic sebastian") ||
		slices.Contains(rig.fj.writes, "POST /api/v1/repos/demo-app/demo-app/branch_protections by token projects-tok") {
		t.Fatalf("an empty repository: migrates %v, seeds %d, writes %q", rig.fj.migrated, len(rig.fj.seeds), rig.fj.writes)
	}
}

// Mirror rows without an import, production's path: create, seed whole.
func TestRegisterMirrorsWithoutImport(t *testing.T) {
	rig := newRegisterRig(t)
	rig.store.m["demo-app"] = importedProject()
	rig.reg.Mirrors = &memMirrors{rows: map[string]Mirror{"demo-app export": {Project: "demo-app", Direction: MirrorExport, Remote: "acme/app",
		Username: "u", Token: "ghp-s3cret", State: "created"}}}
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if len(rig.fj.migrated) != 0 || !slices.Contains(rig.fj.writes, "POST /api/v1/orgs/demo-app/repos by basic sebastian") {
		t.Fatalf("migrates %v, writes %q: want the create and no migrate", rig.fj.migrated, rig.fj.writes)
	}
	want := map[string]string{"README.md": "# template\n", "deploy/staging/kustomization.yaml": "namespace: demo-staging\n", ".forgejo/CODEOWNERS": "* @demo-app\n"}
	maps.Copy(want, factoryWorkflows)
	if len(rig.fj.seeds) != 1 || rig.fj.seeds[0].message != "Seed from template ts-go (registration)" || rig.fj.seeds[0].branch != "main" || !reflect.DeepEqual(rig.fj.seeds[0].files, want) {
		t.Fatalf("commits %+v, want one seed with %q", rig.fj.seeds, want)
	}
}

// The row reads copying since the copy's start, before the migrate, and
// a failed row keeps that start. After a restart no copy runs here and the
// row is read against Forgejo, which finishes a migrate its caller left: a
// non-empty repository is the copy's, imported at any age, with no
// GitHub call. An empty one under 20 minutes old (twice the migrate's
// timeout) may still copy: the reconcile waits, no failure, the row as it
// was, and Register reports the wait. An older one is asked of GitHub once,
// one rule for pending, copying and failed rows: an empty source (409) is
// imported and seeded whole; a source with a commit means a cut or refused
// copy, whose repository the admin deletes before it copies again; any
// other answer leaves the row and fails the reconcile.
func TestRegisterImportAfterRestart(t *testing.T) {
	rig, mirrors := importRig(t)
	rig.fj.migrateFail = `{"message":"cut"}`
	before := time.Now().Truncate(time.Second)
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err == nil || len(mirrors.set) != 2 {
		t.Fatalf("registration error %v, states set %q", err, mirrors.set)
	}
	state, at, _ := strings.Cut(strings.TrimPrefix(mirrors.set[0], "demo-app import: "), " |  | ")
	if start, err := time.Parse(time.RFC3339, at); err != nil || start.Before(before) || start.After(time.Now()) ||
		state != "copying since "+start.Format("2006-01-02 15:04")+" UTC" || !strings.HasPrefix(mirrors.set[1], "demo-app import: failed: ") ||
		!strings.HasSuffix(mirrors.set[1], " | "+at) {
		t.Fatalf("states set %q: want copying since the migrate's start, then failed with it", mirrors.set)
	}

	all := map[string]string{"README.md": "# template\n", "deploy/staging/kustomization.yaml": "namespace: demo-staging\n", ".forgejo/CODEOWNERS": "* @demo-app\n"}
	maps.Copy(all, factoryWorkflows)
	started := func(ago time.Duration, empty bool, state string, github int) (*registerRig, *memMirrors, Mirror) {
		rig, mirrors := importRig(t)
		rig.fj.repo, rig.fj.created, rig.gh.status = true, time.Now().Add(-ago), github
		if github == http.StatusConflict {
			rig.gh.body = `{"message":"Git Repository is empty."}`
		}
		if !empty {
			rig.fj.defaultBranch, rig.fj.branches["main"] = "main", "1mp0rt"
			for _, f := range rig.fj.source {
				rig.fj.files[f] = true
			}
		}
		row := mirrors.rows["demo-app import"]
		if state != "pending" {
			row.LastUpdate = rig.fj.created
		}
		row.State = strings.ReplaceAll(state, "T", row.LastUpdate.UTC().Format(minuteUTC))
		mirrors.rows["demo-app import"] = row
		return rig, mirrors, row
	}
	for _, c := range []struct {
		ago      time.Duration
		empty    bool
		state    string
		github   int
		seed     string
		files    map[string]string
		calls    int
		deletion bool
	}{
		{21 * time.Minute, false, "copying since T", http.StatusOK, "Factory workflows", factoryWorkflows, 0, false},
		{19 * time.Minute, false, "copying since T", http.StatusOK, "Factory workflows", factoryWorkflows, 0, false},
		{2 * time.Minute, false, "copying since T", http.StatusOK, "Factory workflows", factoryWorkflows, 0, false},
		{21 * time.Minute, true, "copying since T", http.StatusConflict, "Seed from template ts-go (registration)", all, 1, false},
		{21 * time.Minute, true, "pending", http.StatusOK, "Factory workflows", factoryWorkflows, 1, true},
		{21 * time.Minute, true, "copying since T", http.StatusOK, "Factory workflows", factoryWorkflows, 1, true},
		{21 * time.Minute, true, "failed: cut", http.StatusConflict, "Factory workflows", factoryWorkflows, 0, true},
	} {
		rig, mirrors, _ := started(c.ago, c.empty, c.state, c.github)
		if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
			t.Fatalf("%q %v ago, empty %v: %v\n%s", c.state, c.ago, c.empty, err, rig.logs)
		}
		migrates := map[bool]int{true: 1}[c.deletion]
		if row := mirrors.rows["demo-app import"]; len(rig.fj.migrated) != migrates || row.Token != "" || !strings.HasPrefix(row.State, "imported ") ||
			len(rig.gh.calls) != c.calls || slices.Contains(rig.fj.writes, deleteRepo) != c.deletion {
			t.Fatalf("%q %v ago, empty %v: migrates %d, import row token %q, state %q, GitHub calls %q, writes %q",
				c.state, c.ago, c.empty, len(rig.fj.migrated), row.Token, row.State, rig.gh.calls, rig.fj.writes)
		}
		if len(rig.fj.seeds) != 1 || rig.fj.seeds[0].message != c.seed || rig.fj.seeds[0].branch != "main" || !reflect.DeepEqual(rig.fj.seeds[0].files, c.files) {
			t.Fatalf("%q %v ago, empty %v: commits %+v, want one %q with %q", c.state, c.ago, c.empty, rig.fj.seeds, c.seed, c.files)
		}
	}

	rig, mirrors, want := started(21*time.Minute, true, "copying since T", http.StatusUnauthorized)
	err := rig.reg.Reconcile(context.Background(), rig.store.m["demo-app"])
	if row := mirrors.rows["demo-app import"]; err == nil || !strings.Contains(err.Error(), "GitHub answers 401 Unauthorized to the commits of acme/app") ||
		row != want || len(mirrors.set) != 0 || len(rig.gh.calls) != 1 || len(rig.fj.migrated) != 0 || slices.Contains(rig.fj.writes, deleteRepo) {
		t.Fatalf("GitHub refusing: error %v, import row %+v, states set %q, GitHub calls %q, migrates %d, writes %q",
			err, row, mirrors.set, rig.gh.calls, len(rig.fj.migrated), rig.fj.writes)
	}

	for _, ago := range []time.Duration{2 * time.Minute, 19 * time.Minute} {
		rig, mirrors, want := started(ago, true, "copying since T", http.StatusOK)
		if err := rig.reg.Reconcile(context.Background(), rig.store.m["demo-app"]); err != nil || rig.reg.ReconcileError("demo-app") != "" {
			t.Fatalf("started %v ago: reconcile error %v, %q", ago, err, rig.reg.ReconcileError("demo-app"))
		}
		line := "registration: repository demo-app/demo-app: copying github.com/acme/app since " + want.LastUpdate.UTC().Format(minuteUTC)
		if row := mirrors.rows["demo-app import"]; row != want || len(mirrors.set) != 0 || len(rig.fj.migrated) != 0 || len(rig.fj.seeds) != 0 ||
			len(rig.gh.calls) != 0 || !strings.Contains(rig.logs.String(), "project demo-app: "+line+"\n") {
			t.Fatalf("started %v ago: import row %+v, states set %q, migrates %d, seeds %d, GitHub calls %q; want %+v unchanged, none set, the line %q\n%s",
				ago, row, mirrors.set, len(rig.fj.migrated), len(rig.fj.seeds), rig.gh.calls, want, line, rig.logs)
		}
		if _, err := rig.reg.Register(context.Background(), "demo-app", false); err == nil || err.Error() != line {
			t.Fatalf("started %v ago: register-project's error %v, want %q", ago, err, line)
		}
	}
}

// The copy runs beside the reconciles: project A's reconcile starts it
// and returns with its row copying since the start, no failure; project
// B's reconcile takes the registrar's lock while A's migrate still blocks;
// a second reconcile of A during the copy starts no second migrate. The
// migrate's end writes imported and reconciles A again, which seeds it.
func TestImportCopiesAside(t *testing.T) {
	rig, mirrors := importRig(t)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	rig.fj.migrateGate = gate
	rig.store.m["other"] = Project{Name: "other", Repository: "other/other", Objects: policy.ObjectsFactory, File: true,
		PullToken: "pull", GitToken: "git", MergeToken: "merge"}
	ctx := context.Background()
	reconcile := func(name string) {
		t.Helper()
		p, done := rig.store.m[name], make(chan error, 1)
		go func() { done <- rig.reg.Reconcile(ctx, p) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s's reconcile: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s's reconcile still waits after 5 s", name)
		}
	}
	before := time.Now().Truncate(time.Second)
	reconcile("demo-app")
	row, _, _ := mirrors.MirrorToken(ctx, "demo-app", MirrorImport)
	if row.LastUpdate.Before(before) || row.LastUpdate.After(time.Now()) || row.State != "copying since "+row.LastUpdate.UTC().Format("2006-01-02 15:04")+" UTC" ||
		row.Token != importToken || rig.reg.ReconcileError("demo-app") != "" {
		t.Fatalf("during the copy: import row %+v, reconcile error %q", row, rig.reg.ReconcileError("demo-app"))
	}
	reconcile("other")
	reconcile("demo-app")
	release()
	rig.reg.copies.Wait()
	if row := mirrors.rows["demo-app import"]; len(rig.fj.migrated) != 1 || row.Token != "" || !strings.HasPrefix(row.State, "imported ") {
		t.Fatalf("after the copy: migrates %d, import row token %q, state %q", len(rig.fj.migrated), row.Token, row.State)
	}
	if len(rig.fj.seeds) != 1 || rig.fj.seeds[0].message != "Factory workflows" || rig.reg.ReconcileError("demo-app") != "" {
		t.Fatalf("after the copy: commits %+v, reconcile error %q\n%s", rig.fj.seeds, rig.reg.ReconcileError("demo-app"), rig.logs)
	}
	for _, line := range []string{"project other: repository other/other: " + string(Waiting),
		"project demo-app: repository demo-app/demo-app: imported from acme/app"} {
		if !strings.Contains(rig.logs.String(), line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, rig.logs)
		}
	}
}

// factoryd's stop cuts a running copy: its row stays copying, no failure
// written, no reconcile error, for the next start to read against Forgejo.
func TestImportCopyStopsWithFactoryd(t *testing.T) {
	rig, mirrors := importRig(t)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	rig.fj.migrateGate = gate
	ctx, stop := context.WithCancel(context.Background())
	if err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"]); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	copying, _, _ := mirrors.MirrorToken(ctx, "demo-app", MirrorImport)
	stop()
	rig.reg.copies.Wait()
	const line = "project demo-app: repository demo-app/demo-app: the copy of github.com/acme/app stopped with factoryd; its row stays copying\n"
	if row := mirrors.rows["demo-app import"]; !strings.HasPrefix(row.State, "copying since ") || row != copying || len(mirrors.set) != 1 ||
		rig.reg.ReconcileError("demo-app") != "" || !strings.Contains(rig.logs.String(), line) {
		t.Fatalf("after the stop: import row %+v, states set %q, reconcile error %q\n%s", row, mirrors.set, rig.reg.ReconcileError("demo-app"), rig.logs)
	}
	if _, running := rig.reg.copying.Load("demo-app"); running {
		t.Fatal("the stopped copy still counts as running")
	}
}

// An imported repository without deploy/ gets the template's deploy/
// files, rendered, in the workflows' commit, "Factory workflows and
// manifests"; one with deploy/ keeps its own: "Factory workflows".
func TestImportedManifests(t *testing.T) {
	withManifests := map[string]string{"deploy/base/kustomization.yaml": "resources: [deployment.yaml]\n",
		"deploy/base/deployment.yaml":       "image: git.aisoftwarefactory.world/demo-app/demo-app\n",
		"deploy/staging/kustomization.yaml": "namespace: demo-staging\n"}
	maps.Copy(withManifests, factoryWorkflows)
	for _, c := range []struct {
		source  []string
		message string
		want    map[string]string
	}{
		{[]string{"go.mod", ".forgejo/CODEOWNERS"}, "Factory workflows and manifests", withManifests},
		{[]string{"go.mod", ".forgejo/CODEOWNERS", "deploy/k8s.yaml"}, "Factory workflows", factoryWorkflows},
	} {
		rig, _ := importRig(t)
		files := rig.store.m["demo-app"].TemplateFiles
		files["deploy/base/kustomization.yaml"], files["deploy/base/deployment.yaml"] = "resources: [deployment.yaml]\n", "image: [[.Image]]\n"
		rig.fj.source = c.source
		if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
			t.Fatalf("%v\n%s", err, rig.logs)
		}
		if len(rig.fj.seeds) != 1 || rig.fj.seeds[0].message != c.message || !reflect.DeepEqual(rig.fj.seeds[0].files, c.want) {
			t.Fatalf("source %q: commits %+v, want one %q with %q", c.source, rig.fj.seeds, c.message, c.want)
		}
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): the poll
// deletes, with one log line each, an import row still pending, copying
// or failed 7 days after its last write whose project no file names, and
// its token with it; it keeps a younger one, a filed project's, an
// imported one and an export row.
func TestPollDeletesStaleImports(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	logs := captureLog(t)
	rows := []struct {
		m    Mirror
		age  string
		gone bool
	}{
		{Mirror{Project: "gone", Direction: MirrorImport, Remote: "acme/gone", Token: "ghp_g0neT0ken", State: "pending"}, "169 hours", true},
		{Mirror{Project: "cut", Direction: MirrorImport, Remote: "acme/cut", Token: "ghp_cutT0ken", State: "copying since 2026-10-01 10:00 UTC"}, "169 hours", true},
		{Mirror{Project: "gone", Direction: MirrorExport, Remote: "acme/gone", Username: "u", Token: "ghp-exp0rt", State: "created"}, "169 hours", false},
		{Mirror{Project: "young", Direction: MirrorImport, Remote: "acme/young", State: "pending"}, "167 hours", false},
		{Mirror{Project: "filed", Direction: MirrorImport, Remote: "acme/filed", State: "pending"}, "169 hours", false},
		{Mirror{Project: "broken", Direction: MirrorImport, Remote: "acme/broken", Token: "ghp_br0kenT0ken", State: "failed: x"}, "169 hours", true},
		{Mirror{Project: "done", Direction: MirrorImport, Remote: "acme/done", State: "imported 2026-10-01 10:00 UTC"}, "169 hours", false},
	}
	for _, r := range rows {
		if err := s.PutMirror(ctx, r.m); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.DeleteMirror(ctx, r.m.Project, r.m.Direction) })
		if _, err := s.db.ExecContext(ctx, `UPDATE project_mirrors SET updated_at = now() - $3::interval WHERE project = $1 AND direction = $2`,
			r.m.Project, r.m.Direction, r.age); err != nil {
			t.Fatal(err)
		}
	}
	r := &Registrar{Store: &memProjects{m: map[string]Project{"filed": {Name: "filed", File: true}}}, Mirrors: s}
	r.poll(ctx)
	for _, row := range rows {
		if _, ok, err := s.Mirror(ctx, row.m.Project, row.m.Direction); err != nil || ok == row.gone {
			t.Errorf("%s %s row kept %v, %v; want %v", row.m.Project, row.m.Direction, ok, err, !row.gone)
		}
	}
	lines := []string{"project gone: import row of github.com/acme/gone deleted: pending over 7 days, and no file projects/gone.yaml\n",
		"project cut: import row of github.com/acme/cut deleted: copying over 7 days, and no file projects/cut.yaml\n",
		"project broken: import row of github.com/acme/broken deleted: failed over 7 days, and no file projects/broken.yaml\n"}
	for _, line := range lines {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("log lacks %q", line)
		}
	}
	if strings.Count(logs.String(), "import row") != 3 || strings.Contains(logs.String(), "T0ken") {
		t.Fatalf("log:\n%s\nwant only the lines %q, no token", logs, lines)
	}
}
