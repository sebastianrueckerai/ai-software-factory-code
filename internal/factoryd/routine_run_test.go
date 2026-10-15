package factoryd

import (
	"context"
	"encoding/json"
	"errors"
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
// project in the maintenance process, the code repository's issues and a
// recorder of the workflows it starts.
func routineRunner(t *testing.T, tree policy.Tree) (*Runner, *fakeStore, *codeIssues, *fakeWorkflows) {
	t.Helper()
	issues := &codeIssues{labels: map[int64][]string{}}
	store, wf := newFakeStore(), &fakeWorkflows{}
	store.projects = []Project{{Name: codeProject, Repository: "sf/ai-software-factory-code", File: true, Objects: policy.ObjectsSeed,
		Process: "maintenance", Lanes: maintenanceLanes}}
	v := policy.Version{Tree: tree}
	r := &Runner{store: store, forgejo: issues.srv(t), timeline: &Timeline{Objects: &fakeObjectStore{}},
		cfg: RunnerConfig{Policy: func() policy.Version { return v }, Workflows: wf}, Routines: store}
	return r, store, issues, wf
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

// The weekly bump at its due: a human card on the code repository, the
// brief's text as its body with the run's provenance, waiting on a
// person; the run stays open with its card, and the inbox of the
// project's approvers hears of it once, with the routine, the run and
// the schedule as the decider line. A human card pokes under tell quiet
// too. Hand-derived: the first issue is #71, the fixture's approver
// sebastian.
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
	if got := issues.labels[71]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
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
		"apt-archive.postgresql.org", "go.mod's `toolchain`", "ADD --checksum", "buildkitSHA256", "scripts/upstream-images-test/test.sh",
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
		Content: "#71 needs you: Weekly bump of the pinned outside inputs.", TaskID: card.ID, Decider: "routine weekly-bump · run 1 · every Monday 06:00"}}
	if !reflect.DeepEqual(store.pokes, []fakePoke{want}) {
		t.Fatalf("pokes %+v,\nwant %+v", store.pokes, want)
	}

	bump, _ := routineTree(t).Routine(codeProject, "weekly-bump")
	bump.Tell = policy.TellQuiet
	r, store, _, _ = routineRunner(t, withRoutines(routineTree(t), codeProject, bump))
	if _, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime}); err != nil {
		t.Fatal(err)
	}
	if len(store.pokes) != 1 || store.pokes[0].m.Content != "#71 needs you: Weekly bump of the pinned outside inputs." {
		t.Fatalf("pokes under tell quiet %+v, want the human card's one", store.pokes)
	}
}

// While the last run's card #781 is open, a routine that holds files
// nothing: the run ends held, names the card in its note and keeps no
// task of its own, so the card's end finds the run that filed it; under
// tell inbox the inbox hears why, about that card. Quiet holds in
// silence, fold ends folded into the card, and file files a second card
// beside it. Once #781 ends, the next run files.
func TestRoutineRunHeldWhileOpen(t *testing.T) {
	ctx := context.Background()
	onTime := time.Now().Truncate(time.Minute)
	for _, tt := range []struct {
		name, whileOpen, tell string
		want                  RoutineRunResult
		poke                  string
	}{
		{"hold", policy.WhileOpenHold, policy.TellInbox, RoutineRunResult{Outcome: "held", Note: "held: #781 is still open"},
			"Weekly bump of the pinned outside inputs is held: #781 from the last run is still open."},
		{"hold quietly", policy.WhileOpenHold, policy.TellQuiet, RoutineRunResult{Outcome: "held", Note: "held: #781 is still open"}, ""},
		{"fold", policy.WhileOpenFold, policy.TellInbox, RoutineRunResult{Outcome: "folded", Note: "folded into #781"}, ""},
		{"file", policy.WhileOpenFile, policy.TellQuiet, RoutineRunResult{Outcome: "filed", Issue: 71}, "#71 needs you: Weekly bump of the pinned outside inputs."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bump, _ := routineTree(t).Routine(codeProject, "weekly-bump")
			bump.WhileOpen, bump.Tell = tt.whileOpen, tt.tell
			r, store, issues, _ := routineRunner(t, withRoutines(routineTree(t), codeProject, bump))
			store.tasks["t-781"] = Task{ID: "t-781", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-code", IssueNumber: 781, Source: SourceRoutine}
			last, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: onTime.Add(-7 * 24 * time.Hour), Trigger: "cron"})
			if err := store.RoutineRunFiled(ctx, codeProject, "weekly-bump", last.Run, "t-781", 781); err != nil {
				t.Fatal(err)
			}
			res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: onTime})
			if err != nil {
				t.Fatal(err)
			}
			pokedAbout := "t-781"
			if tt.whileOpen == policy.WhileOpenFile {
				if len(store.tasks) != 2 || store.tasks[res.TaskID].IssueNumber != 71 {
					t.Fatalf("tasks %+v, result %+v: want a second card, #71", store.tasks, res)
				}
				tt.want.TaskID, pokedAbout = res.TaskID, res.TaskID
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
					Content: tt.poke, TaskID: pokedAbout, Decider: "routine weekly-bump · run 2 · every Monday 06:00"}}}
			}
			if !reflect.DeepEqual(store.pokes, pokes) {
				t.Fatalf("pokes %+v,\nwant %+v", store.pokes, pokes)
			}
		})
	}

	r, store, issues, _ := routineRunner(t, routineTree(t))
	store.tasks["t-781"] = Task{ID: "t-781", Kind: "human", Status: "done", Repository: "sf/ai-software-factory-code", IssueNumber: 781, Source: SourceRoutine}
	last, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: onTime.Add(-7 * 24 * time.Hour), Trigger: "cron"})
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
	if len(issues.issues) != 0 || len(store.tasks) != 0 || len(store.pokes) != 0 {
		t.Fatalf("issues %d, tasks %d, pokes %d: want none", len(issues.issues), len(store.tasks), len(store.pokes))
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
	if run := store.routineRuns[codeProject+"/weekly-bump"][0]; len(issues.issues) != 0 || len(store.pokes) != 0 || run.Outcome != "skipped" || run.Ended.IsZero() {
		t.Fatalf("issues %d, pokes %d, run %+v", len(issues.issues), len(store.pokes), run)
	}
	if res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now()}); err != nil ||
		res.Outcome != "filed" || len(issues.issues) != 1 {
		t.Fatalf("on time: %+v, %v, issues %d", res, err, len(issues.issues))
	}
}

