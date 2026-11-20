package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// A card whose merge approval pends speaks for itself in Needs you: the
// four-column view does not also hold it in In progress, where it read as
// still verifying while only its approval had moved on. The plans yield
// the same way.
func TestMergeApprovalYieldsTheCardInNeedsYou(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 7, PRNumber: 9})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(col(t, body, "In progress"), `data-card="t-m"`) {
		t.Fatal("In progress holds a card whose merge approval speaks for it in Needs you")
	}
	if !strings.Contains(col(t, body, "Needs you"), `data-card="t-m"`) {
		t.Fatal("Needs you holds no approval for the card")
	}
}

// factoryd's own human card waits out of sight for its face: in
// waiting-agent, its case climbing, it is in no row of Needs you and adds
// nothing to the figure, and In progress holds it as working. At its face,
// waiting on a human on its issue with its case at the human, it is one row
// of Needs you, counted once.
func TestAHeldHumanCardWaitsInProgressUntilItsFace(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.set("", factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-agent", Repository: "linkshort/linkshort", Source: factoryd.SourceChainStall,
		Objective: "The chain is pending. Re-run it?"})
	r.openCases = map[string][]factoryd.Case{"t-h": {{ID: 51, TaskID: "t-h", Repository: "linkshort/linkshort", Kind: factoryd.CaseHumanCard,
		Status: factoryd.CaseOpen, OwnerRung: 2, Problem: "The chain is pending. Re-run it?"}}}
	ck, _ := r.loggedIn(t)
	board := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	}
	body := board()
	if needs := col(t, body, "Needs you"); strings.Contains(needs, `data-card="t-h"`) {
		t.Fatalf("Needs you lists the card while its case climbs:\n%s", needs)
	}
	if !strings.Contains(col(t, body, "In progress"), `data-card="t-h"`) {
		t.Fatal("In progress lacks the card whose case climbs")
	}
	// its state reads in words, never as the status's code word
	if small := faceOf(t, body, "t-h"); !strings.Contains(small, "with the factory agent") || strings.Contains(small, ">waiting-agent<") {
		t.Fatalf("the climbing card's face does not say it is with the factory agent:\n%s", small)
	}
	// and its opened card asks a human nothing yet: the card's own actions stand
	if opened := drawer(t, r, ck, "t-h", "overview"); strings.Contains(opened, `<section class="o-ask"`) || !strings.Contains(opened, `data-answer="close"`) {
		t.Fatalf("the opened card asks a human while its case climbs, or lacks the card's end:\n%s", opened)
	}
	for _, want := range []string{`<b class="n">0</b> need you`, `<b class="n">1</b> working`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the status line lacks %q while the case climbs", want)
		}
	}
	r.tasks.set("", factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "linkshort/linkshort", Source: factoryd.SourceChainStall,
		IssueNumber: 33, Objective: "The chain is pending. Re-run it?"})
	r.openCases["t-h"][0].Status = factoryd.CaseAtHuman
	body = board()
	if !strings.Contains(col(t, body, "Needs you"), `data-card="t-h"`) || strings.Contains(col(t, body, "In progress"), `data-card="t-h"`) {
		t.Fatalf("at its face the card is not in Needs you alone:\n%s", body)
	}
	if !strings.Contains(body, `<b class="n">1</b> need you`) {
		t.Fatal("the status line does not count the card at its face once")
	}
}

