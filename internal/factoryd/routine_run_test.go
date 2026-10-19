package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// codeIssues is the code repository's issue API as a routine's card uses
// it: the open issues, a new issue (numbered from 71), its labels.
type codeIssues struct {
	mu     sync.Mutex
	issues []string // "title\n\nbody", the first numbered 71
	closed []int64
	labels map[int64][]string
	// older are open issues the bot filed before, by number: their titles
	older map[int64]string
	// holdOne, when set, parks the next issue POST until the test closes
	// it; parked says it has parked: the go race test's slow hand.
	holdOne chan struct{}
	parked  chan struct{}
}

func (c *codeIssues) srv(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/ai-software-factory-code/")
		switch {
		case r.Method == http.MethodGet && path == "issues":
			open := []map[string]any{}
			for n, title := range c.older {
				if r.URL.Query().Get("page") == "1" {
					open = append(open, map[string]any{"number": n, "title": title, "user": map[string]string{"login": "factory-agent"}})
				}
			}
			for i, is := range c.issues {
				if n := int64(71 + i); r.URL.Query().Get("page") == "1" && !slices.Contains(c.closed, n) {
					title, _, _ := strings.Cut(is, "\n\n")
					open = append(open, map[string]any{"number": n, "title": title, "user": map[string]string{"login": "factory-agent"}})
				}
			}
			json.NewEncoder(w).Encode(open) // #nosec G104 -- test stub
		case r.Method == http.MethodPost && path == "issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			if c.holdOne != nil {
				hold, parked := c.holdOne, c.parked
				c.holdOne = nil
				c.mu.Unlock()
				close(parked)
				<-hold
				c.mu.Lock()
			}
			c.issues = append(c.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 70 + len(c.issues)}) // #nosec G104 -- test stub
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/labels"):
			var in struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "issues/"), "/labels"), 10, 64)
			c.labels[n] = in.Labels
			json.NewEncoder(w).Encode([]any{}) // #nosec G104 -- test stub
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// codeProject is the fixture tree's code project, which holds the weekly
// bump and the audit.
const codeProject = "ai-software-factory-code"

// routineRunner is a runner over tree with an empty store, the code
// project in the maintenance process, the code repository's issues, a
// recorder of the workflows it starts and of the events it records.
func routineRunner(t *testing.T, tree policy.Tree) (*Runner, *fakeStore, *codeIssues, *fakeWorkflows) {
	t.Helper()
	issues := &codeIssues{labels: map[int64][]string{}}
	store, wf := newFakeStore(), &fakeWorkflows{}
	store.projects = []Project{{Name: codeProject, Repository: "sf/ai-software-factory-code", File: true, Objects: policy.ObjectsSeed,
		Process: "maintenance", Lanes: maintenanceLanes}}
	v := policy.Version{Tree: tree}
	r := &Runner{store: store, forgejo: issues.srv(t), timeline: &Timeline{Objects: &fakeObjectStore{}, Events: &fakeEventRecorder{}, Store: store},
		cfg: RunnerConfig{Policy: func() policy.Version { return v }, Workflows: wf}, Routines: store}
	return r, store, issues, wf
}

// fakePoke is one poke the Poker writes: m for logins on repository.
type fakePoke struct {
	repository string
	logins     []string
	m          ChatMessage
}

// routinePokes are the pokes the Poker writes for the routine_ran events
// r recorded, as Poker.Pass does: their detail read back as the events
// table gives it, to the project's notify list, else its approvers.
func routinePokes(t *testing.T, r *Runner, store *fakeStore) []fakePoke {
	t.Helper()
	var out []fakePoke
	for _, e := range recordedEvents(r, EventRoutineRan) {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			t.Fatal(err)
		}
		e.Detail = nil
		if err := json.Unmarshal(b, &e.Detail); err != nil {
			t.Fatal(err)
		}
		if m, ok := pokeOf(e, store.tasks[e.TaskID]); ok {
			out = append(out, fakePoke{repository: e.Repository, logins: pokeTo(r.cfg.Policy().Tree, e.Repository), m: m})
		}
	}
	return out
}

