package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
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
	calls := modelGateway(t, briefAnswers)
	p := newPass(s, f, calls)

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
	for _, m := range calls.messages() {
		if strings.Contains(m, "abc123secretvalue") {
			t.Errorf("a model call carried #18's token: %s", m)
		}
	}
	wantCards := []cardRow{
		{Task: "t-11", Card: "sf/app#11", OldTitle: "The doClose refusal narrowed to plans that hold criteria", Title: "Narrow the close refusal to plans that hold criteria",
			Why: "So a plan with no criteria closes without a waiver.", Summary: summary11},
		{Task: "t-16", Card: "sf/app#16", OldTitle: "The two reproducible manifests name the proofs trees", Title: "Change two lines in the runner's build files",
			Why: why16},
		{Task: "t-17", Card: "sf/app#17", OldTitle: "Skipped rows: count them", Title: "Count the rows the import skips", Why: "So the import report says what it dropped.", Summary: summary17},
		{Task: "t-18", Card: "sf/app#18", OldTitle: "Gateway: retry loop", Title: "Gateway: fix the retry loop", Why: "So a retried call keeps its request id.",
			Summary: "The gateway keeps the request id when it retries a call. A test retries a call twice with the key [redacted].", Unchecked: true},
	}
	if !reflect.DeepEqual(drafted.Cards, wantCards) {
		t.Errorf("cards\n got %+v\nwant %+v", drafted.Cards, wantCards)
	}
	wantNames := []nameRow{{Plan: "t-plan", Card: "sf/app#10", Old: "empty-commit-fires-gate", Name: "Close refusal fix"}}
	if !reflect.DeepEqual(drafted.Names, wantNames) {
		t.Errorf("names = %+v, want %+v", drafted.Names, wantNames)
	}
	wantPromises := []promiseRow{
		{Plan: "t-plan", Card: "sf/app#10", Revisions: []int{1}, Criterion: "stays_green", Claim: "The tests in ./... stay green.", Promise: "The tests stay green."},
		{Plan: "t-plan", Card: "sf/app#10", Revisions: []int{2, 1}, Criterion: "closes_empty",
			Claim: "Closing a plan that holds no criteria needs no waiver.", Promise: "A plan with no criteria closes without a waiver."},
	}
	if !reflect.DeepEqual(drafted.Promises, wantPromises) {
		t.Errorf("promises\n got %+v\nwant %+v", drafted.Promises, wantPromises)
	}
	out := p.out.(*bytes.Buffer).String()
	if strings.Contains(out, "case 65") {
		t.Errorf("the month's cap case of a plan's human card is asked as the plan's question:\n%s", out)
	}
	for _, line := range []string{
		"| sf/app#18 | Gateway: retry loop | Gateway: fix the retry loop | So a retried call keeps its request id. | yes |",
		`- sf/app#10: "empty-commit-fires-gate" becomes "Close refusal fix".`,
		`- sf/app#10, criterion stays_green: "The tests in ./... stay green." becomes "The tests stay green.".`,
		`- sf/app#19 waits with no case: it asks "The card used all 3 sessions. Allow more, or end the card?" again, which opens its case.`,
		`- sf/app#23 waits with no case: it asks "Look at the error rate of the export" again, which opens its case.`,
		`- sf/app#16, case 61: it asks "Change two lines in the runner's build files" with today's answers: I did it, I cannot do this, Not now.`,
		`- sf/app#21, case 62: it asks "Repair the red chain of the app" with today's answers: I did it, Not now.`,
		`- sf/app#22, case 63: it asks "Look at the 5xx rate of the app" with today's answers: Noted, Not now.`,
		`- sf/app#25, case 64: it asks "Look at the 5xx rate of the export" with today's answers: Noted, Not now.`,
		"- sf/app#20, case 80 (other): its face, stored before it counted findings, is stored again from today's facts at factoryd's next minute pass.",
		`- sf/app#20, case 80: the case writer's story names the answer "Not now", so the writer writes it again.`,
		"- sf/app#12 keeps its brief: a human wrote its title.",
		"- sf/app#13 keeps its brief: a human's own issue keeps its title.",
		"- sf/app#14 keeps its brief: it runs a session, which keeps the title it started with; a later run of the pass takes it.",
		"- sf/app#15 keeps its brief: it passes the door.",
		"- sf/app#24 waits on its author with no case: factoryd's minute pass asks its information request on the board, if it has one; else answer it on its issue, or end it.",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the dry run's output lacks %q:\n%s", line, out)
		}
	}

	// the owner corrects #11's why line and strikes #18; an earlier run
	// stopped after it retitled #17's issue and its card
	var reviewed table
	raw, _ := json.Marshal(drafted)
	if err := json.Unmarshal(raw, &reviewed); err != nil {
		t.Fatal(err)
	}
	reviewed.Cards[0].Why = "So a plan that holds no criteria closes without a waiver."
	reviewed.Cards = reviewed.Cards[:3]
	c17 := s.tasks["t-17"]
	c17.Objective = "Count the rows the import skips\n\nThe import drops rows without a word."
	s.tasks["t-17"] = c17
	f.issues[17] = [2]string{"Count the rows the import skips", "Why: So the import report says what it dropped.\n\nSummary: " + summary17 + "\n\n---\n\nThe import drops rows without a word."}
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
		"brief t-17 So the import report says what it dropped. | " + summary17 + " | false",
		"event t-17 card_edited factoryd/factoryd rebrief=true title=Count the rows the import skips",
		"plan name t-plan Close refusal fix",
		`plan revision t-plan 1 criteria [closes_empty:"" stays_green:"The tests stay green."]`,
		`plan revision t-plan 2 criteria [closes_empty:"A plan with no criteria closes without a waiver." stays_green:"The tests stay green."]`,
		`plan revision t-plan 1 criteria [closes_empty:"A plan with no criteria closes without a waiver." stays_green:"The tests stay green."]`,
		"event t-19 question_asked factoryd/factoryd ask=The card used all 3 sessions. Allow more, or end the card? comment=501 reason=the session cap is reached source=cap",
		"event t-23 question_asked factoryd/factoryd ask=" + ask23 + " by=factoryd choices=[Noted Not now] info= offers=[{noted Noted} {park Not now}] preselected=map[action:choice:noted] " +
			"question=" + ask23 + " recommendation=The alert no longer fires. recommended_by=the factory resolved_at=2026-11-17T10:00:00Z source=alert why=" + why23,
		"ask face 61 " + face16 + " | row offers=done:I did it,hand_back:I cannot do this,park:Not now pick=done by=the factory agent story=false",
		"ask face 62 " + face21 + " | row offers=done:I did it,park:Not now pick=choice:done by=the factory agent story=false",
		"ask face 63 " + face22 + " | row offers=noted:Noted,park:Not now pick=<nil> by=<nil> story=true",
		"ask face 64 " + face25 + " | row offers=park:Not now,noted:Noted pick=choice:park by=the factory agent story=true",
		"case face 80 story=<nil>",
		`case brief 80 "" ""`,
	}
	if !reflect.DeepEqual(s.writes, wantWrites) {
		t.Errorf("store writes\n got %q\nwant %q", s.writes, wantWrites)
	}
	// what Act reads to take an answer on a human card: today's values
	if got := factoryd.AskOffers(s.asks[63].Detail); !reflect.DeepEqual(got, []factoryd.QuestionChoice{{Value: "noted", Label: "Noted"}, {Value: "park", Label: "Not now"}}) {
		t.Errorf("the alert card's ask row offers %v, want noted and park", got)
	}

	s.writes, f.edits, p.out = nil, nil, &bytes.Buffer{}
	if err := p.apply(ctx, reviewed); err != nil {
		t.Fatalf("the second run: %v\n%s", err, p.out)
	}
	if len(s.writes) > 0 || len(f.edits) > 0 {
		t.Errorf("the second run wrote: store %q, forge %q", s.writes, f.edits)
	}
}

