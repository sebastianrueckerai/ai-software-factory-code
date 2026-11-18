package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// faceOf is the face of card id: the first card article in section that
// carries its id. An escalation article shares the face's data-card, so
// the face's own match is the one with a place.
func faceOf(t *testing.T, section, id string) string {
	t.Helper()
	i := strings.Index(section, `data-card="`+id+`" data-place="`)
	if i < 0 {
		i = strings.Index(section, `data-card="`+id+`"`)
	}
	if i < 0 {
		t.Fatalf("card %s missing:\n%s", id, section)
	}
	start := strings.LastIndex(section[:i], "<article")
	return section[start : i+strings.Index(section[i:], "</article>")]
}

// titleOf is the title on the small card of card id.
func titleOf(t *testing.T, body, id string) string {
	t.Helper()
	face := faceOf(t, body, id)
	_, title, _ := strings.Cut(face, `<strong class="title">`)
	title, _, _ = strings.Cut(title, "</strong>")
	return title
}

// The face's title is the objective's first paragraph, the title as the
// issue was filed, whole: never cut, never a plan child's key.
func TestCardFaceTitle(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for _, tc := range []struct {
		name string
		task factoryd.Task
		want string
	}{
		{"plan key", factoryd.Task{PlanKey: "web-pages", Objective: "Build the pages. All of them."}, "Build the pages. All of them."},
		{"no full stop, a long body first sentence", factoryd.Task{Objective: factoryd.IssueObjective("Merge approval only on a green staged head",
			"A merge approval request reaches the merge gate after its checks pass, and the approver should see a green staged head before approving it.")}, "Merge approval only on a green staged head"},
		{"an internal full stop keeps the whole title", factoryd.Task{Objective: factoryd.IssueObjective("Fix the cache. It drops entries.", "Then update the tests.")}, "Fix the cache. It drops entries."},
		{"a final full stop keeps the title", factoryd.Task{Objective: factoryd.IssueObjective("Fix the cache.", "Then update the tests.")}, "Fix the cache."},
		{"an objective with no body", factoryd.Task{Objective: factoryd.IssueObjective("Speed up the board", "")}, "Speed up the board"},
		{"a path keeps its dot", factoryd.Task{Objective: "Fix internal/console/board.go so it renders"}, "Fix internal/console/board.go so it renders"},
		{"a version keeps its dot", factoryd.Task{Objective: "Bump to v1.2 and release"}, "Bump to v1.2 and release"},
		{"lines joined", factoryd.Task{Objective: "Fix the\ncache"}, "Fix the cache"},
		{"a hidden comment shows on no face", factoryd.Task{Objective: factoryd.IssueObjective("Fix the cache <!-- later --> now", "Then the tests.")}, "Fix the cache now"},
		{"80 runes", factoryd.Task{Objective: strings.Repeat("é", 80)}, strings.Repeat("é", 80)},
		{"over 80 runes, whole", factoryd.Task{Objective: strings.Repeat("abcdefghij ", 10)}, strings.TrimSuffix(strings.Repeat("abcdefghij ", 10), " ")},
		{"over 80 runes, one word", factoryd.Task{Objective: strings.Repeat("é", 81)}, strings.Repeat("é", 81)},
	} {
		tc.task.ID, tc.task.Kind, tc.task.Status, tc.task.Repository = "t-1", "agent", "running", "sf/code"
		r.tasks.set("", tc.task)
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		if got := titleOf(t, body, "t-1"); got != tc.want {
			t.Errorf("%s: title %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The foot's state: one hue per state and the board's word for it, so the
// waits stay apart; the author's wait wears the hollow hue, a card in the
// Queued column the queued hue, and a card parked on its image says it
// waits for it. A final card's word stands without a dot. A waiting card
// stands in Needs you with its ask, so its state is its opened card's.
func TestCardFacePillPerStatus(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for status, want := range map[string]string{
		"running":        `<span class="st st-running"><i class="dot" aria-hidden="true"></i>building</span>`,
		"closing":        `<span class="st st-running"><i class="dot" aria-hidden="true"></i>building</span>`,
		"queued":         `<span class="st st-queued"><i class="dot" aria-hidden="true"></i>queued</span>`,
		"waiting-deps":   `<span class="st st-queued"><i class="dot" aria-hidden="true"></i>queued</span>`,
		"waiting-image":  `<span class="st st-queued"><i class="dot" aria-hidden="true"></i>queued</span>`,
		"waiting-human":  `<span class="st st-waiting"><i class="dot" aria-hidden="true"></i>needs you</span>`,
		"waiting-poster": `<span class="st st-waiting hollow"><i class="dot" aria-hidden="true"></i>needs you</span>`,
		"in-review":      `<span class="st st-review"><i class="dot" aria-hidden="true"></i>verifying</span>`,
		"done":           `<p class="foot"><span class="it st">done</span>`,
		"failed":         `<p class="foot"><span class="it st">failed</span>`,
		"closed":         `<p class="foot"><span class="it st">closed</span>`,
	} {
		r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: status, Track: "direct", Repository: "sf/code", Objective: "work"})
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		face := faceOf(t, body, "t-1")
		if status == "waiting-human" || status == "waiting-poster" {
			face = drawer(t, r, ck, "t-1", "overview")
		}
		if !strings.Contains(face, want) || status == "waiting-image" && !strings.Contains(face, `<span class="it waits">waits for its image</span>`) {
			t.Errorf("%s: the face lacks %s:\n%s", status, want, face)
		}
	}
}

// From 80 percent of its budget, to the cent, a card's foot warns with the
// sign and the words; below it, and without a budget, the foot shows no
// money, which the card's life holds.
func TestCardFaceSpend(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-low", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "low", BudgetEUR: 10, SpentEUR: 7.99},
		factoryd.Task{ID: "t-at", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "at 80", BudgetEUR: 10, SpentEUR: 8},
		factoryd.Task{ID: "t-cents", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "at 80 in floats", BudgetEUR: 3, SpentEUR: 2.4},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no budget"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	const near = `<span class="it flag">⚠ near its budget</span>`
	for _, id := range []string{"t-at", "t-cents"} {
		if face := faceOf(t, body, id); !strings.Contains(face, near) {
			t.Errorf("%s lacks %s:\n%s", id, near, face)
		}
	}
	for _, id := range []string{"t-low", "t-none"} {
		if face := faceOf(t, body, id); strings.Contains(face, "near its budget") || strings.Contains(face, "€") {
			t.Errorf("%s shows money on its face:\n%s", id, face)
		}
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-low/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, `<span class="mono">€7.99 of €10.00, 80 percent</span></p>`) {
		t.Errorf("the card's life lacks the spend under 80 percent:\n%s", drawer)
	}
}

// The near-budget flag counts the holds: 3.80 settled of 5.00 is 76
// percent, under the 80 percent line, and 3.80 with a 0.40 hold is 84
// percent, over it.
func TestNearBudgetNetsHolds(t *testing.T) {
	r := newRig(t)
	held := factoryd.Task{ID: "t-held", Kind: "agent", Status: "running", Repository: "sf/code", BudgetEUR: 5, SpentEUR: 3.8, HeldEUR: 0.4}
	settled := held
	settled.ID, settled.HeldEUR = "t-settled", 0
	if !r.c.card(held).NearBudget {
		t.Error("4.20 of 5.00 with the hold is not near its budget")
	}
	if r.c.card(settled).NearBudget {
		t.Error("3.80 of 5.00 with no hold is near its budget")
	}
}

// The card's money nets the gateway's holds, the figure its caps and
// floor count: 4.50 settled and 0.40 held of 5.00 warns on the face and
// on the opened card's head, and its life shows 4.90 spent.
func TestBoardLeftNetsHolds(t *testing.T) {
	r := fundRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-e", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?",
		BudgetEUR: 5, SpentEUR: 4.5, HeldEUR: 0.4, ModelAlias: "factory/small"})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face, want := faceOf(t, body, "t-e"), `<span class="it flag">⚠ near its budget</span>`; !strings.Contains(face, want) {
		t.Errorf("the face lacks %s:\n%s", want, face)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{`<span class="it flag">⚠ near its budget</span>`, `<span class="mono">€4.90 of €5.00, 98 percent</span> ⚠ near its budget`} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the opened card lacks %s:\n%s", want, drawer)
		}
	}
}

// The calls line beside the spend: the task's call rows summed — its
// tokens to date, prompt, cached and output, and the modeled
// pay-as-you-go cost of them (never money billed, roadmap 206); a card
// without call rows shows none.
func TestCardFaceCalls(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-calls", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "calls", BudgetEUR: 5, SpentEUR: 1.2},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no calls", BudgetEUR: 5, SpentEUR: 1.2})
	r.ledger.calls = map[string][]policy.ModelCall{
		"t-calls": {
			{Ref: "small", EUR: 0.0180, Usage: policy.Usage{PromptTokens: 70000, CachedTokens: 66000, OutputTokens: 1200}},
			{Ref: "big", EUR: 0.0240, Usage: policy.Usage{PromptTokens: 30000, OutputTokens: 800}},
		},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-calls"); strings.Contains(face, "modeled") {
		t.Errorf("the face shows the calls:\n%s", face)
	}
	want := `<span class="mono">€1.20 of €5.00, 24 percent</span> <span class="mono" title="the task's model calls to date: the tokens its providers reported and the modeled pay-as-you-go cost of them, never money billed">· 100,000 prompt · 66,000 cached · 2,000 output tokens · €0.0420 modeled</span></p>`
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-calls/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, want) {
		t.Errorf("the drawer lacks its calls beside the budget:\n%s", drawer)
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-none/drawer?tab=overview", nil), ck).Body.String(); strings.Contains(drawer, "modeled") {
		t.Errorf("a card without call rows shows calls:\n%s", drawer)
	}
}

// The foot holds only what a card has: never its model or a badge; a
// queued card whose dependencies are not done waits for them, a card in
// waiting-deps for what it waits on, and a card whose dependencies are
// done only for its turn.
func TestCardFaceChips(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-bare", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "bare", CreatedAt: time.Now()}},
		{Task: factoryd.Task{ID: "t-full", Kind: "agent", Status: "running", Track: "direct", Repository: "sf/code", Objective: "full",
			ModelAlias: "small"}, Dependencies: []factoryd.DepFact{{Issue: 3, Status: "queued"}, {Issue: 5, Status: "queued"}}},
		{Task: factoryd.Task{ID: "t-blocked", Kind: "agent", Status: "queued", Track: "direct", Repository: "sf/code", Objective: "blocked"},
			Dependencies: []factoryd.DepFact{{Issue: 3, Status: "queued"}, {Issue: 5, Status: "queued"}}},
		{Task: factoryd.Task{ID: "t-free", Kind: "agent", Status: "queued", Track: "direct", Repository: "sf/code", Objective: "deps done"}},
		{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "waiting-deps", Track: "direct", Repository: "sf/code", Objective: "waits"},
			WaitingOn: "#7", Dependencies: []factoryd.DepFact{{Issue: 7, Status: "queued"}}},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if full := faceOf(t, body, "t-full"); strings.Contains(full, "small") || strings.Contains(full, `class="badge"`) || strings.Contains(full, "waits") {
		t.Errorf("the running face names the model, wears a badge or waits:\n%s", full)
	}
	if bare := faceOf(t, body, "t-bare"); !strings.Contains(bare, `<p class="foot"><span class="it"><span class="st st-running"><i class="dot" aria-hidden="true"></i>building</span> under a minute</span></p>`) {
		t.Errorf("a card with nothing to show has more than its state in its foot:\n%s", bare)
	}
	const blocked = `<span class="it waits">waits for <a class="ref-link" href="https://git.example/sf/code/issues/3" target="_blank" rel="noopener">#3</a>`
	if b := faceOf(t, body, "t-blocked"); !strings.Contains(b, blocked) || !strings.Contains(b, `#5</a>`) {
		t.Errorf("a queued card with open dependencies does not wait for them:\n%s", b)
	}
	if free := faceOf(t, body, "t-free"); !strings.Contains(free, `<span class="it waits">waits for its turn to start</span>`) {
		t.Errorf("a card whose dependencies are done waits for more than its turn:\n%s", free)
	}
	if wait := faceOf(t, body, "t-wait"); strings.Count(wait, `class="it waits"`) != 1 ||
		!strings.Contains(wait, `<span class="it waits">waits for <a class="ref-link" href="https://git.example/sf/code/issues/7" target="_blank" rel="noopener">#7</a>`) {
		t.Errorf("a card in waiting-deps does not wait for exactly #7:\n%s", wait)
	}
}

