package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestBriefAnswer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		brief   string
		detail  string
		wantErr bool
	}{{
		name:    "plain json",
		content: `{"brief": "PR #12 asks to merge the token refresh.", "detail": "- risk class small\n- review: pass"}`,
		brief:   "PR #12 asks to merge the token refresh.",
		detail:  "- risk class small\n- review: pass",
	}, {
		name:    "fenced json",
		content: "```json\n{\"brief\": \"One.\", \"detail\": \"- a\\n- b\"}\n```",
		brief:   "One.",
		detail:  "- a\n- b",
	}, {
		name:    "detail lines trimmed",
		content: `{"brief": "B", "detail": "\n  - a  \n\n- b \n"}`,
		brief:   "B",
		detail:  "- a\n- b",
	}, {
		name:    "no brief",
		content: `{"brief": "", "detail": "- x"}`,
		wantErr: true,
	}, {
		name:    "not json",
		content: "The PR merges the refresh.",
		wantErr: true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			brief, detail, err := briefAnswer(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q / %q", brief, detail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if brief != tt.brief || detail != tt.detail {
				t.Fatalf("got %q / %q, want %q / %q", brief, detail, tt.brief, tt.detail)
			}
		})
	}
}

// fakeBriefStore records the brief writer's writes.
type fakeBriefStore struct {
	BriefStore
	tasks     map[string]Task
	rows      []ApprovalRequest
	pending   []ApprovalRequest
	revisions map[string]PlanRevision
	verdicts  map[string][]CriterionVerdict
	brief     map[int64][2]string
	failed    map[int64]string
	events    []Event
	taskErr   error
	cases     []Case
	steps     map[int64][]CaseStep
	answered  *time.Time
	chat      []string
	chatSpent float64
	chatRaise float64
	perRepo   []int
	caseBrief map[int64][2]string
	// caseFailed marks the cases the writer failed on
	caseFailed map[int64]string
}

func (f *fakeBriefStore) Task(ctx context.Context, id string) (Task, bool, error) {
	t, ok := f.tasks[id]
	return t, ok, f.taskErr
}

func (f *fakeBriefStore) BrieflessApprovals(ctx context.Context, perProject int) ([]ApprovalRequest, error) {
	f.perRepo = append(f.perRepo, perProject)
	return f.rows, nil
}

func (f *fakeBriefStore) SetApprovalBrief(ctx context.Context, id int64, brief, detail string) error {
	f.brief[id] = [2]string{brief, detail}
	return nil
}

func (f *fakeBriefStore) SetApprovalBriefError(ctx context.Context, id int64, e string) error {
	f.failed[id] = e
	for i, a := range f.rows {
		if a.ID == id {
			f.rows[i].BriefError = e
		}
	}
	return nil
}

func (f *fakeBriefStore) PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error) {
	rev, ok := f.revisions[id]
	return rev, ok, nil
}

func (f *fakeBriefStore) CriteriaVerdicts(ctx context.Context, taskID string) ([]CriterionVerdict, error) {
	return f.verdicts[taskID], nil
}

