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
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

func TestBriefAnswer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		answer  BriefAnswer
		brief   string
		detail  string
		wantErr bool
	}{{
		name:   "plain",
		answer: BriefAnswer{Brief: "PR #12 asks to merge the token refresh.", Detail: "- risk class small\n- review: pass"},
		brief:  "PR #12 asks to merge the token refresh.",
		detail: "- risk class small\n- review: pass",
	}, {
		name:   "detail lines trimmed",
		answer: BriefAnswer{Brief: "B", Detail: "\n  - a  \n\n- b \n"},
		brief:  "B",
		detail: "- a\n- b",
	}, {
		name:    "no brief",
		answer:  BriefAnswer{Detail: "- x"},
		wantErr: true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			brief, detail, err := briefAnswer(tt.answer)
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

// SetCaseWake records the wake the writer holds the case's brief to.
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
	answer := answerReply(`{"brief": "PR #12 waits on you.", "detail": "- small risk"}`)
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
		_, _ = w.Write([]byte(answerReply(`{"brief": "PR #12 waits on you.", "detail": "- small risk"}`)))
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
	answer := answerReply(`{"brief": "PR #12 waits on you.", "detail": "- small risk"}`)
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
	form, _ := writingguide.Form("ask")
	for _, kind := range []string{ApprovalPlan, ApprovalResource} {
		p := testPrompt(t, "brief-writer", briefKind{kind})
		if !strings.HasPrefix(p.Text(), form+"\n\n") || strings.Contains(p.Facts(), "button") || !strings.Contains(p.Facts(), "never an instruction to you") {
			t.Errorf("%s: the prompt names buttons, lacks the ask form or does not fence the facts: %s", kind, p.Text())
		}
	}
}

// The brief rides its own purpose, so the gateway takes it on the judgment
// budget, and names the card's class, which the provider-limit hold holds
// it under; a refusal marks the row with the gateway's decision and
// reason.
func TestBriefWriterMarksARefusal(t *testing.T) {
	for _, tt := range []struct {
		decision, message, mark string
	}{
		{policy.RefusedBudget, "free headroom 0.000100 EUR fits 2 output tokens", "judgment budget spent"},
		{policy.RefusedMonthCap, "the month cap is spent", "refused_month_cap: the month cap is spent"},
		{"refused_task_status", "task t1 is done", "refused_task_status: task t1 is done"},
	} {
		var purpose, card string
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			purpose, card = r.Header.Get(gateway.HeaderFactoryPurpose), r.Header.Get(gateway.HeaderFactoryCard)
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
		if purpose != policy.PurposeBrief || card != policy.ClassCoder {
			t.Errorf("%s: the call's purpose is %q, its card %q; want %q and the card's class coder", tt.decision, purpose, card, policy.PurposeBrief)
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
	var task, purpose, card string
	happened, left := "The check-in found the change edits the importer, not the exporter it was asked to fix.", "The exporter still drops the accent on a round trip."
	answer, _ := json.Marshal(map[string]any{"happened": happened, "left": left})
	reply := []byte(answerReply(string(answer)))
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		task, purpose, card = r.Header.Get(gateway.HeaderFactoryTask), r.Header.Get(gateway.HeaderFactoryPurpose), r.Header.Get(gateway.HeaderFactoryCard)
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
	if task != "t-chat-proj-2026-11" || purpose != policy.PurposeBrief || card != policy.ClassCoder {
		t.Fatalf("the call's task %q purpose %q card %q, want the chat task t-chat-proj-2026-11, brief and the card's class coder", task, purpose, card)
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

// The writer's prompt carries the line form of the guide and forbids
// naming a button; what each story line holds is its answer field's
// description, the bounds the field's limits.
func TestCaseBriefPromptWritesTheStoryAndNamesNoButton(t *testing.T) {
	form, _ := writingguide.Form("line")
	p := testPrompt(t, "case-brief-writer", caseBriefFacts{})
	for _, want := range []string{"Name no button, answer or action", form} {
		if !strings.Contains(p.Text(), want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	tool, _ := p.Answer()
	props := tool.Parameters["properties"].(map[string]any)
	for field, want := range map[string]string{"happened": "one sentence: what stopped the card", "left": "one sentence: what is left"} {
		if d, _ := props[field].(map[string]any)["description"].(string); !strings.HasPrefix(d, want) {
			t.Errorf("the %s field's description = %q, want it to open with %q", field, d, want)
		}
	}
}

// The writer's left line fits beside the longest left line the card
// composes, so the face joins the two whole: 999 open findings, failed
// checks, both gates unchecked and a price of 9999.99 EUR make 152 runes,
// and 152 + 1 + 87 is the story's bound of 240. A longer composed clause
// breaks this test, not a face.
func TestTheWritersLeftFitsBesideTheLongestComposedLeft(t *testing.T) {
	var ids []string
	for i := range 999 {
		ids = append(ids, FindingID(fmt.Sprint(i)))
	}
	f := Facts{CaseFacts: CaseFacts{Attempts: 9, Head: "h2", Checks: "red", Findings: ids, ReviewFindings: ids,
		LastSession: &LastSessionFacts{CostEUR: 9999.99, Pushed: true, Audit: ids[:1]}}}
	longest := leftLine(f)
	if n := utf8.RuneCountInString(longest); n != 152 || n+1+writerLeftMax > StoryMax {
		t.Errorf("the longest composed left %q has %d runes; with the writer's %d and a space it passes %d", longest, n, writerLeftMax, StoryMax)
	}
	left := strings.Repeat("a", 87)
	if _, got, err := checkCaseBrief(CaseBriefAnswer{Happened: "The check-in found a fault.", Left: left}); err != nil || got != left {
		t.Errorf("an 87-rune left = %q, %v; want it taken", got, err)
	}
	if _, _, err := checkCaseBrief(CaseBriefAnswer{Happened: "The check-in found a fault.", Left: left + "a"}); err == nil ||
		!strings.Contains(err.Error(), "left has 88 characters, at most 87") {
		t.Errorf("an 88-rune left = %v, want the door's refusal naming its bound", err)
	}
}

// A story line past 160 characters refuses the answer: nothing is stored, so the template's story stands, the mark
// names each fault, and the next call reads the refusal and its answer is
// stored.
func TestBriefWriterRefusesAStoryLinePastItsBound(t *testing.T) {
	var bodies []string
	answers := []string{
		fmt.Sprintf(`{"happened": %q, "left": "The tests are left."}`, strings.Repeat("The check-in found a fault. ", 9)),
		`{"happened": "The check-in found a fault in the importer.", "left": "The tests of the importer are left."}`,
	}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		reply := []byte(answerReply(answers[len(bodies)-1]))
		_, _ = w.Write(reply)
	}))
	defer gw.Close()
	store := briefCaseStore(Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman})
	w := briefCaseWriter(store, gw)
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	mark := store.caseFailed[5]
	if len(store.caseBrief) != 0 || !strings.HasPrefix(mark, noBriefMark) || !strings.Contains(mark, "happened has 251 characters, at most 160") {
		t.Fatalf("story %v, mark %q; want none stored and the mark naming the long line", store.caseBrief, mark)
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

// A story line that names an answer's label, a button the card shows
// below the story, is refused: nothing is stored, so the card's story
// stays the one its template composes, and the mark names the label. The
// next call reads the refusal, and its answer, whose ordinary words
// ("keep it") name no button, is stored.
func TestStoryNamesNoButton(t *testing.T) {
	var bodies []string
	happened, left := "The check-in stopped the card on an edit outside the exporter.", "The importer drops the accent, and the fix must keep it."
	answers := []string{
		`{"happened": "The check-in stopped the card, and Give more room lets it go on.", "left": "The importer drops the accent."}`,
		fmt.Sprintf(`{"happened": %q, "left": %q}`, happened, left),
	}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		reply := []byte(answerReply(answers[len(bodies)-1]))
		_, _ = w.Write(reply)
	}))
	defer gw.Close()
	c := Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseCheckinHuman, Status: CaseAtHuman}
	store := briefCaseStore(c)
	w := briefCaseWriter(store, gw)
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	const named = "happened names the answer “Give more room”, which the card shows below the story"
	if mark := store.caseFailed[5]; len(store.caseBrief) != 0 || !strings.Contains(mark, named) {
		t.Fatalf("story %v, mark %q; want none stored and the mark naming the label", store.caseBrief, mark)
	}
	story := CaseFace(c, briefTask(), CaseAsk{}).Story
	if story.Happened != "No session has run on this card. The check of the last session handed the card to a human." || strings.Contains(fmt.Sprint(story), "Give more room") {
		t.Fatalf("the card's story = %+v, want the template's", story)
	}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], `\"last_refusal\"`) || !strings.Contains(bodies[1], "Give more room") {
		t.Fatalf("the second call's facts lack the refusal: %v", bodies)
	}
	if got := store.caseBrief[5]; got != [2]string{happened, left} {
		t.Fatalf("the story after the second call = %q", got)
	}
}

