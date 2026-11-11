package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// titleOf is the title line's words on the face of card id, inside the
// link that opens its drawer for a writer.
func titleOf(t *testing.T, body, id string) string {
	t.Helper()
	face := faceOf(t, body, id)
	_, title, _ := strings.Cut(face, `<strong class="title" title="`)
	_, title, _ = strings.Cut(title, `">`)
	title, _, _ = strings.Cut(title, "</strong>")
	title = strings.TrimPrefix(title, `<a href="#card=`+id+`" class="open">`)
	return strings.TrimSuffix(title, "</a>")
}

// The face's title: a writer's summary, else the objective's first
// paragraph — the title as the issue was filed, in full; only a title
// longer than 80 runes is shortened, at a word boundary with an
// ellipsis, never mid-word. A plan child without a summary shows its
// objective's headline; its key sits on the plan chip.
func TestCardFaceTitle(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for _, tc := range []struct {
		name string
		task factoryd.Task
		want string
	}{
		{"plan key", factoryd.Task{PlanKey: "web-pages", Objective: "Build the pages. All of them."}, "Build the pages. All of them."},
		{"a writer's summary", factoryd.Task{PlanKey: "web-pages", Objective: "Fix the cache.", HumanSummary: "the cache, at last"}, "the cache, at last"},
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
		{"over 80 runes, cut at a word", factoryd.Task{Objective: strings.Repeat("abcdefghij ", 10)}, strings.TrimSuffix(strings.Repeat("abcdefghij ", 7), " ") + "…"},
		{"over 80 runes, one word", factoryd.Task{Objective: strings.Repeat("é", 81)}, strings.Repeat("é", 79) + "…"},
	} {
		tc.task.ID, tc.task.Kind, tc.task.Status, tc.task.Repository = "t-1", "agent", "running", "sf/code"
		r.tasks.set("", tc.task)
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		if got := titleOf(t, body, "t-1"); got != tc.want {
			t.Errorf("%s: title %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The pill: one class per state and the status's own words, so the waits
// stay apart; the author's wait wears the hollow pill, and a card parked on
// its image says it waits for it.
func TestCardFacePillPerStatus(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for status, class := range map[string]string{
		"running": "st-running", "closing": "st-running", "queued": "st-queued", "waiting-deps": "st-waiting",
		"waiting-human": "st-waiting", "waiting-poster": "st-waiting hollow", "waiting-image": "st-waiting", "in-review": "st-review",
		"done": "st-done", "failed": "st-failed", "closed": "st-closed",
	} {
		r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: status, Track: "direct", Repository: "sf/code", Objective: "work"})
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		word := cmp.Or(map[string]string{"running": "building", "closing": "building", "in-review": "verifying", "waiting-image": "waiting for image"}[status], status)
		want := `<span class="pill ` + class + `">` + word + `</span>`
		if glyph, final := map[string]string{"done": "✓", "failed": "✕", "closed": "⊘"}[status]; final {
			want = `<span class="st"><span class="g" aria-hidden="true">` + glyph + `</span>` + status + `</span>`
		}
		if status == "waiting-human" {
			// the five columns show the card as its case's row, with no pill; its face stays in the wide view's column
			body = col(t, r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String(), "Waiting on a human")
		}
		if !strings.Contains(faceOf(t, body, "t-1"), want) {
			t.Errorf("%s: the face lacks %s:\n%s", status, want, faceOf(t, body, "t-1"))
		}
	}
}

// Spend against budget to the cent: muted below 80 percent of the budget,
// from 80 percent on prominent with the warning sign and the words; a card
// without a budget shows none.
func TestCardFaceSpend(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-low", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "low", BudgetEUR: 10, SpentEUR: 7.99},
		factoryd.Task{ID: "t-at", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "at 80", BudgetEUR: 10, SpentEUR: 8},
		factoryd.Task{ID: "t-cents", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "at 80 in floats", BudgetEUR: 3, SpentEUR: 2.4},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no budget"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for id, want := range map[string]string{
		"t-at":    `<span class="spend near">€8.00 / €10.00 ⚠ near its budget</span>`,
		"t-cents": `<span class="spend near">€2.40 / €3.00 ⚠ near its budget</span>`,
	} {
		if face := faceOf(t, body, id); !strings.Contains(face, want) {
			t.Errorf("%s lacks %s:\n%s", id, want, face)
		}
	}
	for _, id := range []string{"t-low", "t-none"} {
		if face := faceOf(t, body, id); strings.Contains(face, `class="spend`) {
			t.Errorf("%s shows spend on its face:\n%s", id, face)
		}
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-low/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, "€7.99 of €10.00") {
		t.Errorf("the drawer lacks the spend under 80 percent:\n%s", drawer)
	}
	css := r.do(httptest.NewRequest(http.MethodGet, "/static/console.css", nil)).Body.String()
	for _, rule := range []string{".spend { color: var(--text-muted); }", ".spend.near { color: var(--need-text); }"} {
		if !strings.Contains(css, rule) {
			t.Errorf("console.css lacks %s", rule)
		}
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
// floor count: 4.50 settled and 0.40 held of 5.00 shows 4.90 spent on
// the face and in the drawer, and the Extend form starts from 0.10 left.
func TestBoardLeftNetsHolds(t *testing.T) {
	r := fundRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-e", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?",
		BudgetEUR: 5, SpentEUR: 4.5, HeldEUR: 0.4, ModelAlias: "factory/small"})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face, want := faceOf(t, body, "t-e"), `<span class="spend near">€4.90 / €5.00 ⚠ near its budget</span>`; !strings.Contains(face, want) {
		t.Errorf("the face lacks %s:\n%s", want, face)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{"€4.90 / €5.00 ⚠ near its budget", "€4.90 of €5.00, 98 percent", `€0.10 of €5.00">Extend the card`} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %s:\n%s", want, drawer)
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
	want := `<span class="mono">€1.20 of €5.00, 24 percent</span> <span class="mono" title="the task's model calls to date: the tokens its providers reported and the modeled pay-as-you-go cost of them, never money billed">· 100,000 prompt · 66,000 cached · 2,000 output tokens · €0.0420 modeled</span>`
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-calls/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, want) {
		t.Errorf("the drawer lacks its calls beside the budget:\n%s", drawer)
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-none/drawer?tab=overview", nil), ck).Body.String(); strings.Contains(drawer, "modeled") {
		t.Errorf("a card without call rows shows calls:\n%s", drawer)
	}
}

// The chips line holds only what a card has: its lane badge, its model,
// "blocked by n" with the issues in its title while dependencies are not
// done, "waiting on" in waiting-deps (never both); a card with none has
// no chips line and no transitions line.
func TestCardFaceChips(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-bare", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "bare", CreatedAt: time.Now()}},
		{Task: factoryd.Task{ID: "t-full", Kind: "agent", Status: "running", Track: "direct", Repository: "sf/code", Objective: "full",
			ModelAlias: "small"}, Dependencies: "#3, #5"},
		{Task: factoryd.Task{ID: "t-free", Kind: "agent", Status: "queued", Track: "direct", Repository: "sf/code", Objective: "deps done"}},
		{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "waiting-deps", Track: "direct", Repository: "sf/code", Objective: "waits"},
			WaitingOn: "#7", Dependencies: "#7"},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	full := faceOf(t, body, "t-full")
	if want := `<span class="chip blocked" title="#3, #5">blocked by 2</span>`; !strings.Contains(full, want) {
		t.Errorf("t-full lacks %s:\n%s", want, full)
	}
	if strings.Contains(full, "small") || strings.Contains(full, `class="badge"`) {
		t.Errorf("the face names the model or wears a badge:\n%s", full)
	}
	if bare := faceOf(t, body, "t-bare"); strings.Contains(bare, `class="chips"`) || strings.Contains(bare, `class="transitions"`) {
		t.Errorf("a card with nothing to show has a chips or transitions line:\n%s", bare)
	}
	if free := faceOf(t, body, "t-free"); strings.Contains(free, "blocked by") {
		t.Errorf("a card whose dependencies are done is blocked:\n%s", free)
	}
	wait := faceOf(t, body, "t-wait")
	if !strings.Contains(wait, `<span class="chip waiting">waiting on #7</span>`) || strings.Contains(wait, "blocked by") {
		t.Errorf("a card in waiting-deps shows not exactly its waiting chip:\n%s", wait)
	}
}