// A plan child's place is the plan's name, a link that opens the plan's
// card, never its own key, and its place in the plan's filing order
// (links-api files after the foundation and before the integration card
// that waits on both); an unnamed plan's place is its title. The
// plan's own place names the plan and counts its cards, with no #0 for a
// plan filed before its issue.
func TestPlanChildChipCarriesTheName(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "running", Track: "plan", Process: "spec", Objective: "Link shortener",
			Repository: "linkshort/linkshort", IssueNumber: 1}},
		{Task: factoryd.Task{ID: "t-c4", Kind: "agent", Status: "running", Track: "direct", Process: "spec", Objective: "Links API",
			Repository: "linkshort/linkshort", IssueNumber: 4, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "links-api"}},
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {planRevision("t-plan", 1, "sha-1")}}
	place := func(name string) string {
		return `<span class="it place"><a class="ref-link" href="#card=t-plan">` + name + `</a> 2/3</span>`
	}
	for _, tc := range []struct {
		name, child, plan string
		issue             int64
	}{
		{"", place("Link shortener"), `<span class="it place">plan of 3 cards</span>`, 1},
		{"fix prod latency", place("fix prod latency"), `<span class="it place">plan fix prod latency, 3 cards</span>`, 1},
		{"fix prod latency", place("fix prod latency"), `<span class="it place">plan fix prod latency, 3 cards</span>`, 0},
	} {
		r.tasks.tasks[0].PlanName, r.tasks.tasks[0].IssueNumber = tc.name, tc.issue
		body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
		if face := faceOf(t, body, "t-c4"); !strings.Contains(face, tc.child) || strings.Contains(face, "links-api") {
			t.Errorf("%q #%d: t-c4's place is not %s, or shows the key:\n%s", tc.name, tc.issue, tc.child, face)
		}
		if face := faceOf(t, body, "t-plan"); !strings.Contains(face, tc.plan) || strings.Contains(face, "#0") {
			t.Errorf("%q #%d: the plan's own place is not %s:\n%s", tc.name, tc.issue, tc.plan, face)
		}
	}
}

// A done plan's card counts its cards in its place, the plan's revision
// its source.
func TestCardFacePlanChildren(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = planTasks()
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {planRevision("t-plan", 1, "sha-1")}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, `<span class="it place">plan of 3 cards</span>`) {
		t.Errorf("the plan's face does not count its cards:\n%s", face)
	}
}

// Checks n/n from the newest checks step of the card's timeline, read for
// a card with a pull request that is not final; a final card and one
// without a pull request show none.
func TestCardFaceChecks(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	timeline := []byte(`{"step":"session_start","attempt":1}
{"step":"checks","state":"failure","jobs":[{"name":"test","status":"failure"},{"name":"vet","status":"success"}]}
{"step":"checks","state":"failure","jobs":[{"name":"test","status":"success"},{"name":"vet","status":"success"},{"name":"lint","status":"failure"}]}
{"step":"model_review","verdict":"approve"}
`)
	for _, id := range []string{"t-pr", "t-done", "t-nopr"} {
		r.objects["sessions/"+id+"/timeline.jsonl"] = timeline
	}
	r.objects["sessions/t-skip/timeline.jsonl"] = []byte(`{"step":"checks","state":"success","jobs":[{"name":"test","status":"success"},{"name":"docs","status":"skipped"},{"name":"vet","status":"success"}]}
`)
	r.tasks.set("",
		factoryd.Task{ID: "t-pr", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "a pr", PRNumber: 4},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", Objective: "merged", PRNumber: 5},
		factoryd.Task{ID: "t-nopr", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no pr yet"},
		factoryd.Task{ID: "t-skip", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "a skipped job", PRNumber: 6})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-pr"); !strings.Contains(face, `<span class="it">2/3 checks</span>`) {
		t.Errorf("the card in review lacks 2/3 checks:\n%s", face)
	}
	if face := faceOf(t, body, "t-skip"); !strings.Contains(face, `<span class="it">2/2 checks</span>`) {
		t.Errorf("the card with a skipped job lacks 2/2 checks:\n%s", face)
	}
	for _, id := range []string{"t-done", "t-nopr"} {
		if face := faceOf(t, body, id); strings.Contains(face, "checks</span>") {
			t.Errorf("%s shows checks:\n%s", id, face)
		}
	}
}

// The face's checks count names the head it judged: a newer push the
// checks never ran on leaves the earlier pass's counts off the face
// (#1396 wore "5/5 checks" from a pass its cycle had left behind), and
// they return once checks run on that newer head. With no push at all,
// the newest checks step stands on its own.
func TestCardFaceChecksNameTheHeadTheyJudged(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	fiveOf := func(commit string) string {
		return `{"step":"checks","commit":"` + commit + `","state":"success","jobs":[` +
			`{"name":"build","status":"success"},{"name":"vet","status":"success"},{"name":"race","status":"success"},` +
			`{"name":"lint","status":"success"},{"name":"docs","status":"success"}]}`
	}
	push := func(commit string) string {
		return `{"step":"pushed","session":"s","branch":"b","commit":"` + commit + `"}`
	}
	start := `{"step":"session_start","attempt":1}`
	r.objects["sessions/t-stale/timeline.jsonl"] = []byte(start + "\n" + push("aaa") + "\n" + fiveOf("aaa") + "\n" + push("bbb") + "\n")
	r.objects["sessions/t-caught-up/timeline.jsonl"] = []byte(start + "\n" + push("aaa") + "\n" + fiveOf("aaa") + "\n" + push("bbb") + "\n" + fiveOf("bbb") + "\n")
	r.objects["sessions/t-no-push/timeline.jsonl"] = []byte(start + "\n" + fiveOf("aaa") + "\n")
	r.tasks.set("",
		factoryd.Task{ID: "t-stale", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "a newer push", PRNumber: 4},
		factoryd.Task{ID: "t-caught-up", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "checked again", PRNumber: 5},
		factoryd.Task{ID: "t-no-push", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "checks only", PRNumber: 6})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-stale"); strings.Contains(face, "checks</span>") {
		t.Errorf("a push the checks never ran on still wears their counts:\n%s", face)
	}
	if face := faceOf(t, body, "t-caught-up"); !strings.Contains(face, `<span class="it">5/5 checks</span>`) {
		t.Errorf("the checks on the newest head lack their 5/5:\n%s", face)
	}
	if face := faceOf(t, body, "t-no-push"); !strings.Contains(face, `<span class="it">5/5 checks</span>`) {
		t.Errorf("checks with no push to be stale against lack their 5/5:\n%s", face)
	}
}

// The foot holds the state's word and its one time, the stage's age,
// with no attempt beside it: the runs and their attempts are the opened
// card's.
func TestCardFaceAttemptBesideTheGateClock(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	attempts := `{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"retry"}
{"step":"session_start","attempt":2}
{"step":"session_end","attempt":2,"outcome":"retry"}
{"step":"session_start","attempt":3}`
	r.objects["sessions/t-gate/timeline.jsonl"] = []byte(attempts)
	since := time.Now().Add(-(25*time.Minute + 30*time.Second))
	r.tasks.set("", factoryd.Task{ID: "t-gate", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "at the gate", PRNumber: 4,
		CreatedAt: since, Gate: factoryd.GateChecks, GateSince: &since})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := `<span class="it"><span class="st st-review"><i class="dot" aria-hidden="true"></i>verifying</span> 25 min</span>`
	if face := faceOf(t, body, "t-gate"); !strings.Contains(face, want) || strings.Contains(face, "attempt") {
		t.Errorf("the card at its gate lacks %s, or names its attempt:\n%s", want, face)
	}
}

// countedObjects counts the reads of each object key.
type countedObjects struct {
	fakeObjects
	mu    sync.Mutex
	reads map[string]int
}

func (o *countedObjects) Get(ctx context.Context, k string) ([]byte, error) {
	o.mu.Lock()
	o.reads[k]++
	o.mu.Unlock()
	return o.fakeObjects.Get(ctx, k)
}

// A card's checks count is read once per checksTTL: a second render within
// it reads no timeline, one after it reads the timeline again.
func TestCardFaceChecksCached(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	objects := &countedObjects{fakeObjects: r.objects, reads: map[string]int{}}
	r.c.cfg.Timelines = objects
	key := "sessions/t-pr/timeline.jsonl"
	r.objects[key] = []byte(`{"step":"checks","state":"success","jobs":[{"name":"test","status":"success"}]}` + "\n")
	r.tasks.set("", factoryd.Task{ID: "t-pr", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "a pr", PRNumber: 4})
	render := func(wantReads int) {
		t.Helper()
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		if face := faceOf(t, body, "t-pr"); !strings.Contains(face, `<span class="it">1/1 checks</span>`) {
			t.Errorf("the card lacks 1/1 checks:\n%s", face)
		}
		if got := objects.reads[key]; got != wantReads {
			t.Errorf("the timeline was read %d times, want %d", got, wantReads)
		}
	}
	render(1)
	render(1)
	r.c.mu.Lock()
	got := r.c.checks["t-pr"]
	got.at = got.at.Add(-checksTTL)
	r.c.checks["t-pr"] = got
	r.c.mu.Unlock()
	render(2)
}

// The board renders an escalated card once, in Needs you, and no id
// repeats on the page; the opened card's brief and colour fields each
// carry their own id.
func TestEscalatedCardRepeatsNoID(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("The agent asks: which region?", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
		IssueNumber: 13, Objective: "asks"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if n := strings.Count(body, `data-card="t-e"`); n != 1 {
		t.Errorf("the board renders the escalated card's data-card %d times, want once", n)
	}
	ids := map[string]int{}
	for _, m := range regexp.MustCompile(` id="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		ids[m[1]]++
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("id %q appears %d times", id, n)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer", nil), ck).Body.String()
	for _, want := range []string{
		`<input type="text" id="f-t-e-edit-brief-title-03b496b3" name="edit_brief.title" value="asks" hx-preserve>`, `<textarea id="f-t-e-edit-brief-summary" name="edit_brief.summary" rows="2" maxlength="4000" hx-preserve></textarea>`,
		`<select id="f-t-e-accent-accent" name="accent.accent">`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %s:\n%s", want, drawer)
		}
	}
	ids = map[string]int{}
	for _, m := range regexp.MustCompile(` id="([^"]+)"`).FindAllStringSubmatch(drawer, -1) {
		ids[m[1]]++
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("the drawer's id %q appears %d times", id, n)
		}
	}
}

// The paragraph takes no margins and wraps a long word inside the card,
// the foot's separator trails its item, and the accent edge rules come
// after .card .card with its weight, so a nested face keeps its edge.
func TestCardFaceCSS(t *testing.T) {
	r := newRig(t)
	css := r.do(httptest.NewRequest(http.MethodGet, "/static/console.css", nil)).Body.String()
	nested := strings.Index(css, ".card .card {")
	for _, accent := range factoryd.Accents {
		if edge := strings.Index(css, ".face.edge-"+accent+" {"); edge < nested {
			t.Errorf(".face.edge-%s at %d, not after .card .card at %d", accent, edge, nested)
		}
		if strings.Contains(css, "\n.edge-"+accent) {
			t.Errorf("a bare .edge-%s rule loses to .card .card", accent)
		}
	}
	for _, rule := range []string{
		".para { margin: 0; color: var(--text-muted); font-size: 13px; line-height: 1.45; overflow-wrap: anywhere; }",
		`.foot .it:not(:last-child)::after { content: "\00a0·"; }`,
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("console.css lacks %s", rule)
		}
	}
}