func (f *fakeBriefStore) PlanCards(ctx context.Context, plan string) ([]Task, error) {
	var out []Task
	for _, t := range f.tasks {
		if t.Parent == plan && t.Source == SourcePlan {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeBriefStore) ApprovalRequests(ctx context.Context, status string) ([]ApprovalRequest, error) {
	return f.pending, nil
}

func (f *fakeBriefStore) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (Task, error) {
	f.chat = append(f.chat, name)
	return Task{ID: ChatTaskID(name, at), Kind: KindChat, Status: "running", Repository: repository, BudgetEUR: budget + f.chatRaise, SpentEUR: f.chatSpent}, nil
}

func (f *fakeBriefStore) BrieflessCases(ctx context.Context, perRepo int) ([]Case, error) {
	f.perRepo = append(f.perRepo, perRepo)
	return slices.Clone(f.cases), nil
}

func (f *fakeBriefStore) SetCaseBrief(ctx context.Context, id int64, background, recommendation string) error {
	f.caseBrief[id] = [2]string{background, recommendation}
	return nil
}

func (f *fakeBriefStore) SetCaseBriefError(ctx context.Context, id int64, e string) error {
	f.caseFailed[id] = e
	for i, c := range f.cases {
		if c.ID == id {
			f.cases[i].BriefError = e
		}
	}
	return nil
}

// SetCaseWake sets the case's wake as PGStore.SetCaseWake does, so the
// pass waits a freeze out at it.
func (f *fakeBriefStore) SetCaseWake(ctx context.Context, id int64, rung int, on []string, at *time.Time) error {
	for i, c := range f.cases {
		if c.ID == id {
			f.cases[i].OwnerRung, f.cases[i].WakeOn, f.cases[i].NextWakeAt = rung, on, at
		}
	}
	return nil
}

func (f *fakeBriefStore) CaseSteps(ctx context.Context, caseID int64) ([]CaseStep, error) {
	return f.steps[caseID], nil
}

func (f *fakeBriefStore) LastAnswerAt(ctx context.Context, taskID string) (*time.Time, error) {
	return f.answered, nil
}

func (f *fakeBriefStore) Record(ctx context.Context, e Event) (int64, error) {
	f.events = append(f.events, e)
	return int64(len(f.events)), nil
}

// briefGateway answers the writer's one call as want says.
func briefGateway(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// briefTestBudget is the writer's model client's token budget.
var briefTestBudget = TokenBudget{AnswerTokens: 512, HeadroomTokens: 512}

// briefLife is the card's life the writer reads: card #12's head alone.
func briefLife(context.Context, string) (CardDigest, error) {
	return CardDigest{Head: DigestHead{Issue: 12, BudgetEUR: 3, SpentEUR: 0.4, Status: "in-review", Gate: GateApproval}}, nil
}

func briefTask() Task {
	return Task{ID: "t1", Repository: "o/r", RequiredCapabilities: []string{policy.ClassCoder}}
}

func briefVersion(url string) policy.Version {
	return policy.Version{Tree: policy.Tree{Document: policy.Document{Classes: map[string]policy.Class{
		policy.ClassCoder: {SupervisorModel: "supervisor"},
	}, Limits: policy.Limits{SupervisorTimeout: 6 * time.Minute}},
		Projects: map[string]policy.ProjectConfig{"proj": {Repository: "o/r"}, "code": {Repository: "sf/code"}}}}
}

func planSpec(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(PlanSpec{Revision: 2})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBriefWriterPass(t *testing.T) {
	row := ApprovalRequest{ID: 7, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r",
		Spec: planSpec(t), Reason: "a question", CreatedAt: time.Unix(0, 0).UTC()}
	answer := `{"choices":[{"message":{"content":"{\"brief\": \"PR #12 waits on you.\", \"detail\": \"- small risk\"}"}}]}`
	gw := briefGateway(t, http.StatusOK, answer)
	defer gw.Close()
	store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
		rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	w := &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := store.brief[7]
	if !ok {
		t.Fatal("the brief was not filed")
	}
	if got[0] != "PR #12 waits on you." || got[1] != "- small risk" {
		t.Fatalf("got %q / %q", got[0], got[1])
	}
	if len(store.failed) != 0 || len(store.events) != 0 {
		t.Fatalf("a good call marked or recorded a failure: %v %v", store.failed, store.events)
	}
}

// The writer reads the card's life once per approval it briefs, and the
// model reads it under its heading beside the facts. A life that cannot
// be read marks the row as a failed call does, and no model runs.
func TestBriefWriterReadsTheCardsLife(t *testing.T) {
	var body []byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"brief\": \"PR #12 waits on you.\", \"detail\": \"- small risk\"}"}}]}`))
	}))
	defer gw.Close()
	row := ApprovalRequest{ID: 7, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r", Spec: planSpec(t)}
	store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()}, rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	var read []string
	w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) },
		CardDigest: func(ctx context.Context, taskID string) (CardDigest, error) {
			read = append(read, taskID)
			return briefLife(ctx, taskID)
		}}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 || read[0] != "t1" {
		t.Fatalf("the writer read the lives of %v, want t1's once", read)
	}
	for _, want := range []string{"The card's life", "#12 · budget 3.00 spent 0.40 holds 0.00 EUR · in-review, approval"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the model's call lacks %q: %s", want, body)
		}
	}
	if _, ok := store.brief[7]; !ok {
		t.Fatal("the brief was not filed")
	}

	body = nil
	store = &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()}, rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	w.Store = store
	w.CardDigest = func(context.Context, string) (CardDigest, error) {
		return CardDigest{}, errors.New("the timeline does not read")
	}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body != nil || store.failed[7] != "the card's life: the timeline does not read" || len(store.events) != 1 {
		t.Fatalf("an unread life: call %s, mark %q, events %v; want no call, the row marked and the failure recorded", body, store.failed[7], store.events)
	}
}

// The brief's call is a supervisor call and waits as long as the
// policy's supervisor_timeout lets one: a model slower than the bound
// fails the row, one inside it files the brief.
func TestBriefWriterTakesThePolicysTimeout(t *testing.T) {
	answer := `{"choices":[{"message":{"content":"{\"brief\": \"PR #12 waits on you.\", \"detail\": \"- small risk\"}"}}]}`
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(answer))
	}))
	defer gw.Close()
	for _, tt := range []struct {
		bound time.Duration
		filed bool
	}{{50 * time.Millisecond, false}, {5 * time.Second, true}} {
		store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
			rows:  []ApprovalRequest{{ID: 7, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r", Spec: planSpec(t)}},
			brief: map[int64][2]string{}, failed: map[int64]string{}}
		v := briefVersion(gw.URL)
		v.Document.Limits.SupervisorTimeout = tt.bound
		w := &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return v }}
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, filed := store.brief[7]; filed != tt.filed || (store.failed[7] != "") == tt.filed {
			t.Fatalf("bound %s: filed %v, failure %q; want filed %v", tt.bound, filed, store.failed[7], tt.filed)
		}
	}
}

func TestBriefWriterFallback(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "refused", status: http.StatusForbidden, body: "no headroom"},
		{name: "no brief in answer", status: http.StatusOK, body: `{"choices":[{"message":{"content":"hello"}}]}`},
		{name: "broken body", status: http.StatusOK, body: `not json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gw := briefGateway(t, tt.status, tt.body)
			defer gw.Close()
			store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
				rows:  []ApprovalRequest{{ID: 9, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r", Spec: json.RawMessage(`{"revision":2}`)}},
				brief: map[int64][2]string{}, failed: map[int64]string{}}
			w := &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
			if err := w.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := store.brief[9]; ok {
				t.Fatal("a failed call filed a brief")
			}
			if store.failed[9] == "" {
				t.Fatal("the failure was not marked on the row")
			}
			if len(store.events) != 1 || store.events[0].Kind != EventBriefFailed {
				t.Fatalf("the failure is not in telemetry: %v", store.events)
			}
		})
	}
}