// A plan child's chip carries the plan's name after the plan reference:
// "from plan #1 · fix prod latency"; an unnamed plan reads "from plan
// #1"; a parent that has no issue number yet keeps the chip with the key
// alone.
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
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-c4"); !strings.Contains(face, `<span class="chip plan">from plan #1</span>`) || strings.Contains(face, "links-api</span>") {
		t.Errorf("t-c4's face lacks its bare plan chip, or shows the key:\n%s", face)
	}
	// a named plan: the name takes the key's slot, on the child and on the plan's own face
	r.tasks.tasks[0].PlanName = "fix prod latency"
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-c4"); !strings.Contains(face, `<span class="chip plan">from plan #1 · fix prod latency</span>`) || strings.Contains(face, "links-api</span>") {
		t.Errorf("t-c4's face lacks its plan chip with the name, or still shows the key:\n%s", face)
	}
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, `<span class="chip plan">#1 · fix prod latency</span>`) {
		t.Errorf("the plan's own face lacks its name chip:\n%s", face)
	}
	r.tasks.tasks[0].PlanName = ""
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-c4"); !strings.Contains(face, `<span class="chip plan">from plan #1</span>`) {
		t.Errorf("t-c4's face lacks the bare plan chip of an unnamed plan:\n%s", face)
	}
	if face := faceOf(t, body, "t-plan"); strings.Contains(face, `chip plan`) {
		t.Errorf("an unnamed plan's own face carries a plan chip:\n%s", face)
	}
	// the parent filed before its issue: the chip keeps the key alone
	r.tasks.tasks[0].IssueNumber = 0
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-c4"); !strings.Contains(face, `<span class="chip plan">links-api</span>`) || strings.Contains(face, "from plan") {
		t.Errorf("the child of a plan without an issue number lacks the key alone on its chip:\n%s", face)
	}
	// named, the name outranks the key, and the plan's own chip shows no #0
	r.tasks.tasks[0].PlanName = "fix prod latency"
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-c4"); !strings.Contains(face, `<span class="chip plan">fix prod latency</span>`) {
		t.Errorf("the child of a named plan without an issue number lacks the name alone on its chip:\n%s", face)
	}
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, `<span class="chip plan">fix prod latency</span>`) || strings.Contains(face, "#0") {
		t.Errorf("a named plan without an issue number lacks the name alone on its chip:\n%s", face)
	}
}

