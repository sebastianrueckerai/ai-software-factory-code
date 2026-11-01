package console

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// A press answers at once: the note and nothing else into the note
// outlet, the board's re-render behind the board-refresh trigger, so the
// press never waits on the board's reads.
func TestPlanAnswerPressAnswersWithTheNoteAlone(t *testing.T) {
	r := newRig(t)
	questionsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(htmxPost("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"1"}, "answer": {"60 seconds"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer = %d:\n%s", rec.Code, rec.Body)
	}
	if got, want := rec.Body.String(), `<p class="note" role="status">Answered question 1 of plan #1.</p>`; got != want {
		t.Fatalf("body = %q, want only the note %q", got, want)
	}
	if rec.Header().Get("HX-Trigger") != "board-refresh" {
		t.Fatalf("HX-Trigger = %q, want board-refresh", rec.Header().Get("HX-Trigger"))
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

// A refused press names its refusal into the note outlet; the board
// stays as it is.
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

// The press wiring on disk: every posting form targets the note outlet,
// the reads (find and menus) keep the board, the board itself re-renders
// on board-refresh, and the page holds the outlet a screen reader hears.
func TestPressWiringConstruction(t *testing.T) {
	for _, name := range []string{"templates/board.html", "templates/drawer.html"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "hx-post=") || strings.Contains(line, "hx-select=") ||
				strings.Contains(line, `hx-target="#drawer"`) || strings.Contains(line, "routine-act-target") {
				continue
			}
			if !strings.Contains(line, `hx-target="#note" hx-swap="innerHTML"`) {
				t.Errorf("FIX MISSING: %s posts to the board, want the note outlet:\n%s", name, line)
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
	if !strings.Contains(string(board), `{{else}}hx-target="#note" hx-swap="innerHTML"{{end}}`) {
		t.Errorf("FIX MISSING: a routine act outside the drawer posts to the board, want the note outlet")
	}
	for _, want := range []string{`<form class="find"`, `class="menu-pop auto-submit" hx-get="/"`} {
		if !strings.Contains(string(board), want) {
			t.Errorf("FIX MISSING: the board lost its read form %q", want)
		}
	}
	page, err := os.ReadFile("templates/page.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `<div id="note" aria-live="polite"></div>`) {
		t.Errorf("FIX MISSING: the page holds no note outlet")
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