// The opened merge card: its full brief holds each why-a-human entry, and
// an agent pull request on red checks asks its return and close as the
// case's own answers, with no merge.
func TestMergeDrawerRendersTheAskAndItsActions(t *testing.T) {
	r := newRig(t)
	ap := factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-red", Project: "sf/code", PRNumber: 10, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal","red_checks":true,"why_human":["checks round 1 failed","review round 2 said the same"]}`)}
	r.approvals.rows = append(r.approvals.rows, ap)
	r.tasks.set("", factoryd.Task{ID: "t-red", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	k, err := factoryd.RequestCase(ap)
	if err != nil {
		t.Fatal(err)
	}
	k.ID, k.Status = 8, factoryd.CaseAtHuman
	r.openCases = map[string][]factoryd.Case{"t-red": {k}}
	ck, _ := r.loggedIn(t)
	body := drawer(t, r, ck, "t-red", "overview")
	if !strings.Contains(body, "<dt>Why a human</dt><dd><ul><li>checks round 1 failed</li><li>review round 2 said the same</li></ul></dd>") {
		t.Errorf("the full brief lacks the why-a-human entries:\n%s", body)
	}
	for _, key := range []string{factoryd.ActDenyMerge, factoryd.ActDenyMerge + "/reject"} {
		if f := answerForm(t, body, key); f.Action != "/tasks/t-red/resolve" || f.Values.Get("case_id") != "8" || f.Values.Get("action") != key {
			t.Errorf("the %s answer posts %s %v, want the case's resolve", key, f.Action, f.Values)
		}
	}
	if strings.Contains(askBlockOf(t, body), "Merge it") {
		t.Errorf("the red pull request's ask offers the merge:\n%s", askBlockOf(t, body))
	}
}

// The drawer's merge card of a plan's integration card shows each opened
// proof of the plan: the original, and beside it the pull request's
// version at the request's head when the two differ. No stored proof
// reaches it, a feature card's shows none, and so does the board's
// column copy of the card.
func TestDrawerShowsTheOpenedProofBesideTheRepositorysVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  map[string]string
		fail bool
		want []string
		pres int
	}{
		{"changed", map[string]string{"8e3a1b0c acceptance/group_open_test.go": "v2 code"}, false, []string{`<h5>group_open: acceptance/group_open_test.go, changed</h5>`,
			`<pre class="proof original" tabindex="0">v1 code</pre>`, `<pre class="proof repository" tabindex="0">v2 code</pre>`}, 2},
		{"unchanged", map[string]string{"8e3a1b0c acceptance/group_open_test.go": "v1 code"}, false, []string{`<h5>group_open: acceptance/group_open_test.go, unchanged</h5>`,
			`<pre class="proof original" tabindex="0">v1 code</pre>`}, 1},
		{"not in the pull request", nil, false, []string{`<h5>group_open: acceptance/group_open_test.go, not in the pull request</h5>`,
			`<pre class="proof original" tabindex="0">v1 code</pre>`}, 1},
		{"unread", nil, true, []string{`<p class="problem">The console could not read the opened proofs. Reload the board.</p>`}, 0},
		{"a feature card", map[string]string{"8e3a1b0c acceptance/group_open_test.go": "v2 code"}, false, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			plan, _ := json.Marshal(factoryd.Plan{Summary: "The opening hours", Cards: []factoryd.PlanCard{
				{Key: "hours", Role: factoryd.RoleFeature, Title: "Hours"},
				{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integration"},
			}})
			r.tasks.set("",
				factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "Opening hours", Repository: "sf/code", IssueNumber: 3},
				factoryd.Task{ID: "t-int", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 12, Parent: "t-plan",
					Source: factoryd.SourcePlan, PlanKey: map[bool]string{true: "hours", false: "integration"}[tc.name == "a feature card"], PlanRevision: 1, PRNumber: 14})
			r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}}
			r.tasks.proofs = map[string][]factoryd.PlanProof{"t-plan": {
				{Idx: 1, Origin: factoryd.ProofOriginCriterion, Criterion: "size_bound", ID: "size_bound", Path: "proofs/size_bound/size_bound_test.go",
					Code: "package size_bound", State: factoryd.ProofStored},
				{Idx: 2, Origin: factoryd.ProofOriginCriterion, Criterion: "group_open", ID: "group_open", Path: "acceptance/group_open_test.go",
					Code: "v1 code", State: factoryd.ProofOpened},
			}}
			r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 41, Kind: factoryd.ApprovalMerge, TaskID: "t-int",
				Project: "sf/code", PRNumber: 14, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal","head_sha":"8e3a1b0c"}`)})
			r.fg.rawAt = tc.raw
			ck, _ := r.loggedIn(t)
			r.fg.failAPI = tc.fail
			body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-int/drawer?tab=overview", nil), ck).Body.String()
			r.fg.failAPI = false
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Fatalf("the drawer lacks %s:\n%s", w, body)
				}
			}
			if got := strings.Count(body, `<pre class="proof`); got != tc.pres || strings.Contains(body, "size_bound") {
				t.Fatalf("the drawer holds %d proof blocks, want %d, and no stored proof:\n%s", got, tc.pres, body)
			}
			board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
			if !strings.Contains(board, `data-card="t-int"`) || strings.Contains(board, `class="proof`) {
				t.Fatal("the board's column copy of the merge card shows an opened proof, or no card")
			}
		})
	}
}

