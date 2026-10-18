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
	if face := faceOf(t, body, "t-run"); !strings.Contains(face, `<span class="pill st-running">building</span> <span class="elapsed">4 h 0 min</span>`) {
		t.Errorf("the face lacks the card's age beside its stage (C11):\n%s", face)
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-run/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, "<dd>building, 20 min, 4 h 0 min in all</dd>") {
		t.Errorf("the drawer counts the stage from the admission, not the newest status row:\n%s", drawer)
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