// An accent is the card's left edge and the foot's last item, its dot and
// its name, and changes no state rendering; a card without one has
// neither.
func TestCardFaceAccent(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-teal", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "teal work", Accent: "teal", CreatedAt: time.Now()},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "plain work", CreatedAt: time.Now()})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	teal := faceOf(t, body, "t-teal")
	if !strings.HasPrefix(teal, `<article class="card face edge-teal" data-card="t-teal" `) ||
		!strings.Contains(teal, `<span class="it accent"><i class="dot acc-teal" aria-hidden="true"></i>teal</span></p>`) ||
		!strings.Contains(teal, `<span class="st st-running"><i class="dot" aria-hidden="true"></i>building</span>`) {
		t.Errorf("the teal card lacks its edge, its accent or its plain running state:\n%s", teal)
	}
	if none := faceOf(t, body, "t-none"); !strings.HasPrefix(none, `<article class="card face" data-card="t-none" `) || strings.Contains(none, "accent") {
		t.Errorf("a card without an accent shows one:\n%s", none)
	}
}

// The amber ring and the stale mark: a live card whose newest event is
// half an hour old or more, or that has had none in its half hour on the
// board; never a final card, one parked in the queue or one that waits
// for a human's answer. The opened card says how long.
func TestCardFaceStaleRing(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	old, recent := time.Now().Add(-45*time.Minute), time.Now().Add(-5*time.Minute)
	r.tasks.set("",
		factoryd.Task{ID: "t-quiet", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "quiet", CreatedAt: old},
		factoryd.Task{ID: "t-silent", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "silent", CreatedAt: old},
		factoryd.Task{ID: "t-busy", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "busy", CreatedAt: old},
		factoryd.Task{ID: "t-new", Kind: "agent", Status: "queued", Track: "direct", Repository: "sf/code", Objective: "never moved", CreatedAt: old},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", Objective: "done", CreatedAt: old})
	for id, at := range map[string]time.Time{"t-quiet": old, "t-busy": recent, "t-done": old, "t-new": old} {
		if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: id, Kind: factoryd.EventCardRetried,
			Actor: "sebastian", Via: factoryd.ViaButton, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-ask", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", Objective: "asks", CreatedAt: old}, Escalation: "The agent asks: which region?\nthe details"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	// a quiet running card wears the ring, its mark in its band, and the
	// opened card says how long nothing happened
	for id, words := range map[string]string{"t-quiet": "stale, nothing for over 45 min", "t-silent": "stale, no event yet, 45 min on the card"} {
		if face := faceOf(t, body, id); !strings.HasPrefix(face, `<article class="card face stale" data-card="`+id+`" `) || !strings.Contains(face, `<span class="it flag">stale</span>`) {
			t.Errorf("%s lacks the ring or the stale mark:\n%s", id, face)
		}
		if d := drawer(t, r, ck, id, "overview"); !strings.Contains(d, `<span class="it flag">`+words+`</span>`) {
			t.Errorf("the opened card of %s lacks %q:\n%s", id, words, d)
		}
	}
	// a card that waits for a human's answer owes no work: no ring
	for _, id := range []string{"t-busy", "t-done", "t-new", "t-ask"} {
		if face := faceOf(t, body, id); strings.Contains(face, "stale") || strings.Contains(face, "nothing for over") || strings.Contains(face, "no event yet") {
			t.Errorf("%s wears the ring:\n%s", id, face)
		}
	}
}

// The ring reads the card's own work too: a running card whose newest
// event is old but whose session wrote a fresh timeline step wears no
// ring, and one whose newest step is old wears the ring for the step's
// age, not the event's.
func TestCardFaceStaleRingReadsTheTimeline(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	old := time.Now().Add(-45 * time.Minute)
	r.tasks.set("",
		factoryd.Task{ID: "t-live", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "live session", CreatedAt: old},
		factoryd.Task{ID: "t-dead", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "dead workflow", CreatedAt: old})
	for _, id := range []string{"t-live", "t-dead"} {
		if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: id, Kind: factoryd.EventCardRetried,
			Actor: "sebastian", Via: factoryd.ViaButton, At: old}); err != nil {
			t.Fatal(err)
		}
	}
	step := func(at time.Time) []byte {
		line, err := json.Marshal(map[string]any{"step": "session_start", "attempt": 1, "time": at.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			t.Fatal(err)
		}
		return append(line, '\n')
	}
	r.objects["sessions/t-live/timeline.jsonl"] = step(time.Now().Add(-2 * time.Minute))
	r.objects["sessions/t-dead/timeline.jsonl"] = step(time.Now().Add(-40 * time.Minute))
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-live"); strings.Contains(face, "stale") || strings.Contains(face, "nothing for over") {
		t.Errorf("a card whose session writes steps wears the ring:\n%s", face)
	}
	face := faceOf(t, body, "t-dead")
	if !strings.HasPrefix(face, `<article class="card face stale" data-card="t-dead" `) {
		t.Errorf("the quiet card wears no ring:\n%s", face)
	}
	if want := "nothing for over 40 min"; !strings.Contains(drawer(t, r, ck, "t-dead", "overview"), want) {
		t.Errorf("the opened card's words are not the step's age:\n%s", drawer(t, r, ck, "t-dead", "overview"))
	}
}

// A waiting card's small card asks the question it waits on, as a
// sentence, never its newest event: in Needs you and in the opened card.
func TestCardFaceWaitingReason(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set(factoryd.AgentAsks+"use a replica or a shard?\nWhy: the reads grow",
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Objective: "add the feed", IssueNumber: 12})
	r.openCases = map[string][]factoryd.Case{"t-e": {{ID: 1, TaskID: "t-e", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman, Problem: "use a replica or a shard?"}}}
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-e", Kind: factoryd.EventCardRetried,
		Actor: "sebastian", Via: factoryd.ViaButton, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	five := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, five, "t-e"); !strings.Contains(face, `<strong class="title">Use a replica or a shard?</strong>`) || strings.Contains(face, "card_retried") {
		t.Errorf("Needs you: the waiting card's ask is not its question:\n%s", face)
	}
	if row := escalationOf(t, r, ck, "t-e"); !strings.Contains(row, `<p class="o-question">Use a replica or a shard?</p>`) || strings.Contains(row, "card_retried") {
		t.Errorf("the opened card's ask is not its question:\n%s", row)
	}
}

// The top bar (C23, C24): the six destinations sit in three groups, the
// open page's link alone carries aria-current, New task is the bar's one
// bordered word with the phone's "+" beside its word, and the menu sheet
// repeats every destination as a row with the logout last, so a narrow
// bar never hides a word behind a second press.
func TestTopBarSparseNav(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	get := func(target string) string {
		rec := r.do(httptest.NewRequest(http.MethodGet, target, nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	board := get("/")
	for _, want := range []string{
		`class="ng ng-work"`, `class="ng ng-ref"`, `class="ng ng-gov"`,
		`<a class="act" href="/tasks/new"><span class="w">New task</span><span class="p">+</span></a>`,
		`<details class="menu">`, `<summary>`,
		`<a class="s-work" href="/tasks/new">New task</a>`,
		`<a class="s-ref" href="/costs">Costs</a>`,
		`<a class="s-gov" href="/settings">Settings</a>`,
		`<form method="post" action="/logout" class="logout s-out">`,
	} {
		if !strings.Contains(board, want) {
			t.Errorf("the board's bar lacks %s", want)
		}
	}
	if hdr := board[:strings.Index(board, "</header>")]; strings.Contains(hdr, ">Board<") || strings.Contains(hdr, ">Projects<") || strings.Contains(hdr, ">Alerts<") {
		t.Error("the board's bar still shows a dropped word")
	}
	if i, j := strings.Index(board, `class="ng ng-ref"`), strings.Index(board, `class="ng ng-gov"`); i > j {
		t.Error("the reference group sits after governance")
	}
	for target, want := range map[string][2]string{
		"/settings":  {`<a href="/settings" aria-current="page">Settings</a>`, `href="/" aria-current="page"`},
		"/tasks/new": {`<a class="act" href="/tasks/new" aria-current="page">`, `href="/" aria-current="page"`},
		"/costs":     {`<a href="/costs" aria-current="page">Costs</a>`, `href="/" aria-current="page"`},
		"/policy":    {`<a href="/policy" aria-current="page">Policy</a>`, `href="/" aria-current="page"`},
		"/resources": {`<a href="/resources" aria-current="page">Resources</a>`, `href="/" aria-current="page"`},
	} {
		body := get(target)
		if !strings.Contains(body, want[0]) {
			t.Errorf("GET %s lacks %s", target, want[0])
		}
		if strings.Contains(body, want[1]) {
			t.Errorf("GET %s marks %s current", target, want[1])
		}
	}
}

// <html> carries the dark theme, the header the theme selector (C15) with
// the moon pressed, the head applies the stored theme before the
// stylesheet with a script that is not deferred, so a light page never
// paints dark first, and the selector's script and the light theme's
// tokens are served.
func TestThemeOnHTMLAndToggleServed(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<html lang="en" data-theme="dark">`, `<div id="theme-switch" class="theme-switch" role="group" aria-label="Theme">`,
		`<button type="button" data-theme-set="dark" aria-pressed="true" aria-label="Dark theme"`, `<button type="button" data-theme-set="light" aria-pressed="false" aria-label="Light theme"`,
		"<script src=\"/static/theme.js\"></script>\n<link rel=\"stylesheet\" href=\"/static/console.css\">",
		`<script src="/static/console.js" defer></script>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	for path, wants := range map[string][]string{
		"/static/theme.js":   {`document.documentElement.setAttribute("data-theme", localStorage.getItem("theme") === "light" ? "light" : "dark")`},
		"/static/console.js": {`localStorage.setItem("theme", theme)`, `document.documentElement.setAttribute("data-theme", theme)`},
	} {
		js := r.do(httptest.NewRequest(http.MethodGet, path, nil))
		for _, want := range wants {
			if js.Code != http.StatusOK || !strings.Contains(js.Body.String(), want) {
				t.Errorf("%s = %d, lacks %s", path, js.Code, want)
			}
		}
	}
	if css := r.do(httptest.NewRequest(http.MethodGet, "/static/console.css", nil)).Body.String(); !strings.Contains(css, "[data-theme=light] {") {
		t.Error("console.css lacks the light theme")
	}
}

// The opened card's Edit the brief: a writer's press answers 303, stores the why
// line and the summary with one row and clears the board's mark of an
// unchecked brief once the whole brief passes; a title as it stands edits
// the issue's body alone, which opens with the why line and the summary,
// and an edit that changes nothing answers 400. A brief that
// CheckEdit refuses answers 400 with its refusal and stores nothing, and
// a non-writer gets 403.
func TestBriefPost(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "queued", Repository: "sf/code", Objective: "Fix the cache. Then the tests.",
		IssueNumber: 1, BriefUnchecked: true})
	const mark = `<span class="it unchecked">brief not checked</span>`
	if face := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-1"); !strings.Contains(face, mark) {
		t.Fatalf("an unchecked brief shows no mark:\n%s", face)
	}
	brief := func(why, summary string) *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-1/resolve", url.Values{"csrf": {s.csrf}, "action": {"edit_brief"}, "edit_brief.title": {"Fix the cache. Then the tests."},
			"edit_brief.why": {why}, "edit_brief.summary": {summary}}), ck)
	}
	stored := func() factoryd.Task {
		k, _, _ := r.tasks.Task(t.Context(), "t-1")
		return k
	}
	before := ev.count()
	if rec := brief("So the board loads at once.", "The cache keeps its entries. A test reads them back."); rec.Code != http.StatusSeeOther || ev.count()-before != 1 {
		t.Fatalf("a brief = %d with %d rows, want 303 with one: %s", rec.Code, ev.count()-before, rec.Body)
	}
	if k := stored(); k.Why != "So the board loads at once." || k.Summary != "The cache keeps its entries. A test reads them back." || k.BriefUnchecked {
		t.Fatalf("stored %q, %q, unchecked %v", k.Why, k.Summary, k.BriefUnchecked)
	}
	mirror := `/api/v1/repos/sf/code/issues/1 {"body":"Why: So the board loads at once.\n\nSummary: The cache keeps its entries. A test reads them back.\n\n---\n\nIt drops entries."}`
	if !slices.Equal(r.fg.patched, []string{mirror}) {
		t.Fatalf("a title as it stands patched %q, want the issue's body alone, opening with the why line and the summary", r.fg.patched)
	}
	if face := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-1"); strings.Contains(face, mark) {
		t.Fatalf("a checked brief still shows the mark:\n%s", face)
	}
	if rec := brief("So the board loads at once.", "The cache keeps its entries. A test reads them back."); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "The edit changes nothing") {
		t.Fatalf("an edit that changes nothing = %d: %s", rec.Code, rec.Body)
	}
	before = ev.count()
	rec := brief("So a plan closes; the waiver goes.", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "The why line holds a semicolon. Write two sentences.") || ev.count() != before {
		t.Fatalf("a refused brief = %d with %d rows: %s", rec.Code, ev.count()-before, rec.Body)
	}
	if k := stored(); k.Why != "So the board loads at once." {
		t.Fatalf("a refused brief stored the why line %q", k.Why)
	}
	r.repoWriters["sf/code"] = nil
	r.forgetWriters()
	if rec := brief("So it is mine.", ""); rec.Code != http.StatusForbidden || ev.count() != before {
		t.Fatalf("a non-writer's brief = %d with %d rows, want 403 and none", rec.Code, ev.count()-before)
	}
}