// recordedEvents are the events of kind r recorded, oldest first.
func recordedEvents(r *Runner, kind string) []Event {
	rec := r.timeline.Events.(*fakeEventRecorder)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []Event
	for _, e := range rec.rows {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// onlyTask is the store's one task.
func onlyTask(t *testing.T, store *fakeStore) Task {
	t.Helper()
	if len(store.tasks) != 1 {
		t.Fatalf("tasks %+v, want one", store.tasks)
	}
	for _, card := range store.tasks {
		return card
	}
	return Task{}
}

// The weekly bump at its due: a human card on the code repository,
// labelled routine, the brief's text as its body with the run's
// provenance, waiting on a person, its card_created recorded. The run
// stays open with its card, its routine_ran event names the card, and the
// inbox of the project's approvers hears of it once through that event,
// with the routine, the run and the schedule as the decider line. A human
// card pokes under tell quiet too. Hand-derived: the first issue is #71,
// the fixture's approver sebastian.
func TestRoutineRunFilesAHumanCard(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	onTime := time.Now().Truncate(time.Minute)
	res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues.issues) != 1 {
		t.Fatalf("issues %d, want 1", len(issues.issues))
	}
	card := onlyTask(t, store)
	if card.Kind != "human" || card.Status != "waiting-human" || card.Source != SourceRoutine || card.IssueNumber != 71 || card.RiskClass != "standard" ||
		card.Repository != "sf/ai-software-factory-code" || card.Author != "factory-agent" {
		t.Fatalf("card %+v", card)
	}
	if want := (RoutineRunResult{Outcome: "filed", TaskID: card.ID, Issue: 71}); res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"human", "routine", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	if created := recordedEvents(r, EventCardCreated); len(created) != 1 || created[0].TaskID != card.ID ||
		!reflect.DeepEqual(created[0].Detail, map[string]any{"by": "factory-agent", "issue": int64(71)}) {
		t.Errorf("card_created %+v, want the card's one", created)
	}
	if ran := recordedEvents(r, EventRoutineRan); len(ran) != 1 || ran[0].Repository != "sf/ai-software-factory-code" || ran[0].TaskID != card.ID ||
		ran[0].IssueNumber != 71 || ran[0].Actor != FactoryDecider || ran[0].Via != ViaFactoryd ||
		!reflect.DeepEqual(ran[0].Detail, map[string]any{"project": codeProject, "routine": "weekly-bump", "run": 1, "outcome": "filed",
			"schedule": "every Monday 06:00", "poke": "#71 needs you: Weekly bump of the pinned outside inputs."}) {
		t.Errorf("routine_ran %+v", ran)
	}
	title, body, _ := strings.Cut(issues.issues[0], "\n\n")
	if title != "Weekly bump of the pinned outside inputs" || card.Objective != title+"\n\n"+body {
		t.Errorf("title %q, objective %q", title, card.Objective)
	}
	if !strings.HasPrefix(body, "The factory pins every outside input of its builds and its cluster") ||
		!strings.HasSuffix(body, "since a pod whose copy is missing does not start.\n\nfiled by the factory · routine weekly-bump · run 1") {
		t.Errorf("body:\n%s", body)
	}
	for _, want := range []string{"upstream-images.txt", "DEBIAN_SNAPSHOT", "POSTGRESQL_VERSION", "LIBPQ_VERSION", "POSTGRESQL_COMMON_VERSION",
		"apt-archive.postgresql.org", "go.mod's `toolchain`", "ADD --checksum", "buildkitSHA256", "upstream-files.txt", "scripts/upstream-images-test/test.sh",
		"chart", "index.yaml", "ts-go template", "Talos", "Cilium"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card names no %s:\n%s", want, body)
		}
	}
	runs := store.routineRuns[codeProject+"/weekly-bump"]
	if len(runs) != 1 || runs[0].Run != 1 || runs[0].Trigger != "cron" || runs[0].Outcome != "filed" || runs[0].TaskID != card.ID ||
		runs[0].Issue != 71 || !runs[0].Ended.IsZero() {
		t.Fatalf("runs %+v, want run 1 filed and open", runs)
	}
	want := fakePoke{repository: "sf/ai-software-factory-code", logins: []string{"sebastian"}, m: ChatMessage{Role: "assistant", Kind: MessagePoke,
		Content: "#71 needs you: Weekly bump of the pinned outside inputs.", TaskID: card.ID, Decider: "routine weekly-bump · run 1 · every Monday 06:00", BirthKind: EventRoutineRan}}
	if pokes := routinePokes(t, r, store); !reflect.DeepEqual(pokes, []fakePoke{want}) {
		t.Fatalf("pokes %+v,\nwant %+v", pokes, want)
	}

	bump, _ := routineTree(t).Routine(codeProject, "weekly-bump")
	bump.Tell = policy.TellQuiet
	r, store, _, _ = routineRunner(t, withRoutines(routineTree(t), codeProject, bump))
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime}); err != nil {
		t.Fatal(err)
	}
	if pokes := routinePokes(t, r, store); len(pokes) != 1 || pokes[0].m.Content != "#71 needs you: Weekly bump of the pinned outside inputs." {
		t.Fatalf("pokes under tell quiet %+v, want the human card's one", pokes)
	}
}