// The opened card's head reads the card's state like the face's: a merged
// card's says its chain's state, a done card's says done, a closed card's
// says why it closed when it stored why: a reason that says "closed …"
// alone, any other after "closed: ".
func TestDrawerPillReadsTheState(t *testing.T) {
	r := newRig(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-merged", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 7, PRNumber: 9},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 8, PRNumber: 10},
		factoryd.Task{ID: "t-closed", Kind: "agent", Status: "closed", Repository: "sf/code", IssueNumber: 11, ClosingReason: "closed by sebastian: Superseded."},
		factoryd.Task{ID: "t-forge", Kind: "agent", Status: "closed", Repository: "sf/code", IssueNumber: 13, ClosingReason: "closed in Forgejo by alice"},
		factoryd.Task{ID: "t-cleared", Kind: "human", Status: "closed", Repository: "sf/code", IssueNumber: 14, ClosingReason: "The alert cleared at 2026-11-19T22:35:56Z."},
		factoryd.Task{ID: "t-replan", Kind: "agent", Status: "closed", Repository: "sf/code", IssueNumber: 15, ClosingReason: "stopped for the replan of the plan"},
		factoryd.Task{ID: "t-down", Kind: "agent", Status: "closed", Repository: "sf/code", IssueNumber: 16, ClosingReason: "closedown of the service"},
		factoryd.Task{ID: "t-shut", Kind: "agent", Status: "closed", Repository: "sf/code", IssueNumber: 12})
	ck, _ := r.loggedIn(t)
	for id, want := range map[string]string{
		"t-merged":  `<p class="o-line"><span class="it"><span class="st st-merged"><i class="dot" aria-hidden="true"></i>waiting for the chain</span></span>`,
		"t-done":    `<p class="o-line"><span class="it st">done</span>`,
		"t-closed":  `<p class="o-line"><span class="it st">closed by sebastian: Superseded.</span>`,
		"t-forge":   `<p class="o-line"><span class="it st">closed in Forgejo by alice</span>`,
		"t-cleared": `<p class="o-line"><span class="it st">closed: The alert cleared at 2026-11-19T22:35:56Z.</span>`,
		"t-replan":  `<p class="o-line"><span class="it st">closed: stopped for the replan of the plan</span>`,
		"t-down":    `<p class="o-line"><span class="it st">closed: closedown of the service</span>`,
		"t-shut":    `<p class="o-line"><span class="it st">closed</span>`,
	} {
		body := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("%s's drawer lacks %s:\n%s", id, want, body)
		}
	}
}

// The merged card's chain word wears the chain's own hue: red where the
// chain failed, amber while it stalls, the merged teal while it runs —
// never green words for a red chain.
func TestAMergedCardWearsItsChainState(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	card := func(id, state string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "merged", Repository: "sf/code",
			PRNumber: 9, Objective: id + " change"}, ChainState: state}
	}
	r.tasks.tasks = []factoryd.BoardTask{card("t-red", "failure"), card("t-run", "running"), card("t-stall", "stalled")}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	progress := col(t, body, "In progress")
	for id, word := range map[string]string{
		"t-red":   `<span class="st st-failed"><i class="dot" aria-hidden="true"></i>chain red</span>`,
		"t-run":   `<span class="st st-merged"><i class="dot" aria-hidden="true"></i>chain running</span>`,
		"t-stall": `<span class="st st-waiting"><i class="dot" aria-hidden="true"></i>stalled</span>`,
	} {
		if face := faceOf(t, progress, id); !strings.Contains(face, word) {
			t.Errorf("In progress lacks %s wearing %s:\n%s", id, word, face)
		}
	}
}

// A merged card under an open roll reads "blocked by roll #N", the words
// linking the roll's issue, ahead of what the chain's own row says; one
// with no roll keeps the chain's words.
func TestAMergedCardNamesItsRoll(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	card := func(id string, roll int64, state string) factoryd.BoardTask {
		b := factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "merged", Repository: "sf/code",
			PRNumber: 9, Objective: id + " change"}, ChainState: state, RollIssue: roll}
		if roll > 0 {
			b.RollID = "t-r"
		}
		return b
	}
	r.tasks.tasks = []factoryd.BoardTask{card("t-roll", 1162, "failure"), card("t-none", 0, "failure")}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	progress := col(t, body, "In progress")
	want := `<span class="st st-merged"><i class="dot" aria-hidden="true"></i><a href="#card=t-r">blocked by roll #1162</a></span>`
	if !strings.Contains(progress, want) {
		t.Errorf("In progress lacks %s:\n%s", want, progress)
	}
	if strings.Count(progress, "blocked by roll") != 1 || !strings.Contains(faceOf(t, progress, "t-none"), `<span class="st st-failed"><i class="dot" aria-hidden="true"></i>chain red</span>`) {
		t.Errorf("the card with no roll must read chain red, and only one card names a roll:\n%s", progress)
	}
}