// A card in Needs you whose brief was filed unchecked says so in its
// small card's band.
func TestCaseFaceMarksAnUncheckedBrief(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	ck, _ := r.loggedIn(t)
	const mark = `<span class="unchecked">brief not checked</span>`
	if c := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-e"); strings.Contains(c, mark) {
		t.Fatalf("a checked brief's case wears the mark:\n%s", c)
	}
	r.tasks.mu.Lock()
	r.tasks.tasks[0].BriefUnchecked = true
	r.tasks.mu.Unlock()
	if c := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-e"); !strings.Contains(c, `<span class="ref">#13</span> <span class="unchecked">brief not checked</span>`) {
		t.Fatalf("an unchecked brief's case lacks the mark in its band:\n%s", c)
	}
}

// The accent: each of the eight names answers 303 and lands on the card
// with one row; another name answers 400 and leaves it; none (the empty
// value) clears it; a non-writer gets 403.
func TestAccentPost(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work", IssueNumber: 1})
	stored := func() string {
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		return r.tasks.tasks[0].Accent
	}
	accent := func(value string) int {
		return r.do(post("/tasks/t-1/resolve", url.Values{"csrf": {s.csrf}, "action": {"accent"}, "accent.accent": {value}}), ck).Code
	}
	for _, name := range []string{"red", "orange", "amber", "green", "teal", "blue", "violet", "pink"} {
		before := ev.count()
		if code := accent(name); code != http.StatusSeeOther || stored() != name || ev.count()-before != 1 {
			t.Fatalf("accent %s = %d, stored %q, %d rows; want 303, stored, one row", name, code, stored(), ev.count()-before)
		}
	}
	if code := accent("purple"); code != http.StatusBadRequest || stored() != "pink" {
		t.Fatalf("accent purple = %d, stored %q; want 400 and pink kept", code, stored())
	}
	drawer := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/cards/t-1/drawer?tab=overview", nil), ck).Body.String()
	}
	if d := drawer(); !strings.Contains(d, `<option value="">none</option>`) || !strings.Contains(d, `<option value="pink" selected>pink</option>`) {
		t.Fatalf("the accent select lacks none or the chosen pink:\n%s", d)
	}
	if code := accent(""); code != http.StatusSeeOther || stored() != "" {
		t.Fatalf("accent none = %d, stored %q; want 303 and cleared", code, stored())
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-1"); strings.Contains(face, "accent") || strings.Contains(face, "edge-") ||
		!strings.Contains(drawer(), `<option value="" selected>none</option>`) {
		t.Fatalf("a cleared accent still shows:\n%s", face)
	}
	r.repoWriters["sf/code"] = nil
	r.forgetWriters()
	if code := accent("blue"); code != http.StatusForbidden || stored() != "" {
		t.Fatalf("a non-writer's accent = %d, stored %q; want 403 and nothing", code, stored())
	}
}

// On a filtered board every answer carries the filter back: retry, the
// plan's changes, the brief and the accent; the poll asks for the same board,
// and a browser's POST is sent back to it.
func TestFilteredBoardFormsCarryTheFilter(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	tasks := append(planTasks(), failedCard(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-ask", Kind: "agent", Status: "waiting-human",
		Repository: "linkshort/linkshort", IssueNumber: 30, Objective: "asks"}, Escalation: "The agent asks: which region?"})
	for i := range tasks {
		tasks[i].Accent = "teal"
	}
	r.tasks.tasks = tasks
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-spec2": {planRevision("t-spec2", 1, "sha-1")}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 20, Kind: factoryd.ApprovalPlan, TaskID: "t-spec2",
		Project: "linkshort/linkshort", Status: "pending", Spec: json.RawMessage(`{"revision":1,"sha256":"sha-1"}`)})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	filter := `<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort">`
	htmx := func(action string) string {
		return `action="` + action + `" hx-post="` + action + `"`
	}
	if want := `data-query="?project=linkshort" hx-get="/board?project=linkshort" hx-trigger="board-refresh from:body" hx-swap="outerHTML">`; !strings.Contains(body, want) {
		t.Errorf("the filtered board lacks %s", want)
	}
	for id, wants := range map[string][]string{
		"t-c7":    {htmx("/tasks/t-c7/resolve") + ` class="answer" data-answer="retry">` + filter},
		"t-spec2": {htmx("/tasks/t-spec2/plan/changes") + ` class="answer" data-answer="revise" data-to="running">` + filter},
		"t-ask":   {htmx("/tasks/t-ask/resolve") + ` class="answer" data-answer="resume" data-to="running">` + filter},
		"t-c2": {
			htmx("/tasks/t-c2/resolve") + ` class="answer" data-answer="edit_brief">` + filter,
			htmx("/tasks/t-c2/resolve") + ` class="answer" data-answer="accent">` + filter,
		},
	} {
		opened := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?project=linkshort", nil), ck).Body.String()
		for _, want := range wants {
			if !strings.Contains(opened, want) {
				t.Errorf("the filtered opened card of %s lacks %s:\n%s", id, want, opened)
			}
		}
	}
	for path, form := range map[string]url.Values{
		"/tasks/t-c2/resolve":         {"action": {"edit_brief"}, "edit_brief.why": {"So links stay short."}, "edit_brief.summary": {"The links"}},
		"/tasks/t-c7/resolve":         {"action": {"accent"}, "accent.accent": {"blue"}},
		"/tasks/t-spec2/plan/changes": {"text": {"Split the API card."}},
		"/tasks/t-ask/answer":         {"answer": {"europe"}},
	} {
		form.Set("csrf", s.csrf)
		form.Set("project", "linkshort")
		if rec := r.do(post(path, form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" {
			t.Errorf("%s = %d to %q, want 303 to the filtered board: %s", path, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
	}
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	if got := r.tasks.tasks[1]; got.ID != "t-c2" || got.Summary != "The links" {
		t.Errorf("t-c2's summary = %q, want The links", got.Summary)
	}
	if i := slices.IndexFunc(r.tasks.tasks, func(bt factoryd.BoardTask) bool { return bt.ID == "t-c7" }); r.tasks.tasks[i].Accent != "blue" {
		t.Errorf("t-c7's accent = %q, want blue, not the filter's teal", r.tasks.tasks[i].Accent)
	}
}

// escalationOf is the ask block of card id's opened card.
func escalationOf(t *testing.T, r *rig, ck *http.Cookie, id string) string {
	t.Helper()
	return askBlockOf(t, drawer(t, r, ck, id, "overview"))
}

// askBlockOf is the ask block of the opened card in drawer body.
func askBlockOf(t *testing.T, body string) string {
	t.Helper()
	_, ask, ok := strings.Cut(body, `<section class="o-ask"`)
	if !ok {
		t.Fatalf("the opened card holds no ask:\n%s", body)
	}
	ask, _, _ = strings.Cut(ask, "</section>")
	return ask
}

// otherActionsOf is the fold of other actions of the opened card in drawer
// body.
func otherActionsOf(t *testing.T, body string) string {
	t.Helper()
	_, more, ok := strings.Cut(body, `<details class="fold" data-sec="more">`)
	if !ok {
		t.Fatalf("the opened card holds no other actions:\n%s", body)
	}
	more, _, _ = strings.Cut(more, "</details>")
	return more
}

// A needs-you card with its agent's choices names how many answers it
// holds, and its opened card offers one answer per choice, each a form to
// the resolve endpoint with the case and the session's token; a card
// without choices offers none of the agent's, and the human's own words.
// A click is one answer: the choice on the issue as the human, one row via
// button, a 303 to the filtered board.
func TestNeedsYouCardAnswersWithAChoice(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-ch", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "add the feed"},
			Escalation: "The agent asks: a replica or a shard?\nWhy: the reads outgrow one node", Choices: []string{"a replica", "a shard", "both"}},
		{Task: factoryd.Task{ID: "t-plain", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14, Objective: "pick a port"},
			Escalation: "The agent asks: which port?"},
	}
	ck, s := r.loggedIn(t)
	board := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if face := faceOf(t, board, "t-ch"); !strings.Contains(face, `<strong class="title">A replica or a shard?</strong>`) ||
		!strings.Contains(face, `<a class="others" href="#card=t-ch">4 answers ›</a>`) || strings.Contains(face, "<form") {
		t.Errorf("the small card of t-ch =\n%s\nwant its ask and the four answers on its page behind the open", face)
	}
	esc := escalationOf(t, r, ck, "t-ch")
	if !strings.Contains(esc, `<p class="o-question">A replica or a shard?</p>`) {
		t.Errorf("the escalation of t-ch lacks its question:\n%s", esc)
	}
	for _, choice := range []string{"a replica", "a shard", "both"} {
		want := `<form method="post" action="/tasks/t-ch/resolve" hx-post="/tasks/t-ch/resolve" class="answer" data-answer="say:` + choice + `" data-to="running">` +
			`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="case_id" value="1"><input type="hidden" name="action" value="say:` + choice + `">` +
			"\n" + `<button type="submit" class="pick">` + choice + `</button>`
		if !strings.Contains(esc, want) {
			t.Errorf("the escalation of t-ch lacks the answer %q as its own send:\n%s", choice, esc)
		}
	}
	if n := strings.Count(esc, `name="action" value="say:`); n != 3 {
		t.Errorf("t-ch offers %d answers of the agent, want 3", n)
	}
	if plain := escalationOf(t, r, ck, "t-plain"); strings.Contains(plain, `value="say:`) ||
		!strings.Contains(plain, `data-press="continue/own_words">Answer in my words</button>`) ||
		!strings.Contains(plain, `name="continue/own_words.note" rows="2" maxlength="4000" placeholder="The answer the agent needs" required hx-preserve>`) {
		t.Errorf("the escalation of t-plain =\n%s\nwant no answer of the agent, and the human's own words", plain)
	}

	before := ev.count()
	rec := r.do(post("/tasks/t-ch/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "q": {"shard"},
		"case_id": {"1"}, "action": {"say:a shard"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code&q=shard" {
		t.Fatalf("the choice = %d to %q, want 303 to the filtered board", rec.Code, rec.Header().Get("Location"))
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "a shard" {
		t.Fatalf("the comment = %q, want the choice", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-ch",
		detail: map[string]any{"action": factoryd.ActContinue, "answer": "a shard", "case_id": int64(1), "comment": int64(1)}})
}

// A shaped question's small card holds the case's ask and its suggested
// answer, never the question's raw text; the opened card holds the case's
// answers, and with no case no ask at all. A card that holds answers its
// run has not taken warns above them.
func TestNeedsYouCardRendersTheShapedQuestion(t *testing.T) {
	question := &factoryd.HumanQuestion{
		Ask:            "The call cap is reached: 4 calls of 4, 1.50 EUR of 6.00 EUR left. Which way on?",
		Why:            "the session cap is reached: every finding sent to a human returns unacted",
		Info:           "A grant buys one more session on this card. Park holds the card for a person to work with.",
		Recommendation: "Grant one session",
		Source:         "the factory question",
		Choices: []factoryd.QuestionChoice{
			{Value: "grant", Label: "Grant one session"},
			{Value: "park", Label: "Park it"},
			{Value: "close", Label: "Close the card"},
		},
	}
	page := func(bt factoryd.BoardTask, url string, held bool) string {
		r, _ := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{bt}
		r.openCases = map[string][]factoryd.Case{}
		if held {
			r.openCases = map[string][]factoryd.Case{"t-q": {{ID: 1, TaskID: "t-q", Kind: factoryd.CaseSessionCap, Status: factoryd.CaseAtHuman, Problem: question.Ask,
				Facts: roomyFacts(nil)}}}
			r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"recommendation": question.Recommendation, "recommended_by": "the factory question",
				"preselected": map[string]any{"action": factoryd.ActExtend}}}}
		}
		ck, _ := r.loggedIn(t)
		return r.do(httptest.NewRequest(http.MethodGet, url, nil), ck).Body.String()
	}
	bt := factoryd.BoardTask{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
		Escalation: "the session cap is reached: every finding sent to a human returns unacted", Question: question}
	// the small card asks the register's ask and suggests its pick, which
	// asks how many sessions, so it opens the card rather than sends
	face := faceOf(t, page(bt, "/?project=ai-software-factory-code", true), "t-q")
	for _, want := range []string{
		`<strong class="title">The card used all 3 sessions. Allow more, or end the card?</strong>`,
		`<a class="pick-link" href="#card=t-q:press=extend">Allow more sessions</a>`,
		`<span class="by">factory question&#39;s pick</span><span class="others">or <a href="#card=t-q">2 others ›</a></span>`,
	} {
		if !strings.Contains(face, want) {
			t.Errorf("the small card lacks %q:\n%s", want, face)
		}
	}
	for _, bulky := range []string{"<form", `<pre class="reason detail">`, "every finding sent to a human returns unacted", "Grant one session"} {
		if strings.Contains(face, bulky) {
			t.Errorf("the small card carries %q, which belongs to the opened card:\n%s", bulky, face)
		}
	}
	esc := askBlockOf(t, page(bt, "/cards/t-q/drawer?tab=overview", true))
	for _, want := range []string{
		`<p class="o-question">The card used all 3 <span class="term">sessions</span>. Allow more, or end the card?</p>`,
		`class="answer suggested" data-answer="extend" data-to="running">`,
		`<p class="o-by">suggested by the factory question: <span class="quote">Grant one session</span></p>`,
		`data-press="extend">Allow more sessions</button>`,
		`data-press="note">Not now</button>`,
		`data-press="close">End the card</button>`,
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("the opened card lacks %q:\n%s", want, esc)
		}
	}
	if strings.Contains(esc, `name="answer" value=`) || strings.Contains(esc, `name="value"`) || strings.Contains(esc, "Park it") {
		t.Errorf("the case's card offers the old choices as buttons:\n%s", esc)
	}
	// a card no case holds asks nothing on its opened card: the factory
	// writes no ask without its case, so a stored question there is no ask
	drawer := page(bt, "/cards/t-q/drawer?tab=overview", false)
	if strings.Contains(drawer, `class="o-ask"`) || strings.Contains(drawer, "/tasks/t-q/answer") || strings.Contains(drawer, "Park it") {
		t.Errorf("the opened card of a card no case holds asks:\n%s", drawer)
	}

	bt.AnswersPending = 2
	held := page(bt, "/cards/t-q/drawer?tab=overview", true)
	if !strings.Contains(held, `<p class="o-sent liveness" role="status">This card holds answers its run has not taken.`) {
		t.Errorf("the card with pending answers carries no liveness warning:\n%s", held)
	}
	if esc := askBlockOf(t, held); !strings.Contains(esc, `name="action" value="note"`) {
		t.Errorf("the card with pending answers lost its answers:\n%s", esc)
	}
}