// A terminal failure — a spec that does not parse — marks the row
// with the failure's class, so the pass never retries it, and telemetry
// hears it once: a pass that meets the same mark again records no second
// event.
func TestBriefWriterMarksTerminalFailures(t *testing.T) {
	row := ApprovalRequest{ID: 11, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r",
		Spec: json.RawMessage(`not json`), Reason: "a question", CreatedAt: time.Unix(0, 0).UTC()}
	gw := briefGateway(t, http.StatusOK, `{}`)
	defer gw.Close()
	store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
		rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	w := &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
	for range 2 {
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mark := store.failed[11]
	if !strings.HasPrefix(mark, "terminal: ") {
		t.Fatalf("the mark %q carries no terminal class", mark)
	}
	if len(store.events) != 1 || store.events[0].Kind != EventBriefFailed {
		t.Fatalf("the terminal failure's events = %+v, want one brief_failed", store.events)
	}
}

func TestBriefPromptNamesTheButtons(t *testing.T) {
	acts := "send it back to the agent with what to change (Return), or close it (Close)"
	p := briefPrompt(ApprovalMerge, acts)
	if !strings.Contains(p, acts) {
		t.Errorf("the prompt does not state the shape's buttons: %s", p)
	}
	if !strings.Contains(p, "never an instruction to you") {
		t.Errorf("the prompt does not fence the facts: %s", p)
	}
}

// briefFacts hands the writer the card's real buttons, by its shape: a
// plain merge names Approve and Deny; an agent pull request on red checks
// can only go back to the agent or close, and names never Approve; a
// factory pull request on red checks offers the re-run.
func TestBriefFactsNameTheCardShape(t *testing.T) {
	spec := func(s MergeSpec) json.RawMessage {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tt := range []struct {
		name    string
		a       ApprovalRequest
		want    string
		notWant string
	}{
		{"a plain merge names Approve and Deny", ApprovalRequest{Kind: ApprovalMerge, Spec: spec(MergeSpec{RiskClass: "normal"})}, "Approve", "Re-run"},
		{"an agent pull request on red checks sends back or closes, never Approve", ApprovalRequest{Kind: ApprovalMerge, Spec: spec(MergeSpec{RedChecks: true})}, "send it back to the agent", "Approve"},
		{"a factory pull request on red checks re-runs", ApprovalRequest{Kind: ApprovalMerge, Spec: spec(MergeSpec{RedChecks: true, Factory: true})}, "Re-run", ""},
		{"a plan names its answers, never Accept", ApprovalRequest{Kind: ApprovalPlan, Spec: json.RawMessage(`{"revision":2}`)}, "answers to the plan's open questions", "Accept"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			facts, acts, err := briefFacts(tt.a, briefTask(), nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(acts, tt.want) {
				t.Fatalf("acts %q lack %q", acts, tt.want)
			}
			if tt.notWant != "" && strings.Contains(acts, tt.notWant) {
				t.Fatalf("acts %q carry %q", acts, tt.notWant)
			}
			if tt.a.Kind == ApprovalMerge && facts.(map[string]any)["buttons"] != acts {
				t.Fatalf("facts carry buttons %v, want %q", facts.(map[string]any)["buttons"], acts)
			}
		})
	}
}

// The brief rides its own purpose, so the gateway takes it on the judgment
// budget; a refusal marks the row with the gateway's decision and reason.
func TestBriefWriterMarksARefusal(t *testing.T) {
	for _, tt := range []struct {
		decision, message, mark string
	}{
		{policy.RefusedBudget, "free headroom 0.000100 EUR fits 2 output tokens", "judgment budget spent"},
		{policy.RefusedMonthCap, "the month cap is spent", "refused_month_cap: the month cap is spent"},
		{"refused_task_status", "task t1 is done", "refused_task_status: task t1 is done"},
	} {
		var purpose string
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			purpose = r.Header.Get(gateway.HeaderFactoryPurpose)
			w.Header().Set(gateway.HeaderDecision, tt.decision)
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"` + tt.message + `"}}`))
		}))
		store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
			rows:  []ApprovalRequest{{ID: 9, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r", Status: "pending", Spec: json.RawMessage(`{"revision":2}`)}},
			brief: map[int64][2]string{}, failed: map[int64]string{}}
		w := &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		gw.Close()
		if purpose != policy.PurposeBrief {
			t.Errorf("%s: the call's purpose is %q, want %q", tt.decision, purpose, policy.PurposeBrief)
		}
		if store.failed[9] != tt.mark {
			t.Errorf("%s: mark %q, want %q", tt.decision, store.failed[9], tt.mark)
		}
	}
}

// A criteria request's brief reads the plan's criteria with their
// standing verdicts and names the card's three answers (fault 7: the gap
// card showed a brief error instead).
func TestBriefFactsCarryTheCriteriaGap(t *testing.T) {
	spec, err := json.Marshal(CriteriaSpec{Task: "t-int"})
	if err != nil {
		t.Fatal(err)
	}
	a := ApprovalRequest{Kind: ApprovalCriteria, TaskID: "t-plan", Project: "sf/code",
		Spec: spec, Reason: "the plan's cards ran out with a criterion unmet"}
	criteria := []criterionFact{
		{ID: "c1", Kind: "test", Text: "go test ./... is green", Verdict: "met", Evidence: "the review read the diff"},
		{ID: "c2", Kind: "human", Text: "the feature reads well", Verdict: "open"},
		{ID: "c3", Kind: "invariant", Text: "no endpoint times out", Verdict: "waived", Evidence: "the timeout is the provider's SLA"},
	}
	facts, acts, err := briefFacts(a, briefTask(), criteria, "")
	if err != nil {
		t.Fatal(err)
	}
	m := facts.(map[string]any)
	if fmt.Sprint(m["criteria"]) != fmt.Sprint(criteria) {
		t.Errorf("criteria = %v, want %v", m["criteria"], criteria)
	}
	for _, want := range []string{"Waive", "one more bounded card", "Confirm", "no final press"} {
		if !strings.Contains(acts, want) {
			t.Errorf("acts %q lack %q", acts, want)
		}
	}
	if p := briefPrompt(a.Kind, acts); !strings.Contains(p, "criteria gap") {
		t.Errorf("the prompt does not name the card: %s", p)
	}
}

// A criteria_rerun request's brief reads the same criteria facts, the
// writer fetching them itself, and names its answers, none of which runs
// the review.
func TestBriefOfCriteriaNeverJudged(t *testing.T) {
	spec, _ := json.Marshal(CriteriaSpec{Task: "t-int"})
	a := ApprovalRequest{Kind: ApprovalCriteriaRerun, TaskID: "t-plan", Project: "sf/code", Spec: spec,
		Reason: "criteria never judged: the review of card #1380 did not run (the review never ran): c1"}
	criteria := []criterionFact{{ID: "c1", Kind: "test", Text: "a link is stored", Verdict: "open"}}
	facts, acts, err := briefFacts(a, briefTask(), criteria, "")
	if err != nil {
		t.Fatal(err)
	}
	if m := facts.(map[string]any); fmt.Sprint(m["criteria"]) != fmt.Sprint(criteria) || m["verdict_rows_of"] != "t-int" {
		t.Errorf("facts = %v, want the criteria and their rows' card", m)
	}
	for _, want := range []string{"Waive", "Confirm", "Deny", "No button runs the review"} {
		if !strings.Contains(acts, want) {
			t.Errorf("acts %q lack %q", acts, want)
		}
	}
	if p := briefPrompt(a.Kind, acts); !strings.Contains(p, "criteria never judged card") {
		t.Errorf("the prompt does not name the card: %s", p)
	}
	plan, _ := json.Marshal(Plan{Criteria: []PlanCriterion{{ID: "c1", Kind: "test", Claim: "a link is stored", Inputs: "POST /api/links", Expected: "201"}}})
	store := &fakeBriefStore{revisions: map[string]PlanRevision{"t-plan": {TaskID: "t-plan", Revision: 3, Plan: plan}}, verdicts: map[string][]CriterionVerdict{}}
	got, err := (&BriefWriter{Store: store}).criteriaFacts(context.Background(), a)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(criteria) {
		t.Fatalf("the writer's criteria = %v (%v), want %v", got, err, criteria)
	}
}

// The writer reads a criteria request's plan and verdict rows itself and
// the model's brief lands on the row, with the criteria as the model
// reads them (fault 7).
func TestBriefWriterWritesTheCriteriaBrief(t *testing.T) {
	spec, err := json.Marshal(CriteriaSpec{Task: "t-int"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(Plan{Criteria: []PlanCriterion{
		{ID: "c1", Kind: "test", Claim: "a link is stored", Inputs: "POST /api/links", Expected: "201"},
		{ID: "c2", Kind: "human", Claim: "the feature reads well"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var sent string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
				Role    string `json:"role"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				sent = m.Content
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"background\": \"Two criteria wait on you.\", \"recommendation\": \"c1 is met\"}"}}]}`))
	}))
	defer gw.Close()
	store := &fakeBriefStore{
		tasks:   map[string]Task{"t-plan": {ID: "t-plan", Repository: "sf/code", RequiredCapabilities: []string{policy.ClassCoder}}},
		pending: []ApprovalRequest{{ID: 11, Kind: ApprovalCriteria, TaskID: "t-plan", Project: "sf/code", Spec: spec, Reason: "a criterion unmet", CreatedAt: time.Unix(0, 0).UTC()}},
		cases:   []Case{{ID: 4, TaskID: "t-plan", Repository: "sf/code", Kind: CasePlanCriteria, Status: CaseAtHuman, Facts: map[string]any{"approval": float64(11)}}},
		brief:   map[int64][2]string{}, failed: map[int64]string{}, caseBrief: map[int64][2]string{}, caseFailed: map[int64]string{},
		revisions: map[string]PlanRevision{"t-plan": {TaskID: "t-plan", Revision: 3, Plan: plan}},
		verdicts: map[string][]CriterionVerdict{"t-int": {
			{Criterion: "c1", Verdict: "met", Evidence: "the review read the diff", Kind: "test", Source: "review"},
		}},
	}
	w := briefCaseWriter(store, gw)
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.caseBrief[4]; got[0] != "Two criteria wait on you." || got[1] != "c1 is met" {
		t.Fatalf("the brief = %v", got)
	}
	for _, want := range []string{"the feature reads well", `"verdict":"open"`, `"verdict":"met"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("the model's facts lack %q: %s", want, sent)
		}
	}
}

