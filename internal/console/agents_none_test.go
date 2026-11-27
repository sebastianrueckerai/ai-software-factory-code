package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const infraProject = "ai-software-factory-infra"

// infraPR seats the fixtures' factory pull request card on the infra
// project, which runs no agents: its case waits at the human with Merge it
// and Reject and close it, and pull request #14's head is abc1234.
// sebastian writes sf/infra and approves it, as he does sf/code.
func infraPR(t *testing.T) (*rig, *http.Cookie, *session, factoryd.Task) {
	t.Helper()
	all := facefixtures.All()
	i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool {
		return fx.Group == facefixtures.GroupCard && fx.Kind == "factory pull request on a project without agents"
	})
	if i < 0 {
		t.Fatal("no factory pull request fixture")
	}
	fx := all[i]
	fx.Task.Repository = "sf/infra"
	k := *fx.Case
	k.Repository = "sf/infra"
	fx.Case = &k
	s := fixtureScene(fx, all)
	s.project = labProject{infraProject, "sf/infra", "sebastian"}
	s.config = &policy.ProjectConfig{Repository: "sf/infra", MergeRule: policy.MergeHuman, Objects: policy.ObjectsSeed, Agents: policy.AgentsNone}
	r := newRig(t)
	ck, sess := r.loggedIn(t)
	r.seedScene(s)
	r.fg.setPullHead("/api/v1/repos/sf/infra/pulls/14", "abc1234")
	return r, ck, sess, fx.Task
}

// A project that runs no agents has its board like any project: the
// select offers it beside the code project, its status line counts the
// card whose case waits on the human, the card stands in Needs you, and
// the opened card offers the case's Merge it and Reject and close it to an
// approver of the repository.
func TestAProjectWithoutAgentsHasItsBoard(t *testing.T) {
	r, ck, _, card := infraPR(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/?project="+infraProject, nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /?project=%s = %d: %s", infraProject, rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`<select id="project-filter" name="project">
<option value="ai-software-factory-code">ai-software-factory-code</option>
<option value="ai-software-factory-infra" selected>ai-software-factory-infra</option>
<option value="demo-app">demo-app</option>
</select>`, `data-project="ai-software-factory-infra"`, `<b class="n">1</b> need you`} {
		if !strings.Contains(body, want) {
			t.Errorf("the infra board lacks %s", want)
		}
	}
	if needs := col(t, body, "Needs you"); !strings.Contains(needs, `data-card="`+card.ID+`"`) {
		t.Errorf("Needs you lacks the card %s:\n%s", card.ID, needs)
	}
	opened := drawer(t, r, ck, card.ID, "overview")
	for key, label := range map[string]string{"choice:" + factoryd.ActApproveMerge: "Merge it", "choice:" + factoryd.ActDenyMerge: "Reject and close it"} {
		if f := answerForm(t, opened, key); f.Label != label {
			t.Errorf("the opened card's %s is labelled %q, want %q", key, f.Label, label)
		}
	}
}

// The card fragment names the infra project, read without a filter as
// the stream's swap reads it, and the case's own page answers there, so
// the carousel's arrows read the card's cases from its board: a monitor's
// flag faced after the card's ask is its second page.
func TestTheCasesOfAProjectWithoutAgentsPageOnItsBoard(t *testing.T) {
	r, ck, _, card := infraPR(t)
	k := r.openCases[card.ID][0]
	flag := factoryd.Case{ID: k.ID + 1, TaskID: card.ID, Repository: "sf/infra", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"flag": "wait_over_bound"}, OpenedAt: k.OpenedAt.Add(time.Minute)}
	r.openCases[card.ID] = append(r.openCases[card.ID], flag)
	next := fmt.Sprintf(`/projects/%s/cards/%s/cases/%d?project=%s`, infraProject, card.ID, flag.ID, infraProject)
	if frag := getAs(r, ck, "/cards/"+card.ID, true).Body.String(); !strings.Contains(frag, `hx-get="`+next+`"`) {
		t.Fatalf("the card fragment's arrow does not read %s:\n%s", next, frag)
	}
	if rec := getAs(r, ck, next, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "2 of 2") {
		t.Fatalf("GET %s = %d, want the case's page 2 of 2:\n%s", next, rec.Code, rec.Body)
	}
}

