package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// revertMains is newGitRig's demo-app/demo-app with three changes on main
// after base: m1, a card's merge that adds a.go; x, another card's merge
// that writes xText to xFile; m2, a plain commit (a squash merge) that
// edits a.go. f reaches it and the bot's fork through file URLs.
func revertMains(t *testing.T, xFile, xText string) (g *gitRig, f *Forgejo, m1, x, m2 string) {
	t.Helper()
	g = newGitRig(t)
	merge := func(branch, file, text string) string {
		g.git(g.work, "checkout", "-q", "-b", branch, "main")
		g.commit(branch, file, text)
		g.git(g.work, "checkout", "-q", "main")
		g.git(g.work, "merge", "-q", "--no-ff", "-m", "Merge "+branch, branch)
		return g.git(g.work, "rev-parse", "HEAD")
	}
	m1 = merge("card-1", "a.go", "package a\n\nfunc A() int { return 1 }\n")
	x = merge("card-x", xFile, xText)
	m2 = g.commit("card-2", "a.go", "package a\n\nfunc A() int { return 2 }\n")
	g.git(g.work, "push", "-q", g.forgejo, "main")
	f = &Forgejo{BaseURL: "file://" + g.root + "/forgejo", BotUser: "factory-agent", BotPassword: "bot-pw"}
	return g, f, m1, x, m2
}