// The brief of the integration card's merge request reads the plan's
// criteria with their standing rows and tells the human to settle each
// row before Approve; a feature card's merge request carries no rows.
func TestTheMergeBriefCarriesThePlansRows(t *testing.T) {
	plan, err := json.Marshal(Plan{Criteria: []PlanCriterion{
		{ID: "size_bound", Kind: CriterionTest, Claim: "every size row stays under its bound", Inputs: "GET /sizes", Expected: "3 rows"},
		{ID: "reads_calm", Kind: CriterionJudgement, Claim: "the sizes page reads calm", Prompt: "Judge whether the page names every size."},
		{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."},
		{ID: "pages_ok", Kind: CriterionHuman, Claim: "The pages read well", Evidence: "Open /sizes and /doors at 390 px"},
		{ID: "page_links", Kind: CriterionHuman, Claim: "Every page links home", Evidence: "the links on /help"},
	}, Cards: []PlanCard{{Key: "sizes", Role: RoleFeature}, {Key: "integration", Role: RoleIntegration}}})
	if err != nil {
		t.Fatal(err)
	}
	var sent []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
				Role    string `json:"role"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				var facts map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(m.Content, ChatInputLead)), &facts)
				sent = append(sent, facts)
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"background\": \"Two rows wait on you.\", \"recommendation\": \"\"}"}}]}`))
	}))
	defer gw.Close()
	card := func(id, key string, issue int64) Task {
		return Task{ID: id, Repository: "sf/code", IssueNumber: issue, Parent: "t-plan", Source: SourcePlan, PlanKey: key,
			Status: "in-review", RequiredCapabilities: []string{policy.ClassCoder}}
	}
	spec := json.RawMessage(`{"risk_class":"standard","head_sha":"8e3a1b0c"}`)
	store := &fakeBriefStore{
		tasks: map[string]Task{"t-int": card("t-int", "integration", 5), "t-sizes": card("t-sizes", "sizes", 6)},
		pending: []ApprovalRequest{
			{ID: 31, Kind: ApprovalMerge, TaskID: "t-int", Project: "sf/code", PRNumber: 12, Spec: spec, CreatedAt: time.Unix(0, 0).UTC()},
			{ID: 32, Kind: ApprovalMerge, TaskID: "t-sizes", Project: "sf/code", PRNumber: 13, Spec: spec, CreatedAt: time.Unix(0, 0).UTC()},
		},
		cases: []Case{
			{ID: 1, TaskID: "t-int", Repository: "sf/code", Kind: CaseMergeApproval, Status: CaseAtHuman, Facts: map[string]any{"approval": float64(31)}},
			{ID: 2, TaskID: "t-sizes", Repository: "sf/code", Kind: CaseMergeApproval, Status: CaseAtHuman, Facts: map[string]any{"approval": float64(32)}},
		},
		brief: map[int64][2]string{}, failed: map[int64]string{}, caseBrief: map[int64][2]string{}, caseFailed: map[int64]string{},
		revisions: map[string]PlanRevision{"t-plan": {TaskID: "t-plan", Revision: 1, Plan: plan}},
		verdicts: map[string][]CriterionVerdict{"t-int": {
			{Criterion: "size_bound", Verdict: "met", Evidence: "blind: criterion size_bound: TestSizeBound in ./proofs/size_bound passed", Kind: CriterionTest, Source: CriteriaSourceGate},
			{Criterion: "reads_calm", Verdict: "met", Evidence: "the page names every size (model big, prompt 6b2355d78a19)", Kind: CriterionJudgement, Source: CriteriaSourceJudgement},
			{Criterion: "all_green", Verdict: "unmet", Evidence: "criterion all_green: suite ./... failed: Failed boom", Kind: CriterionInvariant, Source: CriteriaSourceGate},
		}},
	}
	w := briefCaseWriter(store, gw)
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || len(store.caseFailed) != 0 {
		t.Fatalf("the writer sent %d briefs' facts and marked %v, want two and none", len(sent), store.caseFailed)
	}
	for i := range sent {
		sent[i], _ = sent[i]["approval_request"].(map[string]any)
	}
	want := []criterionFact{
		{ID: "size_bound", Kind: CriterionTest, Text: "every size row stays under its bound", Verdict: "met", Evidence: "blind: criterion size_bound: TestSizeBound in ./proofs/size_bound passed"},
		{ID: "reads_calm", Kind: CriterionJudgement, Text: "the sizes page reads calm", Verdict: "met", Evidence: "the page names every size (model big, prompt 6b2355d78a19)"},
		{ID: "all_green", Kind: CriterionInvariant, Text: "./...", Verdict: "unmet", Evidence: "criterion all_green: suite ./... failed: Failed boom"},
		{ID: "pages_ok", Kind: CriterionHuman, Text: "The pages read well", Verdict: "open"},
		{ID: "page_links", Kind: CriterionHuman, Text: "Every page links home", Verdict: "open"},
	}
	var got []criterionFact
	raw, _ := json.Marshal(sent[0]["criteria"])
	if err := json.Unmarshal(raw, &got); err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the merge brief's criteria = %s (%v)\nwant %v", raw, err, want)
	}
	if got := sent[0]["buttons"]; got != "Settle each human criterion row first: Met or Not met on a human criterion, Waive on any row. "+
		"Approve opens once every human criterion has a verdict, then merges the pull request into main; Deny refuses it with the human's reason." {
		t.Errorf("the merge brief's buttons = %q", got)
	}
	if _, ok := sent[1]["criteria"]; ok || sent[1]["buttons"] != "Approve merges the pull request into main; Deny refuses it with the human's reason, posted on the pull request as them." {
		t.Errorf("the feature card's merge brief = %v, want no criteria and the plain buttons", sent[1])
	}
}

