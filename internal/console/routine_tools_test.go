package console

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// routineRig is the chat rig on demo-app with the policy package's test
// tree as infra's head: the fake infra holds the tree's
// projects/demo-app.yaml, and CheckChange judges a change against the
// tree the loader's way. A test moves the head by changing the map.
func routineRig(t *testing.T) (*rig, *chatGateway, *infraFake, map[string][]byte) {
	t.Helper()
	r, _, g := newChatRig(t)
	g.project = "demo-app"
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	infra := withInfra(t, r)
	infra.files = map[string]string{"factory/projects/demo-app.yaml": string(files["projects/demo-app.yaml"])}
	ladder := factoryd.ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}.Aliases()
	r.c.cfg.CheckChange = func(_ context.Context, changes map[string][]byte) ([]string, error) {
		if err := factoryd.CheckChange(files, changes, ladder); err != nil {
			return strings.Split(err.Error(), "\n"), nil
		}
		return nil, nil
	}
	return r, g, infra, files
}

// headAt makes the files infra's head and the configuration in force:
// the fake infra serves them and the ledger's demo-app is the tree's.
func headAt(t *testing.T, r *rig, infra *infraFake, files map[string][]byte) {
	t.Helper()
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	r.ledger.files = map[string]policy.ProjectConfig{"demo-app": tree.Projects["demo-app"]}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	for name, b := range files {
		if name == "projects/demo-app.yaml" || strings.HasPrefix(name, "routines/") {
			infra.files["factory/"+name] = string(b)
		}
	}
}

// lastCommit is the fake infra's newest commit, each file as its
// operation, path, sha when it names one, and text.
func lastCommit(t *testing.T, infra *infraFake) []string {
	t.Helper()
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.contents) == 0 {
		t.Fatal("no commit")
	}
	var out []string
	for _, f := range infra.contents[len(infra.contents)-1].body["files"].([]any) {
		change := f.(map[string]any)
		line := fmt.Sprint(change["operation"], " ", change["path"])
		if sha, ok := change["sha"]; ok {
			line += fmt.Sprint(" ", sha)
		}
		text, _ := base64.StdEncoding.DecodeString(fmt.Sprint(change["content"]))
		out = append(out, line+"\n"+string(text))
	}
	return out
}

// routineDetail is a routine event's detail on demo-app, which the chat
// runs at manual with merge rule auto, the member deciding.
func routineDetail(more map[string]any) map[string]any {
	d := map[string]any{"project": "demo-app", "autonomy": "manual", "merge_rule": "auto", "decider": "member",
		"chat_task": factoryd.ChatTaskID("demo-app", time.Now())}
	for k, v := range more {
		d[k] = v
	}
	return d
}

const depsBlock = "routines:\n  - name: dependency-update\n    title: Dependency update\n    schedule:\n      - cron: 0 6 * * 4\n    files: agent\n    skill: deps\n"

