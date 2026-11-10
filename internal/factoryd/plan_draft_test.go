package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The draft is the task's one row: a second one replaces the first, a
// stored revision deletes it, and while it stands the task has no revision
// and no card.
func TestPGStorePlanDraft(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plan_draft"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{ID: "t-plan", Status: "running", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Track: "plan", IssueNumber: 20}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PlanDraft(ctx, "t-plan"); err != nil || ok {
		t.Fatalf("a draft before any filing: %v, %v", ok, err)
	}
	if err := s.SetPlanDraft(ctx, "t-plan", json.RawMessage(`{"summary":"one"}`), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlanDraft(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), []string{"c"}); err != nil {
		t.Fatal(err)
	}
	d, ok, err := s.PlanDraft(ctx, "t-plan")
	if err != nil || !ok || !slices.Equal(d.Findings, []string{"c"}) || !strings.Contains(string(d.Plan), `"two"`) {
		t.Fatalf("draft %+v, %v, %v; want the second filing's", d, ok, err)
	}
	if _, ok, _ := s.PlanRevision(ctx, "t-plan", 0); ok {
		t.Fatal("a draft is a revision")
	}
	if cards, err := s.PlanCards(ctx, "t-plan"); err != nil || len(cards) != 0 {
		t.Fatalf("a draft opened cards %v, %v", cards, err)
	}
	if n, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"three"}`), "s3"); err != nil || n != 1 {
		t.Fatalf("revision %d, %v", n, err)
	}
	if _, ok, _ := s.PlanDraft(ctx, "t-plan"); ok {
		t.Fatal("the draft outlived the revision")
	}
}

func draftRig(t *testing.T) (*Runner, *fakeStore, *fakeKube) {
	t.Helper()
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, recordPlanTask(), kube, &fakeObjectStore{})
	v := fixtureVersion(t)
	runner.cfg.Policy = func() policy.Version { return v }
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, id == 7, nil }
	return runner, store, kube
}

// A refused filing keeps its plan and findings as the draft, the next
// refusal replaces it, and the planner session that follows starts from it;
// the stored revision deletes it.
func TestFilePlanKeepsTheRefusedPlanAsADraft(t *testing.T) {
	runner, store, kube := draftRig(t)
	ctx := context.Background()
	over := testPlan()
	over.Cards[1].Size = "huge"
	res, err := runner.FilePlan(ctx, "t-plan", planJSON(t, over))
	if err != nil {
		t.Fatal(err)
	}
	finding := "plan: card links-api: size huge: split it; a card is one package, one page or one set of endpoints"
	if !slices.Equal(res.Findings, []string{finding}) {
		t.Fatalf("findings %q", res.Findings)
	}
	d, ok, _ := store.PlanDraft(ctx, "t-plan")
	if !ok || !slices.Equal(d.Findings, []string{finding}) || !strings.Contains(string(d.Plan), `"huge"`) {
		t.Fatalf("draft %+v, %v", d, ok)
	}
	if _, ok, _ := store.PlanRevision(ctx, "t-plan", 0); ok {
		t.Fatal("a refused plan is a revision")
	}

	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string{"an earlier finding"})
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, "no plan"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal([]byte(podEnv(kube.podAt(0))["FINDINGS"]), &got); err != nil || len(got) != 2 || got[0] != "an earlier finding" {
		t.Fatalf("FINDINGS %q, %v", got, err)
	}
	if !strings.Contains(got[1], "your last filing was refused and kept as a draft") || !strings.Contains(got[1], "- "+finding+"\n") || !strings.Contains(got[1], `"huge"`) {
		t.Fatalf("the draft's brief %q", got[1])
	}

	other := testPlan()
	other.Cards[0].Size = "huge"
	if _, err := runner.FilePlan(ctx, "t-plan", planJSON(t, other)); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := store.PlanDraft(ctx, "t-plan"); len(d.Findings) != 1 || strings.Contains(d.Findings[0], "links-api") {
		t.Fatalf("the second refusal did not replace the draft: %+v", d)
	}

	if res, err := runner.FilePlan(ctx, "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 1 {
		t.Fatalf("filed %+v, %v", res, err)
	}
	if _, ok, _ := store.PlanDraft(ctx, "t-plan"); ok {
		t.Fatal("the draft outlived the stored revision")
	}
}

// The time-cap ask names the draft and how many findings refused it; with
// no draft, or for any other ask, it reads as it was.
func TestTimeCapAskNamesTheDraft(t *testing.T) {
	task := recordPlanTask()
	runner, store, _ := newActivityRunner(t, task, &prForgejo{labels: map[int64][]string{1: {"state/running"}}})
	ctx := context.Background()
	ask := "the planner used its agent time and its automatic second session without filing a plan: an answer starts one more planner session"
	if got, err := runner.withPlanDraft(ctx, "t-plan", ask); err != nil || got != ask {
		t.Fatalf("no draft: %q, %v", got, err)
	}
	if err := store.SetPlanDraft(ctx, "t-plan", json.RawMessage(`{}`), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	want := ask + ". The planner's last refused filing is kept as a draft, refused for 2 findings."
	if got, err := runner.withPlanDraft(ctx, "t-plan", ask); err != nil || got != want {
		t.Fatalf("with a draft: %q, %v; want %q", got, err, want)
	}
	if got, _ := runner.withPlanDraft(ctx, "t-plan", "something else"); got != "something else" {
		t.Fatalf("another ask: %q", got)
	}
	if _, err := execActivity(t, runner, ActivityEscalate, "t-plan", ask); err != nil {
		t.Fatal(err)
	}
	if e := store.intakeEvents; len(e) != 1 || e[0].Body != want {
		t.Fatalf("the escalation = %+v, want %q", e, want)
	}
}

// The draft's brief masks URL userinfo and a bearer value, in its plan and
// its findings.
func TestDraftBriefIsScrubbed(t *testing.T) {
	const leak = "https://user:pass@host/x Authorization: Bearer abcdefghijklmnopqrstuvwx"
	r := &Runner{Scrub: redact.New()}
	got := r.draftBrief(PlanDraft{Plan: json.RawMessage(`{"summary":"` + leak + `"}`), Findings: []string{"plan: " + leak}})
	if strings.Contains(got, "user:pass") || strings.Contains(got, "abcdefghijklmnopqrstuvwx") || strings.Count(got, "[redacted]") < 4 {
		t.Fatalf("the brief leaks, or masks too little: %q", got)
	}
}
