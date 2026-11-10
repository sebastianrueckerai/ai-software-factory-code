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
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
// Wip-Commit header — POST /message as the message endpoint does, and
// every other route with status. wipStatus answers /wip with that status
// instead, and hang holds each /wip ask. It records each ask's bearer,
// each answer's commit, the task branch's tip as each ask arrived, the
// text of each /message, and the first error a shipped answer met; order
// notes each /wip ask, /message and check-in conversation call as they
// arrive, so a test can pin their interleaving. ship, when set, replaces
// the WIP commit and bundle the fake would build: it answers the commit
// to name in the Wip-Commit header and the body to answer with, so a
// test can ship a commit that does not descend from the start, a forged
// author and message, or a body that is no bundle.
type wipServe struct {
	mu        sync.Mutex
	status    int
	wipStatus int
	wipBody   string
	msgBody   string
	redirect  string
	hang      time.Duration
	// base is the session's start, seed the work repo the WIP commits
	// are built in, fork the bare fork the interim pushes land on, and
	// dir where the bundles are written
	base, seed, fork, dir  string
	edits                  []func(dir string)
	wipAuth, commits, tips []string
	msgs                   []string
	err                    error
	order                  *wipOrder
	ship                   func(i int, tree string) (string, []byte, error)
}

func (s *wipServe) start(t *testing.T, r *Runner) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if req.URL.Path == "/message" {
			var in struct {
				Text string `json:"text"`
			}
			b, _ := io.ReadAll(req.Body)
			json.Unmarshal(b, &in)
			s.msgs = append(s.msgs, in.Text)
			if s.order != nil {
				s.order.add("message")
			}
			w.WriteHeader(s.status)
			io.WriteString(w, s.msgBody)
			return
		}
		if req.URL.Path != "/wip" {
			w.WriteHeader(s.status)
			return
		}
		if s.order != nil {
			s.order.add("wip")
		}
		s.wipAuth = append(s.wipAuth, req.Header.Get("Authorization"))
		if s.hang > 0 {
			time.Sleep(s.hang)
		}
		if s.redirect != "" {
			http.Redirect(w, req, s.redirect, http.StatusFound)
			return
		}
		if s.wipStatus != 0 {
			w.WriteHeader(s.wipStatus)
			io.WriteString(w, s.wipBody)
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
	serve := &wipServe{status: http.StatusNoContent, base: baseSHA, seed: seed, fork: fork, dir: t.TempDir(), edits: edits, order: &wipOrder{}}
	serve.start(t, runner)
	return runner, objects, serve
}

// wipOrder notes one run's interleaving: each /wip ask, each /message
// and each check-in conversation call, in the order they arrived.
type wipOrder struct {
	mu sync.Mutex
	ev []string
}

func (o *wipOrder) add(ev string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ev = append(o.ev, ev)
}

func (o *wipOrder) all() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.ev)
}

// wipChat is the check-in's model for the interim push's tests: it
// answers in order, records each call's input — the ask as the check-in
// read it — and notes each conversation call in the shared order.
type wipChat struct {
	answers []CheckinResponse
	order   *wipOrder
	mu      sync.Mutex
	calls   int
	inputs  []string
}

func (c *wipChat) Call(_ context.Context, _, _ string, in CheckinRequest) (CheckinResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls >= len(c.answers) {
		return CheckinResponse{}, errors.New("no scripted answer left")
	}
	ans := c.answers[c.calls]
	c.calls++
	c.inputs = append(c.inputs, in.Input)
	if c.order != nil {
		c.order.add("checkin")
	}
	return ans, nil
}

// wipContinue is the check-in answer that lets the session run on
// without posting a message of its own.
var wipContinue = CheckinResponse{Text: `{"decision":"continue","reason":"the work goes on"}`}

// wipSteps counts the steps named step on the task's timeline.
func wipSteps(t *testing.T, objects *fakeObjectStore, taskID, step string) int {
	t.Helper()
	b, _ := objects.Get(context.Background(), "sessions/"+taskID+"/timeline.jsonl")
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["step"] == step {
			n++
		}
	}
	return n
}

// wipGoOn is the check-in answer that keeps the session running, one per
// expected check-in.
var wipGoOn = CheckinResponse{Text: `{"decision":"message","guidance":"go on"}`}