// routine_write proposes the routine as the YAML it writes, and nothing
// commits until Do it. Do it commits the project file with the routine
// appended and every other line kept, through a pull request it merges,
// and records one routine_written row via chat. A second write of the
// same name replaces the entry and writes its brief beside it.
func TestRoutineWriteProposesThenCommitsTheProjectFile(t *testing.T) {
	r, g, infra, files := routineRig(t)
	ck, s := r.loggedIn(t)
	before := r.events.count()
	original := string(files["projects/demo-app.yaml"])
	args := `{"name":"dependency-update","title":"Dependency update","schedule":"0 6 * * 4","files":"agent","skill":"deps"}`
	g.replies = [][]string{{call(1, "routine_write", args)}}
	_, proposed, tokens := chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"Every Thursday at six, update the dependencies"}})
	equal(t, "the proposal", proposed, []string{`proposal {"arguments":` + args + `,"description":"Write a routine, new in projects/demo-app.yaml:\n\n` +
		`- name: dependency-update\n  title: Dependency update\n  schedule:\n    - cron: 0 6 * * 4\n  files: agent\n  skill: deps","id":"TOKEN","tool":"routine_write"}`,
		`done {}`})
	if len(infra.contents) != 0 {
		t.Fatalf("a proposal committed: %+v", infra.contents)
	}

	_, done, _ := chatPost(t, r, ck, s, "/projects/demo-app/chat/proposals/"+tokens[0], nil)
	equal(t, "Do it", done, ran("routine_write", args,
		"Pull request #12 added routine dependency-update to projects/demo-app.yaml. The board shows it once the factory loads the new configuration."))
	equal(t, "the commit", lastCommit(t, infra), []string{"update factory/projects/demo-app.yaml blob1\n" + original + depsBlock})
	equal(t, "the pull request and its merge", []any{len(infra.pulls), infra.pulls[0].body["title"], len(infra.merges)},
		[]any{1, "Project demo-app: routine dependency-update", 1})
	files["projects/demo-app.yaml"] = []byte(original + depsBlock)
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := tree.Routine("demo-app", "dependency-update")
	equal(t, "the routine in the tree", rt, policy.Routine{Name: "dependency-update", Title: "Dependency update",
		Schedule: []policy.Trigger{{Cron: "0 6 * * 4"}}, Files: policy.FilesAgent, Skill: "deps"})

	headAt(t, r, infra, files)
	args = `{"name":"dependency-update","title":"Dependency update","schedule":"0 6 * * 4; merges:20","files":"agent","skill":"deps",` +
		`"brief_text":"Update the dependencies and run the tests."}`
	g.replies = [][]string{{call(1, "routine_write", args)}}
	_, proposed, tokens = chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"Also every 20 merges, with a brief"}})
	equal(t, "the change's proposal", proposed, []string{`proposal {"arguments":` + args + `,"description":"Write a routine, in place of dependency-update in projects/demo-app.yaml:\n\n` +
		`- name: dependency-update\n  title: Dependency update\n  schedule:\n    - cron: 0 6 * * 4\n    - merges: 20\n  files: agent\n` +
		`  brief: routines/demo-app-dependency-update.md\n  skill: deps\n\nIts brief, routines/demo-app-dependency-update.md:\n\n` +
		`Update the dependencies and run the tests.","id":"TOKEN","tool":"routine_write"}`, `done {}`})
	_, done, _ = chatPost(t, r, ck, s, "/projects/demo-app/chat/proposals/"+tokens[0], nil)
	equal(t, "the change's Do it", done, ran("routine_write", args,
		"Pull request #12 changed routine dependency-update in projects/demo-app.yaml and wrote its brief routines/demo-app-dependency-update.md. "+
			"The board shows it once the factory loads the new configuration."))
	changed := original + "routines:\n  - name: dependency-update\n    title: Dependency update\n    schedule:\n      - cron: 0 6 * * 4\n      - merges: 20\n" +
		"    files: agent\n    brief: routines/demo-app-dependency-update.md\n    skill: deps\n"
	equal(t, "the change's commit", lastCommit(t, infra), []string{"update factory/projects/demo-app.yaml blob1\n" + changed,
		"create factory/routines/demo-app-dependency-update.md\nUpdate the dependencies and run the tests.\n"})
	files["projects/demo-app.yaml"], files["routines/demo-app-dependency-update.md"] = []byte(changed), []byte("Update the dependencies and run the tests.\n")
	if tree, err = policy.ParseTree(files); err != nil {
		t.Fatal(err)
	}
	rt, _ = tree.Routine("demo-app", "dependency-update")
	brief, _ := tree.Brief(rt.Brief)
	equal(t, "the changed routine and its brief", []any{rt.Schedule, rt.Brief, brief},
		[]any{[]policy.Trigger{{Cron: "0 6 * * 4"}, {Merges: 20}}, "routines/demo-app-dependency-update.md", "Update the dependencies and run the tests.\n"})

	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventRoutineWritten, Detail: routineDetail(map[string]any{"routine": "dependency-update", "created": true})},
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventRoutineWritten, Detail: routineDetail(map[string]any{"routine": "dependency-update", "created": false})})
}