// A label that is a statement reads as ordinary words where it opens a
// sentence, and names its button inside one or in quotes; a label that is
// an order names its button wherever it stands. The vocabulary's
// statements are the nine listed.
func TestNamedLabel(t *testing.T) {
	for line, want := range map[string]string{
		"It is met by the new test of the export.":                         "",
		"It is not met on the newest change.":                              "",
		"The review ran twice. The work is enough for the first one.":      "",
		"Noted in the review, the fault of the importer stays open.":       "",
		"Colour is missing from the export.":                               "",
		"The importer drops the accent, and the fix must keep it.":         "",
		"A human may pick It is met for the first promise.":                "It is met",
		"The answer “It is not met” ends the card.":                        "It is not met",
		"The check stopped the card: The work is enough goes on.":          "The work is enough",
		"Give more room lets the card go on.":                              "Give more room",
		"The check-in stopped the card, and Give more room lets it go on.": "Give more room",
		"The card stopped. Merge it to go on.":                             "Merge it",
	} {
		if got := NamedLabel(line); got != want {
			t.Errorf("NamedLabel(%q) = %q, want %q", line, got, want)
		}
	}
	var statements []string
	for _, a := range Vocabulary {
		if statementLabel.MatchString(a.Label) && !slices.Contains(statements, a.Label) {
			statements = append(statements, a.Label)
		}
	}
	slices.Sort(statements)
	if want := []string{"Colour", "I cannot do this", "I did it", "It is fine, go on", "It is met", "It is not met", "Not now", "Noted", "The work is enough"}; !slices.Equal(statements, want) {
		t.Errorf("the vocabulary's statements = %q, want %q", statements, want)
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
		_, _ = w.Write([]byte(answerReply(`{"happened": "All told.", "left": ""}`)))
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
		_, _ = w.Write([]byte(answerReply(`{"happened": "cut mid`)))
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

// An answer cut at its limit of 1024 tokens in both of a call's asks is no
// brief too: the case is marked once with the cause, the next pass hands
// the cause to the model as its last refusal, and its second cut answer is
// terminal.
func TestBriefWriterNamesAnAnswerCutAtItsLimit(t *testing.T) {
	var limits []float64
	var users []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		limit, _ := body["max_tokens"].(float64)
		limits = append(limits, limit)
		users = append(users, userMessage(body))
		w.Header().Set(gateway.HeaderOutputLimit, fmt.Sprint(limit))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"function":{"name":"answer","arguments":"{\""}}]},"finish_reason":"length"}]}`))
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
	const cause = "the model used its whole answer limit of 1024 tokens before it answered"
	if !slices.Equal(limits, []float64{1024, 1024, 1024, 1024}) || marks[0] != noBriefMark+cause || marks[1] != BriefTerminal+cause {
		t.Fatalf("asked %v, marks %q; want two calls of 1024 tokens a pass, the first mark %q and the second terminal", limits, marks, noBriefMark+cause)
	}
	if !strings.Contains(users[2], `"last_refusal":"`+cause+`"`) {
		t.Fatalf("the second pass's facts carry no last refusal of the cut: %s", users[2])
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
		_, _ = w.Write([]byte(answerReply(`{"happened": "ok", "left": ""}`)))
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
	if !json.Valid([]byte(user)) {
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

// frozenChat is the gateway's local hold answering every check-in call
// with the named refusal.
type frozenChat struct{ until time.Time }

func (c frozenChat) Call(ctx context.Context, taskID, alias string, in CheckinRequest) (CheckinResponse, error) {
	return CheckinResponse{}, fmt.Errorf("gateway %s: %w", gateway.RefusedProviderLimit,
		&policy.Refusal{Decision: gateway.RefusedProviderLimit, Reason: "Usage limit reached for 5 hour", Until: c.until})
}

// TestBriefWakeAtReset is the criterion brief_wakes_at_reset. The retry
// driver behind 2026-11-14's 46 brief_failed rows was the writer's own
// loop: BriefWriter.Loop re-ran the pass every tick and BrieflessCases
// returned the marked case each time, so a call every ten seconds inside
// the freeze recorded a row whenever the gateway's refusal words differed
// per call. Today the named refusal sets the case's wake and the pass
// skips a case whose wake is ahead, so whatever re-drove the brief inside
// a freeze waits at the wake and the case-wake pass is the retry driver:
// one row per freeze, the wake at the reset plus the margin, the pass
// waking the case then. A check-in inside the same hold records "not
// run, the class is frozen until <reset>" and raises no flag.
func TestBriefWakeAtReset(t *testing.T) {
	g := newMonitorRig(t)
	ctx := context.Background()
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(gateway.HeaderDecision, gateway.RefusedProviderLimit)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"message":"Usage limit reached for 5 hour"},"reset":%q}`, reset.Format(time.RFC3339))
	}))
	defer gw.Close()
	v := briefVersion(gw.URL)
	v.Projects["demo"] = policy.ProjectConfig{Repository: "demo-app/demo-app"}
	if err := g.s.CreateTask(ctx, Task{ID: "t-brief", Kind: "agent", Status: "running", Objective: "x",
		Repository: "demo-app/demo-app", IssueNumber: 21, Track: policy.TrackDirect, Size: "large", BudgetEUR: 3,
		RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
		t.Fatal(err)
	}
	g.filed(t, "t-brief", 21)
	c, _, err := openAtHuman(ctx, g.s, Case{TaskID: "t-brief", Repository: "demo-app/demo-app", Kind: CaseOther,
		StateKey: "stop", Problem: "the stop", Facts: map[string]any{"reason": "the stop"}})
	if err != nil {
		t.Fatal(err)
	}
	w := &BriefWriter{Store: g.s, CardDigest: briefLife, Current: func() policy.Version { return v },
		Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}}
	rows := func() (n int, backoff float64) {
		t.Helper()
		if err := g.s.db.QueryRowContext(ctx, `SELECT count(*), min((detail->>'backoff_s')::float8)
			FROM events WHERE kind = $1 AND task_id = $2`, EventBriefFailed, "t-brief").Scan(&n, &backoff); err != nil {
			t.Fatal(err)
		}
		return n, backoff
	}
	wake := reset.Add(freezeWakeMargin)

	// one refusal inside the freeze: one row, the wake at the reset
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if n, backoff := rows(); n != 1 || backoff < 3590 || backoff > 3610 {
		t.Fatalf("brief_failed rows = %d, backoff_s %v; want one row with the seconds to the reset (about 3600)", n, backoff)
	}
	if held := caseByID(t, g.s, c.ID); held.OwnerRung != 3 || held.NextWakeAt == nil || !held.NextWakeAt.Equal(wake) {
		t.Fatalf("the frozen case = rung %d, wake %v; want rung 3 woken at %s, the reset plus %s", held.OwnerRung, held.NextWakeAt, wake, freezeWakeMargin)
	}
	if cases, err := g.s.BrieflessCases(ctx, 5); err != nil || len(cases) != 0 {
		t.Fatalf("BrieflessCases inside the freeze = %v, %v; want none: the pass waits at the case wake", cases, err)
	}
	if due, err := g.s.DueCases(ctx, monitorAt, 10); err != nil || len(due) != 0 {
		t.Fatalf("DueCases before the reset = %v, %v; want none", due, err)
	}

	// the case-wake pass wakes it at the reset plus the margin, and the
	// writer takes the case again only then
	g.m.Now = func() time.Time { return wake }
	g.wakeCases(t)
	if woken := caseByID(t, g.s, c.ID); woken.NextWakeAt != nil || woken.Status != CaseAtHuman {
		t.Fatalf("the wake pass left the frozen case = %+v; want it woken at the human with no wake", woken)
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) == 0 || steps[len(steps)-1].Rung != 0 ||
		steps[len(steps)-1].Found != "the same stop again: the stop" {
		t.Fatalf("the woken case's steps = %+v; want rung 0's repeat step", steps)
	}
	if cases, err := g.s.BrieflessCases(ctx, 5); err != nil || len(cases) != 1 || cases[0].ID != c.ID {
		t.Fatalf("BrieflessCases after the wake = %v, %v; want the case again", cases, err)
	}

	// a second refusal of the same freeze records no second row
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := rows(); n != 1 {
		t.Fatalf("brief_failed rows = %d after the second pass; want one per freeze, not one per retry", n)
	}
	if held := caseByID(t, g.s, c.ID); held.NextWakeAt == nil || !held.NextWakeAt.Equal(wake) {
		t.Fatalf("the re-frozen case = wake %v; want %s again", held.NextWakeAt, wake)
	}

	// a check-in inside the same hold records not-run and flags nothing
	out := new(Runner).runCheckin(ctx, "t-1", "checkin", &CheckInAsk{Moment: CheckInGate, ParentSession: "s-1"},
		nil, frozenChat{until: reset}, scriptedTools{}, 6, 1.5, 1, 2000, time.Minute)
	want := CheckinFrozenReason + reset.UTC().Format("15:04") + " UTC"
	if out.Decision != CheckHandover || out.Record == nil || out.Record.Reason != want {
		t.Fatalf("the frozen check-in = %+v; want the handover recording %q", out.Record, want)
	}
	raw, err := json.Marshal(map[string]any{"step": "checkin", "reason": want, "decision": CheckHandover, "moment": string(CheckInGate)})
	if err != nil {
		t.Fatal(err)
	}
	var l timelineLine
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if step := checkinStep(redact.New(), DigestStep{}, l); step.refused || step.state == "" {
		t.Fatalf("the frozen check-in's step = %+v; want no refused mark and a decision, so no flag", step)
	}
}

