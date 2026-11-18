package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A dry run writes nothing, to the store or the forge; a run writes what
// the table it reads holds, the owner's corrections and strikes taken;
// a second run writes nothing.
func TestDryWritesNothingAndTheRunWritesTheTable(t *testing.T) {
	ctx := context.Background()
	s := liveBoard()
	f := forgeOf(s)
	calls := modelGateway(t)
	p := &pass{store: s, forge: f, refs: allRefs, policy: version(), out: &bytes.Buffer{},
		model: &factoryd.SupervisorModel{GatewayURL: calls.url, Token: "tok", Budget: factoryd.TokenBudget{AnswerTokens: 2048, HeadroomTokens: 16384}}}

	drafted, err := p.draft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.writes) > 0 || len(f.edits) > 0 {
		t.Fatalf("the dry run wrote: store %q, forge %q", s.writes, f.edits)
	}
	if got := calls.count(); got != 9 {
		t.Errorf("the dry run made %d model calls, want 9: one each for #11 and #16, two for #17, three for #18, one name, one promise", got)
	}
	wantCards := []cardRow{
		{Task: "t-11", Card: "sf/app#11", OldTitle: "The doClose refusal narrowed to plans that hold criteria", Title: "Narrow the close refusal to plans that hold criteria",
			Why: "So a plan with no criteria closes without a waiver.", Summary: summary11},
		{Task: "t-16", Card: "sf/app#16", OldTitle: "The two reproducible manifests name the proofs trees", Title: "Change two lines in the runner's build files",
			Why: why16},
		{Task: "t-17", Card: "sf/app#17", OldTitle: "Skipped rows: count them", Title: "Count the rows the import skips", Why: "So the import report says what it dropped.", Summary: summary17},
		{Task: "t-18", Card: "sf/app#18", OldTitle: "Gateway: retry loop", Title: "Gateway: fix the retry loop", Why: "So a retried call keeps its request id.", Summary: summary18, Unchecked: true},
	}
	if !reflect.DeepEqual(drafted.Cards, wantCards) {
		t.Errorf("cards\n got %+v\nwant %+v", drafted.Cards, wantCards)
	}
	wantNames := []nameRow{{Plan: "t-plan", Card: "sf/app#10", Old: "empty-commit-fires-gate", Name: "Close refusal fix"}}
	if !reflect.DeepEqual(drafted.Names, wantNames) {
		t.Errorf("names = %+v, want %+v", drafted.Names, wantNames)
	}
	wantPromises := []promiseRow{{Plan: "t-plan", Card: "sf/app#10", Revisions: []int{1}, Criterion: "closes_empty",
		Claim: "Closing a plan that holds no criteria needs no waiver.", Promise: "A plan with no criteria closes without a waiver."}}
	if !reflect.DeepEqual(drafted.Promises, wantPromises) {
		t.Errorf("promises = %+v, want %+v", drafted.Promises, wantPromises)
	}
	out := p.out.(*bytes.Buffer).String()
	for _, line := range []string{
		"| sf/app#18 | Gateway: retry loop | Gateway: fix the retry loop | So a retried call keeps its request id. | yes |",
		`- sf/app#10: "empty-commit-fires-gate" becomes "Close refusal fix".`,
		`- sf/app#19 waits with no case: it asks "The card used all 3 sessions. Allow more, or end the card?" again, which opens its case.`,
		`- sf/app#16, case 61: it asks "Change two lines in the runner's build files" with today's answers: I did it, I cannot do this, Not now.`,
		`- sf/app#21, case 62: it asks "Repair the red chain of the app" with today's answers: I did it, Not now.`,
		"- sf/app#20, case 80 (other): its face, stored before it counted findings, is stored again from today's facts at factoryd's next minute pass.",
		`- sf/app#20, case 80: the case writer's story names the answer "Not now", so the writer writes it again.`,
		"- sf/app#12 keeps its brief: a human wrote its title.",
		"- sf/app#13 keeps its brief: a human's own issue keeps its title.",
		"- sf/app#14 keeps its brief: it runs a session, which keeps the title it started with; a later run of the pass takes it.",
		"- sf/app#15 keeps its brief: it passes the door.",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the dry run's output lacks %q:\n%s", line, out)
		}
	}

	// the owner corrects #11's why line and strikes #18
	var reviewed table
	raw, _ := json.Marshal(drafted)
	if err := json.Unmarshal(raw, &reviewed); err != nil {
		t.Fatal(err)
	}
	reviewed.Cards[0].Why = "So a plan that holds no criteria closes without a waiver."
	reviewed.Cards = reviewed.Cards[:3]
	p.model, p.out = nil, &bytes.Buffer{}
	if err := p.apply(ctx, reviewed); err != nil {
		t.Fatalf("%v\n%s", err, p.out)
	}
	if got := calls.count(); got != 9 {
		t.Errorf("the run called the model: %d calls in all, want the dry run's 9", got)
	}
	wantEdits := []string{
		"#11 Narrow the close refusal to plans that hold criteria\nWhy: So a plan that holds no criteria closes without a waiver.\n\nSummary: " + summary11 + "\n\n---\n\nThe console refuses to close a plan while criteria are open.",
		"#16 Change two lines in the runner's build files\nWhy: " + why16 + "\n\n---\n\nEdit Dockerfile.runner and ci.yml.",
		"#17 Count the rows the import skips\nWhy: So the import report says what it dropped.\n\nSummary: " + summary17 + "\n\n---\n\nThe import drops rows without a word.",
	}
	if !reflect.DeepEqual(f.edits, wantEdits) {
		t.Errorf("issue edits\n got %q\nwant %q", f.edits, wantEdits)
	}
	wantWrites := []string{
		"objective t-11 Narrow the close refusal to plans that hold criteria\n\nThe console refuses to close a plan while criteria are open.",
		"brief t-11 So a plan that holds no criteria closes without a waiver. | " + summary11 + " | false",
		"event t-11 card_edited factoryd/factoryd rebrief=true title=Narrow the close refusal to plans that hold criteria",
		"objective t-16 Change two lines in the runner's build files\n\nEdit Dockerfile.runner and ci.yml.",
		"brief t-16 " + why16 + " |  | false",
		"event t-16 card_edited factoryd/factoryd rebrief=true title=Change two lines in the runner's build files",
		"objective t-17 Count the rows the import skips\n\nThe import drops rows without a word.",
		"brief t-17 So the import report says what it dropped. | " + summary17 + " | false",
		"event t-17 card_edited factoryd/factoryd rebrief=true title=Count the rows the import skips",
		"plan name t-plan Close refusal fix",
		`plan revision t-plan 1 criteria [closes_empty:"A plan with no criteria closes without a waiver." stays_green:"The tests stay green."]`,
		"event t-19 question_asked factoryd/factoryd ask=The card used all 3 sessions. Allow more, or end the card? comment=501 reason=the session cap is reached source=cap",
		"case facts 61 " + face16,
		"case face 61 " + face16,
		"case facts 62 " + face21,
		"case face 62 " + face21,
		"case face 80 story=<nil>",
		`case brief 80 "" ""`,
	}
	if !reflect.DeepEqual(s.writes, wantWrites) {
		t.Errorf("store writes\n got %q\nwant %q", s.writes, wantWrites)
	}

	s.writes, f.edits, p.out = nil, nil, &bytes.Buffer{}
	if err := p.apply(ctx, reviewed); err != nil {
		t.Fatalf("the second run: %v\n%s", err, p.out)
	}
	if len(s.writes) > 0 || len(f.edits) > 0 {
		t.Errorf("the second run wrote: store %q, forge %q", s.writes, f.edits)
	}
}

