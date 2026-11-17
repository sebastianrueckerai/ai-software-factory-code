package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A plan that is done planning while 5 of its 8 cards are queued reads "3
// of 8 cards done" on its small card, its opened card and every reference
// to it, in the hue of running work and with its why line, never "done";
// once all 8 are done it reads done.
func TestAPlanWithOpenCardsIsNotDone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	planned := time.Now().Add(-time.Hour)
	plan := factoryd.Task{ID: "t-plan", Kind: "agent", Track: policy.TrackPlan, Status: "done", Repository: "sf/code", IssueNumber: 40, PlanName: "one face",
		Objective: "Plan one face\n\nOne face for every card.", Why: "Every card shows one face.", CreatedAt: planned.Add(-time.Hour), FinishedAt: &planned}
	var p factoryd.Plan
	tasks := []factoryd.BoardTask{{Task: plan}}
	for i := range 8 {
		key := fmt.Sprintf("c%d", i+1)
		p.Cards = append(p.Cards, factoryd.PlanCard{Key: key, Title: "Build part " + key})
		k := factoryd.Task{ID: "t-" + key, Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: int64(41 + i), Source: factoryd.SourcePlan,
			Parent: plan.ID, PlanKey: key, Objective: "Build part " + key, CreatedAt: planned}
		if i < 3 {
			k.Status, k.FinishedAt = "done", &planned
		}
		tasks = append(tasks, factoryd.BoardTask{Task: k})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{plan.ID: {{TaskID: plan.ID, Revision: 1, Plan: raw, CreatedAt: planned}}}
	note := factoryd.Task{ID: "t-note", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 60, Objective: "Note the plan",
		Why: "It follows #40.", CreatedAt: planned}
	r.tasks.tasks = append(tasks, factoryd.BoardTask{Task: note})

	const state = `<span class="st st-running"><i class="dot" aria-hidden="true"></i>3 of 8 cards done</span>`
	face := smallCard(t, r, ck, "t-plan")
	if !strings.Contains(face, state) || strings.Contains(face, " final") || !strings.Contains(face, `<span class="why">Every card shows one face.</span>`) {
		t.Errorf("the plan's small card does not read 3 of 8 cards done as running work with its why line:\n%s", face)
	}
	if opened := drawer(t, r, ck, "t-plan", "overview"); !strings.Contains(opened, "3 of 8 cards done</span>") || strings.Contains(opened, ">done<") {
		t.Errorf("the plan's opened card does not read 3 of 8 cards done:\n%s", opened)
	}
	if ref := smallCard(t, r, ck, "t-note"); !strings.Contains(ref, `<span class="ref-of">(Plan one face, 3 of 8 cards done)</span>`) {
		t.Errorf("a reference to the plan does not read 3 of 8 cards done:\n%s", ref)
	}

	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].Parent == plan.ID {
			r.tasks.tasks[i].Status, r.tasks.tasks[i].FinishedAt = "done", &planned
		}
	}
	if face := smallCard(t, r, ck, "t-plan"); !strings.Contains(face, "final d-done") || !strings.Contains(face, `<span class="it st">done</span>`) {
		t.Errorf("the plan whose 8 cards are done does not read done:\n%s", face)
	}
}

// A done, a failed and a closed card offer nothing on their small card: no
// button and no form, not even the Retry the failed card of a plan takes
// on its opened card.
func TestFinishedCardsOfferNothing(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	ended := time.Now().Add(-time.Hour)
	card := func(id string, n int64, status string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: status, Repository: "sf/code", IssueNumber: n, Objective: "Build part " + id,
			Why: "The board needs it.", CreatedAt: ended.Add(-time.Hour), FinishedAt: &ended}}
	}
	failed := card("t-fail", 42, "failed")
	failed.Source, failed.Parent, failed.PlanKey = factoryd.SourcePlan, "t-plan", "c2"
	plan := card("t-plan", 40, "running")
	plan.Track, plan.FinishedAt = policy.TrackPlan, nil
	r.tasks.tasks = []factoryd.BoardTask{plan, card("t-done", 41, "done"), failed, card("t-closed", 43, "closed")}
	if opened := drawer(t, r, ck, "t-fail", "overview"); !strings.Contains(otherActionsOf(t, opened), "Try the failed card again") {
		t.Fatalf("the failed card's opened card does not offer its retry:\n%s", opened)
	}
	for _, id := range []string{"t-done", "t-fail", "t-closed"} {
		face := smallCard(t, r, ck, id)
		if !strings.Contains(face, " final ") || strings.Contains(face, "<button") || strings.Contains(face, "<form") {
			t.Errorf("the finished card %s offers something on its small card:\n%s", id, face)
		}
	}
}