// A criterion as one line on a criteria card's brief: its claim, else
// its suite, else its prompt.
func TestCriterionText(t *testing.T) {
	for _, tc := range []struct {
		c    PlanCriterion
		want string
	}{
		{PlanCriterion{Kind: CriterionInvariant, Suite: "./..."}, "./..."},
		{PlanCriterion{Kind: CriterionTest, Claim: "c", Inputs: "i"}, "c"},
		{PlanCriterion{Kind: CriterionJudgement, Prompt: "p"}, "p"},
	} {
		if got := criterionText(tc.c); got != tc.want {
			t.Errorf("criterionText(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func briefCaseStore(c Case, steps ...CaseStep) *fakeBriefStore {
	return &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()}, cases: []Case{c}, steps: map[int64][]CaseStep{c.ID: steps},
		brief: map[int64][2]string{}, failed: map[int64]string{}, caseBrief: map[int64][2]string{}, caseFailed: map[int64]string{}}
}

func briefCaseWriter(store *fakeBriefStore, gw *httptest.Server) *BriefWriter {
	return &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget},
		Current: func() policy.Version { return briefVersion(gw.URL) }, Clock: func() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }}
}

// A session-cap case at the human is briefed by one call booked on the
// project's chat task, not the card; the facts carry both steps' findings
// and the digest's head line, and the answer is cut at 4000 characters for
// the background and 600 for the recommendation, marked.
func TestBriefWriterBriefsACaseOnTheJudgmentBudget(t *testing.T) {
	var body []byte
	var task, purpose string
	answer, _ := json.Marshal(map[string]any{"background": strings.Repeat("a", 4100), "recommendation": strings.Repeat("b", 700)})
	reply, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(answer)}}}})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		task, purpose = r.Header.Get(gateway.HeaderFactoryTask), r.Header.Get(gateway.HeaderFactoryPurpose)
		_, _ = w.Write(reply)
	}))
	defer gw.Close()
	c := Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman, Problem: "The session cap is spent."}
	store := briefCaseStore(c,
		CaseStep{ID: 1, Rung: 1, Actor: CheckinDecider, Found: "the check-in found a green build", Outcome: StepHandedUp},
		CaseStep{ID: 2, Rung: 2, Actor: "factory agent", Found: "rung two found the last review approved", Decision: ActExtend, Grounds: "one more session ends it", Outcome: StepHandedUp})
	if err := briefCaseWriter(store, gw).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if task != "t-chat-proj-2026-11" || purpose != policy.PurposeBrief {
		t.Fatalf("the call's task %q purpose %q, want the chat task t-chat-proj-2026-11 and brief", task, purpose)
	}
	for _, want := range []string{"the check-in found a green build", "rung two found the last review approved", "one more session ends it",
		"#12 · budget 3.00 spent 0.40 holds 0.00 EUR · in-review, approval", "offered_actions"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the facts lack %q: %s", want, body)
		}
	}
	got := store.caseBrief[5]
	if want := [2]string{strings.Repeat("a", 3999) + "…", strings.Repeat("b", 599) + "…"}; got != want {
		t.Fatalf("the case's brief = %d and %d characters, want the background cut to 4000 and the recommendation to 600, marked", len([]rune(got[0])), len([]rune(got[1])))
	}
	if len(store.caseFailed) != 0 || len(store.events) != 0 {
		t.Fatalf("a good call marked a failure: %v %v", store.caseFailed, store.events)
	}
}

