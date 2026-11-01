package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The push rule of the stuck-card faults: before every push, the branch's
// diff against its merge base must fit the card's line bound. A sync that
// brought main's content in without its ancestry lands as a merge, a merge
// the rule can make lands as one, and a diff that fits no shape is refused
// by name. The seed's main moves once (m1 changes keep.txt and adds 150
// lines of big.txt); every case is a second session starting from the
// first session's pushed commit p1.
func TestPushFitsTheCardLineBound(t *testing.T) {
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	big := func(lines int, prefix string) []byte {
		rows := make([]string, lines)
		for i := range rows {
			rows[i] = prefix + "-line"
		}
		return []byte(strings.Join(rows, "\n") + "\n")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep\n"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "m0")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	m0 := run("-C", seed, "rev-parse", "HEAD")

	// the first session's pushed commit is the base of every second session;
	// each case gets its own branch, so one case's push moves no other's
	firstSession := func(branch string) string {
		e1 := workerEnv{taskID: "t1", sessionID: "s-1", author: "alice", objective: "o", repoSHA: m0, branch: branch, repoURL: remote, pushURL: fork}
		first := filepath.Join(t.TempDir(), "first")
		if err := cloneBase(e1, first); err != nil {
			t.Fatal(err)
		}
		base, mainHead0, err := prepareRepo(first, e1)
		if err != nil || base != m0 {
			t.Fatalf("base %s main %s (%v), want base %s", base, mainHead0, err, m0)
		}
		os.WriteFile(filepath.Join(first, "own.txt"), []byte("own\n"), 0o644)
		p1, err := commitAndPush(first, e1, base, mainHead0, func() error { return nil }, fixedToken("t"))
		if err != nil {
			t.Fatal(err)
		}
		return p1
	}

	os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("kept, changed on main\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "big.txt"), big(150, "main"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "m1 moves main")
	run("-C", seed, "push", "-q", remote, "main")
	run("-C", seed, "push", "-q", fork, "main")
	m1 := run("-C", seed, "rev-parse", "HEAD")

	for _, tc := range []struct {
		name    string
		bound   int
		agent   func(dir string)
		refused bool
		parents string // the pushed commit's parents, besides itself
		diff    string // git diff --name-only m1 <pushed>
	}{
		{name: "flat sync becomes a merge", bound: 100, agent: func(dir string) {
			run("-C", dir, "checkout", "-q", "origin/main", "--", "big.txt", "keep.txt")
			os.WriteFile(filepath.Join(dir, "own2.txt"), []byte("own2\n"), 0o644)
		}, parents: "p1 m1", diff: "own.txt\nown2.txt"},
		{name: "behind main with own work stays flat", bound: 100, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "own.txt"), []byte("own\nand the session's edit\n"), 0o644)
		}, parents: "p1", diff: "own.txt"},
		{name: "a partial sync is finished as the rule's merge", bound: 100, agent: func(dir string) {
			run("-C", dir, "checkout", "-q", "origin/main", "--", "big.txt")
			os.WriteFile(filepath.Join(dir, "own2.txt"), []byte("own2\n"), 0o644)
		}, parents: "p1 m1", diff: "own.txt\nown2.txt"},
		{name: "conflicting sync is refused by name", bound: 100, refused: true, agent: func(dir string) {
			run("-C", dir, "checkout", "-q", "origin/main", "--", "big.txt")
			os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("kept, changed by the session\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "own2.txt"), []byte("own2\n"), 0o644)
		}},
		{name: "own work over the bound is refused", bound: 100, refused: true, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "huge.txt"), big(150, "session"), 0o644)
		}},
		{name: "zero bound keeps the flat push", bound: 0, agent: func(dir string) {
			run("-C", dir, "checkout", "-q", "origin/main", "--", "big.txt", "keep.txt")
			os.WriteFile(filepath.Join(dir, "own2.txt"), []byte("own2\n"), 0o644)
		}, parents: "p1", diff: "big.txt\nkeep.txt\nown.txt\nown2.txt"},
		{name: "a session that merged main passes through", bound: 100, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "own.txt"), []byte("own\nand the session's edit\n"), 0o644)
			run("-C", dir, "add", "-A")
			run("-C", dir, "-c", "user.name=agent", "-c", "user.email=a@a", "commit", "-q", "-m", "own")
			run("-C", dir, "-c", "user.name=agent", "-c", "user.email=a@a", "merge", "-q", "--no-edit", "origin/main")
		}, parents: "p1 m1", diff: "own.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/t-bound-" + strings.ReplaceAll(strings.ReplaceAll(tc.name, " ", "-"), ",", "")
			p1 := firstSession(branch)
			tc.parents = strings.ReplaceAll(tc.parents, "p1", p1)
			tc.parents = strings.ReplaceAll(tc.parents, "m1", m1)
			e := workerEnv{taskID: "t1", sessionID: "s-2", author: "alice", objective: "o",
				repoSHA: p1, branch: branch, repoURL: remote, pushURL: fork, lineBound: tc.bound}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := cloneBase(e, dir); err != nil {
				t.Fatal(err)
			}
			base, mainHead, err := prepareRepo(dir, e)
			if err != nil || base != p1 || mainHead != m1 {
				t.Fatalf("base %s main %s (%v), want %s and %s", base, mainHead, err, p1, m1)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
			if tc.refused {
				if err == nil || !errors.Is(err, errPushOverBound) {
					t.Fatalf("commitAndPush = %q, %v; want the over-bound refusal", commit, err)
				}
				if !strings.Contains(err.Error(), "merge main") {
					t.Fatalf("refusal %q does not say to merge main", err)
				}
				if head := run("-C", fork, "rev-parse", "--verify", "-q", "refs/heads/"+branch); head != p1 {
					t.Fatalf("fork branch = %q, want still %s: nothing is pushed", head, p1)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := run("-C", fork, "rev-list", "--parents", "-n", "1", commit); got != commit+" "+tc.parents {
				t.Fatalf("commit and parents = %q, want %q", got, commit+" "+tc.parents)
			}
			if got := run("-C", fork, "diff", "--name-only", m1+"..."+commit); got != tc.diff {
				t.Fatalf("change against main = %q, want %q", got, tc.diff)
			}
		})
	}
}

