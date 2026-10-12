package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The schema pass (roadmap 166): infra's copies of factoryd's generated
// schemas follow the code. `factoryd config check` only warns about a
// stale copy; factoryd itself opens ONE pull request on the configuration
// repository that brings factory/schema/ to what this binary generates, as
// factory-projects, whose token registration uses for project
// repositories. An approver merges it like any other project file: nothing
// merges it automatically.

// SchemaBranchPrefix is the branch's prefix; the id after it is the short
// sha of the build factoryd runs, or the hash of the generated schema
// content when the binary does not know its commit.
const (
	SchemaBranchPrefix = "factory/schema-"
	schemaTitlePrefix  = "Schema copies for "
	schemaPathPrefix   = policy.DirSchema + "/"
	// schemaBase is the branch a schema copy pull request merges into:
	// the loader reads the tree from it, so the copies sit on it too.
	schemaBase = "main"
)

// SchemaCopier keeps the configuration repository's copies of the
// generated schemas from going stale silently: once per process at boot it
// compares them with what this binary generates and opens, or leaves open,
// one pull request that writes the changed files whole.
type SchemaCopier struct {
	Forgejo *Forgejo
	// Token writes the configuration repository: factory-projects', the
	// token registration uses for project repositories.
	Token string
	// Repo is the configuration repository; its tree directory and the
	// read of it at a commit come from Config.
	Repo Repo
	// Config reads the tree at a commit, schema/ left out.
	Config *ForgejoConfig
	// Head is the head commit of the configuration version in force, ""
	// while none is known.
	Head func() string
	// Build names this binary: the short sha of the commit it was built
	// from, or the hash of the generated schema content when it does not
	// know its commit.
	Build func() string
}

// SyncOnce runs the comparison once per process, after the boot's loader
// has a configuration version in hand. A read that fails is tried again up
// to tries times, wait apart, while ctx lives: a cold Forgejo may refuse
// the first read. It returns the last error; a caller logs it and never
// fails a boot over it.
func (c *SchemaCopier) SyncOnce(ctx context.Context, tries int, wait time.Duration) error {
	var err error
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = c.sync(ctx); err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func (c *SchemaCopier) sync(ctx context.Context) error {
	build := strings.TrimSpace(c.Build())
	if build == "" {
		return errors.New("schema copies: this binary names no build")
	}
	head := strings.TrimSpace(c.Head())
	if head == "" {
		return errors.New("schema copies: no configuration commit in hand")
	}
	// reads go with the credential that reads the tree; writes and the
	// close of a stale request carry factory-projects' token, the one
	// registration uses for project repositories. The comment on a closed
	// request goes as the bot, whose token alone has the issue scope.
	w := c.Forgejo.As("", c.Token)
	title := schemaTitle(build)
	visible, err := c.Forgejo.RepoVisible(ctx, c.Repo)
	if err != nil {
		return err
	}
	if !visible {
		log.Printf("schema copies: %s is not visible to this token: nothing filed", c.Repo)
		return nil
	}
	copies, err := c.readCopies(ctx, head)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		// the configuration commit holds no readable factory/ tree: every
		// generated copy is one to write
		copies = nil
	}
	diffs, err := policy.DiffSchemas(copies)
	if err != nil {
		return err
	}
	open, err := c.Forgejo.OpenPullRequests(ctx, c.Repo.Owner, c.Repo.Name)
	if err != nil {
		return err
	}
	written, why := map[string][]byte{}, map[string]string{}
	for _, d := range diffs {
		written[schemaPathInRepo(c.Config.Dir, d.Path)] = d.Want
		why[d.Path] = schemaWhy(d)
	}
	if len(diffs) == 0 {
		return c.closeStale(ctx, open, title, build, nil)
	}
	branch := SchemaBranchPrefix + shortID(build)
	// Forgejo answers one head branch with one open pull request: an
	// older build's title on this branch is a manual edit, which
	// factoryd leaves to a person rather than filing over it.
	if pr := openForBranch(open, branch); pr != nil {
		if pr.Title == title {
			log.Printf("schema copies: %d stale of %s/: pull request #%d (%s) already open", len(diffs), c.Config.Dir, pr.Number, title)
			return c.closeStale(ctx, open, title, build, &branch)
		}
		log.Printf("schema copies: branch %s holds pull request #%d titled %q, not %q: left alone", branch, pr.Number, pr.Title, title)
		return nil
	}
	if pr := openSchemaByOtherBranch(open, branch); pr != nil && pr.Title == title {
		// This build's own request stands open from a branch that is not
		// its own (renamed or deleted since): nothing to file over it.
		log.Printf("schema copies: pull request #%d (%s) already open from %s", pr.Number, pr.Title, pr.Head)
		return nil
	}
	// The request this build opens replaces every older titled schema
	// request, whether or not it still stands open: a close racing an
	// earlier boot's pass must not leave the comment to a second pass.
	for _, pr := range open {
		if isSchemaTitle(pr.Title) && pr.Title != title {
			if err := c.closeWithComment(ctx, &pr, build); err != nil {
				return err
			}
		}
	}
	// The changed files go onto the build's branch as one commit, every
	// one written whole: created when Forgejo makes the branch with the
	// first file of an empty repository, updated onto it when it exists
	// (a closed pull request's branch survives its merge or close).
	if _, err := w.UpdateFiles(ctx, c.Repo, branch, schemaBase, title, written); err != nil {
		return fmt.Errorf("schema copies: write %s: %w", branch, err)
	}
	ref, err := w.CreatePullRequest(ctx, c.Repo.Owner, c.Repo.Name, branch, schemaBase, title, schemaBody(build, why))
	if err != nil {
		return errors.Join(fmt.Errorf("schema copies: open pull request from %s: %w", branch, err), w.DeleteBranch(ctx, c.Repo.Owner, c.Repo.Name, branch))
	}
	log.Printf("schema copies: created pull request #%d (%s): %s", ref.Number, title, strings.Join(sortedPaths(written), ", "))
	return nil
}

