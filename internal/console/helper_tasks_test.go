package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// helperRig seats a plan whose prover runs, a card whose sizing runs after
// its screening ended, and a card whose screening runs. The plan ended its
// planning 30 days ago and stays open while its cards and its prover work.
func helperRig(t *testing.T) (*rig, *http.Cookie, *session) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	now := time.Now()
	ago := func(d time.Duration) *time.Time {
		at := now.Add(-d)
		return &at
	}
	card := func(id string, n int64, status, title string) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Repository: "sf/code", IssueNumber: n, Objective: title, Track: policy.TrackDirect,
			Source: factoryd.SourceWebhook, BudgetEUR: 2, CreatedAt: now.Add(-2 * time.Hour), AdmittedAt: ago(time.Hour)}
	}
	helper := func(id, source, parent, status string, filed time.Duration) factoryd.Task {
		h := factoryd.Task{ID: id, Kind: "agent", Status: status, Repository: "sf/code", Objective: factoryd.SourceBriefs[source](factoryd.BriefFacts{}).Title,
			Track: policy.TrackDirect, Source: source, Parent: parent, BudgetEUR: 1, CreatedAt: now.Add(-filed)}
		if factoryd.Final(status) {
			h.FinishedAt = ago(filed - 2*time.Minute)
		}
		return h
	}
	plan := card("t-plan", 40, "done", "Board export: snapshots out and back in")
	plan.Track, plan.PlanName, plan.FinishedAt = policy.TrackPlan, "board export", ago(30*24*time.Hour)
	r.tasks.set("",
		plan, helper("t-prove", factoryd.SourceProver, "t-plan", "running", 20*time.Minute),
		card("t-c", 41, "running", "Board export: import a snapshot back"),
		helper("t-screen", factoryd.SourceScreening, "t-c", "done", 10*time.Minute),
		helper("t-size", factoryd.SourceSizing, "t-c", "running", 3*time.Minute),
		card("t-d", 42, "running", "Board export: keep the nightly snapshots"),
		helper("t-screen-d", factoryd.SourceScreening, "t-d", "running", 4*time.Minute))
	return r, ck, s
}

// A helper task is a step of its parent's life: the board holds no card
// for it, finished or not, and its parent's state names the step while it
// runs, on the small card and on the opened card. A plan whose prover runs
// stays on the board past the finished window.
func TestHelperTasksShowOnTheirParent(t *testing.T) {
	r, ck, _ := helperRig(t)
	body := getPage(r, ck, "/").Body.String()
	for _, id := range []string{"t-prove", "t-screen", "t-size", "t-screen-d"} {
		if strings.Contains(body, `data-card="`+id+`"`) {
			t.Errorf("the board holds a card for the helper task %s", id)
		}
	}
	if done := columnItems(t, body, "Done"); !equalIDs(done, "t-plan") {
		t.Errorf("Done holds %v, want the plan alone and no finished helper task", done)
	}
	if progress := columnItems(t, body, "In progress"); !equalIDs(progress, "t-c", "t-d") && !equalIDs(progress, "t-d", "t-c") {
		t.Errorf("In progress holds %v, want t-c and t-d", progress)
	}
	// the state's one time is the step's own: the prover filed 20 min ago,
	// the sizing 3 min, the screening 4 min
	for _, tc := range []struct{ id, small, opened string }{
		{"t-plan", `<span class="it st">writing the proofs</span> <span class="it">20 min</span>`, `<span class="it st">writing the proofs</span> <span class="it">20 min</span>`},
		{"t-c", `></i>sizing</span> 3 min</span>`, `></i>sizing</span></span> <span class="it">3 min</span>`},
		{"t-d", `></i>screening</span> 4 min</span>`, `></i>screening</span></span> <span class="it">4 min</span>`},
	} {
		small, err := boardItems(body, "data-card", tc.id)
		if err != nil || !strings.Contains(string(small), tc.small) {
			t.Errorf("%s's small card lacks %q (%v):\n%s", tc.id, tc.small, err, small)
		}
		if opened := drawer(t, r, ck, tc.id, "overview"); !strings.Contains(opened, tc.opened) {
			t.Errorf("%s's opened card lacks %q:\n%s", tc.id, tc.opened, opened)
		}
	}
}

// A helper task that stops and asks a human asks on its parent: its case
// is a page of the parent's card in Needs you, after the parent's own, and
// its answers go to the helper's case.
func TestAHelpersAskIsAPageOfItsParent(t *testing.T) {
	r, ck, s := helperRig(t)
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-size" {
			r.tasks.tasks[i].Status = "waiting-human"
		}
	}
	r.tasks.mu.Unlock()
	opened := time.Now().Add(-time.Hour)
	r.openCases = map[string][]factoryd.Case{
		"t-c":    {{ID: 5, TaskID: "t-c", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman, OpenedAt: opened, Facts: map[string]any{"flag": "wait_over_bound"}}},
		"t-size": {{ID: 7, TaskID: "t-size", Kind: factoryd.CaseGateStop, Status: factoryd.CaseAtHuman, OpenedAt: opened.Add(time.Minute)}},
	}
	needs := needsOf(t, r, ck, "/")
	if strings.Contains(needs, `data-card="t-size"`) || !strings.Contains(needs, `data-card="t-c"`) || !strings.Contains(needs, `<span class="pos" aria-live="polite">1 of 2</span>`) {
		t.Fatalf("Needs you does not hold t-c with its own ask and its sizing's:\n%s", needs)
	}
	// the page names the step that asks, on the small card and the opened one
	page := getAs(r, ck, caseURL("t-c", 7), true)
	if body := page.Body.String(); page.Code != http.StatusOK || !strings.Contains(body, `<span class="pos" aria-live="polite">2 of 2</span>`) ||
		!strings.Contains(body, `data-card="t-c"`) || !strings.Contains(body, `<span class="flag">the sizing asks</span>`) {
		t.Fatalf("the sizing's page of t-c = %d:\n%s", page.Code, body)
	}
	drawerBody := getPage(r, ck, "/cards/t-c/drawer?tab=overview&case=t-c:7").Body.String()
	if want := `<p class="o-note">This ask is the sizing&#39;s, a step of this card: its answers act on the sizing alone.</p>`; !strings.Contains(drawerBody, want) {
		t.Errorf("the opened card does not say whose ask it shows, %s:\n%s", want, drawerBody)
	}
	pressed := answerForm(t, drawerBody, factoryd.ActClose)
	if pressed.Action != "/tasks/t-size/resolve" || pressed.Values.Get("case_id") != "7" {
		t.Fatalf("the sizing's answer posts to %s with case %q, want /tasks/t-size/resolve with case 7", pressed.Action, pressed.Values.Get("case_id"))
	}
	form := url.Values{"csrf": {s.csrf}}
	for k, v := range pressed.Values {
		form[k] = v
	}
	if rec := r.do(post(pressed.Action, form), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the press = %d: %s", rec.Code, rec.Body)
	}
	in, _ := actedOnce(t, r)
	if in.TaskID != "t-size" || in.CaseID != 7 || in.Res.Action != factoryd.ActClose {
		t.Fatalf("Act took %+v, want close on the sizing's case 7", in)
	}
}

func equalIDs(got []string, want ...string) bool {
	return strings.Join(got, " ") == strings.Join(want, " ")
}