// Every check-in ships the session's work in progress: factoryd asks
// the pod's /wip with the session's bearer, and pushes the shipped WIP
// — the worker's own commit on the session's start, its objects as a
// bundle — as factoryd's own commit built from the shipped tree, to the
// task branch, each push replacing the tip with the newest tree, so the
// branch holds exactly one commit past the start after two check-ins,
// and the ask that follows sees the push before it landed; exactly one
// ask precedes each check-in.
func TestInterimPushReplacesTheTaskBranchTipAtEachCheckin(t *testing.T) {
	runner, objects, serve := wipRig(t, nil,
		func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644) },
		func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\ntwo\n"), 0o644) })
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue, wipContinue}}
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
	if order := serve.order.all(); !slices.Equal(order, []string{"wip", "checkin", "wip", "checkin"}) {
		t.Fatalf("the run's order = %q, want exactly one ask before each check-in", order)
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
	if serve.tips[0] != serve.base {
		t.Fatalf("task branch tip as the first ask arrived = %q, want the base", serve.tips[0])
	}
	// the landed heads are factoryd's own commits — the shipped tree, the
	// start as the only parent, the factory's identity and message — never
	// the commits the sandbox made
	first := serve.tips[1]
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if first == serve.commits[0] || tip == serve.commits[1] {
		t.Fatalf("landed heads = %s, %s; want factoryd's own commits, not the shipped WIPs %s, %s", first, tip, serve.commits[0], serve.commits[1])
	}
	for i, c := range []string{first, tip} {
		if got, _ := wipGitRun(serve.fork, nil, "rev-list", "--parents", "-n", "1", c); got != c+" "+serve.base {
			t.Fatalf("the landed head's parents = %q, want the start alone as its parent", got)
		}
		shipped, _ := wipGitRun(serve.seed, nil, "rev-parse", serve.commits[i]+"^{tree}")
		if got, _ := wipGitRun(serve.fork, nil, "rev-parse", c+"^{tree}"); got != shipped {
			t.Fatalf("the landed head's tree = %s, want the shipped WIP's tree %s", got, shipped)
		}
		if got, _ := wipGitRun(serve.fork, nil, "log", "-1", "--format=%an <%ae>", c); got != "factory-agent <factory-agent@factory.local>" {
			t.Fatalf("the landed head's author = %q, want the factory's own identity", got)
		}
		if got, _ := wipGitRun(serve.fork, nil, "log", "-1", "--format=%s", c); got != "factory task t2: work in progress" {
			t.Fatalf("the landed head's subject = %q, want the interim push's own message", got)
		}
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 2 {
		t.Fatalf("wip_pushed steps = %d, want one per landed push", n)
	}
	if step := timelineStep(t, objects, "t2", "wip_pushed"); step["commit"] != tip {
		t.Fatalf("the wip_pushed step = %v, want the landed head %s", step, tip)
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

// The shipped body is verified as a bundle before anything is taken from
// it: a body that is no bundle — whatever its header says it ships — is
// refused, nothing is fetched from it, nothing is pushed, and the
// refusal is no message to the agent: the sandbox's answer is not the
// agent's to fix.
func TestInterimPushRefusesABodyThatIsNoBundle(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	serve.ship = func(i int, tree string) (string, []byte, error) {
		// a gitdir file pointing at a repository on the test's disk whose
		// wip ref descends from the base: taken as a local repository it
		// would push a commit the sandbox never shipped
		commit, err := wipGitRun(serve.seed, ruleIdentity, "commit-tree", tree, "-p", serve.base, "-m", "unshipped")
		if err != nil {
			return "", nil, err
		}
		if _, err := wipGitRun(serve.seed, nil, "update-ref", "refs/heads/wip", commit); err != nil {
			return "", nil, err
		}
		return commit, []byte("gitdir: " + filepath.Join(serve.seed, ".git") + "\n"), nil
	}
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
		t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in ran on", res, err, chat.calls)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	if len(serve.commits) != 1 || len(serve.wipAuth) != 1 {
		t.Fatalf("the pod shipped %d WIPs under %d asks, want the one refused under one ask", len(serve.commits), len(serve.wipAuth))
	}
	if len(serve.msgs) != 0 {
		t.Fatalf("the pod was messaged %q, want no message: a broken body is no refusal the agent can act on", serve.msgs)
	}
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if !strings.Contains(fmt.Sprint(step["error"]), "the shipped body is no bundle") {
		t.Fatalf("the wip_push_failed step = %v, want the no-bundle refusal", step)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 0 {
		t.Fatalf("wip_pushed steps = %d, want none: nothing was pushed", n)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
		t.Fatalf("task branch = %s, want the start alone: the body was refused", tip)
	}
}

// A malformed object inside a well-formed bundle is refused by the
// strict index: a commit with a bad author date and a tree that holds a
// .git entry never reach the push. The branch holds the start and
// nothing reaches the agent.
func TestInterimPushRefusesAMalformedObject(t *testing.T) {
	hashObject := func(t *testing.T, seed, typ string, raw []byte) string {
		t.Helper()
		cmd := exec.Command("git", "-C", seed, "hash-object", "-t", typ, "-w", "--literally", "--stdin")
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for name, build := range map[string]func(t *testing.T, seed, base, tree string) string{
		"bad-author-date": func(t *testing.T, seed, base, tree string) string {
			return hashObject(t, seed, "commit", []byte("tree "+tree+"\nparent "+base+"\nauthor a <a@a.example> not-a-date +0000\ncommitter a <a@a.example> 1700000000 +0000\n\nwip\n"))
		},
		"dot-git-tree-entry": func(t *testing.T, seed, base, tree string) string {
			blob := hashObject(t, seed, "blob", []byte("x\n"))
			id, _ := hex.DecodeString(blob)
			bad := hashObject(t, seed, "tree", append([]byte("100644 .git\x00"), id...))
			return hashObject(t, seed, "commit", []byte("tree "+bad+"\nparent "+base+"\nauthor a <a@a.example> 1700000000 +0000\ncommitter a <a@a.example> 1700000000 +0000\n\nwip\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner, objects, serve := wipRig(t, nil, func(dir string) {
				os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
			})
			serve.ship = func(i int, tree string) (string, []byte, error) {
				bad := build(t, serve.seed, serve.base, tree)
				if _, err := wipGitRun(serve.seed, nil, "update-ref", "refs/heads/wip", bad); err != nil {
					return "", nil, err
				}
				bundle := filepath.Join(serve.dir, "bad.bundle")
				if _, err := wipGitRun(serve.seed, nil, "bundle", "create", bundle, serve.base+"..refs/heads/wip"); err != nil {
					return "", nil, err
				}
				b, err := os.ReadFile(bundle)
				return bad, b, err
			}
			chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
			runner.checkin = chat

			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
				commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
				t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in ran on", res, err, chat.calls)
			}
			serve.mu.Lock()
			defer serve.mu.Unlock()
			if step := timelineStep(t, objects, "t2", "wip_push_failed"); !strings.Contains(fmt.Sprint(step["error"]), "index the shipped bundle") {
				t.Fatalf("the wip_push_failed step = %v, want the strict index's refusal", step)
			}
			if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 0 {
				t.Fatalf("wip_pushed steps = %d, want none: the malformed object was refused", n)
			}
			if len(serve.msgs) != 0 {
				t.Fatalf("the pod was messaged %q, want no message", serve.msgs)
			}
			if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
				t.Fatalf("task branch = %s, want the start alone", tip)
			}
		})
	}
}

// bombBundle is a bundle a few KiB long whose one delta copies a 16 MiB
// zero blob copies times: the pack index inflates copies*16 MiB from it.
func bombBundle(t *testing.T, copies int) (string, []byte) {
	t.Helper()
	const size = 0xFFFFFF
	objHeader := func(typ, n int) []byte {
		b := []byte{byte(typ<<4 | n&0x0F)}
		for n >>= 4; n > 0; n >>= 7 {
			b[len(b)-1] |= 0x80
			b = append(b, byte(n&0x7F))
		}
		return b
	}
	varint := func(n int) []byte {
		var b []byte
		for {
			c := byte(n & 0x7F)
			if n >>= 7; n == 0 {
				return append(b, c)
			}
			b = append(b, c|0x80)
		}
	}
	deflate := func(b []byte) []byte {
		var buf bytes.Buffer
		w, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
		w.Write(b)
		w.Close()
		return buf.Bytes()
	}
	zeros := make([]byte, size)
	pack := append([]byte("PACK"), 0, 0, 0, 2, 0, 0, 0, 2)
	baseOff := len(pack)
	pack = append(append(pack, objHeader(3, size)...), deflate(zeros)...)
	delta := append(varint(size), varint(size*copies)...)
	for range copies {
		delta = append(delta, 0xF0, 0xFF, 0xFF, 0xFF)
	}
	deltaOff := len(pack)
	back := deltaOff - baseOff
	ofs := []byte{byte(back & 0x7F)}
	for back >>= 7; back > 0; back >>= 7 {
		back--
		ofs = append([]byte{0x80 | byte(back&0x7F)}, ofs...)
	}
	pack = append(append(append(pack, objHeader(6, len(delta))...), ofs...), deflate(delta)...)
	sum := sha1.Sum(pack)
	pack = append(pack, sum[:]...)
	blob := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", size)), zeros...))
	id := hex.EncodeToString(blob[:])
	return id, append([]byte("# v2 git bundle\n"+id+" refs/heads/wip\n\n"), pack...)
}

// A bundle a few KiB long that inflates to hundreds of MiB is refused by
// the memory cap on git: 32 copies is the 512 MiB bomb, 21 copies the
// 336 MiB one that fit the old 384 MiB cap. Git dies on its allocation.
func TestInterimPushRefusesAZipBomb(t *testing.T) {
	for _, copies := range []int{32, 21} {
		t.Run(fmt.Sprint(copies, "-copies"), func(t *testing.T) {
			runner, objects, serve := wipRig(t, nil, func(dir string) {
				os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
			})
			id, bomb := bombBundle(t, copies)
			if len(bomb) > 32<<10 {
				t.Fatalf("the bomb is %d bytes, want a few KiB", len(bomb))
			}
			serve.ship = func(i int, tree string) (string, []byte, error) { return id, bomb, nil }
			chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
			runner.checkin = chat

			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
				commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
				t.Fatalf("result = (%+v, %v) after %d conversations, want the check-in to run on", res, err, chat.calls)
			}
			step := timelineStep(t, objects, "t2", "wip_push_failed")
			if text := fmt.Sprint(step["error"]); !strings.Contains(text, "index the shipped bundle") || !strings.Contains(text, "memory") {
				t.Fatalf("the wip_push_failed step = %v, want git's allocation failure at the index", step)
			}
			if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
				t.Fatalf("task branch = %s, want the start alone", tip)
			}
		})
	}
}

// gitSpy puts a git on PATH that notes each call's subcommand, and
// answers the file the notes go to.
func gitSpy(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$1\" >> '"+calls+"'\nexec '"+realGit+"' \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return calls
}

