package factoryd

// The push rules factoryd holds for both pushes (pushrules.go) — the
// worker's end push calls them through its aliases — and the interim
// push of the session's work in progress that factoryd itself makes at
// a check-in (wippush.go). The rules' proofs run on real git
// repositories: the workflow-directory rule's refusal names the
// directory and the action to take, the migration-naming rule refuses
// the next number, and the line-bound rule refuses a diff no merge can
// fit by name, passes a fitting diff through, and finishes a sync as
// the merge the rule makes with the pusher's own message. The interim
// push's proofs run against a seeded fork and a pod endpoint that ships
// real bundles.

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// wipGit is the git the push rules' tests run: plain git under the
// test's environment, stdout alone on success.
type wipGit struct{}

func (wipGit) Git(dir string, args ...string) (string, error) {
	return wipGitRun(dir, nil, args...)
}

func (wipGit) RunGit(dir string, env []string, args ...string) (string, error) {
	return wipGitRun(dir, env, args...)
}

func wipGitRun(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204 -- the test's own fixed git subcommands
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append([]string{
		"PATH=/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// wipRun seeds git for the rules' tests and fails the test on any error.
func wipRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ruleIdentity is the commit identity the tests' merge commits carry.
var ruleIdentity = []string{
	"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
	"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
}

// wipCommit commits the work tree of dir as one commit and answers its
// id.
func wipCommit(t *testing.T, dir, message string) string {
	t.Helper()
	wipRun(t, "-C", dir, "add", "-A")
	wipRun(t, "-C", dir, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "-q", "-m", message)
	return wipRun(t, "-C", dir, "rev-parse", "HEAD")
}

// The moved workflow-directory rule refuses a push whose .forgejo/ tree
// differs from the base's with the sentence a refused interim push
// delivers to the agent: it names the directory and the action — revert
// the change; the rule lets no agent change it. A case variant beside
// the directory counts as the directory; a change elsewhere passes.
func TestPushRuleRefusesAWorkflowDirChangeWithTheAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, tc := range []struct {
		name       string
		agent      func(dir string)
		wantRefuse string // "" = the rule passes
	}{
		{name: "edit-under-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte("gate: changed\n"), 0o644)
		}},
		{name: "case-variant-beside-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			os.MkdirAll(filepath.Join(dir, ".FORGEJO"), 0o755)
			os.WriteFile(filepath.Join(dir, ".FORGEJO", "x.yml"), []byte("gate: true\n"), 0o644)
		}},
		{name: "change-elsewhere-passes", agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			wipRun(t, "init", "-q", "-b", "main", dir)
			os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
			os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte("gate: true\n"), 0o644)
			base := wipCommit(t, dir, "base")
			tc.agent(dir)
			head := wipCommit(t, dir, "head")

			err := RefuseWorkflowDirChange(wipGit{}, dir, base, head)
			if tc.wantRefuse == "" {
				if err != nil {
					t.Fatalf("RefuseWorkflowDirChange = %v, want the rule to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RefuseWorkflowDirChange passed, want the refusal")
			}
			for _, want := range []string{tc.wantRefuse + "/", "differ", "revert the workflow directory change", "the rule lets no agent change it"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not carry %q", err, want)
				}
			}
		})
	}
}

// The moved migration-naming rule refuses the next number under a
// migration directory glob, naming the file, and passes a timestamped
// name.
func TestPushRuleRefusesAMisnamedMigration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	wipRun(t, "init", "-q", "-b", "main", dir)
	os.MkdirAll(filepath.Join(dir, "internal", "policy", "migrations"), 0o755)
	os.WriteFile(filepath.Join(dir, "internal", "policy", "migrations", "20260102030405_seed.sql"), []byte("create table x;\n"), 0o644)
	base := wipCommit(t, dir, "base")

	os.WriteFile(filepath.Join(dir, "internal", "policy", "migrations", "0017_add_users.sql"), []byte("alter table x;\n"), 0o644)
	bad := wipCommit(t, dir, "next number")
	err := RefuseMisnamedMigrations(wipGit{}, []string{"internal/policy/migrations/**"}, dir, base, bad)
	if err == nil || !strings.Contains(err.Error(), "internal/policy/migrations/0017_add_users.sql") {
		t.Fatalf("RefuseMisnamedMigrations = %v, want the next-number refusal naming the file", err)
	}

	os.Remove(filepath.Join(dir, "internal", "policy", "migrations", "0017_add_users.sql"))
	os.WriteFile(filepath.Join(dir, "internal", "policy", "migrations", "20261110090000_add_users.sql"), []byte("alter table x;\n"), 0o644)
	good := wipCommit(t, dir, "timestamped")
	if err := RefuseMisnamedMigrations(wipGit{}, []string{"internal/policy/migrations/**"}, dir, base, good); err != nil {
		t.Fatalf("RefuseMisnamedMigrations = %v, want the timestamped name to pass", err)
	}
}