// Two plans whose names are slugs draft two names: the second plan's
// namer finds the first's name taken. A name the owner corrects past the
// namer's bounds, or onto another open plan's name, is not written.
func TestTheNamesOfTwoSlugPlansDiffer(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	for i, id := range []string{"t-a", "t-b"} {
		s.tasks[id] = factoryd.Task{ID: id, IssueNumber: int64(30 + i), Status: "queued", Repository: "sf/app", Kind: "agent", Track: policy.TrackPlan,
			Objective: "Plan the close refusal\n\nText.", PlanName: "close-refusal", Why: "So plans close cleanly.", Summary: "The plan narrows the close refusal. A test closes a plan."}
	}
	s.tasks["t-taken"] = factoryd.Task{ID: "t-taken", IssueNumber: 40, Status: "queued", Repository: "sf/app", Kind: "agent", Track: policy.TrackPlan,
		Objective: "Plan the export\n\nText.", PlanName: "Board export", Why: "So boards export.", Summary: "The plan exports boards. A test exports one."}
	calls := modelGateway(t, func(system string, in input) any {
		if strings.Contains(in.Raw, "Close refusal fix") {
			return map[string]string{"name": "Close refusal rules"}
		}
		return map[string]string{"name": "Close refusal fix"}
	})
	p := newPass(s, &fakeForge{issues: map[int64][2]string{}}, calls)
	drafted, err := p.draft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []nameRow{{Plan: "t-a", Card: "sf/app#30", Old: "close-refusal", Name: "Close refusal fix"}, {Plan: "t-b", Card: "sf/app#31", Old: "close-refusal", Name: "Close refusal rules"}}
	if !reflect.DeepEqual(drafted.Names, want) {
		t.Fatalf("names = %+v, want %+v", drafted.Names, want)
	}
	drafted.Names[0].Name = "Close the refusal for every plan that holds criteria"
	drafted.Names[1].Name = "board EXPORT"
	p.model, p.out = nil, &bytes.Buffer{}
	err = p.apply(ctx, table{Names: drafted.Names})
	for _, line := range []string{
		`- sf/app#30: the name "Close the refusal for every plan that holds criteria" does not hold: a plan's name has 3 to 24 characters on one line, and no other open plan of the project has it`,
		`- sf/app#31: the name "board EXPORT" does not hold`,
	} {
		if !strings.Contains(p.out.(*bytes.Buffer).String(), line) {
			t.Errorf("the run's output lacks %q:\n%s", line, p.out)
		}
	}
	if err == nil || len(s.writes) > 0 {
		t.Errorf("the run = %v with writes %q, want both names left", err, s.writes)
	}
}