// seedGit runs git in dir with stdin and answers its trimmed output.
func seedGit(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// shipsTree makes the fake pod ship a WIP commit on the start whose tree
// build makes in the seed repository from objects alone: no file reaches
// the disk, so the work of a test is the size of its objects.
func (s *wipServe) shipsTree(build func(seed string) (string, error)) {
	s.ship = func(i int, _ string) (string, []byte, error) {
		tree, err := build(s.seed)
		if err != nil {
			return "", nil, err
		}
		commit, err := wipGitRun(s.seed, ruleIdentity, "commit-tree", tree, "-p", s.base, "-m", "wip")
		if err != nil {
			return "", nil, err
		}
		if _, err := wipGitRun(s.seed, nil, "update-ref", "refs/heads/wip", commit); err != nil {
			return "", nil, err
		}
		file := filepath.Join(s.dir, "tree.bundle")
		if _, err := wipGitRun(s.seed, nil, "bundle", "create", file, s.base+"..refs/heads/wip"); err != nil {
			return "", nil, err
		}
		body, err := os.ReadFile(file)
		return commit, body, err
	}
}

// Work too large for any card is refused before any diff or listing reads
// it: a 16 MiB file of newlines (a 16 KB bundle that FitBound's diff
// inflates to 700 MiB), a directory of 100,000 entries (a tree object over
// the tree bound), 60,000 files in small directories (over the file bound
// of the whole tree), and one 7 MiB blob under ten names (over the blob
// bound of the whole tree). The trees are built from objects, so no test
// waits on the disk. The failing step is the size check, and git never ran
// diff or read-tree on it.
func TestInterimPushRefusesTooLargeWorkBeforeReadingIt(t *testing.T) {
	noEdit := func(string) {}
	for name, tc := range map[string]struct {
		edit func(dir string)
		tree func(seed string) (string, error)
	}{
		"file-of-newlines": {edit: func(dir string) {
			os.WriteFile(filepath.Join(dir, "nl.txt"), bytes.Repeat([]byte("\n"), 16<<20), 0o644)
		}},
		"many-files": {edit: noEdit, tree: func(seed string) (string, error) {
			empty, err := seedGit(seed, "", "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			var top strings.Builder
			for d := range 60 {
				var dir strings.Builder
				for i := range 1000 {
					fmt.Fprintf(&dir, "100644 blob %s\tf%04d\n", empty, i)
				}
				id, err := seedGit(seed, dir.String(), "mktree")
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&top, "040000 tree %s\td%02d\n", id, d)
			}
			return seedGit(seed, top.String(), "mktree")
		}},
		"ten-copies-of-a-blob": {edit: noEdit, tree: func(seed string) (string, error) {
			blob, err := seedGit(seed, string(make([]byte, 7<<20)), "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			var top strings.Builder
			for i := range 10 {
				fmt.Fprintf(&top, "100644 blob %s\tcopy%d.bin\n", blob, i)
			}
			return seedGit(seed, top.String(), "mktree")
		}},
		"tree-bound-alone": {edit: noEdit, tree: func(seed string) (string, error) {
			// 40,000 entries with 40-character names: a 2.7 MB tree, over the tree
			// bound and under every other bound
			empty, err := seedGit(seed, "", "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			var dir strings.Builder
			for i := range 40000 {
				fmt.Fprintf(&dir, "100644 blob %s\t%040d\n", empty, i)
			}
			id, err := seedGit(seed, dir.String(), "mktree")
			if err != nil {
				return "", err
			}
			return seedGit(seed, "040000 tree "+id+"\twide\n", "mktree")
		}},
		"wide-tree": {edit: noEdit, tree: func(seed string) (string, error) {
			empty, err := seedGit(seed, "", "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			var wide strings.Builder
			for i := range 100000 {
				fmt.Fprintf(&wide, "100644 blob %s\tf%06d\n", empty, i)
			}
			id, err := seedGit(seed, wide.String(), "mktree")
			if err != nil {
				return "", err
			}
			return seedGit(seed, "040000 tree "+id+"\twide\n", "mktree")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			runner, objects, serve := wipRig(t, nil, tc.edit)
			if tc.tree != nil {
				serve.shipsTree(tc.tree)
			}
			version := testPolicyVersion()
			version.Document.Risk.LargeLines = 1000
			runner.cfg.Policy = func() policy.Version { return version }
			runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
			calls := gitSpy(t)
			chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
			runner.checkin = chat

			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
				commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
				t.Fatal(err)
			}
			step := timelineStep(t, objects, "t2", "wip_push_failed")
			if !strings.Contains(fmt.Sprint(step["error"]), "the shipped work is too large") {
				t.Fatalf("the wip_push_failed step = %v, want the size refusal", step)
			}
			log, _ := os.ReadFile(calls)
			for _, sub := range []string{"diff", "read-tree"} {
				if slices.Contains(strings.Fields(string(log)), sub) {
					t.Fatalf("git ran %s on the oversized work: %s", sub, log)
				}
			}
			serve.mu.Lock()
			defer serve.mu.Unlock()
			if len(serve.msgs) != 0 {
				t.Fatalf("the pod was messaged %q, want no message: the end push has no size bound, so the agent is not at fault", serve.msgs)
			}
			if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
				t.Fatalf("task branch = %s, want the start alone", tip)
			}
		})
	}
}

// A large card still lands under the cap: 15 MiB of incompressible data in
// three files, each under the object bound.
func TestInterimPushLandsALargePack(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		for i := range 3 {
			big := make([]byte, 5<<20)
			rand.New(rand.NewSource(int64(i))).Read(big)
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("big%d.bin", i)), big, 0o644)
		}
	})
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the large pack landed; timeline %s", n, objGet(objects, "sessions/t2/timeline.jsonl"))
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip == serve.base {
		t.Fatal("task branch is still the start")
	}
}

// One interim push handles its body at a time: the pod is asked first, and
// while another push holds the slot a second waits for its own bound, records
// a queue step and gives up without pushing; with the slot free it lands.
func TestInterimPushesDoNotOverlap(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	spec := sessionSpec{id: "s-1", startRef: serve.base, repo: Repo{Owner: "sf", Name: "repo"}, messageToken: "tok"}
	kube := runner.kube.(*fakeKube)
	kube.pods["ns/pod"] = &fakePod{ns: "ns", name: "pod", phase: "Running"}
	old := wipSlotWait
	wipSlotWait = 150 * time.Millisecond
	t.Cleanup(func() { wipSlotWait = old })
	interimSlot <- struct{}{}
	out := runner.interimPush(context.Background(), supervisedTask("t2"), spec, "ns", "pod", nil)
	<-interimSlot
	serve.mu.Lock()
	asked := len(serve.wipAuth)
	serve.mu.Unlock()
	if asked != 1 {
		t.Fatalf("the pod was asked %d times while the slot was held, want once: the ask comes before the slot", asked)
	}
	if !reflect.DeepEqual(out, wipOutcome{}) {
		t.Fatalf("outcome %+v while the slot was held, want a push that waited and gave up", out)
	}
	if step := timelineStep(t, objects, "t2", "wip_push_failed"); step["at"] != "queue" {
		t.Fatalf("the wip_push_failed step = %v, want the queue's", step)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
		t.Fatalf("task branch = %s, want the start alone: the waiting push gave up", tip)
	}
	serve.edits = append(serve.edits, func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o644) })
	if out := runner.interimPush(context.Background(), supervisedTask("t2"), spec, "ns", "pod", nil); out.head == "" {
		t.Fatalf("outcome %+v with the slot free, want the push landed; timeline %s", out, objGet(objects, "sessions/t2/timeline.jsonl"))
	}
}

// A pack that carries deltas indexes under the cap: with git's default
// threads, index-pack cannot map its resolver threads' stacks.
func TestInterimPushLandsAPackWithDeltas(t *testing.T) {
	var lines []string
	for i := range 3000 {
		lines = append(lines, fmt.Sprintf("line %d of a file the card edits in a few places", i))
	}
	text := strings.Join(lines, "\n") + "\n"
	runner, objects, serve := wipRig(t, func(dir string) {
		os.WriteFile(filepath.Join(dir, "data.txt"), []byte(text), 0o644)
	}, func(dir string) {
		os.WriteFile(filepath.Join(dir, "data.txt"), []byte(strings.Replace(text, "line 7 ", "edited 7 ", 1)), 0o644)
	})
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the delta pack landed; timeline %s", n, objGet(objects, "sessions/t2/timeline.jsonl"))
	}
}