const (
	summary11 = "The console refuses to close a plan while criteria are open. This card limits the refusal to plans that hold criteria. A test closes a plan with none and one with two."
	why16     = "The push rule keeps agents out of workflow files, so a human makes these two edits."
	summary17 = "The import counts the rows it skips and prints the count. A test imports a file with two bad rows."
	summary18 = "The gateway keeps the request id when it retries a call. A test retries a call twice."
	// face16 is #16's question as its plan files one today
	face16 = `ask=Change two lines in the runner's build files check=The factory does not check this. The card closes, and the plan goes on. ` +
		`offers=[{"value":"done","label":"I did it"},{"value":"hand_back","label":"I cannot do this"},{"value":"park","label":"Not now"}] plan=t-plan ` +
		`waits=The plan waits for it. why=The factory's push rule keeps agents out of workflow files, so a human does this card, and plan #10 waits for it`
	// face21 is #21's stored question with today's value of I did it
	face21 = `ask=Repair the red chain of the app check=<nil> ` +
		`offers=[{"value":"done","label":"I did it"},{"value":"park","label":"Not now"}] plan=<nil> waits=<nil> why=The merge queue waits until the chain is green.`
)

func version() policy.Version {
	return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{"app": {Repository: "sf/app"}},
		Document: policy.Document{Limits: policy.Limits{SupervisorTimeout: time.Minute}}}}
}

