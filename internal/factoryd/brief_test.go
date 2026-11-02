package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	revisions map[string]PlanRevision
	verdicts  map[string][]CriterionVerdict
	brief     map[int64][2]string
	failed    map[int64]string
	events    []Event
	taskErr   error
}

func (f *fakeBriefStore) Task(ctx context.Context, id string) (Task, bool, error) {
	t, ok := f.tasks[id]
	return t, ok, f.taskErr
}

func (f *fakeBriefStore) BrieflessApprovals(ctx context.Context, limit int) ([]ApprovalRequest, error) {
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

func briefTask() Task {
	return Task{ID: "t1", Repository: "o/r", RequiredCapabilities: []string{policy.ClassCoder}}
}

func briefVersion(url string) policy.Version {
	return policy.Version{Tree: policy.Tree{Document: policy.Document{Classes: map[string]policy.Class{
		policy.ClassCoder: {SupervisorModel: "supervisor"},
	}, Limits: policy.Limits{SupervisorTimeout: 6 * time.Minute}}}}
}

func mergeSpec(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(MergeSpec{RiskClass: "small"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBriefWriterPass(t *testing.T) {
	row := ApprovalRequest{ID: 7, Kind: ApprovalMerge, TaskID: "t1", Project: "o/r", PRNumber: 12,
		Spec: mergeSpec(t), Reason: "risk class small", CreatedAt: time.Unix(0, 0).UTC()}
	answer := `{"choices":[{"message":{"content":"{\"brief\": \"PR #12 waits on you.\", \"detail\": \"- small risk\"}"}}]}`
	gw := briefGateway(t, http.StatusOK, answer)
	defer gw.Close()
	store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
		rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
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
			rows:  []ApprovalRequest{{ID: 7, Kind: ApprovalMerge, TaskID: "t1", Project: "o/r", PRNumber: 12, Spec: mergeSpec(t)}},
			brief: map[int64][2]string{}, failed: map[int64]string{}}
		v := briefVersion(gw.URL)
		v.Document.Limits.SupervisorTimeout = tt.bound
		w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return v }}
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
			w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
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

// A terminal failure — no plan revision left to read — marks the row
// with the failure's class, so the pass never retries it, and telemetry
// hears it once: a pass that meets the same mark again records no second
// event.
func TestBriefWriterMarksTerminalFailures(t *testing.T) {
	row := ApprovalRequest{ID: 11, Kind: ApprovalCriteria, TaskID: "t1", Project: "o/r",
		Spec: json.RawMessage(`{"task":"t-int"}`), Reason: "a criterion unmet", CreatedAt: time.Unix(0, 0).UTC()}
	gw := briefGateway(t, http.StatusOK, `{}`)
	defer gw.Close()
	store := &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()},
		rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
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
			facts, acts, err := briefFacts(tt.a, briefTask(), nil)
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

// The brief rides its own purpose, so the gateway takes it on a card that
// waits on a person; a refusal of the card's own budget marks the row as
// one a raise answers, and no other failure does.
func TestBriefWriterMarksABudgetRefusal(t *testing.T) {
	for _, tt := range []struct {
		decision, message, mark string
		unfunded                bool
	}{
		{policy.RefusedBudget, "free headroom 0.000100 EUR fits 2 output tokens", "refused_budget: free headroom 0.000100 EUR fits 2 output tokens", true},
		{policy.RefusedMonthCap, "the month cap is spent", "refused_month_cap: the month cap is spent", false},
		{"refused_task_status", "task t1 is done", "refused_task_status: task t1 is done", false},
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
		w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
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
		row := store.rows[0]
		row.BriefError = store.failed[9]
		if BriefUnfunded(row) != tt.unfunded {
			t.Errorf("%s: unfunded %v, want %v", tt.decision, !tt.unfunded, tt.unfunded)
		}
	}
	if BriefUnfunded(ApprovalRequest{Status: "approved", BriefError: "refused_budget: spent"}) {
		t.Error("a decided request asks for a raise")
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
	facts, acts, err := briefFacts(a, briefTask(), criteria)
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
	criteria := []criterionFact{{ID: "c1", Kind: "test", Text: "go test ./... is green", Verdict: "open"}}
	facts, acts, err := briefFacts(a, briefTask(), criteria)
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
	plan, _ := json.Marshal(Plan{Criteria: []PlanCriterion{{ID: "c1", Kind: "test", Test: "go test ./... is green"}}})
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
		{ID: "c1", Kind: "test", Test: "go test ./... is green"},
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"brief\": \"Two criteria wait on you.\", \"detail\": \"- c1 met\"}"}}]}`))
	}))
	defer gw.Close()
	store := &fakeBriefStore{
		tasks: map[string]Task{"t-plan": {ID: "t-plan", Repository: "sf/code", RequiredCapabilities: []string{policy.ClassCoder}}},
		rows:  []ApprovalRequest{{ID: 11, Kind: ApprovalCriteria, TaskID: "t-plan", Project: "sf/code", Spec: spec, Reason: "a criterion unmet", CreatedAt: time.Unix(0, 0).UTC()}},
		brief: map[int64][2]string{}, failed: map[int64]string{},
		revisions: map[string]PlanRevision{"t-plan": {TaskID: "t-plan", Revision: 3, Plan: plan}},
		verdicts: map[string][]CriterionVerdict{"t-int": {
			{Criterion: "c1", Verdict: "met", Evidence: "the review read the diff", Kind: "test", Source: "review"},
		}},
	}
	w := &BriefWriter{Store: store, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version { return briefVersion(gw.URL) }}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.brief[11]; got[0] != "Two criteria wait on you." || got[1] != "- c1 met" {
		t.Fatalf("the brief = %v", got)
	}
	for _, want := range []string{"the feature reads well", `"verdict":"open"`, `"verdict":"met"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("the model's facts lack %q: %s", want, sent)
		}
	}
}