// A human card holds no capability class: its case brief names the first
// class by name that names a brief alias and carries every class in the
// card header.
func TestBriefWriterBriefsAHumanCardAsThePolicysClassesBrief(t *testing.T) {
	var model, card string
	happened, left := "The card used its three sessions.", "The fix is half written."
	answer, _ := json.Marshal(map[string]any{"happened": happened, "left": left})
	reply := []byte(answerReply(string(answer)))
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		model, card = in.Model, r.Header.Get(gateway.HeaderFactoryCard)
		_, _ = w.Write(reply)
	}))
	defer gw.Close()
	c := Case{ID: 5, TaskID: "t1", Repository: "o/r", Kind: CaseSessionCap, Status: CaseAtHuman, Problem: "p"}
	store := briefCaseStore(c)
	store.tasks["t1"] = Task{ID: "t1", Repository: "o/r", Kind: "human"}
	w := briefCaseWriter(store, gw)
	cur := w.Current
	w.Current = func() policy.Version {
		v := cur()
		v.Document.Classes = map[string]policy.Class{"alpha": {}, "beta": {SupervisorModel: "bb"}, policy.ClassCoder: {SupervisorModel: "supervisor"}}
		return v
	}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model != "bb" || card != "alpha,beta,coder" {
		t.Fatalf("the call's model %q card %q, want bb and alpha,beta,coder", model, card)
	}
	if got := store.caseBrief[5]; got != [2]string{happened, left} || len(store.caseFailed) != 0 {
		t.Fatalf("brief %v failed %v, want it written", got, store.caseFailed)
	}
}