// The go action on the weekly bump's waiting card: the factory files the
// work as an agent card in the shape every agent routine's card takes,
// its objective the card's own words with who handed it over named (the
// bump names no skill, so no skill line), and the run's row points at
// the new card: the hold keeps holding and the run ends with the new
// card, not the waiting one, which stays as it was for the close that
// follows. A second go is refused: the run no longer names the card.
// Hand-derived: the run's card is #71, the hand-over #72, the budget the
// fixture's default 2.
func TestGoHandsTheCardOver(t *testing.T) {
	ctx := context.Background()
	r, store, issues, wf := routineRunner(t, routineTree(t))
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now().Truncate(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	waiting := onlyTask(t, store)
	issue, err := r.Go(ctx, waiting.ID, "sebastian")
	if err != nil || issue != 72 {
		t.Fatalf("Go = %d, %v; want #72", issue, err)
	}
	if len(store.tasks) != 2 {
		t.Fatalf("%d tasks, want the waiting card and its hand-over", len(store.tasks))
	}
	if again := store.tasks[waiting.ID]; again.Status != "waiting-human" || again.Kind != "human" {
		t.Fatalf("the waiting card = %+v, want it as it was for the close", again)
	}
	var n Task
	for id, c := range store.tasks {
		if id != waiting.ID {
			n = c
		}
	}
	if n.Kind != "agent" || n.Status != "queued" || n.Lane != policy.LaneReady || n.Process != "maintenance" || n.Source != SourceRoutine ||
		n.RiskClass != "standard" || n.BudgetEUR != 2 || n.IssueNumber != 72 || n.Author != "" ||
		n.Repository != "sf/ai-software-factory-code" || !slices.Equal(n.RequiredCapabilities, []string{"coder"}) {
		t.Fatalf("the hand-over = %+v", n)
	}
	if want := waiting.Objective + "\n\nhanded over by sebastian"; n.Objective != want {
		t.Fatalf("objective %q, want %q", n.Objective, want)
	}
	if len(issues.issues) != 2 || issues.issues[1] != n.Objective {
		t.Fatalf("issues %q, want the hand-over's issue carrying the objective", issues.issues)
	}
	if got := issues.labels[72]; !slices.Equal(got, []string{"routine", "state/queued"}) {
		t.Errorf("labels = %v", got)
	}
	if !slices.Equal(wf.started, []string{n.ID}) {
		t.Errorf("started %v, want the hand-over's run", wf.started)
	}
	runs := store.routineRuns[codeProject+"/weekly-bump"]
	if len(runs) != 1 || runs[0].TaskID != n.ID || runs[0].Issue != 72 || runs[0].Outcome != "filed" || !runs[0].Ended.IsZero() {
		t.Fatalf("runs %+v, want the open run on the hand-over", runs)
	}
	if _, err := r.Go(ctx, waiting.ID, "sebastian"); err == nil || err.Error() != "Card #71 was already handed over." {
		t.Fatalf("the second go: %v, want the refusal", err)
	}
	if len(store.tasks) != 2 || len(issues.issues) != 2 {
		t.Fatalf("the second go acted: %d tasks, %d issues", len(store.tasks), len(issues.issues))
	}
}

// Two hands race the go of one waiting card: the filing is slow, the
// second hand arrives while the first is still filing, and the hand-over
// claim refuses it, so the agent card is filed at most once. Hand-derived:
// the run's card is #71, the hand-over #72.
func TestGoFilesOnceWhenTwoHandsRace(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now().Truncate(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	waiting := onlyTask(t, store)
	issues.holdOne, issues.parked = make(chan struct{}), make(chan struct{})
	hold := issues.holdOne // the parking handler nils the field, so keep the channel here
	first := make(chan error, 1)
	go func() {
		_, err := r.Go(ctx, waiting.ID, "sebastian")
		first <- err
	}()
	<-issues.parked
	_, racing := r.Go(ctx, waiting.ID, "sebastian")
	close(hold)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if racing == nil || racing.Error() != "Card #71 was already handed over." {
		t.Fatalf("the racing go: %v, want the refusal", racing)
	}
	if len(issues.issues) != 2 || len(store.tasks) != 2 {
		t.Fatalf("%d issues, %d tasks: the racing go filed a second hand-over", len(issues.issues), len(store.tasks))
	}
}

// A single-paragraph card hands over without stray blank lines: the
// hand-over line joins the card's own words directly, and a plain
// single-paragraph card keeps its objective exactly.
func TestGoJoinsTheHandOverWithoutBlankLines(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now().Truncate(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	waiting := onlyTask(t, store)
	waiting.Objective = "Bump the dependencies"
	store.tasks[waiting.ID] = waiting
	if _, err := r.Go(ctx, waiting.ID, "sebastian"); err != nil {
		t.Fatal(err)
	}
	var n Task
	for id, c := range store.tasks {
		if id != waiting.ID {
			n = c
		}
	}
	if want := "Bump the dependencies\n\nhanded over by sebastian"; n.Objective != want {
		t.Fatalf("objective %q, want %q", n.Objective, want)
	}
	if issues.issues[1] != n.Objective {
		t.Fatalf("the issue %q, want the objective", issues.issues[1])
	}
	if err := store.CreateTask(ctx, Task{ID: "t-one", Kind: "human", Status: "backlog", Repository: "sf/ai-software-factory-code",
		IssueNumber: 9, Source: "webhook", RiskClass: "standard", BudgetEUR: 1, Objective: "Tidy the docs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Go(ctx, "t-one", "sebastian"); err != nil {
		t.Fatal(err)
	}
	var plain Task
	for id, c := range store.tasks {
		if id != waiting.ID && id != "t-one" && id != n.ID {
			plain = c
		}
	}
	if plain.Objective != "Tidy the docs" {
		t.Fatalf("objective %q, want the card's own words without trailing blank lines", plain.Objective)
	}
}

// Go refuses a card that is no human card waiting on a person and files
// nothing; a human card in the backlog goes too, on its own words, with
// no routine's label.
func TestGoRefusedAndBacklog(t *testing.T) {
	ctx := context.Background()
	r, store, issues, wf := routineRunner(t, routineTree(t))
	if err := store.CreateTask(ctx, Task{ID: "t-agent", Kind: "agent", Status: "queued", Lane: policy.LaneReady, Process: "maintenance",
		Repository: "sf/ai-software-factory-code", IssueNumber: 5, Source: SourceRoutine, RiskClass: "standard", BudgetEUR: 2,
		Objective: "Audit\n\nRun the skill audit."}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, Task{ID: "t-back", Kind: "human", Status: "backlog", Repository: "sf/ai-software-factory-code",
		IssueNumber: 6, Source: "webhook", RiskClass: "standard", Objective: "Tidy the docs\n\nTidy the console's docs."}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Go(ctx, "t-agent", "sebastian"); err == nil || err.Error() != "Card #5 is an agent card: the factory runs it." {
		t.Fatalf("the go of an agent card: %v, want the refusal", err)
	}
	issue, err := r.Go(ctx, "t-back", "sebastian")
	if err != nil || issue != 71 {
		t.Fatalf("the go of the backlog card = %d, %v; want #71", issue, err)
	}
	if len(store.tasks) != 3 {
		t.Fatalf("%d tasks, want the two cards and the hand-over", len(store.tasks))
	}
	var n Task
	for id, c := range store.tasks {
		if id != "t-agent" && id != "t-back" {
			n = c
		}
	}
	if n.Objective != "Tidy the docs\n\nTidy the console's docs." || n.Source != "webhook" || n.Kind != "agent" || n.Status != "queued" {
		t.Fatalf("the hand-over = %+v", n)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"state/queued"}) {
		t.Errorf("labels = %v, want none but the state's", got)
	}
	if len(issues.issues) != 1 || issues.issues[0] != n.Objective {
		t.Fatalf("issues %q, want the hand-over alone", issues.issues)
	}
	if len(wf.started) != 1 || wf.started[0] == "t-back" {
		t.Fatalf("started %v", wf.started)
	}
}

// While the last run's card #781 is open, a routine that holds files
// nothing: the run ends held, names the card in its note and keeps no
// task of its own, so the card's end finds the run that filed it. A held
// run is board material and pokes nobody, under tell inbox too (rule 7).
// File files a second card beside it: a new issue, since #781, the bot's
// open issue of the same title, is the open card's. Once #781 ends, the
// next run files.
func TestRoutineRunHeldWhileOpen(t *testing.T) {
	ctx := context.Background()
	onTime := time.Now().Truncate(time.Minute)
	for _, tt := range []struct {
		name, whileOpen, tell string
		want                  RoutineRunResult
		poke                  string
	}{
		{"hold", policy.WhileOpenHold, policy.TellInbox, RoutineRunResult{Outcome: "held", Note: "held: #781 is still open"}, ""},
		{"hold quietly", policy.WhileOpenHold, policy.TellQuiet, RoutineRunResult{Outcome: "held", Note: "held: #781 is still open"}, ""},
		{"file", policy.WhileOpenFile, policy.TellQuiet, RoutineRunResult{Outcome: "filed", Issue: 71}, "#71 needs you: Weekly bump of the pinned outside inputs."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bump, _ := routineTree(t).Routine(codeProject, "weekly-bump")
			bump.WhileOpen, bump.Tell = tt.whileOpen, tt.tell
			r, store, issues, _ := routineRunner(t, withRoutines(routineTree(t), codeProject, bump))
			issues.older = map[int64]string{781: "Weekly bump of the pinned outside inputs"}
			store.tasks["t-781"] = Task{ID: "t-781", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 781, Source: SourceRoutine}
			last, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: onTime.Add(-7 * 24 * time.Hour), Trigger: "cron"}, true)
			if err := store.RoutineRunFiled(ctx, codeProject, "weekly-bump", last.Run, "t-781", 781); err != nil {
				t.Fatal(err)
			}
			res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime})
			if err != nil {
				t.Fatal(err)
			}
			if tt.whileOpen == policy.WhileOpenFile {
				if len(store.tasks) != 2 || len(issues.issues) != 1 || store.tasks[res.TaskID].IssueNumber != 71 {
					t.Fatalf("tasks %+v, issues %q, result %+v: want a second card on a new issue, #71", store.tasks, issues.issues, res)
				}
				tt.want.TaskID = res.TaskID
			} else if len(issues.issues) != 0 || len(store.tasks) != 1 {
				t.Fatalf("issues %q, tasks %+v: want none filed", issues.issues, store.tasks)
			}
			if res != tt.want {
				t.Fatalf("result %+v, want %+v", res, tt.want)
			}
			run := store.routineRuns[codeProject+"/weekly-bump"][1]
			if run.Run != 2 || run.Outcome != tt.want.Outcome || run.Note != tt.want.Note || run.TaskID != tt.want.TaskID || run.Ended.IsZero() != (tt.want.TaskID != "") {
				t.Fatalf("run %+v", run)
			}
			var pokes []fakePoke
			if tt.poke != "" {
				pokes = []fakePoke{{repository: "sf/ai-software-factory-code", logins: []string{"sebastian"}, m: ChatMessage{Role: "assistant", Kind: MessagePoke,
					Content: tt.poke, TaskID: res.TaskID, Decider: "routine weekly-bump · run 2 · every Monday 06:00", BirthKind: EventRoutineRan}}}
			}
			if got := routinePokes(t, r, store); !reflect.DeepEqual(got, pokes) {
				t.Fatalf("pokes %+v,\nwant %+v", got, pokes)
			}
		})
	}

	r, store, issues, _ := routineRunner(t, routineTree(t))
	store.tasks["t-781"] = Task{ID: "t-781", Kind: "human", Status: "done", Repository: "sf/ai-software-factory-code", IssueNumber: 781, Source: SourceRoutine,
		FinishedAt: &onTime}
	last, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: onTime.Add(-7 * 24 * time.Hour), Trigger: "cron"}, true)
	if err := store.RoutineRunFiled(ctx, codeProject, "weekly-bump", last.Run, "t-781", 781); err != nil {
		t.Fatal(err)
	}
	if res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime}); err != nil ||
		res.Outcome != "filed" || res.Issue != 71 || len(issues.issues) != 1 {
		t.Fatalf("after #781 ended: result %+v, %v, issues %d", res, err, len(issues.issues))
	}
}

