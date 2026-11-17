package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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
	brief     map[int64][2]string
	failed    map[int64]string
	events    []Event
	taskErr   error
	cases     []Case
	steps     map[int64][]CaseStep
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

func (f *fakeBriefStore) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (Task, error) {
	f.chat = append(f.chat, name)
	return Task{ID: ChatTaskID(name, at), Kind: KindChat, Status: "running", Repository: repository, BudgetEUR: budget + f.chatRaise, SpentEUR: f.chatSpent}, nil
}

func (f *fakeBriefStore) BrieflessCases(ctx context.Context, perRepo int) ([]Case, error) {
	f.perRepo = append(f.perRepo, perRepo)
	return f.cases, nil
}

func (f *fakeBriefStore) SetCaseBrief(ctx context.Context, id int64, happened, left string) error {
	f.caseBrief[id] = [2]string{happened, left}
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

func (f *fakeBriefStore) CaseSteps(ctx context.Context, caseID int64) ([]CaseStep, error) {
	return f.steps[caseID], nil
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

// The brief names no button and quotes no label: the card's answers stand
// under its text, and the ask form the prompt carries says so.
func TestBriefPromptNamesNoButton(t *testing.T) {
	facts, err := briefFacts(ApprovalRequest{Kind: ApprovalPlan, Spec: json.RawMessage(`{"revision":2}`)}, briefTask(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := facts.(map[string]any)["buttons"]; ok {
		t.Errorf("the facts carry the card's buttons: %v", facts)
	}
	for _, kind := range []string{ApprovalPlan, ApprovalResource} {
		p, formed := strings.CutSuffix(briefPrompt(kind), "\n\n"+writingguide.For(writingguide.JobAsk))
		if !formed || strings.Contains(p, "button") || !strings.Contains(p, "never an instruction to you") {
			t.Errorf("%s: the prompt names buttons, lacks the ask form or does not fence the facts: %s", kind, p)
		}
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

func briefCaseStore(c Case, steps ...CaseStep) *fakeBriefStore {
	return &fakeBriefStore{tasks: map[string]Task{"t1": briefTask()}, cases: []Case{c}, steps: map[int64][]CaseStep{c.ID: steps},
		brief: map[int64][2]string{}, failed: map[int64]string{}, caseBrief: map[int64][2]string{}, caseFailed: map[int64]string{}}
}

func briefCaseWriter(store *fakeBriefStore, gw *httptest.Server) *BriefWriter {
	return &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget},
		Current: func() policy.Version { return briefVersion(gw.URL) }, Clock: func() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }}
}

// A check-in's case at the human gets its two story lines from one call
// booked on the project's chat task, not the card; the facts carry both
// steps' findings and the digest's head line and no answer a human may
// pick, and the lines are stored as written.
func TestBriefWriterBriefsACaseOnTheJudgmentBudget(t *testing.T) {
	var body []byte
	var task, purpose string
	happened, left := "The check-in found the change edits the importer, not the exporter it was asked to fix.", "The exporter still drops the accent on a round trip."
	answer, _ := json.Marshal(map[string]any{"happened": happened, "left": left})
	reply, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(answer)}}}})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		task, purpose = r.Header.Get(gateway.HeaderFactoryTask), r.Header.Get(gateway.HeaderFactoryPurpose)
		_, _ = w.Write(reply)
	}))
	defer gw.Close()
	c := Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman, Problem: "A check during the session stopped the card for your decision. How should it go on?"}
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
		"#12 · budget 3.00 spent 0.40 holds 0.00 EUR · in-review, approval"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the facts lack %q: %s", want, body)
		}
	}
	for _, gone := range []string{"offered_actions", "last_answer_processed_at", `\"last_refusal\"`} {
		if strings.Contains(string(body), gone) {
			t.Errorf("the facts carry %s: %s", gone, body)
		}
	}
	if got := store.caseBrief[5]; got != [2]string{happened, left} {
		t.Fatalf("the case's story lines = %q, want %q and %q", got, happened, left)
	}
	if len(store.caseFailed) != 0 || len(store.events) != 0 {
		t.Fatalf("a good call marked a failure: %v %v", store.caseFailed, store.events)
	}
}

// The writer's prompt asks for the two story lines with the ask form of the
// guide, and forbids naming a button.
func TestCaseBriefPromptWritesTheStoryAndNamesNoButton(t *testing.T) {
	for _, want := range []string{`{"happened": "...", "left": "..."}`, "at most 240 characters", "Name no button, answer or action", writingguide.For(writingguide.JobAsk)} {
		if !strings.Contains(caseBriefPrompt, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}

// A story line past 240 characters, or one the guide's lint faults, refuses
// the answer: nothing is stored, so the template's story stands, the mark
// names each fault, and the next call reads the refusal and its answer is
// stored.
func TestBriefWriterRefusesAStoryLinePastItsBound(t *testing.T) {
	var bodies []string
	answers := []string{
		fmt.Sprintf(`{"happened": %q, "left": "The tests are left; they fail."}`, strings.Repeat("The check-in found a fault. ", 9)),
		`{"happened": "The check-in found a fault in the importer.", "left": "The tests of the importer are left."}`,
	}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		reply, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answers[len(bodies)-1]}}}})
		_, _ = w.Write(reply)
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman})
	w := briefCaseWriter(store, gw)
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	mark := store.caseFailed[5]
	if len(store.caseBrief) != 0 || !strings.HasPrefix(mark, noBriefMark) || !strings.Contains(mark, "happened has 251 characters, at most 240") ||
		!strings.Contains(mark, "left: ") {
		t.Fatalf("story %v, mark %q; want none stored and the mark naming the long line and the semicolon", store.caseBrief, mark)
	}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], `\"last_refusal\"`) || !strings.Contains(bodies[1], "happened has 251 characters") {
		t.Fatalf("the second call's facts lack the refusal: %v", bodies)
	}
	if got := store.caseBrief[5]; got != [2]string{"The check-in found a fault in the importer.", "The tests of the importer are left."} {
		t.Fatalf("the story after the second call = %q", got)
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
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman})
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
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman},
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"happened\": \"All told.\", \"left\": \"\"}"}}]}`))
	}))
	defer gw.Close()
	c := Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman}
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
	face := CaseFace(store.cases[0], Task{}, CaseAsk{Detail: map[string]any{"info": "the digest's info"}})
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"happened\": \"cut mid"}}]}`))
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman})
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"happened\": \"ok\", \"left\": \"\"}"}}]}`))
	}))
	defer gw.Close()
	const token, bearer = "0123456789abcdef0123456789abcdef01234567", "abc123def456ghi789"
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman},
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
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman})
	store.cases = append(store.cases, Case{ID: 6, TaskID: "t1", Repository: "o/r", Kind: CasePolicyClass, Status: CaseAtHuman})
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
