package factoryd

// The push rules factoryd holds for both pushes (pushrules.go): the
// worker's end push calls them through its aliases today, and the
// interim push of the next cards calls them here. The proofs run the
// rules on real git repositories: the workflow-directory rule's refusal
// names the directory and the action to take, the migration-naming rule
// refuses the next number, and the line-bound rule refuses a diff no
// merge can fit by name, passes a fitting diff through, and finishes a
// sync as the merge the rule makes with the pusher's own message.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// wipIdentity is the commit identity the tests' merge commits carry.
var wipIdentity = []string{
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
	_, _, err := FitBound(wipGit{}, dir, over, overTree, base, base, base, 100, wipIdentity, "wip merge")
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
	head, guard, err := FitBound(wipGit{}, dir, fitting, fitTree, base, base, base, 100, wipIdentity, "wip merge")
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

	merged, guard, err := FitBound(wipGit{}, dir, head, tree, p1, p1, m1, 100, wipIdentity, "wip merge")
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
