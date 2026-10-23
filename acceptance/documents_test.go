package acceptance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The documents' live flow end to end (roadmap 277, the 2026-10-15 plan,
// Verification's Live paragraph), driven through the console's HTTP
// surface on the acceptance rig with the document repository fake
// seated: Sebastian opens the roadmap, edits a line, saves, and the
// commit lands on master under his name; the chat's agent edits other
// lines, his next save merges; it edits the same lines, his save is
// refused with the clash. One commit per landed save, none on a refusal,
// one event row per write.

// documentsRoadmap is the roadmap the fake seeds: two items whose Sources
// link the plans, so the read renders drawer links.
const documentsRoadmap = "# The roadmap\n\n" +
	"- **7. The documents in the screen.** Sources: [the documents plan](plans/documents.md). Where it stands: in progress.\n" +
	"- **8. The board beside the chat.** Sources: [the board plan](plans/board.md). Where it stands: done.\n"

// docsRig is the harness for a documents run: the member logged in and
// the document repository sf/docs seated with the roadmap and the two
// plans it links. It answers the rig, the fake, the project's chat and
// docs paths, the session cookie and the form token.
func docsRig(t *testing.T) (*AcceptanceRig, *AccDocs, string, string, *http.Cookie, string) {
	t.Helper()
	r, docs, chatPath := NewAcceptanceRig(t, false, map[string]string{
		"docs/roadmap.md":         documentsRoadmap,
		"docs/plans/documents.md": "# The documents plan\n\nThe drawer's document mode.\n",
		"docs/plans/board.md":     "# The board plan\n\nThe board beside the chat.\n",
	})
	ck, csrf := r.Login(t)
	return r, docs, chatPath, strings.TrimSuffix(chatPath, "/chat") + "/docs", ck, csrf
}

// docProposalToken answers the write_doc proposal's token in the
// stream's events.
func docProposalToken(t *testing.T, events []string) string {
	t.Helper()
	for _, e := range events {
		if !strings.HasPrefix(e, "proposal ") {
			continue
		}
		var p struct {
			ID   string `json:"id"`
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "proposal ")), &p); err != nil {
			t.Fatalf("the proposal event %q: %v", e, err)
		}
		if p.Tool == "write_doc" {
			return p.ID
		}
	}
	t.Fatalf("the stream holds no write_doc proposal:\n%s", strings.Join(events, "\n"))
	return ""
}

