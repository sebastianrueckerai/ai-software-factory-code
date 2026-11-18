package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

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

// A merge request waits for its case's face: while the case is open on its
// rungs Needs you lists no approval for the card, which stays in In
// progress, and the card's drawer offers no merge, neither Approve nor
// Deny; once the case is at the human the card's row in Needs you is the
// case's, hosting the approval with the rungs' analysis and
// recommendation, In progress holds the card no more, and the drawer
// offers the merge.
func TestAMergeRequestWaitsForItsCasesFace(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 7, PRNumber: 9})
	r.openCases = map[string][]factoryd.Case{"t-m": {{ID: 41, TaskID: "t-m", Repository: "sf/code", Kind: factoryd.CaseMergeApproval,
		Status: factoryd.CaseOpen, Problem: "Pull request #9 waits for a merge approval", Facts: map[string]any{"approval": 4.0, "class": "high"}}}}
	ck, _ := r.loggedIn(t)
	board := func() string { return r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String() }
	body := board()
	if needs := col(t, body, "Needs you"); strings.Contains(needs, `data-card="t-m"`) || strings.Contains(needs, "approval-4") {
		t.Fatalf("Needs you lists the request while its case is open:\n%s", needs)
	}
	if progress := col(t, body, "In progress"); !strings.Contains(progress, `data-card="t-m"`) {
		t.Fatalf("In progress lacks the card whose merge case is open:\n%s", progress)
	}
	drawer := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	}
	if got := drawer(); strings.Contains(got, "/approvals/4/") || strings.Contains(got, `value="approve"`) || strings.Contains(got, `value="deny"`) {
		t.Fatalf("the drawer offers the merge while its case is open:\n%s", got)
	}
	r.openCases["t-m"][0].Status = factoryd.CaseAtHuman
	r.caseAsks = map[int64]factoryd.CaseAsk{41: {Detail: map[string]any{"info": "The factory agent found: the column is added with its default.",
		"recommendation": "approve_merge: the migration is additive", "recommended_by": "the factory agent"}}}
	body = board()
	needs := col(t, body, "Needs you")
	for _, want := range []string{`id="need-t-m"`, `aria-labelledby="approval-4"`, "The factory agent found: the column is added with its default.",
		`<p class="recommendation">Recommendation (the factory agent): approve_merge: the migration is additive</p>`} {
		if !strings.Contains(needs, want) {
			t.Fatalf("Needs you lacks %q once the case is at the human:\n%s", want, needs)
		}
	}
	if strings.Contains(col(t, body, "In progress"), `data-card="t-m"`) {
		t.Fatal("In progress holds the card whose merge case is at the human")
	}
	if got := drawer(); !strings.Contains(got, `action="/approvals/4/approve"`) || !strings.Contains(got, `value="approve"`) || !strings.Contains(got, `value="deny"`) {
		t.Fatalf("the drawer lacks the merge's Approve and Deny once its case is at the human:\n%s", got)
	}
}

// A plan's request waits for its revision's face: while the revision's case
// climbs, Needs you lists no plan approval, the waiting plan sits in In
// progress and its drawer offers no plan approval. A case of an older
// revision at the human is no face of the newer request: the card's row is
// that case's, with no plan approval in it. Once a case of the request's
// revision is at the human the card's row carries the plan's approval card,
// In progress holds the card no more and the drawer offers it.
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
	drawer := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?tab=overview", nil), ck).Body.String()
	}
	const plan = `class="card approval plan"`
	body := board()
	if needs := col(t, body, "Needs you"); strings.Contains(needs, `data-card="t-plan"`) || strings.Contains(needs, plan) {
		t.Fatalf("Needs you lists the plan while its case climbs:\n%s", needs)
	}
	if progress := col(t, body, "In progress"); !strings.Contains(progress, `data-card="t-plan"`) {
		t.Fatalf("In progress lacks the plan whose case climbs:\n%s", progress)
	}
	if got := drawer(); strings.Contains(got, plan) {
		t.Fatalf("the drawer offers the plan's approval while its case climbs:\n%s", got)
	}
	r.openCases["t-plan"] = append(r.openCases["t-plan"], question(60, factoryd.CaseAtHuman, 1, "Which cache?"))
	if needs := col(t, board(), "Needs you"); !strings.Contains(needs, `data-case="60"`) || strings.Contains(needs, plan) {
		t.Fatalf("Needs you: want revision 1's case row with no approval of revision 2:\n%s", needs)
	}
	r.openCases["t-plan"] = []factoryd.Case{question(61, factoryd.CaseAtHuman, 2, "Which store?")}
	body = board()
	if needs := col(t, body, "Needs you"); !strings.Contains(needs, `data-case="61"`) || strings.Count(needs, plan) != 1 {
		t.Fatalf("Needs you: want case 61's row carrying the plan's approval once:\n%s", needs)
	}
	if strings.Contains(col(t, body, "In progress"), `data-card="t-plan"`) {
		t.Fatal("In progress holds the plan whose case is at the human")
	}
	if got := drawer(); !strings.Contains(got, plan) {
		t.Fatalf("the drawer lacks the plan's approval once its case is at the human:\n%s", got)
	}
}