// A human card's needs-you row is briefed under the policy's classes too.
func TestBriefWriterBriefsAHumanCardsRowAsThePolicysClassesBrief(t *testing.T) {
	var model, card string
	reply := answerReply(`{"brief": "b", "detail": "d"}`)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		model, card = in.Model, r.Header.Get(gateway.HeaderFactoryCard)
		_, _ = w.Write([]byte(reply))
	}))
	defer gw.Close()
	row := ApprovalRequest{ID: 7, Kind: ApprovalPlan, TaskID: "t1", Project: "o/r",
		Spec: planSpec(t), Reason: "a question", CreatedAt: time.Unix(0, 0).UTC()}
	store := &fakeBriefStore{tasks: map[string]Task{"t1": {ID: "t1", Repository: "o/r"}},
		rows: []ApprovalRequest{row}, brief: map[int64][2]string{}, failed: map[int64]string{}}
	w := &BriefWriter{Store: store, CardDigest: briefLife, Model: &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}, Current: func() policy.Version {
		v := briefVersion(gw.URL)
		v.Document.Classes = map[string]policy.Class{"alpha": {}, "beta": {SupervisorModel: "bb"}, policy.ClassCoder: {SupervisorModel: "supervisor"}}
		return v
	}}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model != "bb" || card != "alpha,beta,coder" || store.brief[7] != [2]string{"b", "d"} {
		t.Fatalf("model %q card %q brief %v, want bb, alpha,beta,coder and the brief written", model, card, store.brief[7])
	}
}

