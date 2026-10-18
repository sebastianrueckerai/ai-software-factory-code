package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The waiting time of a Needs you item: minutes, or hours and minutes,
// since the item began to wait; empty when its start is unknown.
func TestNeedWaiting(t *testing.T) {
	now := time.Now()
	if got := (need{}).Waiting(); got != "" {
		t.Errorf("a need without a start waits %q, want nothing", got)
	}
	if got := (need{Since: now.Add(-25 * time.Minute)}).Waiting(); got != "25 min" {
		t.Errorf("a need of 25 minutes waits %q", got)
	}
	if got := (approvalCard{ApprovalRequest: factoryd.ApprovalRequest{CreatedAt: now.Add(-125 * time.Minute)}}).Waiting(); got != "2 h 5 min" {
		t.Errorf("an approval of 125 minutes waits %q", got)
	}
	if got := (approvalCard{}).Waiting(); got != "" {
		t.Errorf("an approval without a filing time waits %q, want nothing", got)
	}
}

// A card's stage without any time to count from has a word and no age.
func TestStageWithoutATime(t *testing.T) {
	word, age := stage(factoryd.Task{Status: "running", Lane: policy.LaneReady}, time.Now())
	if word != "building" || age != "" {
		t.Errorf("stage = %q, %q; want building and no age", word, age)
	}
	word, age = stage(factoryd.Task{Status: "queued"}, time.Now())
	if word != "" || age != "" {
		t.Errorf("a queued card has a stage %q, %q", word, age)
	}
}

// The time in the stage counts from the card's newest status row when
// that row is the newest event (C8): a card re-run after review shows
// the time since it ran again, not since its first admission.
func TestStageAgeFromTheNewestStatusRow(t *testing.T) {
	r, ev := newRigWithEvents(t)
	admitted := time.Now().Add(-3 * time.Hour)
	r.tasks.set("", factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Lane: policy.LaneReady, Repository: "sf/code", Objective: "run again",
		CreatedAt: admitted.Add(-time.Hour), AdmittedAt: &admitted})
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventTaskStatus, Actor: factoryd.FactoryDecider,
		Via: factoryd.ViaFactoryd, At: time.Now().Add(-20 * time.Minute), Detail: map[string]any{"status": "running", "gate": ""}}); err != nil {
		t.Fatal(err)
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-run"); !strings.Contains(face, `<span class="pill st-running">building</span> <span class="elapsed">20 min</span>`) {
		t.Errorf("the face lacks the time in its stage:\n%s", face)
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-run/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, "<dd>building, 20 min, 4 h 0 min in all</dd>") {
		t.Errorf("the drawer counts the stage from the admission, not the newest status row:\n%s", drawer)
	}
}

// The status panel's "In progress" and "Needs you" lists went with C20
// (card t-e98f769e872d), and with them the test that read them
// (TestStatusPanelListsInTheWideView): the lists duplicated the board —
// the strip's counts already say how many cards work and need you, and
// the columns already list the cards — so no list remains in the panel
// whose content could be tested; its figures are covered by
// TestBoardPageCarriesTheFiguresOfTheStatusPanel and, under a find, by
// TestFindNarrowsTheColumns. What needs holding is the shape the ruling
// leaves, in both views: the panel carries only its Spend, Provider,
// GitHub, configuration and Dependencies sections (the last unknown until
// the prober writes, roadmap 299), and the columns list the cards the
// lists once named — the human task as a plain card, with no escalation
// article of its own.
func TestStatusPanelListsNoCards(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Lane: policy.LaneReady, Repository: "sf/code", Objective: "working", CreatedAt: time.Now().Add(-time.Hour)},
		factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/code", Objective: "rotate the key", IssueNumber: 16})
	ck, _ := r.loggedIn(t)
	for _, page := range []string{"/", "/?view=wide"} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		start := strings.Index(body, `<div class="sp" id="status-panel"`)
		if start < 0 {
			t.Fatalf("%s: the status panel is missing", page)
		}
		panel := body[start:]
		end := strings.Index(panel, "</section>\n</div>")
		if end < 0 {
			t.Fatalf("%s: the status panel does not close", page)
		}
		panel = panel[:end]
		if n := strings.Count(panel, "<h3>"); n != 3 || !strings.Contains(panel, "<h3>Spend</h3>") || !strings.Contains(panel, "<h3>Provider, GitHub, configuration</h3>") || !strings.Contains(panel, "<h3>Dependencies</h3>") {
			t.Errorf("%s: the panel holds %d sections, want only Spend, Provider, GitHub, configuration and Dependencies:\n%s", page, n, panel)
		}
		if strings.Contains(body, `class="sp-link"`) || strings.Contains(panel, "<h3>In progress</h3>") || strings.Contains(panel, "<h3>Needs you</h3>") {
			t.Errorf("%s: the panel still lists the board's cards", page)
		}
		if strings.Contains(body, `id="need-t-h"`) {
			t.Errorf("%s: the human task carries an escalation article it has not", page)
		}
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(col(t, body, "Needs you"), `data-card="t-h"`) || !strings.Contains(col(t, body, "In progress"), `data-card="t-run"`) {
		t.Error("the four columns do not list the human task and the running card the panel once listed")
	}
	wide := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	if !strings.Contains(col(t, wide, "Human tasks"), `data-card="t-h"`) || !strings.Contains(wide, `data-card="t-run"`) {
		t.Error("the wide view does not list the human task and the running card the panel once listed")
	}
}

// A queued card with an event keeps its face bare: the activity line
// speaks only on a card in progress or waiting.
func TestAQueuedCardDoesNotSpeak(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.set("", factoryd.Task{ID: "t-q", Kind: "agent", Status: "queued", Lane: policy.LaneReady, Repository: "sf/code", Objective: "queued", CreatedAt: time.Now()})
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-q", Kind: factoryd.EventCardMoved, Actor: "sebastian", Via: factoryd.ViaButton, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-q"); strings.Contains(face, `class="activity"`) {
		t.Errorf("a queued card speaks:\n%s", face)
	}
}

// The page's policy allows no inline style (style-src 'self'), so a
// template must set nothing by a style attribute: a width set that way is
// dropped, and the gauge it drew stayed empty.
func TestNoInlineStyle(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Lane: policy.LaneReady, Repository: "sf/code", Objective: "spends", BudgetEUR: 10, SpentEUR: 5.1})
	ck, _ := r.loggedIn(t)
	for _, page := range []string{"/", "/?view=wide", "/cards/t-run/drawer?tab=overview", "/costs", "/tasks/t-run/timeline"} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		if strings.Contains(body, ` style="`) {
			t.Errorf("%s sets a style attribute, which the policy drops", page)
		}
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `<span class="gauge"><meter min="0" max="100" value="`) {
		t.Errorf("the strip's gauge is not a meter")
	}
}

// A span in words: under a minute, minutes, hours and minutes, days and
// hours.
func TestMinutesInWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "under a minute", 7 * time.Minute: "7 min", 125 * time.Minute: "2 h 5 min",
		24 * time.Hour: "1 d 0 h", 50 * time.Hour: "2 d 2 h", 100*time.Hour + 59*time.Minute: "4 d 4 h",
	} {
		if got := minutes(d); got != want {
			t.Errorf("minutes(%s) = %q, want %q", d, got, want)
		}
	}
}