// The moved line-bound rule refuses a diff over the bound that no merge
// with main can fit by name, with the sentence that says to merge main,
// and passes a fitting diff through unchanged.
func TestPushRuleFitsOrRefusesTheCardBound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	wipRun(t, "init", "-q", "-b", "main", dir)
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep\n"), 0o644)
	base := wipCommit(t, dir, "base")

	big := make([]string, 150)
	for i := range big {
		big[i] = "line"
	}
	os.WriteFile(filepath.Join(dir, "huge.txt"), []byte(strings.Join(big, "\n")+"\n"), 0o644)
	over := wipCommit(t, dir, "over the bound")
	overTree := wipRun(t, "-C", dir, "rev-parse", over+"^{tree}")
	_, _, err := FitBound(wipGit{}, dir, over, overTree, base, base, base, 100, ruleIdentity, "wip merge")
	if !errors.Is(err, ErrPushOverBound) {
		t.Fatalf("FitBound = %v, want the over-bound refusal by name", err)
	}
	if !strings.Contains(err.Error(), "merge main") {
		t.Fatalf("refusal %q does not say to merge main", err)
	}

	os.Remove(filepath.Join(dir, "huge.txt"))
	os.WriteFile(filepath.Join(dir, "own.txt"), []byte("own\n"), 0o644)
	fitting := wipCommit(t, dir, "fits")
	fitTree := wipRun(t, "-C", dir, "rev-parse", fitting+"^{tree}")
	head, guard, err := FitBound(wipGit{}, dir, fitting, fitTree, base, base, base, 100, ruleIdentity, "wip merge")
	if err != nil || head != fitting || guard != base {
		t.Fatalf("FitBound = %q, %q, %v; want the fitting diff passed through unchanged", head, guard, err)
	}
}

// The rule's merge: a tree that already holds main's content — a sync
// performed without main's ancestry — is recorded as the merge it is,
// with the pusher's own message on the merge commit, so main's commits
// stay main's in the pull request's diff.
func TestPushRuleRecordsTheSyncAsItsOwnMerge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	wipRun(t, "init", "-q", "-b", "main", dir)
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep\n"), 0o644)
	wipCommit(t, dir, "m0")

	wipRun(t, "-C", dir, "checkout", "-q", "-b", "task")
	os.WriteFile(filepath.Join(dir, "own.txt"), []byte("own\n"), 0o644)
	p1 := wipCommit(t, dir, "the pushed start")

	wipRun(t, "-C", dir, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("kept, changed on main\n"), 0o644)
	m1 := wipCommit(t, dir, "main moves")

	wipRun(t, "-C", dir, "checkout", "-q", "task")
	wipRun(t, "-C", dir, "checkout", "-q", "main", "--", "keep.txt")
	os.WriteFile(filepath.Join(dir, "own2.txt"), []byte("own2\n"), 0o644)
	head := wipCommit(t, dir, "the session synced main without its ancestry")
	tree := wipRun(t, "-C", dir, "rev-parse", head+"^{tree}")

	merged, guard, err := FitBound(wipGit{}, dir, head, tree, p1, p1, m1, 100, ruleIdentity, "wip merge")
	if err != nil {
		t.Fatal(err)
	}
	if merged == head {
		t.Fatalf("FitBound kept the flat head %s, want the rule's merge", merged)
	}
	if guard != m1 {
		t.Fatalf("guard = %q, want main's head %s", guard, m1)
	}
	if got := wipRun(t, "-C", dir, "rev-list", "--parents", "-n", "1", merged); got != merged+" "+p1+" "+m1 {
		t.Fatalf("the rule's merge and its parents = %q, want %s parented by the start %s and main %s", got, merged, p1, m1)
	}
	if got, err := (wipGit{}).Git(dir, "log", "-1", "--format=%s", merged); err != nil || got != "wip merge" {
		t.Fatalf("the rule's merge message = %q (%v), want the pusher's own", got, err)
	}
	if got := wipRun(t, "-C", dir, "diff", "--name-only", m1+"..."+merged); got != "own.txt\nown2.txt" {
		t.Fatalf("the merge's diff against main = %q, want only the branch's own files", got)
	}
}