func TestDeployStageWords(t *testing.T) {
	for _, c := range []struct {
		state string
		roll  int64
		run   int64
		want  string
	}{
		{"failure", 5, 3252, "blocked by roll #5"}, {"failure", 0, 3252, "run 3252 red"}, {"cancelled", 0, 0, "chain red"},
		{"stalled", 0, 3253, "run 3253 stalled"}, {"stalled", 0, 0, "stalled"}, {"", 0, 0, "waiting for the chain"},
		{"pending", 0, 3251, "chain run 3251"}, {"pending", 0, 0, "chain running"},
	} {
		if got := deployStage(c.state, c.roll, c.run); got != c.want {
			t.Errorf("deployStage(%q, %d, %d) = %q, want %q", c.state, c.roll, c.run, got, c.want)
		}
	}
}

// A roll task is a card like any other: running, it sits in In progress
// under its title; its full brief lists the cards it carries, and the
// merged card's head names the roll it waits for.
func TestARollCardShowsOnTheBoard(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-roll", Kind: "agent", Source: factoryd.SourceRoll, Status: "running", Repository: "sf/code",
			IssueNumber: 1162, Objective: "roll abc1234: factoryd, gateway, worker, ci-runner"}},
		{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 7, PRNumber: 9, Objective: "feed page"}, RollID: "t-roll", RollIssue: 1162},
	}
	r.tasks.carries = map[string][]string{"t-roll": {"t-m"}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if progress := col(t, body, "In progress"); !strings.Contains(progress, `data-card="t-roll"`) || !strings.Contains(progress, "roll abc1234: factoryd, gateway, worker, ci-runner") {
		t.Fatalf("In progress lacks the roll card:\n%s", progress)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-roll/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(drawer, `<h4>It carries</h4><ul><li><a class="ref-link" href="#card=t-m">#7</a>`) {
		t.Errorf("the roll's opened card does not list the card it carries:\n%s", drawer)
	}
	drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(drawer, `<a href="#card=t-roll">blocked by roll #1162</a>`) {
		t.Errorf("the merged card's opened card does not name its roll:\n%s", drawer)
	}
}

// A filed roll task waits in Queued like any card.
func TestAFiledRollCardIsQueued(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-roll", Kind: "agent", Source: factoryd.SourceRoll, Status: "queued",
		Repository: "sf/code", IssueNumber: 1162, Objective: "roll abc1234: factoryd, gateway, worker, ci-runner"}}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if q := col(t, body, "Queued"); !strings.Contains(q, `data-card="t-roll"`) {
		t.Fatalf("Queued lacks the filed roll card:\n%s", q)
	}
}

// A card's press moves the card at once (C31): every column names the
// statuses it holds, and the approval's answer the status of its task, whose card the decided request
// leaves standing. The press lands in one row, and it answers with the
// board's refresh alone when htmx sends it; a browser's form keeps its
// redirect.
func TestCardPressAnswersWithTheBoardAlone(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-card", Kind: "agent", Status: "in-review",
		Repository: "sf/code", IssueNumber: 9, PRNumber: 9}})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 31, Kind: factoryd.ApprovalMerge, TaskID: "t-card",
		Project: "sf/code", PRNumber: 9, Status: "pending", Spec: json.RawMessage(`{"head_sha":"b7e04c19d2a3"}`)})
	var approved []string
	r.c.cfg.ApproveMerge = func(_ context.Context, taskID string, pr int64, approver string) error {
		approved = append(approved, fmt.Sprintf("%s #%d by %s", taskID, pr, approver))
		return nil
	}
	ck, s := r.loggedIn(t)
	for page, wants := range map[string][]string{
		"/?project=ai-software-factory-code": {`id="needs-you" data-holds="waiting-human waiting-poster">`,
			`data-holds="queued waiting-deps waiting-image">`, `data-holds="running closing waiting-agent in-review merged">`, `data-holds="done failed closed"`},
		"/cards/t-card/drawer?tab=overview": {
			`<form method="post" action="/tasks/t-card/resolve" hx-post="/tasks/t-card/resolve" class="answer" data-answer="approve_merge" data-to="in-review">`},
	} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %s", page, want)
			}
		}
	}
	form := askForm(s.csrf, 31, factoryd.ActApproveMerge)
	form.Set("project", "ai-software-factory-code")
	before := r.events.count()
	rec := r.do(htmxPost("/tasks/t-card/resolve", form), ck)
	if body := rec.Body.String(); rec.Code != http.StatusNoContent || body != "" || rec.Header().Get("HX-Trigger") != "board-refresh" {
		t.Fatalf("the htmx approve = %d (%q):\n%s", rec.Code, rec.Header().Get("HX-Trigger"), body)
	}
	if !slices.Equal(approved, []string{"t-card #9 by sebastian"}) {
		t.Fatalf("the htmx approve merged %v, want t-card #9 by sebastian", approved)
	}
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventMergeApproved, repo: "sf/code", taskID: "t-card", detail: map[string]any{"pr": int64(9)}})
	if rec := r.do(post("/tasks/t-card/resolve", form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code" {
		t.Fatalf("the browser's approve = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

// The merge card of the successor that carries the integration card's key
// shows the plan's opened proof as the card's own does.
func TestDrawerOfAnIntegrationSuccessorShowsTheOpenedProof(t *testing.T) {
	r := newRig(t)
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The opening hours", Cards: []factoryd.PlanCard{
		{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integration"},
	}})
	r.tasks.set("",
		factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "Opening hours", Repository: "sf/code", IssueNumber: 3},
		factoryd.Task{ID: "t-int", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 12, Parent: "t-plan",
			Source: factoryd.SourcePlan, PlanKey: "integration", PlanRevision: 1},
		factoryd.Task{ID: "t-is", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 13, Parent: "t-plan",
			Source: factoryd.SourceSuccessor, PlanKey: "integration", PlanRevision: 1, PRNumber: 14})
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}}
	r.tasks.proofs = map[string][]factoryd.PlanProof{"t-plan": {{Idx: 1, Origin: factoryd.ProofOriginCriterion, Criterion: "group_open", ID: "group_open",
		Path: "acceptance/group_open_test.go", Code: "v1 code", State: factoryd.ProofOpened}}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 41, Kind: factoryd.ApprovalMerge, TaskID: "t-is",
		Project: "sf/code", PRNumber: 14, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal","head_sha":"8e3a1b0c"}`)})
	r.fg.rawAt = map[string]string{"8e3a1b0c acceptance/group_open_test.go": "v2 code"}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-is/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(body, `<h5>group_open: acceptance/group_open_test.go, changed</h5>`) {
		t.Fatalf("the successor's drawer lacks the opened proof:\n%s", body)
	}
}