// Merge it and Reject and close it on the infra board take the path a
// press on the code project's board takes: Act with the human's merge or
// reason, and the answer leads back to the infra board.
func TestAPressOnAProjectWithoutAgentsActs(t *testing.T) {
	const merge = "POST /api/v1/repos/sf/infra/pulls/14/merge Bearer access-1"
	r, ck, s, card := infraPR(t)
	rec := r.do(post("/tasks/"+card.ID+"/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.ActApproveMerge}, "head": {"abc1234"}, "project": {infraProject}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project="+infraProject {
		t.Fatalf("Merge it = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got, posts := actedOnce(t, r); got.TaskID != card.ID || got.Res.Action != factoryd.ActApproveMerge || !posts || !slices.Contains(r.fg.calls, merge) {
		t.Errorf("Act took %+v, Forgejo %q; want the merge as the human", got, r.fg.calls)
	}
	if back := r.do(httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil), ck); back.Code != http.StatusOK {
		t.Errorf("the board the press leads to = %d: %s", back.Code, back.Body)
	}

	r, ck, s, card = infraPR(t)
	const reason = "The runner image is not ready for this change."
	rec = r.do(post("/tasks/"+card.ID+"/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.ActDenyMerge}, "answer": {reason}, "project": {infraProject}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("Reject and close it = %d %q", rec.Code, rec.Body)
	}
	if got, _ := actedOnce(t, r); got.Res.Action != factoryd.ActDenyMerge || got.Res.Note != reason ||
		!slices.Equal(r.fg.closed, []string{"/api/v1/repos/sf/infra/pulls/14 closed"}) || slices.Contains(r.fg.calls, merge) {
		t.Errorf("Act took %+v, closed %v, calls %q; want the rejection with the reason and the pull request closed", got.Res, r.fg.closed, r.fg.calls)
	}
}

// The fold of a card's life reads the infra card as the board does.
func TestTheLifeOfACardWithoutAgentsReads(t *testing.T) {
	r, ck, _, card := infraPR(t)
	if rec := getPage(r, ck, "/tasks/"+card.ID+"/digest"); rec.Code != http.StatusOK {
		t.Fatalf("the fold of the infra card = %d, want 200:\n%s", rec.Code, rec.Body)
	}
}

// A project without agents takes no card filed from the console: no agent
// would work it, and an issue opened on its repository goes to the human
// lane. No card of it waits on another: its cards wait on a human.
func TestAProjectWithoutAgentsTakesNoFiledCardAndNoDependency(t *testing.T) {
	r, ck, s, card := infraPR(t)
	const want = "Project ai-software-factory-infra runs no agents, so no card is filed on it here. " +
		"A human opens an issue on sf/infra instead, and the issue goes to the human lane.\n"
	for _, track := range []string{policy.TrackDirect, factoryd.LabelHuman} {
		rec := r.do(post("/tasks", url.Values{"csrf": {s.csrf}, "project": {infraProject}, "title": {"Rotate the token"}, "track": {track}}), ck)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != want || len(r.fg.issues) != 0 {
			t.Errorf("filing on track %s = %d %q, %d issues; want 400 %q and none", track, rec.Code, rec.Body, len(r.fg.issues), want)
		}
	}

	other := card
	other.ID, other.IssueNumber = "t-infra-2", card.IssueNumber+1
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: other})
	r.tasks.mu.Unlock()
	_, err := r.c.setDependency(context.Background(), actor{s: s}, card.ID, other.ID, false)
	why := fmt.Sprintf("Card #%d waits on a human: an answer, an approval or a decision, not a card that has not started: one of its choices answers it (answer). Close ends it.", card.IssueNumber)
	if no := refusedWith(err); no == nil || no.status != http.StatusConflict || no.msg != why {
		t.Errorf("a dependency between infra cards: %v, want 409 %q", err, why)
	}
}

// The chat's board and find read the infra project's board; a project
// factoryd does not work on has none, and they say so.
func TestTheChatReadsTheBoardOfAProjectWithoutAgents(t *testing.T) {
	r, _, s, card := infraPR(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(t.Context(), ctxKey{}, s))
	i := slices.IndexFunc(r.projects.rows, func(p factoryd.Project) bool { return p.Name == infraProject })
	a, q := actor{s: s}, fmt.Sprintf("#%d", card.IssueNumber)
	line := fmt.Sprintf("#%d %s ", card.IssueNumber, card.ID)
	if text, err := r.c.boardText(req, r.projects.rows[i], a); err != nil || !strings.Contains(text, "Needs you (1):\n  "+line) {
		t.Errorf("the board read = %q, %v; want Needs you to list %s", text, err, line)
	}
	if found, err := r.c.foundText(req, r.projects.rows[i], a, q); err != nil || !strings.Contains(found, fmt.Sprintf("Cards matching %q (1):\n  %s", q, line)) {
		t.Errorf("the find = %q, %v; want it to list %s", found, err, line)
	}
	r.projects.rows[i].File = false
	const none = "Project ai-software-factory-infra: no board, factoryd does not work on its repository."
	if text, err := r.c.boardText(req, r.projects.rows[i], a); err != nil || text != none {
		t.Errorf("the board read of a project factoryd does not work on = %q, %v; want %q", text, err, none)
	}
	if found, err := r.c.foundText(req, r.projects.rows[i], a, q); err != nil || found != none {
		t.Errorf("the find on a project factoryd does not work on = %q, %v; want %q", found, err, none)
	}
}

// The chat header of a project without agents offers neither the intake
// switch, whose roadmap pull files cards for agents, nor the level
// control: full auto would write merge_rule auto into its file, and its
// level stays what its project file says. The code project's header
// offers both to its approver. The level's two doors, the control's
// press and the chat's set_autonomy, refuse such a project and name its
// project file.
func TestTheChatHeaderOfAProjectWithoutAgentsOffersNoAgentControl(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.repoWriters["sf/infra"] = []string{"sebastian"}
	r.forgetWriters()
	r.ledger.files = map[string]policy.ProjectConfig{
		"ai-software-factory-code": {Repository: "sf/code", MergeRule: policy.MergeHuman, Objects: policy.ObjectsSeed, Agents: policy.AgentsCoder, Process: "maintenance"},
		infraProject:               {Repository: "sf/infra", MergeRule: policy.MergeHuman, Objects: policy.ObjectsSeed, Agents: policy.AgentsNone},
	}
	ck, s := r.loggedIn(t)
	for project, offers := range map[string]bool{"ai-software-factory-code": true, infraProject: false} {
		body := r.do(httptest.NewRequest(http.MethodGet, "/?project="+project, nil), ck).Body.String()
		if !strings.Contains(body, `<span class="chat-who">project agent · `+project+`</span>`) {
			t.Errorf("%s: the board has no chat with the project agent", project)
		}
		for _, control := range []string{`id="intake-btn"`, `id="level-btn"`} {
			if strings.Contains(body, control) != offers {
				t.Errorf("%s: the chat header holds %s: %t, want %t", project, control, !offers, offers)
			}
		}
	}

	const why = "Project ai-software-factory-infra runs no agents, so its level is not set here: " +
		"change autonomy in its project file, projects/ai-software-factory-infra.yaml, by a reviewed commit."
	if rec := r.do(post("/projects/"+infraProject+"/autonomy", url.Values{"csrf": {s.csrf}, "autonomy": {policy.AutonomyFullAuto}}), ck); rec.Code != http.StatusConflict || rec.Body.String() != why+"\n" {
		t.Errorf("the level control's press = %d %q, want 409 %q", rec.Code, rec.Body, why)
	}
	tool, ok := r.c.chatTool("set_autonomy")
	if !ok {
		t.Fatal("there is no set_autonomy tool")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(t.Context(), ctxKey{}, s))
	i := slices.IndexFunc(r.projects.rows, func(p factoryd.Project) bool { return p.Name == infraProject })
	_, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/infra", via: factoryd.ViaChat, decider: "member"}, p: r.projects.rows[i]},
		toolInput{Autonomy: policy.AutonomyFullAuto})
	if no := refusedWith(err); no == nil || no.status != http.StatusConflict || no.msg != why {
		t.Errorf("the chat's set_autonomy: %v, want 409 %q", err, why)
	}
	for _, e := range r.events.all() {
		if e.Kind == factoryd.EventAutonomyChanged {
			t.Errorf("a refused level change recorded %+v", e)
		}
	}
}
