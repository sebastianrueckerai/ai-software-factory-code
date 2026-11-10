package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

const timeCapAsk = "the planner used its agent time and its automatic second session without filing a plan: an answer starts one more planner session"

// The time-cap ask names the draft and how many findings refused it, in
// the case's facts and the issue comment's face, outside the reason that
// keys the case: another count is the same case. With no draft, or for any
// other ask, nothing is added.
func TestTimeCapAskNamesTheDraft(t *testing.T) {
	r, store, fj := outsidePlanRig(t)
	ctx := context.Background()
	if got, err := r.planDraftNote(ctx, "t-b", timeCapAsk); err != nil || got != "" {
		t.Fatalf("no draft: %q, %v", got, err)
	}
	if err := store.SetPlanDraft(ctx, "t-b", json.RawMessage(`{}`), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.planDraftNote(ctx, "t-b", "something else"); got != "" {
		t.Fatalf("another ask: %q", got)
	}
	want := "The planner's last refused filing is kept as a draft, refused for 2 findings."
	first, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: timeCapAsk}, tripAttempt{N: 1})
	if err != nil || first.CaseID == 0 || first.Comment == 0 {
		t.Fatalf("first trip %+v, %v", first, err)
	}
	c, _ := store.CaseByID(ctx, first.CaseID)
	if c.Facts["plan_draft"] != want || strings.Contains(c.Problem, "draft") || strings.Contains(c.StateKey, "draft") {
		t.Fatalf("case facts %v, problem %q, key %q; want the note as its own fact", c.Facts, c.Problem, c.StateKey)
	}
	var face string
	for _, cm := range fj.comments {
		if cm.id == first.Comment {
			face = cm.body
		}
	}
	if !strings.Contains(face, want) || !strings.Contains(face, timeCapAsk) {
		t.Fatalf("the face %q lacks the note or the ask", face)
	}
	if err := store.SetPlanDraft(ctx, "t-b", json.RawMessage(`{}`), []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}); err != nil {
		t.Fatal(err)
	}
	second, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: timeCapAsk}, tripAttempt{N: 1})
	if err != nil || second.CaseID != first.CaseID {
		t.Fatalf("second trip %+v, %v; want the same case %d", second, err, first.CaseID)
	}
}

// The Escalate activity, which cases-off runs use, adds the same note.
func TestEscalateNamesTheDraft(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{1: {"state/running"}}}
	runner, store, _ := newActivityRunner(t, recordPlanTask(), fj)
	ctx := context.Background()
	if err := store.SetPlanDraft(ctx, "t-plan", json.RawMessage(`{}`), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execActivity(t, runner, ActivityEscalate, "t-plan", timeCapAsk); err != nil {
		t.Fatal(err)
	}
	if e := store.intakeEvents; len(e) != 1 || e[0].Body != timeCapAsk {
		t.Fatalf("the escalation = %+v, want the plain ask", e)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "refused for 2 findings.") {
		t.Fatalf("the face %q lacks the note", fj.comments)
	}
}

// The draft's brief masks URL userinfo and a bearer value, in its plan and
// its findings.
func TestDraftBriefIsScrubbed(t *testing.T) {
	const leak = "https://user:pass@host/x Authorization: Bearer abcdefghijklmnopqrstuvwx"
	r := &Runner{Scrub: redact.New()}
	got := r.draftBrief(PlanDraft{Plan: json.RawMessage(`{"summary":"` + leak + `"}`), Findings: []string{"plan: " + leak}}, nil)
	if strings.Contains(got, "user:pass") || strings.Contains(got, "abcdefghijklmnopqrstuvwx") || strings.Count(got, "[redacted]") < 4 {
		t.Fatalf("the brief leaks, or masks too little: %q", got)
	}
}

// A plan over 32 KiB is cut there with a line that says so and how large
// it was; a finding the workflow carries already is not repeated.
func TestDraftBriefIsBounded(t *testing.T) {
	r := &Runner{Scrub: redact.New()}
	plan := `"` + strings.Repeat("x", 40000) + `"`
	got := r.draftBrief(PlanDraft{Plan: json.RawMessage(plan), Findings: []string{"carried", "new"}}, []string{"carried"})
	if !strings.Contains(got, "- new\n") || strings.Contains(got, "- carried") {
		t.Fatalf("findings in %q", got[:200])
	}
	if !strings.HasSuffix(got, "```\nthe draft is cut here at 32768 of its 40002 bytes") || len(got) > 32768+600 {
		t.Fatalf("the cut: %d bytes, ends %q", len(got), got[len(got)-80:])
	}
	short := r.draftBrief(PlanDraft{Plan: json.RawMessage(`{}`), Findings: []string{"x"}}, nil)
	if strings.Contains(short, "cut here") {
		t.Fatalf("a small draft is cut: %q", short)
	}
}