// A task branch an earlier push left with a file past the cap does not
// drive factoryd's base fetch past it: the fetch dies in git, and the
// interim push fails.
func TestInterimPushBaseFetchRunsUnderTheCap(t *testing.T) {
	runner, objects, serve := wipRig(t, func(dir string) {
		f, err := os.Create(filepath.Join(dir, "huge.bin"))
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(300 << 20)
		f.Close()
	}, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if text := fmt.Sprint(step["error"]); !strings.Contains(text, "fetch the task branch") || !strings.Contains(text, "memory") {
		t.Fatalf("the wip_push_failed step = %v, want git's allocation failure in the base fetch", step)
	}
}

// The push carries a lease on the tip pushWip fetched: an end push that
// lands between the fetch and the push stays, and the interim push
// fails.
func TestInterimPushLosesTheLeaseToAnEndPush(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	end := wipRun(t, "-C", serve.seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", wipRun(t, "-C", serve.seed, "rev-parse", "HEAD^{tree}"), "-p", serve.base, "-m", "end")
	wipRun(t, "-C", serve.seed, "push", "-q", serve.fork, end+":refs/heads/other")
	// a git that moves the fork's tip to the end push's commit as factoryd
	// builds its own commit, after the fetch and before the push
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nif [ \"$1\" = commit-tree ] && [ \"${*#*Factory-Role}\" != \"$*\" ]; then '"+realGit+"' --git-dir='"+serve.fork+"' update-ref refs/heads/factory/t2 "+end+"; fi\nexec '"+realGit+"' \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 0 {
		t.Fatalf("wip_pushed steps = %d, want none: the tip moved under the push", n)
	}
	if step := timelineStep(t, objects, "t2", "wip_push_failed"); step["at"] != "push" {
		t.Fatalf("the wip_push_failed step = %v, want the push's failure", step)
	}
	if len(serve.msgs) != 0 {
		t.Fatalf("the pod was messaged %q, want no message: a lost lease is no refusal", serve.msgs)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != end {
		t.Fatalf("task branch = %s, want the end push's commit %s", tip, end)
	}
}

// A pod's redirect is not followed: factoryd makes no request to the
// Location the pod names.
func TestInterimPushDoesNotFollowARedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	serve.redirect = target.URL + "/admin"
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the redirect target was hit %d times, want none", hits.Load())
	}
	if step := timelineStep(t, objects, "t2", "wip_push_failed"); step["at"] != "ask" || !strings.Contains(fmt.Sprint(step["error"]), "302") {
		t.Fatalf("the wip_push_failed step = %v, want the ask's 302 refusal", step)
	}
}

// A refusal that quotes the agent's own file path reaches the session's
// /message and the check-in's ask with a bearer value in the path masked.
func TestInterimPushRefusalTextIsScrubbed(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.MkdirAll(filepath.Join(dir, "internal", "policy", "migrations"), 0o755)
		os.WriteFile(filepath.Join(dir, "internal", "policy", "migrations", "Bearer abcdefghij0123456789xyz.sql"), []byte("alter table x;\n"), 0o644)
	})
	version := testPolicyVersion()
	version.Document.Risk = policy.Risk{
		Classes: []policy.RiskClass{{Name: "migration", Outcome: policy.OutcomeRequiresApproval, Migration: true}},
		Rules:   []policy.RiskRule{{Class: "migration", Paths: []string{"internal/policy/migrations/**"}}},
	}
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(serve.msgs) != 1 || !strings.Contains(serve.msgs[0], "is misnamed") || strings.Contains(serve.msgs[0], "abcdefghij0123456789xyz") {
		t.Fatalf("the pod was messaged %q, want the misnamed-migration refusal with the bearer masked", serve.msgs)
	}
	if len(chat.inputs) != 1 || !strings.Contains(chat.inputs[0], serve.msgs[0]) || strings.Contains(chat.inputs[0], "abcdefghij0123456789xyz") {
		t.Fatalf("the ask's statement does not carry the masked refusal")
	}
}

// A Wip-Commit header that is no commit id is refused, and its quoted
// value is bounded.
func TestInterimPushBoundsANonHexCommitHeader(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	serve.ship = func(i int, tree string) (string, []byte, error) {
		return "--" + strings.Repeat("x", 5000), []byte("body"), nil
	}
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if text := fmt.Sprint(step["error"]); step["at"] != "ask" || !strings.Contains(text, "names commit") || len(text) > 300 {
		t.Fatalf("the wip_push_failed step = %v, want the ask's refusal, bounded", step)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 0 {
		t.Fatal("a push ran")
	}
}

// A refusal the pod cannot take logs and records the delivery's error
// scrubbed.
func TestInterimPushUndeliveredRefusalErrorIsScrubbed(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
		os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte("gate: false\n"), 0o644)
	})
	serve.status = http.StatusInternalServerError
	serve.msgBody = "upstream https://user:hunter2pass@example.com/x failed; Authorization: Bearer abcdefghij0123456789xyz"
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	step := fmt.Sprint(timelineStep(t, objects, "t2", "message"))
	for name, text := range map[string]string{"message step": step, "log": logs.String()} {
		if !strings.Contains(text, "500") || strings.Contains(text, "hunter2pass") || strings.Contains(text, "abcdefghij0123456789xyz") {
			t.Fatalf("the %s = %q, want the delivery's error with userinfo and bearer masked", name, text)
		}
	}
}

// A start that is no commit id reaches no git call: no ask, no step.
func TestInterimPushRefusesAStartThatIsNoCommitID(t *testing.T) {
	runner, objects, serve := wipRig(t, nil)
	out := runner.interimPush(context.Background(), supervisedTask("t2"), sessionSpec{id: "s-1", startRef: "--upload-pack=x", repo: Repo{Owner: "sf", Name: "repo"}}, "ns", "pod", nil)
	if !reflect.DeepEqual(out, wipOutcome{}) || len(serve.wipAuth) != 0 || wipSteps(t, objects, "t2", "wip_push_failed") != 0 {
		t.Fatalf("outcome %+v, asks %d: want nothing run", out, len(serve.wipAuth))
	}
}

// Every text built from an interim push's error is scrubbed: a pod's
// failure body carrying URL userinfo and a bearer value reaches the log
// and the timeline masked.
func TestInterimPushScrubsItsFailureText(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	serve.wipStatus, serve.wipBody = http.StatusBadGateway, "upstream https://user:hunter2pass@example.com/x failed; Authorization: Bearer abcdefghij0123456789xyz"
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	step := fmt.Sprint(timelineStep(t, objects, "t2", "wip_push_failed"))
	for name, text := range map[string]string{"timeline": step, "log": logs.String()} {
		if !strings.Contains(text, "502") || strings.Contains(text, "hunter2pass") || strings.Contains(text, "abcdefghij0123456789xyz") {
			t.Fatalf("the %s = %q, want the failure with userinfo and bearer masked", name, text)
		}
	}
}

// A refusal the session never received is no guidance: when the message
// cannot be delivered, the check-in's ask does not name it as delivered.
func TestInterimPushRefusalNotDeliveredIsNotNamedAsGuidance(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
		os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte("gate: false\n"), 0o644)
	})
	serve.status = http.StatusInternalServerError
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	serve.mu.Lock()
	sent := len(serve.msgs)
	serve.mu.Unlock()
	if sent != 1 {
		t.Fatalf("the pod saw %d messages, want the one refusal that failed", sent)
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.inputs) != 1 || strings.Contains(chat.inputs[0], "Guidance delivered to the agent this session:") {
		t.Fatalf("the ask's statement = %q, want no delivered guidance: the refusal never arrived", chat.inputs)
	}
}

// A WIP that does not descend from the session's start — a commit from
// another history, the shape no worker /wip builds — is refused: it is
// not the session's work, whatever tree it carries. Nothing is pushed,
// and the refusal is no message to the agent.
func TestInterimPushRefusesAWipFromAnotherHistory(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	serve.ship = func(i int, tree string) (string, []byte, error) {
		rogue, err := wipGitRun(serve.seed, ruleIdentity, "commit-tree", tree) // a root commit: no parent at all
		if err != nil {
			return "", nil, err
		}
		if _, err := wipGitRun(serve.seed, nil, "update-ref", "refs/heads/wip", rogue); err != nil {
			return "", nil, err
		}
		bundle := filepath.Join(serve.dir, "rogue.bundle")
		if _, err := wipGitRun(serve.seed, nil, "bundle", "create", bundle, "refs/heads/wip"); err != nil {
			return "", nil, err
		}
		b, err := os.ReadFile(bundle)
		return rogue, b, err
	}
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
		t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in ran on", res, err, chat.calls)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	if len(serve.msgs) != 0 {
		t.Fatalf("the pod was messaged %q, want no message: another history is no refusal the agent can act on", serve.msgs)
	}
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if !strings.Contains(fmt.Sprint(step["error"]), "does not descend from the session's start") {
		t.Fatalf("the wip_push_failed step = %v, want the ancestry refusal", step)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 0 {
		t.Fatalf("wip_pushed steps = %d, want none: nothing was pushed", n)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
		t.Fatalf("task branch = %s, want the start alone: the WIP was refused", tip)
	}
}