// A merge request whose summary, findings, recommendation and reasons are
// each far longer than a card: the small card and the opened card's ask
// hold the register's ask and none of the parts' tails, and the opened
// card's full brief holds the whole text.
func TestAMergeAskHoldsNoTailOfItsLongParts(t *testing.T) {
	r := newRig(t)
	pad := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "pad %d the face past every bound a card keeps. ", i)
		}
		return b.String()
	}
	deep := func(word string) string { return pad(40) + " " + word + " tail" }
	reco := "`bash check` exits 0 and one `tofu -check` block holds.\n```go\n" + deep("RECO_TAIL_MARKER") + "\n```"
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 33, Objective: "add the column"}}}
	r.approvals.rows = []factoryd.ApprovalRequest{{
		ID: 4, Kind: "merge", TaskID: "t-m", Project: "sf/code", PRNumber: 9, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal","head_sha":"abc123","verdict_summary":"the change is safe. ` + deep("SUMMARY_TAIL_MARKER") + `",` +
			`"review":{"verdict":"approve","findings":["a finding. ` + deep("FINDING_TAIL_MARKER") + `","second. ` + deep("F2") + `","third. ` + deep("F3") + `","fourth. ` + deep("F4_TAIL_MARKER") + `"]},` +
			`"recommendation":` + strconv.Quote(reco) + `,"why_human":["a human decides this merge. ` + deep("WHY_TAIL_MARKER") + `"]}`),
	}}
	ck, s := r.loggedIn(t)
	// the small card asks the register's merge, its model review's pick its
	// one press, and holds no part's tail
	small := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-m")
	if strings.Count(small, "<form") != 1 || !strings.Contains(small, `action="/tasks/t-m/resolve"`) || !strings.Contains(small, `name="action" value="approve_merge"`) {
		t.Errorf("the small card holds a form other than its pick's press:\n%s", small)
	}
	for _, tail := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER", "```", "pad 0"} {
		if strings.Contains(small, tail) {
			t.Errorf("the small card carries %s:\n%s", tail, small)
		}
	}
	// the opened card's ask holds the register's question, why, the model
	// review's pick with its verdict in one line and each answer's one line
	// of effect, and none of the parts' tails; the full brief holds them whole
	body := drawer(t, r, ck, "t-m", "overview")
	ask := askBlockOf(t, body)
	for _, want := range []string{
		`<p class="o-question">Merge “add the column”?</p>`,
		`<p class="o-askwhy">The change is of normal risk, and the factory&#39;s rules let only a human merge it.</p>`,
		`<p class="o-by">suggested by the model review: <span class="quote">It approved the change with 4 findings.</span></p>`,
		`<span class="effect">Merges the pull request.</span>`,
		`<span class="effect">Returns the pull request to its agent with your note.</span>`,
		`<span class="effect">Closes the pull request. The card ends.</span>`,
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the ask lacks %q:\n%s", want, ask)
		}
	}
	for _, tail := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER", "pad 0"} {
		if strings.Contains(ask, tail) {
			t.Errorf("the ask carries %s:\n%s", tail, ask)
		}
	}
	// one form an answer, each its own send: no fund control beside them,
	// and a note box for each answer that takes words
	for what, want := range map[string]int{`<form `: 3, `action="/tasks/t-m/resolve"`: 3, `data-answer="approve_merge"`: 1, `data-answer="deny_merge"`: 1,
		`data-answer="deny_merge/reject"`: 1, `name="fund_eur"`: 0, `<textarea`: 2} {
		if got := strings.Count(ask, what); got != want {
			t.Errorf("the ask carries %s %d times, want %d:\n%s", what, got, want, ask)
		}
	}
	for _, whole := range []string{"RECO_TAIL_MARKER", "SUMMARY_TAIL_MARKER", "FINDING_TAIL_MARKER", "WHY_TAIL_MARKER", "F4_TAIL_MARKER"} {
		if !strings.Contains(body, whole) {
			t.Errorf("the drawer lost the whole text (%s):\n%s", whole, body)
		}
	}
	// Merge it's send merges and lands one row on the card
	before := r.events.count()
	send := askForm(s.csrf, 4, factoryd.ActApproveMerge)
	send.Set("project", "ai-software-factory-code")
	if rec := r.do(post("/tasks/t-m/resolve", send), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the send = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(4); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "" {
		t.Errorf("the request = %q by %q, note %q, want approved by sebastian, no note", a.Status, a.DecidedBy, a.DecisionNote)
	}
	checkRows(t, r.events, before,
		wantRow{kind: factoryd.EventMergeApproved, repo: "sf/code", taskID: "t-m", detail: map[string]any{"pr": int64(9)}})
}