// A name the owner corrected with a double space is written as the namer
// writes names, and a second run finds it standing.
func TestACorrectedNameStandsOnASecondRun(t *testing.T) {
	s := newStore()
	s.tasks["t-a"] = factoryd.Task{ID: "t-a", IssueNumber: 30, Status: "queued", Repository: "sf/app", Kind: "agent", Track: policy.TrackPlan, PlanName: "close-refusal"}
	p := newPass(s, &fakeForge{issues: map[int64][2]string{}}, nil)
	rows := table{Names: []nameRow{{Plan: "t-a", Card: "sf/app#30", Old: "close-refusal", Name: "Close  refusal fix"}}}
	for run := 1; run <= 2; run++ {
		if err := p.apply(context.Background(), rows); err != nil {
			t.Fatalf("run %d: %v\n%s", run, err, p.out)
		}
	}
	if want := []string{"plan name t-a Close refusal fix"}; !reflect.DeepEqual(s.writes, want) {
		t.Errorf("writes %q, want %q", s.writes, want)
	}
}

// A dry run into a table file that exists makes no model call, and a dry
// run that fails leaves no table file.
func TestADryRunIntoAnExistingTableCallsNoModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.json")
	if err := os.WriteFile(path, []byte(`{"cards": [{"title": "corrected"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := liveBoard()
	calls := modelGateway(t, briefAnswers)
	p := newPass(s, forgeOf(s), calls)
	if err := p.dryRun(context.Background(), path); !errors.Is(err, fs.ErrExist) || calls.count() > 0 {
		t.Errorf("a dry run into an existing table = %v with %d model calls, want it refused with none", err, calls.count())
	}
	if raw, _ := os.ReadFile(path); string(raw) != `{"cards": [{"title": "corrected"}]}` {
		t.Errorf("the table now reads %s", raw)
	}
	fresh := filepath.Join(t.TempDir(), "table.json")
	p.policy.Tree.Projects["lab"] = policy.ProjectConfig{Repository: "sf/broken"}
	s.broken = "sf/broken"
	if err := p.dryRun(context.Background(), fresh); err == nil {
		t.Fatal("a dry run whose board read fails passed")
	}
	if _, err := os.Stat(fresh); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed dry run left its table file: %v", err)
	}
}

// A row whose issue a human retitled since the card was filed is left,
// as the intake takes no title edit and only the issue holds it.
func TestARowWhoseIssueAHumanRetitledIsLeft(t *testing.T) {
	s := newStore()
	s.tasks["t-11"] = factoryd.Task{ID: "t-11", IssueNumber: 11, Status: "queued", Repository: "sf/app", Kind: "agent", Objective: "Skipped rows: count them\n\nText."}
	f := &fakeForge{issues: map[int64][2]string{11: {"Count the skipped rows, says Sebastian", "Text."}}}
	p := newPass(s, f, nil)
	err := p.apply(context.Background(), table{Cards: []cardRow{{Task: "t-11", Card: "sf/app#11", OldTitle: "Skipped rows: count them",
		Title: "Count the rows the import skips", Why: "So the import report says what it dropped.", Summary: summary17}}})
	if line := `- sf/app#11: its issue was retitled "Count the skipped rows, says Sebastian" since the card was filed`; !strings.Contains(p.out.(*bytes.Buffer).String(), line) {
		t.Errorf("the run's output lacks %q:\n%s", line, p.out)
	}
	if err == nil || len(s.writes) > 0 || len(f.edits) > 0 {
		t.Errorf("the run = %v, writes %q, edits %q: want the row left", err, s.writes, f.edits)
	}
}

