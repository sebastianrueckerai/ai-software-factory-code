package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
