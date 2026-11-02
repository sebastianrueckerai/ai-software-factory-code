package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pressRefreshes reports whether rec answers an htmx press as it should:
// no content and the board-refresh trigger.
func pressRefreshes(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusNoContent && rec.Body.Len() == 0 && rec.Header().Get("HX-Trigger") == "board-refresh"
}

// A press answers at once: no content and the board-refresh trigger, so
// the board's re-render is the answer and the press never waits on the
// board's reads.
func TestPlanAnswerPressAnswersWithTheBoardAlone(t *testing.T) {
	r := newRig(t)
	questionsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(htmxPost("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"1"}, "answer": {"60 seconds"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("answer = %d %q, want 204 with no body", rec.Code, rec.Body)
	}
	if rec.Header().Get("HX-Trigger") != "board-refresh" {
		t.Fatalf("HX-Trigger = %q, want board-refresh", rec.Header().Get("HX-Trigger"))
	}
	const comments = "/api/v1/repos/linkshort/linkshort/issues/1/comments"
	if r.fg.comments[comments] != "Answer 1: 60 seconds" || !slices.Contains(r.fg.calls, "SIGNAL plan_answered t-plan 2 1 by sebastian") {
		t.Fatalf("comment %q, calls %v, want the answer posted and signalled", r.fg.comments[comments], r.fg.calls)
	}
}

// A form post without htmx keeps its redirect to the whole page: the
// no-JS press still lands on a board.
func TestPlanAnswerPressWithoutHtmxRedirects(t *testing.T) {
	r := newRig(t)
	questionsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"1"}, "answer": {"60 seconds"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" {
		t.Fatalf("answer = %d to %q, want the redirect to the board", rec.Code, rec.Header().Get("Location"))
	}
}

// A refused press names its refusal with its status; the board stays as
// it is.
func TestPlanAnswerPressRefusalNamesTheRefusal(t *testing.T) {
	r := newRig(t)
	questionsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(htmxPost("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"9"}, "answer": {"x"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusConflict || rec.Body.String() != "Plan #1 revision 2 has no question 9.\n" {
		t.Fatalf("refusal = %d %q", rec.Code, rec.Body)
	}
}

// The press wiring on disk: no template or script names a note outlet, a
// posting form targets nothing but the drawer, the reads (find and menus)
// keep the board, and the board itself re-renders on board-refresh.
func TestPressWiringConstruction(t *testing.T) {
	templates, err := filepath.Glob("templates/*")
	if err != nil || len(templates) == 0 {
		t.Fatalf("templates = %v, %v", templates, err)
	}
	for _, name := range append(templates, "static/console.js") {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "#note") {
			t.Errorf("FIX MISSING: %s still names the note outlet", name)
		}
	}
	for _, name := range []string{"templates/board.html", "templates/drawer.html"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "hx-post=") || strings.Contains(line, "hx-select=") || strings.Contains(line, "routine-act-target") {
				continue
			}
			if strings.Contains(line, "hx-target=") && !strings.Contains(line, `hx-target="#drawer"`) {
				t.Errorf("FIX MISSING: %s posts to a target other than the drawer:\n%s", name, line)
			}
		}
	}
	board, err := os.ReadFile("templates/board.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(board), `hx-trigger="board-refresh from:body"`) || !strings.Contains(string(board), `hx-get="/board{{.Query}}"`) {
		t.Errorf("FIX MISSING: the board div carries no board-refresh trigger with its GET")
	}
	if !strings.Contains(string(board), `{{define "routine-act-target"}}{{if .Drawer}} hx-target="#drawer" hx-swap="innerHTML"{{end}}{{end}}`) {
		t.Errorf("FIX MISSING: a routine act targets the drawer from the drawer and nothing else")
	}
	for _, want := range []string{`<form class="find"`, `class="menu-pop auto-submit" hx-get="/"`} {
		if !strings.Contains(string(board), want) {
			t.Errorf("FIX MISSING: the board lost its read form %q", want)
		}
	}
}

// The pending state rides the form: htmx marks the form it posts, not the
// button, so the dim and the spinner sit on the form's buttons.
func TestPressPendingRidesTheForm(t *testing.T) {
	css, err := os.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "form.htmx-request button") || strings.Contains(string(css), "button.htmx-request") {
		t.Error("FIX MISSING: the pending state does not ride the form's buttons")
	}
}