// RepoVisible reads whether the credential f acts with sees repo at all:
// false on a 404, which is also Forgejo's answer for a repository it hides.
func (f *Forgejo) RepoVisible(ctx context.Context, repo Repo) (bool, error) {
	_, err := f.call(ctx, http.MethodGet, "/api/v1/repos/"+repo.String(), nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// BranchExists reads whether repo holds branch, as the token f acts with.
func (f *Forgejo) BranchExists(ctx context.Context, repo Repo, branch string) (bool, error) {
	_, err := f.BranchHeadSHA(ctx, repo.Owner, repo.Name, branch)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// UpdateFiles commits files, keyed by repository path, onto a branch as
// whoever f acts with: each file the branch holds whose content differs
// becomes an update carrying its blob sha, each file it lacks a create. It
// returns the commit's sha, "" when the branch already holds them.
func (f *Forgejo) UpdateFiles(ctx context.Context, repo Repo, branch, base, message string, files map[string][]byte) (string, error) {
	exists, err := f.BranchExists(ctx, repo, branch)
	if err != nil {
		return "", err
	}
	if !exists {
		// Forgejo keeps no branch without a commit: the new one starts at
		// base and takes the files below.
		if err := f.CopyBranch(ctx, repo, base, branch); err != nil {
			return "", err
		}
	}
	changes := make([]FileChange, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		have, blob, err := f.FileContent(ctx, repo.Owner, repo.Name, branch, path)
		if errors.Is(err, ErrNotFound) {
			// Forgejo hides the file: on a branch it had no commit for,
			// this create is what starts the branch.
			changes = append(changes, FileChange{Path: path, Content: files[path]})
			continue
		}
		if err != nil {
			return "", err
		}
		if !bytes.Equal(have, files[path]) {
			changes = append(changes, FileChange{Path: path, Content: files[path], Blob: blob})
		}
	}
	if len(changes) == 0 {
		return "", nil
	}
	return f.CommitFiles(ctx, repo.Owner, repo.Name, branch, "", message, changes)
}

// readCopies are the configuration repository's schema/ files at commit,
// keyed relative to Dir as ParseTree and DiffSchemas key them: the loader
// leaves schema/ out of the tree it reads, so they come from the git tree.
func (c *SchemaCopier) readCopies(ctx context.Context, commit string) (map[string][]byte, error) {
	dir, err := c.Config.tree(ctx, commit, false)
	if err != nil {
		return nil, err
	}
	sha := ""
	for _, e := range dir {
		if e.Path == c.Config.Dir && e.Type == "tree" {
			sha = e.SHA
		}
	}
	if sha == "" {
		return nil, fmt.Errorf("commit %s of %s has no directory %s/", commit, c.Repo, c.Config.Dir)
	}
	entries, err := c.Config.tree(ctx, sha, true)
	if err != nil {
		return nil, err
	}
	copies := map[string][]byte{}
	for _, e := range entries {
		rel, ok := strings.CutPrefix(e.Path, policy.DirSchema+"/")
		if !ok || e.Type != "blob" {
			continue
		}
		b, err := c.Config.File(ctx, commit, policy.DirSchema+"/"+rel)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // the commit moved between the listing and the read
			}
			return nil, err
		}
		copies[policy.DirSchema+"/"+rel] = b
	}
	// every name factoryd generates is there or missing: DiffSchemas reads
	// an absent copy as one to write, so no key is invented for it
	return copies, nil
}

// closeStale closes every open schema pull request of an older build, each
// with a comment naming the new one. keep, when set, is the branch this
// build's pull request came from: a differently titled request holding
// that branch is a manual edit and stands.
func (c *SchemaCopier) closeStale(ctx context.Context, open []PullRequestRef, title, build string, keep *string) error {
	for _, pr := range open {
		if !isSchemaTitle(pr.Title) || pr.Title == title {
			continue
		}
		if keep != nil && pr.Head == *keep {
			continue
		}
		if err := c.closeWithComment(ctx, &pr, build); err != nil {
			return err
		}
	}
	return nil
}

// closeWithComment closes schema pull request pr and then says which
// request replaces it: the close goes with the credential that reads the
// repository, the comment as factoryd's bot, whose token carries the issue
// scope the projects token lacks.
func (c *SchemaCopier) closeWithComment(ctx context.Context, pr *PullRequestRef, build string) error {
	if err := c.Forgejo.ClosePullRequest(ctx, c.Repo.Owner, c.Repo.Name, pr.Number); err != nil {
		return fmt.Errorf("schema copies: close #%d: %w", pr.Number, err)
	}
	replaced := fmt.Sprintf("Closed by factoryd: the schema copies of this build (%s) go to %s instead.", shortID(build), schemaTitle(build))
	if _, err := c.Forgejo.CreateIssueComment(ctx, c.Repo.Owner, c.Repo.Name, pr.Number, replaced); err != nil {
		return fmt.Errorf("schema copies: comment on #%d: %w", pr.Number, err)
	}
	log.Printf("schema copies: closed pull request #%d (%s), replaced by %s", pr.Number, pr.Title, schemaTitle(build))
	return nil
}

// openForBranch is the open pull request whose head branch is branch.
func openForBranch(open []PullRequestRef, branch string) *PullRequestRef {
	for i := range open {
		if open[i].Head == branch {
			return &open[i]
		}
	}
	return nil
}

// openSchemaByOtherBranch is the open schema pull request whose head
// branch is not branch, oldest first.
func openSchemaByOtherBranch(open []PullRequestRef, branch string) *PullRequestRef {
	for i := range open {
		if open[i].Head != branch && isSchemaTitle(open[i].Title) {
			return &open[i]
		}
	}
	return nil
}

// schemaTitle is the pull request's title: the short sha of the build when
// it has one, else the hash of the generated schema content.
func schemaTitle(build string) string {
	return schemaTitlePrefix + shortID(build)
}

// shortID is an identifier's first 12 characters, lower case: the sha of a
// commit, or of the generated content, as the title and the branch carry
// it.
func shortID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// isSchemaTitle tells a schema copy pull request's title from any other.
func isSchemaTitle(title string) bool { return strings.HasPrefix(title, schemaTitlePrefix) }

// schemaPathInRepo is a tree-relative path as the repository holds it.
func schemaPathInRepo(dir, rel string) string { return dir + "/" + rel }

func schemaWhy(d policy.SchemaDifference) string {
	if d.Missing {
		return "missing"
	}
	return "differs"
}

// schemaBody lists the files the pull request writes whole.
func schemaBody(build string, why map[string]string) string {
	names := make([]string, 0, len(why))
	for name := range why {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	fmt.Fprintf(&b, "Infra's copies of factoryd's generated JSON Schemas follow the code (roadmap 166).\n\n")
	fmt.Fprintf(&b, "factoryd build `%s` compares its schemas with `%s/` and writes the files that differ whole:\n\n",
		shortID(build), policy.DirSchema)
	for _, name := range names {
		fmt.Fprintf(&b, "- `%s` (%s)\n", name, why[name])
	}
	b.WriteString("\nThe check of the configuration tree only warns about a stale copy now; merging this brings the copies to this build.\n")
	return b.String()
}

// sameFiles reads whether body lists exactly the paths and why of the
// files, one per line as schemaBody writes them: a request whose list
// differs in any way is an older build's, not this one's.
func sameFiles(why map[string]string, body string) bool {
	got := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "- `")
		if !ok {
			continue
		}
		name, rest, _ := strings.Cut(name, "`")
		if !strings.HasPrefix(name, schemaPathPrefix) {
			continue
		}
		reason, _, _ := strings.Cut(strings.TrimPrefix(rest, " ("), ")")
		got[name] = reason
	}
	return maps.Equal(got, why)
}

func sortedPaths(files map[string][]byte) []string {
	return slices.Sorted(maps.Keys(files))
}