// A card of a project whose judgment budget has no task this month gets
// no brief, and no call is made.
func TestACardWithNoJudgmentBudgetIsListed(t *testing.T) {
	s := newStore()
	delete(s.tasks, "t-chat-app-2026-11")
	s.tasks["t-17"] = factoryd.Task{ID: "t-17", IssueNumber: 17, Status: "queued", Repository: "sf/app", Kind: "agent", Objective: "Skipped rows: count them\n\nText."}
	calls := modelGateway(t, briefAnswers)
	p := newPass(s, &fakeForge{issues: map[int64][2]string{}}, calls)
	drafted, err := p.draft(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	line := "- sf/app#17 gets no new brief: project app has no judgment budget this month yet, as no chat task t-chat-app-2026-11 exists"
	if len(drafted.Cards) > 0 || calls.count() > 0 || !strings.Contains(p.out.(*bytes.Buffer).String(), line) {
		t.Errorf("cards %+v, %d calls, output:\n%s\nwant no card, no call and %q", drafted.Cards, calls.count(), p.out, line)
	}
}

// Every Forgejo call of the pass carries its token, the door's reference
// reads among them, and never the bot's login.
func TestEveryForgejoCallCarriesTheToken(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		status := http.StatusOK
		if r.Method == http.MethodPatch {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"title": "T", "body": "B", "state": "open"}`))
	}))
	defer srv.Close()
	f, forge := forgejoOf(srv.URL+"/", "factory-agent", "tok")
	refs, err := factoryd.BotRefs(context.Background(), f, version().Tree, "sf/app")
	if err != nil {
		t.Fatal(err)
	}
	refs.Issue(5)
	if _, _, err := forge.Issue(context.Background(), "sf/app", 5); err != nil {
		t.Fatal(err)
	}
	if err := forge.EditIssue(context.Background(), "sf/app", 5, "T", "B"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/v1/repos/sf/app/issues/5 token tok", "GET /api/v1/repos/sf/app/issues/5 token tok", "PATCH /api/v1/repos/sf/app/issues/5 token tok"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("calls %q, want %q", seen, want)
	}
}

// The real run, which opens the store as factoryd does and so applies the
// checkout's migrations, names each one the database lacks.
func TestARunNamesTheMigrationsTheDatabaseLacks(t *testing.T) {
	root := fstest.MapFS{"factoryd/a.sql": {}, "factoryd/b.sql": {}, "gateway/c.sql": {}, "README": {}}
	left, err := unapplied(map[string]bool{"factoryd/a.sql": true, "gateway/c.sql": true}, root)
	if err != nil || !slices.Equal(left, []string{"factoryd/b.sql"}) {
		t.Errorf("unapplied = %q, %v; want factoryd/b.sql", left, err)
	}
}

const (
	summary11 = "The console refuses to close a plan while criteria are open. This card limits the refusal to plans that hold criteria. A test closes a plan with none and one with two."
	why16     = "The push rule keeps agents out of workflow files, so a human makes these two edits."
	summary17 = "The import counts the rows it skips and prints the count. A test imports a file with two bad rows."
	ask23     = "Look at the error rate of the export"
	why23     = "The alert fired on the export and may point at a fault there."
	why22     = "The alert fires on the app and may point at a fault there."
	why25     = "The alert fires on the export and may point at a fault there."
	// face16 is #16's question as its plan files one today
	face16 = `ask=Change two lines in the runner's build files check=The factory does not check this. The card closes, and the plan goes on. ` +
		`offers=[{"value":"done","label":"I did it"},{"value":"hand_back","label":"I cannot do this"},{"value":"park","label":"Not now"}] plan=t-plan ` +
		`waits=The plan waits for it. why=The factory's push rule keeps agents out of workflow files, so a human does this card, and plan #10 waits for it`
	// face21 is #21's stored question with today's value of I did it
	face21 = `ask=Repair the red chain of the app check=<nil> ` +
		`offers=[{"value":"done","label":"I did it"},{"value":"park","label":"Not now"}] plan=<nil> waits=<nil> why=The merge queue waits until the chain is green.`
	// face25 is the same of the export's alert
	face25 = `ask=Look at the 5xx rate of the export check=<nil> ` +
		`offers=[{"value":"noted","label":"Noted"},{"value":"park","label":"Not now"}] plan=<nil> waits=<nil> why=` + why25
	// face22 is the alert card's stored question with today's value of Noted
	face22 = `ask=Look at the 5xx rate of the app check=<nil> ` +
		`offers=[{"value":"noted","label":"Noted"},{"value":"park","label":"Not now"}] plan=<nil> waits=<nil> why=` + why22
)

var now = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func version() policy.Version {
	return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{"app": {Repository: "sf/app"}},
		Document: policy.Document{Limits: policy.Limits{SupervisorTimeout: time.Minute}}}}
}

