package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// splitFiles are the large pull request's files, 1100 lines in all.
var splitFiles = []PRFile{{Filename: "web/app.ts", Additions: 500}, {Filename: "web/app.css", Additions: 80},
	{Filename: "api/links.go", Additions: 500, Deletions: 20}}

// splitPlan is a passing split of splitFiles: the api card first, the web
// card on it, each medium (under 600) and alone carrying 520 and 580 lines.
func splitPlan() Plan {
	return Plan{Summary: "The links API first, then the page on it.", Cards: []PlanCard{
		{Key: "web", Role: RoleFeature, Title: "The links page", Brief: "Carry over web/ of pull/9.", Directories: []string{"web"},
			DependsOn: []string{"api"}, Acceptance: []string{"TestLinksPage in web"}, Size: "medium"},
		{Key: "api", Role: RoleFeature, Title: "The links API", Brief: "Carry over api/links.go of pull/9.", Directories: []string{"api/links.go"},
			Acceptance: []string{"TestCreateLink in api"}, Size: "medium"},
	}}
}

// The check over the planner's answer: 2 or 3 cards, each size under the
// bound, dependencies only among the cards and acyclic, every file
// covered by a card's directories, a directory that is the root covering
// none, and the lines of the files only one card covers under that card's
// size.
func TestCheckSplit(t *testing.T) {
	change := func(edit func(p *Plan)) string {
		p := splitPlan()
		edit(&p)
		return fencePlan(p)
	}
	extra := PlanCard{Key: "docs", Role: RoleFeature, Title: "Docs", Brief: "b", Acceptance: []string{"a"}, Size: "small"}
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"passing": {fencePlan(splitPlan()), nil},
		"four cards": {change(func(p *Plan) { p.Cards = append(p.Cards, extra, extra); p.Cards[3].Key = "more" }),
			[]string{"plan: 4 cards; this process allows 2 to 3"}},
		"over the bound": {change(func(p *Plan) { p.Cards[0].Size = "large" }),
			[]string{"plan: card web: size large plans up to 1000 changed lines and the bound is 1000: split the card",
				"split: card web is large, up to 1000 changed lines; each card stays under 1000"}},
		"cycle": {change(func(p *Plan) { p.Cards[1].DependsOn = []string{"web"} }),
			[]string{"plan: the dependencies form a cycle: web -> api -> web"}},
		"outside the split": {change(func(p *Plan) { p.Cards[0].DependsOn = []string{"auth"} }),
			[]string{`plan: card web: depends on "auth", which is no card of the plan`}},
		"missing file": {change(func(p *Plan) { p.Cards[0].Directories = []string{"web/app.ts"} }),
			[]string{"split: no card's directories cover web/app.css"}},
		"the root": {change(func(p *Plan) {
			p.Cards[0].Directories = []string{"web/.."}
			p.Cards[1].Directories = []string{"api/links.go", "./"}
		}),
			[]string{"split: no card's directories cover web/app.ts, web/app.css"}},
		"alone over its size": {change(func(p *Plan) { p.Cards[1].Size = "small" }),
			[]string{"split: card api alone carries 520 changed lines (api/links.go), not under its size small's 300"}},
		"shared, not alone": {change(func(p *Plan) { p.Cards[0].Directories = []string{"web", "api"}; p.Cards[1].Size = "small" }), nil},
	} {
		if _, got := CheckSplit(tc.text, splitFiles, 1000, testLimits); !slices.Equal(got, tc.want) {
			t.Errorf("%s: findings %q, want %q", name, got, tc.want)
		}
	}
}

// A file whose changed lines alone exceed the bound makes the split
// impossible: the finding names it and its lines.
func TestSplitOverBoundReturnsFindingsForAnOverBoundFile(t *testing.T) {
	files := []PRFile{{Filename: "web/app.ts", Additions: 600, Deletions: 500}, {Filename: "api/links.go", Additions: 400}}
	got := splitOverBound(files, 1000)
	want := []string{"split: web/app.ts alone changes 1100 lines, over the bound 1000: no card can stay under it"}
	if !slices.Equal(got, want) {
		t.Fatalf("findings %q, want %q", got, want)
	}
}