// A refused judgment budget marks the case once and asks nobody for money:
// the second pass meets the same mark and records no second event, and no
// approval row carries a mark.
func TestBriefWriterMarksASpentJudgmentBudgetOnce(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(gateway.HeaderDecision, policy.RefusedBudget)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"the chat's budget is spent"}}`))
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman})
	w := briefCaseWriter(store, gw)
	for range 2 {
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if want := "judgment budget spent"; store.caseFailed[5] != want {
		t.Fatalf("the case's mark = %q, want %q", store.caseFailed[5], want)
	}
	if len(store.events) != 1 || store.events[0].Kind != EventBriefFailed || store.events[0].Detail["case"] != int64(5) {
		t.Fatalf("events = %+v, want one brief_failed of case 5", store.events)
	}
	if len(store.failed) != 0 || len(store.caseBrief) != 0 {
		t.Fatalf("approval marks %v, briefs %v; want none", store.failed, store.caseBrief)
	}
}

// One brief refusal during a freeze writes one brief row with the reset,
// sets the case's wake to the reset plus the margin, and a pass inside the
// freeze makes no call; a check-in inside the freeze records 'not run' and
// flags nothing (criterion brief_wakes_at_reset).
//
// The retry driver behind the incident's 46 brief_failed rows between
// 16:42:52 and 17:04:01 was the brief writer's own Loop (main.go wires it
// at a 5-second tick): every pass re-served every briefless case whose
// mark was not terminal and made one model call per case — a class the
// provider had frozen answered each call with the same refusal, so the
// rows piled up one per tick. The wake the freeze now sets parks the row
// (Pass drops a case whose NextWakeAt is ahead), so whatever re-drove
// the brief inside a freeze waits at the case wake instead; the monitor
// sweep wakes due cases, so the next call lands after the reset.
func TestBriefWakeAtReset(t *testing.T) {
	// the freeze: the gateway's hold answers the brief call locally, with
	// the reset it named; the margin sits a minute past it for the test
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	reset := at.Add(2 * time.Hour)
	oldMargin := caseWakeMargin
	caseWakeMargin = time.Minute
	defer func() { caseWakeMargin = oldMargin }()
	calls := 0
	frozen := true
	answer, _ := json.Marshal(map[string]any{"background": "the session cap is spent and one more session ends it", "recommendation": "extend the session"})
	reply, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(answer)}}}})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !frozen {
			_, _ = w.Write(reply)
			return
		}
		w.Header().Set(gateway.HeaderDecision, gateway.RefusedProviderLimit)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"message":"provider limit hold for class big: reset %s: five-hour limit"}}`,
			reset.Format(time.RFC3339))
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap,
		Status: CaseAtHuman, Problem: "The session cap is spent."})
	w := briefCaseWriter(store, gw)

	// the refusal during the freeze: one call, one brief_failed row whose
	// BackoffS is the seconds to the reset, the case parked at the wake
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the freeze met %d calls, want one", calls)
	}
	if want := "the class is frozen until " + reset.Format(time.RFC3339); store.caseFailed[5] != want {
		t.Fatalf("the case's mark = %q, want %q", store.caseFailed[5], want)
	}
	wake := reset.Add(caseWakeMargin)
	if store.cases[0].NextWakeAt == nil || !store.cases[0].NextWakeAt.Equal(wake) {
		t.Fatalf("the case's wake = %v, want the reset %s plus the margin %s", store.cases[0].NextWakeAt, reset, caseWakeMargin)
	}
	if len(store.events) != 1 || store.events[0].Kind != EventBriefFailed || store.events[0].Detail["case"] != int64(5) {
		t.Fatalf("events = %+v, want one brief_failed of case 5", store.events)
	}
	if backoff := store.events[0].Detail["backoff_s"]; backoff != int(reset.Sub(at).Seconds()) {
		t.Fatalf("the row's BackoffS = %v, want the %d seconds to the reset", backoff, int(reset.Sub(at).Seconds()))
	}

	// the pass waits at the wake: no call, no second row
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(store.events) != 1 {
		t.Fatalf("a pass inside the freeze made %d calls and %d rows, want the one before", calls, len(store.events))
	}

	// the monitor sweep wakes the case at the reset plus the margin: at a
	// clock past the wake the sweep's condition holds (next_wake_at <=
	// now, DueCases), and the wake clears the case's wake (monitor.wake's
	// SetCaseWake at nil) — the next pass then makes its one call, the
	// provider answers again, and the brief files
	past := wake.Add(time.Second)
	if store.cases[0].NextWakeAt.After(past) {
		t.Fatalf("the case is not due at %v, its wake %v still ahead", past, store.cases[0].NextWakeAt)
	}
	w.Clock = func() time.Time { return past }
	if err := store.SetCaseWake(context.Background(), store.cases[0].ID, store.cases[0].OwnerRung, store.cases[0].WakeOn, nil); err != nil {
		t.Fatal(err)
	}
	frozen = false
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("the woken case met %d calls in all, want one before the freeze and one at the wake", calls)
	}
	if store.caseBrief[5] == [2]string{} {
		t.Fatalf("the woken case filed no brief: %q %q", store.caseBrief[5][0], store.caseBrief[5][1])
	}
	if len(store.events) != 1 {
		t.Fatalf("the woken case recorded %d rows in all, want the one of the freeze", len(store.events))
	}

	// a check-in call inside the same freeze is answered by the hold: it
	// records 'not run, the class is frozen until <reset>' and raises no
	// flag — the record's reason carries no CheckinRefusedReason, the
	// prefix the digest's checkin_refused flag keys on
	chat := &errChat{err: fmt.Errorf("gateway %s: %w", gateway.RefusedProviderLimit,
		&policy.Refusal{Decision: gateway.RefusedProviderLimit,
			Reason: "provider limit hold for class big: reset " + reset.Format(time.RFC3339) + ": five-hour limit"})}
	ask := &CheckInAsk{Moment: CheckInGate, ParentSession: "s-1", Findings: []CheckFinding{{Text: "f"}}}
	out := new(Runner).runCheckin(context.Background(), "t2", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000, time.Hour)
	if out.Decision != CheckHandover || out.Handover != "" || len(out.Verdicts) != 0 {
		t.Fatalf("the frozen check-in = %+v, want a handover with no note: the round runs as if no check-in existed", out)
	}
	if want := "not run, the class is frozen until " + reset.UTC().Format(time.RFC3339); out.Record.Reason != want {
		t.Fatalf("the check-in's record reason = %q, want %q", out.Record.Reason, want)
	}
	if strings.HasPrefix(out.Record.Reason, CheckinRefusedReason) {
		t.Fatalf("the check-in's record reason carries the refused mark, want none: %q", out.Record.Reason)
	}
}

