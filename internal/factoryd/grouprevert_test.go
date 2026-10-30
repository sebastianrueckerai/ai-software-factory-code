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
// requests and the issues and pull requests it creates; and a card's end:
// the comments, the issues closed and opened again, every pull request
// unmerged.
type groupForgejo struct {
	mu           sync.Mutex
	branches     map[string]string // the fork's branches: name → head
	open         []map[string]any
	issues       []string // "title\n\nbody" of each issue created
	pulls        []string // "head title\n\nbody" of each pull request created
	noIssue      bool     // POST /issues answers 500
	closed       []int64  // the pull requests closed
	comments     []string // "#n body" of each comment
	issuesClosed []int64
	reopened     []int64
	refuseReopen bool // PATCH /issues/{n} to open answers 403
}

// commentsOn are the bodies of the comments on issue n.
func (f *groupForgejo) commentsOn(n int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		if body, ok := strings.CutPrefix(c, fmt.Sprintf("#%d ", n)); ok {
			out = append(out, body)
		}
	}
	return out
}

func (f *groupForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Head, Title, Body, State string
			NewBranch                string `json:"new_branch_name"`
			OldBranch                string `json:"old_branch_name"`
		}
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
		case r.Method == "PATCH" && strings.HasPrefix(p, "/api/v1/repos/sf/repo/pulls/"):
			var n int64
			fmt.Sscanf(strings.TrimPrefix(p, "/api/v1/repos/sf/repo/pulls/"), "%d", &n)
			f.closed = append(f.closed, n)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/api/v1/repos/sf/repo/pulls/"):
			var n int64
			fmt.Sscanf(strings.TrimPrefix(p, "/api/v1/repos/sf/repo/pulls/"), "%d", &n)
			state := "open"
			if slices.Contains(f.closed, n) {
				state = "closed"
			}
			fmt.Fprintf(w, `{"number":%d,"state":%q,"merged":false}`, n, state)
		case r.Method == "GET" && p == "/api/v1/users/factory-agent/tokens",
			r.Method == "GET" && strings.HasSuffix(p, "/labels"):
			io.WriteString(w, `[]`)
		case strings.HasPrefix(p, "/api/v1/repos/sf/repo/issues/") && strings.HasSuffix(p, "/comments"):
			var n int64
			fmt.Sscanf(strings.TrimPrefix(p, "/api/v1/repos/sf/repo/issues/"), "%d", &n)
			if r.Method == "GET" {
				list := []map[string]any{}
				for _, c := range f.comments {
					if b, ok := strings.CutPrefix(c, fmt.Sprintf("#%d ", n)); ok {
						list = append(list, map[string]any{"body": b, "user": map[string]string{"login": "factory-agent"}})
					}
				}
				json.NewEncoder(w).Encode(list)
				return
			}
			f.comments = append(f.comments, fmt.Sprintf("#%d %s", n, body.Body))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 800+len(f.comments))
		case strings.HasPrefix(p, "/api/v1/repos/sf/repo/issues/"):
			var n int64
			fmt.Sscanf(strings.TrimPrefix(p, "/api/v1/repos/sf/repo/issues/"), "%d", &n)
			if r.Method == "GET" {
				state := "open"
				if slices.Contains(f.issuesClosed, n) && !slices.Contains(f.reopened, n) {
					state = "closed"
				}
				fmt.Fprintf(w, `{"state":%q}`, state)
				return
			}
			if body.State == "open" && f.refuseReopen {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if body.State == "open" {
				f.reopened = append(f.reopened, n)
			} else {
				f.issuesClosed = append(f.issuesClosed, n)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == "POST" && p == "/api/v1/repos/factory-agent/repo/branches":
			if f.branches == nil {
				f.branches = map[string]string{}
			}
			f.branches[body.NewBranch] = f.branches[body.OldBranch]
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == "DELETE" && strings.HasPrefix(p, "/api/v1/repos/factory-agent/repo/branches/"):
			delete(f.branches, strings.TrimPrefix(p, "/api/v1/repos/factory-agent/repo/branches/"))
			w.WriteHeader(http.StatusNoContent)
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

// groupSteps are plan p's steps named step, without their time.
func groupSteps(t *testing.T, r *Runner, step string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range stepsNamed(t, r, "p", step) {
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
	if steps, want := groupSteps(t, g.r, "group_revert"), []map[string]any{{"step": "group_revert", "card": id, "shas": []any{"m2", "m1"}, "salvage": "salvage/p", "conflict": ""}}; !reflect.DeepEqual(steps, want) {
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
	if steps, want := groupSteps(t, g.r, "group_revert"), []map[string]any{{"step": "group_revert", "card": id, "shas": []any{"m2", "m1"}, "salvage": "salvage/p", "conflict": "m1"}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
}

// One card reads in the singular: the answer and the pull request.
func TestRevertGroupOfOneCard(t *testing.T) {
	g := newGroupRevertRig(t)
	note, err := g.r.RevertGroup(context.Background(), g.plan, g.cards[:1], "plan revision 3 drops it")
	if want := "Plan #10's 1 merged card comes off main: the merge queue waits for the revert, card #21."; err != nil || note != want {
		t.Fatalf("RevertGroup = %q, %v; want %q", note, err, want)
	}
	id := g.store.pauses["sf/repo"].RevertTask
	want := "factory-agent:factory/" + id + " Revert plan #10: 1 merge, newest first\n\nTakes plan #10's merged cards off main, newest first: #14 m1.\n\n" +
		"Why: plan revision 3 drops it\n\nThe merge queue of sf/repo waits for this revert. Branch salvage/p of the bot's fork keeps main as it was before it."
	if !slices.Equal(g.fj.pulls, []string{want}) {
		t.Errorf("pull requests = %q, want %q", g.fj.pulls, want)
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

// A plain revert whose card fails to file answers the error, closes the
// pull request the call opened and lifts the pause. A pull request an
// earlier call opened stays open. The group_revert step, written right
// after git, stays.
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
	if len(g.fj.pulls) != 1 || !slices.Equal(g.fj.closed, []int64{71}) {
		t.Errorf("pull requests %q, closed %v; want the one opened, closed", g.fj.pulls, g.fj.closed)
	}
	head, _, _ := strings.Cut(strings.Join(g.fj.pulls, ""), " ")
	id := strings.TrimPrefix(head, "factory-agent:factory/")
	if steps, want := groupSteps(t, g.r, "group_revert"), []map[string]any{{"step": "group_revert", "card": id, "shas": []any{"m2", "m1"}, "salvage": "salvage/p", "conflict": ""}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}

	g = newGroupRevertRig(t)
	g.fj.noIssue = true
	g.store.pauses = map[string]QueuePause{"sf/repo": {Project: "sf/repo", Since: time.Now(), Plan: "p", Reverts: []string{"c1", "c2"}, RevertTask: "t-rv"}}
	g.fj.open = []map[string]any{{"number": 70, "head": map[string]any{"ref": "factory/t-rv", "repo": map[string]any{"full_name": "factory-agent/repo"}}}}
	if _, err := g.revert(t); err == nil {
		t.Fatal("RevertGroup answered no error")
	}
	if _, ok := g.store.pauses["sf/repo"]; ok || len(g.fj.pulls) != 0 || len(g.fj.closed) != 0 {
		t.Errorf("paused %t, pull requests %q, closed %v; want the pause lifted and pull request #70 reused and left open", ok, g.fj.pulls, g.fj.closed)
	}
	if steps, want := groupSteps(t, g.r, "group_revert"), []map[string]any{{"step": "group_revert", "card": "t-rv", "shas": []any{"m2", "m1"}, "salvage": "salvage/p", "conflict": ""}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
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

// revertEndRig is newGroupRevertRig with plan p done, its replan waiting
// with off_main, and its pause on the plain revert card t-rv (issue 20,
// pull request 70 of factory/t-rv, head rv1, on main's head h1), which
// takes c1 and c2 off main; w1 waits at the head of sf/repo's merge queue.
// A card's end reaches fj, its completion's too.
func revertEndRig(t *testing.T) *groupRevertRig {
	t.Helper()
	g := newGroupRevertRig(t)
	g.r.completion.Forgejo = g.r.forgejo
	ctx := context.Background()
	p := g.store.tasks["p"]
	p.Status = "done"
	g.store.tasks["p"] = p
	if err := g.r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanPerson, "by": "sebastian", "comment": "the api is wrong", "revision": 1, "off_main": true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Task{
		{ID: "t-rv", Source: SourceRevert, IssueNumber: 20, PRNumber: 70, SHA: "h1", Status: "in-review"},
		{ID: "w1", Source: "webhook", IssueNumber: 30, PRNumber: 75, SHA: "h1", Status: "in-review"},
	} {
		c.Kind, c.Track, c.Repository = "agent", policy.TrackDirect, "sf/repo"
		if err := g.store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	w1 := g.store.tasks["w1"]
	w1.Gate = GateMergeQueue
	g.store.tasks["w1"] = w1
	g.store.queue = append(g.store.queue, queueEntry{project: "sf/repo", task: "w1", at: time.Now()})
	g.store.pauses = map[string]QueuePause{"sf/repo": {Project: "sf/repo", Since: time.Now().Add(-time.Hour), Plan: "p", Reverts: []string{"c1", "c2"}, RevertTask: "t-rv"}}
	g.fj.branches = map[string]string{"factory/t-rv": "rv1"}
	return g
}

// landed sets t-rv's merge commit, as the merge records it.
func (g *groupRevertRig) landed() {
	rv := g.store.tasks["t-rv"]
	rv.MergeCommit = "rv-merge"
	g.store.tasks["t-rv"] = rv
}

// The plan's revert card that merged took its cards off main: each is
// marked reverted, the pause goes, the queue's waiting heads wake, and the
// replan that waited for the revert sends the plan to its planner. The
// plan gets no comment.
func TestRevertMergedMarksTheCardsAndGoesOn(t *testing.T) {
	g := revertEndRig(t)
	g.landed()
	before := time.Now()
	if err := g.r.Finish(context.Background(), "t-rv", "done", "pull request merged by factory-merge on the merge rule auto"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		if at := g.store.tasks[id].RevertedAt; at == nil || at.Before(before) || time.Since(*at) > time.Minute {
			t.Errorf("%s reverted at %v, want now", id, at)
		}
	}
	if p, ok := g.store.pauses["sf/repo"]; ok {
		t.Errorf("pause = %+v, want it gone", p)
	}
	if !slices.Contains(g.wf.signals, "w1 queue_turn  #0: ") {
		t.Errorf("signals = %q, want the waiting head woken", g.wf.signals)
	}
	if p := g.store.tasks["p"]; p.Status != "queued" || !reflect.DeepEqual(g.wf.reopens, []reopenCall{{taskID: "p"}}) {
		t.Errorf("plan %s, reopens %+v; want it queued and its run reopened", p.Status, g.wf.reopens)
	}
	if steps, want := groupSteps(t, g.r, "group_reverted"), []map[string]any{{"step": "group_reverted", "card": "t-rv", "cards": []any{"c1", "c2"}}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
	if c := g.fj.commentsOn(10); len(c) != 0 {
		t.Errorf("plan comments %q, want none", c)
	}
}

// The plan's revert card that ends without its merge leaves the cards on
// main: none is marked, the pause goes, the plan is told once, and the
// replan that waited for the revert sends the plan to its planner all the
// same. The hook again, as a heal after a failed clear would run it, says
// nothing more.
func TestClosedRevertLeavesTheCardsOnMain(t *testing.T) {
	g := revertEndRig(t)
	ctx := context.Background()
	pause := g.store.pauses["sf/repo"]
	if err := g.r.Finish(ctx, "t-rv", "closed", "closed by sebastian"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		if at := g.store.tasks[id].RevertedAt; at != nil {
			t.Errorf("%s reverted at %v, want it on main", id, at)
		}
	}
	if p, ok := g.store.pauses["sf/repo"]; ok {
		t.Errorf("pause = %+v, want it gone", p)
	}
	if !slices.Contains(g.wf.signals, "w1 queue_turn  #0: ") {
		t.Errorf("signals = %q, want the waiting head woken", g.wf.signals)
	}
	if p := g.store.tasks["p"]; p.Status != "queued" || !reflect.DeepEqual(g.wf.reopens, []reopenCall{{taskID: "p"}}) {
		t.Errorf("plan %s, reopens %+v; want it queued and its run reopened", p.Status, g.wf.reopens)
	}
	want := []string{"factoryd: the revert of plan #10 did not land (card #20 ended closed): its cards stay on main."}
	if c := g.fj.commentsOn(10); !slices.Equal(c, want) {
		t.Errorf("plan comments %q, want %q", c, want)
	}
	if steps, want := groupSteps(t, g.r, "group_revert_ended"), []map[string]any{{"step": "group_revert_ended", "card": "t-rv", "state": "closed"}}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}

	g.store.pauses["sf/repo"] = pause
	g.r.revertEnded(ctx, g.store.tasks["t-rv"], "closed", false)
	if c := g.fj.commentsOn(10); !slices.Equal(c, want) {
		t.Errorf("plan comments after the hook again %q, want %q", c, want)
	}
	if p, ok := g.store.pauses["sf/repo"]; ok {
		t.Errorf("pause = %+v, want it gone again", p)
	}
}

// finishedAgo is a finish stamp d before now.
func finishedAgo(d time.Duration) *time.Time {
	at := time.Now().Add(-d)
	return &at
}

// The minute pass heals a lost hook: a pause whose revert card merged
// two minutes ago without its end's hook marks the cards, goes and wakes
// the heads; a second pass changes nothing. A pause whose card still runs
// is left alone, and so is one whose card ended within the minute the
// hook owns. One whose card failed goes with the plan told. The end of a
// main-red pause's revert card leaves that pause to main's runs.
func TestRevertPausePassHeals(t *testing.T) {
	g := revertEndRig(t)
	ctx := context.Background()
	g.landed()
	rv := g.store.tasks["t-rv"]
	rv.Status, rv.FinishedAt = "merged", finishedAgo(2*time.Minute)
	g.store.tasks["t-rv"] = rv
	running := QueuePause{Project: "sf/other", Since: time.Now().Add(-time.Hour), Plan: "q", Reverts: []string{"d1"}, RevertTask: "t-q"}
	g.store.pauses["sf/other"] = running
	if err := g.store.CreateTask(ctx, Task{ID: "t-q", Kind: "agent", Track: policy.TrackDirect, Source: SourceGroupRevert, Repository: "sf/other", IssueNumber: 5, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	g.r.RevertPausePass(ctx)
	for _, id := range []string{"c1", "c2"} {
		if g.store.tasks[id].RevertedAt == nil {
			t.Errorf("%s not marked reverted", id)
		}
	}
	if p, ok := g.store.pauses["sf/repo"]; ok {
		t.Errorf("pause = %+v, want it gone", p)
	}
	if p := g.store.pauses["sf/other"]; !reflect.DeepEqual(p, running) {
		t.Errorf("sf/other's pause = %+v, want it as it stood", p)
	}
	if !slices.Contains(g.wf.signals, "w1 queue_turn  #0: ") {
		t.Errorf("signals = %q, want the waiting head woken", g.wf.signals)
	}
	if len(g.groupRevertEnds(t)) != 1 || len(g.wf.reopens) != 1 {
		t.Fatalf("steps %v, reopens %+v; want one of each", g.groupRevertEnds(t), g.wf.reopens)
	}
	signals := len(g.wf.signals)
	g.r.RevertPausePass(ctx)
	if len(g.groupRevertEnds(t)) != 1 || len(g.wf.reopens) != 1 || len(g.wf.signals) != signals || len(g.store.pauses) != 1 {
		t.Errorf("after a second pass: steps %v, reopens %+v, signals %q, pauses %+v; want nothing new", g.groupRevertEnds(t), g.wf.reopens, g.wf.signals, g.store.pauses)
	}

	g = revertEndRig(t)
	rv = g.store.tasks["t-rv"]
	rv.Status, rv.FinishedAt = "failed", finishedAgo(30*time.Second)
	g.store.tasks["t-rv"] = rv
	pause := g.store.pauses["sf/repo"]
	g.r.RevertPausePass(ctx)
	if p := g.store.pauses["sf/repo"]; !reflect.DeepEqual(p, pause) || len(g.fj.commentsOn(10)) != 0 || len(g.groupRevertEnds(t)) != 0 {
		t.Errorf("pause %+v, plan comments %q, steps %v; want the card's last minute left to its hook", p, g.fj.commentsOn(10), g.groupRevertEnds(t))
	}
	rv.FinishedAt = finishedAgo(2 * time.Minute)
	g.store.tasks["t-rv"] = rv
	g.r.RevertPausePass(ctx)
	want := []string{"factoryd: the revert of plan #10 did not land (card #20 ended failed): its cards stay on main."}
	if _, ok := g.store.pauses["sf/repo"]; ok || !slices.Equal(g.fj.commentsOn(10), want) || g.store.tasks["c1"].RevertedAt != nil {
		t.Errorf("paused %t, plan comments %q, c1 reverted at %v; want the pause gone, %q, no mark", ok, g.fj.commentsOn(10), g.store.tasks["c1"].RevertedAt, want)
	}

	g = revertEndRig(t)
	g.landed()
	mainRed := QueuePause{Project: "sf/repo", RunID: 58, SHA: "c1c1", Since: time.Now().Add(-3 * time.Hour), FixTask: "fix1", RevertTask: "t-rv"}
	g.store.pauses = map[string]QueuePause{"sf/repo": mainRed}
	if err := g.r.Finish(ctx, "t-rv", "done", "pull request merged by factory-merge on the merge rule auto"); err != nil {
		t.Fatal(err)
	}
	g.r.RevertPausePass(ctx)
	if p := g.store.pauses["sf/repo"]; !reflect.DeepEqual(p, mainRed) || g.store.tasks["c1"].RevertedAt != nil || len(g.groupRevertEnds(t)) != 0 || len(g.wf.reopens) != 0 {
		t.Errorf("pause %+v, c1 reverted at %v, steps %v, reopens %+v; want the main-red pause as it stood and nothing else", p, g.store.tasks["c1"].RevertedAt, g.groupRevertEnds(t), g.wf.reopens)
	}
}

// groupRevertEnds are plan p's group_reverted and group_revert_ended
// steps.
func (g *groupRevertRig) groupRevertEnds(t *testing.T) []map[string]any {
	t.Helper()
	return append(groupSteps(t, g.r, "group_reverted"), groupSteps(t, g.r, "group_revert_ended")...)
}

// A pause whose revert card has no row 10 minutes after it began lost its
// filing: the pass files the card again under the pause's own id, the
// heal's reason its why, and the pause stays on it. Before that the pass
// waits, as RevertGroup may still run.
func TestRevertPausePassFilesALostRevertCard(t *testing.T) {
	g := newGroupRevertRig(t)
	ctx := context.Background()
	lost := QueuePause{Project: "sf/repo", Since: time.Now().Add(-9 * time.Minute), Plan: "p", Reverts: []string{"c1", "c2"}, RevertTask: "t-lost"}
	g.store.pauses = map[string]QueuePause{"sf/repo": lost}
	g.r.RevertPausePass(ctx)
	if len(g.calls) != 0 || len(g.fj.pulls) != 0 || len(g.fj.issues) != 0 {
		t.Fatalf("reverts %q, pull requests %q, issues %q; want none within 10 minutes", g.calls, g.fj.pulls, g.fj.issues)
	}

	lost.Since = time.Now().Add(-11 * time.Minute)
	g.store.pauses["sf/repo"] = lost
	for range 2 {
		g.r.RevertPausePass(ctx)
	}
	if want := []string{`sf/repo ["m1" "m2"] salvage/p false factory/t-lost, paused for t-lost: true`}; !slices.Equal(g.calls, want) {
		t.Errorf("groupRevertBranch calls = %q, want %q", g.calls, want)
	}
	want := "factory-agent:factory/t-lost Revert plan #10: 2 merges, newest first\n\nTakes plan #10's merged cards off main, newest first: #15 m2, #14 m1.\n\n" +
		"Why: factoryd files the revert's card again: its first filing did not finish\n\n" +
		"The merge queue of sf/repo waits for this revert. Branch salvage/p of the bot's fork keeps main as it was before it."
	if !slices.Equal(g.fj.pulls, []string{want}) {
		t.Errorf("pull requests = %q, want %q", g.fj.pulls, want)
	}
	if card := g.store.tasks["t-lost"]; card.Source != SourceRevert || card.IssueNumber != 21 || card.PRNumber != 71 || len(g.fj.issues) != 1 {
		t.Errorf("card t-lost = %+v, issues %q; want the plain revert's card, filed once", card, g.fj.issues)
	}
	if p := g.store.pauses["sf/repo"]; !reflect.DeepEqual(p, lost) {
		t.Errorf("pause = %+v, want it as it stood", p)
	}
}

// The group_revert step holds the merges' order a red plain revert's
// hand-over needs, so a step that fails to write fails the revert before
// anything files: the pause lifts, and no pull request or card is left.
func TestRevertGroupFailsWhenItsStepFails(t *testing.T) {
	g := newGroupRevertRig(t)
	g.r.timeline.Objects = &stepFails{fakeObjectStore: g.r.timeline.Objects.(*fakeObjectStore), task: "p", step: "group_revert", fails: 1}
	if _, err := g.revert(t); err == nil || !strings.Contains(err.Error(), "the object store is down") {
		t.Fatalf("error = %v, want the step's", err)
	}
	if p, ok := g.store.pauses["sf/repo"]; ok || len(g.fj.pulls) != 0 || len(g.fj.issues) != 0 || len(g.calls) != 1 {
		t.Errorf("pause %+v, pull requests %q, issues %q, reverts %q; want the pause lifted after one revert and nothing filed", p, g.fj.pulls, g.fj.issues, g.calls)
	}
}

// staleRead is a store whose QueuePause answers the pause as it stood
// before another took its place.
type staleRead struct {
	*fakeStore
	stale QueuePause
}

func (s *staleRead) QueuePause(ctx context.Context, project string) (QueuePause, bool, error) {
	return s.stale, true, nil
}

// The end of a revert clears only its own pause: a pause another revert
// set in the gap after the read (the hook and the minute pass both ending
// the card, one before the other) stays.
func TestRevertEndKeepsAPauseThatTookItsPlace(t *testing.T) {
	g := revertEndRig(t)
	g.landed()
	newer := QueuePause{Project: "sf/repo", Since: time.Now(), Plan: "q", Reverts: []string{"d1"}, RevertTask: "t-q"}
	g.r.store = &staleRead{fakeStore: g.store, stale: g.store.pauses["sf/repo"]}
	g.store.pauses["sf/repo"] = newer
	g.r.revertEnded(context.Background(), g.store.tasks["t-rv"], "done", true)
	if p := g.store.pauses["sf/repo"]; !reflect.DeepEqual(p, newer) {
		t.Errorf("pause = %+v, want the newer one kept", p)
	}
}

// A reopen of the plan that Forgejo refuses (its closed issue stays shut)
// is logged and passed, as the comment's refusal is: no retry changes it,
// and the queue must not stay paused for it.
func TestRevertEndPassesARefusedReopen(t *testing.T) {
	g := revertEndRig(t)
	g.landed()
	g.fj.issuesClosed, g.fj.refuseReopen = []int64{10}, true
	if err := g.r.Finish(context.Background(), "t-rv", "done", "pull request merged by factory-merge on the merge rule auto"); err != nil {
		t.Fatal(err)
	}
	if p, ok := g.store.pauses["sf/repo"]; ok || g.store.tasks["c1"].RevertedAt == nil || len(groupSteps(t, g.r, "group_reverted")) != 1 {
		t.Errorf("pause %+v (%t), c1 reverted at %v, steps %v; want the pause gone, the cards marked and the step", p, ok, g.store.tasks["c1"].RevertedAt, groupSteps(t, g.r, "group_reverted"))
	}
	if !slices.Contains(g.wf.signals, "w1 queue_turn  #0: ") {
		t.Errorf("signals = %q, want the waiting head woken", g.wf.signals)
	}
}