// A human card's opened card asks its question with its choices as the
// answers, and its full brief holds what each answer does, rendered as
// markup.
func TestAHumanCardsOpenedCardRendersItsQuestionsParts(t *testing.T) {
	r, _ := newRigWithEvents(t)
	q := factoryd.HumanQuestion{Ask: "Renew the certificate?", Why: "The certificate expires on Friday.",
		Info:    "## What each answer does\n- Done closes the card\n\n```sh\nrenew --now\n```",
		Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerDone, Label: "I did it"}, {Value: factoryd.AnswerPark, Label: "Not now"}}}
	card := factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 16, Objective: "Renew the certificate"}
	r.tasks.tasks = []factoryd.BoardTask{{Task: card, Escalation: q.Ask, Question: &q}}
	detail := factoryd.HumanAsk{Q: q, By: "sebastian", Asker: "agent", Via: factoryd.ViaChat}.Event(card).Detail
	seatAsk(r, "t-h", factoryd.CaseHumanCard, detail)
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: detail}}
	ck, _ := r.loggedIn(t)
	page := drawer(t, r, ck, "t-h", "overview")
	ask := askBlockOf(t, page)
	for _, want := range []string{
		`<p class="o-question">Renew the certificate?</p>`,
		`<p class="o-askwhy">The certificate expires on Friday.</p>`,
		`<button type="submit" class="pick">I did it</button>`,
		`<button type="submit" class="pick">Not now</button>`,
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the human card's ask lacks %q:\n%s", want, ask)
		}
	}
	for _, want := range []string{
		`<h4>What the factory knows</h4><div class="info"><h2>What each answer does</h2>`,
		`<li>Done closes the card</li>`,
		`<pre><code class="language-sh">renew --now`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the human card's full brief lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "&lt;h2&gt;") {
		t.Errorf("the opened card renders the markup as text:\n%s", page)
	}
}

// The per-kind board fixtures render their asks on the opened card: an
// asking kind's face, a money kind's case face and a plan's question each
// carry the register's ask and why and the suggested answer with its
// writer and grounds, the info in the full brief, and answers that each
// send alone: one Send, at most one note, no fund beside the answer.
func TestBoardFixturesRenderTheirAsks(t *testing.T) {
	for _, tc := range []struct {
		name, ask, why, suggested, by, grounds string
		info                                   bool
	}{
		{"board-agent-escalate.json", "The shared cache, or a shard per feed?",
			"The agent stopped because it needs a decision only a human can make.", "continue/own_words", "the factory agent", "Shard the feed cache", true},
		{"board-session-cap.json", `The card used all 3 <span class="term">sessions</span>. Allow more, or end the card?`,
			"A card runs only the sessions it was given, and this one has used all 3.", "extend", "the factory agent", "Extend by one session", true},
		{"board-plan-question.json", "Which storage backs the short links?",
			"The planner needs your answer before it can finish the plan.", "", "the planner", "a store the operator already runs asks no new standing cost", false},
	} {
		r := newRig(t)
		fx := seatBoardFixture(t, r, tc.name)
		ck, _ := r.loggedIn(t)
		page := drawer(t, r, ck, fx.Card.TaskID, "overview")
		ask := askBlockOf(t, page)
		for _, want := range []string{
			`<p class="o-question">` + tc.ask + `</p>`,
			`<p class="o-askwhy">` + tc.why + `</p>`,
			`<p class="o-by">suggested by ` + tc.by + `: <span class="quote">` + tc.grounds + `</span></p>`,
		} {
			if !strings.Contains(ask, want) {
				t.Errorf("%s: the ask lacks %q:\n%s", tc.name, want, ask)
			}
		}
		if tc.suggested != "" && !strings.Contains(ask, `class="answer suggested" data-answer="`+tc.suggested+`"`) {
			t.Errorf("%s: the ask does not lead with %s:\n%s", tc.name, tc.suggested, ask)
		}
		if strings.Contains(ask, "disabled") {
			t.Errorf("%s: the ask holds an answer that cannot be pressed:\n%s", tc.name, ask)
		}
		if want := `<h4>What the factory knows</h4><div class="info"><p>` + boardFixtureInfo + `</p>`; tc.info && !strings.Contains(page, want) {
			t.Errorf("%s: the full brief lacks %q:\n%s", tc.name, want, page)
		}
		forms := strings.Split(ask, "<form ")[1:]
		if len(forms) < 2 {
			t.Errorf("%s: the ask holds %d answers, want two or more:\n%s", tc.name, len(forms), ask)
		}
		for _, form := range forms {
			form, _, _ = strings.Cut(form, "</form>")
			if strings.Count(form, `type="submit"`) != 1 || strings.Count(form, "<textarea") > 1 || strings.Contains(form, "fund_eur") {
				t.Errorf("%s: an answer does not send alone:\n%s", tc.name, form)
			}
		}
	}
}

// A card that asks closes without a browser confirm: its small card holds
// no close, and End the card on the opened card is an answer of its own,
// whose Send closes the card with the note, if any.
func TestAskingCardClosesOnANoteWithoutAConfirm(t *testing.T) {
	r, _ := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-idle", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 41, Objective: "Pick the next work."},
			Escalation: "The board is idle and main is green. What should I start or improve next?"},
		{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 42, Objective: "the feed"},
			Escalation: "the session cap is reached",
			Question: &factoryd.HumanQuestion{Ask: "The call cap is reached. Which way on?", Why: "the session cap is reached",
				Choices: []factoryd.QuestionChoice{{Value: "park", Label: "Park it"}, {Value: "close", Label: "End the card"}}}},
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	for _, id := range []string{"t-idle", "t-q"} {
		if face := faceOf(t, body, id); strings.Contains(face, "hx-confirm") || strings.Contains(face, "/close") {
			t.Errorf("the small card of %s carries a close:\n%s", id, face)
		}
	}
	if q := escalationOf(t, r, ck, "t-q"); !strings.Contains(q, `<input type="hidden" name="action" value="close">`+"\n"+
		`<button type="button" class="pick" aria-expanded="false" aria-controls="f-t-q-1-close-note-set" data-press="close">End the card</button>`) {
		t.Errorf("the question's End the card is no answer of its own:\n%s", q)
	}
	for _, id := range []string{"t-idle", "t-q"} {
		drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
		if strings.Contains(drawer, "hx-confirm") {
			t.Errorf("the drawer of %s asks the browser to confirm a close:\n%s", id, drawer)
		}
	}
}

// A click anywhere on a needs-you card opens its drawer, as on every
// other card: the board's click handler reads
// e.target.closest("#board [data-card]"), so the small card's article
// carries the data-card and its band, its paragraph and its answer row are
// its children, with the link that opens it for a keyboard.
func TestNeedsYouCardOpensItsDrawerFromAnywhere(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
		Escalation: "the session cap is reached: every finding sent to a human returns unacted",
		Question: &factoryd.HumanQuestion{Ask: "The call cap is reached: which way on?",
			Choices: []factoryd.QuestionChoice{{Value: "park", Label: "Park it"}, {Value: "close", Label: "End the card"}}}}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	face := faceOf(t, body, "t-q")
	if !strings.HasPrefix(face, `<article class="card face need`) {
		t.Fatalf("the needs-you card is no small card with a data-card:\n%s", face)
	}
	open := strings.Index(face, `<a class="open" href="#card=t-q"><span class="visually-hidden">Open #15</span></a>`)
	band, para := strings.Index(face, `<p class="band">`), strings.Index(face, `<p class="para">`)
	if strings.Count(face, "<article") != 1 || open < 0 || band < open || para < band {
		t.Errorf("the open link, the band and the paragraph do not sit in the one article:\n%s", face)
	}
}