// A plan's criteria request waits for its case's face: while the case is
// open on its rungs Needs you holds no row for the plan and its opened
// card asks nothing; once the case is at the human the plan's row stands
// in Needs you, and each page of its promises carries the rungs' view and
// the answer they suggest, from the case's ask. A done plan outside the
// board's finished page is listed all the same.
func TestACriteriaRequestWaitsForItsCasesFace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		onBoard bool
	}{{"on the board", true}, {"past the finished page", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			verdictRig(r)
			if !tc.onBoard {
				// the plan, done 30 days ago, behind 51 cards finished in the last hour
				now := time.Now()
				done := now.Add(-30 * 24 * time.Hour)
				r.tasks.tasks[0].FinishedAt = &done
				for i := range 51 {
					at := now.Add(-time.Duration(i+1) * time.Minute)
					r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: fmt.Sprintf("t-f%02d", i), Kind: "agent", Status: "done",
						Repository: "sf/code", IssueNumber: int64(100 + i), Objective: "Ship it.", CreatedAt: at.Add(-time.Hour), FinishedAt: &at}})
				}
			}
			r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 41, TaskID: "t-plan", Repository: "sf/code", Kind: factoryd.CasePlanCriteria,
				Status: factoryd.CaseOpen, Problem: "criteria not met once every card of the plan ended: c2, c3",
				Facts: map[string]any{"approval": 30.0, "approval_kind": factoryd.ApprovalCriteria}}}}
			ck, _ := r.loggedIn(t)
			if got := needsOf(t, r, ck, "/?project=ai-software-factory-code"); strings.Contains(got, `data-card="t-plan"`) || strings.Contains(got, "promises") {
				t.Fatalf("Needs you lists the request while its case is open:\n%s", got)
			}
			if tc.onBoard {
				if drawer := criteriaPage(t, r, ck, ""); strings.Contains(drawer, `<section class="o-ask"`) {
					t.Fatalf("the plan's opened card asks while its case is open:\n%s", drawer)
				}
			}
			r.openCases["t-plan"][0].Status = factoryd.CaseAtHuman
			r.caseAsks = map[int64]factoryd.CaseAsk{41: {Detail: map[string]any{"info": "The factory agent found:\n\n> c3 fails on main's head.",
				"preselected": map[string]any{"action": factoryd.ActGrantCriteria}, "recommendation": "One more card fixes the suite.", "recommended_by": "the factory agent"}}}
			small := faceOf(t, needsOf(t, r, ck, "/?project=ai-software-factory-code"), "t-plan")
			if !strings.Contains(small, "Plan “Shortener” finished, but 2 of 3 promises are not met. Settle each one.") {
				t.Fatalf("the plan's row in Needs you lacks its ask once the case is at the human:\n%s", small)
			}
			if !tc.onBoard {
				return
			}
			for _, key := range []string{"", "41.2"} {
				page := criteriaPage(t, r, ck, key)
				for _, want := range []string{"c3 fails on main's head.",
					`<p class="o-by">suggested by the factory agent: <span class="quote">One more card fixes the suite.</span></p>`} {
					if !strings.Contains(page, want) {
						t.Fatalf("the page %q lacks %q once the case is at the human:\n%s", key, want, askBlockOf(t, page))
					}
				}
			}
		})
	}
}