var plan10 = `{"spec": {"outcome": "A plan without criteria closes without a waiver."}, "summary": "Narrow the close refusal.",
	"criteria": [{"id": "closes_empty", "kind": "test", "claim": "closing a plan that holds no criteria needs no waiver", "suite": "internal/console", "name": "TestCloseEmpty"},
		{"id": "stays_green", "kind": "invariant", "suite": "./...", "promise": "The tests stay green."}],
	"cards": [{"key": "narrow", "title": "The doClose refusal narrowed to plans that hold criteria"}, {"key": "hand", "title": "The two reproducible manifests name the proofs trees"}]}`

// liveBoard is the live board the test reads: a plan whose name is a slug and
// one criterion without a promise, and cards that each meet one rule.
func liveBoard() *fakeStore {
	card := func(issue int64, status, objective string) factoryd.Task {
		return factoryd.Task{ID: fmt.Sprintf("t-%d", issue), IssueNumber: issue, Status: status, Repository: "sf/app", Kind: "agent", Objective: objective}
	}
	s := &fakeStore{tasks: map[string]factoryd.Task{}, events: map[string][]factoryd.Event{}, asks: map[int64]factoryd.CaseAsk{},
		revisions: map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: json.RawMessage(plan10)}}},
		confirmed: map[string]int{"t-plan": 1}}
	add := func(t factoryd.Task) { s.tasks[t.ID] = t }
	add(factoryd.Task{ID: "t-plan", IssueNumber: 10, Status: "done", Repository: "sf/app", Kind: "agent", Track: policy.TrackPlan,
		Objective: "Plan the close refusal\n\nThe plan's text.", PlanName: "empty-commit-fires-gate"})
	c11 := card(11, "queued", "The doClose refusal narrowed to plans that hold criteria\n\nThe console refuses to close a plan while criteria are open.")
	c11.Source, c11.Parent, c11.PlanKey = factoryd.SourcePlan, "t-plan", "narrow"
	add(c11)
	add(card(12, "in-review", "Keep the nightly snapshots\n\nThe export keeps a week."))
	s.events["t-12"] = []factoryd.Event{{TaskID: "t-12", Kind: factoryd.EventCardEdited, Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"title": "Keep the nightly snapshots"}}}
	c13 := card(13, "queued", "Board export: keep it\n\nPlease.")
	c13.Source = factoryd.SourceWebhook
	add(c13)
	add(card(14, "running", "The flaky test\n\nIt fails one run in ten."))
	c15 := card(15, "queued", "Count the skipped rows\n\nThe import drops rows.")
	c15.Why, c15.Summary = "So the import report says what it dropped.", summary17
	add(c15)
	c16 := card(16, "waiting-human", "The two reproducible manifests name the proofs trees\n\nEdit Dockerfile.runner and ci.yml.")
	c16.Kind, c16.Source, c16.Parent, c16.PlanKey, c16.Why = "human", factoryd.SourcePlan, "t-plan", "hand", "The case is the human card."
	add(c16)
	add(card(17, "queued", "Skipped rows: count them\n\nThe import drops rows without a word."))
	add(card(18, "queued", "Gateway: retry loop\n\nThe retry loses the request id."))
	c19 := card(19, "waiting-human", "Fix the import of old snapshots\n\nThe import refuses the old format.")
	c19.Why, c19.Summary = "So old snapshots load again.", "The import reads the old snapshot format. A test loads a snapshot of each format."
	add(c19)
	s.events["t-19"] = []factoryd.Event{{TaskID: "t-19", Kind: factoryd.EventQuestionAsked, Detail: map[string]any{
		"ask": "The card used all 3 sessions. Allow more, or end the card?", "source": "cap", "reason": "the session cap is reached",
		"case_id": 70, "story": map[string]any{"planned": "This card builds it."}, "comment": 501}}}
	c20 := card(20, "waiting-human", "Fix the export of large boards\n\nThe export runs out of memory.")
	c20.Why, c20.Summary = "So a board of 5,000 cards exports.", "The export streams its rows. A test exports a board of 5,000 cards."
	add(c20)
	c21 := card(21, "waiting-human", "Repair the red chain of the app\n\nThe chain is red at abc1234.")
	c21.Kind, c21.Source, c21.Why = "human", factoryd.SourceChainRepair, "The merge queue waits until the chain is green."
	add(c21)

	old := []factoryd.QuestionChoice{{Value: "close", Label: "I did it"}, {Value: "park", Label: "Not now"}}
	s.seat(factoryd.Case{ID: 61, TaskID: "t-16", Repository: "sf/app", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "The two reproducible manifests name the proofs trees", "why": "The case is the human card.", "offers": old, "source": factoryd.SourcePlan, "by": "factoryd"}})
	s.seat(factoryd.Case{ID: 62, TaskID: "t-21", Repository: "sf/app", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "Repair the red chain of the app", "why": c21.Why, "offers": old, "source": factoryd.SourceChainRepair, "by": "factoryd"}})
	s.seat(factoryd.Case{ID: 80, TaskID: "t-20", Repository: "sf/app", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman,
		Brief: "Press Not now to wait for the export.", BriefDetail: "The export of the large board is left.",
		Facts: map[string]any{"ask": "The export stopped. Try again?", "story": map[string]any{"planned": "This card builds it."}}})
	return s
}

