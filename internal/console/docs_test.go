package console

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// docsFake is the document repository sf/docs on a Forgejo as its contents
// API answers: master's files by path with a blob sha per version, a
// commit answered 201; every other call goes to the rig's Forgejo. It
// records each commit's Authorization, and slows a commit when asked, the
// lab's slow save.
type docsFake struct {
	mu      sync.Mutex
	files   map[string]string
	version int
	commits []map[string]any
	tokens  []string // each commit's Authorization, in order
	slow    time.Duration
	// failCommits, when set, is Forgejo refusing every commit: a save's 502.
	failCommits bool
}

// docsOver is the document repository sf/docs on the Forgejo at next: a
// proxy in front of it that answers the contents API of master's files by
// path with a blob sha per version, the head commit, and a commit on a
// write answered 201.
func docsOver(t *testing.T, next string, files map[string]string) (*docsFake, *httptest.Server) {
	t.Helper()
	f := &docsFake{files: files}
	up, err := url.Parse(next)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(up)
	const base = "/api/v1/repos/sf/docs/contents"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, base+"/") && req.URL.Query().Get("ref") == "master":
			text, ok := f.files[strings.TrimPrefix(req.URL.Path, base+"/")]
			if !ok {
				http.Error(w, `{"message":"object does not exist"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(text)), "sha": factoryd.ContentHash(text)[:12]})
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/repos/sf/docs/commits" && req.URL.Query().Get("path") != "" &&
			req.URL.Query().Get("sha") == "master":
			// the file's head commit: the fake's version as its sha
			json.NewEncoder(w).Encode([]map[string]any{{"sha": "c0ffee" + strings.Repeat("0", f.version),
				"commit": map[string]any{"committer": map[string]any{"date": "2026-10-15T14:00:00Z"}}}})
		case req.Method == http.MethodPost && req.URL.Path == base:
			var body map[string]any
			json.NewDecoder(req.Body).Decode(&body)
			if f.slow > 0 {
				time.Sleep(f.slow)
			}
			if f.failCommits {
				http.Error(w, `{"message":"Forgejo is down"}`, http.StatusBadGateway)
				return
			}
			change := body["files"].([]any)[0].(map[string]any)
			path, _ := change["path"].(string)
			current, present := f.files[path]
			switch {
			case change["operation"] == "update" && (!present || change["sha"] != factoryd.ContentHash(current)[:12]):
				http.Error(w, `{"message":"sha does not match"}`, http.StatusConflict)
				return
			case change["operation"] == "create" && present:
				http.Error(w, `{"message":"file exists"}`, http.StatusUnprocessableEntity)
				return
			}
			raw, _ := base64.StdEncoding.DecodeString(change["content"].(string))
			f.files[path] = string(raw)
			f.version++
			f.commits = append(f.commits, body)
			f.tokens = append(f.tokens, req.Header.Get("Authorization"))
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c0ffee" + strings.Repeat("0", f.version)}})
		default:
			proxy.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// withDocs is the documents of the rig's Forgejo: sf/docs over r.fg.
func withDocs(t *testing.T, r *rig, files map[string]string) *docsFake {
	f, srv := docsOver(t, r.fg.srv.URL, files)
	r.c.cfg.ForgejoURL, r.c.cfg.DocsToken = srv.URL, "docs-token"
	return f
}

const docsPlan = "# The cache plan\n\nThe store: Postgres.\nThe window: one week.\n"

// read_doc answers the file with its base_sha256, or the lines holding a
// word; write_doc edits one passage against that hash and answers the new
// one; a new file takes content (roadmap 239, the 2026-10-13 plan).
func TestDocsReadAndEdit(t *testing.T) {
	r, _, _ := newChatRig(t)
	f := withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
	ctx, a := t.Context(), actor{s: &session{user: "sebastian"}, via: factoryd.ViaChat, chatTask: "t-chat"}
	hash := factoryd.ContentHash(docsPlan)
	got, err := r.c.readDoc(ctx, "sf/docs", "docs/plans/cache.md", "")
	equal(t, "the read", []any{got, err}, []any{"docs/plans/cache.md on sf/docs@master: 61 bytes, base_sha256 " + hash + ".\n\n" + docsPlan, error(nil)})
	got, _ = r.c.readDoc(ctx, "sf/docs", "docs/plans/cache.md", "WINDOW")
	equal(t, "the find", got, "docs/plans/cache.md on sf/docs@master: 61 bytes, base_sha256 "+hash+".\n\nline 4: The window: one week.\n")

	edited := strings.Replace(docsPlan, "one week", "two weeks", 1)
	got, err = r.c.doWriteDoc(ctx, a, "sf/docs", "sf/code", "docs/plans/cache.md", docEdit{Old: "one week", New: "two weeks", Base: hash})
	equal(t, "the edit", []any{got, err}, []any{"Committed c0ffee0 to sf/docs@master: docs/plans/cache.md, now base_sha256 " + factoryd.ContentHash(edited) + ".", error(nil)})
	equal(t, "the file", f.files["docs/plans/cache.md"], edited)
	equal(t, "the commit", []any{f.commits[0]["branch"], f.commits[0]["message"]}, []any{"master", "docs: docs/plans/cache.md\n\nFactory-Role: chat\nFactory-Task: t-chat\nRequested-by: sebastian\n"})

	got, err = r.c.doWriteDoc(ctx, a, "sf/docs", "sf/code", "docs/plans/new.md", docEdit{Content: "# New\n"})
	equal(t, "the new file", []any{got, err, f.files["docs/plans/new.md"]},
		[]any{"Committed c0ffee00 to sf/docs@master: docs/plans/new.md, now base_sha256 " + factoryd.ContentHash("# New\n") + ".", error(nil), "# New\n"})

	now := factoryd.ContentHash(edited)
	for _, tt := range []struct {
		name string
		path string
		edit docEdit
		want string
	}{
		{"content over an existing file", "docs/plans/cache.md", docEdit{Content: "x"}, "docs/plans/cache.md exists: edit a passage of it with old and new, base_sha256 " + now},
		{"a passage not there", "docs/plans/cache.md", docEdit{Old: "SQLite", New: "x", Base: now}, "the passage old is not in docs/plans/cache.md: read_doc it with find and copy the passage exactly"},
		{"a passage twice", "docs/plans/cache.md", docEdit{Old: "The ", New: "x", Base: now}, "the passage old is in docs/plans/cache.md 3 times: widen it until it is there once"},
		{"an edit of no file", "docs/plans/none.md", docEdit{Old: "a", New: "b"}, "docs/plans/none.md does not exist on master: a new file takes content"},
		{"both", "docs/plans/cache.md", docEdit{Old: "a", Content: "b"}, "content writes a new file and old and new edit an existing one: send one of the two"},
		{"outside the roots", "README.md", docEdit{Content: "b"}, "README.md is outside the document roots: docs/roadmap.md, docs/plans/, docs/board/ or docs/<name>.md"},
	} {
		if _, err := r.c.doWriteDoc(ctx, a, "sf/docs", "sf/code", tt.path, tt.edit); err == nil || err.Error() != tt.want {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
	equal(t, "the commits", len(f.commits), 2)
}

// Two writes read at one hash: the first lands, the second's stale hash
// refuses with the current one while its passage is still there, and its
// resend lands, so both edits are in the file; a stale write whose
// passage is gone gets the current content.
func TestDocsConcurrentWritesBothLand(t *testing.T) {
	r, _, _ := newChatRig(t)
	f := withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
	ctx, a := t.Context(), actor{s: &session{user: "sebastian"}, via: factoryd.ViaChat}
	base := factoryd.ContentHash(docsPlan)
	edits := []docEdit{{Old: "Postgres", New: "Postgres 17", Base: base}, {Old: "one week", New: "two weeks", Base: base}}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, e := range edits {
		wg.Go(func() { _, errs[i] = r.c.doWriteDoc(ctx, a, "sf/docs", "sf/code", "docs/plans/cache.md", e) })
	}
	wg.Wait()
	landed := 0
	for i, err := range errs {
		if err == nil {
			landed++
			continue
		}
		after := strings.Replace(docsPlan, edits[1-i].Old, edits[1-i].New, 1)
		want := "docs/plans/cache.md changed since it was read: its base_sha256 is now " + factoryd.ContentHash(after) +
			", and the passage old is still there once, so the same edit with that base_sha256 applies"
		equal(t, "the stale refusal", err.Error(), want)
		edits[i].Base = factoryd.ContentHash(after)
		if _, err := r.c.doWriteDoc(ctx, a, "sf/docs", "sf/code", "docs/plans/cache.md", edits[i]); err != nil {
			t.Fatalf("the resend: %v", err)
		}
	}
	equal(t, "the first round's landings", landed, 1)
	equal(t, "the file", f.files["docs/plans/cache.md"], "# The cache plan\n\nThe store: Postgres 17.\nThe window: two weeks.\n")

	_, err := r.c.doWriteDoc(ctx, a, "sf/docs", "sf/code", "docs/plans/cache.md", docEdit{Old: "one week", New: "three weeks", Base: base})
	now := f.files["docs/plans/cache.md"]
	if err == nil || err.Error() != "docs/plans/cache.md: it changed since it was read and the passage old is no longer there once. Its base_sha256 is now "+
		factoryd.ContentHash(now)+"; its current content:\n\n"+now {
		t.Fatalf("a stale write whose passage is gone = %v", err)
	}
}

// A roadmap edit that breaks an item's structure refuses.
func TestDocsRoadmapStructure(t *testing.T) {
	r, _, _ := newChatRig(t)
	roadmap := "# Roadmap\n\n- **7. A thing.** Sources: X. Where it stands: open.\n"
	withDocs(t, r, map[string]string{"docs/roadmap.md": roadmap})
	_, err := r.c.doWriteDoc(t.Context(), actor{s: &session{user: "sebastian"}, via: factoryd.ViaChat}, "sf/docs", "sf/code", "docs/roadmap.md",
		docEdit{Old: " Where it stands: open.", New: "", Base: factoryd.ContentHash(roadmap)})
	equal(t, "the refusal", err.Error(), "The roadmap edit breaks the file's structure: item 7 lost its Where-it-stands line.")
}

// The drawer's document read (the 2026-10-15 plan, item 1) renders a
// document's links by the round's engine rules: a relative target inside
// the roots opens in the drawer as a data-doc link, one outside stays
// quiet text with a title, an http target opens a new tab; raw HTML is
// escaped and headings carry ids. The answer carries the title, the
// content hash, the head commit and its time.
func TestDocsReadRendersLinks(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	layers := strings.Join([]string{
		"# The three layers",
		"",
		"The [roadmap](roadmap.md) names the sections, [an outside](../../escape.md) stays quiet, [the round](https://example.com/the-round) opens a tab, and <b>raw</b> is escaped.",
		"",
		"A [plan](plans/cache.md) of a sub-directory resolves.",
	}, "\n")
	withDocs(t, r, map[string]string{"docs/layers.md": layers, "docs/roadmap.md": "# Roadmap\n"})
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/docs/docs/layers.md", nil), ck)
	equal(t, "the status", rec.Code, http.StatusOK)
	equal(t, "the type", rec.Header().Get("Content-Type"), "application/json")
	var a docAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("the answer decodes: %v", err)
	}
	for _, want := range []string{
		`<h1 id="the-three-layers">The three layers</h1>`,
		`<a class="doc-link" data-doc="docs/roadmap.md" href="?doc=docs/roadmap.md">roadmap</a>`,
		`<a class="doc-link doc-missing" title="no document at that path">an outside</a>`,
		`<a class="doc-link doc-ext" href="https://example.com/the-round" target="_blank" rel="noopener">the round</a>`,
		`<a class="doc-link" data-doc="docs/plans/cache.md" href="?doc=docs/plans/cache.md">plan</a>`,
		`<!-- raw HTML omitted -->raw<!-- raw HTML omitted --> is escaped`,
	} {
		if !strings.Contains(a.HTML, want) {
			t.Errorf("the HTML holds %q: %s", want, a.HTML)
		}
	}
	equal(t, "the title", a.Title, "The three layers")
	equal(t, "the hash", a.SHA256, factoryd.ContentHash(layers))
	equal(t, "the head commit", a.Commit, "c0ffee")
	equal(t, "the commit's time", a.At.UTC(), time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC))
}

// ?raw=1 answers the text byte for byte, ?peek=1 the first paragraph as
// plain text — the hover peek's — cut at 280 bytes with an ellipsis.
func TestDocsReadRawAndPeek(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	plan := strings.Join([]string{
		"# The cache plan",
		"",
		"The store: Postgres. `The window: *one* week.`",
		"The second line joins it.",
		"",
		"- a list is no paragraph",
	}, "\n")
	withDocs(t, r, map[string]string{"docs/plans/cache.md": plan, "docs/plans/long.md": strings.Repeat("é", 200) + strings.Repeat("z", 200) + "\n"})
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/docs/docs/plans/cache.md?raw=1", nil), ck)
	equal(t, "the raw status", rec.Code, http.StatusOK)
	equal(t, "the raw type", rec.Header().Get("Content-Type"), "text/plain; charset=utf-8")
	equal(t, "the raw text", rec.Body.String(), plan)
	rec = r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/docs/docs/plans/cache.md?peek=1", nil), ck)
	equal(t, "the peek", rec.Body.String(), "The store: Postgres. The window: one week. The second line joins it.")
	rec = r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/docs/docs/plans/long.md?peek=1", nil), ck)
	equal(t, "the long peek", rec.Body.String(), strings.Repeat("é", 140)+"…")
}

// Whoever sees the project's board reads its documents, as its event
// stream's rule does: the project's writers, the factory's writers,
// Forgejo's readers of its repository; anyone else gets a 404.
func TestDocsReadAccess(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	withDocs(t, r, map[string]string{"docs/roadmap.md": "# Roadmap\n"})
	ck, _ := r.loggedIn(t) // sebastian writes the code project
	equal(t, "the project's writer reads", r.do(httptest.NewRequest(http.MethodGet,
		"/projects/ai-software-factory-code/docs/docs/roadmap.md", nil), ck).Code, http.StatusOK)
	members(r)
	bob, _ := memberLogin(t, r, "bob")
	equal(t, "another project's member does not read it", r.do(httptest.NewRequest(http.MethodGet,
		"/projects/ai-software-factory-code/docs/docs/roadmap.md", nil), bob).Code, http.StatusNotFound)
	alice, _ := memberLogin(t, r, "alice")
	equal(t, "the factory's writer reads every project's", r.do(httptest.NewRequest(http.MethodGet,
		"/projects/demo/docs/docs/roadmap.md", nil), alice).Code, http.StatusOK)
	equal(t, "the other project's writer reads it", r.do(httptest.NewRequest(http.MethodGet,
		"/projects/demo/docs/docs/roadmap.md", nil), bob).Code, http.StatusOK)
}

// A project that names no document repository has no Documents button,
// and the read refuses.
func TestDocsReadWithoutRepo(t *testing.T) {
	r, _, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/docs/docs/roadmap.md", nil), ck)
	equal(t, "the status", rec.Code, http.StatusConflict)
	equal(t, "the refusal", strings.TrimSpace(rec.Body.String()),
		"the project names no document repository (chat.doc_repo); there are no documents")
	page := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck)
	if strings.Contains(page.Body.String(), `id="documents"`) {
		t.Errorf("the board carries no Documents button without a doc repo")
	}
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	page = r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck)
	if !strings.Contains(page.Body.String(), `id="documents"`) {
		t.Errorf("the board carries the Documents button with a doc repo")
	}
}

// saveDoc posts the editor's save of path as the member of ck and answers
// the status with the answer's JSON.
func saveDoc(t *testing.T, r *rig, ck *http.Cookie, s *session, path string, save docSave) (int, struct {
	Reason  string `json:"reason"`
	SHA256  string `json:"sha256"`
	Current string `json:"current"`
}) {
	t.Helper()
	body, err := json.Marshal(save)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/docs/"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	rec := r.do(req, ck)
	var ans struct {
		Reason  string `json:"reason"`
		SHA256  string `json:"sha256"`
		Current string `json:"current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil && rec.Code == http.StatusOK {
		t.Fatalf("the answer %d decodes: %v (%s)", rec.Code, err, rec.Body.String())
	}
	return rec.Code, ans
}