// The findings an ask counts stand in the opened card's full brief, one
// item each, and its hint counts them: #2004's ask counts 8 open audit
// findings, and the fold lists the 8.
func TestReviewsSectionShowsTheFindingsTheAskCites(t *testing.T) {
	_, _, _, _, body := openedEntry(t, "case", string(factoryd.CaseBudgetFloor), "")
	m := regexp.MustCompile(`(\d+) open (?:review |audit )?findings`).FindStringSubmatch(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(askBlockOf(t, body), ""))
	if m == nil {
		t.Fatalf("the ask counts no findings:\n%s", askBlockOf(t, body))
	}
	n, _ := strconv.Atoi(m[1])
	_, fold, ok := strings.Cut(body, fmt.Sprintf(`<h4>The %d open findings</h4><div class="info"><ul>`, n))
	if !ok {
		t.Fatalf("the full brief has no list of the %d findings the ask counts:\n%s", n, body)
	}
	fold, _, _ = strings.Cut(fold, "</ul>")
	if got := strings.Count(fold, "<li>"); n != 8 || got != n {
		t.Errorf("the ask counts %d findings and the fold lists %d, want 8 and 8:\n%s", n, got, fold)
	}
	if !strings.Contains(fold, "<li>test-only credential at acceptance/pool_gateway_rig_test.go:41</li>") {
		t.Errorf("the fold lacks the eighth audit finding:\n%s", fold)
	}
	if !strings.Contains(body, `<summary>The full brief<span class="hint">8 open findings, the evidence</span>`) {
		t.Errorf("the full brief's hint does not say it holds the 8 findings:\n%s", body)
	}
}

// A finding's text reaches the page only through the face's scrubber: a
// token it quotes is masked and a link to a host the policy does not
// allow is defanged, on the small card and on the opened card alike, in
// the list of findings and in the box that clears one.
func TestFindingsPassTheFaceScrubber(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	const token, outside = "abc123def456ghi789jkl012mno345", "https://evil.example/log"
	texts := []string{"the client sends Authorization: Bearer " + token, "the [log](" + outside + ") shows it"}
	facts := roomyFacts(func(f *factoryd.CaseFacts) {
		f.Head, f.FindingTexts = "c0ffee1", map[string]string{}
		for _, text := range texts {
			id := factoryd.FindingID(text)
			f.Findings, f.AuditFindings, f.FindingOrder, f.FindingTexts[id] = append(f.Findings, id), append(f.AuditFindings, id), append(f.FindingOrder, id), text
		}
	})
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-f", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 41,
		Objective: "Import a snapshot back", BudgetEUR: 2, CreatedAt: time.Now()}, AskCase: 1}}
	seatAsk(r, "t-f", factoryd.CaseSessionCap, facts)
	opened := drawer(t, r, ck, "t-f", "overview")
	if !strings.Contains(opened, "<h4>The 2 open findings</h4>") {
		t.Fatalf("the opened card lists no findings:\n%s", opened)
	}
	for name, page := range map[string]string{"the small card": smallCard(t, r, ck, "t-f"), "the opened card": opened} {
		if strings.Contains(page, token) || strings.Contains(page, outside) || strings.Contains(page, `href="https://evil`) {
			t.Errorf("%s shows the token or a link to an outside host:\n%s", name, page)
		}
	}
	// the box that clears a finding posts the part before the token, a
	// prefix the audit's confirmation still names the finding by
	if !strings.Contains(opened, `name="confirm_audit.findings" value="the client sends Authorization: Bearer">`) {
		t.Errorf("the box of the finding with the token posts no prefix of it:\n%s", opened)
	}
}