// wipServe is the pod's half of the interim push for the tests: it
// answers POST /wip as the worker does — one WIP commit of the worker's
// own shape on the session's start, built in the seed repo by the next
// queued edit, shipped as a real bundle with the commit named in the
// Wip-Commit header — and every other route with status, as the message
// fake does. wipStatus answers /wip with that status instead, and hang
// holds each /wip ask. It records each ask's bearer, each answer's
// commit, the task branch's tip as each ask arrived, and the first
// error a shipped answer met. ship, when set, replaces the WIP commit
// and bundle the fake would build: it answers the commit to name in the
// Wip-Commit header and the body to answer with, so a test can ship a
// commit that does not descend from the start, a forged author and
// message, or a body that is no bundle.
type wipServe struct {
	mu        sync.Mutex
	status    int
	wipStatus int
	hang      time.Duration
	// base is the session's start, seed the work repo the WIP commits
	// are built in, fork the bare fork the interim pushes land on, and
	// dir where the bundles are written
	base, seed, fork, dir  string
	edits                  []func(dir string)
	wipAuth, commits, tips []string
	err                    error
	ship                   func(i int, tree string) (string, []byte, error)
}

func (s *wipServe) start(t *testing.T, r *Runner) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if req.URL.Path != "/wip" {
			w.WriteHeader(s.status)
			return
		}
		s.wipAuth = append(s.wipAuth, req.Header.Get("Authorization"))
		if s.hang > 0 {
			time.Sleep(s.hang)
		}
		if s.wipStatus != 0 {
			w.WriteHeader(s.wipStatus)
			return
		}
		i := len(s.commits)
		if i >= len(s.edits) {
			w.WriteHeader(http.StatusNoContent) // the tree equals the start's
			return
		}
		// the task branch's tip as this ask arrives: the pushes before it
		// have landed
		tip, err := wipGitRun(s.fork, nil, "rev-parse", "--verify", "--quiet", "refs/heads/factory/t2")
		if err != nil {
			tip = ""
		}
		s.tips = append(s.tips, strings.TrimSpace(tip))
		s.edits[i](s.seed)
		if _, err := wipGitRun(s.seed, nil, "add", "-A"); s.err == nil {
			s.err = err
		}
		tree, err := wipGitRun(s.seed, nil, "write-tree")
		if s.err == nil {
			s.err = err
		}
		commit, body := "", []byte(nil)
		if s.ship != nil {
			commit, body, err = s.ship(i, tree)
			if s.err == nil {
				s.err = err
			}
		} else {
			worker := []string{"GIT_AUTHOR_NAME=factory-agent", "GIT_AUTHOR_EMAIL=factory-agent@factory.local",
				"GIT_COMMITTER_NAME=factory-agent", "GIT_COMMITTER_EMAIL=factory-agent@factory.local"}
			commit, err = wipGitRun(s.seed, worker, "commit-tree", tree, "-p", s.base, "-m", "work in progress")
			if s.err == nil {
				s.err = err
			}
			if _, err := wipGitRun(s.seed, nil, "update-ref", "refs/heads/wip", commit); s.err == nil {
				s.err = err
			}
			bundle := filepath.Join(s.dir, fmt.Sprintf("wip-%d.bundle", i))
			if _, err := wipGitRun(s.seed, nil, "bundle", "create", bundle, s.base+"..refs/heads/wip"); s.err == nil {
				s.err = err
			}
			body, err = os.ReadFile(bundle)
			if s.err == nil {
				s.err = err
			}
		}
		if s.err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.commits = append(s.commits, commit)
		w.Header().Set(wipCommitHeader, commit)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	r.workerPort, _ = strconv.Atoi(port)
}