// The pushed commit is factoryd's own, never the sandbox's: a WIP whose
// commit carries a forged author and a message with closing keywords of
// its own choosing lands as the commit factoryd builds from the shipped
// tree — the factory's identity, the interim push's message, the start
// as the only parent — and the forged commit itself never reaches the
// branch.
func TestInterimPushBuildsItsOwnCommitFromTheShippedTree(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	forge := []string{"GIT_AUTHOR_NAME=Evil Agent", "GIT_AUTHOR_EMAIL=evil@sandbox.local",
		"GIT_COMMITTER_NAME=Evil Agent", "GIT_COMMITTER_EMAIL=evil@sandbox.local"}
	serve.ship = func(i int, tree string) (string, []byte, error) {
		forged, err := wipGitRun(serve.seed, forge, "commit-tree", tree, "-p", serve.base,
			"-m", "Closes #2\n\nthe closing keyword the sandbox chose")
		if err != nil {
			return "", nil, err
		}
		if _, err := wipGitRun(serve.seed, nil, "update-ref", "refs/heads/wip", forged); err != nil {
			return "", nil, err
		}
		bundle := filepath.Join(serve.dir, "forged.bundle")
		if _, err := wipGitRun(serve.seed, nil, "bundle", "create", bundle, serve.base+"..refs/heads/wip"); err != nil {
			return "", nil, err
		}
		b, err := os.ReadFile(bundle)
		return forged, b, err
	}
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
		t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in ran on", res, err, chat.calls)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip == serve.commits[0] {
		t.Fatalf("task branch = the sandbox's forged commit %s, want factoryd's own", tip)
	}
	if got, _ := wipGitRun(serve.fork, nil, "rev-list", "--parents", "-n", "1", tip); got != tip+" "+serve.base {
		t.Fatalf("the landed head's parents = %q, want the start alone as its parent", got)
	}
	shipped, _ := wipGitRun(serve.seed, nil, "rev-parse", serve.commits[0]+"^{tree}")
	if got, _ := wipGitRun(serve.fork, nil, "rev-parse", tip+"^{tree}"); got != shipped {
		t.Fatalf("the landed head's tree = %s, want the shipped tree %s: the work itself lands", got, shipped)
	}
	if got, _ := wipGitRun(serve.fork, nil, "log", "-1", "--format=%an <%ae>", tip); got != "factory-agent <factory-agent@factory.local>" {
		t.Fatalf("the landed head's author = %q, want the factory's own identity", got)
	}
	if got, _ := wipGitRun(serve.fork, nil, "log", "-1", "--format=%cn <%ce>", tip); got != "factory-agent <factory-agent@factory.local>" {
		t.Fatalf("the landed head's committer = %q, want the factory's own identity", got)
	}
	if body, _ := wipGitRun(serve.fork, nil, "log", "-1", "--format=%B", tip); strings.Contains(body, "Closes #2") || !strings.Contains(body, "factory task t2: work in progress") {
		t.Fatalf("the landed head's message = %q, want the interim push's own, not the forged", body)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the one push", n)
	}
	if step := timelineStep(t, objects, "t2", "wip_pushed"); step["commit"] != tip {
		t.Fatalf("the wip_pushed step = %v, want the landed head %s", step, tip)
	}
}

// The interim push fires before the check-in conversation opens: exactly
// one /wip ask precedes the conversation's first call, the head it lands
// records one wip_pushed timeline step carrying the commit, and the
// ask's facts name the head, so the check-in judges the session with its
// shipped work in hand.
func TestInterimPushFiresBeforeTheCheckinAndNamesItsHead(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{{ToolName: "card_facts"}, wipContinue}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 2 {
		t.Fatalf("result = (%+v, %v) after %d calls, want the session's own end after the check-in read the facts", res, err, chat.calls)
	}
	serve.mu.Lock()
	if len(serve.wipAuth) != 1 {
		serve.mu.Unlock()
		t.Fatalf("the pod saw %d /wip asks, want exactly one before the check-in", len(serve.wipAuth))
	}
	if len(serve.msgs) != 0 {
		serve.mu.Unlock()
		t.Fatalf("the pod was messaged %q, want no message: the push landed", serve.msgs)
	}
	serve.mu.Unlock()
	if order := serve.order.all(); !slices.Equal(order, []string{"wip", "checkin", "checkin"}) {
		t.Fatalf("the run's order = %q, want the one /wip ask before the conversation's calls", order)
	}
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the one push", n)
	}
	if step := timelineStep(t, objects, "t2", "wip_pushed"); step["commit"] != tip {
		t.Fatalf("the wip_pushed step = %v, want the landed head %s", step, tip)
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.inputs) != 2 || !strings.Contains(chat.inputs[1], `"wip_head":"`+tip+`"`) {
		t.Fatalf("the ask's facts after card_facts = %q, want the head %s named", chat.inputs, tip)
	}
}

// A push rule's refusal reaches the running session and its check-in:
// the workflow-directory rule's sentence is posted into the session as
// one /message — with the work still in the sandbox — and named in the
// ask's messages, so the check-in judges the session knowing its work
// was refused, not as the session's last line.
func TestInterimPushRefusalReachesTheSessionAndItsCheckin(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
		os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte("gate: false\n"), 0o644)
	})
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || chat.calls != 1 {
		t.Fatalf("result = (%+v, %v) after %d conversations, want the session's own end after the check-in judged the refusal", res, err, chat.calls)
	}
	serve.mu.Lock()
	if len(serve.wipAuth) != 1 {
		serve.mu.Unlock()
		t.Fatalf("the pod saw %d /wip asks, want exactly one before the check-in", len(serve.wipAuth))
	}
	if len(serve.msgs) != 1 || !strings.Contains(serve.msgs[0], ".forgejo/") ||
		!strings.Contains(serve.msgs[0], "revert the workflow directory change") ||
		!strings.Contains(serve.msgs[0], "the rule lets no agent change it") {
		serve.mu.Unlock()
		t.Fatalf("the pod was messaged %q, want exactly the refusal's sentence", serve.msgs)
	}
	if !strings.HasPrefix(serve.msgs[0], "factoryd's interim push of your work in progress was refused: ") {
		serve.mu.Unlock()
		t.Fatalf("the refusal = %q, want it framed as the interim push's", serve.msgs[0])
	}
	refused := serve.msgs[0]
	serve.mu.Unlock()
	if order := serve.order.all(); !slices.Equal(order, []string{"wip", "message", "checkin"}) {
		t.Fatalf("the run's order = %q, want the ask, then the refusal's message, then the conversation", order)
	}
	chat.mu.Lock()
	if len(chat.inputs) != 1 || !strings.Contains(chat.inputs[0], "Guidance delivered to the agent this session:") || !strings.Contains(chat.inputs[0], refused) {
		chat.mu.Unlock()
		t.Fatalf("the ask's statement = %q, want the refusal named in the ask's messages", chat.inputs)
	}
	chat.mu.Unlock()
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if refused != "factoryd's interim push of your work in progress was refused: "+fmt.Sprint(step["error"]) {
		t.Fatalf("the wip_push_failed step = %v, want the rule's sentence the refusal carries", step)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 0 {
		t.Fatalf("wip_pushed steps = %d, want none: the push was refused", n)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
		t.Fatalf("task branch = %s, want the start alone: the WIP was refused", tip)
	}
}