// Every accepted filing deletes the draft, the re-file of the newest
// revision by value included, and the next session's brief carries none.
func TestAcceptedRefileDropsTheDraft(t *testing.T) {
	runner, store, kube := draftRig(t)
	ctx := context.Background()
	if res, err := runner.FilePlan(ctx, "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 1 {
		t.Fatalf("revision 1: %+v, %v", res, err)
	}
	over := testPlan()
	over.Cards[1].Size = "huge"
	if _, err := runner.FilePlan(ctx, "t-plan", planJSON(t, over)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.PlanDraft(ctx, "t-plan"); !ok {
		t.Fatal("no draft after the refusal")
	}
	if res, err := runner.FilePlan(ctx, "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 1 {
		t.Fatalf("the re-file: %+v, %v", res, err)
	}
	if _, ok, _ := store.PlanDraft(ctx, "t-plan"); ok {
		t.Fatal("the draft outlived the accepted re-file")
	}
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string{})
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, "no plan"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if env := podEnv(kube.podAt(0))["FINDINGS"]; strings.Contains(env, "kept as a draft") {
		t.Fatalf("the brief carries a draft: %s", env)
	}
}

// Each refusal of FilePlan keeps the plan and its findings as the draft.
func TestEveryRefusalKeepsADraft(t *testing.T) {
	for name, tc := range map[string]struct {
		rig  func(t *testing.T) (*Runner, *fakeStore, string)
		want string
	}{
		"a companion that cannot be read": {func(t *testing.T) (*Runner, *fakeStore, string) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/repos/sf/locked/commits" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				io.WriteString(w, `[{"sha":"82ab246"}]`)
			}))
			t.Cleanup(srv.Close)
			runner, _, store := newTestRunner(t, recordPlanTask(), newFakeKube(), &fakeObjectStore{})
			runner.forgejo = &Forgejo{BaseURL: srv.URL, Seeded: testSeeded}
			v := browserInForce(t)
			if name, c, ok := v.ProjectOf("linkshort/linkshort"); ok {
				c.Companions = []policy.Companion{{Repository: "sf/locked"}}
				v.Projects[name] = c
			}
			runner.cfg.Policy = func() policy.Version { return v }
			return runner, store, planJSON(t, testPlan())
		}, companionUnread("sf/locked")},
		"an earlier round's key": {func(t *testing.T) (*Runner, *fakeStore, string) {
			r, store, _, _ := roundTwoRig(t, nil)
			answer := roundTwoAnswer()
			links := roundOne().Cards[1]
			links.Round = 0
			answer.Cards = append(answer.Cards, links)
			return r, store, planJSON(t, answer)
		}, "the key of a merged card of round 1"},
		"a card size": {func(t *testing.T) (*Runner, *fakeStore, string) {
			r, store, _ := draftRig(t)
			over := testPlan()
			over.Cards[1].Size = "huge"
			return r, store, planJSON(t, over)
		}, "size huge"},
		"a question over its bound": {func(t *testing.T) (*Runner, *fakeStore, string) {
			r, store, _ := draftRig(t)
			long := testPlan()
			long.Questions = []PlanQuestion{{Text: strings.Repeat("x", MaxQuestion+1), Choices: []string{"a", "b"}}}
			return r, store, planJSON(t, long)
		}, "question 1: text is over"},
		"a criterion changed under its id": {func(t *testing.T) (*Runner, *fakeStore, string) {
			r, store, _ := draftRig(t)
			if res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 1 {
				t.Fatalf("revision 1: %+v, %v", res, err)
			}
			changed := testPlan()
			changed.Criteria[0].Expected = "200"
			return r, store, planJSON(t, changed)
		}, "revision 1 holds this id with another definition"},
		"a take-off with no reason": {func(t *testing.T) (*Runner, *fakeStore, string) {
			r, store, _ := draftRig(t)
			if res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 1 {
				t.Fatalf("revision 1: %+v, %v", res, err)
			}
			off := testPlan()
			off.Cards[1].Brief = "Create and list links."
			off.TakeOffMain = []string{"links-api"}
			return r, store, planJSON(t, off)
		}, "take_off_main names links-api with no reason"},
	} {
		t.Run(name, func(t *testing.T) {
			r, store, plan := tc.rig(t)
			ctx := context.Background()
			res, err := r.FilePlan(ctx, "t-plan", plan)
			if err != nil || res.Revision > 1 || !strings.Contains(strings.Join(res.Findings, "\n"), tc.want) {
				t.Fatalf("filing %+v, %v; want a refusal naming %q", res, err, tc.want)
			}
			d, ok, _ := store.PlanDraft(ctx, "t-plan")
			if !ok || len(d.Findings) == 0 || !slices.Equal(d.Findings, res.Findings) || len(d.Plan) == 0 {
				t.Fatalf("draft %+v, %v; want the filing's findings %q", d, ok, res.Findings)
			}
		})
	}
}