// Files each under the bound leave the split possible: no findings.
func TestSplitOverBoundReturnsNilWhenAllFilesAreUnderBound(t *testing.T) {
	if got := splitOverBound(splitFiles, 1000); got != nil {
		t.Fatalf("findings %q, want nil", got)
	}
}

// Without a bound (0) there is no check, whatever the files' sizes.
func TestSplitOverBoundReturnsNilWhenBoundIsZero(t *testing.T) {
	files := []PRFile{{Filename: "web/app.ts", Additions: 5000, Deletions: 9000}}
	if got := splitOverBound(files, 0); got != nil {
		t.Fatalf("findings %q, want nil", got)
	}
}

// A planted over-bound file refuses the split before any session: the
// card never runs, no pod starts, the findings name the file and its
// lines.
func TestRunSplitRefusesAnOverBoundFileWithoutASession(t *testing.T) {
	big := Task{ID: "t1", Status: "in-review", Objective: "Links", Repository: "sf/r", SHA: "abc123", Process: "maintenance",
		IssueNumber: 5, PRNumber: 9, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, big, kube, &fakeObjectStore{})
	hub.prFiles = `[{"filename":"web/app.ts","additions":1100,"deletions":0},{"filename":"api/links.go","additions":100,"deletions":0}]`
	v := runner.cfg.Policy()
	v.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return v }
	val, err := execActivity(t, runner, ActivityRunSplit, "t1", "factory/coding-frontier", int64(0), []string(nil))
	if err != nil {
		t.Fatal(err)
	}
	var res PlanResult
	if err := val.Get(&res); err != nil {
		t.Fatal(err)
	}
	want := []string{"split: web/app.ts alone changes 1100 lines, over the bound 1000: no card can stay under it"}
	if !slices.Equal(res.Findings, want) {
		t.Fatalf("findings %q, want %q", res.Findings, want)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "in-review" {
		t.Fatalf("status after the refusal %s, want in-review", got.Status)
	}
	if len(kube.podOrder) != 0 {
		t.Fatalf("a pod started for a doomed split")
	}
}