// wipRig seeds the two repositories the interim push touches — the
// task's, with main at the base, and the bot's fork, with the task
// branch at the base — points the runner's git at them, and answers the
// pod's endpoints from the seed repo: the session starts at the base
// commit, and each /wip ask ships the next queued edit as the worker's
// WIP shape.
func wipRig(t *testing.T, base func(dir string), edits ...func(dir string)) (*Runner, *fakeObjectStore, *wipServe) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitDir := t.TempDir()
	seed := filepath.Join(gitDir, "seed")
	wipRun(t, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644)
	if base != nil {
		base(seed)
	}
	baseSHA := wipCommit(t, seed, "base")
	up, fork := filepath.Join(gitDir, "sf", "repo.git"), filepath.Join(gitDir, "factory-agent", "repo.git")
	wipRun(t, "init", "-q", "--bare", up)
	wipRun(t, "init", "-q", "--bare", fork)
	wipRun(t, "-C", seed, "push", "-q", up, baseSHA+":refs/heads/main")
	wipRun(t, "-C", seed, "push", "-q", fork, baseSHA+":refs/heads/factory/t2")

	task := supervisedTask("t2")
	task.SHA = baseSHA
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, task, newFakeKube(), objects)
	runner.cfg.GitBaseURL = gitDir
	serve := &wipServe{status: http.StatusNoContent, base: baseSHA, seed: seed, fork: fork, dir: t.TempDir(), edits: edits}
	serve.start(t, runner)
	return runner, objects, serve
}

// wipGoOn is the check-in answer that keeps the session running, one per
// expected check-in.
var wipGoOn = CheckinResponse{Text: `{"decision":"message","guidance":"go on"}`}

// Every check-in ships the session's work in progress: factoryd asks
// the pod's /wip with the session's bearer, and pushes the shipped WIP
// — one commit of the worker's own on the session's start, its objects
// as a bundle — to the task branch, each push replacing the tip with
// the newest tree, so the branch holds exactly one commit past the
// start after two check-ins, and the ask that follows sees the push
// before it landed. The session's own end push then lands its commit
// the same way, not refused for the replaced tip.
func TestInterimPushReplacesTheTaskBranchTipAtEachCheckin(t *testing.T) {
	runner, objects, serve := wipRig(t, nil,
		func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644) },
		func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\ntwo\n"), 0o644) })
	chat := &scriptedChat{answers: []CheckinResponse{wipGoOn, wipGoOn}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+commands(24, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 2 {
		t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own pushed end after two check-ins", res, err, chat.calls)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	if serve.err != nil {
		t.Fatalf("the pod's answers broke: %v", serve.err)
	}
	if len(serve.commits) != 2 || len(serve.wipAuth) != 2 {
		t.Fatalf("the pod shipped %d WIPs under %d asks, want one ask and one WIP per check-in", len(serve.commits), len(serve.wipAuth))
	}
	if serve.wipAuth[0] != serve.wipAuth[1] || !strings.HasPrefix(serve.wipAuth[0], "Bearer ") || len(serve.wipAuth[0]) < 26 {
		t.Fatalf("/wip bearers = %q, want the session's bearer on both asks", serve.wipAuth)
	}
	for _, c := range serve.commits {
		if got, _ := wipGitRun(serve.seed, nil, "rev-list", "--parents", "-n", "1", c); got != c+" "+serve.base {
			t.Fatalf("WIP %s parents = %q, want the worker's own shape on the start", c, got)
		}
	}
	if serve.tips[0] != serve.base || serve.tips[1] != serve.commits[0] {
		t.Fatalf("task branch tips as the asks arrived = %q, want the base, then the first WIP landed", serve.tips)
	}
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip != serve.commits[1] {
		t.Fatalf("task branch = %s, want the second check-in's WIP %s", tip, serve.commits[1])
	}
	if got := wipRun(t, "-C", serve.fork, "rev-list", "--count", serve.base+".."+tip); got != "1" {
		t.Fatalf("commits past the start = %s, want the one WIP: each push replaces the tip", got)
	}
	if got := wipRun(t, "-C", serve.fork, "diff", "--shortstat", serve.base, tip); !strings.Contains(got, "1 file changed, 2 insertions(+)") {
		t.Fatalf("the WIP's diff against the start = %q, want b.txt's two lines", got)
	}
	if strings.Contains(string(objGet(objects, "sessions/t2/timeline.jsonl")), "wip_push_failed") {
		t.Fatalf("timeline = %s, want no failed interim push", objGet(objects, "sessions/t2/timeline.jsonl"))
	}

	// the session's end push lands its own commit — parented on the
	// start, as the worker shapes it — on the replaced tip, not refused
	// as a non-fast-forward
	os.WriteFile(filepath.Join(serve.seed, "b.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	wipRun(t, "-C", serve.seed, "add", "-A")
	tree := wipRun(t, "-C", serve.seed, "write-tree")
	end := wipRun(t, "-C", serve.seed, "-c", "user.name=test", "-c", "user.email=test@test", "commit-tree", tree, "-p", serve.base, "-m", "the session's end")
	wipRun(t, "-C", serve.seed, "push", "-q", serve.fork, "+"+end+":refs/heads/factory/t2")
	if got := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); got != end {
		t.Fatalf("task branch = %s after the end push, want the end commit %s", got, end)
	}
	if got := wipRun(t, "-C", serve.fork, "rev-list", "--count", serve.base+".."+end); got != "1" {
		t.Fatalf("commits past the start after the end push = %s, want the one end commit", got)
	}
	if got := wipRun(t, "-C", serve.fork, "diff", "--shortstat", serve.base, end); !strings.Contains(got, "3 insertions(+)") {
		t.Fatalf("the end commit's diff against the start = %q, want b.txt's three lines", got)
	}
}

// The interim push runs the pushes' rule set on the diff start..WIP: a
// WIP that changes the workflow directory is refused — the refusal
// names the directory and the action — and nothing is pushed, the
// branch holds the start alone. The refusal is the interim push's own
// to tolerate: it is logged and recorded, and the check-in runs on.
func TestInterimPushRefusesAWorkflowDirChangeAndRunsOn(t *testing.T) {
	ci := func(body string) func(dir string) {
		return func(dir string) {
			os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
			os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte(body), 0o644)
		}
	}
	runner, objects, serve := wipRig(t, ci("gate: true\n"), ci("gate: false\n"))
	chat := &scriptedChat{answers: []CheckinResponse{wipGoOn}}
	runner.checkin = chat
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
		t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in ran on", res, err, chat.calls)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	if len(serve.commits) != 1 {
		t.Fatalf("the pod shipped %d WIPs, want the one refused", len(serve.commits))
	}
	refusal := fmt.Sprintf("refusing to push: .forgejo/ differs from the base (%s): revert the workflow directory change", serve.base)
	for _, want := range []string{"interim push", ".forgejo/", "differ", "revert the workflow directory change"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refusal's log misses %q:\n%s", want, logs.String())
		}
	}
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if !strings.Contains(fmt.Sprint(step["error"]), refusal) {
		t.Fatalf("the wip_push_failed step = %v, want the rule's refusal", step)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
		t.Fatalf("task branch = %s, want the start alone: the WIP was refused", tip)
	}
}