// A schedule the routine's check refuses comes back as the tool's
// result, naming the fault, before any proposal: nothing is proposed or
// committed and the agent answers on.
func TestRoutineWriteRefusesABadSchedule(t *testing.T) {
	r, g, infra, _ := routineRig(t)
	ck, s := r.loggedIn(t)
	before := r.events.count()
	args := `{"name":"dependency-update","title":"Dependency update","schedule":"every monday","files":"agent","skill":"deps"}`
	g.replies = [][]string{{call(1, "routine_write", args)}, {say("The schedule needs a cron.")}}
	_, events, tokens := chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"every monday, update the dependencies"}})
	equal(t, "the stream", []any{events, len(tokens)}, []any{[]string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"routine_write","result":"Refused: projects/demo-app.yaml: routine \"dependency-update\": ` +
			`cron \"every monday\": five fields, minute hour day-of-month month day-of-week, in policy.yaml's time_zone"}`,
		`text {"text":"The schedule needs a cron."}`, `done {}`}, 0})
	if len(infra.contents) != 0 || r.events.count() != before {
		t.Fatalf("a refused schedule committed %+v or recorded %d rows", infra.contents, r.events.count()-before)
	}
}

// The schedule's free form: parts split on ";" and trimmed, merges:<n> a
// count, on:<event> an event, any other part a cron with its spaces
// made single, which the routine's check then judges.
func TestScheduleOf(t *testing.T) {
	got, err := scheduleOf(" 0  6 * * 1 ; merges:20;on:push-main ;")
	equal(t, "the three triggers", []any{got, err},
		[]any{[]policy.Trigger{{Cron: "0 6 * * 1"}, {Merges: 20}, {On: "push-main"}}, error(nil)})
	for _, bad := range []string{"merges:twenty", "merges:0", "merges:-3"} {
		if _, err := scheduleOf(bad); err == nil || err.Error() != `schedule part "`+bad+`": merges takes a count of merged cards, such as merges:20` {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// routine_delete proposes the routine it takes out, and Do it commits the
// project file without it, the routines key gone with the last one, and
// deletes its brief, which no other routine names; one routine_deleted
// row via chat. A brief another routine names stays, and a routine of
// policy.yaml or none at all is refused.
func TestRoutineDeleteRemovesTheRoutineAndItsBrief(t *testing.T) {
	r, g, infra, files := routineRig(t)
	ck, s := r.loggedIn(t)
	original := string(files["projects/demo-app.yaml"])
	entry := "  - name: dependency-update\n    title: Dependency update\n    schedule:\n      - cron: \"0 6 * * 4\"\n    files: agent\n" +
		"    brief: routines/demo-app-dependency-update.md\n    skill: deps\n"
	files["projects/demo-app.yaml"] = []byte(original + "routines:\n" + entry)
	files["routines/demo-app-dependency-update.md"] = []byte("Update the dependencies.\n")
	headAt(t, r, infra, files)
	before := r.events.count()

	args := `{"name":"dependency-update"}`
	g.replies = [][]string{{call(1, "routine_delete", args)}}
	_, proposed, tokens := chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"stop updating the dependencies"}})
	equal(t, "the proposal", proposed, []string{`proposal {"arguments":` + args + `,"description":"Delete a routine, out of projects/demo-app.yaml:\n\n` +
		`- name: dependency-update\n  title: Dependency update\n  schedule:\n    - cron: 0 6 * * 4\n  files: agent\n  brief: routines/demo-app-dependency-update.md\n  skill: deps` +
		`\n\nIts brief, routines/demo-app-dependency-update.md, goes too.","id":"TOKEN","tool":"routine_delete"}`, `done {}`})
	_, done, _ := chatPost(t, r, ck, s, "/projects/demo-app/chat/proposals/"+tokens[0], nil)
	equal(t, "Do it", done, ran("routine_delete", args,
		"Pull request #12 deleted routine dependency-update from projects/demo-app.yaml, and its brief routines/demo-app-dependency-update.md."))
	equal(t, "the commit", lastCommit(t, infra), []string{"update factory/projects/demo-app.yaml blob1\n" + original,
		"delete factory/routines/demo-app-dependency-update.md blob1\n"})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventRoutineDeleted, Detail: routineDetail(map[string]any{"routine": "dependency-update"})})

	// a second routine names the same brief: the brief stays
	second := strings.ReplaceAll(entry, "dependency-update\n", "dependency-audit\n")
	files["projects/demo-app.yaml"] = []byte(original + "routines:\n" + entry + second)
	headAt(t, r, infra, files)
	g.replies = [][]string{{call(1, "routine_delete", `{"name":"dependency-audit"}`)}}
	_, _, tokens = chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"stop the audit"}})
	_, done, _ = chatPost(t, r, ck, s, "/projects/demo-app/chat/proposals/"+tokens[0], nil)
	equal(t, "the shared brief's Do it", done, ran("routine_delete", `{"name":"dependency-audit"}`,
		"Pull request #12 deleted routine dependency-audit from projects/demo-app.yaml."))
	equal(t, "the shared brief's commit", lastCommit(t, infra), []string{"update factory/projects/demo-app.yaml blob1\n" + original + "routines:\n" + entry})

	// policy.yaml's template pass runs for demo-app once it has a template
	demo := r.ledger.files["demo-app"]
	demo.Template = "ts-go"
	r.ledger.files["demo-app"] = demo
	r.ledger.routines = []policy.Routine{{Name: "template-pass", Title: "Template update", Schedule: []policy.Trigger{{Cron: "3 * * * *"}},
		Files: policy.FilesPullRequest, Pass: policy.PassTemplate}}
	for _, tt := range []struct{ name, want string }{
		{"template-pass", "Routine template-pass is policy.yaml's, a pass over every project: the project's chat cannot change it."},
		{"nightly", "Project demo-app has no routine nightly. The routines read lists its routines."},
	} {
		g.replies = [][]string{{call(1, "routine_delete", `{"name":"`+tt.name+`"}`)}, {say("No.")}}
		_, events, _ := chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"delete " + tt.name}})
		equal(t, tt.name, events[0], `tool {"arguments":{"name":"`+tt.name+`"},"duration_ms":0,"name":"routine_delete","result":"Refused: `+tt.want+`"}`)
	}
}