// A split session runs the planner on the card's pull request: the card
// runs meanwhile, the guide names the pull request, its files and the
// bound, the clone holds its head as branch pull/9, and a passing answer
// is the card's plan revision; the card is in review again after it.
func TestRunSplitReadsThePullRequest(t *testing.T) {
	big := Task{ID: "t1", Status: "in-review", Objective: "Links\n\nThe links page and API.", Repository: "sf/r", SHA: "abc123", Process: "maintenance",
		IssueNumber: 5, PRNumber: 9, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, big, kube, &fakeObjectStore{})
	hub.prFiles = `[{"filename":"web/app.ts","additions":500,"deletions":0},{"filename":"web/app.css","additions":80,"deletions":0},` +
		`{"filename":"api/links.go","additions":500,"deletions":20}]`
	v := runner.cfg.Policy()
	v.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return v }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunSplit, "t1", "factory/coding-frontier", int64(0), []string{"an earlier finding"})
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "running" {
		t.Fatalf("status during the session %s, want running", got.Status)
	}
	kube.scriptPods(0, resultLine(true, fencePlan(splitPlan())))
	if err := <-done; err != nil {
		t.Fatalf("RunSplit: %v", err)
	}
	if res.Revision != 1 || len(res.Findings) != 0 {
		t.Fatalf("result %+v", res)
	}
	env := podEnv(kube.podAt(0))
	guide := env["REPORT_GUIDE"]
	for _, want := range []string{"Pull request #9 of this card changes 1100 lines in 3 files, more than the 1000 lines one pull request may change",
		"each one pull request of under 1000 changed lines", "branch pull/9", "- web/app.ts +500 -0\n- web/app.css +80 -0\n- api/links.go +500 -20\n",
		"size: small, medium or large; every card's size stays under 1000 lines."} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide lacks %q:\n%s", want, guide)
		}
	}
	if env["MODE"] != "report" || env["FINDINGS"] != `["an earlier finding"]` {
		t.Fatalf("pod env %v", env)
	}
	clone := containerEnv(kube.podAt(0).spec["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any))
	if clone["PULL"] != "9" || clone["REPO_SHA"] != "abc123" {
		t.Fatalf("clone env %v", clone)
	}
	if rev, ok, _ := store.PlanRevision(context.Background(), "t1", 1); !ok || rev.SHA256 != res.SHA256 {
		t.Fatalf("revision %+v %v", rev, ok)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "in-review" {
		t.Fatalf("status after the session %s, want in-review", got.Status)
	}
}

// A split whose files all stay under the bound runs its planner session
// as before: the pod starts, the guide names the pull request and the
// bound, and the planner's answer becomes the card's plan revision.
func TestRunSplitRunsTheSessionWhenAllFilesAreUnderBound(t *testing.T) {
	big := Task{ID: "t1", Status: "in-review", Objective: "Links", Repository: "sf/r", SHA: "abc123", Process: "maintenance",
		IssueNumber: 5, PRNumber: 9, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, big, kube, &fakeObjectStore{})
	hub.prFiles = `[{"filename":"web/app.ts","additions":500,"deletions":0},{"filename":"api/links.go","additions":520,"deletions":0}]`
	v := runner.cfg.Policy()
	v.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return v }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunSplit, "t1", "factory/coding-frontier", int64(0), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	plan := Plan{Summary: "The API first, then the page on it.", Cards: []PlanCard{
		{Key: "api", Role: RoleFeature, Title: "The links API", Brief: "Carry over api/ of pull/9.", Directories: []string{"api"},
			Acceptance: []string{"TestCreateLink in api"}, Size: "medium"},
		{Key: "web", Role: RoleFeature, Title: "The links page", Brief: "Carry over web/ of pull/9.", Directories: []string{"web"},
			DependsOn: []string{"api"}, Acceptance: []string{"TestLinksPage in web"}, Size: "medium"},
	}}
	kube.scriptPods(0, resultLine(true, fencePlan(plan)))
	if err := <-done; err != nil {
		t.Fatalf("RunSplit: %v", err)
	}
	raw, sha, _ := planSHA(plan)
	if res.Revision != 1 || res.SHA256 != sha || len(res.Findings) != 0 {
		t.Fatalf("result %+v, want revision 1 at %s", res, sha)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods %d, want the session's one", len(kube.podOrder))
	}
	env := podEnv(kube.podAt(0))
	guide := env["REPORT_GUIDE"]
	for _, want := range []string{"Pull request #9 of this card changes 1020 lines in 2 files, more than the 1000 lines one pull request may change",
		"- web/app.ts +500 -0\n- api/links.go +520 -0\n"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide lacks %q:\n%s", want, guide)
		}
	}
	if env["MODE"] != "report" || env["FINDINGS"] != "null" {
		t.Fatalf("pod env MODE=%s FINDINGS=%s, want report and null", env["MODE"], env["FINDINGS"])
	}
	if rev, ok, _ := store.PlanRevision(context.Background(), "t1", 1); !ok || string(rev.Plan) != string(raw) {
		t.Fatalf("revision %+v %v", rev, ok)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "in-review" {
		t.Fatalf("status after the session %s, want in-review", got.Status)
	}
}

// A split refused at once spends no session: the findings name the file,
// no pod starts, the card keeps its status, and a second try on the same
// over-bound head is refused again without one.
func TestRunSplitSpendsNoSessionOnAnOverBoundRefusal(t *testing.T) {
	big := Task{ID: "t1", Status: "waiting-human", Objective: "Links", Repository: "sf/r", SHA: "abc123", Process: "maintenance",
		IssueNumber: 5, PRNumber: 9, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, big, kube, objects)
	hub.prFiles = `[{"filename":"web/app.ts","additions":500,"deletions":0},{"filename":"api/links.go","additions":1100,"deletions":0}]`
	v := runner.cfg.Policy()
	v.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return v }

	run := func(findings []string) PlanResult {
		t.Helper()
		val, err := execActivity(t, runner, ActivityRunSplit, "t1", "factory/coding-frontier", int64(0), findings)
		if err != nil {
			t.Fatal(err)
		}
		var res PlanResult
		if err := val.Get(&res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	want := []string{"split: api/links.go alone changes 1100 lines, over the bound 1000: no card can stay under it"}
	first := run([]string(nil))
	if !slices.Equal(first.Findings, want) {
		t.Fatalf("refusal findings %q, want %q", first.Findings, want)
	}
	second := run(first.Findings)
	if !slices.Equal(second.Findings, want) {
		t.Fatalf("second refusal %q, want %q", second.Findings, want)
	}
	if len(kube.podOrder) != 0 {
		t.Fatalf("pods %d: a session ran for a doomed split", len(kube.podOrder))
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "waiting-human" {
		t.Fatalf("status after the refusals %s, want waiting-human", got.Status)
	}
	line := timelineStep(t, objects, "t1", "split_refused")
	if !slices.Equal(asStrings(line["findings"]), want) {
		t.Fatalf("timeline findings %v, want %q", line["findings"], want)
	}
}

// asStrings is a timeline findings list, its entries as strings.
func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, len(list))
	for i, e := range list {
		out[i], _ = e.(string)
	}
	return out
}

// FileSplit files the split's cards like a plan's, the member who split
// the card their author, budgets by size, the dependencies among them, and
// puts them into lane ready at main's head, where their workflows start;
// the card's issue names them.
func TestFileSplitStartsTheCards(t *testing.T) {
	big := Task{ID: "t-big", Kind: "agent", Status: "in-review", Lane: policy.LaneReady, Process: "spec", Repository: "linkshort/linkshort",
		IssueNumber: 1, PRNumber: 9, Author: "alice", AuthorWriter: true, Priority: 2, Objective: "Links\n\nThe links page and API."}
	fj := &planForgejo{issues: map[int64]*planIssue{1: {title: "Links", poster: "alice"}}, next: 1}
	runner, _, store := newTestRunner(t, big, newFakeKube(), &fakeObjectStore{})
	runner.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	raw, sha, _ := planSHA(splitPlan())
	store.AddPlanRevision(context.Background(), "t-big", raw, sha)

	val, err := execActivity(t, runner, ActivityFileSplit, "t-big", 1, "sebastian", int64(7))
	res := getInto[ChildrenResult](t, val, err)
	if !slices.Equal(res.Issues, []int64{2, 3}) {
		t.Fatalf("issues %v", res.Issues)
	}
	cards, _ := store.PlanCards(context.Background(), "t-big")
	byKey := map[string]Task{}
	for _, c := range cards {
		byKey[c.PlanKey] = c
	}
	api, web := byKey["api"], byKey["web"]
	for _, c := range []struct {
		card  Task
		issue int64
		eur   float64
	}{{api, 2, 5}, {web, 3, 5}} {
		k := c.card
		if k.IssueNumber != c.issue || k.Status != "queued" || k.Lane != policy.LaneReady || k.SHA != "m1" || k.BudgetEUR != c.eur ||
			k.Process != "spec" || k.Author != "sebastian" || !slices.Equal(k.RequiredCapabilities, []string{policy.ClassCoder}) {
			t.Fatalf("card %s: %+v", k.PlanKey, k)
		}
		if labels := fj.issues[c.issue].labels; !slices.Equal(labels, []string{LabelAgent, "state/queued"}) {
			t.Fatalf("labels of #%d: %v", c.issue, labels)
		}
	}
	if !strings.HasPrefix(fj.issues[3].body, "Card web of the split of #1 (2 of 2), accepted by sebastian.") {
		t.Fatalf("body of #3:\n%s", fj.issues[3].body)
	}
	if deps := store.deps[web.ID]; !slices.Equal(deps, []string{api.ID}) || !slices.Equal(fj.issues[3].deps, []int64{2}) || store.deps[api.ID] != nil {
		t.Fatalf("deps %v, issue deps %v", store.deps, fj.issues[3].deps)
	}
	if started := runner.cfg.Workflows.(*fakeWorkflows).started; !slices.Equal(started, []string{api.ID, web.ID}) {
		t.Fatalf("started %v", started)
	}
	if len(fj.comments) != 1 || fj.comments[0].issue != 1 || fj.comments[0].body != "factoryd: split into #2, #3; this card closes." {
		t.Fatalf("comments %+v", fj.comments)
	}
}

// newSplitHarness is the gate harness on a pull request classed large:
// RunSplit answers results in order, FileSplit files #12 and #13; calls
// are both activities' arguments.
func newSplitHarness(t *testing.T, results ...PlanResult) (*gateHarness, *[]string) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), func(g *gateHarness) {
		g.gate = PolicyGate{Class: policy.ClassLarge, Outcome: policy.OutcomeRequiresApproval, Lines: 1400, LargeLines: 1000, PolicyVersion: 7}
	})
	var calls []string
	g.env.OnActivity(ActivityRunSplit, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			calls = append(calls, fmt.Sprintf("run %s %d %q", model, pin, findings))
			return results[len(calls)-1], nil
		}).After(planSession)
	g.env.OnActivity(ActivityFileSplit, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
			calls = append(calls, fmt.Sprintf("file %d %s %d", revision, approver, pin))
			return ChildrenResult{Issues: []int64{12, 13}}, nil
		})
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	return g, &calls
}