// A done plan's card rolls its cards up as n running · n done · n waiting
// · n queued, only the parts with cards: a plan whose cards all wait for
// a work slot reads them queued (roadmap 225).
func TestCardFacePlanChildren(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = append(planTasks(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-c9", Kind: "agent", Status: "running", Track: "direct",
		Process: "spec", Objective: "card 9", Repository: "linkshort/linkshort", IssueNumber: 9, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k9"}})
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, "<span class=\"chip rollup\">1\u00a0running · 1\u00a0done · 1\u00a0waiting · 2\u00a0queued</span>") {
		t.Errorf("the plan's face lacks its rollup:\n%s", face)
	}
	r.tasks.tasks = planTasks()[:3]
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, "<span class=\"chip rollup\">2\u00a0queued</span>") {
		t.Errorf("a plan with only queued cards lacks their rollup:\n%s", face)
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
	if face := faceOf(t, body, "t-pr"); !strings.Contains(face, `<span class="checks">2/3 checks</span>`) {
		t.Errorf("the card in review lacks 2/3 checks:\n%s", face)
	}
	if face := faceOf(t, body, "t-skip"); !strings.Contains(face, `<span class="checks">2/2 checks</span>`) {
		t.Errorf("the card with a skipped job lacks 2/2 checks:\n%s", face)
	}
	for _, id := range []string{"t-done", "t-nopr"} {
		if face := faceOf(t, body, id); strings.Contains(face, `class="checks"`) {
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
	if face := faceOf(t, body, "t-stale"); strings.Contains(face, `class="checks"`) {
		t.Errorf("a push the checks never ran on still wears their counts:\n%s", face)
	}
	if face := faceOf(t, body, "t-caught-up"); !strings.Contains(face, `<span class="checks">5/5 checks</span>`) {
		t.Errorf("the checks on the newest head lack their 5/5:\n%s", face)
	}
	if face := faceOf(t, body, "t-no-push"); !strings.Contains(face, `<span class="checks">5/5 checks</span>`) {
		t.Errorf("checks with no push to be stale against lack their 5/5:\n%s", face)
	}
}

// The face carries the newest session's attempt beside the gate clock,
// which restarts on every retry: "under a minute" alone reads as
// progress, not as the third try's restart. A card out of its stage
// shows no floating attempt.
func TestCardFaceAttemptBesideTheGateClock(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	attempts := `{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"retry"}
{"step":"session_start","attempt":2}
{"step":"session_end","attempt":2,"outcome":"retry"}
{"step":"session_start","attempt":3}`
	r.objects["sessions/t-gate/timeline.jsonl"] = []byte(attempts)
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(attempts)
	r.tasks.set("",
		factoryd.Task{ID: "t-gate", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "at the gate", PRNumber: 4},
		factoryd.Task{ID: "t-wait", Kind: "agent", Status: "waiting-poster", Repository: "sf/code", Objective: "waiting", PRNumber: 5})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-gate"); !strings.Contains(face, `<span class="elapsed">attempt 3</span>`) {
		t.Errorf("the card at its gate lacks its third attempt beside the clock:\n%s", face)
	}
	if face := faceOf(t, body, "t-wait"); strings.Contains(face, "attempt") {
		t.Errorf("a card out of its stage carries a floating attempt:\n%s", face)
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
		if face := faceOf(t, body, "t-pr"); !strings.Contains(face, `<span class="checks">1/1 checks</span>`) {
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

// The wide view renders an escalated card twice, as its case's row in the
// escalations and as its face in its column, the five columns once, in
// Needs you: its summary and accent fields sit inside their labels with no
// id, so no id repeats on the page.
func TestEscalatedCardRepeatsNoID(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("The agent asks: which region?", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
		IssueNumber: 13, Objective: "asks"})
	if n := strings.Count(r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), `data-card="t-e"`); n != 1 {
		t.Errorf("the five columns render the escalated card's data-card %d times, want its case row's alone", n)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	if n := strings.Count(body, `data-card="t-e"`); n != 2 {
		t.Fatalf("the escalated card renders %d times, want its case row's and its column card's", n)
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
		`<input type="text" id="summary-t-e-d" name="summary" maxlength="255" value="" placeholder="asks">`,
		`<select id="accent-t-e-d" name="value">`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %s:\n%s", want, drawer)
		}
	}
}

// The face's lines take no paragraph margins, and the accent edge rules
// come after .card .card with its weight, so the face nested in an
// escalation keeps its edge.
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
	if !strings.Contains(css, ".face > p { margin: 0; }") {
		t.Error("console.css lacks .face > p { margin: 0; }")
	}
	// an approval face's brief holds at one line and its five parts at
	// four, as .card .reason holds a reason; the rules sit inside a
	// lane's column, so the drawer shows the parts whole
	for _, rule := range []string{
		".column .approval .brief { display: -webkit-box; -webkit-line-clamp: 1; -webkit-box-orient: vertical; overflow: hidden; }",
		".column .approval .why, .column .approval .info, .column .approval .finding, .column .approval .recommendation { display: -webkit-box; -webkit-line-clamp: 4; -webkit-box-orient: vertical; overflow: hidden; }",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("console.css lacks %s", rule)
		}
	}
}

// An accent is a chip with its name and the card's left edge and changes
// no state rendering; a card without one has neither.
func TestCardFaceAccent(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-teal", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "teal work", Accent: "teal", CreatedAt: time.Now()},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "plain work", CreatedAt: time.Now()})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	teal := faceOf(t, body, "t-teal")
	if !strings.HasPrefix(teal, `<article class="card face edge-teal" data-card="t-teal" `) || !strings.Contains(teal, `<span class="chip accent-teal">teal</span>`) ||
		!strings.Contains(teal, `<span class="pill st-running">building</span>`) {
		t.Errorf("the teal card lacks its edge, its chip or its plain running pill:\n%s", teal)
	}
	if none := faceOf(t, body, "t-none"); !strings.HasPrefix(none, `<article class="card face" data-card="t-none" `) || strings.Contains(none, "chip accent-") {
		t.Errorf("a card without an accent shows one:\n%s", none)
	}
}

// The amber ring and its words: a live card whose newest event is half an
// hour old or more, or that has had none in its half hour on the board;
// never a final card, never one parked in the queue.
func TestCardFaceStaleRing(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	old, recent := time.Now().Add(-45*time.Minute), time.Now().Add(-5*time.Minute)
	r.tasks.set("",
		factoryd.Task{ID: "t-quiet", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "quiet", CreatedAt: old},
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
	// the waiting card's face is its column's in the wide view; the five columns show it as its case's row
	waiting := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String(), "Waiting on a human")
	quiet := `card retried by sebastian via button at ` + old.UTC().Format("15:04") + ` — nothing for over 45 min</p>`
	for id, words := range map[string]string{"t-quiet": quiet,
		"t-ask": `<p class="activity">which region? — no event yet, 45 min on the card</p>`} {
		board := body
		if id == "t-ask" {
			board = waiting
		}
		if face := faceOf(t, board, id); !strings.HasPrefix(face, `<article class="card face stale" data-card="`+id+`" `) || !strings.Contains(face, words) {
			t.Errorf("%s lacks the ring or %s:\n%s", id, words, face)
		}
	}
	for _, id := range []string{"t-busy", "t-done", "t-new"} {
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
	if want := "nothing for over 40 min"; !strings.Contains(face, want) {
		t.Errorf("the ring's words are not the step's age:\n%s", face)
	}
}

// A waiting card's activity line is the question it waits on, not its
// newest event, in its column; the escalations and Needs you show the
// question as the ask part of the card's case.
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
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	want := `<p class="activity"><a href="https://git.example/sf/code/issues/12" target="_blank" rel="noopener">answer on the issue</a> — use a replica or a shard?</p>`
	if face := faceOf(t, col(t, body, "Waiting on a human"), "t-e"); !strings.Contains(face, want) || strings.Contains(face, "card_retried") {
		t.Errorf("Waiting on a human: the waiting card's activity is not its question:\n%s", face)
	}
	// the escalations and Needs you show the card as its case's row, whose ask part is the question
	for title, board := range map[string]string{"Escalations": body, "Needs you": five} {
		if row := escalationOf(t, col(t, board, title), "t-e"); !strings.Contains(row, `<p class="ask">use a replica or a shard?</p>`) || strings.Contains(row, "card_retried") {
			t.Errorf("%s: the waiting card's ask part is not its question:\n%s", title, row)
		}
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

// The summary: a writer's POST answers 303 and the face shows it trimmed,
// up to 255 characters (not bytes), 400 beyond with nothing stored; an
// empty or blank one restores the generated title; a non-writer gets 403.
// One row per stored summary.
func TestSummaryPost(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "Fix the cache. Then the tests.", IssueNumber: 1})
	for _, step := range []struct {
		summary string
		code    int
		title   string
		rows    int
	}{
		{"a short summary", http.StatusSeeOther, "a short summary", 1},
		{strings.Repeat("é", 255), http.StatusSeeOther, strings.Repeat("é", 255), 1},
		{strings.Repeat("x", 256), http.StatusBadRequest, strings.Repeat("é", 255), 0},
		{"  the cache  ", http.StatusSeeOther, "the cache", 1},
		{"   ", http.StatusSeeOther, "Fix the cache. Then the tests.", 1},
		{"", http.StatusSeeOther, "Fix the cache. Then the tests.", 1},
	} {
		before := ev.count()
		rec := r.do(post("/tasks/t-1/summary", url.Values{"csrf": {s.csrf}, "summary": {step.summary}}), ck)
		if rec.Code != step.code || ev.count()-before != step.rows {
			t.Fatalf("summary %.20q = %d with %d rows, want %d with %d: %s", step.summary, rec.Code, ev.count()-before, step.code, step.rows, rec.Body)
		}
		if got := titleOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-1"); got != step.title {
			t.Fatalf("after summary %.20q the title is %.20q, want %.20q", step.summary, got, step.title)
		}
	}
	r.repoWriters["sf/code"] = nil
	r.forgetWriters()
	before := ev.count()
	if rec := r.do(post("/tasks/t-1/summary", url.Values{"csrf": {s.csrf}, "summary": {"mine"}}), ck); rec.Code != http.StatusForbidden || ev.count() != before {
		t.Fatalf("a non-writer's summary = %d with %d rows, want 403 and none", rec.Code, ev.count()-before)
	}
	if k, _, _ := r.tasks.Task(t.Context(), "t-1"); k.HumanSummary != "" {
		t.Fatalf("a refused summary stored %q", k.HumanSummary)
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
		return r.do(post("/tasks/t-1/accent", url.Values{"csrf": {s.csrf}, "value": {value}}), ck).Code
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
	if face := faceOf(t, body, "t-1"); strings.Contains(face, "chip accent-") || strings.Contains(face, "edge-") ||
		!strings.Contains(drawer(), `<option value="" selected>none</option>`) {
		t.Fatalf("a cleared accent still shows:\n%s", face)
	}
	r.repoWriters["sf/code"] = nil
	r.forgetWriters()
	if code := accent("blue"); code != http.StatusForbidden || stored() != "" {
		t.Fatalf("a non-writer's accent = %d, stored %q; want 403 and nothing", code, stored())
	}
}

// On a filtered board every form carries the filter back: retry, the
// plan's changes, summary and accent; the poll asks for the same board,
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
	for _, want := range []string{
		`data-query="?project=linkshort" hx-get="/board?project=linkshort" hx-trigger="board-refresh from:body" hx-swap="outerHTML">`,
		htmx("/tasks/t-c7/retry") + ` class="inline">` + filter,
		htmx("/tasks/t-spec2/plan/changes") + ` data-to="running">` + "\n" + filter,
		`<form method="post" ` + htmx("/tasks/t-ask/resolve") + ` class="decide resolve" data-to="running">` + "\n" + filter,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the filtered board lacks %s", want)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c2/drawer?project=linkshort", nil), ck).Body.String()
	for _, want := range []string{
		htmx("/tasks/t-c2/summary") + ` class="edit-form summary-edit">` + filter,
		htmx("/tasks/t-c2/accent") + ` class="edit-form accent-edit auto-submit">` + filter,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the filtered drawer lacks %s", want)
		}
	}
	// the accent applies as it is chosen: its form has no button to press
	if i := strings.Index(drawer, `class="edit-form accent-edit auto-submit">`); i < 0 || strings.Contains(drawer[i:i+strings.Index(drawer[i:], "</form>")], "<button") {
		t.Error("the accent form is missing or holds a button")
	}
	for path, form := range map[string]url.Values{
		"/tasks/t-c2/summary": {"summary": {"links"}},
		"/tasks/t-c2/accent":  {"value": {"blue"}},
		"/tasks/t-ask/answer": {"answer": {"europe"}},
	} {
		form.Set("csrf", s.csrf)
		form.Set("project", "linkshort")
		if rec := r.do(post(path, form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" {
			t.Errorf("%s = %d to %q, want 303 to the filtered board", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	if got := r.tasks.tasks[1]; got.ID != "t-c2" || got.HumanSummary != "links" || got.Accent != "blue" {
		t.Errorf("t-c2 = %q, %q; want the summary links and the accent blue, not the filter's teal", got.HumanSummary, got.Accent)
	}
}

// escalationOf is card id's escalation on the board: its face, its
// question and its forms.
func escalationOf(t *testing.T, body, id string) string {
	t.Helper()
	for _, part := range strings.Split(body, `<article class="card case" id="need-`)[1:] {
		row, _, _ := strings.Cut(part, "</article>")
		if strings.Contains(row, `data-card="`+id+`"`) {
			return row + "</article>"
		}
	}
	for _, part := range strings.Split(body, `<article class="card escalation" id="need-`)[1:] {
		face, rest, _ := strings.Cut(part, "</article>")
		if strings.Contains(face, `data-card="`+id+`"`) {
			own, _, _ := strings.Cut(rest, "</article>")
			return face + "</article>" + own
		}
	}
	t.Fatalf("no escalation of %s:\n%s", id, body)
	return ""
}

// A needs-you card with its agent's choices offers one button per choice
// above the text box, each a form of its own to the answer endpoint with
// the session's token and the board's filter; a card without choices
// offers the text box alone, as before. A click is one answer: the choice
// on the issue as the member, one row via button, a 303 to the filtered
// board.
func TestNeedsYouCardAnswersWithAChoice(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-ch", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "add the feed"},
			Escalation: "The agent asks: a replica or a shard?\nWhy: the reads outgrow one node", Choices: []string{"a replica", "a shard", "both"}},
		{Task: factoryd.Task{ID: "t-plain", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14, Objective: "pick a port"},
			Escalation: "The agent asks: which port?"},
	}
	ck, s := r.loggedIn(t)
	for _, page := range []string{"/?project=ai-software-factory-code&view=wide", "/?project=ai-software-factory-code"} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		esc := escalationOf(t, body, "t-ch")
		want := `<p class="ask">a replica or a shard?</p>`
		if !strings.Contains(esc, want) || !strings.Contains(esc, `action="/tasks/t-ch/resolve"`) || !strings.Contains(esc, `name="case_id" value="1"`) ||
			!strings.Contains(esc, `<div class="premade" role="group" aria-label="The answers the ask offers, sent as a continue">`+
				`<button type="submit" name="premade" value="a replica" class="primary">a replica</button>`+"\n"+
				`<button type="submit" name="premade" value="a shard" class="primary">a shard</button>`+"\n"+
				`<button type="submit" name="premade" value="both" class="primary">both</button>`+"\n</div>") {
			t.Errorf("%s: the escalation of t-ch =\n%s\nwant the one form with the three premade answers", page, esc)
		}
		if n := strings.Count(esc, `name="premade"`); n != 3 {
			t.Errorf("%s: t-ch offers %d premade buttons, want 3", page, n)
		}
		plain := escalationOf(t, body, "t-plain")
		if strings.Contains(plain, `class="premade"`) || !strings.Contains(plain, `<textarea id="note-t-plain" name="note"`) {
			t.Errorf("%s: the escalation of t-plain =\n%s\nwant the note box alone", page, plain)
		}
	}

	before := ev.count()
	rec := r.do(post("/tasks/t-ch/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "view": {"wide"},
		"case_id": {"1"}, "premade": {"a shard"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code&view=wide" {
		t.Fatalf("the choice = %d to %q, want 303 to the filtered board", rec.Code, rec.Header().Get("Location"))
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "a shard" {
		t.Fatalf("the comment = %q, want the choice", got)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-ch",
		detail: map[string]any{"action": factoryd.ActContinue, "answer": "a shard", "case_id": int64(1), "comment": int64(1)}})
}

// A shaped question's case row on the board holds the case's ask and its
// buttons, never the question's raw text; the drawer holds the question in
// full, its why and info rendered as markup with the raw words behind a fold. A
// card that holds answers its run has not taken warns instead of
// offering the box.
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
	page := func(bt factoryd.BoardTask, url string) string {
		r, _ := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{bt}
		r.openCases = map[string][]factoryd.Case{"t-q": {{ID: 1, TaskID: "t-q", Kind: factoryd.CaseSessionCap, Status: factoryd.CaseAtHuman, Problem: question.Ask,
			Facts: roomyFacts(nil)}}}
		r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"recommendation": question.Recommendation, "recommended_by": "the factory question"}}}
		ck, _ := r.loggedIn(t)
		return r.do(httptest.NewRequest(http.MethodGet, url, nil), ck).Body.String()
	}
	bt := factoryd.BoardTask{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
		Escalation: "the session cap is reached: every finding sent to a human returns unacted", Question: question}
	esc := escalationOf(t, page(bt, "/?project=ai-software-factory-code"), "t-q")
	for _, want := range []string{
		`<p class="ask">The call cap is reached: 4 calls of 4, 1.50 EUR of 6.00 EUR left. Which way on?</p>`,
		`<button type="submit" name="action" value="extend" class="secondary">Extend</button>`,
		`<button type="submit" name="action" value="note" class="secondary">Note</button>`,
		`<button type="submit" name="action" value="close" class="secondary">Close</button>`,
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("the escalation lacks %q:\n%s", want, esc)
		}
	}
	// §5.8 puts the recommendation, with who wrote it, on the face
	if want := `<p class="recommendation">Recommendation (the factory question): Grant one session</p>`; !strings.Contains(esc, want) {
		t.Errorf("the case's row lacks %q:\n%s", want, esc)
	}
	if strings.Count(esc, `<p class="ask">`) != 1 {
		t.Errorf("the row shows the ask other than once:\n%s", esc)
	}
	for _, bulky := range []string{`<pre class="reason detail">`, "every finding sent to a human returns unacted"} {
		if strings.Contains(esc, bulky) {
			t.Errorf("the board card carries %q, which belongs to the drawer:\n%s", bulky, esc)
		}
	}
	if strings.Contains(esc, `name="answer" value=`) {
		t.Errorf("the shaped card offers the agent's choices as labels:\n%s", esc)
	}
	if strings.Contains(esc, `name="value"`) {
		t.Errorf("the shaped card renders a value button of the old choices:\n%s", esc)
	}
	drawer := page(bt, "/cards/t-q/drawer?tab=overview")
	for _, want := range []string{
		`<p class="ask">The call cap is reached: 4 calls of 4, 1.50 EUR of 6.00 EUR left. Which way on?</p>`,
		`<div class="why"><p>the session cap is reached: every finding sent to a human returns unacted</p>`,
		`<details class="fold"><summary>What each answer does</summary>`,
		`<div class="info"><p>A grant buys one more session on this card. Park holds the card for a person to work with.</p>`,
		`<p class="recommendation">Recommendation (the factory question): Grant one session</p>`,
		`<details class="fold"><summary>The raw text, for copying</summary><pre class="reason detail">the session cap is reached: every finding sent to a human returns unacted</pre></details>`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %q:\n%s", want, drawer)
		}
	}

	// the why renders as markup when it is markup, its raw text behind the fold
	question.Why = "## Why the card stopped\n- the cap counts `4` calls\n\n```go\nfmt.Println(4)\n```"
	drawer = page(bt, "/cards/t-q/drawer?tab=overview")
	for _, want := range []string{
		`<div class="why"><h2>Why the card stopped</h2>`,
		`<li>the cap counts <code>4</code> calls</li>`,
		`<pre><code class="language-go">fmt.Println(4)`,
		`<details class="fold"><summary>The raw text, for copying</summary><pre class="reason detail">## Why the card stopped`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %q:\n%s", want, drawer)
		}
	}
	if strings.Contains(drawer, "&lt;h2&gt;") || strings.Contains(drawer, "&lt;code&gt;") {
		t.Errorf("the drawer renders the markup as text:\n%s", drawer)
	}

	bt.AnswersPending = 2
	esc = escalationOf(t, page(bt, "/?project=ai-software-factory-code"), "t-q")
	if !strings.Contains(esc, `<p class="liveness" role="status">This card holds answers its run has not taken`) {
		t.Errorf("the card with pending answers carries no liveness warning:\n%s", esc)
	}
	if strings.Contains(esc, `id="answer-t-q"`) {
		t.Errorf("the card with pending answers still offers the answer box:\n%s", esc)
	}
	if !strings.Contains(esc, `name="action" value="note"`) {
		t.Errorf("the card with pending answers lost its buttons:\n%s", esc)
	}
}