// The routines read is one line per routine of the chat's project: its
// name, title, schedule in words, what it files, the keys it sets of
// skill, while_open, missed and tell, its next run, its last run with the
// card, and the open card; a project without routines says so.
func TestRoutinesReadListsTheProject(t *testing.T) {
	r, _, g := newChatRig(t)
	audit := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "audit", Title: "Audit pair", Files: policy.FilesAgent, Skill: "audit", Schedule: []policy.Trigger{{Merges: 20}},
			WhileOpen: policy.WhileOpenFold},
		NextWords: "after 6 more merges", Last: factoryd.RoutineRun{Run: 2, Outcome: "filed", Issue: 790}, Open: &factoryd.Task{IssueNumber: 790}, Held: true}
	bump := weeklyBump()
	bump.Last = factoryd.RoutineRun{Run: 3, Outcome: "merged", Issue: 781}
	nightly := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "nightly", Title: "Nightly report", Files: policy.FilesReport, Schedule: []policy.Trigger{{Cron: "0 2 * * *"}, {On: policy.OnPushMain}},
			Missed: policy.MissedSkip, Tell: policy.TellQuiet},
		NextWords: "Tue 02:00"}
	r.routines.views = []factoryd.RoutineView{bump, audit, nightly}
	read := func(project string) string {
		t.Helper()
		g.project = project
		ck, s := r.loggedIn(t)
		before := len(g.calls())
		g.replies = [][]string{{call(1, "routines", `{}`)}, {say("Read them.")}}
		chatPost(t, r, ck, s, "/projects/"+project+"/chat", url.Values{"message": {"what runs on its own?"}})
		msgs := messages(t, g.calls()[before+1])
		return msgs[len(msgs)-1]["content"].(string)
	}
	equal(t, "the code project's routines", read("ai-software-factory-code"),
		"weekly-bump · Weekly bump · every Monday 06:00 · files human · next Mon 20 Oct 06:00 · last merged (#781)\n"+
			"audit · Audit pair · every 20 merges · files agent · skill audit · while open fold · next after 6 more merges · last filed (#790) · open #790\n"+
			"nightly · Nightly report · daily 02:00 · on each push to main · files report · missed skip · tell quiet · next Tue 02:00 · no run yet")
	equal(t, "demo-app's", read("demo-app"), "Project demo-app has no routines.")
}