// A merge request whose case climbs its rungs rides no other case's row:
// card t-m's monitor flag at the human is its row in Needs you, in its
// carousel's page and in its card swap, each with no approval of the
// merge.
func TestAClimbingMergeRidesNoOtherCasesRow(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 7, PRNumber: 9})
	r.openCases = map[string][]factoryd.Case{"t-m": {
		{ID: 41, TaskID: "t-m", Repository: "sf/code", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseOpen, OwnerRung: 2,
			Problem: "Pull request #9 waits for a merge approval", Facts: map[string]any{"approval": 4.0, "class": "high"}},
		{ID: 42, TaskID: "t-m", Repository: "sf/code", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman,
			Problem: "the review waits past its bound", Facts: map[string]any{"flag": "wait_over_bound"}}}}
	ck, _ := r.loggedIn(t)
	for where, path := range map[string]string{"Needs you": "/", "the carousel's page": "/projects/ai-software-factory-code/cards/t-m/cases/42",
		"the card swap": "/cards/t-m"} {
		got := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
		if where == "Needs you" {
			got = col(t, got, "Needs you")
		}
		if !strings.Contains(got, `data-case="42"`) || strings.Contains(got, "approval-4") || strings.Contains(got, "/approvals/4/") {
			t.Errorf("%s: want case 42's row with no approval of the climbing merge:\n%s", where, got)
		}
	}
}