func newPass(s *fakeStore, f *fakeForge, calls *calls) *pass {
	p := &pass{store: s, forge: f, refs: allRefs, policy: version(), out: &bytes.Buffer{}, now: func() time.Time { return now }}
	if calls != nil {
		p.model = &factoryd.SupervisorModel{GatewayURL: calls.url, Token: "tok", Budget: factoryd.TokenBudget{AnswerTokens: 2048, HeadroomTokens: 16384}}
	}
	return p
}

// newStore holds the project's judgment budget this month and nothing
// else.
func newStore() *fakeStore {
	return &fakeStore{tasks: map[string]factoryd.Task{"t-chat-app-2026-11": {ID: "t-chat-app-2026-11", Kind: factoryd.KindChat, Status: "running", Repository: "sf/app", BudgetEUR: 5}},
		events: map[string][]factoryd.Event{}, asks: map[int64]factoryd.CaseAsk{}, revisions: map[string][]factoryd.PlanRevision{}, confirmed: map[string]int{}}
}

const (
	plan10r1 = `{"spec": {"outcome": "A plan without criteria closes without a waiver."}, "summary": "Narrow the close refusal.",
	"criteria": [{"id": "closes_empty", "kind": "test", "claim": "closing a plan that holds no criteria needs no waiver", "suite": "internal/console", "name": "TestCloseEmpty"},
		{"id": "stays_green", "kind": "invariant", "suite": "./..."}],
	"cards": [{"key": "narrow", "title": "The doClose refusal narrowed to plans that hold criteria"}, {"key": "hand", "title": "The two reproducible manifests name the proofs trees"}]}`
	plan10r2 = `{"spec": {"outcome": "A plan without criteria closes without a waiver."}, "summary": "Narrow the close refusal.",
	"criteria": [{"id": "closes_empty", "kind": "test", "claim": "closing a plan that holds no criteria needs no waiver", "suite": "internal/console", "name": "TestCloseEmpty"},
		{"id": "stays_green", "kind": "invariant", "suite": "./...", "promise": "The tests stay green."}],
	"cards": [{"key": "narrow", "title": "The doClose refusal narrowed to plans that hold criteria"}, {"key": "hand", "title": "The two reproducible manifests name the proofs trees"}]}`
)