// A review's or an audit's gate retry is pinned to the freeze's reset
// while a hold covers it: the call the hold refused is made again once,
// at the reset it named, and not before.
func TestGateRetryPinnedToTheFreezeReset(t *testing.T) {
	// the freeze: a hold whose reset sits five hours out, the incident's
	// five-hour hold; the clock moves to the reset when the pin sleeps, so
	// the test waits no hold out
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour).Truncate(time.Second)
	clock := now
	var waited time.Duration
	oldNow, oldSleep := gateFreezeNow, gateFreezeSleep
	gateFreezeNow = func() time.Time { return clock }
	gateFreezeSleep = func(ctx context.Context, d time.Duration) bool {
		waited = d
		clock = reset.Add(time.Second)
		return true
	}
	defer func() { gateFreezeNow, gateFreezeSleep = oldNow, oldSleep }()
	var called []time.Time
	// the results arrive already refused, so the handler's one request is
	// the pinned re-call itself, made at the reset the hold named: the
	// hold has lapsed there, and the gateway answers it
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, clock)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"review answer"}}]}`))
	}))
	defer gw.Close()
	r := &Runner{supervisor: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}}
	refusal := func(at time.Time) error {
		return fmt.Errorf("gateway %s: %w", gateway.RefusedProviderLimit,
			&policy.Refusal{Decision: gateway.RefusedProviderLimit,
				Reason: "provider limit hold for class big: reset " + at.Format(time.RFC3339) + ": five-hour limit"})
	}
	results := []callResult{{Err: refusal(reset)}, {Err: fmt.Errorf("gateway refused_budget: no money")}}
	calls := []reviewCall{{Prompt: "p", Input: func() any { return "in" }}, {Prompt: "q", Input: func() any { return "in2" }}}

	// the retry is scheduled at the reset: the pin waits the five hours in
	// one sleep, makes no call and no dead wait inside the freeze, and
	// re-calls once, at or after the reset — the money refusal is no
	// freeze, so the guard re-calls the frozen call alone
	r.pinGateFreeze(context.Background(), "t1", "big", policy.PurposeReview, time.Minute, calls, results)
	if waited != reset.Sub(now) {
		t.Fatalf("the pin waited %s, want the whole hold to the reset %s", waited, reset.Sub(now))
	}
	if len(called) != 1 {
		t.Fatalf("the pinned retry made %d calls, want one", len(called))
	}
	if called[0].Before(reset) {
		t.Fatalf("the pinned retry ran at %v, before the reset %v", called[0], reset)
	}
	if refused, ok := refusedForFreeze(results[0].Err); ok {
		t.Fatalf("the re-called call was refused again at %v: %v", refused, results[0].Err)
	}
	if _, ok := refusedForFreeze(results[1].Err); ok || results[1].Err.Error() != "gateway refused_budget: no money" {
		t.Fatalf("the money refusal moved: %v", results[1].Err)
	}
	if _, ok := refusedForFreeze(fmt.Errorf("gateway %s: %w", policy.RefusedBudget,
		&policy.Refusal{Decision: policy.RefusedBudget, Reason: "no money"})); ok {
		t.Fatal("a money refusal is no freeze")
	}
	if _, ok := refusedForFreeze(nil); ok {
		t.Fatal("no error is no freeze")
	}

	// a cancelled attempt makes no call at all: the sleep gives up on the
	// context, the re-call guard keeps the attempt out of the class
	clock = now
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.pinGateFreeze(ctx, "t1", "big", policy.PurposeReview, time.Minute, calls, []callResult{{Err: refusal(reset)}})
	if len(called) != 1 {
		t.Fatalf("a cancelled attempt made %d calls in all, want none beside the pinned one", len(called))
	}
}

// A step's finding with URL userinfo and a bearer value is masked in the
// facts the model reads and in the mark of a failed call.
func TestBriefWriterMasksSecretsInTheCaseFacts(t *testing.T) {
	var body []byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set(gateway.HeaderDecision, "refused_capability")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"https://user:pass@host refused Authorization: Bearer abc123def456ghi789"}}`))
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman},
		CaseStep{ID: 1, Rung: 1, Found: "cloned https://user:pass@host/r.git with Authorization: Bearer abc123def456ghi789", Outcome: StepHandedUp})
	if err := briefCaseWriter(store, gw).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"user:pass", "abc123def456ghi789"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the facts carry %q: %s", leak, body)
		}
		if strings.Contains(store.caseFailed[5], leak) {
			t.Errorf("the mark carries %q: %s", leak, store.caseFailed[5])
		}
	}
	if store.caseFailed[5] == "" {
		t.Fatal("the refusal was not marked")
	}
}