// A pending Skip once takes the next run: it ends skipped by its member,
// files nothing and pokes nobody, the mark is gone, and the run after it
// files.
func TestRoutineRunSkipsOnce(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	if err := store.SetRoutineSkip(ctx, codeProject, "weekly-bump", true, "sebastian"); err != nil {
		t.Fatal(err)
	}
	onTime := time.Now().Truncate(time.Minute)
	res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RoutineRunResult{Outcome: "skipped", Note: "skipped once by sebastian"}); res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if pokes := routinePokes(t, r, store); len(issues.issues) != 0 || len(store.tasks) != 0 || len(pokes) != 0 {
		t.Fatalf("issues %d, tasks %d, pokes %d: want none", len(issues.issues), len(store.tasks), len(pokes))
	}
	if run := store.routineRuns[codeProject+"/weekly-bump"][0]; run.Outcome != "skipped" || run.Note != "skipped once by sebastian" || run.Ended.IsZero() {
		t.Fatalf("run %+v", run)
	}
	if st, _ := store.RoutineState(ctx, codeProject, "weekly-bump"); st.SkipNext || st.SkipBy != "" {
		t.Fatalf("state %+v: the mark stays", st)
	}
	if res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime.Add(7 * 24 * time.Hour)}); err != nil ||
		res.Outcome != "filed" || len(issues.issues) != 1 {
		t.Fatalf("the run after: %+v, %v, issues %d", res, err, len(issues.issues))
	}
}