// A pod that answers the ask with a failure, or hangs past the ask's
// bound, costs the interim push alone: the failure is logged and
// recorded, the branch keeps the start, and the check-in runs on.
func TestInterimPushToleratesAPodThatFailsOrHangs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(*wipServe)
		want  string
	}{
		{name: "answered-5xx", serve: func(s *wipServe) { s.wipStatus = http.StatusBadGateway }, want: "502"},
		{name: "hung-past-the-ask", serve: func(s *wipServe) { s.hang = 300 * time.Millisecond }, want: "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := wipAskTimeout
			wipAskTimeout = 100 * time.Millisecond
			t.Cleanup(func() { wipAskTimeout = old })
			runner, objects, serve := wipRig(t, nil, func(dir string) {
				os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
			})
			tc.serve(serve)
			chat := &scriptedChat{answers: []CheckinResponse{wipGoOn}}
			runner.checkin = chat
			var logs bytes.Buffer
			defer log.SetOutput(log.Writer())
			log.SetOutput(&logs)

			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
				commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
				t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in ran on", res, err, chat.calls)
			}
			if !strings.Contains(logs.String(), "interim push") || !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("the failure's log misses the interim push and %q:\n%s", tc.want, logs.String())
			}
			if step := timelineStep(t, objects, "t2", "wip_push_failed"); step == nil || step["at"] != "ask" {
				t.Fatalf("the wip_push_failed step = %v, want the ask's failure", step)
			}
			if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
				t.Fatalf("task branch = %s, want the start alone: nothing was pushed", tip)
			}
		})
	}
}