// liveBoard is the live board the test reads: a plan whose name is a slug,
// confirmed at revision 1 and filed again at revision 2, with one
// criterion that holds a promise only at 2 and one that holds none, and
// cards that each meet one rule.
func liveBoard() *fakeStore {
	card := func(issue int64, status, objective string) factoryd.Task {
		return factoryd.Task{ID: fmt.Sprintf("t-%d", issue), IssueNumber: issue, Status: status, Repository: "sf/app", Kind: "agent", Objective: objective}
	}
	s := newStore()
	s.revisions["t-plan"] = []factoryd.PlanRevision{{TaskID: "t-plan", Revision: 1, Plan: json.RawMessage(plan10r1)}, {TaskID: "t-plan", Revision: 2, Plan: json.RawMessage(plan10r2)}}
	s.confirmed["t-plan"] = 1
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
	c18 := card(18, "queued", "Gateway: retry loop\n\nThe retry loses the request id. It logs Authorization: Bearer abc123secretvalue.")
	c18.BudgetEUR, c18.SpentEUR = 2, 2
	add(c18)
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
	c22 := card(22, "waiting-human", "Look at the 5xx rate of the app\n\nThe rate is 7 %.")
	c22.Kind, c22.Source, c22.Why = "human", factoryd.SourceAlert, why22
	add(c22)
	c23 := card(23, "waiting-human", ask23+"\n\nThe rate is 4 %.")
	c23.Kind, c23.Source, c23.Why = "human", factoryd.SourceAlert, why23
	add(c23)
	oldAlert := []factoryd.QuestionChoice{{Value: "close", Label: "Noted"}, {Value: "park", Label: "Not now"}}
	s.events["t-23"] = []factoryd.Event{{TaskID: "t-23", Kind: factoryd.EventQuestionAsked, Detail: map[string]any{
		"ask": ask23, "question": ask23, "why": why23, "offers": oldAlert, "choices": []string{"Noted", "Not now"}, "source": factoryd.SourceAlert, "by": "factoryd",
		"resolved_at": "2026-11-17T10:00:00Z", "case_id": 90, "story": map[string]any{"planned": "It fired."},
		"preselected": map[string]any{"action": "choice:close"}, "recommended_by": "the factory", "recommendation": "The alert no longer fires."}}}
	c25 := card(25, "waiting-human", "Look at the 5xx rate of the export\n\nThe rate is 6 %.")
	c25.Kind, c25.Source, c25.Why = "human", factoryd.SourceAlert, why25
	add(c25)
	c26 := card(26, "waiting-human", "Change the base image of the runner\n\nEdit Dockerfile.runner.")
	c26.Kind, c26.Source, c26.Parent, c26.Why = "human", factoryd.SourcePlan, "t-plan", "The push rule keeps agents out of workflow files, so a human makes this edit."
	add(c26)
	c24 := card(24, "waiting-poster", "Find the cause of the import stall\n\nIt stops at row 9,000.")
	c24.Why, c24.Summary = "So the nightly import finishes again.", "The import stops at row 9,000. The card finds why. A test imports 10,000 rows."
	add(c24)

	old := []factoryd.QuestionChoice{{Value: "close", Label: "I did it"}, {Value: "park", Label: "Not now"}}
	s.seat(factoryd.Case{ID: 61, TaskID: "t-16", Repository: "sf/app", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "The two reproducible manifests name the proofs trees", "why": "The case is the human card.", "offers": old, "source": factoryd.SourcePlan, "by": "factoryd"}},
		map[string]any{"preselected": map[string]any{"action": "close", "args": nil, "fund": nil}, "recommended_by": "the factory agent", "recommendation": "The two edits are on main."})
	s.seat(factoryd.Case{ID: 62, TaskID: "t-21", Repository: "sf/app", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "Repair the red chain of the app", "why": c21.Why, "offers": old, "source": factoryd.SourceChainRepair, "by": "factoryd"}},
		map[string]any{"preselected": map[string]any{"action": "choice:close"}, "recommended_by": "the factory agent", "recommendation": "The chain is green again."})
	// the alert's face was stored on its ask row again: offers with their
	// effect lines, and the factory's own pick
	s.seat(factoryd.Case{ID: 63, TaskID: "t-22", Repository: "sf/app", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "Look at the 5xx rate of the app", "why": why22, "offers": oldAlert, "source": factoryd.SourceAlert, "by": "factoryd"}},
		map[string]any{"story": map[string]any{"planned": "It fires."}, "offers": []factoryd.AskOffer{{Value: "choice:close", Label: "Noted", Effect: "Ends this notice."},
			{Value: "choice:park", Label: "Not now", Effect: "The card keeps waiting for your answer."}},
			"preselected": map[string]any{"action": "choice:close"}, "recommended_by": "the factory", "recommendation": "The alert no longer fires."})
	// the same, with rung 2's pick on Not now, which the face leads with
	s.seat(factoryd.Case{ID: 64, TaskID: "t-25", Repository: "sf/app", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "Look at the 5xx rate of the export", "why": why25, "offers": oldAlert, "source": factoryd.SourceAlert, "by": "factoryd"}},
		map[string]any{"story": map[string]any{"planned": "It fires."}, "offers": []factoryd.AskOffer{{Value: "choice:close", Label: "Noted", Effect: "Ends this notice."},
			{Value: "choice:park", Label: "Not now", Effect: "The card keeps waiting for your answer."}},
			"preselected": map[string]any{"action": "park", "args": nil, "fund": nil}, "recommended_by": "the factory agent", "recommendation": "The rate falls by itself after a deploy."})
	// a month's cap a plan's human card asks keeps its own question
	s.seat(factoryd.Case{ID: 65, TaskID: "t-26", Repository: "sf/app", Kind: factoryd.CaseMonthCap, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"ask": "The factory's spending reached its limit for the month. Raise it?", "why": "Every card waits until the limit rises or the month turns.",
			"offers": []factoryd.QuestionChoice{{Value: "noted", Label: "Noted"}, {Value: "park", Label: "Not now"}}, "source": factoryd.SourceMonthCap, "by": "factoryd"}}, nil)
	s.seat(factoryd.Case{ID: 80, TaskID: "t-20", Repository: "sf/app", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman,
		Brief: "Press Not now to wait for the export.", BriefDetail: "The export of the large board is left.",
		Facts: map[string]any{"ask": "The export stopped. Try again?", "story": map[string]any{"planned": "This card builds it."}}}, nil)
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