// A card that asks closes like every ask-less card since roadmap 1131:
// on a note, without a browser confirm. The plain ask ends on a note
// posted on the issue; a shaped question's End-the-card choice closes
// in one click, as the same choice does in the chat. The native confirm
// stays only where no note carries the close: the ask-less non-human
// cards.
func TestAskingCardClosesOnANoteWithoutAConfirm(t *testing.T) {
	r, _ := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-idle", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 41, Objective: "Pick the next work."},
			Escalation: "The board is idle and main is green. What should I start or improve next?"},
		{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 42, Objective: "the feed"},
			Escalation: "the session cap is reached",
			Question: &factoryd.HumanQuestion{Ask: "The call cap is reached. Which way on?", Why: "the session cap is reached",
				Choices: []factoryd.QuestionChoice{{Value: "grant", Label: "Grant one session"}, {Value: "close", Label: "End the card"}}}},
	}
	ck, s := r.loggedIn(t)
	for _, page := range []string{"/?project=ai-software-factory-code&view=wide", "/?project=ai-software-factory-code"} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		filter := `<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code">`
		if strings.Contains(page, "view=wide") {
			filter += `<input type="hidden" name="view" value="wide">`
		}
		idleClose := `<form method="post" action="/tasks/t-idle/close" hx-post="/tasks/t-idle/close" class="inline close" data-to="off">` + filter +
			`<textarea name="note" rows="1" maxlength="4000" required aria-label="note, posted on the issue as you"></textarea><button type="submit" class="deny" aria-label="Close #41 with a note">Close with note</button></form>`
		idle := faceOf(t, body, "t-idle")
		if !strings.Contains(idle, idleClose) {
			t.Errorf("%s: the asking card's face lacks the close with note:\n%s", page, idle)
		}
		if strings.Contains(idle, `>Close</button>`) {
			t.Errorf("%s: the asking card's face still offers the plain close:\n%s", page, idle)
		}
		for _, id := range []string{"t-idle", "t-q"} {
			if face := faceOf(t, body, id); strings.Contains(face, "hx-confirm") {
				t.Errorf("%s: the face of %s asks the browser to confirm a close:\n%s", page, id, face)
			}
		}
		q := escalationOf(t, body, "t-q")
		oneClick := `<button type="submit" name="action" value="close" class="secondary">Close</button>`
		if !strings.Contains(q, oneClick) {
			t.Errorf("%s: the question's End-the-card choice is not a one-click close:\n%s", page, q)
		}
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
// e.target.closest("#board [data-card]"), so the case article carries
// the data-card and its ask, its choices row and its foot are its
// children. The answer buttons stay their own doors: the handler skips
// a form, a button and a label.
func TestNeedsYouCardOpensItsDrawerFromAnywhere(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
		Escalation: "the session cap is reached: every finding sent to a human returns unacted",
		Question: &factoryd.HumanQuestion{Ask: "The call cap is reached: which way on?",
			Choices: []factoryd.QuestionChoice{{Value: "grant", Label: "Grant one session"}, {Value: "park", Label: "Park it"}}}}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if !strings.Contains(body, `<article class="card case" id="need-t-q" data-card="t-q"`) {
		t.Errorf("the case article carries no data-card, so a click on the ask, the choices or the foot opens no drawer:\n%s", escalationOf(t, body, "t-q"))
	}
	// the head, the ask, the choices and the foot are children of the one
	// article whose data-card the walk finds
	esc := escalationOf(t, body, "t-q")
	askAt := strings.Index(esc, `<p class="ask">`)
	if strings.Contains(esc, "<article") || askAt < 0 || !strings.Contains(esc[askAt:], `class="decide resolve"`) || !strings.Contains(esc[askAt:], `<p class="need-foot">`) {
		t.Errorf("the ask, the choices and the foot do not sit in the case article:\n%s", esc)
	}
}