// agentWrites runs the chat turn whose write_doc call the member OKs: the
// message proposes the agent's edit, the Do it lands it. It answers the
// Do it's stream events.
func agentWrites(t *testing.T, r *AcceptanceRig, chatPath string, ck *http.Cookie, csrf, call, message string) []string {
	t.Helper()
	r.Reply([]string{call}, []string{Say("Done.")})
	code, body := postChat(t, r, chatPath, ck, url.Values{"message": {message}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	token := docProposalToken(t, eventsOf(t, body))
	code, body = postChat(t, r, chatPath+"/proposals/"+token, ck, url.Values{"csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	return eventsOf(t, body)
}

// getDoc runs one GET on the console as the member of ck through a real
// server and answers the status with the body.
func getDoc(t *testing.T, r *AcceptanceRig, ck *http.Cookie, path string) (int, []byte) {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// docSaveBody is the editor's save: the whole text, the content hash it
// based it on and the head commit it names.
type docSaveBody struct {
	Text   string `json:"text"`
	Base   string `json:"base"`
	Commit string `json:"commit,omitempty"`
}

// docSaveAnswer covers the save's answer and its refusals: the landed
// hash, commit and merge, or the reason with the current text, its hash
// and the clash's who, time and lines.
type docSaveAnswer struct {
	SHA256  string   `json:"sha256"`
	Commit  string   `json:"commit"`
	Merged  string   `json:"merged"`
	Text    string   `json:"text"`
	Reason  string   `json:"reason"`
	Current string   `json:"current"`
	Who     string   `json:"who"`
	At      string   `json:"at"`
	Lines   [][2]int `json:"lines"`
}

// saveDoc posts the editor's save of path as the member through a real
// server and answers the status with the answer's JSON.
func saveDoc(t *testing.T, r *AcceptanceRig, docsPath string, ck *http.Cookie, csrf, path string, save docSaveBody) (int, docSaveAnswer) {
	t.Helper()
	body, err := json.Marshal(save)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+docsPath+"/"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(ck)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var ans docSaveAnswer
	if err := json.Unmarshal(b, &ans); err != nil {
		t.Fatalf("the save's answer %d decodes: %v (%s)", resp.StatusCode, err, b)
	}
	return resp.StatusCode, ans
}

// docRows answers the event spine's doc_written rows as
// "actor via path hash commit", and fails on any other row.
func docRows(t *testing.T, r *AcceptanceRig) []string {
	t.Helper()
	var out []string
	for _, e := range r.Events() {
		if e.Kind != factoryd.EventDocWritten {
			t.Fatalf("an event row besides the writes: %+v", e)
		}
		out = append(out, strings.Join([]string{e.Actor, e.Via, e.Detail["path"].(string),
			e.Detail["hash"].(string), e.Detail["commit"].(string)}, " "))
	}
	return out
}

// TestDocumentsEndToEnd is the live flow's first half: Sebastian opens
// the roadmap, the read rendered with drawer links; he edits a line and
// saves, and one commit lands on master under his name with the console's
// message; the chat's agent edits other lines, and his next save commits
// the merged text once. One commit per landed save, one event row per
// write.
func TestDocumentsEndToEnd(t *testing.T) {
	r, docs, chatPath, docsPath, ck, csrf := docsRig(t)

	// He opens the roadmap: the read renders its links as drawer links.
	code, body := getDoc(t, r, ck, docsPath+"/docs/roadmap.md")
	equal(t, "the read's status", code, http.StatusOK)
	var opened struct {
		Path, Title, HTML, SHA256, Commit string
		At                                time.Time
	}
	if err := json.Unmarshal(body, &opened); err != nil {
		t.Fatalf("the read's answer decodes: %v (%s)", err, body)
	}
	equal(t, "the opened document", []any{opened.Path, opened.Title, opened.SHA256, opened.Commit, opened.At.UTC()},
		[]any{"docs/roadmap.md", "The roadmap", factoryd.ContentHash(documentsRoadmap), "c0ffee",
			time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)})
	for _, link := range []string{
		`<a class="doc-link" data-doc="docs/plans/documents.md" href="?doc=docs/plans/documents.md">the documents plan</a>`,
		`<a class="doc-link" data-doc="docs/plans/board.md" href="?doc=docs/plans/board.md">the board plan</a>`,
	} {
		if !strings.Contains(opened.HTML, link) {
			t.Errorf("the rendered roadmap lacks the drawer link %q:\n%s", link, opened.HTML)
		}
	}

	// He edits a line and saves: one commit on master under his name, the
	// member's own Forgejo token, and one doc_written row.
	mine := strings.Replace(documentsRoadmap, "Where it stands: done.", "Where it stands: in review.", 1)
	code, saved := saveDoc(t, r, docsPath, ck, csrf, "docs/roadmap.md",
		docSaveBody{Text: mine, Base: factoryd.ContentHash(documentsRoadmap)})
	equal(t, "the save's status", code, http.StatusOK)
	equal(t, "the save's answer", []any{saved.SHA256, saved.Commit, saved.Merged},
		[]any{factoryd.ContentHash(mine), "c0ffee0", ""})
	commits := docs.Commits()
	equal(t, "the landed commits", len(commits), 1)
	equal(t, "the commit's message", commits[0].Message,
		"docs/roadmap.md: edited in the console\n\nFactory-Role: console\nRequested-by: sebastian\n")
	equal(t, "the commit under his name", commits[0].Bearer, "Bearer access-1")
	equal(t, "the write's row", docRows(t, r),
		[]string{"sebastian console docs/roadmap.md " + factoryd.ContentHash(mine) + " c0ffee0"})

	// The chat's agent edits other lines: the proposal, his Do it, one
	// commit and its row via chat.
	theirs := strings.Replace(mine, "Where it stands: in progress.", "Where it stands: in review.", 1)
	events := agentWrites(t, r, chatPath, ck, csrf, Call(1, "write_doc",
		`{"path":"docs/roadmap.md","old":"Where it stands: in progress.","new":"Where it stands: in review.","base_sha256":"`+
			factoryd.ContentHash(mine)+`"}`), "mark item 7 in review")
	want := "Committed c0ffee00 to sf/docs@master: docs/roadmap.md, now base_sha256 " + factoryd.ContentHash(theirs) + "."
	if !hasToolResult(events, "write_doc", want) {
		t.Errorf("the write_doc's answer:\n%s\nwant the result %q", strings.Join(events, "\n"), want)
	}
	equal(t, "the file after the agent's edit", docs.Files()["docs/roadmap.md"], theirs)
	equal(t, "the landed commits", len(docs.Commits()), 2)
	equal(t, "the writes' rows", docRows(t, r), []string{
		"sebastian console docs/roadmap.md " + factoryd.ContentHash(mine) + " c0ffee0",
		"sebastian chat docs/roadmap.md " + factoryd.ContentHash(theirs) + " c0ffee00"})

	// His next save, based on the text he saved, merges the agent's
	// change: the merged text commits once.
	typed := strings.Replace(mine, "# The roadmap", "# The roadmap of the factory", 1)
	merged := strings.Replace(theirs, "# The roadmap", "# The roadmap of the factory", 1)
	code, resaved := saveDoc(t, r, docsPath, ck, csrf, "docs/roadmap.md",
		docSaveBody{Text: typed, Base: factoryd.ContentHash(mine), Commit: "c0ffee0"})
	equal(t, "the merging save's status", code, http.StatusOK)
	equal(t, "the merging save's answer", []any{resaved.Merged, resaved.Text, resaved.SHA256, resaved.Commit},
		[]any{"merged with sebastian's change", merged, factoryd.ContentHash(merged), "c0ffee000"})
	equal(t, "the merged file", docs.Files()["docs/roadmap.md"], merged)
	commits = docs.Commits()
	equal(t, "the landed commits", len(commits), 3)
	equal(t, "the merged commit's message", commits[2].Message,
		"docs/roadmap.md: edited in the console\n\nFactory-Role: console\nRequested-by: sebastian\n")
	equal(t, "one row per write", docRows(t, r), []string{
		"sebastian console docs/roadmap.md " + factoryd.ContentHash(mine) + " c0ffee0",
		"sebastian chat docs/roadmap.md " + factoryd.ContentHash(theirs) + " c0ffee00",
		"sebastian console docs/roadmap.md " + factoryd.ContentHash(merged) + " c0ffee000"})
}

// TestDocumentsConflictEndToEnd is the live flow's second half: the chat's
// agent edits the same lines Sebastian is typing on, and his save refuses
// with the current text and its hash, committing nothing; the agent's
// landed change raised exactly one doc_written; a save that breaks the
// roadmap's structure refuses, too.
func TestDocumentsConflictEndToEnd(t *testing.T) {
	r, docs, chatPath, docsPath, ck, csrf := docsRig(t)

	// The agent edits the same lines he is typing on: one commit, and the
	// push raised exactly one doc_written.
	theirs := strings.Replace(documentsRoadmap, "Where it stands: done.", "Where it stands: in review.", 1)
	events := agentWrites(t, r, chatPath, ck, csrf, Call(1, "write_doc",
		`{"path":"docs/roadmap.md","old":"Where it stands: done.","new":"Where it stands: in review.","base_sha256":"`+
			factoryd.ContentHash(documentsRoadmap)+`"}`), "mark item 8 in review")
	if !hasToolResult(events, "write_doc",
		"Committed c0ffee0 to sf/docs@master: docs/roadmap.md, now base_sha256 "+factoryd.ContentHash(theirs)+".") {
		t.Fatalf("the agent's write_doc did not land:\n%s", strings.Join(events, "\n"))
	}
	equal(t, "the push's one row", docRows(t, r),
		[]string{"sebastian chat docs/roadmap.md " + factoryd.ContentHash(theirs) + " c0ffee0"})

	// His save on those lines refuses with the current text and its hash,
	// the clash's who, time and lines, and commits nothing.
	mine := strings.Replace(documentsRoadmap, "Where it stands: done.", "Where it stands: closed.", 1)
	code, refused := saveDoc(t, r, docsPath, ck, csrf, "docs/roadmap.md",
		docSaveBody{Text: mine, Base: factoryd.ContentHash(documentsRoadmap), Commit: "c0ffee"})
	equal(t, "the refusal's status", code, http.StatusConflict)
	equal(t, "the clash", []any{refused.Reason, refused.Who, refused.At, refused.Lines},
		[]any{"sebastian changed the same lines (14:00)", "sebastian", "2026-10-15T14:00:00Z", [][2]int{{4, 4}}})
	equal(t, "the current text and hash", []any{refused.SHA256, refused.Current},
		[]any{factoryd.ContentHash(theirs), theirs})
	equal(t, "the refusal committed nothing", len(docs.Commits()), 1)
	equal(t, "the refusal raised no row", docRows(t, r),
		[]string{"sebastian chat docs/roadmap.md " + factoryd.ContentHash(theirs) + " c0ffee0"})

	// A save that breaks an item's structure refuses, committing nothing.
	code, raw := getDoc(t, r, ck, docsPath+"/docs/roadmap.md?raw=1")
	equal(t, "the raw read's status", code, http.StatusOK)
	equal(t, "the raw text", string(raw), theirs)
	broken := strings.Replace(theirs, " Where it stands: in review.", "", 1)
	code, structural := saveDoc(t, r, docsPath, ck, csrf, "docs/roadmap.md",
		docSaveBody{Text: broken, Base: factoryd.ContentHash(theirs)})
	equal(t, "the structure refusal", []any{code, structural.Reason}, []any{http.StatusUnprocessableEntity,
		"The roadmap save breaks the file's structure: item 8 lost its Where-it-stands line."})
	equal(t, "the structure refusal committed nothing", len(docs.Commits()), 1)
	equal(t, "the rows", docRows(t, r),
		[]string{"sebastian chat docs/roadmap.md " + factoryd.ContentHash(theirs) + " c0ffee0"})
}