// The editor's save (the 2026-10-15 plan, item 3) commits the member's
// whole text as one commit on master with the member's own Forgejo token:
// the message "<path>: edited in the console" with the trailers naming
// the member and the console. The answer names the text's hash, the
// commit and its time, and the door's one doc_written event follows, via
// console, naming the project's repository (item 4).
func TestDocsSaveCommitsAsMember(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	f := withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
	ck, s := r.loggedIn(t)
	before := r.events.count()
	edited := strings.Replace(docsPlan, "one week", "two weeks", 1)
	body, err := json.Marshal(docSave{Text: edited, Base: factoryd.ContentHash(docsPlan)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/docs/docs/plans/cache.md", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	rec := r.do(req, ck)
	equal(t, "the status", rec.Code, http.StatusOK)
	equal(t, "the type", rec.Header().Get("Content-Type"), "application/json")
	var saved docSaved
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("the answer decodes: %v (%s)", err, rec.Body.String())
	}
	equal(t, "the hash", saved.SHA256, factoryd.ContentHash(edited))
	equal(t, "the commit", saved.Commit, "c0ffee0")
	equal(t, "the commit's time", saved.At.UTC(), time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC))
	equal(t, "the commits", len(f.commits), 1)
	equal(t, "the message", f.commits[0]["message"], "docs/plans/cache.md: edited in the console\n\nFactory-Role: console\nRequested-by: sebastian\n")
	equal(t, "the member's token", f.tokens, []string{"Bearer access-1"})
	rows := r.events.all()[before:]
	equal(t, "the event rows", len(rows), 1)
	equal(t, "the doc_written event", []any{rows[0].Repository, rows[0].Kind, rows[0].Actor, rows[0].Via, rows[0].Detail["path"],
		rows[0].Detail["hash"], rows[0].Detail["commit"]},
		[]any{"sf/code", factoryd.EventDocWritten, "sebastian", factoryd.ViaConsole, "docs/plans/cache.md",
			factoryd.ContentHash(edited), "c0ffee0"})
}