// The member's split at the approval gate: the planner's passing answer
// is filed and the card ends closed, split into its cards, unmerged.
func TestSplitFilesTheCardsAndCloses(t *testing.T) {
	g, calls := newSplitHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	g.signalAfter(time.Hour, SignalSplit, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" || len(g.finals) != 1 || g.finals[0][1] != "split into #12, #13" {
		t.Fatalf("final %q, finals %v", final, g.finals)
	}
	if !slices.Equal(*calls, []string{"run factory/coding-frontier 7 []", "file 1 sebastian 7"}) || slices.Contains(g.steps, "merge") {
		t.Fatalf("calls %q, steps %v", *calls, g.steps)
	}
}

// A failing answer goes back to the planner with its findings once; the
// second failure asks a human with them, and the approval wait goes on.
func TestSplitFailsTwiceThenAsksAHuman(t *testing.T) {
	missing := []string{"split: no card's directories cover web/app.css"}
	four := []string{"plan: 4 cards; this process allows 2 to 3"}
	g, calls := newSplitHarness(t, PlanResult{Findings: missing}, PlanResult{Findings: four})
	g.signalAfter(time.Hour, SignalSplit, TaskSignal{Author: "sebastian"})
	g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final %q, finals %v", final, g.finals)
	}
	want := []string{"run factory/coding-frontier 7 []", `run factory/coding-frontier 7 ["split: no card's directories cover web/app.css"]`}
	if !slices.Equal(*calls, want) {
		t.Fatalf("calls %q, want %q", *calls, want)
	}
	ask := "The split failed its check twice; approve, deny or split the pull request again. The findings:\n- plan: 4 cards; this process allows 2 to 3"
	if !slices.Equal(g.escalated, []string{ask}) {
		t.Fatalf("escalated %q", g.escalated)
	}
}