// A cron run that begins long after its due, the factory down at the
// time: file-once files it as if on time, skip ends it skipped with the
// time it missed in the factory's zone, Europe/Berlin. A skip routine's
// run on time files.
func TestRoutineRunMissed(t *testing.T) {
	ctx := context.Background()
	due := time.Date(2026, 10, 12, 6, 0, 0, 0, berlin) // Mon 06:00, days before the test runs
	in := RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: due}
	r, store, issues, _ := routineRunner(t, routineTree(t))
	if res, err := r.RoutineRun(ctx, in); err != nil || res.Outcome != "filed" || len(issues.issues) != 1 {
		t.Fatalf("file-once: %+v, %v, issues %d", res, err, len(issues.issues))
	}

	bump, _ := routineTree(t).Routine(codeProject, "weekly-bump")
	bump.Missed = policy.MissedSkip
	r, store, issues, _ = routineRunner(t, withRoutines(routineTree(t), codeProject, bump))
	res, err := r.RoutineRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if want := (RoutineRunResult{Outcome: "skipped", Note: "missed: the factory was down at Mon 06:00"}); res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if run, pokes := store.routineRuns[codeProject+"/weekly-bump"][0], routinePokes(t, r, store); len(issues.issues) != 0 || len(pokes) != 0 || run.Outcome != "skipped" || run.Ended.IsZero() {
		t.Fatalf("issues %d, pokes %d, run %+v", len(issues.issues), len(pokes), run)
	}
	if res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now()}); err != nil ||
		res.Outcome != "filed" || len(issues.issues) != 1 {
		t.Fatalf("on time: %+v, %v, issues %d", res, err, len(issues.issues))
	}
}

// The audit at its twentieth merge: an agent card through the intake
// every agent card the factory files takes, in the project's ready lane,
// queued and started, labelled routine, a label every repository carries.
// Its objective is the routine's title, the skill to run and the brief,
// its budget the policy's default (2 EUR in the fixture), and its issue
// names the run. Tell inbox pokes the filing.
func TestRoutineRunFilesAnAgentCard(t *testing.T) {
	ctx := context.Background()
	tree := routineTree(t)
	audit, _ := tree.Routine(codeProject, "audit")
	audit.Brief = "routines/audit.md"
	tree.Briefs = maps.Clone(tree.Briefs)
	tree.Briefs[audit.Brief] = "Hunt bugs in the code changed since the last audit.\n"
	r, store, issues, wf := routineRunner(t, withRoutines(tree, codeProject, audit))
	res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "audit", Trigger: "merges", Due: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	card := onlyTask(t, store)
	if card.Kind != "agent" || card.Status != "queued" || card.Lane != policy.LaneReady || card.Process != "maintenance" || card.Source != SourceRoutine ||
		card.BudgetEUR != 2 || card.RiskClass != "standard" || card.IssueNumber != 71 || card.Author != "" ||
		card.Objective != "Audit: bug hunt and architecture review\n\nRun the skill audit.\n\nHunt bugs in the code changed since the last audit." {
		t.Fatalf("card %+v", card)
	}
	if want := []string{"Audit: bug hunt and architecture review\n\nRun the skill audit.\n\nHunt bugs in the code changed since the last audit.\n\n" +
		"filed by the factory · routine audit · run 1"}; !slices.Equal(issues.issues, want) {
		t.Fatalf("issues %q, want %q", issues.issues, want)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"routine", "state/queued"}) {
		t.Errorf("labels = %v", got)
	}
	for _, l := range issues.labels[71] {
		if _, ok := BootLabels[l]; !ok {
			t.Errorf("the label %s is no boot label: Forgejo drops it", l)
		}
	}
	if !slices.Equal(wf.started, []string{card.ID}) {
		t.Errorf("started %v, want the card's run", wf.started)
	}
	if want := (RoutineRunResult{Outcome: "filed", TaskID: card.ID, Issue: 71}); res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if run := store.routineRuns[codeProject+"/audit"][0]; run.Outcome != "filed" || run.TaskID != card.ID || !run.Ended.IsZero() {
		t.Fatalf("run %+v", run)
	}
	want := fakePoke{repository: "sf/ai-software-factory-code", logins: []string{"sebastian"}, m: ChatMessage{Role: "assistant", Kind: MessagePoke,
		Content: `Filed #71 "Audit: bug hunt and architecture review".`, TaskID: card.ID, Decider: "routine audit · run 1 · every 20 merges", BirthKind: EventRoutineRan}}
	if pokes := routinePokes(t, r, store); !reflect.DeepEqual(pokes, []fakePoke{want}) {
		t.Fatalf("pokes %+v,\nwant %+v", pokes, want)
	}
}