// The audit at its twentieth merge: an agent card through the intake
// every agent card the factory files takes, in the project's ready lane,
// queued and started, labelled routine; its objective is the routine's
// title and the skill to run, its budget the policy's default (2 EUR in
// the fixture), and its issue names the run. Tell inbox pokes the
// filing.
func TestRoutineRunFilesAnAgentCard(t *testing.T) {
	ctx := context.Background()
	r, store, issues, wf := routineRunner(t, routineTree(t))
	res, err := r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "audit", Trigger: "merges", Due: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	card := onlyTask(t, store)
	if card.Kind != "agent" || card.Status != "queued" || card.Lane != policy.LaneReady || card.Process != "maintenance" || card.Source != SourceRoutine ||
		card.BudgetEUR != 2 || card.RiskClass != "standard" || card.IssueNumber != 71 || card.Author != "" ||
		card.Objective != "Audit: bug hunt and architecture review\n\nRun the skill audit." {
		t.Fatalf("card %+v", card)
	}
	if want := []string{"Audit: bug hunt and architecture review\n\nRun the skill audit.\n\nfiled by the factory · routine audit · run 1"}; !slices.Equal(issues.issues, want) {
		t.Fatalf("issues %q, want %q", issues.issues, want)
	}
	if got := issues.labels[71]; !slices.Equal(got, []string{"routine", "state/queued"}) {
		t.Errorf("labels = %v", got)
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
		Content: `Filed #71 "Audit: bug hunt and architecture review".`, TaskID: card.ID, Decider: "routine audit · run 1 · every 20 merges"}}
	if !reflect.DeepEqual(store.pokes, []fakePoke{want}) {
		t.Fatalf("pokes %+v,\nwant %+v", store.pokes, want)
	}
}

// policy.yaml's template pass runs the registrar's pass over every
// project: the cards it filed are the run's note, a pass that filed none
// ends no change, and the factory's routine pokes no inbox, even under
// tell inbox. A failed pass fails the attempt and leaves the run open,
// so the retry runs the pass again.
func TestRoutineRunRunsTheTemplatePass(t *testing.T) {
	ctx := context.Background()
	tree := routineTree(t)
	tree.Document.Routines = slices.Clone(tree.Document.Routines)
	tree.Document.Routines[0].Tell = policy.TellInbox
	r, store, _, _ := routineRunner(t, tree)
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
		{0, RoutineRunResult{Outcome: "no change", Note: "no change"}},
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
	if passes != 3 || len(store.pokes) != 0 {
		t.Fatalf("passes %d, pokes %+v: want 3 and none", passes, store.pokes)
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
	if len(issues.issues) != 1 || len(store.tasks) != 1 || len(store.routineRuns[codeProject+"/weekly-bump"]) != 1 || len(store.pokes) != 1 {
		t.Fatalf("issues %d, tasks %d, runs %d, pokes %d: want one each", len(issues.issues), len(store.tasks), len(store.routineRuns[codeProject+"/weekly-bump"]), len(store.pokes))
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
	filed, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: due.Add(-7 * 24 * time.Hour), Trigger: "cron"})
	if err := store.RoutineRunFiled(ctx, codeProject, "weekly-bump", filed.Run, "t-781", 781); err != nil {
		t.Fatal(err)
	}
	held, _, _ := store.BeginRoutineRun(ctx, RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: due, Trigger: "cron"})
	if err := store.EndRoutineRun(ctx, codeProject, "weekly-bump", held.Run, "held", "held: #781 is still open", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	card := Task{ID: "t-781", Kind: "agent", Status: "done", Repository: "sf/ai-software-factory-code", IssueNumber: 781, Source: SourceRoutine, SpentEUR: 1.25}
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