// A session that dies without a result line — OOMKilled, no worker push
// — still hands its next session the interim head: the result carries
// the last head the check-in's interim push landed, so the card resumes
// from the pushed branch, not from the base.
func TestASessionThatDiesCarriesItsInterimHeadInItsResult(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, commands(0, 24))
	if err != nil {
		t.Fatal(err)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip == serve.base {
		t.Fatalf("task branch = the start, want the interim push's head landed before the session died")
	}
	if res.Commit != tip {
		t.Fatalf("the dead session's result commit = %q, want the interim head %s", res.Commit, tip)
	}
	if step := timelineStep(t, objects, "t2", "wip_pushed"); step["commit"] != tip {
		t.Fatalf("the wip_pushed step = %v, want the landed head %s", step, tip)
	}
	if len(res.Findings) == 0 || !strings.HasPrefix(res.Findings[0], "previous session ended unfinished: ") {
		t.Fatalf("findings = %q, want a session that holds a pushed head to read as unfinished", res.Findings)
	}
}

// A stopped worker whose end push failed but whose interim push landed
// does not say its work is unpushed.
func TestStopReportCountsTheInterimHeadAsPushed(t *testing.T) {
	out := sessionOutcome{noResult: "the worker died", wip: "w1"}
	if got := out.stopReport(); len(got) != 1 || got[0] != "the worker died" {
		t.Fatalf("stopReport = %q, want the reason alone, as for a pushed session", got)
	}
	out = sessionOutcome{result: &harness.Result{Summary: "stopped"}, wip: "w1"}
	if got := out.stopReport(); got != nil {
		t.Fatalf("stopReport = %q, want no report: the branch holds the work", got)
	}
}

// A session that pushed work in progress and then ended with nothing past
// its start says the branch still holds the interim push, in a first
// session and in a resumed one, and does not claim the start as its result.
func TestASessionThatRevertsItsWorkSaysWhatTheBranchHolds(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprint("resumed=", resumed), func(t *testing.T) {
			runner, _, serve := wipRig(t, nil, func(dir string) {
				os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
			})
			if resumed {
				// the start is an earlier interim head, not main
				prior := wipRun(t, "-C", serve.seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", "-p", serve.base, "-m", "prior", wipRun(t, "-C", serve.seed, "rev-parse", "HEAD^{tree}"))
				wipRun(t, "-C", serve.seed, "push", "-q", serve.fork, "+"+prior+":refs/heads/factory/t2")
				serve.base = prior
			}
			chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
			runner.checkin = chat
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
				commands(0, 24)+resultLine(true, "no changes to push"))
			if err != nil {
				t.Fatal(err)
			}
			tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
			if res.Outcome == OutcomePushed || res.Commit != tip || !strings.Contains(res.Summary, "the branch still holds the interim push "+tip) {
				t.Fatalf("result = %+v, want the interim head %s named as what the branch holds, not a pushed outcome", res, tip)
			}
		})
	}
}

// gitConfig keeps the two settings every capped call needs: one pack thread,
// and a 32 MiB pack window instead of the whole pack.
func TestGitConfigHoldsTheCapSettings(t *testing.T) {
	for _, want := range []string{"pack.threads=1", "core.packedGitWindowSize=32m", "grep.threads=1"} {
		if !slices.Contains(gitConfig, want) {
			t.Fatalf("gitConfig = %q, want %s", gitConfig, want)
		}
	}
}

// A first push, when the fork has no task branch yet, falls back to main
// as the start and creates the branch.
func TestInterimPushFallsBackToMainWhenTheForkHasNoBranch(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	wipRun(t, "-C", serve.fork, "update-ref", "-d", "refs/heads/factory/t2")
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the first push landed on a new branch; timeline %s", n, objGet(objects, "sessions/t2/timeline.jsonl"))
	}
}

// The /wip body goes to a file, never into memory: two concurrent answers of
// 15 MiB allocate a small part of that, and the file holds the body.
func TestPostWipStreamsTheBodyToAFile(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	}, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o644)
	})
	body := bytes.Repeat([]byte("x"), 15<<20)
	id := "0123456789012345678901234567890123456789"
	serve.ship = func(i int, tree string) (string, []byte, error) { return id, body, nil }
	runner.kube.(*fakeKube).pods["ns/pod"] = &fakePod{ns: "ns", name: "pod", phase: "Running"}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var wg sync.WaitGroup
	paths := make([]string, 2)
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commit, path, err := runner.postWip(context.Background(), "ns", "pod", "tok")
			if err != nil || commit != id {
				t.Errorf("postWip = (%q, %q, %v), want the commit and a file", commit, path, err)
			}
			paths[i] = path
		}()
	}
	wg.Wait()
	runtime.ReadMemStats(&after)
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || fi.Size() != int64(len(body)) {
			t.Fatalf("bundle file %q: %v, %v; want %d bytes", p, fi, err, len(body))
		}
		os.Remove(p)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
		t.Fatalf("two 15 MiB bodies allocated %d MiB, want far less than one body: they stream to disk", got>>20)
	}
}

// A pod's response headers are bounded: 100 KiB of header fails the ask.
func TestInterimPushBoundsTheResponseHeaders(t *testing.T) {
	edit := func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644) }
	runner, objects, serve := wipRig(t, nil, edit, edit, edit, edit, edit, edit)
	serve.ship = func(i int, tree string) (string, []byte, error) {
		return strings.Repeat("x", 100<<10), []byte("body"), nil
	}
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	step := timelineStep(t, objects, "t2", "wip_push_failed")
	if text := fmt.Sprint(step["error"]); step["at"] != "ask" || !strings.Contains(text, "exceeded") {
		t.Fatalf("the wip_push_failed step = %v, want the ask's header-bound failure", step)
	}
}

// gitRun fails a call whose output passes its cap, and keeps one at it.
func TestGitRunFailsPastItsOutputCap(t *testing.T) {
	dir := t.TempDir()
	wipRun(t, "init", "-q", "-b", "main", dir)
	if _, err := gitRun(context.Background(), dir, "", nil, gitLimits{maxOut: 8}, "--version"); !errors.Is(err, errGitOutput) {
		t.Fatalf("a 20-byte output under an 8-byte cap: err = %v, want the cap's failure", err)
	}
	if out, err := gitRun(context.Background(), dir, "", nil, gitLimits{maxOut: 64}, "--version"); err != nil || !strings.HasPrefix(out, "git version") {
		t.Fatalf("under the cap: (%q, %v), want the output", out, err)
	}
}

// A bundle that builds on a commit the repository lacks is refused, and the
// refusal names that commit, not the pod's body.
func TestInterimPushRefusesABundleWithAMissingPrerequisite(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	var missing string
	serve.ship = func(i int, tree string) (string, []byte, error) {
		var err error
		if missing, err = wipGitRun(serve.seed, ruleIdentity, "commit-tree", tree, "-m", "elsewhere"); err != nil {
			return "", nil, err
		}
		w, err := wipGitRun(serve.seed, ruleIdentity, "commit-tree", tree, "-p", missing, "-m", "wip")
		if err != nil {
			return "", nil, err
		}
		if _, err := wipGitRun(serve.seed, nil, "update-ref", "refs/heads/wip", w); err != nil {
			return "", nil, err
		}
		file := filepath.Join(serve.dir, "missing.bundle")
		if _, err := wipGitRun(serve.seed, nil, "bundle", "create", file, missing+"..refs/heads/wip"); err != nil {
			return "", nil, err
		}
		b, err := os.ReadFile(file)
		return w, b, err
	}
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprint(timelineStep(t, objects, "t2", "wip_push_failed")["error"])
	if !strings.Contains(text, "builds on commit "+missing) || strings.Contains(text, "no bundle") {
		t.Fatalf("the refusal = %q, want it to name the missing commit %s, not the pod's body", text, missing)
	}
}

// Capped git calls run one at a time across the process: two overlapping
// calls would pass the memory the cap assumes.
func TestCappedGitCallsDoNotOverlap(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, notes := t.TempDir(), filepath.Join(t.TempDir(), "notes")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho start >> '"+notes+"'\nsleep 0.2\necho end >> '"+notes+"'\nexec '"+realGit+"' \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gitRun(context.Background(), t.TempDir(), "", nil, gitLimits{addressSpace: wipGitMemory}, "--version")
		}()
	}
	wg.Wait()
	got, _ := os.ReadFile(notes)
	if want := "start\nend\nstart\nend\n"; string(got) != want {
		t.Fatalf("capped calls ran as %q, want %q", got, want)
	}
}