func forgeOf(s *fakeStore) *fakeForge {
	f := &fakeForge{issues: map[int64][2]string{}}
	for _, t := range s.tasks {
		title, body, _ := strings.Cut(t.Objective, "\n\n")
		f.issues[t.IssueNumber] = [2]string{title, body}
	}
	return f
}

// calls is the fake gateway: it answers each brief, name and promise call
// as the small model would, and counts them.
type calls struct {
	url string
	mu  sync.Mutex
	n   int
}

func (c *calls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func modelGateway(t *testing.T) *calls {
	c := &calls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "bad call", http.StatusBadRequest)
			return
		}
		if body.Model != policy.Small || r.Header.Get(gateway.HeaderFactoryPurpose) != policy.PurposeTitle || r.Header.Get(gateway.HeaderFactoryTask) == "" {
			t.Errorf("a call on %q for %q of %q, want the small model for title on a card", body.Model, r.Header.Get(gateway.HeaderFactoryPurpose), r.Header.Get(gateway.HeaderFactoryTask))
		}
		var in struct {
			Card        struct{ Title string }
			LastRefusal []string `json:"last_refusal"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(body.Messages[1].Content, factoryd.ChatInputLead)), &in)
		system := body.Messages[0].Content
		var answer any
		switch {
		case strings.HasPrefix(system, "You name build plans"):
			answer = map[string]string{"name": "Close refusal fix"}
		case strings.HasPrefix(system, "You write the promise"):
			answer = map[string]any{"promises": map[string]string{"closes_empty": "A plan with no criteria closes without a waiver."}}
		case in.Card.Title == "The doClose refusal narrowed to plans that hold criteria":
			answer = brief("Narrow the close refusal to plans that hold criteria", "So a plan with no criteria closes without a waiver.", summary11)
		case in.Card.Title == "The two reproducible manifests name the proofs trees":
			answer = brief("Change two lines in the runner's build files", why16, "")
		case in.Card.Title == "Skipped rows: count them" && len(in.LastRefusal) == 0:
			answer = brief("The skipped rows", "So the import report says what it dropped.", summary17)
		case in.Card.Title == "Skipped rows: count them":
			answer = brief("Count the rows the import skips", "So the import report says what it dropped.", summary17)
		case in.Card.Title == "Gateway: retry loop":
			answer = brief("Gateway: fix the retry loop", "So a retried call keeps its request id.", summary18)
		default:
			t.Errorf("a call the test does not answer: %s", body.Messages[1].Content)
		}
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
		content, _ := json.Marshal(answer)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}}}})
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	return c
}

func brief(title, why, summary string) map[string]string {
	return map[string]string{"title": title, "why": why, "summary": summary}
}

// allRefs resolves every reference: the test's texts name none that fail.
func allRefs(context.Context, string) (factoryd.RefResolver, error) { return refs{}, nil }

type refs struct{}

func (refs) Issue(int) (string, string, bool) { return "", "open", true }
func (refs) Files(path string) []string       { return []string{path} }
func (refs) Heading(string, string) bool      { return true }
func (refs) Err() error                       { return nil }

type fakeForge struct {
	issues map[int64][2]string
	edits  []string
}

func (f *fakeForge) Issue(_ context.Context, _ string, n int64) (string, string, error) {
	return f.issues[n][0], f.issues[n][1], nil
}

func (f *fakeForge) EditIssue(_ context.Context, _ string, n int64, title, body string) error {
	f.issues[n] = [2]string{title, body}
	f.edits = append(f.edits, fmt.Sprintf("#%d %s\n%s", n, title, body))
	return nil
}

// fakeStore is factoryd's store as the pass reads and writes it; writes
// lists each write it took, in order.
type fakeStore struct {
	tasks     map[string]factoryd.Task
	events    map[string][]factoryd.Event
	cases     []factoryd.Case
	asks      map[int64]factoryd.CaseAsk
	revisions map[string][]factoryd.PlanRevision
	confirmed map[string]int
	writes    []string
}

// seat opens case c with its ask row, which names it.
func (s *fakeStore) seat(c factoryd.Case) {
	s.cases = append(s.cases, c)
	detail := maps.Clone(c.Facts)
	detail["case_id"] = c.ID
	s.asks[c.ID] = factoryd.CaseAsk{Detail: detail}
}

func (s *fakeStore) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *fakeStore) Dependencies(context.Context, string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (s *fakeStore) PlanRevision(_ context.Context, id string, n int) (factoryd.PlanRevision, bool, error) {
	revs := s.revisions[id]
	for i := len(revs) - 1; i >= 0; i-- {
		if n == 0 || revs[i].Revision == n {
			return revs[i], true, nil
		}
	}
	return factoryd.PlanRevision{}, false, nil
}

func (s *fakeStore) PlanCards(_ context.Context, plan string) ([]factoryd.Task, error) {
	var out []factoryd.Task
	for _, t := range s.tasks {
		if t.Parent == plan && t.Source == factoryd.SourcePlan {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *fakeStore) PlanQuestions(context.Context, string, int) ([]factoryd.PlanQuestion, error) {
	return nil, nil
}

func (s *fakeStore) OpenPlanNames(_ context.Context, repository, exclude string) ([]string, error) {
	var out []string
	for _, t := range s.tasks {
		if t.Repository == repository && t.ID != exclude && t.Track == policy.TrackPlan && t.PlanName != "" {
			out = append(out, t.PlanName)
		}
	}
	return out, nil
}

func (s *fakeStore) UnnamedOpenPlans(context.Context) ([]factoryd.Task, error) { return nil, nil }

func (s *fakeStore) SetPlanName(_ context.Context, id, name string) error {
	t := s.tasks[id]
	t.PlanName = name
	s.tasks[id] = t
	s.writes = append(s.writes, "plan name "+id+" "+name)
	return nil
}

func (s *fakeStore) BoardTasks(_ context.Context, repository string) ([]factoryd.BoardTask, error) {
	var out []factoryd.BoardTask
	for _, t := range s.tasks {
		if t.Repository == repository && !factoryd.Final(t.Status) && t.Kind != factoryd.KindChat && !t.Report() {
			out = append(out, factoryd.BoardTask{Task: t})
		}
	}
	return out, nil
}

func (s *fakeStore) CaseDesk(_ context.Context, repository string) (factoryd.CaseDesk, error) {
	d := factoryd.CaseDesk{Asks: map[int64]factoryd.CaseAsk{}}
	for _, c := range s.cases {
		if c.Repository == repository && (c.Status == factoryd.CaseOpen || c.Status == factoryd.CaseAtHuman) {
			d.Cases = append(d.Cases, c)
			d.Asks[c.ID] = s.asks[c.ID]
		}
	}
	return d, nil
}

func (s *fakeStore) TaskEventsOfKind(_ context.Context, id, kind string, limit int) ([]factoryd.Event, error) {
	var out []factoryd.Event
	for _, e := range slices.Backward(s.events[id]) {
		if e.Kind == kind && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) PlanConfirmed(_ context.Context, id string) (int, bool, error) {
	n, ok := s.confirmed[id]
	return n, ok, nil
}

func (s *fakeStore) SetObjective(_ context.Context, id, objective string) error {
	t := s.tasks[id]
	t.Objective = objective
	s.tasks[id] = t
	s.writes = append(s.writes, "objective "+id+" "+objective)
	return nil
}

func (s *fakeStore) SetBrief(_ context.Context, id, why, summary string, unchecked bool) error {
	t := s.tasks[id]
	t.Why, t.Summary, t.BriefUnchecked = why, summary, unchecked
	s.tasks[id] = t
	s.writes = append(s.writes, fmt.Sprintf("brief %s %s | %s | %v", id, why, summary, unchecked))
	return nil
}

// Record keeps e, and a question_asked opens its case at the human, as
// the store's ask door does.
func (s *fakeStore) Record(_ context.Context, e factoryd.Event) (int64, error) {
	s.events[e.TaskID] = append(s.events[e.TaskID], e)
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(e.Detail)) {
		if k == "rebrief" || k == "title" || e.Kind == factoryd.EventQuestionAsked {
			keys = append(keys, fmt.Sprintf("%s=%v", k, e.Detail[k]))
		}
	}
	s.writes = append(s.writes, fmt.Sprintf("event %s %s %s/%s %s", e.TaskID, e.Kind, e.Actor, e.Via, strings.Join(keys, " ")))
	if e.Kind == factoryd.EventQuestionAsked {
		s.seat(factoryd.Case{ID: int64(100 + len(s.cases)), TaskID: e.TaskID, Repository: e.Repository, Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Facts: maps.Clone(e.Detail)})
	}
	return int64(len(s.writes)), nil
}

func (s *fakeStore) SetCaseFacts(_ context.Context, id int64, facts map[string]any) error {
	i := slices.IndexFunc(s.cases, func(c factoryd.Case) bool { return c.ID == id })
	maps.Copy(s.cases[i].Facts, facts)
	s.writes = append(s.writes, fmt.Sprintf("case facts %d %s", id, faceWords(facts)))
	return nil
}

func (s *fakeStore) SetCaseFace(_ context.Context, id int64, keys map[string]any) error {
	maps.Copy(s.asks[id].Detail, keys)
	s.writes = append(s.writes, fmt.Sprintf("case face %d %s", id, faceWords(keys)))
	return nil
}

func (s *fakeStore) SetCaseBrief(_ context.Context, id int64, happened, left string) error {
	i := slices.IndexFunc(s.cases, func(c factoryd.Case) bool { return c.ID == id })
	s.cases[i].Brief, s.cases[i].BriefDetail = happened, left
	s.writes = append(s.writes, fmt.Sprintf("case brief %d %q %q", id, happened, left))
	return nil
}

func (s *fakeStore) SetPlanRevision(_ context.Context, plan string, revision int, raw json.RawMessage) error {
	for i, r := range s.revisions[plan] {
		if r.Revision == revision {
			s.revisions[plan][i].Plan = raw
		}
	}
	var p factoryd.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	var promises []string
	for _, c := range p.Criteria {
		promises = append(promises, fmt.Sprintf("%s:%q", c.ID, c.Promise))
	}
	s.writes = append(s.writes, fmt.Sprintf("plan revision %s %d criteria %v", plan, revision, promises))
	return nil
}

// faceWords are the keys of a face or a question the test reads, each as
// JSON when it is no string.
func faceWords(keys map[string]any) string {
	if len(keys) == 1 {
		for k, v := range keys {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	var out []string
	for _, k := range []string{"ask", "check", "offers", "plan", "waits", "why"} {
		v, ok := keys[k].(string)
		if !ok {
			raw, _ := json.Marshal(keys[k])
			v = string(raw)
			if keys[k] == nil {
				v = "<nil>"
			}
		}
		out = append(out, k+"="+v)
	}
	return strings.Join(out, " ")
}