// forkHas tells whether the bot's fork has branch.
func forkHas(g *gitRig, branch string) bool {
	_, err := git(context.Background(), g.fork, "", "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	return err == nil
}

var revertRepo = Repo{Owner: "demo-app", Name: "demo-app"}

// The group revert reverts the merges newest first whatever order they
// come in: m1 first would conflict, a.go having changed since. main's
// head goes to the salvage branch before, unless it exists.
func TestGroupRevertBranchRevertsNewestFirst(t *testing.T) {
	g, f, m1, _, m2 := revertMains(t, "b.go", "package a\n\nfunc B() int { return 3 }\n")
	shas := []string{m1, m2}
	conflict, err := groupRevertBranch(context.Background(), f, revertRepo, shas, "salvage/p", false, "factory/t-rv")
	if err != nil || conflict != "" {
		t.Fatalf("groupRevertBranch = %q, %v; want no conflict", conflict, err)
	}
	if !slices.Equal(shas, []string{m2, m1}) {
		t.Errorf("shas = %q, want them newest first: %q", shas, []string{m2, m1})
	}
	if got := g.git(g.fork, "rev-parse", "salvage/p"); got != m2 {
		t.Errorf("salvage/p at %s, want main's head %s", got, m2)
	}
	if got := g.git(g.fork, "rev-parse", "factory/t-rv~2"); got != m2 {
		t.Errorf("factory/t-rv~2 is %s, want main's head %s under the two reverts", got, m2)
	}
	for ref, sha := range map[string]string{"factory/t-rv~1": m2, "factory/t-rv": m1} {
		if msg := g.git(g.fork, "log", "-1", "--format=%an %B", ref); !strings.HasPrefix(msg, "factory-agent ") || !strings.Contains(msg, "This reverts commit "+sha) {
			t.Errorf("%s: %q, want the bot's revert of %s", ref, msg, sha)
		}
	}
	if files := g.git(g.fork, "ls-tree", "--name-only", "factory/t-rv"); files != "README\nb.go" {
		t.Errorf("factory/t-rv holds %q, want README and b.go: a.go gone, x kept", files)
	}

	if _, err := groupRevertBranch(context.Background(), f, revertRepo, []string{m2}, "salvage/q", true, "factory/t-rv2"); err != nil {
		t.Fatal(err)
	}
	if forkHas(g, "salvage/q") || !forkHas(g, "factory/t-rv2") {
		t.Errorf("salvage/q pushed %t, factory/t-rv2 %t; want only the revert: the salvage branch exists", forkHas(g, "salvage/q"), forkHas(g, "factory/t-rv2"))
	}
}

// A revert that conflicts stops the group revert at its commit: x changed
// the lines m1 added. The reverts are not pushed; the salvage branch is.
func TestGroupRevertBranchStopsAtAConflict(t *testing.T) {
	g, f, m1, _, m2 := revertMains(t, "a.go", "package a\n\nfunc A() int { return 10 }\n")
	conflict, err := groupRevertBranch(context.Background(), f, revertRepo, []string{m1, m2}, "salvage/p", false, "factory/t-rv")
	if err != nil || conflict != m1 {
		t.Fatalf("groupRevertBranch = %q, %v; want the conflict at m1 %s", conflict, err, m1)
	}
	if forkHas(g, "factory/t-rv") {
		t.Error("factory/t-rv pushed after a conflict")
	}
	if got := g.git(g.fork, "rev-parse", "salvage/p"); got != m2 {
		t.Errorf("salvage/p at %s, want main's head %s", got, m2)
	}
}

// A commit not on main's first-parent history is no merge of main to
// revert: an error, not a conflict, and nothing is pushed.
func TestGroupRevertBranchRefusesACommitOffMain(t *testing.T) {
	g, f, _, _, m2 := revertMains(t, "b.go", "package a\n")
	g.git(g.work, "checkout", "-q", "-b", "elsewhere", "main")
	off := g.commit("elsewhere", "c.go", "package a\n")
	g.git(g.work, "push", "-q", g.forgejo, "elsewhere")
	_, err := groupRevertBranch(context.Background(), f, revertRepo, []string{m2, off}, "", false, "factory/t-rv")
	if want := "commit " + off + " is not on main's first-parent history"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if forkHas(g, "factory/t-rv") {
		t.Error("factory/t-rv pushed")
	}
}

// groupForgejo plays a group revert's calls on sf/repo and the bot's fork
// factory-agent/repo: main's head h1, the fork's branches, the open pull
// requests and the issues and pull requests it creates.
type groupForgejo struct {
	mu       sync.Mutex
	branches map[string]string // the fork's branches: name → head
	open     []map[string]any
	issues   []string // "title\n\nbody" of each issue created
	pulls    []string // "head title\n\nbody" of each pull request created
	noIssue  bool     // POST /issues answers 500
}

func (f *groupForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct{ Head, Title, Body string }
		json.NewDecoder(r.Body).Decode(&body)
		p := r.URL.Path
		switch {
		case r.Method == "GET" && p == "/api/v1/repos/factory-agent/repo":
			io.WriteString(w, `{"fork":true,"private":true,"has_actions":false,"parent":{"full_name":"sf/repo"}}`)
		case r.Method == "GET" && p == "/api/v1/repos/sf/repo/branches/main":
			io.WriteString(w, `{"commit":{"id":"h1"}}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/api/v1/repos/factory-agent/repo/branches/"):
			head, ok := f.branches[strings.TrimPrefix(p, "/api/v1/repos/factory-agent/repo/branches/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
		case r.Method == "GET" && p == "/api/v1/repos/sf/repo/pulls":
			json.NewEncoder(w).Encode(f.open)
		case r.Method == "POST" && p == "/api/v1/repos/sf/repo/pulls":
			f.pulls = append(f.pulls, body.Head+" "+body.Title+"\n\n"+body.Body)
			owner, ref, _ := strings.Cut(body.Head, ":")
			pr := map[string]any{"number": 71, "head": map[string]any{"ref": ref, "repo": map[string]any{"full_name": owner + "/repo"}}}
			f.open = append(f.open, pr)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(pr)
		case r.Method == "POST" && p == "/api/v1/repos/sf/repo/issues" && f.noIssue:
			http.Error(w, "forgejo down", http.StatusInternalServerError)
		case r.Method == "POST" && p == "/api/v1/repos/sf/repo/issues":
			f.issues = append(f.issues, body.Title+"\n\n"+body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d}`, 20+len(f.issues))
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// groupRevertRig is plan p (issue 10) on sf/repo with its merged cards c1
// (issue 14, merge m1) and c2 (issue 15, merge m2), and groupRevertBranch
// swapped for a stub that records each call, with whether p's pause stood
// at it, and orders the merges newest first as git would, m2 being the
// newer; answer is the stub's answer.
type groupRevertRig struct {
	r      *Runner
	store  *fakeStore
	fj     *groupForgejo
	wf     *fakeWorkflows
	plan   Task
	cards  []Task
	calls  []string
	answer func() (string, error)
}

func newGroupRevertRig(t *testing.T) *groupRevertRig {
	t.Helper()
	g := &groupRevertRig{fj: &groupForgejo{}, wf: &fakeWorkflows{}, answer: func() (string, error) { return "", nil }}
	g.plan = Task{ID: "p", Kind: "agent", Track: policy.TrackPlan, Source: "webhook", Repository: "sf/repo", IssueNumber: 10, Status: "running"}
	g.r, _, g.store = newTestRunner(t, g.plan, newFakeKube(), &fakeObjectStore{})
	g.r.forgejo = &Forgejo{BaseURL: g.fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	g.r.cfg.Workflows = g.wf
	for _, c := range []Task{{ID: "c1", IssueNumber: 14, MergeCommit: "m1"}, {ID: "c2", IssueNumber: 15, MergeCommit: "m2"}} {
		c.Kind, c.Track, c.Source, c.Parent, c.Repository, c.Status = "agent", policy.TrackDirect, SourcePlan, "p", "sf/repo", "done"
		if err := g.store.CreateTask(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		g.cards = append(g.cards, c)
	}
	saved := groupRevertBranch
	t.Cleanup(func() { groupRevertBranch = saved })
	groupRevertBranch = func(ctx context.Context, f *Forgejo, repo Repo, shas []string, salvage string, salvageExists bool, branch string) (string, error) {
		pause, paused := g.store.pauses["sf/repo"]
		g.calls = append(g.calls, fmt.Sprintf("%s %q %s %t %s, paused for %s: %t", repo, shas, salvage, salvageExists, branch, pause.RevertTask, paused && pause.Plan == "p"))
		slices.Reverse(shas)
		return g.answer()
	}
	return g
}

func (g *groupRevertRig) revert(t *testing.T) (string, error) {
	t.Helper()
	return g.r.RevertGroup(context.Background(), g.plan, g.cards, "asked by sebastian: the api is wrong")
}

// groupRevertSteps are plan p's group_revert steps, without their time.
func groupRevertSteps(t *testing.T, r *Runner) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range stepsNamed(t, r, "p", "group_revert") {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "time")
		out = append(out, m)
	}
	return out
}

// A plain revert: the pause names the plan, its cards and the revert card
// before the git work, and the card is a factory pull request card on
// main's head for the pull request of the card's branch. A conflict files
// the agent card with the merges newest first and the conflict instead.
func TestRevertGroupFilesTheFactoryCard(t *testing.T) {
	g := newGroupRevertRig(t)
	note, err := g.revert(t)
	if err != nil {
		t.Fatal(err)
	}
	pause := g.store.pauses["sf/repo"]
	id := pause.RevertTask
	if want := (QueuePause{Project: "sf/repo", Since: pause.Since, Plan: "p", Reverts: []string{"c1", "c2"}, RevertTask: id}); !strings.HasPrefix(id, "t-") ||
		time.Since(pause.Since) > time.Minute || !reflect.DeepEqual(pause, want) {
		t.Fatalf("pause = %+v, want the plan's with its cards and the revert card", pause)
	}
	if want := []string{fmt.Sprintf(`sf/repo ["m1" "m2"] salvage/p false factory/%s, paused for %s: true`, id, id)}; !slices.Equal(g.calls, want) {
		t.Errorf("groupRevertBranch calls = %q, want %q", g.calls, want)
	}
	body := "Takes plan #10's merged cards off main, newest first: #15 m2, #14 m1.\n\nWhy: asked by sebastian: the api is wrong\n\n" +
		"The merge queue of sf/repo waits for this revert. Branch salvage/p of the bot's fork keeps main as it was before it."
	title := "Revert plan #10: 2 merges, newest first"
	if want := []string{"factory-agent:factory/" + id + " " + title + "\n\n" + body}; !slices.Equal(g.fj.pulls, want) {
		t.Errorf("pull requests = %q, want %q", g.fj.pulls, want)
	}
	card := g.store.tasks[id]
	if card.Source != SourceRevert || card.SHA != "h1" || card.PRNumber != 71 || card.IssueNumber != 21 || card.RiskClass != "standard" ||
		card.Kind != "agent" || card.Status != "queued" || card.Process != "" || card.Objective != title+"\n\n"+body {
		t.Errorf("card = %+v, want the plain revert's factory pull request card on main's head", card)
	}
	if !slices.Equal(g.wf.started, []string{id}) {
		t.Errorf("started = %q, want the card's run", g.wf.started)
	}
	if want := "Plan #10's 2 merged cards come off main: the merge queue waits for the revert, card #21."; note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
	if steps, want := groupRevertSteps(t, g.r), []map[string]any{{"step": "group_revert", "card": id, "shas": []any{"m2", "m1"}, "salvage": "salvage/p", "conflict": ""}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}

	g = newGroupRevertRig(t)
	g.fj.branches = map[string]string{"salvage/p": "h0"}
	g.answer = func() (string, error) { return "m1", nil }
	if note, err = g.revert(t); err != nil {
		t.Fatal(err)
	}
	id = g.store.pauses["sf/repo"].RevertTask
	if want := []string{fmt.Sprintf(`sf/repo ["m1" "m2"] salvage/p true factory/%s, paused for %s: true`, id, id)}; !slices.Equal(g.calls, want) {
		t.Errorf("groupRevertBranch calls = %q, want %q", g.calls, want)
	}
	objective := "Take plan #10's cards off main: revert these merge commits, newest first, with git revert -m 1 for a merge commit: m2 (#15), m1 (#14). " +
		"The plain revert stopped at m1: resolve each conflict so the later changes on main stay and main builds green. Change nothing else."
	if want := []string{title + "\n\n" + objective}; len(g.fj.pulls) != 0 || !slices.Equal(g.fj.issues, want) {
		t.Errorf("pull requests %q, issues %q; want no pull request and the issue %q", g.fj.pulls, g.fj.issues, want)
	}
	card = g.store.tasks[id]
	if card.Source != SourceGroupRevert || card.SHA != "h1" || card.PRNumber != 0 || card.IssueNumber != 21 || card.Kind != "agent" ||
		card.Status != "queued" || card.Track != policy.TrackDirect || card.Process != "" || card.BudgetEUR <= 0 || card.Objective != title+"\n\n"+objective {
		t.Errorf("card = %+v, want the agent card on main's head", card)
	}
	if want := "Plan #10's 2 merged cards come off main: the merge queue waits for the revert, card #21."; note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
	if steps, want := groupRevertSteps(t, g.r), []map[string]any{{"step": "group_revert", "card": id, "shas": []any{"m2", "m1"}, "salvage": "salvage/p", "conflict": "m1"}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
}

// A call that finds the plan's own pause, a retry's, goes on with its
// revert card: it files t-rv once, and a second call files nothing more
// and answers the same. A failure before the card exists lifts the pause.
func TestRevertGroupResumesItsOwnPause(t *testing.T) {
	g := newGroupRevertRig(t)
	own := QueuePause{Project: "sf/repo", Since: time.Now().Add(-time.Minute), Plan: "p", Reverts: []string{"c2", "c1"}, RevertTask: "t-rv"}
	g.store.pauses = map[string]QueuePause{"sf/repo": own}
	want := "Plan #10's 2 merged cards come off main: the merge queue waits for the revert, card #21."
	for i := range 2 {
		if note, err := g.revert(t); err != nil || note != want {
			t.Fatalf("call %d: %q, %v; want %q", i+1, note, err, want)
		}
	}
	if card := g.store.tasks["t-rv"]; card.Source != SourceRevert || card.IssueNumber != 21 {
		t.Errorf("card t-rv = %+v, want the plain revert's", card)
	}
	if len(g.calls) != 1 || len(g.fj.pulls) != 1 || len(g.fj.issues) != 1 || !slices.Equal(g.wf.started, []string{"t-rv"}) {
		t.Errorf("reverts %q, pull requests %q, issues %q, started %q; want one of each", g.calls, g.fj.pulls, g.fj.issues, g.wf.started)
	}
	if p := g.store.pauses["sf/repo"]; !reflect.DeepEqual(p, own) {
		t.Errorf("pause = %+v, want it as it stood", p)
	}

	g = newGroupRevertRig(t)
	g.store.pauses = map[string]QueuePause{"sf/repo": own}
	g.answer = func() (string, error) { return "", errors.New("git push: the fork is gone") }
	if _, err := g.revert(t); err == nil || !strings.Contains(err.Error(), "the fork is gone") {
		t.Fatalf("error = %v, want the revert's", err)
	}
	if p, ok := g.store.pauses["sf/repo"]; ok {
		t.Errorf("pause = %+v, want it lifted", p)
	}
	if _, ok := g.store.tasks["t-rv"]; ok || len(g.fj.issues) != 0 {
		t.Errorf("card t-rv filed, issues %q; want none", g.fj.issues)
	}
}

// A plain revert whose card fails to file answers the error and lifts
// the pause.
func TestRevertGroupFailedFilingLiftsThePause(t *testing.T) {
	g := newGroupRevertRig(t)
	g.fj.noIssue = true
	note, err := g.revert(t)
	if err == nil || !strings.Contains(err.Error(), "forgejo down") {
		t.Fatalf("RevertGroup = %q, %v; want the filing's error", note, err)
	}
	if p, ok := g.store.pauses["sf/repo"]; ok {
		t.Errorf("pause = %+v, want it lifted", p)
	}
	if steps := groupRevertSteps(t, g.r); len(steps) != 0 {
		t.Errorf("steps = %v, want none", steps)
	}
}

// readFailing is a store whose Task reads fail where fail says, by their
// number from 1.
type readFailing struct {
	*fakeStore
	reads int
	fail  func(n int) bool
}

func (s *readFailing) Task(ctx context.Context, id string) (Task, bool, error) {
	s.reads++
	if s.fail(s.reads) {
		return Task{}, false, errors.New("the database is gone")
	}
	return s.fakeStore.Task(ctx, id)
}

// A call that fails lifts the pause only when a read past the error finds
// no row of its card: a card that exists holds the pause, and so does a
// pause whose card no read can tell.
func TestRevertGroupLiftsThePauseOnlyWithoutItsCard(t *testing.T) {
	own := QueuePause{Project: "sf/repo", Since: time.Now(), Plan: "p", Reverts: []string{"c1", "c2"}, RevertTask: "t-rv"}
	for name, tc := range map[string]struct {
		pause  *QueuePause
		fail   func(n int) bool
		lifted bool
	}{
		"the card's read fails":       {nil, func(n int) bool { return n == 1 }, true},
		"the read after it fails too": {nil, func(n int) bool { return n <= 2 }, false},
		"the card exists":             {&own, func(n int) bool { return n == 2 }, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGroupRevertRig(t)
			if tc.pause != nil {
				g.store.pauses = map[string]QueuePause{"sf/repo": *tc.pause}
				if err := g.store.CreateTask(context.Background(), Task{ID: "t-rv", Kind: "agent", Track: policy.TrackDirect, Source: SourceRevert, Repository: "sf/repo", IssueNumber: 21, Status: "queued"}); err != nil {
					t.Fatal(err)
				}
			}
			g.r.store = &readFailing{fakeStore: g.store, fail: tc.fail}
			if _, err := g.revert(t); err == nil || !strings.Contains(err.Error(), "the database is gone") {
				t.Fatalf("error = %v, want the read's", err)
			}
			if p, ok := g.store.pauses["sf/repo"]; ok == tc.lifted || ok && p.Plan != "p" {
				t.Errorf("pause = %+v (%t), want it lifted %t", p, ok, tc.lifted)
			}
			if len(g.calls) != 0 || len(g.fj.issues) != 0 {
				t.Errorf("reverts %q, issues %q; want none", g.calls, g.fj.issues)
			}
		})
	}
}

// Another pause refuses with its cause and leaves it standing: main red
// since its time in UTC, or a plan's revert by the plan's issue. A pause
// of the same plan is another revert when it takes other cards off main,
// or when its card ended.
func TestRevertGroupRefusals(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	ownRefusal := "The merge queue of sf/repo is paused: plan #10 is being taken off main. Replan without taking work off main, or wait until the queue goes on."
	for name, tc := range map[string]struct {
		pause QueuePause
		card  string // the status of the pause's card t-rv; "" for none
		want  string
	}{
		"main red": {QueuePause{Project: "sf/repo", RunID: 58, SHA: "c1c1", Since: time.Date(2026, 11, 7, 11, 0, 0, 0, cet), FixTask: "fix1"}, "",
			"The merge queue of sf/repo is paused: main is red since 2026-11-07T10:00:00Z. Replan without taking work off main, or wait until the queue goes on."},
		"another plan": {QueuePause{Project: "sf/repo", Since: time.Now(), Plan: "q", Reverts: []string{"d1"}, RevertTask: "t-q"}, "",
			"The merge queue of sf/repo is paused: plan #12 is being taken off main. Replan without taking work off main, or wait until the queue goes on."},
		"the plan's revert of other cards": {QueuePause{Project: "sf/repo", Since: time.Now(), Plan: "p", Reverts: []string{"c1"}, RevertTask: "t-rv"}, "", ownRefusal},
		"the plan's revert that ended":     {QueuePause{Project: "sf/repo", Since: time.Now(), Plan: "p", Reverts: []string{"c1", "c2"}, RevertTask: "t-rv"}, "done", ownRefusal},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGroupRevertRig(t)
			if err := g.store.CreateTask(context.Background(), Task{ID: "q", Kind: "agent", Track: policy.TrackPlan, Repository: "sf/repo", IssueNumber: 12, Status: "done"}); err != nil {
				t.Fatal(err)
			}
			if tc.card != "" {
				if err := g.store.CreateTask(context.Background(), Task{ID: "t-rv", Kind: "agent", Track: policy.TrackDirect, Source: SourceRevert, Repository: "sf/repo", IssueNumber: 21, Status: tc.card}); err != nil {
					t.Fatal(err)
				}
			}
			g.store.pauses = map[string]QueuePause{"sf/repo": tc.pause}
			_, err := g.revert(t)
			var no ErrNoReplan
			if !errors.As(err, &no) || string(no) != tc.want {
				t.Fatalf("error = %v, want the refusal %q", err, tc.want)
			}
			if p := g.store.pauses["sf/repo"]; !reflect.DeepEqual(p, tc.pause) || len(g.calls) != 0 || len(g.fj.issues) != 0 {
				t.Errorf("pause %+v, reverts %q, issues %q; want the pause as it stood and nothing done", p, g.calls, g.fj.issues)
			}
		})
	}
}

// A group revert's agent card runs outside the work bound: it waits for no
// slot while the queue stands paused, and is neither cut nor replanned.
func TestGroupRevertCardIsUnbound(t *testing.T) {
	c := Task{Kind: "agent", Track: policy.TrackDirect, Source: SourceGroupRevert, Status: "running"}
	if !c.Unbound() {
		t.Error("the group revert's card is in the work bound")
	}
	if cuttable(c, sessionSpec{planTrack: true}) {
		t.Error("the group revert's card is cuttable")
	}
	if MayReplan(c, true) {
		t.Error("the group revert's card offers Replan")
	}
}
