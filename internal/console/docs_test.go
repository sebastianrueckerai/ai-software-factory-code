package console

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// docsFake is the document repository sf/docs on a Forgejo as its contents
// API answers: master's files by path with a blob sha per version, a
// commit answered 201; every other call goes to the rig's Forgejo.
type docsFake struct {
	mu      sync.Mutex
	files   map[string]string
	version int
	commits []map[string]any
}

func withDocs(t *testing.T, r *rig, files map[string]string) *docsFake {
	t.Helper()
	f := &docsFake{files: files}
	next, err := url.Parse(r.fg.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(next)
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
		case req.Method == http.MethodPost && req.URL.Path == base:
			var body map[string]any
			json.NewDecoder(req.Body).Decode(&body)
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
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c0ffee" + strings.Repeat("0", f.version)}})
		default:
			proxy.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(srv.Close)
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
	got, err = r.c.doWriteDoc(ctx, a, "sf/docs", "docs/plans/cache.md", docEdit{Old: "one week", New: "two weeks", Base: hash})
	equal(t, "the edit", []any{got, err}, []any{"Committed c0ffee0 to sf/docs@master: docs/plans/cache.md, now base_sha256 " + factoryd.ContentHash(edited) + ".", error(nil)})
	equal(t, "the file", f.files["docs/plans/cache.md"], edited)
	equal(t, "the commit", []any{f.commits[0]["branch"], f.commits[0]["message"]}, []any{"master", "docs: docs/plans/cache.md\n\nFactory-Role: chat\nFactory-Task: t-chat\nRequested-by: sebastian\n"})

	got, err = r.c.doWriteDoc(ctx, a, "sf/docs", "docs/plans/new.md", docEdit{Content: "# New\n"})
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
		if _, err := r.c.doWriteDoc(ctx, a, "sf/docs", tt.path, tt.edit); err == nil || err.Error() != tt.want {
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
		wg.Go(func() { _, errs[i] = r.c.doWriteDoc(ctx, a, "sf/docs", "docs/plans/cache.md", e) })
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
		if _, err := r.c.doWriteDoc(ctx, a, "sf/docs", "docs/plans/cache.md", edits[i]); err != nil {
			t.Fatalf("the resend: %v", err)
		}
	}
	equal(t, "the first round's landings", landed, 1)
	equal(t, "the file", f.files["docs/plans/cache.md"], "# The cache plan\n\nThe store: Postgres 17.\nThe window: two weeks.\n")

	_, err := r.c.doWriteDoc(ctx, a, "sf/docs", "docs/plans/cache.md", docEdit{Old: "one week", New: "three weeks", Base: base})
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
	_, err := r.c.doWriteDoc(t.Context(), actor{s: &session{user: "sebastian"}, via: factoryd.ViaChat}, "sf/docs", "docs/roadmap.md",
		docEdit{Old: " Where it stands: open.", New: "", Base: factoryd.ContentHash(roadmap)})
	equal(t, "the refusal", err.Error(), "The roadmap edit breaks the file's structure: item 7 lost its Where-it-stands line.")
}