// The live toggle, read when the run fires: a weekly-bump flipped to run
// on its own files the agent card of a routine with no skill (no skill
// line in its words); an audit flipped to need the member files a
// waiting human card that pokes under any tell; the undo follows the
// config again; a pull-request routine ignores the toggle. Hand-derived:
// both first issues are #71, the budget the fixture's default 2.
func TestRoutineRunNeedsMeToggle(t *testing.T) {
	ctx := context.Background()
	no, yes := false, true
	onTime := time.Now().Truncate(time.Minute)

	r, store, issues, wf := routineRunner(t, routineTree(t))
	if err := store.SetRoutineNeedsMe(ctx, codeProject, "weekly-bump", &no, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime}); err != nil {
		t.Fatal(err)
	}
	card := onlyTask(t, store)
	if card.Kind != "agent" || card.Status != "queued" || card.Lane != policy.LaneReady || card.Source != SourceRoutine || card.BudgetEUR != 2 || card.IssueNumber != 71 || card.Author != "" {
		t.Fatalf("the flipped bump's card %+v", card)
	}
	if obj := card.Objective; !strings.HasPrefix(obj, "Weekly bump of the pinned outside inputs\n\nThe factory pins") ||
		!strings.HasSuffix(obj, "does not start.") || strings.Contains(obj, "Run the skill") {
		t.Errorf("objective:\n%s", obj)
	}
	if body := issues.issues[0]; strings.Contains(body, "Run the skill") || !strings.HasSuffix(body, "filed by the factory · routine weekly-bump · run 1") {
		t.Errorf("the issue:\n%s", body)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"routine", "state/queued"}) {
		t.Errorf("labels = %v", got)
	}
	if !slices.Equal(wf.started, []string{card.ID}) {
		t.Errorf("started %v, want the card's run", wf.started)
	}
	want := fakePoke{repository: "sf/ai-software-factory-code", logins: []string{"sebastian"}, m: ChatMessage{Role: "assistant", Kind: MessagePoke,
		Content: `Filed #71 "Weekly bump of the pinned outside inputs".`, TaskID: card.ID, Decider: "routine weekly-bump · run 1 · every Monday 06:00", BirthKind: EventRoutineRan}}
	if pokes := routinePokes(t, r, store); !reflect.DeepEqual(pokes, []fakePoke{want}) {
		t.Fatalf("pokes %+v,\nwant %+v", pokes, want)
	}

	tree := routineTree(t)
	audit, _ := tree.Routine(codeProject, "audit")
	audit.Brief = "routines/audit.md"
	tree.Briefs = maps.Clone(tree.Briefs)
	tree.Briefs[audit.Brief] = "Hunt bugs in the code changed since the last audit.\n"
	r, store, issues, wf = routineRunner(t, withRoutines(tree, codeProject, audit))
	if err := store.SetRoutineNeedsMe(ctx, codeProject, "audit", &yes, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "audit", Trigger: "merges", Due: onTime}); err != nil {
		t.Fatal(err)
	}
	card = onlyTask(t, store)
	if card.Kind != "human" || card.Status != "waiting-human" || card.Source != SourceRoutine || card.RiskClass != "standard" || card.IssueNumber != 71 || card.Author != "factory-agent" {
		t.Fatalf("the flipped audit's card %+v", card)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"human", "routine", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	if body := issues.issues[0]; !strings.HasSuffix(body, "Hunt bugs in the code changed since the last audit.\n\nfiled by the factory · routine audit · run 1") {
		t.Errorf("the issue:\n%s", body)
	}
	if len(wf.started) != 0 {
		t.Errorf("a human card started %v", wf.started)
	}
	want = fakePoke{repository: "sf/ai-software-factory-code", logins: []string{"sebastian"}, m: ChatMessage{Role: "assistant", Kind: MessagePoke,
		Content: "#71 needs you: Audit: bug hunt and architecture review.", TaskID: card.ID, Decider: "routine audit · run 1 · every 20 merges", BirthKind: EventRoutineRan}}
	if pokes := routinePokes(t, r, store); !reflect.DeepEqual(pokes, []fakePoke{want}) {
		t.Fatalf("pokes %+v,\nwant %+v", pokes, want)
	}

	// the undo clears the override: the config's side files again
	r, store, _, _ = routineRunner(t, routineTree(t))
	for _, side := range []*bool{&no, nil} {
		if err := store.SetRoutineNeedsMe(ctx, codeProject, "weekly-bump", side, "sebastian"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime}); err != nil {
		t.Fatal(err)
	}
	if card := onlyTask(t, store); card.Kind != "human" || card.Status != "waiting-human" {
		t.Fatalf("the undone bump's card %+v", card)
	}

	// a pull-request routine ignores the toggle
	r, store, _, _ = routineRunner(t, routineTree(t))
	if err := store.SetRoutineNeedsMe(ctx, "", "template-pass", &no, "sebastian"); err != nil {
		t.Fatal(err)
	}
	passes := 0
	r.TemplatePass = func(context.Context) (int, error) {
		passes++
		return 2, nil
	}
	res, err := r.RoutineRun(ctx, RoutineRunInput{Routine: "template-pass", Trigger: "cron", Due: onTime})
	if err != nil || res != (RoutineRunResult{Outcome: "filed", Note: "2 cards"}) || passes != 1 {
		t.Fatalf("the template pass under the toggle: %+v %v passes %d", res, err, passes)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("the pass left cards %+v", store.tasks)
	}
}