// The shaped buttons route by value: a park or keep waiting settles the
// card's case, its choice riding as the settle's park arg and the label
// posted as the answer on the issue, and a typed answer that repeats a
// button's words takes that button's route.
func TestNeedsYouCardRoutesTheValue(t *testing.T) {
	question := func() *factoryd.HumanQuestion {
		return &factoryd.HumanQuestion{
			Ask:  "The card's spend is above the policy's bound: 0.90 EUR of the 6.00 EUR budget. Which way on?",
			Why:  "the card's spend is above the policy's bound: sessions bought on now would overrun the card's budget",
			Info: "Fund raises the card's budget. Park holds it. Keep waiting holds it and waits for the budget's next window.",
			Choices: []factoryd.QuestionChoice{
				{Value: "fund", Label: "Fund the card"},
				{Value: "park", Label: "Park it"},
				{Value: "keep_waiting", Label: "Wait for the window"},
			},
		}
	}
	setup := func(t *testing.T) (*rig, *fakeEvents, *http.Cookie, *session) {
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
			Escalation: "the card's spend is above the policy's bound", Question: question()}}
		ck, s := r.loggedIn(t)
		return r, ev, ck, s
	}
	before := 0
	r, ev, ck, s := setup(t)
	before = ev.count()
	rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"Park it"}, "value": {"park"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the park = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "Park it" {
		t.Fatalf("the park posted %q, want the label as the answer", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-q",
		detail: map[string]any{"action": factoryd.ActSettle, "answer": "Park it", "case_id": int64(1), "comment": int64(1)}})
	got, _ := actedOnce(t, r)
	equal(t, "the park", got.Res, factoryd.Resolution{Action: factoryd.ActSettle, Args: map[string]any{"park": factoryd.AnswerPark}, Note: "Park it"})

	r, ev, ck, s = setup(t)
	before = ev.count()
	if rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"Wait for the window"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the typed label = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "Wait for the window" {
		t.Fatalf("the typed label posted %q, want the label as the answer", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-q",
		detail: map[string]any{"action": factoryd.ActSettle, "answer": "Wait for the window", "case_id": int64(1), "comment": int64(1)}})
	got, _ = actedOnce(t, r)
	equal(t, "the typed label", got.Res, factoryd.Resolution{Action: factoryd.ActSettle, Args: map[string]any{"park": factoryd.AnswerKeepWaiting}, Note: "Wait for the window"})

	r, ev, ck, s = setup(t)
	before = ev.count()
	if rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"hold the card until the roadmap names it"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the typed answer = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "hold the card until the roadmap names it" {
		t.Fatalf("the typed answer posted %q", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-q",
		detail: map[string]any{"action": factoryd.ActContinue, "answer": "hold the card until the roadmap names it", "case_id": int64(1), "comment": int64(1)}})

	// past a raised cap the fund answer posts like any answer, the label
	// retrying the card; the card holds no budget refusal here
	r, ev, ck, s = setup(t)
	before = ev.count()
	if rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"Fund one more session"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the fund answer = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "Fund one more session" {
		t.Fatalf("the fund answer posted %q", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-q",
		detail: map[string]any{"action": factoryd.ActContinue, "answer": "Fund one more session", "case_id": int64(1), "comment": int64(1)}})
}

// The one-click buttons of a shaped question post their value alone, no
// answer field, so doAnswerValue takes the choice's own label as the
// answer text: park and keep waiting settle the case, their label the
// answer on the issue. Fund, a value no answer takes any more, is refused
// with the card's state and the press that fits, and posts nothing.
func TestNeedsYouCardAnswersAChoiceWithoutText(t *testing.T) {
	question := func() *factoryd.HumanQuestion {
		return &factoryd.HumanQuestion{
			Ask:  "The card's spend is above the policy's bound: 0.90 EUR of the 6.00 EUR budget. Which way on?",
			Why:  "the card's spend is above the policy's bound",
			Info: "Fund raises the card's budget. Park holds it. Keep waiting holds it and waits for the budget's next window.",
			Choices: []factoryd.QuestionChoice{
				{Value: "fund", Label: "Fund the card"},
				{Value: "park", Label: "Park it"},
				{Value: "keep_waiting", Label: "Wait for the window"},
			},
		}
	}
	setup := func(t *testing.T) (*rig, *fakeEvents, *http.Cookie, *session) {
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
			Escalation: "the card's spend is above the policy's bound", Question: question()}}
		ck, s := r.loggedIn(t)
		return r, ev, ck, s
	}
	r, ev, ck, s := setup(t)
	before := ev.count()
	rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "value": {"fund"}}), ck)
	if want := "The question of card #15 no longer offers the answer “Fund the card”. Open the card and press one of the answers it shows."; rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), want) || r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"] != "" || ev.count() != before {
		t.Fatalf("the fund click = %d %q with %d rows, want 409 with %q and nothing posted", rec.Code, rec.Body.String(), ev.count()-before, want)
	}
	for _, tc := range []struct {
		value, label, action string
	}{
		{"park", "Park it", factoryd.ActSettle},
		{"keep_waiting", "Wait for the window", factoryd.ActSettle},
	} {
		r, ev, ck, s := setup(t)
		before := ev.count()
		rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
			"value": {tc.value}}), ck)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("the %s click = %d %q, want 303", tc.value, rec.Code, rec.Body.String())
		}
		if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != tc.label {
			t.Fatalf("the %s click posted %q, want the label %q", tc.value, got, tc.label)
		}
		checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-q",
			detail: map[string]any{"action": tc.action, "answer": tc.label, "case_id": int64(1), "comment": int64(1)}})
	}
}

// A typed answer that matches none of a shaped question's choices is not
// an answer: its words go to the member's chat as a user row naming the
// card, a turn answers them there without the request, nothing is posted
// on the issue, and the member lands on the chat page.
func TestNeedsYouFreeAnswerGoesToTheChat(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?"},
		Question: &factoryd.HumanQuestion{Ask: "The session hit its cap. Which way on?", Choices: []factoryd.QuestionChoice{
			{Value: "fund", Label: "Fund one more session"}, {Value: "park", Label: "Park it"}}}}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("I read the card and answer with my own judgement.")}}
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"use your own judgement on the replica"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the typed answer = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != chatPath {
		t.Fatalf("the typed answer lands on %q, want the chat %q", got, chatPath)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "" {
		t.Fatalf("the typed answer posted %q, want no comment", got)
	}
	var msgs []factoryd.ChatMessage
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		chat.mu.Lock()
		msgs = slices.Clone(chat.msgs["sf/code "+s.user])
		chat.mu.Unlock()
		if len(msgs) >= 2 {
			break
		}
	}
	if len(msgs) < 2 {
		t.Fatalf("the chat holds %d rows, want the member's words and the turn's answer: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "About card #13 (t-e): use your own judgement on the replica" {
		t.Fatalf("the member's row = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" {
		t.Fatalf("the turn's row = %+v", msgs[1])
	}
}

// The same typed answer sent by htmx (the card's form) lands on the chat
// by HX-Redirect: htmx would swap a followed redirect's page into the
// board.
func TestNeedsYouFreeAnswerByHtmxLandsOnTheChat(t *testing.T) {
	r, _, g := newChatRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?"},
		Question: &factoryd.HumanQuestion{Ask: "The session hit its cap. Which way on?", Choices: []factoryd.QuestionChoice{
			{Value: "fund", Label: "Fund one more session"}, {Value: "park", Label: "Park it"}}}}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("I read the card and answer with my own judgement.")}}
	rec := r.do(htmxPost("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"use your own judgement on the replica"}}), ck)
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != chatPath || rec.Header().Get("Location") != "" || rec.Body.Len() != 0 {
		t.Fatalf("the typed answer by htmx = %d, HX-Redirect %q, Location %q, body %q; want 200 to the chat %q",
			rec.Code, rec.Header().Get("HX-Redirect"), rec.Header().Get("Location"), rec.Body.String(), chatPath)
	}
}

// The merge approval's small card stays as small as a question's: no
// part's later lines, no facts wall, no review's criterion. Its opened card
// asks the merge case: each answer at a line of effect and a send of its
// own, the note a field of the answers that need it, and the parts' whole
// text in the full brief.
func TestNeedsYouMergeFaceStaysSmall(t *testing.T) {
	r := newRig(t)
	long := "line one of the recommendation\nline two weighs the risk\nline three names the migration order"
	spec := fmt.Sprintf(`{"risk_class":"high","migration":false,"verdict_summary":"adds the column",`+
		`"review":{"verdict":"approve","findings":["0018_x.sql: add a default"],`+
		`"criteria":[{"criterion":"no schema migration","kind":"human","evidence":"the diff moves the column"}]},`+
		`"head_sha":"abc123","recommendation":%q,`+
		`"why_human":["first why","second why"]}`, long)
	ap := factoryd.ApprovalRequest{ID: 6, Kind: "merge", TaskID: "t-m2", Project: "sf/code", PRNumber: 12, Status: "pending", Spec: json.RawMessage(spec)}
	r.approvals.rows = append(r.approvals.rows, ap)
	kase, err := factoryd.RequestCase(ap)
	if err != nil {
		t.Fatal(err)
	}
	kase.ID = 90
	r.openCases = map[string][]factoryd.Case{"t-m2": {kase}}
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m2", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 12, PRNumber: 12, Objective: "Add the column"}})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	face := faceOf(t, body, "t-m2")
	// the bound holds on a small card carrying its open case: no part's
	// later lines, no facts wall, no review's criterion
	for _, tail := range []string{"line two weighs the risk", "line three names the migration order",
		"0018_x.sql: add a default", "yours to judge at this merge", "<textarea"} {
		if strings.Contains(face, tail) {
			t.Errorf("the merge face carries %q beyond its one line:\n%s", tail, face)
		}
	}
	esc := escalationOf(t, r, ck, "t-m2")
	for _, want := range []string{
		`<p class="o-question">Merge “Add the column”?</p>`,
		`<p class="o-by">suggested by the model review: <span class="quote">It approved the change with 1 finding.</span></p>` + "\n" +
			`<button type="submit" class="pick">Merge it</button> <span class="effect">Merges the pull request.</span>`,
		`data-press="deny_merge">Send it back</button> <span class="effect">Returns the pull request to its agent with your note.</span>`,
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("the merge ask lacks %q:\n%s", want, esc)
		}
	}
	for what, want := range map[string]int{`<form `: 3, `action="/tasks/t-m2/resolve"`: 3, `name="case_id" value="90"`: 3,
		`<textarea`: 2, `type="submit" class="send"`: 2, "fund": 0} {
		if got := strings.Count(esc, what); got != want {
			t.Errorf("the merge ask carries %s %d times, want %d:\n%s", what, got, want, esc)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m2/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{
		`<dt>Why a human</dt><dd>first why; second why</dd>`,
		`<dt>Model review</dt><dd>approve: adds the column<br>- 0018_x.sql: add a default</dd>`,
		`<dt>Agent's recommendation</dt><dd class="reason">` + long + `</dd>`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the merge card's full brief lacks %q", want)
		}
	}
	// a human criterion is a row of the plan's merge card, not a line of the review's
	if strings.Contains(drawer, "yours to judge") {
		t.Errorf("the merge drawer carries the review's human criterion line")
	}
}

func humanAsk(id string, issue int64, choices ...factoryd.QuestionChoice) factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: issue,
		Objective: "Which store?"},
		Escalation: "Which store keeps the snapshots?",
		Question: &factoryd.HumanQuestion{Ask: "Which store keeps the snapshots?", Why: "Two stores fit.", Info: "Postgres is shared, SQLite is local.",
			Recommendation: "Postgres", Source: "the agent", Choices: choices}}
}

// A human card's End the card says that the cards waiting for it ask
// first while a card waits for it, in a plan or outside one.
func TestAHumanCardsEndSaysItsWaitingCardsAsk(t *testing.T) {
	r := newRig(t)
	waiter := factoryd.BoardTask{Task: factoryd.Task{ID: "t-w", Kind: "agent", Status: "waiting-deps", Repository: "sf/code", IssueNumber: 52, Objective: "Ship the export"}}
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerDone, Label: "I did it"}), waiter}
	ck, _ := r.loggedIn(t)
	if body := drawer(t, r, ck, "t-hq", "overview"); !strings.Contains(body, "Ends the card and closes its issue. Nothing more runs.") {
		t.Errorf("with no card waiting, End the card lacks its plain line:\n%s", body)
	}
	r.tasks.mu.Lock()
	r.tasks.deps = map[string][]string{"t-w": {"t-hq"}}
	r.tasks.mu.Unlock()
	if body := drawer(t, r, ck, "t-hq", "overview"); !strings.Contains(body, "Ends the card and closes its issue. The cards that wait for it ask you first.") {
		t.Errorf("with #52 waiting, End the card does not say the waiting cards ask first:\n%s", body)
	}
}