// The temporary file that holds the shipped body is gone after each way an
// interim push can end.
func TestInterimPushRemovesItsBundleFile(t *testing.T) {
	workflow := func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
		os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "ci.yml"), []byte("gate: false\n"), 0o644)
	}
	plain := func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644) }
	for name, tc := range map[string]struct {
		edit    func(dir string)
		holdFor time.Duration // the slot is held for this long, 0: not held
		ctx     time.Duration // the context's timeout, 0: none
		ship    func(serve *wipServe) func(i int, tree string) (string, []byte, error)
	}{
		"gave-up-at-the-slot":       {edit: plain, holdFor: time.Hour},
		"context-ended-in-the-wait": {edit: plain, holdFor: time.Hour, ctx: 100 * time.Millisecond},
		"refused":                   {edit: workflow},
		"push-failed": {edit: plain, ship: func(*wipServe) func(int, string) (string, []byte, error) {
			return func(int, string) (string, []byte, error) {
				return "0123456789012345678901234567890123456789", []byte("not a bundle"), nil
			}
		}},
		"pushed": {edit: plain},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			runner, _, serve := wipRig(t, nil, tc.edit)
			if tc.ship != nil {
				serve.ship = tc.ship(serve)
			}
			runner.kube.(*fakeKube).pods["ns/pod"] = &fakePod{ns: "ns", name: "pod", phase: "Running"}
			spec := sessionSpec{id: "s-1", startRef: serve.base, repo: Repo{Owner: "sf", Name: "repo"}, messageToken: "tok"}
			ctx := context.Background()
			if tc.ctx > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctx)
				defer cancel()
			}
			if tc.holdFor > 0 {
				old := wipSlotWait
				wipSlotWait = 150 * time.Millisecond
				if tc.ctx > 0 {
					wipSlotWait = time.Hour
				}
				t.Cleanup(func() { wipSlotWait = old })
				interimSlot <- struct{}{}
				defer func() { <-interimSlot }()
			}
			runner.interimPush(ctx, supervisedTask("t2"), spec, "ns", "pod", nil)
			if left, _ := filepath.Glob(filepath.Join(tmp, "wip-*.bundle")); len(left) != 0 {
				t.Fatalf("bundle files left behind: %v", left)
			}
		})
	}
}

// A header Go can refuse never reaches git: a header with no end inside its
// bound, and one with 17 prerequisites in upper case, which the bound counts
// like lower case.
func TestInterimPushBoundsTheBundleHeaderBeforeGit(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef01234567"
	upper := strings.ToUpper("fedcba9876543210fedcba9876543210fedcba98")
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"header-without-an-end":             {"# v2 git bundle\n" + strings.Repeat("-"+upper+" x\n", 3000), "no header end"},
		"seventeen-uppercase-prerequisites": {"# v2 git bundle\n" + strings.Repeat("-"+upper+" x\n", 17) + id + " refs/heads/wip\n\nPACK", "lists 17 prerequisites"},
	} {
		t.Run(name, func(t *testing.T) {
			edit := func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644) }
			runner, objects, serve := wipRig(t, nil, edit)
			serve.ship = func(int, string) (string, []byte, error) { return id, []byte(tc.body), nil }
			calls := gitSpy(t)
			chat := &wipChat{order: serve.order, answers: []CheckinResponse{wipContinue}}
			runner.checkin = chat

			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
				commands(0, 24)+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
				t.Fatal(err)
			}
			if text := fmt.Sprint(timelineStep(t, objects, "t2", "wip_push_failed")["error"]); !strings.Contains(text, tc.want) {
				t.Fatalf("the refusal = %q, want %q", text, tc.want)
			}
			log, _ := os.ReadFile(calls)
			if slices.Contains(strings.Fields(string(log)), "bundle") || slices.Contains(strings.Fields(string(log)), "cat-file") {
				t.Fatalf("git read the refused header: %s", log)
			}
		})
	}
}

// A call's own timeout starts once it holds the capped slot: a wait for the
// slot longer than the timeout does not fail it.
func TestCappedGitTimeoutStartsAfterTheSlot(t *testing.T) {
	old := gitTimeout
	gitTimeout = 400 * time.Millisecond
	t.Cleanup(func() { gitTimeout = old })
	cappedGit <- struct{}{}
	go func() {
		time.Sleep(700 * time.Millisecond)
		<-cappedGit
	}()
	if out, err := gitRun(context.Background(), t.TempDir(), "", nil, gitLimits{addressSpace: wipGitMemory}, "--version"); err != nil || !strings.HasPrefix(out, "git version") {
		t.Fatalf("gitRun = (%q, %v), want it to run after waiting longer than its timeout", out, err)
	}
}

// A size trip on a session that holds finished work over the card's
// bound loses nothing: the interim push lands the whole work although it
// is over the bound (the end push holds that rule), and the stop names
// the bound, the size and the files by their changed lines, the largest
// first. A 350-line a.txt and a 20-line b.txt against a small card's 300.
func TestSizeTripPushesWorkOverTheBoundInParts(t *testing.T) {
	runner, objects, serve := wipRig(t, nil,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "b.txt"), []byte(strings.Repeat("b\n", 20)), 0o644)
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Repeat("a\n", 350)), 0o644)
		})
	store := runner.store.(*fakeStore)
	tk := store.tasks["t2"]
	tk.Kind, tk.Size = "agent", "small"
	store.tasks["t2"] = tk
	version := testPolicyVersion()
	version.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	chat := &wipChat{order: serve.order, answers: []CheckinResponse{{ToolName: "card_facts"}, wipContinue}}
	runner.checkin = chat

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		cardDiffLine(250, 120)+"\n"+pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
		t.Fatal(err)
	}
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip == serve.base {
		t.Fatal("the task branch holds the start alone: the size bound refused the interim push")
	}
	if got := wipRun(t, "-C", serve.fork, "diff", "--shortstat", serve.base, tip); !strings.Contains(got, "2 files changed, 370 insertions(+)") {
		t.Fatalf("the pushed work = %q, want both files whole", got)
	}
	if n := wipSteps(t, objects, "t2", "wip_push_failed"); n != 0 {
		t.Fatalf("wip_push_failed steps = %d, want none", n)
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	want := "the card's diff is 370 changed lines (added plus deleted), over the 300 its size plans; the work is pushed as the branch's work in progress, its files by changed lines: a.txt 350, b.txt 20"
	if len(chat.inputs) != 2 || !strings.Contains(chat.inputs[0], want) {
		t.Fatalf("the ask = %q, want the stop naming the bound, the size and the parts: %q", chat.inputs, want)
	}
	// the check-in's card_facts tool answers with the facts runner.go built
	if facts := `"wip_files":[{"path":"a.txt","lines":350},{"path":"b.txt","lines":20}]`; !strings.Contains(chat.inputs[1], facts) {
		t.Fatalf("the card_facts answer = %q, want %s", chat.inputs[1], facts)
	}
}

// The parts are the card's diff against main, which the line bound
// measures, not the session's own: a session that starts from an earlier
// push (a.txt, 200 lines) and adds b.txt names both.
func TestInterimPushNamesThePartsOfTheCardsWholeDiff(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte(strings.Repeat("b\n", 30)), 0o644) })
	os.WriteFile(filepath.Join(serve.seed, "a.txt"), []byte(strings.Repeat("a\n", 200)), 0o644)
	earlier := wipCommit(t, serve.seed, "earlier work")
	wipRun(t, "-C", serve.seed, "push", "-q", "-f", serve.fork, earlier+":refs/heads/factory/t2")
	serve.base = earlier

	runner.kube.(*fakeKube).pods["ns/pod"] = &fakePod{ns: "ns", name: "pod", phase: "Running"}
	spec := sessionSpec{id: "s-1", startRef: earlier, repo: Repo{Owner: "sf", Name: "repo"}, messageToken: "tok"}
	out := runner.interimPush(context.Background(), supervisedTask("t2"), spec, "ns", "pod", nil)
	want := wipParts{list: []wipPart{{"a.txt", 200}, {"b.txt", 30}}}
	if out.head == "" || !reflect.DeepEqual(out.parts, want) {
		t.Fatalf("outcome = %+v, want a landed head and the parts %+v", out, want)
	}
}