// The case writer is told of a refusal only when its message carries one.
func TestCaseBriefPromptTellsOfLastRefusalOnlyWhenSet(t *testing.T) {
	for refused, want := range map[bool]bool{false: false, true: true} {
		if got := strings.Contains(testPrompt(t, "case-brief-writer", caseBriefFacts{Refused: refused}).Text(), "last_refusal"); got != want {
			t.Errorf("refused %v: the prompt tells of it = %v, want %v", refused, got, want)
		}
	}
}

// A happened line that holds an amount of money is refused, naming the
// amount and what to tell instead; a line without one passes.
func TestCaseBriefRefusesAnAmount(t *testing.T) {
	_, _, err := checkCaseBrief(CaseBriefAnswer{Happened: "The card spent EUR 4.20 before it stopped.", Left: "Decide."})
	var no *noBriefError
	if !errors.As(err, &no) || !strings.Contains(no.Error(), `names the amount "EUR 4.20"`) || !strings.Contains(no.Error(), "tell what stopped the card") {
		t.Fatalf("an amount: %v, want a refusal naming it and the fix", err)
	}
	if _, _, err := checkCaseBrief(CaseBriefAnswer{Happened: "The card stopped at its time bound.", Left: "Decide."}); err != nil {
		t.Fatalf("no amount: %v", err)
	}
}