// A human card whose stored ask reads as no question, and whose title
// makes none either, asks whether the work is done on its opened card; its
// small card leads with the card's title, so Needs you names the work.
func TestAFallbackAskLeadsTheSmallCardWithTheTitle(t *testing.T) {
	r := newRig(t)
	const title = "Board export: keep the nightly snapshots"
	hq := humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerDone, Label: "I did it"})
	hq.Objective, hq.Question.Ask, hq.Escalation = title+"\n\nWrite the board to a snapshot every night.", title, title
	r.tasks.tasks = []factoryd.BoardTask{hq}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if face := faceOf(t, body, "t-hq"); !strings.Contains(face, `<strong class="title">`+title+`</strong>`) || strings.Contains(face, "Is the work of this card done?") {
		t.Errorf("the small card does not lead with the card's title:\n%s", face)
	}
	if esc := askBlockOf(t, drawer(t, r, ck, "t-hq", "overview")); !strings.Contains(esc, `<p class="o-question">Is the work of this card done?</p>`) {
		t.Errorf("the opened card does not ask whether the work is done:\n%s", esc)
	}
}

// A human card shows the question a chat escalation files it from
// (values are the labels in the "say:" namespace): the whole ask once, as
// its small card's title; its opened card asks it with one answer per
// choice, the why under the ask, the asker's view in its story and the
// info in the full brief. An agent card's opened card asks the agent's
// question.
func TestAHumanCardShowsItsQuestion(t *testing.T) {
	r := newRig(t)
	const ask = "Which store keeps the snapshots of the nightly export once the shared Postgres runs out of disk?"
	hq := humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "say:Postgres", Label: "Postgres"}, factoryd.QuestionChoice{Value: "say:SQLite", Label: "SQLite"})
	hq.Objective, hq.Question.Ask = ask+"\n\nWhy: Two stores fit.", ask
	r.tasks.tasks = []factoryd.BoardTask{hq, agentEndsAsk()}
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"info": hq.Question.Info}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	drawer := drawer(t, r, ck, "t-hq", "overview")
	esc := askBlockOf(t, drawer)
	for _, want := range []string{
		`<p class="o-question">` + ask + `</p>`,
		`<p class="o-askwhy">Two stores fit.</p>`,
		`The agent&#39;s view: <span class="quote">Postgres</span>`,
		`action="/tasks/t-hq/answer"`,
		`<input type="hidden" name="value" value="say:Postgres">` + "\n" + `<button type="submit" class="pick">Postgres</button>`,
		`<input type="hidden" name="value" value="say:SQLite">` + "\n" + `<button type="submit" class="pick">SQLite</button>`,
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("the human card's ask lacks %q:\n%s", want, esc)
		}
	}
	if want := `<div class="info"><p>Postgres is shared, SQLite is local.</p>`; !strings.Contains(drawer, want) {
		t.Errorf("the human card's full brief lacks %q:\n%s", want, drawer)
	}
	// the ask is the card's title: the small card shows it once, as its title
	if face := faceOf(t, body, "t-hq"); strings.Count(face, ask) != 1 || !strings.Contains(face, `<strong class="title">`+ask+`</strong>`) {
		t.Errorf("the human card shows its ask other than once, as its title:\n%s", face)
	}
	if agent := escalationOf(t, r, ck, "t-a"); !strings.Contains(agent, `<p class="o-question">Use a replica?</p>`) {
		t.Errorf("the agent card lost its ask:\n%s", agent)
	}
}

// A human card's go and done choices take it off the board and post to
// the answer route; park keeps it.
func TestAHumanCardChoicesLeaveTheBoardOrStay(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "go", Label: "Have the factory do it"},
		factoryd.QuestionChoice{Value: "park", Label: "Wait for it"}, factoryd.QuestionChoice{Value: factoryd.AnswerDone, Label: "I did it"})}
	ck, _ := r.loggedIn(t)
	esc := escalationOf(t, r, ck, "t-hq")
	for value, to := range map[string]string{"go": ` data-to="off"`, factoryd.AnswerDone: ` data-to="off"`, "park": ""} {
		if want := `<form method="post" action="/tasks/t-hq/answer" hx-post="/tasks/t-hq/answer" class="answer" data-answer="choice:` + value + `"` + to + `>`; !strings.Contains(esc, want) {
			t.Errorf("the choice %s lacks %s:\n%s", value, want, esc)
		}
	}
	if strings.Contains(esc, `action="/tasks/t-hq/close"`) {
		t.Errorf("a choice posts to the close route:\n%s", esc)
	}
}

// What an ask-less card still offers: an agent card's opened card holds no
// answer box; a question-less human card that asks, an old row, takes the
// human's words; one that asks nothing offers none; a close with a note
// posts it and records the close.
func TestAskLessAndQuestionLessCardsKeepTheirForms(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-wait", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 31, Objective: "Bump the weekly numbers."}},
		{Task: factoryd.Task{ID: "t-ask", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 33, Objective: "Pick the store."},
			Escalation: "The card asks: which store keeps the snapshots?"},
		{Task: factoryd.Task{ID: "t-agent", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 34, Objective: "the feed"}},
	}
	ck, s := r.loggedIn(t)
	if agent := drawer(t, r, ck, "t-agent", "overview"); strings.Contains(agent, `name="answer"`) {
		t.Errorf("the ask-less agent card still answers:\n%s", agent)
	}
	if wait := drawer(t, r, ck, "t-wait", "overview"); strings.Contains(wait, `name="answer"`) || strings.Contains(wait, "Answer in my words") {
		t.Errorf("the ask-less card's opened card offers an answer:\n%s", wait)
	}
	if ask := askBlockOf(t, drawer(t, r, ck, "t-ask", "overview")); !strings.Contains(ask, `<form method="post" action="/tasks/t-ask/answer" hx-post="/tasks/t-ask/answer" class="answer" data-answer="note/own_words">`) ||
		!strings.Contains(ask, `name="answer" rows="2" maxlength="4000" placeholder="The answer, posted on the issue as you" required hx-preserve>`) {
		t.Errorf("the old asking card lost its answer:\n%s", ask)
	}

	before := ev.count()
	rec := r.do(post("/tasks/t-wait/resolve", url.Values{"action": {factoryd.ActClose}, "csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "close.note": {"Not this week."}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the close with note = %d, want 303", rec.Code)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/31/comments"]; got != "Not this week." {
		t.Errorf("the note = %q, want it on the issue", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-wait", detail: map[string]any{"note": "Not this week."}},
		wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-wait",
			detail: map[string]any{"action": factoryd.ActClose, "case_id": int64(1), "answer": "Not this week.", "value": factoryd.ActClose, "comment": int64(1)}})
}

// A plan that filed its cards ends done as a final card whose place names
// it as its live face does; an unnamed plan's place adds no name, and a
// plan with no issue number shows no #0.
func TestDonePlanRowCarriesTheName(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Track: "plan", Process: "spec", Objective: "Link shortener",
			Repository: "linkshort/linkshort", IssueNumber: 1, PlanName: "fix prod latency"}},
	}
	get := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	}
	face := faceOf(t, get(), "t-plan")
	if !strings.Contains(face, "face final") || !strings.Contains(face, `<span class="it place">plan fix prod latency</span> <span class="it ref">#1</span>`) {
		t.Errorf("a named plan's done card lacks its place:\n%s", face)
	}
	r.tasks.tasks[0].PlanName = ""
	if face := faceOf(t, get(), "t-plan"); !strings.Contains(face, `<span class="it place">plan</span>`) {
		t.Errorf("an unnamed plan's done card names it:\n%s", face)
	}
	r.tasks.tasks[0].PlanName, r.tasks.tasks[0].IssueNumber = "fix prod latency", 0
	if face := faceOf(t, get(), "t-plan"); !strings.Contains(face, `<span class="it place">plan fix prod latency</span></p>`) || strings.Contains(face, "#0") {
		t.Errorf("a named plan without an issue number lacks its place or shows #0:\n%s", face)
	}
}

// A human card whose question has choices is answered by pressing one: its
// opened card offers no free-text box, which doHumanAnswer would refuse. A
// card whose question has none takes the human's words, posted on the issue.
func TestHumanCardOffersTheAnswerBoxOnlyWithoutChoices(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{
		humanAsk("t-pick", 51, factoryd.QuestionChoice{Value: "park", Label: "Wait for it"}, factoryd.QuestionChoice{Value: factoryd.AnswerDone, Label: "I did it"}),
		humanAsk("t-free", 52),
	}
	ck, _ := r.loggedIn(t)
	for id, want := range map[string]bool{"t-pick": false, "t-free": true} {
		drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
		if got := strings.Contains(drawer, `name="answer"`); got != want {
			t.Errorf("%s: the drawer's answer box = %v, want %v:\n%s", id, got, want, drawer)
		}
	}
	free := answerForm(t, drawer(t, r, ck, "t-free", "overview"), "note/own_words")
	free.Values.Set("answer", "SQLite, one file is enough.")
	if rec := r.do(post(free.Action, free.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the own words = %d, want 303: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/52/comments"]; got != "SQLite, one file is enough." {
		t.Errorf("the comment = %q, want the human's words", got)
	}
}

// A card's ask states the ask it answers: the case's parts, the ask, the
// why and the suggested answer with its writer and grounds, and what the
// call needs in the full brief; each answer's effect is one line. End the
// card closes the issue only if the card has one: a report card has no
// issue, and no text of its ask speaks of one. An answer's send carries the
// action and its note in one POST, which lands as one row, the note the
// row's answer: the issue's comment only where the card has an issue.
func TestTheNoteGoesWhereTheCardSays(t *testing.T) {
	for _, tc := range []struct {
		id             string
		caseID         int64
		card           func(*rig)
		ask, why, ends string
		gone           string
		posts          bool
	}{
		{id: "t-e", caseID: 5, card: caseCard, ask: "Go on with the replica?", why: "The agent stopped because it needs a decision only a human can make.",
			ends: "Ends the card and closes its issue. Nothing more runs.", gone: "kept on the card", posts: true},
		{id: "t-r", caseID: 7, card: reportCard, ask: "A step of the work ran past its time limit. Check the work again, or tell the card how to go on?",
			why: "A step of the work did not finish in the time it was given.", ends: "Ends the card. Nothing more runs.", gone: "issue"},
	} {
		r, ev := newRigWithEvents(t)
		ck, _ := r.loggedIn(t)
		tc.card(r)
		// the case's parts: its problem on the case, what the call needs
		// and the recommendation with its source on the ask
		k := r.openCases[tc.id][0]
		k.Problem = "Go on with the replica?"
		r.openCases[tc.id][0] = k
		r.caseAsks = map[int64]factoryd.CaseAsk{tc.caseID: {At: time.Now(), Detail: map[string]any{"info": "The reads outgrow the single node.",
			"recommendation": "Take the replica, which holds the reads.", "recommended_by": "the factory agent", "preselected": map[string]any{"action": factoryd.ActReplan}}}}
		page := drawer(t, r, ck, tc.id, "overview")
		face := askBlockOf(t, page)
		for _, want := range []string{
			`<p class="o-question">` + tc.ask + `</p>`,
			`<p class="o-askwhy">` + tc.why + `</p>`,
			`class="answer suggested" data-answer="replan" data-to="running">`,
			`<p class="o-by">suggested by the factory agent: <span class="quote">Take the replica, which holds the reads.</span></p>`,
			`<span class="effect">` + tc.ends + `</span>`,
		} {
			if !strings.Contains(face, want) {
				t.Errorf("%s: the ask lacks %s:\n%s", tc.id, want, face)
			}
		}
		if want := `<div class="info"><p>The reads outgrow the single node.</p>`; !strings.Contains(page, want) {
			t.Errorf("%s: the full brief lacks %s", tc.id, want)
		}
		if strings.Contains(face, tc.gone) {
			t.Errorf("%s: the ask says %q:\n%s", tc.id, tc.gone, face)
		}
		// each answer is a send of its own, with no fund beside it
		if got := strings.Count(face, `<form `); got != strings.Count(face, `action="/tasks/`+tc.id+`/resolve"`) || got < 2 || strings.Contains(face, "fund_eur") {
			t.Errorf("%s: the ask holds %d forms, not each a send of its own:\n%s", tc.id, got, face)
		}
		// the send: the action and its note ride one POST, which lands as
		// one row, the note the row's answer: on the issue as the human
		// where the card has one, kept on the card where it has none
		replan := answerForm(t, page, factoryd.ActReplan)
		note := "the replica holds"
		replan.Values.Set("replan.note", note)
		before := ev.count()
		if rec := r.do(post(replan.Action, replan.Values), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: the send = %d: %s", tc.id, rec.Code, rec.Body)
		}
		in, posts := actedOnce(t, r)
		if in.Res.Action != factoryd.ActReplan || in.Res.Note != note || in.Res.Fund != nil {
			t.Errorf("%s: the send answered with %+v, want replan with the note and no draw", tc.id, in.Res)
		}
		if posts != tc.posts {
			t.Errorf("%s: the send posts its note on the issue = %v, want %v", tc.id, posts, tc.posts)
		}
		if got, ok := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; ok != tc.posts || tc.posts && got != note {
			t.Errorf("%s: the note on the issue = %q (posted %v), want the note where the card has an issue", tc.id, got, ok)
		}
		detail := map[string]any{"action": factoryd.ActReplan, "case_id": tc.caseID, "answer": note}
		if tc.posts {
			detail["comment"] = int64(1)
		}
		checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: tc.id, detail: detail})
	}
}