// A merge request waits for its case's face: while the case is open on its
// rungs Needs you holds no row for the card, which stays in In progress,
// and its opened card asks nothing and offers no merge; once the case is
// at the human the card's row in Needs you asks the merge, In progress
// holds the card no more, and the opened card asks it with the rungs' view
// and the answer they suggest, from the case's ask.
func TestAMergeRequestWaitsForItsCasesFace(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 7, PRNumber: 9})
	// the case as the request opens it, open at rung 0
	k, err := factoryd.RequestCase(r.approvals.get(4))
	if err != nil || k.Status != factoryd.CaseOpen {
		t.Fatalf("the request's case = %+v, %v; want it open", k, err)
	}
	k.ID = 41
	r.openCases = map[string][]factoryd.Case{"t-m": {k}}
	ck, _ := r.loggedIn(t)
	board := func() string { return r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String() }
	body := board()
	if needs := col(t, body, "Needs you"); strings.Contains(needs, `data-card="t-m"`) || strings.Contains(needs, "/approvals/4/") {
		t.Fatalf("Needs you lists the request while its case is open:\n%s", needs)
	}
	if progress := col(t, body, "In progress"); !strings.Contains(progress, `data-card="t-m"`) {
		t.Fatalf("In progress lacks the card whose merge case is open:\n%s", progress)
	}
	if got := drawer(t, r, ck, "t-m", "overview"); strings.Contains(got, `<section class="o-ask"`) || strings.Contains(got, "/approvals/4/") {
		t.Fatalf("the opened card offers the merge while its case is open:\n%s", got)
	}
	r.openCases["t-m"][0].Status = factoryd.CaseAtHuman
	r.caseAsks = map[int64]factoryd.CaseAsk{41: {Detail: map[string]any{"info": "The factory agent found:\n\n> The column is added with its default.",
		"preselected": map[string]any{"action": factoryd.ActApproveMerge}, "recommendation": "The migration is additive.", "recommended_by": "the factory agent"}}}
	body = board()
	if small := faceOf(t, col(t, body, "Needs you"), "t-m"); !strings.Contains(small, `<strong class="title">Merge this card&#39;s pull request?</strong>`) {
		t.Fatalf("the card's row in Needs you lacks its ask once the case is at the human:\n%s", small)
	}
	if strings.Contains(col(t, body, "In progress"), `data-card="t-m"`) {
		t.Fatal("In progress holds the card whose merge case is at the human")
	}
	page := drawer(t, r, ck, "t-m", "overview")
	for _, want := range []string{"The column is added with its default.", `action="/tasks/t-m/resolve" hx-post="/tasks/t-m/resolve" class="answer suggested" data-answer="approve_merge"`,
		`<p class="o-by">suggested by the factory agent: <span class="quote">The migration is additive.</span></p>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the opened card lacks %q once the case is at the human:\n%s", want, askBlockOf(t, page))
		}
	}
}

// A plan's request waits for its revision's face: while the revision's case
// climbs, Needs you holds no row for the plan, which sits in In progress,
// and its opened card asks nothing. A case of an older revision at the
// human is no face of the newer request: the card's row is that case's,
// which asks its own question. Once a case of the request's revision is at
// the human the card's row asks that question, In progress holds the card
// no more and the opened card asks it.
func TestAPlanRequestWaitsForItsCasesFace(t *testing.T) {
	r := newRig(t)
	planRig(r)
	question := func(id int64, status string, revision float64, text string) factoryd.Case {
		return factoryd.Case{ID: id, TaskID: "t-plan", Repository: "linkshort/linkshort", Kind: factoryd.CasePlanQuestion, Status: status, OwnerRung: 2,
			Problem: text, Facts: map[string]any{"revision": revision, "index": 1.0, "question": text, "choices": []any{"Postgres", "SQLite"}}}
	}
	r.openCases = map[string][]factoryd.Case{"t-plan": {question(61, factoryd.CaseOpen, 2, "Which store?")}}
	ck, _ := r.loggedIn(t)
	board := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	}
	body := board()
	if needs := col(t, body, "Needs you"); strings.Contains(needs, `data-card="t-plan"`) {
		t.Fatalf("Needs you lists the plan while its case climbs:\n%s", needs)
	}
	if progress := col(t, body, "In progress"); !strings.Contains(progress, `data-card="t-plan"`) {
		t.Fatalf("In progress lacks the plan whose case climbs:\n%s", progress)
	}
	if got := planDrawer(t, r, ck); strings.Contains(got, `<section class="o-ask"`) {
		t.Fatalf("the opened card asks while the plan's case climbs:\n%s", got)
	}
	r.openCases["t-plan"] = append(r.openCases["t-plan"], question(60, factoryd.CaseAtHuman, 1, "Which cache?"))
	if small := faceOf(t, col(t, board(), "Needs you"), "t-plan"); !strings.Contains(small, "Which cache?") || strings.Contains(small, "Which store?") {
		t.Fatalf("Needs you: want revision 1's question and none of revision 2:\n%s", small)
	}
	r.openCases["t-plan"] = []factoryd.Case{question(61, factoryd.CaseAtHuman, 2, "Which store?")}
	body = board()
	if small := faceOf(t, col(t, body, "Needs you"), "t-plan"); !strings.Contains(small, "Which store?") {
		t.Fatalf("Needs you: want the question of revision 2 once its case is at the human:\n%s", small)
	}
	if strings.Contains(col(t, body, "In progress"), `data-card="t-plan"`) {
		t.Fatal("In progress holds the plan whose case is at the human")
	}
	if ask := askBlockOf(t, planDrawer(t, r, ck)); !strings.Contains(ask, "Which store?") || !strings.Contains(ask, `action="/tasks/t-plan/plan/answer"`) {
		t.Fatalf("the opened card lacks the plan's question once its case is at the human:\n%s", ask)
	}
}

// A merge request whose case climbs its rungs rides no other case's row:
// card t-m's monitor flag at the human is its row in Needs you, its page
// and its opened card, each with nothing of the merge.
func TestAClimbingMergeRidesNoOtherCasesRow(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 7, PRNumber: 9})
	r.openCases = map[string][]factoryd.Case{"t-m": {
		{ID: 41, TaskID: "t-m", Repository: "sf/code", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseOpen, OwnerRung: 2,
			Problem: "Pull request #9 waits for a merge approval", Facts: map[string]any{"approval": 4.0, "class": "high"}},
		{ID: 42, TaskID: "t-m", Repository: "sf/code", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman,
			Problem: "the review waits past its bound", Facts: map[string]any{"flag": "wait_over_bound"}}}}
	ck, _ := r.loggedIn(t)
	for where, path := range map[string]string{"Needs you": "/", "the case's page": "/projects/ai-software-factory-code/cards/t-m/cases/42",
		"the opened card": "/cards/t-m/drawer?tab=overview"} {
		got := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
		if where == "Needs you" {
			got = col(t, got, "Needs you")
		}
		if !strings.Contains(got, "This card has waited too long for its turn. Is that fine?") || strings.Contains(got, "/approvals/4/") || strings.Contains(got, "Merge this card") {
			t.Errorf("%s: want the flag's ask with nothing of the climbing merge:\n%s", where, got)
		}
	}
}

// A merge's, a plan's and a criteria request's answer has one door, its
// case's: the request's own door takes a resource request alone, and no
// route takes a promise's verdict or a plan's confirmation by request.
func TestTheRequestDoorTakesAResourceRequestAlone(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	withRequestCards(r)
	r.fg.calls = nil
	for path, form := range map[string]url.Values{
		"/approvals/4/approve": {"csrf": {s.csrf}},
		"/approvals/4/deny":    {"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"split it"}},
	} {
		rec := r.do(post(path, form), ck)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Open the card and press the answer there.") {
			t.Errorf("POST %s = %d %q, want 409 naming the card's ask", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/approvals/4/criterion", "/approvals/4/confirm"} {
		if rec := r.do(post(path, url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404: the route is gone", path, rec.Code)
		}
	}
	if len(r.fg.calls) != 0 || r.approvals.get(4).Status != "pending" {
		t.Errorf("a refused press acted: %q, request %s", r.fg.calls, r.approvals.get(4).Status)
	}
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther || r.approvals.get(3).Status != "approved" {
		t.Errorf("the resource request's approve = %d, request %s", rec.Code, r.approvals.get(3).Status)
	}
}