// input is a model call's message as the fake gateway reads it.
type input struct {
	Raw         string
	Card        struct{ Title string }
	LastRefusal []string `json:"last_refusal"`
}

// briefAnswers answers the brief, name and promise calls of the live
// board as the small model would.
func briefAnswers(system string, in input) any {
	switch {
	case strings.HasPrefix(system, "You name build plans"):
		return map[string]string{"name": "Close refusal fix"}
	case strings.HasPrefix(system, "You write the promise"):
		return map[string]any{"promises": map[string]string{"closes_empty": "A plan with no criteria closes without a waiver."}}
	case in.Card.Title == "The doClose refusal narrowed to plans that hold criteria":
		return brief("Narrow the close refusal to plans that hold criteria", "So a plan with no criteria closes without a waiver.", summary11)
	case in.Card.Title == "The two reproducible manifests name the proofs trees":
		return brief("Change two lines in the runner's build files", why16, "")
	case in.Card.Title == "Skipped rows: count them" && len(in.LastRefusal) == 0:
		return brief("The skipped rows", "So the import report says what it dropped.", summary17)
	case in.Card.Title == "Skipped rows: count them":
		return brief("Count the rows the import skips", "So the import report says what it dropped.", summary17)
	case in.Card.Title == "Gateway: retry loop":
		return brief("Gateway: fix the retry loop", "So a retried call keeps its request id.",
			"The gateway keeps the request id when it retries a call. A test retries a call twice with the key sk-livesecret99x.")
	}
	return nil
}