// routine_run is the board's Run now through the chat: proposed at
// manual, then the run's outcome as its result and one routine_run_now
// row via chat. routine_skip sets the Skip once and with undo clears it,
// one routine_skipped row via chat each.
func TestChatRoutineRunAndSkip(t *testing.T) {
	wait, poll := runNowWait, runNowPoll
	runNowWait, runNowPoll = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { runNowWait, runNowPoll = wait, poll })
	r, g := newWritesRig(t)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	r.routines.runNow = func(string, string) (factoryd.RoutineRun, error) {
		return factoryd.RoutineRun{Outcome: "held", Note: "held: #781 is still open"}, nil
	}
	before := r.events.count()
	const name = `{"name":"weekly-bump"}`
	proposed, done, _ := proposeAndDo(t, r, g, "routine_run", name)
	equal(t, "the run's proposal", proposed, []string{`proposal {"arguments":` + name + `,"description":"Run a routine now, name weekly-bump","id":"TOKEN","tool":"routine_run"}`, `done {}`})
	equal(t, "the run", done, ran("routine_run", name, "Held: #781 is still open"))
	_, done, _ = proposeAndDo(t, r, g, "routine_skip", name)
	equal(t, "the skip", done, ran("routine_skip", name, "Skipped once · next Mon 20 Oct 06:00"))
	if by := r.routines.skips["ai-software-factory-code/weekly-bump"]; by != "sebastian" {
		t.Fatalf("the skip reached the port as %q", by)
	}
	const undo = `{"name":"weekly-bump","undo":true}`
	proposed, done, _ = proposeAndDo(t, r, g, "routine_skip", undo)
	equal(t, "the undo's proposal", proposed, []string{`proposal {"arguments":` + undo + `,"description":"Skip a routine's next run, name weekly-bump, undo the pending skip","id":"TOKEN","tool":"routine_skip"}`, `done {}`})
	equal(t, "the undo", done, ran("routine_skip", undo, "Skip once undone"))
	routine := func(more map[string]any) map[string]any {
		d := map[string]any{"project": "ai-software-factory-code", "routine": "weekly-bump"}
		for k, v := range more {
			d[k] = v
		}
		return d
	}
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", Kind: factoryd.EventRoutineRunNow, Detail: routine(map[string]any{"outcome": "held"})},
		factoryd.Event{Repository: "sf/code", Kind: factoryd.EventRoutineSkipped, Detail: routine(map[string]any{"undo": false})},
		factoryd.Event{Repository: "sf/code", Kind: factoryd.EventRoutineSkipped, Detail: routine(map[string]any{"undo": true})})
}