// A split whose session could not start asks a human at once, with the
// start failure and the split's own way on, instead of a second session
// that would fail the same way; the approval wait goes on.
func TestSplitThatCannotStartAsksAHumanAtOnce(t *testing.T) {
	ask := "the sandbox cannot start at image pull, and no session can fix it: push the worker image or correct its reference, " +
		"then split the pull request again, or approve or deny it.\nstart failed: image pull: container clone waits with ErrImagePull: not found"
	g, calls := newSplitHarness(t, PlanResult{Escalate: ask, StartFailed: true})
	g.signalAfter(time.Hour, SignalSplit, TaskSignal{Author: "sebastian"})
	g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final %q, finals %v", final, g.finals)
	}
	if !slices.Equal(*calls, []string{"run factory/coding-frontier 7 []"}) || !slices.Equal(g.escalated, []string{ask}) {
		t.Fatalf("calls %q, escalated %q; want one session and the start failure asked", *calls, g.escalated)
	}
}

// A split whose session the gateway refused at a cap asks a human at once
// with the refusal and when the day cap lets a split run again, instead of
// a second session no cap would let spend; the approval wait goes on.
func TestSplitAtACapAsksAHumanAtOnce(t *testing.T) {
	capped := CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedDayCap, Reason: "free headroom -1.000000 EUR"}, At: time.Date(2026, 10, 14, 20, 43, 0, 0, time.UTC)}
	g, calls := newSplitHarness(t, PlanResult{Findings: []string{"plan: the session ended without a last message: refused"}, Capped: &capped})
	g.signalAfter(time.Hour, SignalSplit, TaskSignal{Author: "sebastian"})
	g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final %q, finals %v", final, g.finals)
	}
	ask := "the gateway refused the split's session at a cap (refused_day_cap): split the pull request again once the day's free headroom funds a session, " +
		"at the latest when the UTC day turns at 2026-10-15T00:00:00Z, or approve or deny it.\nfree headroom -1.000000 EUR"
	if !slices.Equal(*calls, []string{"run factory/coding-frontier 7 []"}) || !slices.Equal(g.escalated, []string{ask}) {
		t.Fatalf("calls %q, escalated %q; want one session and the cap asked", *calls, g.escalated)
	}
	if got := escalationSource(ask); got != "split_cap" {
		t.Fatalf("escalation source = %q, want split_cap", got)
	}
}

