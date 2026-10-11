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
	rows map[string]Mirror
	set  []string
}

func (m *memMirrors) MirrorToken(_ context.Context, project, direction string) (Mirror, bool, error) {
	row, ok := m.rows[project+" "+direction]
	return row, ok, nil
}

func (m *memMirrors) SetMirrorState(_ context.Context, project, direction, state, lastError string, lastUpdate time.Time) error {
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

func (m *memMirrors) SetMirrorToken(_ context.Context, project, direction, token string) error {
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

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a scrub
// takes the secret out of the member's messages on the project, texts and
// tool calls, and leaves another member's and another project's alone.
func TestPGStoreScrubChatMessages(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	calls := []ChatToolCall{{ID: "call_1", Name: "comment", Arguments: `{"text":"ghp_s3cret"}`, Result: "Commented."}}
	for _, m := range []struct{ repo, login string }{{"sf/code", "anna"}, {"sf/code", "bob"}, {"linkshort/linkshort", "anna"}} {
		if err := s.AddChatMessage(ctx, m.repo, m.login, ChatMessage{Role: "assistant", Content: "with ghp_s3cret.", ToolCalls: calls}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ScrubChatMessages(ctx, "sf/code", "anna", "ghp_s3cret"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range []struct{ repo, login string }{{"sf/code", "anna"}, {"sf/code", "bob"}, {"linkshort/linkshort", "anna"}} {
		ms, _ := s.ChatMessages(ctx, m.repo, m.login, 0, 10)
		got = append(got, ms[0].Content+" "+ms[0].ToolCalls[0].Arguments+" "+ms[0].ToolCalls[0].Result)
	}
	want := []string{`with [token]. {"text":"[token]"} Commented.`, `with ghp_s3cret. {"text":"ghp_s3cret"} Commented.`, `with ghp_s3cret. {"text":"ghp_s3cret"} Commented.`}
	if !slices.Equal(got, want) {
		t.Fatalf("after the scrub %q, want %q", got, want)
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

// importRig is a registration of demo-app whose row imports acme/app,
// a GitHub repository with main and a CODEOWNERS of its own.
func importRig(t *testing.T) (*registerRig, *memMirrors) {
	rig := newRegisterRig(t)
	rig.store.m["demo-app"] = importedProject()
	mirrors := &memMirrors{rows: map[string]Mirror{"demo-app import": {Project: "demo-app", Direction: MirrorImport, Remote: "acme/app",
		Token: importToken, State: "pending"}}}
	rig.reg.Mirrors = mirrors
	rig.fj.source, rig.fj.sourceBranch = []string{"go.mod", "main.go", ".forgejo/CODEOWNERS"}, "main"
	return rig, mirrors
}

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

// A refused migrate leaves no repository and no seed: the row keeps its
// token for the next try and says failed with the first line of the error,
// the token taken out, cut at 120; the registration error says the same
// and no log line holds the token. The next reconcile migrates again.
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
	if row.Token != importToken || row.State != state || row.LastError != msg {
		t.Fatalf("import row: token kept %v, state %q, error %q\nwant state %q", row.Token == importToken, row.State, row.LastError, state)
	}
	if rig.fj.repo || len(rig.fj.seeds) != 0 || slices.Contains(rig.fj.writes, "POST /api/v1/orgs/demo-app/repos by basic sebastian") {
		t.Fatalf("after a refused migrate: repository %v, seeds %d, writes %q", rig.fj.repo, len(rig.fj.seeds), rig.fj.writes)
	}
	if strings.Contains(rig.logs.String(), importToken) || strings.Contains(err.Error(), importToken) {
		t.Fatalf("the token was logged:\n%s", rig.logs)
	}
	rig.fj.migrateFail = ""
	if err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"]); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if row := mirrors.rows["demo-app import"]; len(rig.fj.migrated) != 2 || row.Token != "" || !strings.HasPrefix(row.State, "imported ") {
		t.Fatalf("the retry: %d migrates, token %q, state %q", len(rig.fj.migrated), row.Token, row.State)
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
		{"master", "master", "Factory workflows", "1mp0rt", []string{"go.mod", ".forgejo/CODEOWNERS"}, factoryWorkflows},
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