// The shaped buttons route by value: a park or keep waiting is a note on
// the card's case that leaves the card waiting, nothing posted on the
// issue, and a typed answer that repeats a button's words takes that
// button's route.
func TestNeedsYouCardRoutesTheValue(t *testing.T) {
	question := func() *factoryd.HumanQuestion {
		return &factoryd.HumanQuestion{
			Ask:  "The check-in's share of the budget is spent: 0.90 EUR of the 6.00 EUR budget. Which way on?",
			Why:  "the check-in's share of the budget is spent: sessions bought on now would overrun the card's share",
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
			Escalation: "the check-in's share of the budget is spent", Question: question()}}
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
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "" {
		t.Fatalf("the park posted %q, want no comment", got)
	}
	checkRows(t, ev, before)
	got, _ := actedOnce(t, r)
	equal(t, "the park", got.Res, factoryd.Resolution{Action: factoryd.ActNote, Note: "Park it"})

	r, ev, ck, s = setup(t)
	before = ev.count()
	if rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"answer": {"Wait for the window"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the typed label = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "" {
		t.Fatalf("the typed label posted %q, want no comment", got)
	}
	checkRows(t, ev, before)
	got, _ = actedOnce(t, r)
	equal(t, "the typed label", got.Res, factoryd.Resolution{Action: factoryd.ActNote, Note: "Wait for the window"})

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
// answer text: fund, with no budget refusal left, is the label's sentence
// on the issue as the human with Act's answer row, and park and keep
// waiting are a note on the case, nothing posted.
func TestNeedsYouCardAnswersAChoiceWithoutText(t *testing.T) {
	question := func() *factoryd.HumanQuestion {
		return &factoryd.HumanQuestion{
			Ask:  "The check-in's share of the budget is spent: 0.90 EUR of the 6.00 EUR budget. Which way on?",
			Why:  "the check-in's share of the budget is spent",
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
			Escalation: "the check-in's share of the budget is spent", Question: question()}}
		ck, s := r.loggedIn(t)
		return r, ev, ck, s
	}
	for _, tc := range []struct {
		value, label string
		posted       bool
	}{
		{"fund", "Fund the card", true},
		{"park", "Park it", false},
		{"keep_waiting", "Wait for the window", false},
	} {
		r, ev, ck, s := setup(t)
		before := ev.count()
		rec := r.do(post("/tasks/t-q/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
			"value": {tc.value}}), ck)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("the %s click = %d %q, want 303", tc.value, rec.Code, rec.Body.String())
		}
		got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]
		if tc.posted && got != tc.label {
			t.Fatalf("the %s click posted %q, want the label %q", tc.value, got, tc.label)
		}
		if !tc.posted && got != "" {
			t.Fatalf("the %s click posted %q, want no comment", tc.value, got)
		}
		if !tc.posted {
			checkRows(t, ev, before)
			continue
		}
		checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-q",
			detail: map[string]any{"action": factoryd.ActContinue, "answer": tc.label, "case_id": int64(1), "comment": int64(1)}})
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