// A plan whose cards could not be read does not read done: its state says
// only that its planning ended, and the board says what it could not read.
func TestAPlanWhoseCardsCannotBeReadIsNotDone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	planned := time.Now().Add(-time.Hour)
	plan := factoryd.Task{ID: "t-plan", Kind: "agent", Track: policy.TrackPlan, Status: "done", Repository: "sf/code", IssueNumber: 40,
		Objective: "Plan one face", Why: "Every card shows one face.", CreatedAt: planned, FinishedAt: &planned}
	raw, err := json.Marshal(factoryd.Plan{Cards: []factoryd.PlanCard{{Key: "c1", Title: "Build part c1"}}})
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{plan.ID: {{TaskID: plan.ID, Revision: 1, Plan: raw, CreatedAt: planned}}}
	r.tasks.tasks = []factoryd.BoardTask{{Task: plan}}
	r.c.cfg.Tasks = unreadPlanCards{r.tasks}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project="+labCodeProject.Name, nil), ck).Body.String()
	face := faceOf(t, body, "t-plan")
	if strings.Contains(face, ">done<") || strings.Contains(face, " final") || !strings.Contains(face, "planned</span>") {
		t.Errorf("the plan whose cards could not be read reads done:\n%s", face)
	}
	if !strings.Contains(body, "the cards of plan t-plan could not be read") {
		t.Errorf("the board does not say it could not read the plan's cards")
	}
}

// unreadPlanCards is the rig's store whose read of a plan's cards fails.
type unreadPlanCards struct{ *fakeTasks }

func (unreadPlanCards) PlanCards(context.Context, string) ([]factoryd.Task, error) {
	return nil, errors.New("the store did not answer")
}

// A card's text that names a pull request names its live state: #40
// closed without a merge reads so in the card's full brief, whose text is
// markdown, as in its why line.
func TestRefsShowLiveState(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.fg.issueReads = map[string]map[string]any{
		"/api/v1/repos/sf/code/issues/40": {"title": "Try the cache", "state": "closed", "pull_request": map[string]any{"merged": false}},
	}
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-r", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 41,
		Objective: "Carry the cache on\n\nThe work sits on pull request #40. Keep `#40` in code.", Why: "It carries #40 on.", CreatedAt: time.Now()}}}
	body := drawer(t, r, ck, "t-r", "overview")
	const pr = `<a class="ref-link" href="https://git.example/sf/code/pulls/40" target="_blank" rel="noopener">#40</a> <span class="ref-of">(Try the cache, closed without a merge)</span>`
	_, brief, ok := strings.Cut(body, `data-sec="brief"`)
	if !ok || !strings.Contains(brief, "on pull request "+pr+".") || !strings.Contains(brief, "<code>#40</code>") {
		t.Errorf("the full brief does not name #40's live state, or links code:\n%s", brief)
	}
	if !strings.Contains(body, "It carries "+pr+" on.") {
		t.Errorf("the why line does not name #40's live state:\n%s", body)
	}
}