// calls is the fake gateway: it answers each call with answer, checks
// that each runs on the small model for title on its account, and keeps
// each message.
type calls struct {
	url  string
	mu   sync.Mutex
	msgs []string
}

func (c *calls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

func (c *calls) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.msgs)
}

func modelGateway(t *testing.T, answer func(system string, in input) any) *calls {
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
		system := body.Messages[0].Content
		account := "t-chat-app-2026-11"
		if strings.HasPrefix(system, "You name build plans") {
			account = "" // the namer books on the plan, as production names it
		}
		if task := r.Header.Get(gateway.HeaderFactoryTask); body.Model != policy.Small || r.Header.Get(gateway.HeaderFactoryPurpose) != policy.PurposeTitle ||
			account != "" && task != account || account == "" && !strings.HasPrefix(task, "t-") {
			t.Errorf("a call on %q for %q of %q, want the small model for title on %q", body.Model, r.Header.Get(gateway.HeaderFactoryPurpose), task, account)
		}
		in := input{Raw: strings.TrimPrefix(body.Messages[1].Content, factoryd.ChatInputLead)}
		_ = json.Unmarshal([]byte(in.Raw), &in)
		got := answer(system, in)
		if got == nil {
			t.Errorf("a call the test does not answer: %s", in.Raw)
		}
		c.mu.Lock()
		c.msgs = append(c.msgs, in.Raw)
		c.mu.Unlock()
		content, _ := json.Marshal(got)
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
	broken    string // a repository whose board read fails
}

// seat opens case c with its ask row, which names it, holds its facts
// and row's keys beside them.
func (s *fakeStore) seat(c factoryd.Case, row map[string]any) {
	s.cases = append(s.cases, c)
	detail := maps.Clone(c.Facts)
	maps.Copy(detail, row)
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
	if repository == s.broken {
		return nil, errors.New("the board read failed")
	}
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
		s.seat(factoryd.Case{ID: int64(100 + len(s.cases)), TaskID: e.TaskID, Repository: e.Repository, Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Facts: maps.Clone(e.Detail)}, nil)
	}
	return int64(len(s.writes)), nil
}

// SetAskFace writes the card, the live case's facts and the face on its
// ask row, the row's pick dropped for the face's, as the store's one
// transaction does.
func (s *fakeStore) SetAskFace(_ context.Context, t factoryd.Task, id int64, facts, face map[string]any) error {
	i := slices.IndexFunc(s.cases, func(c factoryd.Case) bool { return c.ID == id })
	s.tasks[t.ID] = t
	maps.Copy(s.cases[i].Facts, facts)
	row := s.face(id, face)
	_, story := row["story"]
	var offers []string
	for _, o := range factoryd.AskOffers(row) {
		offers = append(offers, o.Value+":"+o.Label)
	}
	pre, _ := row["preselected"].(map[string]any)
	s.writes = append(s.writes, fmt.Sprintf("ask face %d %s | row offers=%s pick=%v by=%v story=%v", id, faceWords(facts), strings.Join(offers, ","), pre["action"], row["recommended_by"], story))
	return nil
}

func (s *fakeStore) SetCaseFace(_ context.Context, id int64, keys map[string]any) error {
	s.face(id, keys)
	s.writes = append(s.writes, fmt.Sprintf("case face %d %s", id, faceWords(keys)))
	return nil
}

// face stores keys on case id's ask row, the row's pick dropped first.
func (s *fakeStore) face(id int64, keys map[string]any) map[string]any {
	row := s.asks[id].Detail
	for _, k := range []string{"preselected", "recommended_by", "recommendation"} {
		delete(row, k)
	}
	maps.Copy(row, keys)
	return row
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