// A split that arrives with a change request is dropped with the stale
// approvals: the change request sends the card back to its agent, and the
// next approval request, for the new head, splits nothing on its own.
func TestSplitSentWithAChangeRequestIsDropped(t *testing.T) {
	g, calls := newSplitHarness(t)
	g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "smaller", PR: 9})
	g.signalAfter(time.Hour, SignalSplit, TaskSignal{Author: "sebastian"})
	g.signalAfter(5*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.sessions) != 2 || len(*calls) != 0 {
		t.Fatalf("final %q, finals %v, sessions %d, split calls %q", final, g.finals, len(g.sessions), *calls)
	}
	if !slices.Equal(g.merges, []string{"9 c2 sebastian"}) {
		t.Fatalf("merges %v", g.merges)
	}
}

// The gate supervisor's cancel of a split session is a failed try, not
// the card's end: the next session gets its reason as the finding.
func TestSplitCancelledByTheGateSupervisorIsATry(t *testing.T) {
	g, calls := newSplitHarness(t, PlanResult{Revision: 1}, PlanResult{Revision: 1})
	g.onGateSupervisor(GateDecision{Action: ActionCancel, Diagnosis: "stuck"})
	g.signalAfter(time.Hour, SignalSplit, TaskSignal{Author: "sebastian"})
	g.tripAfter(time.Hour+2*time.Minute, ActivityRunSplit, 1)
	if final := g.run(t); final != "closed" || g.finals[0][1] != "split into #12, #13" {
		t.Fatalf("final %q, finals %v", final, g.finals)
	}
	want := `run factory/coding-frontier 7 ["split: the gate supervisor cancelled RunSplit: stuck"]`
	if n := len(*calls); n < 2 || (*calls)[n-2] != want || (*calls)[n-1] != "file 1 sebastian 7" {
		t.Fatalf("calls %q", *calls)
	}
}

// A split's escalation tells the member what answers it: the board's
// approval card, not a reply on the issue.
func TestSplitEscalationNamesTheBoard(t *testing.T) {
	big := Task{ID: "t1", Kind: "agent", Status: "waiting-human", Objective: "Links", Repository: "sf/r", SHA: "abc123", IssueNumber: 5, PRNumber: 9}
	runner, hub, _ := newTestRunner(t, big, newFakeKube(), &fakeObjectStore{})
	ask := splitFailed + "; approve, deny or split the pull request again. The findings:\n- plan: 4 cards; this process allows 2 to 3"
	if _, err := execActivity(t, runner, ActivityEscalate, "t1", ask, []string(nil)); err != nil {
		t.Fatal(err)
	}
	if len(hub.comments) != 1 || !strings.HasSuffix(hub.comments[0], "```\n\nApprove, deny or split it on the board.") || strings.Contains(hub.comments[0], "Reply on this issue") {
		t.Fatalf("comments %q", hub.comments)
	}
}
