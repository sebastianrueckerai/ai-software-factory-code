package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// pgSessionStore is pgBriefStore with no sessions and no stored texts.
func pgSessionStore(t *testing.T) *PGStore {
	t.Helper()
	s := pgBriefStore(t)
	if _, err := s.db.ExecContext(context.Background(), `TRUNCATE sessions, session_briefs`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// tokenAuth is the gateway's ServiceAccount check for a test: the bearer
// it maps is the session's token, the API server's signed identity.
type tokenAuth map[string]gateway.Identity

func (a tokenAuth) Authenticate(r *http.Request) (gateway.Identity, error) {
	if id, ok := a[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
		return id, nil
	}
	return gateway.Identity{}, errors.New("not a session's token")
}

// The worker's harness builds the session's prompt, stores its first turn
// through the gateway on the session's own token, and prompts the model
// with it. The card's sessions then read one row: the turn the first
// model call got, the repository map and the sandbox facts among it.
func TestRunSessionStoresItsBrief(t *testing.T) {
	ctx := context.Background()
	s := pgSessionStore(t)
	if err := s.CreateTask(ctx, Task{ID: "t-1", Kind: "agent", Status: "running", Track: policy.TrackDirect, Repository: "sf/r", BudgetEUR: 2,
		Objective: "Count the skipped rows\n\nThe nightly report skips rows."}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSession(ctx, Session{ID: "s-1", TaskID: "t-1", Model: "factory/factory-main"}); err != nil {
		t.Fatal(err)
	}
	fd := httptest.NewServer(SessionBriefHandler(s))
	defer fd.Close()
	gw := gateway.NewServer(gateway.ServerOptions{Auth: tokenAuth{"jwt": {TaskID: "t-1", Session: "s-1"}},
		Briefs: &gateway.Factoryd{BaseURL: fd.URL, Token: "shared", HTTP: fd.Client()}})
	mux := http.NewServeMux()
	mux.Handle("POST /worker/session-brief", gw.Routes("test"))
	gwSrv := httptest.NewServer(mux)
	defer gwSrv.Close()

	oc, dir := fakeServeOpencode(t, "")
	oc.GatewayURL = gwSrv.URL
	const repoMap = "Repository map of the card's directories (guidance, not a boundary):\ncmd/report/: main.go"
	p, err := oc.Text(harness.TaskBrief{Step: "coding", Objective: "Count the skipped rows\n\nWhy: The nightly report undercounts.", Repository: "sf/r", SHA: "abc123",
		Facts: "FACTS: session 1 of 3", Map: repoMap})
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.StoreText(ctx, p.Facts()); err != nil {
		t.Fatal(err)
	}
	if res := oc.Run(ctx, p, func(harness.Event) {}); !res.Success {
		t.Fatalf("Run = %+v", res)
	}
	var prompted string
	for _, r := range servedRequests(t, dir) {
		if strings.HasSuffix(r.Path, "/prompt_async") {
			var p struct {
				Parts []map[string]string `json:"parts"`
			}
			if err := json.Unmarshal([]byte(r.Body), &p); err != nil || len(p.Parts) != 1 {
				t.Fatalf("prompt %s: %v", r.Body, err)
			}
			prompted = p.Parts[0]["text"]
			break
		}
	}
	got, err := s.SessionBriefs(ctx, "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Session != 1 || got[0].Brief != prompted || got[0].At.IsZero() {
		t.Fatalf("session briefs %+v, want session 1 holding the prompt %q", got, prompted)
	}
	for _, part := range []string{repoMap, SandboxFacts(nil), "FACTS: session 1 of 3", "Why: The nightly report undercounts."} {
		if !strings.Contains(got[0].Brief, part) {
			t.Errorf("the stored text lacks %q", part)
		}
	}
}

// A session's text is stored as its number among its card's sessions,
// oldest first. The same text again is the same store; another text for a
// session that has one is refused, and so is a session of another card or
// none.
func TestStoreSessionBriefNumbersTheCardsSessions(t *testing.T) {
	ctx := context.Background()
	s := pgSessionStore(t)
	for _, id := range []string{"t-1", "t-2"} {
		if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Track: policy.TrackDirect, Repository: "sf/r", BudgetEUR: 2, Objective: "Fix it"}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-time.Hour)
	for i, sess := range []Session{{ID: "s-a", TaskID: "t-1"}, {ID: "s-9", TaskID: "t-2"}, {ID: "s-b", TaskID: "t-1"}} {
		sess.CreatedAt = at.Add(time.Duration(i) * time.Minute)
		if err := s.InsertSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	store := func(task, session, text string) (int, error) {
		t.Helper()
		return s.StoreSessionBrief(ctx, task, session, text)
	}
	if n, err := store("t-1", "s-b", "second text"); n != 2 || err != nil {
		t.Fatalf("t-1's second session = %d, %v; want 2", n, err)
	}
	if n, err := store("t-1", "s-a", "first text"); n != 1 || err != nil {
		t.Fatalf("t-1's first session = %d, %v; want 1", n, err)
	}
	if n, err := store("t-1", "s-b", "second text"); n != 2 || err != nil {
		t.Fatalf("the same text again = %d, %v; want 2 stored once", n, err)
	}
	var stored ErrSessionBriefStored
	if _, err := store("t-1", "s-b", "a rebuilt text"); !errors.As(err, &stored) || err.Error() != "session 2 of t-1 stored the text it got already" {
		t.Fatalf("another text = %v, want the refusal", err)
	}
	for _, other := range []string{"s-9", "s-gone"} {
		if _, err := store("t-1", other, "text"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %s for t-1 = %v, want not found", other, err)
		}
	}
	got, err := s.SessionBriefs(ctx, "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Session != 1 || got[0].Brief != "first text" || got[1].Session != 2 || got[1].Brief != "second text" {
		t.Fatalf("t-1's briefs %+v, want 1 and 2 with their first texts", got)
	}
	if got, err := s.SessionBriefs(ctx, "t-2"); len(got) != 0 || err != nil {
		t.Fatalf("t-2's briefs %+v, %v; want none", got, err)
	}
}

// A post that arrives while another one's row of the same session is not
// committed yet waits for it, then compares the texts: the same text is
// stored once and succeeds, another is refused.
func TestStoreSessionBriefWhileAnotherStores(t *testing.T) {
	ctx := context.Background()
	for text, want := range map[string]error{"the text": nil, "a rebuilt text": ErrSessionBriefStored{Task: "t-1", Session: 1}} {
		s := pgSessionStore(t)
		if err := s.CreateTask(ctx, Task{ID: "t-1", Kind: "agent", Status: "running", Track: policy.TrackDirect, Repository: "sf/r", BudgetEUR: 2, Objective: "Fix it"}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertSession(ctx, Session{ID: "s-1", TaskID: "t-1"}); err != nil {
			t.Fatal(err)
		}
		first, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.ExecContext(ctx, `INSERT INTO session_briefs (task_id, session, brief) VALUES ('t-1', 1, 'the text')`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.StoreSessionBrief(ctx, "t-1", "s-1", text)
			done <- err
		}()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var waiting int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the second post never waited on the first's row")
			}
		}
		if err := first.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, want) {
			t.Errorf("%q while the first stores: %v, want %v", text, err, want)
		}
	}
}

// fakeSessionBriefs answers StoreSessionBrief with err and records the call.
type fakeSessionBriefs struct {
	got []string
	err error
}

func (f *fakeSessionBriefs) StoreSessionBrief(_ context.Context, taskID, sessionID, text string) (int, error) {
	f.got = append(f.got, taskID+" "+sessionID+" "+text)
	return 1, f.err
}

// The handler stores the gateway's hand-over: 204 once stored, the
// store's refusals as 404 and 409, a body that names no task, session or
// text 400, and a text past the bound 413, stored nowhere.
func TestSessionBriefHandler(t *testing.T) {
	post := func(f *fakeSessionBriefs, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		SessionBriefHandler(f).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/internal/session-brief", bytes.NewReader(b)))
		return rr
	}
	full := map[string]string{"task_id": "t-1", "session": "s-1", "text": "the text"}
	for name, tc := range map[string]struct {
		err    error
		body   map[string]string
		status int
		stored int
	}{
		"stored":         {body: full, status: http.StatusNoContent, stored: 1},
		"another card's": {err: ErrNotFound, body: full, status: http.StatusNotFound, stored: 1},
		"stored already": {err: ErrSessionBriefStored{Task: "t-1", Session: 1}, body: full, status: http.StatusConflict, stored: 1},
		"no task":        {body: map[string]string{"session": "s-1", "text": "the text"}, status: http.StatusBadRequest},
		"no session":     {body: map[string]string{"task_id": "t-1", "text": "the text"}, status: http.StatusBadRequest},
		"no text":        {body: map[string]string{"task_id": "t-1", "session": "s-1"}, status: http.StatusBadRequest},
		"past the bound": {body: map[string]string{"task_id": "t-1", "session": "s-1", "text": strings.Repeat("a", gateway.MaxModelBody+1)}, status: http.StatusRequestEntityTooLarge},
		// JSON doubles each line break: the bound is the text's, not the body's
		"at the bound": {body: map[string]string{"task_id": "t-1", "session": "s-1", "text": strings.Repeat("\n", gateway.MaxModelBody)}, status: http.StatusNoContent, stored: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSessionBriefs{err: tc.err}
			if rr := post(f, tc.body); rr.Code != tc.status {
				t.Fatalf("status %d (%s), want %d", rr.Code, rr.Body.String(), tc.status)
			}
			if len(f.got) != tc.stored {
				t.Fatalf("stored %d times, want %d", len(f.got), tc.stored)
			}
		})
	}
}