// policy.yaml's template pass runs the registrar's pass over every
// project: the cards it filed are the run's note, a pass that filed none
// ends no change, and the factory's routine pokes no inbox, even under
// tell inbox; its log line names it without a project. A failed pass
// fails the attempt and leaves the run open, so the retry runs the pass
// again.
func TestRoutineRunRunsTheTemplatePass(t *testing.T) {
	ctx := context.Background()
	tree := routineTree(t)
	tree.Document.Routines = slices.Clone(tree.Document.Routines)
	tree.Document.Routines[0].Tell = policy.TellInbox
	r, store, _, _ := routineRunner(t, tree)
	logs := captureLog(t)
	var cards int
	var passErr error
	passes := 0
	r.TemplatePass = func(context.Context) (int, error) {
		passes++
		return cards, passErr
	}
	onTime := time.Now().Truncate(time.Hour)
	for i, tt := range []struct {
		cards int
		want  RoutineRunResult
	}{
		{2, RoutineRunResult{Outcome: "filed", Note: "2 cards"}},
		{1, RoutineRunResult{Outcome: "filed", Note: "1 card"}},
		{0, RoutineRunResult{Outcome: "no change"}},
	} {
		cards = tt.cards
		res, err := r.RoutineRun(ctx, RoutineRunInput{Routine: "template-pass", Trigger: "cron", Due: onTime.Add(time.Duration(i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if res != tt.want {
			t.Fatalf("%d cards: result %+v, want %+v", tt.cards, res, tt.want)
		}
		if run := store.routineRuns["/template-pass"][i]; run.Outcome != tt.want.Outcome || run.Note != tt.want.Note || run.Ended.IsZero() {
			t.Fatalf("%d cards: run %+v", tt.cards, run)
		}
	}
	if pokes := routinePokes(t, r, store); passes != 3 || len(pokes) != 0 || len(recordedEvents(r, EventRoutineRan)) != 0 {
		t.Fatalf("passes %d, pokes %+v: want 3 and none, and no event", passes, pokes)
	}
	if l := logs.String(); !strings.Contains(l, "routines: template-pass run 1 filed 2 cards\n") || strings.Contains(l, "/template-pass") {
		t.Fatalf("log %q, want the pass named without a project", l)
	}

	in := RoutineRunInput{Routine: "template-pass", Trigger: "cron", Due: onTime.Add(3 * time.Hour)}
	cards, passErr = 0, errors.New("forgejo: 502")
	if _, err := r.RoutineRun(ctx, in); err == nil || !strings.Contains(err.Error(), "forgejo: 502") {
		t.Fatalf("a failed pass: %v", err)
	}
	if run := store.routineRuns["/template-pass"][3]; run.Outcome != "" || !run.Ended.IsZero() {
		t.Fatalf("run %+v after a failed pass, want it open", run)
	}
	cards, passErr = 3, nil
	if res, err := r.RoutineRun(ctx, in); err != nil || res != (RoutineRunResult{Outcome: "filed", Note: "3 cards"}) || passes != 5 || len(store.routineRuns["/template-pass"]) != 4 {
		t.Fatalf("the retry: %+v, %v, passes %d, runs %d", res, err, passes, len(store.routineRuns["/template-pass"]))
	}
}

// A report routine files no card: the run ends reported, and under tell
// inbox the inbox hears the title and the brief. Under tell quiet only the
// run's row says it ran.
func TestRoutineRunReports(t *testing.T) {
	ctx := context.Background()
	tree := routineTree(t)
	tree.Briefs = maps.Clone(tree.Briefs)
	tree.Briefs["routines/drift.md"] = "The reverts and the drift of the week.\n"
	drift := policy.Routine{Name: "drift", Title: "Revert and drift report", Schedule: []policy.Trigger{{Cron: "0 7 * * 1"}},
		Files: policy.FilesReport, Brief: "routines/drift.md"}
	for _, tell := range []string{policy.TellInbox, policy.TellQuiet} {
		drift.Tell = tell
		r, store, issues, wf := routineRunner(t, withRoutines(tree, codeProject, drift))
		res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "drift", Trigger: "cron", Due: time.Now().Truncate(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if res != (RoutineRunResult{Outcome: "reported"}) || len(issues.issues) != 0 || len(store.tasks) != 0 || len(wf.started) != 0 {
			t.Fatalf("%s: result %+v, issues %d, tasks %d, started %v: want reported and no card", tell, res, len(issues.issues), len(store.tasks), wf.started)
		}
		if run := store.routineRuns[codeProject+"/drift"][0]; run.Outcome != "reported" || run.Ended.IsZero() {
			t.Fatalf("%s: run %+v", tell, run)
		}
		var pokes []fakePoke
		if tell == policy.TellInbox {
			pokes = []fakePoke{{repository: "sf/ai-software-factory-code", logins: []string{"sebastian"}, m: ChatMessage{Role: "assistant", Kind: MessagePoke,
				Content: "Revert and drift report\n\nThe reverts and the drift of the week.", Decider: "routine drift · run 1 · every Monday 07:00", BirthKind: EventRoutineRan}}}
		}
		if got := routinePokes(t, r, store); !reflect.DeepEqual(got, pokes) {
			t.Fatalf("%s: pokes %+v,\nwant %+v", tell, got, pokes)
		}
	}
}

// The same run twice, as a retry after its completion or a second start
// of the same due: the second returns the first's result and files,
// writes and pokes nothing.
func TestRoutineRunTwiceIsOnce(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	in := RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now().Truncate(time.Minute)}
	first, err := r.RoutineRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.RoutineRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != "filed" || second != first {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if pokes := routinePokes(t, r, store); len(issues.issues) != 1 || len(store.tasks) != 1 || len(store.routineRuns[codeProject+"/weekly-bump"]) != 1 || len(pokes) != 1 {
		t.Fatalf("issues %d, tasks %d, runs %d, pokes %d: want one each", len(issues.issues), len(store.tasks), len(store.routineRuns[codeProject+"/weekly-bump"]), len(pokes))
	}
}

// A run of a routine the tree no longer holds ends gone and writes no run.
func TestRoutineRunOfARoutineGone(t *testing.T) {
	r, store, issues, _ := routineRunner(t, routineTree(t))
	res, err := r.RoutineRun(context.Background(), RoutineRunInput{Project: codeProject, Routine: "nightly", Trigger: "cron", Due: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RoutineRunResult{Outcome: "gone", Note: "the routine is no longer in the configuration"}); res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if len(store.routineRuns) != 0 || len(store.routineStates) != 0 || len(issues.issues) != 0 {
		t.Fatalf("runs %+v, states %+v, issues %d", store.routineRuns, store.routineStates, len(issues.issues))
	}
}

// An attempt whose issue was filed and whose task row then failed leaves
// the run open: the retry takes on that issue, the bot's open one of the
// card's title, and files no second.
func TestRoutineRunRetryTakesOnTheIssueWhoseRowFailed(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	store.createFails = 1
	in := RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now().Truncate(time.Minute)}
	if _, err := r.RoutineRun(ctx, in); err == nil {
		t.Fatal("a failed task row raised nothing")
	}
	res, err := r.RoutineRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	card := onlyTask(t, store)
	if len(issues.issues) != 1 || card.IssueNumber != 71 || card.Status != "waiting-human" || res != (RoutineRunResult{Outcome: "filed", TaskID: card.ID, Issue: 71}) {
		t.Fatalf("issues %d, card %+v, result %+v", len(issues.issues), card, res)
	}
	if runs := store.routineRuns[codeProject+"/weekly-bump"]; len(runs) != 1 || runs[0].TaskID != card.ID {
		t.Fatalf("runs %+v, want the one run with the card", runs)
	}
}

// A routine's card that ends ends the run that filed it: the final state
// is its outcome, its cost the card's spend, and the run its note keeps.
// A later run held while the card was open, which names it in its note
// only, stays as it was.
func TestTaskEndedFinishesTheRun(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	due := time.Now().Truncate(time.Minute)
	filed, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: due.Add(-7 * 24 * time.Hour), Trigger: "cron"}, true)
	if err := store.RoutineRunFiled(ctx, codeProject, "weekly-bump", filed.Run, "t-781", 781); err != nil {
		t.Fatal(err)
	}
	held, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: due, Trigger: "cron"}, true)
	if err := store.EndRoutineRun(ctx, codeProject, "weekly-bump", held.Run, "held", "held: #781 is still open", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	card := Task{ID: "t-781", Kind: "agent", Status: "done", Repository: "sf/ai-software-factory-code", IssueNumber: 781, Source: SourceRoutine, SpentEUR: 1.25,
		FinishedAt: &due}
	store.tasks[card.ID] = card
	taskEnded(ctx, nil, store, nil, nil, card, "done", "")
	runs := store.routineRuns[codeProject+"/weekly-bump"]
	if r := runs[0]; r.Outcome != "done" || r.Ended.IsZero() || r.CostEUR != 1.25 || r.TaskID != "t-781" || r.Issue != 781 || r.Note != "" {
		t.Fatalf("the filing run %+v", r)
	}
	if r := runs[1]; r.Outcome != "held" || r.Note != "held: #781 is still open" || r.TaskID != "" || r.CostEUR != 0 {
		t.Fatalf("the held run %+v", r)
	}
}

// A Run now leaves a pending Skip once to the run it was set for: it
// files, and the cron run after it is the one skipped.
func TestRoutineRunNowLeavesTheSkip(t *testing.T) {
	ctx := context.Background()
	r, store, issues, _ := routineRunner(t, routineTree(t))
	if err := store.SetRoutineSkip(ctx, codeProject, "weekly-bump", true, "sebastian"); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Truncate(time.Minute)
	if res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: TriggerRunNow, Due: due}); err != nil ||
		res.Outcome != "filed" || len(issues.issues) != 1 {
		t.Fatalf("the Run now: %+v %v, issues %d", res, err, len(issues.issues))
	}
	if st, _ := store.RoutineState(ctx, codeProject, "weekly-bump"); !st.SkipNext || st.SkipBy != "sebastian" {
		t.Fatalf("state %+v: the Run now took the mark", st)
	}
	if res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: due.Add(time.Hour)}); err != nil ||
		res.Outcome != "skipped" || res.Note != "skipped once by sebastian" {
		t.Fatalf("the cron run after: %+v %v", res, err)
	}
}

