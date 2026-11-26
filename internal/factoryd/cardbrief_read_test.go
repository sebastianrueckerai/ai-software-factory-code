package factoryd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// briefPlan is the seeded plan of three cards: the foundation k1, the
// feature k2 that depends on it, and the integration card k3.
func briefPlan() Plan {
	return Plan{Summary: "The foundation narrows the refusal, the feature proves the parks, the integration card walks the head to its merge.",
		Spec: &Spec{Outcome: "A settled park stays settled after a restart.", Glossary: []string{"park: a card that waits for a human", "settled: answered and closed"}},
		Criteria: []PlanCriterion{
			{ID: "park_settled", Kind: CriterionTest, Claim: "A settled park stays settled.", Suite: "./acceptance", Name: "TestParkSettled"},
			{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."},
		},
		Cards: []PlanCard{
			{Key: "k1", Role: RoleFoundation, Title: "Narrow the close refusal", Brief: "Narrow the refusal.", Size: "small"},
			{Key: "k2", Role: RoleFeature, Title: "Prove that settled parks stay settled", Why: "A park that wakes again asks a human twice.",
				Summary: "Tests under acceptance/ restart the factory and read the park.", Section: "docs/plan.md#parks",
				Brief: "Write the restart test for a settled park.", Size: "small", Directories: []string{"acceptance"},
				Acceptance: []PlanItem{{Criterion: "park_settled", Text: "TestParkSettled passes after a restart"}}},
			{Key: "k3", Role: RoleIntegration, Title: "Walk the head to its merge press", Brief: "Walk it.", Size: "medium"},
		}}
}

// seedBriefPlan stores plan t-bp (#1947, "criteria gate") with briefPlan
// as its revision, its three cards (#2100 done with its agent's summary,
// #2101 and #2102 queued), their dependencies, one answered question and
// the decided confirmation.
func seedBriefPlan(t *testing.T, s *PGStore) (plan Task, cards []Task) {
	t.Helper()
	ctx := context.Background()
	p := briefPlan()
	plan = Task{ID: "t-bp", Kind: "agent", Status: "running", Track: policy.TrackPlan, Repository: "sf/code", IssueNumber: 1947, PlanName: "criteria gate",
		Objective: "Re-cut the criteria gate\n\nThe merge press runs the gate.", Why: "The gate never ran on a real change.", Author: "controller", AuthorWriter: true, BudgetEUR: 5}
	if err := s.CreateTask(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlanName(ctx, plan.ID, plan.PlanName); err != nil {
		t.Fatal(err)
	}
	raw, sha, err := planSHA(p)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.AddPlanRevision(ctx, plan.ID, raw, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPlanQuestions(ctx, plan.ID, rev, []PlanQuestion{{Text: "Does a park expire after a day?", Choices: []string{"yes", "no"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPlanQuestion(ctx, plan.ID, rev, 1, "", "No, it stays settled until a human answers.", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutPlanConfirmation(ctx, plan.ID, rev, PlanQuestion{Text: "Confirm the plan?"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPlanQuestion(ctx, plan.ID, rev, 2, DecisionConfirm, "", "sebastian"); err != nil {
		t.Fatal(err)
	}
	for i, c := range p.Cards {
		var deps []Dependency
		if i > 0 {
			deps = []Dependency{{Issue: 2100, Key: "k1"}}
		}
		card := Task{ID: "t-k" + c.Key[1:], Kind: "agent", Status: "queued", Track: policy.TrackDirect, Repository: "sf/code", IssueNumber: int64(2100 + i),
			Source: SourcePlan, Parent: plan.ID, PlanKey: c.Key, Objective: c.Title + "\n\n" + CardBody(c, p.Criteria, "plan #1947", i+1, 3, "controller", deps, ""),
			Why: c.Why, Summary: c.Summary, Author: "controller", AuthorWriter: true, BudgetEUR: 2, Size: c.Size}
		if i == 0 {
			card.Status = "done"
		}
		if err := s.CreateTask(ctx, card); err != nil {
			t.Fatal(err)
		}
		cards = append(cards, card)
	}
	if err := s.SetAgentSummary(ctx, cards[0].ID, "I narrowed the refusal to plans with criteria. TestCloseRefusal covers both."); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, cards[1].ID, []string{cards[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, cards[2].ID, []string{cards[0].ID, cards[1].ID}); err != nil {
		t.Fatal(err)
	}
	return plan, cards
}

// pgBriefStore is a store emptied of tasks and plans for the brief's tests.
func pgBriefStore(t *testing.T) *PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, task_deps, plan_revisions, plan_questions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// The brief of a plan's second card holds its place in the plan, the
// plan's three cards with their states, the plan's outcome and glossary,
// the one question a human answered and what the card it depends on
// delivered, as the store holds them.
func TestCardBriefOfAPlanChild(t *testing.T) {
	ctx := context.Background()
	s := pgBriefStore(t)
	_, cards := seedBriefPlan(t, s)
	got, err := s.CardBrief(ctx, cards[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	p := briefPlan()
	_, body, _ := strings.Cut(cards[1].Objective, "\n\n")
	wantText := BriefText{Title: "Prove that settled parks stay settled", Why: "A park that wakes again asks a human twice.",
		Summary: "Tests under acceptance/ restart the factory and read the park.", Body: strings.TrimSuffix(body, "\nSize: small")}
	if got.Text != wantText {
		t.Errorf("text = %+v\nwant %+v", got.Text, wantText)
	}
	k1 := CardRef{Issue: 2100, Title: "Narrow the close refusal", State: "done"}
	if want := p.Cards[1].Acceptance; !reflect.DeepEqual(got.Acceptance, want) {
		t.Errorf("acceptance = %+v, want %+v", got.Acceptance, want)
	}
	if want := p.Criteria[:1]; !reflect.DeepEqual(got.Criteria, want) {
		t.Errorf("criteria = %+v, want the one the item cites %+v", got.Criteria, want)
	}
	if want := []CardRef{k1}; !reflect.DeepEqual(got.DependsOn, want) {
		t.Errorf("depends on = %+v, want %+v", got.DependsOn, want)
	}
	if want := []DeliveredCard{{Card: k1, Summary: "I narrowed the refusal to plans with criteria. TestCloseRefusal covers both."}}; !reflect.DeepEqual(got.Delivered, want) {
		t.Errorf("delivered = %+v, want %+v", got.Delivered, want)
	}
	want := &BriefPlan{Task: "t-bp", Issue: 1947, Name: "criteria gate", Key: "k2", Role: RoleFeature, Index: 2, Of: 3,
		Outcome: "A settled park stays settled after a restart.", Glossary: []string{"park: a card that waits for a human", "settled: answered and closed"},
		Summary: p.Summary, Section: "docs/plan.md#parks",
		Brief:   BriefText{Title: "Re-cut the criteria gate", Why: "The gate never ran on a real change.", Body: "The merge press runs the gate."},
		Cards:   []CardRef{k1, {Issue: 2101, Title: "Prove that settled parks stay settled", State: "queued"}, {Issue: 2102, Title: "Walk the head to its merge press", State: "queued"}},
		Answers: []AnsweredQuestion{{Question: "Does a park expire after a day?", Answer: "No, it stays settled until a human answers."}}}
	if !reflect.DeepEqual(got.Plan, want) {
		t.Errorf("plan = %+v\nwant %+v", got.Plan, want)
	}
}

// A card that continues a plan's card belongs to the same plan, at the
// same place: the successor of a plan's card, the replacement of its
// retry, and the agent card a human handed a plan's card for a human to,
// which takes its place in the plan as the retry's replacement does: its
// key, its dependencies and its dependents.
func TestSuccessorKeepsThePlan(t *testing.T) {
	ctx := context.Background()
	// the plan as card t-a (#5, links-api) of testPlan holds it; its own
	// place in Cards names the card itself
	want := func(issue int) *BriefPlan {
		return &BriefPlan{Task: "t-plan", Issue: 1, Name: "Link shortener", Key: "links-api", Role: RoleFeature, Index: 2, Of: 4,
			Outcome: "A link shortener: links with slugs that redirect and count clicks.", Glossary: []string{"slug: the short key a link gets", "click: one redirect served"},
			Summary: "The foundation first, then links and redirects, then the acceptance tests.", Brief: BriefText{Title: "Link shortener", Body: "Build a link shortener."},
			Cards: []CardRef{{Issue: 4, Title: "Foundation", State: "done"}, {Issue: issue, Title: retryTitle, State: "queued"}, {Issue: 6, Title: "Redirects", State: "waiting-deps"},
				{Title: "Integration through acceptance tests"}}}
	}
	check := func(name string, store *fakeStore, id string) {
		t.Helper()
		c, ok := store.tasks[id]
		if !ok {
			t.Fatalf("%s: no card %s", name, id)
		}
		got, err := store.CardBrief(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w := want(int(c.IssueNumber))
		if c.Status != "queued" {
			w.Cards[1].State = c.Status
		}
		if !reflect.DeepEqual(got.Plan, w) {
			t.Errorf("%s: plan = %+v\nwant %+v", name, got.Plan, w)
		}
	}

	r, store, _, _, _ := newRetryRig(t)
	check("the plan's card", store, "t-a")
	id := SuccessorID("t-a")
	_, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"})
	if err != nil {
		t.Fatal(err)
	}
	check("the successor", store, id)

	r, store, _, _, _ = newRetryRig(t)
	if _, err := r.RetryCard(ctx, "t-a", "alice"); err != nil {
		t.Fatal(err)
	}
	check("the retry's replacement", store, RetryID("t-a"))

	r, store, _ = handOverRig(t)
	if _, err := r.Go(ctx, "t-a", "sebastian"); err != nil {
		t.Fatal(err)
	}
	agent := cardsByKey(t, store)["links-api"]
	check("the hand-over", store, agent.ID)
	if agent.Source != SourcePlan || agent.Size != "small" || store.tasks["t-a"].PlanKey != "" {
		t.Errorf("the agent card %+v, the human card's key %q: want the plan's card of links-api in its place", agent, store.tasks["t-a"].PlanKey)
	}
	if !slices.Equal(store.deps[agent.ID], []string{"t-f"}) || !slices.Equal(store.deps["t-b"], []string{agent.ID}) {
		t.Errorf("edges: %s waits on %q, t-b on %q; want t-f and %s", agent.ID, store.deps[agent.ID], store.deps["t-b"], agent.ID)
	}
}

// handOverRig is links-api of testPlan as a card for a human, t-a (#5)
// of plan t-plan (#1) on the code repository: its project ran no agents
// when it was queued, and runs them again. It depends on the foundation
// t-f (#4), done, and redirects t-b (#6) waits on it; the issues depend
// on each other as the rows do.
func handOverRig(t *testing.T) (*Runner, *fakeStore, *codeIssues) {
	t.Helper()
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	const repo = "sf/ai-software-factory-code"
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Repository: repo, IssueNumber: 1, Objective: "Link shortener\n\nBuild a link shortener."}
	card := func(id string, n int64, status, key, objective string) Task {
		return Task{ID: id, Kind: "agent", Status: status, Track: policy.TrackDirect, Repository: repo, IssueNumber: n, Source: SourcePlan, Parent: "t-plan",
			PlanKey: key, RiskClass: "standard", BudgetEUR: 2, Size: "small", Objective: objective}
	}
	human := card("t-a", 5, "waiting-human", "links-api", retryCard)
	human.Kind, human.Track = "human", ""
	for _, c := range []Task{plan, card("t-f", 4, "done", "foundation", "Foundation"), human, card("t-b", 6, "waiting-deps", "redirects", "Redirects")} {
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	raw, sha, err := planSHA(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "t-plan", raw, sha); err != nil {
		t.Fatal(err)
	}
	store.AddTaskDeps(ctx, "t-a", []string{"t-f"})
	store.AddTaskDeps(ctx, "t-b", []string{"t-a"})
	issues.deps = map[int64][]int64{5: {4}, 6: {5}}
	return r, store, issues
}

// The agent card a human hands a plan's card to is filed as a retry files
// a replacement: it keeps the card's author and standing, its priority,
// its rung and its size, follows the plan's process, and gets its size's
// budget. Forgejo shows the waits it took over: its issue depends on the
// foundation's, and redirects' issue on its issue instead of the human
// card's.
func TestHandOverCarriesWhatARetryCarries(t *testing.T) {
	ctx := context.Background()
	r, store, issues := handOverRig(t)
	h := store.tasks["t-a"]
	h.Author, h.AuthorWriter, h.Priority, h.Rung = "sebastian", true, 2, RungFrontier
	store.tasks["t-a"] = h
	plan := store.tasks["t-plan"]
	plan.Process = "spec"
	store.tasks["t-plan"] = plan
	n, err := r.Go(ctx, "t-a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	agent := cardsByKey(t, store)["links-api"]
	// a small card on the frontier rung gets a medium card's 5 EUR, the
	// policy's task maximum; the project's process is maintenance
	if agent.IssueNumber != n || agent.Author != "sebastian" || !agent.AuthorWriter || agent.Priority != 2 || agent.Rung != RungFrontier ||
		agent.Size != "small" || agent.Process != "spec" || agent.BudgetEUR != 5 {
		t.Errorf("the agent card = %+v", agent)
	}
	if got := issues.deps[n]; !slices.Equal(got, []int64{4}) {
		t.Errorf("#%d depends on %v, want the foundation's #4", n, got)
	}
	if got := issues.deps[6]; !slices.Equal(got, []int64{n}) {
		t.Errorf("redirects' #6 depends on %v, want the agent card's #%d alone", got, n)
	}

	// a plan no human approved files its cards as the factory's bot, an
	// outside author: the agent card keeps that standing, as the plan's
	// other cards, a retry and a successor do
	r, store, _ = handOverRig(t)
	h = store.tasks["t-a"]
	h.Author = "factory-agent"
	store.tasks["t-a"] = h
	if _, err := r.Go(ctx, "t-a", "alice"); err != nil {
		t.Fatal(err)
	}
	if agent := cardsByKey(t, store)["links-api"]; agent.Author != "factory-agent" || agent.AuthorWriter || agent.Standing() != harness.StandingOutside {
		t.Errorf("the bot-approved plan's agent card = %+v, want the bot as author, standing outside", agent)
	}
}

// An outside account's issue that waited on a human (its project ran no
// agents) and is handed to the factory keeps its author and standing, as
// its retry would, so the screening its process runs on an outsider's
// issue runs, and keeps the budget its text asked for. The factory's own
// card, a routine's, hands over with no author: it has no outside text.
func TestGoKeepsTheOutsidersStandingAndTheAskedBudget(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := routineRunner(t, routineTree(t))
	if err := store.CreateTask(ctx, Task{ID: "t-out", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 7,
		Source: SourceWebhook, RiskClass: "standard", Author: "mallory", Objective: "Fix the counter\n\nThe counter skips a row.\nBudget: 3 EUR"}); err != nil {
		t.Fatal(err)
	}
	n, err := r.Go(ctx, "t-out", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	var agent Task
	for _, c := range store.tasks {
		if c.IssueNumber == n {
			agent = c
		}
	}
	screening, _ := routineTree(t).Processes[agent.Process].Stage(policy.StageScreening)
	// the policy's task default is 2 EUR, its maximum 5
	if agent.Author != "mallory" || agent.AuthorWriter || agent.Standing() != harness.StandingOutside || agent.BudgetEUR != 3 {
		t.Errorf("the agent card = %+v, want mallory's outside standing and the asked 3 EUR", agent)
	}
	if why := (&taskRun{standing: agent.Standing()}).skipReason(screening); screening.When != policy.WhenOutsider || why != "" {
		t.Errorf("screening %+v skipped: %q, want it run on the outsider's card", screening, why)
	}
}

// An alert's card that waited on a human is the factory's bot's, and its
// text is telemetry: handed to the factory, it keeps that standing.
func TestGoKeepsAnAlertsTelemetryStanding(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := routineRunner(t, routineTree(t))
	if err := store.CreateTask(ctx, Task{ID: "t-alert", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 8,
		Source: SourceAlert, RiskClass: "standard", Author: "factory-agent", Objective: "Disk full on the runner\n\nThe runner's disk is at 97 percent."}); err != nil {
		t.Fatal(err)
	}
	n, err := r.Go(ctx, "t-alert", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	var agent Task
	for _, c := range store.tasks {
		if c.IssueNumber == n {
			agent = c
		}
	}
	if agent.Author != "factory-agent" || agent.Standing() != harness.StandingTelemetry {
		t.Errorf("the alert's agent card = %+v, want the bot as author, standing telemetry", agent)
	}
}

// A card whose every file sits under a workflow directory goes to no
// agent: the worker's push refuses such changes. The go refuses it, a
// plan's card and a human's own issue alike, and files nothing.
func TestGoRefusesAWorkflowOnlyCard(t *testing.T) {
	ctx := context.Background()
	const repo = "sf/ai-software-factory-code"
	p := testPlan()
	p.Cards = append(p.Cards, PlanCard{Key: "ci", Role: RoleFeature, Title: "Pin the runner image", Brief: "Pin it.", Directories: []string{".forgejo/workflows"}, Size: "small"})
	for name, c := range map[string]Task{
		"a plan's card": {ID: "t-wf", Kind: "human", Status: "waiting-human", Repository: repo, IssueNumber: 7, Source: SourcePlan, Parent: "t-plan", PlanKey: "ci",
			RiskClass: "standard", Objective: "Pin the runner image\n\nPin it."},
		"a human's issue": {ID: "t-wf", Kind: "human", Status: "waiting-human", Repository: repo, IssueNumber: 7, Source: SourceWebhook,
			RiskClass: "standard", Objective: "Pin the runner image\n\nPin it.\nDirectories: .forgejo/workflows"},
	} {
		r, store, _, _ := routineRunner(t, routineTree(t))
		for _, k := range []Task{{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Repository: repo, IssueNumber: 1, Objective: "Link shortener"}, c} {
			if err := store.CreateTask(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		raw, sha, err := planSHA(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddPlanRevision(ctx, "t-plan", raw, sha); err != nil {
			t.Fatal(err)
		}
		var no ErrNoGo
		want := "Card #7 changes only files under a workflow directory (.forgejo/workflows), and the factory's push refuses changes to workflow files: make the change yourself and press Done."
		if _, err := r.Go(ctx, "t-wf", "sebastian"); !errors.As(err, &no) || err.Error() != want {
			t.Errorf("%s: Go = %v, want %q", name, err, want)
		}
		if len(store.tasks) != 2 {
			t.Errorf("%s: the refused hand-over filed a card: %d tasks", name, len(store.tasks))
		}
	}
}

// A dependency the store no longer holds is no card to name: the brief
// leaves it out, never naming a card without a title.
func TestCardBriefLeavesOutAGoneDependency(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	for _, c := range []Task{{ID: "t-1", Track: "direct", Objective: "Tidy the docs"}, {ID: "t-0", Track: "direct", IssueNumber: 3, Status: "done", Objective: "Write the docs"}} {
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	store.AddTaskDeps(ctx, "t-1", []string{"t-0", "t-gone"})
	b, err := store.CardBrief(ctx, "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []CardRef{{Issue: 3, Title: "Write the docs", State: "done"}}; !reflect.DeepEqual(b.DependsOn, want) {
		t.Fatalf("depends on %+v, want %+v", b.DependsOn, want)
	}
}

// A plan is no card of a plan, whatever its parent: a delegated card that
// became a plan belongs to none.
func TestAPlanBelongsToNoPlan(t *testing.T) {
	ctx := context.Background()
	_, store, _, _, _ := newRetryRig(t)
	if err := store.CreateTask(ctx, Task{ID: "t-sub", Track: policy.TrackPlan, Parent: "t-a", Objective: "Split the links API"}); err != nil {
		t.Fatal(err)
	}
	if plan, p, _, err := cardPlan(ctx, store, store.tasks["t-sub"]); plan.ID != "" || p.Cards != nil || err != nil {
		t.Fatalf("cardPlan of a plan = %q, %+v, %v; want none", plan.ID, p, err)
	}
}

// The agent's text of a plan's card holds, in order, the title, the why
// line, the summary, the full text with the acceptance items, the plan's
// outcome, the question a human answered and what the card it depends on
// delivered.
func TestAgentTextHoldsTheBrief(t *testing.T) {
	ctx := context.Background()
	s := pgBriefStore(t)
	_, cards := seedBriefPlan(t, s)
	b, err := s.CardBrief(ctx, cards[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	text := b.AgentText()
	at := 0
	for _, part := range []string{
		"Prove that settled parks stay settled\n\n",
		"Why: A park that wakes again asks a human twice.",
		"Summary: Tests under acceptance/ restart the factory and read the park.",
		"Write the restart test for a settled park.",
		"- TestParkSettled passes after a restart [park_settled]",
		"A settled park stays settled after a restart.",
		"Does a park expire after a day?",
		"No, it stays settled until a human answers.",
		"I narrowed the refusal to plans with criteria. TestCloseRefusal covers both.",
	} {
		i := strings.Index(text[at:], part)
		if i < 0 {
			t.Fatalf("the agent's text lacks %q after byte %d:\n%s", part, at, text)
		}
		at += i + len(part)
	}
	if strings.Count(text, "Why: ") != 2 || strings.Count(text, "Summary: Tests under") != 1 {
		t.Errorf("the why line or the summary stands twice:\n%s", text)
	}
}

// A human's bare issue, a title and its text, is its title and its text:
// no empty part leaves a label or a heading.
func TestAgentTextOfABareIssue(t *testing.T) {
	b := CardBrief{Text: BriefText{Title: "Gateway: the 429 retry loses the request id", Body: "Retrying a 429 drops the provider's request id from the log."}}
	if got, want := b.AgentText(), "Gateway: the 429 retry loses the request id\n\nRetrying a 429 drops the provider's request id from the log."; got != want {
		t.Fatalf("AgentText = %q, want %q", got, want)
	}
	if got := (CardBrief{Text: BriefText{Title: "Tidy the docs"}}).AgentText(); got != "Tidy the docs" {
		t.Fatalf("AgentText of a title alone = %q", got)
	}
}

// The model review reads the card's brief as the agent does: the why line
// and the outcome of the plan the card's parent chain reaches, here a
// successor's.
func TestReviewReadsTheWhy(t *testing.T) {
	ctx := context.Background()
	task := gateTask
	task.Source, task.Parent, task.PlanKey, task.Why = SourceSuccessor, "t-rp", "k2", "A park that wakes again asks a human twice."
	r, store, _ := newGateRunner(t, task, &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n+check:\n"})
	if err := store.CreateTask(ctx, Task{ID: "t-rp", Kind: "agent", Status: "running", Track: policy.TrackPlan, Repository: "sf/repo", IssueNumber: 3,
		Objective: "Re-cut the criteria gate"}); err != nil {
		t.Fatal(err)
	}
	raw, sha, err := planSHA(briefPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "t-rp", raw, sha); err != nil {
		t.Fatal(err)
	}
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"ok","findings":[]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err != nil {
		t.Fatal(err)
	}
	if len(ll.chats) != 1 {
		t.Fatalf("calls = %d, want the one review call", len(ll.chats))
	}
	input := userMessage(ll.chats[0])
	for _, want := range []string{"Why: A park that wakes again asks a human twice.", "A settled park stays settled after a restart."} {
		if !strings.Contains(input, want) {
			t.Errorf("the review's input lacks %q: %s", want, input)
		}
	}
}

// The gateway reads a session's card brief at POST /internal/brief: the
// AgentText of the body's task, 404 for a card the store does not hold,
// 400 for a body that names none.
func TestCardBriefHandlerAnswersTheAgentText(t *testing.T) {
	store := newFakeStore()
	if err := store.CreateTask(context.Background(), Task{ID: "t-h", Track: "direct", Objective: "Tidy the docs\n\nTidy them.\nSize: small", Why: "They drift."}); err != nil {
		t.Fatal(err)
	}
	h := CardBriefHandler(store)
	for body, want := range map[string]struct {
		status int
		text   string
	}{
		`{"task_id":"t-h"}`:    {http.StatusOK, `{"text":"Tidy the docs\n\nWhy: They drift.\n\nTidy them."}` + "\n"},
		`{"task_id":"t-none"}`: {http.StatusNotFound, "card t-none: not found\n"},
		`{}`:                   {http.StatusBadRequest, "the body names no task_id\n"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/internal/brief", strings.NewReader(body)))
		if rr.Code != want.status || rr.Body.String() != want.text {
			t.Errorf("%s: %d %q, want %d %q", body, rr.Code, rr.Body.String(), want.status, want.text)
		}
	}
}
