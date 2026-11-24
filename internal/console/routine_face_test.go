package console

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// A routine's card meets the card contract (C16, C46): its small card is
// the face every card renders through, one paragraph of its title and the
// why line its file holds, a foot of its schedule, its next run in words
// and its last run, and no button, for no answer is suggested. Its opened
// card leads with the title and the why line, and offers its three
// answers as every opened card does: the vocabulary's label, its effect
// line, and a press on the routine's door that the drawer answers.
func TestRoutineCardMeetsTheContract(t *testing.T) {
	r := newRig(t)
	bump := soonBump()
	bump.Routine.Why = "So security fixes reach the factory within a week."
	r.routines.views = []factoryd.RoutineView{bump}
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	g := ghostIn(t, col(t, body, "Queued"), "weekly-bump")
	for _, want := range []string{
		`<article class="card face ghost" data-ghost="ai-software-factory-code/weekly-bump">`,
		`<button type="button" class="open" data-routine="ai-software-factory-code/weekly-bump"><span class="visually-hidden">Open the routine Weekly bump</span></button>`,
		`<p class="para"><span class="glyph" aria-hidden="true">↻</span> <strong class="title">Weekly bump</strong>. <span class="why">So security fixes reach the factory within a week.</span></p>`,
		`<p class="foot"><span class="it">every Monday 06:00</span> <span class="it soon">` + string(moment(bump.Next, "Mon 15:04 MST")) + ` · in 20 min · human card</span> ` +
			`<span class="it">last run <a class="ref-link"`,
	} {
		if !strings.Contains(g, want) {
			t.Errorf("the routine's small card lacks\n%s\n%s", want, g)
		}
	}
	if strings.Contains(g, "<form") || strings.Contains(g, "data-card=") {
		t.Errorf("the routine's small card holds a form or reads as a work card:\n%s", g)
	}
	if prose := htmlText(template.HTML(g)); len([]rune(prose)) > 300 {
		t.Errorf("the small card holds %d characters of prose, want at most 300: %q", len([]rune(prose)), prose)
	}

	d := getPage(r, ck, "/routines/"+bumpKey+"/drawer").Body.String()
	if !strings.Contains(d, `<p class="o-why">So security fixes reach the factory within a week.</p>`) ||
		!strings.Contains(d, "routine · every Monday 06:00 · human card · next "+string(moment(bump.Next, "Mon 15:04 MST"))+", in 20 min") {
		t.Errorf("the opened routine card lacks its why line or its next run in words:\n%s", d)
	}
	for _, x := range []struct{ key, act, value string }{
		{factoryd.AnswerSkipRun, "skip", ""},
		{factoryd.KeyRunAlone, "needs-me", `<input type="hidden" name="needs_me" value="0">`},
		{factoryd.AnswerRunNow, "run", ""},
	} {
		a, ok := factoryd.AnswerOf(x.key)
		if !ok {
			t.Fatalf("the vocabulary has no %s", x.key)
		}
		form := `action="/routines/` + bumpKey + `/` + x.act + `" hx-post="/routines/` + bumpKey + `/` + x.act + `" hx-target="#drawer" hx-swap="innerHTML" class="answer" data-answer="` + a.Key() + `">`
		button := `<button type="submit" class="pick">` + a.Label + `</button> <span class="effect">` + template.HTMLEscapeString(a.Effect) + `</span>`
		i := strings.Index(d, form)
		if i < 0 || !strings.Contains(d[i:], button) || !strings.Contains(d[i:], x.value) || !strings.Contains(d[i:], `name="csrf" value="`+s.csrf+`"`) {
			t.Errorf("the opened routine card lacks %q with its effect line on its door:\n%s\n%s\n%s", a.Label, form, button, d)
		}
	}
}