// The routine edits keep every other line and comment of the file: a
// change replaces the entry of its name in place, the comment above it
// kept, a new routine goes last, and a deletion takes its entry out, the
// routines key with the last one.
func TestRoutineEditsKeepTheFile(t *testing.T) {
	const head = "# the code project\nrepository: sf/code # its repository\n"
	raw := []byte(head + "routines:\n  # the bump\n  - name: weekly-bump\n    title: Bump\n    schedule:\n      - cron: \"0 6 * * 1\"\n    files: human\n")
	bump := policy.Routine{Name: "weekly-bump", Title: "Weekly bump", Schedule: []policy.Trigger{{Cron: "0 6 * * 1"}, {Merges: 20}}, Files: policy.FilesHuman}
	created := true
	out, err := editProjectFile(raw, func(doc *yaml.Node) (err error) { created, err = putRoutine(doc, bump); return err })
	changed := head + "routines:\n  # the bump\n  - name: weekly-bump\n    title: Weekly bump\n    schedule:\n      - cron: 0 6 * * 1\n      - merges: 20\n    files: human\n"
	equal(t, "the change", []any{string(out), created, err}, []any{changed, false, error(nil)})

	audit := policy.Routine{Name: "audit", Title: "Audit", Schedule: []policy.Trigger{{On: policy.OnPushMain}}, Files: policy.FilesReport}
	out, err = editProjectFile(out, func(doc *yaml.Node) (err error) { created, err = putRoutine(doc, audit); return err })
	added := changed + "  - name: audit\n    title: Audit\n    schedule:\n      - \"on\": push-main\n    files: report\n"
	equal(t, "the new routine", []any{string(out), created, err}, []any{added, true, error(nil)})

	out, err = editProjectFile(out, func(doc *yaml.Node) error { return dropRoutine(doc, "weekly-bump") })
	equal(t, "one deleted", []any{string(out), err}, []any{head + "routines:\n  - name: audit\n    title: Audit\n    schedule:\n      - \"on\": push-main\n    files: report\n", error(nil)})
	out, err = editProjectFile(out, func(doc *yaml.Node) error { return dropRoutine(doc, "audit") })
	equal(t, "the last deleted", []any{string(out), err}, []any{head, error(nil)})
	_, err = editProjectFile(out, func(doc *yaml.Node) error { return dropRoutine(doc, "audit") })
	equal(t, "a routine the file lacks", fmt.Sprint(err), "the project file holds no routine audit")

	out, err = editProjectFile([]byte(head+"routines: []\n"), func(doc *yaml.Node) (err error) { _, err = putRoutine(doc, audit); return err })
	equal(t, "an empty flow list", []any{string(out), err}, []any{head + "routines:\n  - name: audit\n    title: Audit\n    schedule:\n      - \"on\": push-main\n    files: report\n", error(nil)})
}

// A write keeps the keys of the routine it changes that its call leaves
// out, so a schedule-only change keeps the agent routine's skill and its
// while_open. A write that leaves the file as it is refuses and commits
// nothing.
func TestRoutineWriteKeepsTheKeysItLeavesOut(t *testing.T) {
	r, g, infra, files := routineRig(t)
	ck, s := r.loggedIn(t)
	original := string(files["projects/demo-app.yaml"])
	routine := func(cron string) string {
		return "routines:\n  - name: dependency-update\n    title: Dependency update\n    schedule:\n      - cron: " + cron + "\n" +
			"    files: agent\n    skill: deps\n    while_open: fold\n"
	}
	files["projects/demo-app.yaml"] = []byte(original + routine("0 6 * * 4"))
	headAt(t, r, infra, files)
	before := r.events.count()
	write := func(cron string) []string {
		t.Helper()
		args := `{"name":"dependency-update","title":"Dependency update","schedule":"` + cron + `","files":"agent"}`
		g.replies = [][]string{{call(1, "routine_write", args)}}
		_, _, tokens := chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"move it"}})
		_, done, _ := chatPost(t, r, ck, s, "/projects/demo-app/chat/proposals/"+tokens[0], nil)
		return done
	}
	equal(t, "the same routine", write("0 6 * * 4"), []string{`tool {"arguments":{"name":"dependency-update","title":"Dependency update",` +
		`"schedule":"0 6 * * 4","files":"agent"},"duration_ms":0,"name":"routine_write","result":"Refused: Nothing changes: projects/demo-app.yaml already holds this."}`,
		`done {}`})
	if len(infra.contents) != 0 || len(infra.pulls) != 0 || r.events.count() != before {
		t.Fatalf("nothing to change committed %+v, opened %+v or recorded %d rows", infra.contents, infra.pulls, r.events.count()-before)
	}
	write("0 7 * * 4")
	equal(t, "the schedule-only change", lastCommit(t, infra), []string{"update factory/projects/demo-app.yaml blob1\n" + original + routine("0 7 * * 4")})
}