// Past wipPartsMax files the stop says how many more there are, a path is
// cut to wipPartPath bytes on a character boundary, and a name with a
// space and an accent stays readable, not C-quoted.
func TestInterimPushBoundsAndMarksItsPartsList(t *testing.T) {
	long := strings.Repeat("é", 100) + ".txt"
	runner, _, serve := wipRig(t, nil, func(dir string) {
		for i := range 40 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("x\n"), 0o644)
		}
		os.WriteFile(filepath.Join(dir, "über wir.txt"), []byte(strings.Repeat("u\n", 9)), 0o644)
		os.WriteFile(filepath.Join(dir, long), []byte(strings.Repeat("l\n", 8)), 0o644)
		os.WriteFile(filepath.Join(dir, "g.txt"), []byte("x\n"), 0o644)
	})
	runner.kube.(*fakeKube).pods["ns/pod"] = &fakePod{ns: "ns", name: "pod", phase: "Running"}
	spec := sessionSpec{id: "s-1", startRef: serve.base, repo: Repo{Owner: "sf", Name: "repo"}, messageToken: "tok"}
	out := runner.interimPush(context.Background(), supervisedTask("t2"), spec, "ns", "pod", nil)
	p := out.parts
	if len(p.list) != wipPartsMax || p.more != 3 {
		t.Fatalf("parts = %d listed and %d more, want %d and 3 of 43 files", len(p.list), p.more, wipPartsMax)
	}
	if p.list[0] != (wipPart{"über wir.txt", 9}) || p.list[1].Lines != 8 || !strings.HasSuffix(p.list[1].Path, "…") || len(p.list[1].Path) > wipPartPath+len("…") || !utf8.ValidString(p.list[1].Path) {
		t.Fatalf("first parts = %+v, want the accented name raw, then the long one cut on a character", p.list[:2])
	}
	if !strings.HasSuffix(p.text(), ", and 3 more files") {
		t.Fatalf("text = %q, want the count of the rest", p.text())
	}
}

func TestWipPartsTextNamesNothingEmpty(t *testing.T) {
	if got := (wipParts{}).text(); got != "" {
		t.Fatalf("text of no parts = %q, want none", got)
	}
	if got := (wipParts{unread: true}).text(); got != "the list of files could not be read" {
		t.Fatalf("text of an unread list = %q", got)
	}
	if got := (wipParts{list: []wipPart{{"a.go", 5}}}).text(); got != "its files by changed lines: a.go 5" {
		t.Fatalf("text = %q", got)
	}
}

type failingGit struct{}

func (failingGit) Git(string, ...string) (string, error) {
	return "", errors.New("output past its cap")
}
func (failingGit) RunGit(string, []string, ...string) (string, error) {
	return "", errors.New("output past its cap")
}

// A listing that fails is information lost, not a push refused.
func TestWipPartsListingFailureIsUnread(t *testing.T) {
	if got := listWipParts(failingGit{}, "dir", "start", "head"); !got.unread {
		t.Fatalf("parts = %+v, want unread", got)
	}
}

// A session whose end push the line bound refuses (work that came after the
// size trip, or inside the last checkpoint window) is not lost with its
// pod: factoryd takes the finished work through the interim push when the
// worker reports the refusal, and the result carries the head.
func TestEndPushRefusedOverTheBoundTakesTheWorkThroughTheInterimPush(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte(strings.Repeat("b\n", 350)), 0o644)
	})
	refused := harness.Event{Kind: harness.EventPushFailed, Time: time.Now(), Detail: map[string]any{"branch": "factory/t2",
		"error": ErrPushOverBound.Error() + ": the branch's diff against its merge base is 50 lines over the card's 300-line bound"}}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		envLine(refused)+"\n"+resultLine(false, "push failed: "+ErrPushOverBound.Error()))
	if err != nil {
		t.Fatal(err)
	}
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip == serve.base {
		t.Fatal("the task branch holds the start alone: the refused work died with the pod")
	}
	if res.Commit != tip {
		t.Fatalf("result commit = %q, want the interim head %s", res.Commit, tip)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the one interim push", n)
	}
	if want := overBoundNote + " factory/t2 at " + tip; len(res.Findings) < 2 || res.Findings[0] != want || strings.Contains(res.Summary, "interim commit") {
		t.Fatalf("summary %q, findings %q; want the note as the first finding, not in the summary", res.Summary, res.Findings)
	}
}

func pushRefusedOverTheBound() string {
	return envLine(harness.Event{Kind: harness.EventPushFailed, Time: time.Now(), Detail: map[string]any{"branch": "factory/t2",
		"error": ErrPushOverBound.Error() + ": the branch's diff against its merge base is 50 lines over the card's 300-line bound"}}) + "\n"
}

// The refusal reaches the interim push whatever the session is doing: when
// the wall-clock cap has stopped it, the SIGTERM end push the bound refused
// still ships its work, and the result names where the work sits.
func TestEndPushRefusedOverTheBoundAfterAStopTakesTheWork(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte(strings.Repeat("b\n", 350)), 0o644)
	})
	runner.stopWait = 5 * time.Second
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3})
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n"+sigtermLogs+pushRefusedOverTheBound()+
			resultLine(false, "push failed: "+ErrPushOverBound.Error()))
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want a wall-clock stop", res, err)
	}
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip == serve.base || res.Commit != tip {
		t.Fatalf("branch %s, result commit %q; want the work shipped at the stop and its head in the result", tip, res.Commit)
	}
	if want := overBoundNote + " factory/t2 at " + tip; len(res.Findings) == 0 || res.Findings[0] != want {
		t.Fatalf("findings = %q, want %q first, for the successor", res.Findings, want)
	}
}

// waitChat is a check-in that stays in its conversation until the pod's
// second /wip ask is answered, or 10 s.
type waitChat struct {
	serve *wipServe
	ready atomic.Bool
}

func (c *waitChat) Call(context.Context, string, string, CheckinRequest) (CheckinResponse, error) {
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.serve.mu.Lock()
		n := len(c.serve.commits)
		c.serve.mu.Unlock()
		if n >= 2 {
			c.ready.Store(true)
			break
		}
	}
	return wipContinue, nil
}

// A refusal that arrives while a check-in's conversation runs is not
// waiting behind it, which can take minutes: the work ships at once.
func TestEndPushRefusedOverTheBoundDuringACheckinTakesTheWork(t *testing.T) {
	runner, _, serve := wipRig(t, nil,
		func(dir string) { os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644) },
		func(dir string) { os.WriteFile(filepath.Join(dir, "c.txt"), []byte(strings.Repeat("c\n", 350)), 0o644) })
	chat := &waitChat{serve: serve}
	runner.checkin = chat
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		commands(0, 24)+pushRefusedOverTheBound()+resultLine(false, "push failed: "+ErrPushOverBound.Error())); err != nil {
		t.Fatal(err)
	}
	if !chat.ready.Load() {
		t.Fatal("the refused work was not shipped while the check-in conversation ran")
	}
}

// Work the over-bound push could not take is no silent loss: when the slot
// stays taken past its bound, the result's first finding says the work was
// not pushed and why, so the case reads it.
func TestOverBoundWorkThatCannotBePushedIsSaidOnTheResult(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte(strings.Repeat("b\n", 350)), 0o644)
	})
	old := wipSlotWait
	wipSlotWait = 100 * time.Millisecond
	interimSlot <- struct{}{}
	t.Cleanup(func() { wipSlotWait = old })
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		pushRefusedOverTheBound()+resultLine(false, "push failed: "+ErrPushOverBound.Error()))
	<-interimSlot
	if err != nil {
		t.Fatal(err)
	}
	if want := overBoundUnpushed + "another interim push held the slot for 100ms"; len(res.Findings) == 0 || res.Findings[0] != want {
		t.Fatalf("findings = %q, want %q first", res.Findings, want)
	}
	if tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2"); tip != serve.base {
		t.Fatalf("the branch moved to %s although the push could not start", tip)
	}
}