// A save that breaks a piece of the write door refuses with a reason and
// commits nothing: a roadmap save that breaks an item's structure (422),
// a base hash the file left behind (409, with its current text and hash),
// a base hash for a file that does not exist, an empty text (400), and
// Forgejo refusing the commit (502). None leaves a doc_written row.
func TestDocsSaveRefusals(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	roadmap := "# Roadmap\n\n- **7. A thing.** Sources: X. Where it stands: open.\n"
	f := withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan, "docs/roadmap.md": roadmap})
	ck, s := r.loggedIn(t)
	before := r.events.count()
	save := func(path string, body docSave) (int, struct {
		Reason  string `json:"reason"`
		SHA256  string `json:"sha256"`
		Current string `json:"current"`
	}) {
		return saveDoc(t, r, ck, s, path, body)
	}
	code, ans := save("docs/roadmap.md", docSave{Text: strings.Replace(roadmap, " Where it stands: open.", "", 1),
		Base: factoryd.ContentHash(roadmap)})
	equal(t, "the structure refusal", []any{code, ans.Reason}, []any{http.StatusUnprocessableEntity,
		"The roadmap save breaks the file's structure: item 7 lost its Where-it-stands line."})
	code, ans = save("docs/plans/cache.md", docSave{Text: "another text", Base: factoryd.ContentHash("stale")})
	equal(t, "the stale refusal", []any{code, ans.Reason, ans.SHA256, ans.Current}, []any{http.StatusConflict,
		"docs/plans/cache.md changed since it was read: its base_sha256 is now " + factoryd.ContentHash(docsPlan) +
			", and its current text follows", factoryd.ContentHash(docsPlan), docsPlan})
	code, ans = save("docs/plans/new.md", docSave{Text: "# New\n", Base: factoryd.ContentHash(docsPlan)})
	equal(t, "the new-file refusal", []any{code, ans.Reason}, []any{http.StatusConflict,
		"docs/plans/new.md does not exist on master: save it without a base hash"})
	code, ans = save("docs/plans/cache.md", docSave{Text: "  ", Base: factoryd.ContentHash(docsPlan)})
	equal(t, "the empty refusal", []any{code, ans.Reason}, []any{http.StatusBadRequest,
		"the document would be empty; the editor does not delete documents"})
	f.failCommits = true
	code, ans = save("docs/plans/cache.md", docSave{Text: docsPlan + "More.", Base: factoryd.ContentHash(docsPlan)})
	equal(t, "the Forgejo refusal", []any{code, ans.Reason}, []any{http.StatusBadGateway,
		"Forgejo refused to commit the document; see the factoryd log"})
	f.failCommits = false
	equal(t, "the refusals committed nothing", []any{len(f.commits), len(f.tokens), r.events.count()}, []any{0, 0, before})
}