// seatApprovalFixture seats the approval fixture of one asking kind on the
// rig: its card on the board, its approval request pending with the spec
// its case's facts carry, a minute old so its wait reads the same on every
// render. The fixture's ask restates the ask its face builds from the
// spec, so the render proves the face shows the ask the request carries.
func seatApprovalFixture(t *testing.T, r *rig, name string) factoryd.Fixture {
	t.Helper()
	fx, err := factoryd.LoadFixture(filepath.Join(boardFixtures, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	c := fx.Card
	status := "in-review"
	if fx.Asks[0].CaseKind == factoryd.ApprovalResource {
		status = "waiting-human"
	}
	task := factoryd.Task{ID: c.TaskID, Kind: cmp.Or(c.Kind, "agent"), Track: cmp.Or(c.Track, "direct"), Status: status,
		Repository: c.Repository, IssueNumber: c.Issue, Objective: c.Objective,
		BudgetEUR: c.BudgetEUR, CeilingEUR: c.CeilingEUR, CreatedAt: time.Now().Add(-time.Minute)}
	spec, err := json.Marshal(fx.Cases[0].Facts)
	if err != nil {
		t.Fatalf("marshal %s's spec: %v", name, err)
	}
	q := factoryd.ApprovalRequest{ID: 41, Kind: fx.Asks[0].CaseKind, TaskID: task.ID, Project: task.Repository,
		Status: "pending", Spec: spec, CreatedAt: time.Now().Add(-time.Minute)}
	if q.Kind == factoryd.ApprovalMerge {
		q.PRNumber = 31
	}
	if m, ok := fx.Cases[0].Facts["monthly_eur"].(float64); ok {
		q.MonthlyEUR = &m
	}
	r.tasks.set("", task)
	r.approvals.rows = append(r.approvals.rows, q)
	return fx
}

// approvalFaceOf is the ask and the full brief of card id's opened card in
// body, where its approval is decided and its evidence shown.
func approvalFaceOf(t *testing.T, body, id string) string {
	t.Helper()
	if !strings.Contains(body, `data-opened="`+id+`"`) {
		t.Fatalf("the opened card %s is missing:\n%s", id, body)
	}
	_, brief, _ := strings.Cut(body, `<details class="fold" data-sec="brief">`)
	brief, _, _ = strings.Cut(brief, "</details>")
	return askBlockOf(t, body) + brief
}

// The merge and resource requests ask on their card's opened card: each
// approval fixture's ask holds the register's question and why, the
// answers each a send of its own with a one-line effect, no fund beside
// them, and the request's evidence in the full brief; Approve lands the
// request.
func TestMergeAndResourceRequestsAskOnTheOpenedCard(t *testing.T) {
	for _, tc := range []struct {
		name, kind, ask, evidence string
		answers                   map[string]string
	}{
		{"board-merge.json", factoryd.ApprovalMerge, "Merge “Shard the feed cache”?",
			`<dt>Model review</dt><dd>adds the shard without changing a read</dd>`,
			map[string]string{factoryd.ActApproveMerge: "/approvals/41/approve", factoryd.ActDenyMerge: "/approvals/41/deny", factoryd.ActDenyMerge + "/reject": "/approvals/41/deny"}},
		{"board-resource.json", factoryd.ApprovalResource, "Rent a cx33 server for 5 hours at about 0.07 EUR?",
			`<h4>The server</h4><p>cx33, for load-test the feed cache.`,
			map[string]string{factoryd.AnswerApprove: "/approvals/41/approve", factoryd.AnswerDeny: "/approvals/41/deny"}},
	} {
		r, _ := newRigWithEvents(t)
		fx := seatApprovalFixture(t, r, tc.name)
		id := fx.Card.TaskID
		ck, _ := r.loggedIn(t)
		page := drawer(t, r, ck, id, "overview")
		esc := approvalFaceOf(t, page, id)
		if want := `<p class="o-question">` + tc.ask + `</p>`; !strings.Contains(esc, want) {
			t.Errorf("%s: the ask lacks %q:\n%s", tc.name, want, esc)
		}
		if !strings.Contains(esc, tc.evidence) {
			t.Errorf("%s: the full brief lacks %q:\n%s", tc.name, tc.evidence, esc)
		}
		for key, post := range tc.answers {
			if got := answerForm(t, page, key).Action; got != post {
				t.Errorf("%s: the answer %s posts to %s, want %s", tc.name, key, got, post)
			}
		}
		for what, want := range map[string]int{
			`<form `:                len(tc.answers),
			`<span class="effect">`: len(tc.answers),
			"fund":                  0,
			`hx-confirm`:            0,
		} {
			if got := strings.Count(askBlockOf(t, page), what); got != want {
				t.Errorf("%s: %s appears %d times in the ask, want %d", tc.name, what, got, want)
			}
		}
		// the fixture's review carries no verdict, so no one suggests the merge
		if strings.Contains(esc, "suggested") {
			t.Errorf("%s: the ask suggests what no one recommended:\n%s", tc.name, esc)
		}
		key := factoryd.AnswerApprove
		if tc.kind == factoryd.ApprovalMerge {
			key = factoryd.ActApproveMerge
		}
		approve := answerForm(t, page, key)
		if rec := r.do(post(approve.Action, approve.Values), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: the approve = %d: %s", tc.name, rec.Code, rec.Body)
		}
		if a := r.approvals.get(41); a.Status != "approved" {
			t.Errorf("%s: the request is %s, want approved", tc.name, a.Status)
		}
		if tc.kind == factoryd.ApprovalMerge && !slices.Contains(r.fg.calls, "POST /api/v1/repos/sf/code/pulls/31/reviews Bearer access-1") {
			t.Errorf("%s: the approval posted no review: %q", tc.name, r.fg.calls)
		}
	}

	// The same two kinds on the board's case path, seated as production
	// holds them: a merge request's case at the human, as openRequestCase
	// opens it, asks the merge on the case; the store opens no case of its
	// own for a resource request, so the request leads the asks of a card
	// that holds another case.
	for _, tc := range []struct {
		name, ask, post string
	}{
		{"board-merge.json", "Merge “Shard the feed cache”?", `action="/tasks/t-board-merge/resolve"`},
		{"board-resource.json", "Rent a cx33 server for 5 hours at about 0.07 EUR?", `action="/approvals/41/approve"`},
	} {
		r := newRig(t)
		fx := seatApprovalFixture(t, r, tc.name)
		id := fx.Card.TaskID
		kc := factoryd.Case{ID: 1, TaskID: id, Repository: fx.Card.Repository, Status: factoryd.CaseAtHuman, OpenedAt: time.Now().Add(-70 * time.Second)}
		if q := r.approvals.get(41); q.Kind == factoryd.ApprovalMerge {
			k, err := factoryd.RequestCase(q)
			if err != nil {
				t.Fatal(err)
			}
			kc.Kind, kc.Problem, kc.Facts = k.Kind, k.Problem, k.Facts
		} else {
			kc.Kind, kc.Problem, kc.Facts = factoryd.CaseOther, "The run waits on its server.", map[string]any{}
		}
		r.openCases = map[string][]factoryd.Case{id: {kc}}
		ck, _ := r.loggedIn(t)
		face := escalationOf(t, r, ck, id)
		if want := `<p class="o-question">` + tc.ask + `</p>`; !strings.Contains(face, want) || !strings.Contains(face, tc.post) {
			t.Errorf("%s: the case path's ask lacks %q or %s:\n%s", tc.name, want, tc.post, face)
		}
		for _, none := range []string{`<p class="o-question"></p>`, "The case is the", "fund"} {
			if strings.Contains(face, none) {
				t.Errorf("%s: the case path's ask shows %q:\n%s", tc.name, none, face)
			}
		}
	}
}

// An approve that names another action is refused rather than approving,
// and leaves the request pending; a deny goes through its own door, where
// it sends the change request.
func TestTheDecideSendRefusesWhatItCannotLand(t *testing.T) {
	r, _ := newRigWithEvents(t)
	seatApprovalFixture(t, r, "board-merge.json")
	ck, s := r.loggedIn(t)
	for _, action := range []string{"deny", "request_changes", "close"} {
		if code := r.do(post("/approvals/41/approve", url.Values{"csrf": {s.csrf}, "action": {action}, "note": {"the shard leaks"}}), ck).Code; code != http.StatusBadRequest {
			t.Errorf("an approve naming %s = %d, want 400", action, code)
		}
	}
	if a := r.approvals.get(41); a.Status != "pending" {
		t.Fatalf("a refused approve left the request %s", a.Status)
	}
	if code := r.do(post("/approvals/41/deny", url.Values{"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"the shard leaks"}}), ck).Code; code != http.StatusSeeOther {
		t.Errorf("the deny = %d, want it to send the change request", code)
	}
}

// A factory pull request's merge ask keeps a human's way to close it: its
// one deny is Reject and close it, said as the close it runs (denyMerge),
// and its send closes the pull request and its card; no agent works on
// the pull request, so nothing sends it back to one.
func TestFactoryMergeFaceKeepsTheClose(t *testing.T) {
	r, _ := newRigWithEvents(t)
	fx := seatApprovalFixture(t, r, "board-merge.json")
	facts := map[string]any{"factory": true}
	for k, v := range fx.Cases[0].Facts {
		facts[k] = v
	}
	spec, err := json.Marshal(facts)
	if err != nil {
		t.Fatalf("marshal the factory spec: %v", err)
	}
	for i, row := range r.approvals.rows {
		if row.ID == 41 {
			r.approvals.rows[i].Spec = spec
		}
	}
	ck, _ := r.loggedIn(t)
	page := drawer(t, r, ck, fx.Card.TaskID, "overview")
	esc := askBlockOf(t, page)
	if !strings.Contains(esc, `data-press="deny_merge/reject">Reject and close it</button> <span class="effect">Closes the pull request. The card ends.</span>`) {
		t.Errorf("the factory merge ask lacks its close:\n%s", esc)
	}
	if strings.Contains(esc, "Send it back") || strings.Contains(esc, "to its agent") {
		t.Errorf("the factory merge ask offers sending it back to an agent it has not:\n%s", esc)
	}
	reject := answerForm(t, page, "deny_merge/reject")
	reject.Values.Set("note", "the shard leaks")
	if rec := r.do(post(reject.Action, reject.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the close = %d: %s", rec.Code, rec.Body)
	}
	if !slices.ContainsFunc(r.fg.calls, func(c string) bool {
		return strings.HasPrefix(c, "SIGNAL changes_requested t-board-merge #31 by ")
	}) {
		t.Errorf("the close never reached the pull request: %q", r.fg.calls)
	}
}