// A write with a brief_text moves a routine's brief to its own file: the
// commit writes routines/<project>-<name>.md and deletes the old brief,
// which no other routine names, and the proposal and the result say so.
func TestRoutineWriteMovesItsBrief(t *testing.T) {
	r, g, infra, files := routineRig(t)
	ck, s := r.loggedIn(t)
	original := string(files["projects/demo-app.yaml"])
	files["projects/demo-app.yaml"] = []byte(original + "routines:\n  - name: dependency-update\n    title: Dependency update\n    schedule:\n" +
		"      - cron: 0 6 * * 4\n    files: human\n    brief: routines/deps.md\n")
	files["routines/deps.md"] = []byte("Update them.\n")
	headAt(t, r, infra, files)
	args := `{"name":"dependency-update","title":"Dependency update","schedule":"0 6 * * 4","files":"human","brief_text":"Update them and run the tests."}`
	g.replies = [][]string{{call(1, "routine_write", args)}}
	_, proposed, tokens := chatPost(t, r, ck, s, "/projects/demo-app/chat", url.Values{"message": {"give it a better brief"}})
	equal(t, "the proposal", proposed, []string{`proposal {"arguments":` + args + `,"description":"Write a routine, in place of dependency-update in projects/demo-app.yaml:\n\n` +
		`- name: dependency-update\n  title: Dependency update\n  schedule:\n    - cron: 0 6 * * 4\n  files: human\n  brief: routines/demo-app-dependency-update.md` +
		`\n\nIts brief, routines/demo-app-dependency-update.md:\n\nUpdate them and run the tests.\n\nIts old brief, routines/deps.md, goes.","id":"TOKEN","tool":"routine_write"}`,
		`done {}`})
	_, done, _ := chatPost(t, r, ck, s, "/projects/demo-app/chat/proposals/"+tokens[0], nil)
	equal(t, "Do it", done, ran("routine_write", args, "Pull request #12 changed routine dependency-update in projects/demo-app.yaml, "+
		"wrote its brief routines/demo-app-dependency-update.md and deleted its old brief routines/deps.md. The board shows it once the factory loads the new configuration."))
	equal(t, "the commit", lastCommit(t, infra), []string{
		"update factory/projects/demo-app.yaml blob1\n" + original + "routines:\n  - name: dependency-update\n    title: Dependency update\n    schedule:\n" +
			"      - cron: 0 6 * * 4\n    files: human\n    brief: routines/demo-app-dependency-update.md\n",
		"create factory/routines/demo-app-dependency-update.md\nUpdate them and run the tests.\n",
		"delete factory/routines/deps.md blob1\n"})
}

// A change keeps the comments of the entry it replaces where their keys
// stay: the entry's own, a value's line comment and a key's foot comment.
// A deletion keeps the routines key's comments while a routine stays, and
// every comment of the other entries and of the file.
func TestRoutineEditsKeepTheComments(t *testing.T) {
	const top = "# the project\nrepository: sf/code # its repository\n# the routines\nroutines: # all of them\n"
	const bump = "  # the bump\n  - name: weekly-bump # the bump's name\n    title: Bump\n    schedule:\n      - cron: \"0 6 * * 1\"\n" +
		"    files: human\n    # foot of the bump\n"
	const audit = "  # the audit\n  - name: audit\n    title: Audit\n    schedule:\n      - on: push-main\n    files: report\n"
	const tail = "# foot of the list\n\n# after the routines\ntokens: {read: x}\n"
	raw := []byte(top + bump + audit + tail)
	same, err := editProjectFile(raw, func(*yaml.Node) error { return nil })
	equal(t, "the fixture as the encoder writes it", []any{string(same), err}, []any{string(raw), error(nil)})

	changed := policy.Routine{Name: "weekly-bump", Title: "Weekly bump", Schedule: []policy.Trigger{{Cron: "0 6 * * 1"}}, Files: policy.FilesHuman}
	out, err := editProjectFile(raw, func(doc *yaml.Node) error { _, err := putRoutine(doc, changed); return err })
	equal(t, "the change", []any{string(out), err}, []any{top + "  # the bump\n  - name: weekly-bump # the bump's name\n    title: Weekly bump\n" +
		"    schedule:\n      - cron: 0 6 * * 1\n    files: human\n    # foot of the bump\n" + audit + tail, error(nil)})
	out, err = editProjectFile(raw, func(doc *yaml.Node) error { return dropRoutine(doc, "weekly-bump") })
	equal(t, "the first deleted", []any{string(out), err}, []any{top + audit + tail, error(nil)})
	out, err = editProjectFile(raw, func(doc *yaml.Node) error { return dropRoutine(doc, "audit") })
	equal(t, "the second deleted", []any{string(out), err}, []any{top + bump + tail, error(nil)})
}
