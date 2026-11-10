package factoryd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The push rules, one rule set for both pushes: the worker's end push
// (commitAndPush, cmd/agent-worker/main.go, whose local names alias
// these) and factoryd's interim push at a check-in. WorkflowDirs
// (activities.go) names the directories the workflow-directory rule
// guards; each pusher brings only its git.

// PushGit is the git the push rules run. Git answers a token-free git
// command's stdout in dir; RunGit one under an extra environment — a
// commit identity, an index file. The worker's end push brings its own
// hardened git; factoryd's interim push brings the scratch repo's.
type PushGit interface {
	Git(dir string, args ...string) (string, error)
	RunGit(dir string, env []string, args ...string) (string, error)
}

// ErrPushOverBound is the named refusal of the card's line bound: the
// branch's diff against its merge base does not fit it, and no shape the
// pusher can give the push changes that.
var ErrPushOverBound = errors.New("push refused: over the card's line bound")

// FitBound is the card's line-bound rule on one push, and the shape the
// sync lands in. A tree that already holds main's content — the sync the
// session performed without main's ancestry — is recorded as the merge it
// is, whatever its size, so main's commits stay main's in the pull
// request's diff. A branch whose diff against its merge base is over the
// bound is merged with main by the rule when that fits; what no merge can
// fit is refused by name. identity and message make the rule's merge
// commit: the end push's attribution, an interim push's work-in-progress.
func FitBound(g PushGit, send, head, tree, base, guard, mainHead string, bound int, identity []string, message string) (string, string, error) {
	if bound <= 0 {
		return head, guard, nil
	}
	mb, err := g.Git(send, "merge-base", head, mainHead)
	if err != nil {
		return "", "", err
	}
	billed, err := ChangedLines(g, send, mb, head)
	if err != nil {
		return "", "", err
	}
	merge := func(t string) (string, string, error) {
		h, err := g.RunGit(send, identity, "commit-tree", t, "-p", base, "-p", mainHead, "-m", message)
		return h, mainHead, err
	}
	merged, mErr := mergedTree(g, send, mb, head, mainHead)
	// the faithful sync: the tree is what a merge with main would give, on
	// a branch main has moved past; one that holds main already has none
	if mErr == nil && mb != mainHead && merged == tree && fits(g, send, mainHead, tree, bound) {
		return merge(tree)
	}
	if billed <= bound {
		return head, guard, nil
	}
	if mErr == nil && fits(g, send, mainHead, merged, bound) {
		return merge(merged)
	}
	return "", "", fmt.Errorf("%w: the branch's diff against its merge base is %d lines over the card's %d-line bound; merge main, run the checks, and push again — if the merge cannot fit, the card closes into a successor",
		ErrPushOverBound, billed-bound, bound)
}

// ChangedLines is the insertions plus deletions between two commits,
// trees or both; a diff git cannot run is an error, never zero lines.
func ChangedLines(g PushGit, send, a, b string) (int, error) {
	short, err := g.Git(send, "diff", "--shortstat", a, b)
	if err != nil {
		return 0, err
	}
	return shortstatLines(short), nil
}

// shortstatPart reads git diff --shortstat's counts: "3 files changed,
// 40 insertions(+), 7 deletions(-)", parts absent when zero.
var shortstatPart = regexp.MustCompile(`(\d+) (file|insertion|deletion)`)