// Every write through the door leaves one doc_written row, via the door
// it came through: the chat's write_doc lands its commit and one row via
// chat, and a second write leaves a second row (the 2026-10-15 plan,
// item 4).
func TestDocWrittenForEveryDoorWrite(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
	ck, s := r.loggedIn(t)
	before := r.events.count()
	edited := strings.Replace(docsPlan, "one week", "two weeks", 1)
	g.replies = [][]string{{call(1, "write_doc", `{"path":"docs/plans/cache.md","old":"one week","new":"two weeks","base_sha256":"`+
		factoryd.ContentHash(docsPlan)+`"}`)}, {say("Done.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"edit the plan"}})
	if len(events) == 0 || !strings.Contains(events[0], `"name":"write_doc"`) {
		t.Fatalf("the write_doc ran: %v", events)
	}
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", Kind: factoryd.EventDocWritten,
			Detail: map[string]any{"path": "docs/plans/cache.md", "hash": factoryd.ContentHash(edited), "commit": "c0ffee0"}})
	edited2 := strings.Replace(edited, "two weeks", "three weeks", 1)
	g.replies = [][]string{{call(1, "write_doc", `{"path":"docs/plans/cache.md","old":"two weeks","new":"three weeks","base_sha256":"`+
		factoryd.ContentHash(edited)+`"}`)}, {say("Done again.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"again"}})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", Kind: factoryd.EventDocWritten,
			Detail: map[string]any{"path": "docs/plans/cache.md", "hash": factoryd.ContentHash(edited), "commit": "c0ffee0"}},
		factoryd.Event{Repository: "sf/code", Kind: factoryd.EventDocWritten,
			Detail: map[string]any{"path": "docs/plans/cache.md", "hash": factoryd.ContentHash(edited2), "commit": "c0ffee00"}})
}