// A human's work card that waits for another card says so in its why
// line, #2100 a link with its live state, and "I did it" stands nowhere
// until #2100 is done: not on the small card, not on the opened card.
// A card it waits for that has no issue yet holds it back too. The why
// line then gives the card's own reason again.
func TestHumanCardSaysWhatItWaitsFor(t *testing.T) {
	all := facefixtures.All()
	i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool { return fx.Task.IssueNumber == 2101 && fx.Case != nil })
	if i < 0 {
		t.Fatal("no fixture of #2101")
	}
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	s := galleryScene()
	s.seat(all[i])
	s.drawer = all[i].Task.ID
	s.tasks[0].Dependencies = []factoryd.DepFact{{TaskID: "t-2100", Issue: 2100, Status: "queued"}}
	s.tasks = append(s.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-2100", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 2100,
		Objective: "Narrow the close refusal", CreatedAt: time.Now()}})
	id := r.seedScene(s)

	const wait = `<span class="why">It waits for <a class="ref-link" href="#card=t-2100">#2100</a> <span class="ref-of">(Narrow the close refusal, queued)</span> to be done first.</span>`
	face, opened := smallCard(t, r, ck, id), drawer(t, r, ck, id, "overview")
	if !strings.Contains(face, wait) {
		t.Errorf("the small card's why line does not say it waits for #2100:\n%s", face)
	}
	for name, page := range map[string]string{"the small card": face, "the opened card": opened} {
		if strings.Contains(page, "I did it") {
			t.Errorf("%s offers I did it while #2100 is not done:\n%s", name, page)
		}
	}
	if !strings.Contains(askBlockOf(t, opened), ">Not now</button>") {
		t.Errorf("the opened card offers no way to wait:\n%s", askBlockOf(t, opened))
	}

	// a card it waits for that has no issue yet still holds "I did it" back
	r.tasks.mu.Lock()
	r.tasks.tasks[0].Dependencies = []factoryd.DepFact{{TaskID: "t-unfiled", Status: "queued"}}
	r.tasks.mu.Unlock()
	face, opened = smallCard(t, r, ck, id), drawer(t, r, ck, id, "overview")
	if !strings.Contains(face, `<span class="why">It waits for another card to be done first.</span>`) || strings.Contains(opened, "I did it") {
		t.Errorf("a card waiting for an unfiled card does not say so, or offers I did it:\n%s", face)
	}

	r.tasks.mu.Lock()
	r.tasks.tasks[0].Dependencies = nil
	r.tasks.tasks[1].Status = "done"
	r.tasks.mu.Unlock()
	face, opened = smallCard(t, r, ck, id), drawer(t, r, ck, id, "overview")
	if strings.Contains(face, "It waits for") || !strings.Contains(face, "keeps agents out of workflow files") {
		t.Errorf("once #2100 is done the why line does not give the card's reason:\n%s", face)
	}
	if !strings.Contains(askBlockOf(t, opened), ">I did it</button>") {
		t.Errorf("once #2100 is done the opened card does not offer I did it:\n%s", askBlockOf(t, opened))
	}
}

// The find matches what a face shows: a card in Needs you shows its ask,
// so a word of the ask finds it; a word of its title alone finds it too,
// and its face then shows the title, its #N a link as on every card text,
// so the reader sees why it is there. A word only its escalation's later
// lines hold, and its status key "waiting-human", which no face shows,
// find nothing.
func TestFindMatchesWhatTheFaceShows(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-esc", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 12,
		Objective: "Pick the snapshot store of #13", CreatedAt: time.Now()}, Escalation: factoryd.AgentAsks + "which region holds the bucket?\nthe quokka note", EscalatedAt: time.Now(), AskCase: 1},
		{Task: factoryd.Task{ID: "t-13", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 13, Objective: "Keep the snapshots", CreatedAt: time.Now()}}}
	find := func(q string) string {
		return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project="+labCodeProject.Name+"&q="+url.QueryEscape(q), nil), ck).Body.String(), "Needs you")
	}
	if got := find("region"); !strings.Contains(got, `data-card="t-esc"`) || strings.Contains(got, `class="found"`) {
		t.Errorf("a word of the ask does not find the card alone:\n%s", got)
	}
	if got := find("snapshot store"); !strings.Contains(got, `<p class="found">Pick the snapshot store of <a class="ref-link" href="#card=t-13">#13</a> <span class="ref-of">(Keep the snapshots, queued)</span></p>`) {
		t.Errorf("a word of the title finds no card that shows its title, its #13 a link:\n%s", got)
	}
	for _, q := range []string{"quokka", "waiting-human"} {
		if got := find(q); strings.Contains(got, `data-card="t-esc"`) {
			t.Errorf("%q, which no face shows, finds the card:\n%s", q, got)
		}
	}
}

// A card whose helper task works reads no "stale": the sizing filed 3 min
// ago is the card's activity, though the card's own newest event is two
// hours old. A helper as quiet as its card leaves the mark.
func TestAHelperAtWorkKeepsItsCardFresh(t *testing.T) {
	r, ck, _ := helperRig(t)
	if face := smallCard(t, r, ck, "t-c"); strings.Contains(face, "stale") {
		t.Errorf("the card whose sizing works reads stale:\n%s", face)
	}
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-size" {
			r.tasks.tasks[i].CreatedAt = time.Now().Add(-2 * time.Hour)
		}
	}
	r.tasks.mu.Unlock()
	if face := smallCard(t, r, ck, "t-c"); !strings.Contains(face, "stale") {
		t.Errorf("the card whose sizing is quiet for two hours does not read stale:\n%s", face)
	}
}