// shortstatLines is a shortstat's insertions plus deletions.
func shortstatLines(s string) int {
	changed := 0
	for _, m := range shortstatPart.FindAllStringSubmatch(s, -1) {
		if m[2] == "file" {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		changed += n
	}
	return changed
}

// fits is whether the diff between a and b holds the bound; a diff that
// cannot run does not.
func fits(g PushGit, send, a, b string, bound int) bool {
	n, err := ChangedLines(g, send, a, b)
	return err == nil && n <= bound
}

// mergedTree is the three-way merge of head and mainHead over base as a
// tree, when git can make it without conflicts.
func mergedTree(g PushGit, send, base, head, mainHead string) (string, error) {
	idx := filepath.Join(send, "fit-index")
	defer os.Remove(idx)
	if _, err := g.RunGit(send, []string{"GIT_INDEX_FILE=" + idx}, "read-tree", "-m", base, head, mainHead); err != nil {
		return "", err
	}
	return g.RunGit(send, []string{"GIT_INDEX_FILE=" + idx}, "write-tree")
}

// RefuseWorkflowDirChange stops a push whose tree entries for the
// workflow directories (.forgejo, .gitea, .github) differ from the
// base's: Forgejo's protected_file_patterns rejects every new factory/*
// branch outright, so branch protection cannot gate this — the pusher,
// trusted, is where the guard lives (the worker runs it with the agent
// already dead; factoryd's interim push in a repo it built itself).
// Object ids are compared, never a diff's path listing, so a rename out
// of the directory, a quoted non-ASCII name or a mode change all show.
// Computed against own, the repo the pusher itself fetched and staged,
// never the agent-controlled repo. The refusal carries the action: the
// rule lets no agent change a workflow directory, so the change is
// reverted.
func RefuseWorkflowDirChange(g PushGit, own, base, head string) error {
	was, err := workflowTreeEntries(g, own, base)
	if err != nil {
		return err
	}
	now, err := workflowTreeEntries(g, own, head)
	if err != nil {
		return err
	}
	for _, dir := range WorkflowDirs {
		if was[dir] != now[dir] {
			return fmt.Errorf("refusing to push: %s/ differs from the base (%s): revert the workflow directory change; the rule lets no agent change it", dir, base)
		}
	}
	return nil
}

// workflowTreeEntries maps each workflow directory to the ls-tree lines
// (mode, type, object id, name) of every top-level entry in commit naming
// it in any case (WorkflowDir); an absent directory has no key. A commit
// that does not exist fails, it never reads as "no workflow directories".
func workflowTreeEntries(g PushGit, own, commit string) (map[string]string, error) {
	out, err := g.Git(own, "ls-tree", commit)
	if err != nil {
		return nil, err
	}
	entries := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		_, name, _ := strings.Cut(line, "\t")
		if dir, ok := WorkflowDir(name); ok {
			entries[dir] += line + "\n"
		}
	}
	return entries, nil
}

// migrationName is the pattern a migration file carries: a UTC timestamp
// and a slug, never the next number (roadmap 186's guide sentence, now a
// guard).
var migrationName = regexp.MustCompile(`^[0-9]{14}_[a-z0-9_]+\.sql$`)

// RefuseMisnamedMigrations stops a push that adds a .sql file under one
// of the policy's migration directory globs — a glob with ** in it; an
// exact path like internal/policy/schema.sql marks one file for review,
// not a directory of timestamped migrations — whose name is off the
// pattern. Only added files are judged: an edited migration keeps its
// name. The listing runs with --no-renames and core.quotePath off, so a
// rename into a bad name lists as an add and a non-ASCII path prints raw
// (RefuseWorkflowDirChange's comment names the same holes). Like it a
// guard of the trusted pusher, run in the repo it built itself; the
// refusal reaches the next session as a finding.
func RefuseMisnamedMigrations(g PushGit, globs []string, own, base, head string) error {
	var dirs []string
	for _, glob := range globs {
		if strings.Contains(glob, "**") {
			dirs = append(dirs, glob)
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	out, err := g.Git(own, "-c", "core.quotePath=off", "diff", "--name-only", "--diff-filter=A", "--no-renames", base, head)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(out, "\n") {
		if !strings.HasSuffix(p, ".sql") || !slices.ContainsFunc(dirs, func(glob string) bool { return policy.GlobMatch(glob, p) }) {
			continue
		}
		if !migrationName.MatchString(path.Base(p)) {
			return fmt.Errorf("refusing to push: migration %s is misnamed: <UTC yyyymmddhhmmss>_<slug>.sql, never the next number", p)
		}
	}
	return nil
}