// A run that failed for good after it began skipped ends skipped with its
// note; one that failed before it began ends failed and leaves a pending
// Skip once to the next run.
func TestRoutineRunFailedKeepsTheSkip(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := routineRunner(t, routineTree(t))
	if err := store.SetRoutineSkip(ctx, codeProject, "weekly-bump", true, "sebastian"); err != nil {
		t.Fatal(err)
	}
	in := RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)}
	if began, ok, err := store.BeginRoutineRun(ctx, RoutineRun{Project: in.Project, Routine: in.Routine, Due: in.Due, Trigger: in.Trigger}, true); err != nil ||
		!ok || began.Outcome != "skipped" {
		t.Fatalf("the begun run: %+v %v %v", began, ok, err)
	}
	if err := r.RoutineRunFailed(ctx, in, "events: connection refused"); err != nil {
		t.Fatal(err)
	}
	if run := store.routineRuns[codeProject+"/weekly-bump"][0]; run.Outcome != "skipped" || run.Note != "skipped once by sebastian" || run.Ended.IsZero() {
		t.Fatalf("the skipped run %+v, want it ended skipped", run)
	}
	if ran := recordedEvents(r, EventRoutineRan); len(ran) != 1 || ran[0].Detail["outcome"] != "skipped" || ran[0].Detail["note"] != "skipped once by sebastian" {
		t.Fatalf("routine_ran %+v, want one skipped", ran)
	}

	if err := store.SetRoutineSkip(ctx, codeProject, "weekly-bump", true, "anna"); err != nil {
		t.Fatal(err)
	}
	next := in
	next.Due = in.Due.Add(7 * 24 * time.Hour)
	if err := r.RoutineRunFailed(ctx, next, "routine ai-software-factory-code/weekly-bump: no project ai-software-factory-code in the store"); err != nil {
		t.Fatal(err)
	}
	if run := store.routineRuns[codeProject+"/weekly-bump"][1]; run.Run != 2 || run.Outcome != "failed" || run.Ended.IsZero() ||
		run.Note != "routine ai-software-factory-code/weekly-bump: no project ai-software-factory-code in the store" {
		t.Fatalf("the run that failed before it began %+v, want run 2 failed", run)
	}
	if st, _ := store.RoutineState(ctx, codeProject, "weekly-bump"); !st.SkipNext || st.SkipBy != "anna" {
		t.Fatalf("state %+v: the failure took the mark", st)
	}
}