// The merge the rule makes counts main's commits as ancestry, not as the
// branch's additions: one worker commit is what the pull request holds.
func TestTheSyncMergeBillsMainToMain(t *testing.T) {
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep\n"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "m0")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	m0 := run("-C", seed, "rev-parse", "HEAD")

	branch := "factory/t-bill"
	e1 := workerEnv{taskID: "t1", sessionID: "s-1", author: "alice", objective: "o", repoSHA: m0, branch: branch, repoURL: remote, pushURL: fork}
	first := filepath.Join(t.TempDir(), "first")
	if err := cloneBase(e1, first); err != nil {
		t.Fatal(err)
	}
	base, mainHead0, err := prepareRepo(first, e1)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(first, "own.txt"), []byte("own\n"), 0o644)
	p1, err := commitAndPush(first, e1, base, mainHead0, func() error { return nil }, fixedToken("t"))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep, moved "+string(rune('a'+i))+"\n"), 0o644)
		run("-C", seed, "add", "-A")
		run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "main moves")
	}
	run("-C", seed, "push", "-q", remote, "main")
	run("-C", seed, "push", "-q", fork, "main")
	m1 := run("-C", seed, "rev-parse", "HEAD")

	e := workerEnv{taskID: "t1", sessionID: "s-2", author: "alice", objective: "o",
		repoSHA: p1, branch: branch, repoURL: remote, pushURL: fork, lineBound: 10}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := cloneBase(e, dir); err != nil {
		t.Fatal(err)
	}
	base, mainHead, err := prepareRepo(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	run("-C", dir, "checkout", "-q", "origin/main", "--", "keep.txt")
	os.WriteFile(filepath.Join(dir, "own2.txt"), []byte("own2\n"), 0o644)

	commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	if got := run("-C", fork, "rev-list", "--parents", "-n", "1", commit); got != commit+" "+p1+" "+m1 {
		t.Fatalf("commit and parents = %q, want the sync recorded as the branch's merge of %s and %s", got, p1, m1)
	}
	if got := run("-C", fork, "diff", "--name-only", m1+"..."+commit); got != "own.txt\nown2.txt" {
		t.Fatalf("the pull request bills %q, want only the card's own files: main's history must not bill to the card", got)
	}
}