// A project whose chat task has no money left gets no call: each of its
// cases is marked "judgment budget spent" once, no event repeats, the face
// shows the digest's info, and the pass writes the brief once the budget has
// room.
func TestBriefWriterSkipsASpentProjectAndTakesItUpAgain(t *testing.T) {
	calls := 0
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"background\": \"All told.\", \"recommendation\": \"\"}"}}]}`))
	}))
	defer gw.Close()
	c := Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman}
	store := briefCaseStore(c)
	store.chatSpent = 1e6
	w := briefCaseWriter(store, gw)
	for range 2 {
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 || store.caseFailed[5] != "judgment budget spent" || len(store.events) != 1 {
		t.Fatalf("calls %d, mark %q, events %d; want no call, the mark once and one event", calls, store.caseFailed[5], len(store.events))
	}
	face := CaseFace(store.cases[0], nil, CaseFacts{}, CaseAsk{Detail: map[string]any{"info": "the digest's info"}}, nil)
	if face.Info != "the digest's info" {
		t.Fatalf("the face's info = %q, want the digest's", face.Info)
	}
	store.chatSpent = 0
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || store.caseBrief[5][0] != "All told." {
		t.Fatalf("after the raise: calls %d, brief %v; want one call and the brief", calls, store.caseBrief[5])
	}
}

// A model answer that is no brief marks the case once and is tried again on
// the next pass; a second one is terminal.
func TestBriefWriterRetriesANoBriefAnswerOnce(t *testing.T) {
	calls := 0
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"background\": \"cut mid"}}]}`))
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman})
	w := briefCaseWriter(store, gw)
	var marks []string
	for range 3 {
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		marks = append(marks, store.caseFailed[5])
		if strings.HasPrefix(store.caseFailed[5], BriefTerminal) {
			store.cases = nil
		}
	}
	if calls != 2 || !strings.HasPrefix(marks[0], noBriefMark) || !strings.HasPrefix(marks[1], BriefTerminal) {
		t.Fatalf("calls %d, marks %q; want two calls, the first mark %q and the second terminal", calls, marks, noBriefMark)
	}
}

// Every string of the facts is scrubbed as the text it is, so a token at the
// start of a line and a header quoted in a command are masked, and the JSON
// the gateway reads stays valid.
func TestBriefWriterMasksLineStartTokensAndQuotedHeaders(t *testing.T) {
	var user string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
				Role    string `json:"role"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"background\": \"ok\", \"recommendation\": \"\"}"}}]}`))
	}))
	defer gw.Close()
	const token, bearer = "0123456789abcdef0123456789abcdef01234567", "abc123def456ghi789"
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman},
		CaseStep{ID: 1, Rung: 1, Found: "the request:\nAuthorization: token " + token + "\nBearer " + bearer + "\nend", Outcome: StepHandedUp},
		CaseStep{ID: 2, Rung: 2, Found: `it ran curl -H "Authorization: Bearer ` + bearer + `" https://host/x`, Outcome: StepHandedUp})
	if err := briefCaseWriter(store, gw).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.caseBrief[5][0] != "ok" || len(store.caseFailed) != 0 {
		t.Fatalf("brief %v, marks %v; want the brief filed", store.caseBrief[5], store.caseFailed)
	}
	if !json.Valid([]byte(strings.TrimPrefix(user, ChatInputLead))) {
		t.Fatalf("the facts are no JSON: %s", user)
	}
	for _, leak := range []string{token, bearer} {
		if strings.Contains(user, leak) {
			t.Errorf("the facts carry %q: %s", leak, user)
		}
	}
}

// A pass takes an equal share of each project's rows: two projects in the
// policy make a share of 10 of the 20, for cases and for approval rows.
func TestBriefWriterPassTakesAShareOfEachProject(t *testing.T) {
	gw := briefGateway(t, http.StatusOK, `{}`)
	defer gw.Close()
	store := &fakeBriefStore{}
	if err := briefCaseWriter(store, gw).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.perRepo) != 2 || store.perRepo[0] != 10 || store.perRepo[1] != 10 {
		t.Fatalf("the shares asked = %v, want 10 for cases and 10 for approval rows (20 over the version's two projects)", store.perRepo)
	}
}

// A gateway that refuses for want of money gets one call: the pass makes no
// more for the project while its chat task's budget has not grown, and takes
// it up again once it has.
func TestBriefWriterMakesNoCallToARefusingBudgetUntilItGrows(t *testing.T) {
	calls := 0
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set(gateway.HeaderDecision, policy.RefusedBudget)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"free headroom 0.0001 EUR fits 2 output tokens"}}`))
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman})
	store.cases = append(store.cases, Case{ID: 6, TaskID: "t1", Repository: "o/r", Kind: CaseDayCap, Status: CaseAtHuman})
	w := briefCaseWriter(store, gw)
	for range 5 {
		if err := w.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(store.events) != 2 {
		t.Fatalf("calls %d, events %d after five passes; want one call (the second case waits too) and one event per case", calls, len(store.events))
	}
	store.chatRaise = 1
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d after the raise, want a second", calls)
	}
}