// The merge approval's face stays as small as a question's (the
// 2026-10-17 issue): the title row with the risk and migration chips,
// the Approve and Deny controls and the foot links. A five-part ask
// of any length — here a recommendation over three lines — waits in
// the drawer, which the face's Details link opens. The brief clamps
// to its first line on the face, and shows whole in the drawer.
func TestNeedsYouMergeFaceStaysSmall(t *testing.T) {
	r := newRig(t)
	long := "line one of the recommendation\nline two weighs the risk\nline three names the migration order"
	spec := fmt.Sprintf(`{"risk_class":"high","migration":false,"verdict_summary":"adds the column",`+
		`"review":{"verdict":"approve","findings":["0018_x.sql: add a default"],`+
		`"criteria":[{"criterion":"no schema migration","kind":"human","evidence":"the diff moves the column"}]},`+
		`"head_sha":"abc123","recommendation":%q,`+
		`"why_human":["first why","second why"]}`, long)
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 6, Kind: "merge", TaskID: "t-m2",
		Project: "sf/code", PRNumber: 12, Status: "pending",
		Spec: json.RawMessage(spec)})
	r.openCases = map[string][]factoryd.Case{"t-m2": {{ID: 90, TaskID: "t-m2", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"approval": 6.0}, Brief: "first line of the merge brief\nsecond line of the merge brief"}}}
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m2", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 12}})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-6"`)
	face := body[start : start+strings.Index(body[start:], "</article>")]
	for _, bulky := range []string{"line one of the recommendation", "first why", "adds the column",
		"second line of the merge brief", "0018_x.sql: add a default", "yours to judge at this merge",
		`<p class="why">`, `<p class="info">`, `<p class="finding">`, `<p class="recommendation">`} {
		if strings.Contains(face, bulky) {
			t.Errorf("the merge face carries %q, which belongs to the drawer:\n%s", bulky, face)
		}
	}
	for _, want := range []string{`action="/approvals/6/approve"`,
		fmt.Sprintf(`<a href="#card=%s:reviews" class="details">Details</a>`, "t-m2")} {
		if !strings.Contains(face, want) {
			t.Errorf("the merge face lacks %q:\n%s", want, face)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m2/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{
		"<p>first line of the merge brief\nsecond line of the merge brief</p>",
		`<p class="why">first why</p>`,
		`<p class="why">second why</p>`,
		`<p class="info"><b>Model review</b> approve: adds the column</p>`,
		`<p class="finding">0018_x.sql: add a default</p>`,
		`<p class="recommendation">Recommendation: ` + long + `</p>`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the merge drawer lacks %q", want)
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

// A human card shows the question a chat escalation files it from
// (values are the labels in the "say:" namespace): the whole ask once, as
// its case's ask part with no second title, one button per choice, the why,
// info and recommendation in the drawer under the ask as its head, and
// the card's own Close beside them. An agent card keeps its ask line
// under its own title.
func TestAHumanCardShowsItsQuestion(t *testing.T) {
	r := newRig(t)
	const ask = "Which store keeps the snapshots of the nightly export once the shared Postgres runs out of disk?"
	hq := humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "say:Postgres", Label: "Postgres"}, factoryd.QuestionChoice{Value: "say:SQLite", Label: "SQLite"})
	hq.Objective, hq.Question.Ask = ask+"\n\nWhy: Two stores fit.", ask
	r.tasks.tasks = []factoryd.BoardTask{hq, agentEndsAsk()}
	r.openCases = map[string][]factoryd.Case{
		"t-hq": {{ID: 1, TaskID: "t-hq", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman, Problem: ask}},
		"t-a":  {{ID: 2, TaskID: "t-a", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman, Problem: "Use a replica?"}},
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	esc := escalationOf(t, body, "t-hq")
	for _, want := range []string{`name="value" value="say:Postgres"`, `aria-label="SQLite, posted on the issue as you"`} {
		if !strings.Contains(esc, want) {
			t.Errorf("the human card lacks %q:\n%s", want, esc)
		}
	}
	// the ask is the card's title: the row shows it once, as its ask part
	if strings.Count(esc, ask) != 1 || !strings.Contains(esc, `<p class="ask">`+ask+`</p>`) || strings.Contains(esc, `class="case-title"`) {
		t.Errorf("the human card shows its ask other than once, as the ask part:\n%s", esc)
	}
	if face := faceOf(t, body, "t-hq"); !strings.Contains(face, `action="/tasks/t-hq/close"`) {
		t.Errorf("the face lost the card's Close:\n%s", face)
	}
	if agent := escalationOf(t, body, "t-a"); !strings.Contains(agent, `<p class="ask">Use a replica?</p>`) {
		t.Errorf("the agent card lost its ask line:\n%s", agent)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-hq/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{ask, "Two stores fit.", "Postgres is shared, SQLite is local.", "Recommendation (the agent): Postgres"} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %q:\n%s", want, drawer)
		}
	}
	if strings.Contains(drawer, `<p class="ask">`) {
		t.Errorf("the drawer repeats the ask below its head:\n%s", drawer)
	}
	if agent := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-a/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(agent, `<p class="ask">Use a replica?</p>`) {
		t.Errorf("the agent card's drawer lost its ask line:\n%s", agent)
	}
}

// A human card's go and close choices take it off the board and post to
// the answer route, close in the deny style; park keeps it.
func TestAHumanCardChoicesLeaveTheBoardOrStay(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "go", Label: "Have the factory do it"},
		factoryd.QuestionChoice{Value: "park", Label: "Wait for it"}, factoryd.QuestionChoice{Value: "close", Label: "Done"})}
	ck, _ := r.loggedIn(t)
	esc := escalationOf(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "t-hq")
	if strings.Count(esc, `class="inline" data-to="off">`) != 2 || strings.Contains(esc, `action="/tasks/t-hq/close"`) {
		t.Errorf("want go and close to leave the board through the answer route and park to stay:\n%s", esc)
	}
	if !strings.Contains(esc, `<button type="submit" class="deny" aria-label="Done, posted on the issue as you">Done</button>`) {
		t.Errorf("the Done choice lost its deny style:\n%s", esc)
	}
}

func TestAQuestionWithoutACloseKeepsTheClose(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hp", 52, factoryd.QuestionChoice{Value: "park", Label: "Wait for it"})}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if face := faceOf(t, body, "t-hp"); !strings.Contains(face, `action="/tasks/t-hp/close"`) {
		t.Errorf("the face lost the card's Close:\n%s", face)
	}
}

// What an ask-less card still offers: an agent card's escalation shows no
// reason and no answer box; a question-less human card that asks, an old
// row, keeps its answer form; one that asks nothing offers no answer and
// its plain Close; a close with a note posts it and records the close.
func TestAskLessAndQuestionLessCardsKeepTheirForms(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-wait", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 31, Objective: "Bump the weekly numbers."}},
		{Task: factoryd.Task{ID: "t-ask", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 33, Objective: "Pick the store."},
			Escalation: "The card asks: which store keeps the snapshots?"},
		{Task: factoryd.Task{ID: "t-agent", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 34, Objective: "the feed"}},
	}
	ck, s := r.loggedIn(t)
	for _, page := range []string{"/?project=ai-software-factory-code&view=wide", "/?project=ai-software-factory-code"} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		if esc := escalationOf(t, body, "t-agent"); strings.Contains(esc, `name="answer"`) || strings.Contains(esc, `<p class="reason">`) {
			t.Errorf("%s: the ask-less agent card still answers:\n%s", page, esc)
		}
		if face := faceOf(t, body, "t-wait"); !strings.Contains(face, `>Close</button>`) {
			t.Errorf("%s: the question-less card lost its plain Close:\n%s", page, face)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-wait/drawer?tab=overview", nil), ck).Body.String()
	if strings.Contains(drawer, `name="answer"`) {
		t.Errorf("the ask-less card's drawer offers an answer:\n%s", drawer)
	}
	askDrawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-ask/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(askDrawer, `<label for="answer-t-ask-d">Your answer, posted on the issue as you</label>`) {
		t.Errorf("the old asking card's drawer lost its answer form:\n%s", askDrawer)
	}

	before := ev.count()
	rec := r.do(post("/tasks/t-wait/close", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "note": {"Not this week."}}), ck)
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

// A plan that filed its cards ends done as a compact row; a named plan's
// row carries the same chip as its live face, an unnamed plan's row none.
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
	if !strings.Contains(face, "face final") || !strings.Contains(face, `<span class="chip plan">#1 · fix prod latency</span>`) {
		t.Errorf("a named plan's done row lacks its chip:\n%s", face)
	}
	r.tasks.tasks[0].PlanName = ""
	if face := faceOf(t, get(), "t-plan"); strings.Contains(face, "chip plan") {
		t.Errorf("an unnamed plan's done row carries a chip:\n%s", face)
	}
	r.tasks.tasks[0].PlanName, r.tasks.tasks[0].IssueNumber = "fix prod latency", 0
	if face := faceOf(t, get(), "t-plan"); !strings.Contains(face, `<span class="chip plan">fix prod latency</span>`) || strings.Contains(face, "#0") {
		t.Errorf("a named plan without an issue number lacks the bare name or shows #0:\n%s", face)
	}
}

// A human card whose question has choices is answered by pressing one: its
// drawer offers no free-text box, which doHumanAnswer would refuse. A card
// whose question has none keeps the box.
func TestHumanCardOffersTheAnswerBoxOnlyWithoutChoices(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{
		humanAsk("t-pick", 51, factoryd.QuestionChoice{Value: "park", Label: "Wait for it"}, factoryd.QuestionChoice{Value: "close", Label: "Done"}),
		humanAsk("t-free", 52),
	}
	ck, _ := r.loggedIn(t)
	for id, want := range map[string]bool{"t-pick": false, "t-free": true} {
		drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
		if got := strings.Contains(drawer, `name="answer"`); got != want {
			t.Errorf("%s: the drawer's answer box = %v, want %v:\n%s", id, got, want, drawer)
		}
	}
}