// The drawer's merge card: the case's brief renders as markup (its own
// lists, never a raw wall), each why-a-human entry its own line, and an
// agent pull request on red checks shows its return and close directly —
// the actions the brief names, not a disclosure button that hides them.
func TestMergeDrawerRendersTheAskAndItsActions(t *testing.T) {
	r := newRig(t)
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{
		ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-red", Project: "sf/code", PRNumber: 10, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal","red_checks":true,"why_human":["checks round 1 failed","review round 2 said the same"]}`),
	})
	r.tasks.set("", factoryd.Task{ID: "t-red", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	r.openCases = map[string][]factoryd.Case{"t-red": {{ID: 8, TaskID: "t-red", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"approval": 6.0}, Brief: "- first fact\n- second fact"}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-red/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(body, "<li>first fact</li>") {
		t.Fatal("the drawer renders the detail's facts as no list")
	}
	if got := strings.Count(body, `class="why"`); got != 2 {
		t.Fatalf("the drawer renders %d why lines, want one per entry", got)
	}
	if !strings.Contains(body, `action="/approvals/6/deny"`) || strings.Contains(body, `id="deny-6-d" hidden`) {
		t.Fatal("the return and close form hides behind a disclosure")
	}
	if strings.Contains(body, `aria-controls="deny-6-d"`) {
		t.Fatal("the card still offers a disclosure button for its actions")
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
		{"unread", nil, true, []string{`<p class="problem">The console could not read the opened proofs; reload the board.</p>`}, 0},
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

// The drawer's pill reads the card's state like the face's: a merged
// card's drawer says merged, a done card's says done.
func TestDrawerPillReadsTheState(t *testing.T) {
	r := newRig(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-merged", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 7, PRNumber: 9},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	ck, _ := r.loggedIn(t)
	for id, want := range map[string]string{
		"t-merged": `<span class="pill st-merged">merged</span>`,
		"t-done":   `<span class="pill st-done">done</span>`,
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
		"t-red":   `<span class="pill st-merged">merged</span> <span class="st st-failed">chain red</span>`,
		"t-run":   `<span class="pill st-merged">merged</span> <span class="st st-merged">chain running</span>`,
		"t-stall": `<span class="pill st-merged">merged</span> <span class="st st-waiting">stalled</span>`,
	} {
		if !strings.Contains(progress, `data-card="`+id+`"`) || !strings.Contains(progress, word) {
			t.Errorf("In progress lacks %s wearing %s:\n%s", id, word, progress)
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
	want := `<span class="pill st-merged">merged</span> <span class="st st-merged"><a href="#card=t-r">blocked by roll #1162</a></span>`
	if !strings.Contains(progress, want) {
		t.Errorf("In progress lacks %s:\n%s", want, progress)
	}
	if strings.Count(progress, "blocked by roll") != 1 || !strings.Contains(progress, `<span class="st st-failed">chain red</span>`) {
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
// under its title; its drawer lists the cards it carries, and the merged
// card's drawer names the roll it waits for.
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
	if !strings.Contains(drawer, `<span class="why">carries</span>`) || !strings.Contains(drawer, `hx-get="/cards/t-m/drawer"`) {
		t.Errorf("the roll's drawer does not list the card it carries:\n%s", drawer)
	}
	drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(drawer, `<span class="why">blocked by</span>`) || !strings.Contains(drawer, `hx-get="/cards/t-roll/drawer"`) {
		t.Errorf("the merged card's drawer does not name its roll:\n%s", drawer)
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
// statuses it holds, in the four columns and the wide view, and the
// approval's one decide form the status of its task, whose card the
// decided request leaves standing. The press is the decide's one send —
// the answer, the fund and the note ride one POST, landing together in
// one row — and it answers with the board's refresh alone when htmx
// sends it; a browser's form keeps its redirect.
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
			`data-holds="queued waiting-deps waiting-image">`, `data-holds="running closing waiting-agent in-review merged">`, `data-holds="done failed closed"`,
			`<form method="post" action="/approvals/31/approve" hx-post="/approvals/31/approve" class="decide resolve" data-to="in-review">`},
		"/?project=ai-software-factory-code&view=wide": {`data-holds="queued waiting-deps">`, `data-holds="running closing waiting-agent">`, `data-holds="in-review">`, `data-holds="merged">`,
			`data-holds="failed"`, `data-holds="closed"`},
	} {
		body := r.do(httptest.NewRequest(http.MethodGet, page, nil), ck).Body.String()
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %s", page, want)
			}
		}
	}
	// the one send: the answer, the fund and the note ride one POST
	form := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"action": {"approve"}, "fund_eur": {"1.50"}, "note": {"the board alone answers"}}
	before := r.events.count()
	rec := r.do(htmxPost("/approvals/31/approve", form), ck)
	if body := rec.Body.String(); rec.Code != http.StatusNoContent || body != "" || rec.Header().Get("HX-Trigger") != "board-refresh" {
		t.Fatalf("the htmx approve = %d (%q):\n%s", rec.Code, rec.Header().Get("HX-Trigger"), body)
	}
	if !slices.Equal(approved, []string{"t-card #9 by sebastian"}) {
		t.Fatalf("the htmx approve merged %v, want t-card #9 by sebastian", approved)
	}
	// the one send landed the fund and the note with the answer, one row
	if k, ok, _ := r.tasks.Task(t.Context(), "t-card"); !ok || k.BudgetEUR != 1.5 {
		t.Errorf("t-card's budget = %v (ok %v), want the 1.50 the one send carried", k.BudgetEUR, ok)
	}
	checkRows(t, r.events, before,
		wantRow{kind: factoryd.EventMergeApproved, repo: "sf/code", taskID: "t-card",
			detail: map[string]any{"pr": int64(9), "approval": int64(31), "fund_eur": 1.5, "reason": "the board alone answers"}})
	if rec := r.do(post("/approvals/31/approve", form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code" {
		t.Fatalf("the browser's approve = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A plan's criteria request waits for its case's face: while the case is
// open on its rungs the board lists no approval for the plan, the card's
// page of criteria answers 404 and the plan's drawer holds no gap card;
// once the case is at the human the gap card stands in Needs you, and on
// each page of its criteria, with the rungs' analysis and recommendation
// from the case's ask. A done plan outside the board's finished page is
// listed all the same.
func TestACriteriaRequestWaitsForItsCasesFace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		onBoard bool
	}{{"on the board", true}, {"past the finished page", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			verdictRig(r)
			if !tc.onBoard {
				r.tasks.tasks = slices.DeleteFunc(r.tasks.tasks, func(bt factoryd.BoardTask) bool { return bt.ID == "t-plan" })
			}
			r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 41, TaskID: "t-plan", Repository: "sf/code", Kind: factoryd.CasePlanCriteria,
				Status: factoryd.CaseOpen, Problem: "criteria not met once every card of the plan ended: c2, c3", Facts: map[string]any{"approval": 30.0}}}}
			ck, _ := r.loggedIn(t)
			needs := func() string {
				return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
			}
			page := func() *httptest.ResponseRecorder {
				return r.do(httptest.NewRequest(http.MethodGet, "/approvals/30/criteria/2?project=ai-software-factory-code", nil), ck)
			}
			if got := needs(); strings.Contains(got, "Criteria gap") {
				t.Fatalf("Needs you lists the request while its case is open:\n%s", got)
			}
			if rec := page(); rec.Code != http.StatusNotFound {
				t.Fatalf("the criteria page while the case is open = %d %q, want 404", rec.Code, rec.Body.String())
			}
			if tc.onBoard {
				if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?tab=overview", nil), ck).Body.String(); strings.Contains(drawer, "Criteria gap") || strings.Contains(drawer, "/approvals/30/") {
					t.Fatalf("the plan's drawer holds the request while its case is open:\n%s", drawer)
				}
			}
			r.openCases["t-plan"][0].Status = factoryd.CaseAtHuman
			r.caseAsks = map[int64]factoryd.CaseAsk{41: {Detail: map[string]any{"info": "The factory agent found: c3 fails on main's head.",
				"recommendation": "grant_criteria: one more card fixes the suite", "recommended_by": "the factory agent"}}}
			for where, got := range map[string]string{"Needs you": needs(), "the criteria page": page().Body.String()} {
				for _, want := range []string{"Criteria gap", "The factory agent found: c3 fails on main's head.",
					`<p class="recommendation">Recommendation (the factory agent): grant_criteria: one more card fixes the suite</p>`} {
					if !strings.Contains(got, want) {
						t.Fatalf("%s lacks %q once the case is at the human:\n%s", where, want, got)
					}
				}
			}
		})
	}
}
