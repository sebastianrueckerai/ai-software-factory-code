package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The template pass (roadmap 165) brings a template's later changes to the
// projects seeded from it as one pull request, TemplateUpdateTitle, from
// TemplateUpdateBranch on factory-projects' fork of the repository: from a
// fork, its jobs hold a read-only token like every other pull request's
// (ruling 97). factoryd files no task for it and never merges it.
const (
	TemplateUpdateBranch = "factory/template-update"
	TemplateUpdateTitle  = "Template update"
	templateDelay        = 5 * time.Minute
	templateEvery        = time.Hour
)

// TemplateUpdates runs the template pass on every ready factory project
// with a template, one log line each.
func (r *Registrar) TemplateUpdates(ctx context.Context) {
	ps, err := r.Store.Projects(ctx)
	if err != nil {
		log.Printf("projects: %v", err)
		return
	}
	for _, p := range ps {
		if !p.File || p.Objects != policy.ObjectsFactory || p.ReadyAt.IsZero() || p.Template == "" {
			continue
		}
		c, err := r.templateUpdate(ctx, p)
		if err != nil {
			log.Printf("project %s: template update: %v", p.Name, err)
			continue
		}
		log.Printf("project %s: template update: %s", p.Name, c)
	}
}

// templateUpdate renders p's template as the seed does and compares main's
// .forgejo/ files with it, deploy/smoke.sh, which the card that owns
// deploy/ fills, only when main lacks it, and its whole deploy/ when main
// has none. It keeps the pull request to the files that differ: opened,
// brought to them, or closed when none does.
func (r *Registrar) templateUpdate(ctx context.Context, p Project) (Change, error) {
	if p.TemplateFiles == nil {
		return "", fmt.Errorf("template %q is not in the configuration version in force", p.Template)
	}
	rendered, err := policy.RenderTemplate(p.TemplateFiles, r.templateData(p))
	if err != nil {
		return "", err
	}
	f, repo := r.Forgejo.As(r.ProjectsUser, r.Forgejo.ProjectsToken), p.Repo()
	deployed, err := f.ProjectFileExists(ctx, repo, "main", "deploy")
	if err != nil {
		return "", err
	}
	want, why := map[string][]byte{}, map[string]string{}
	for path, content := range rendered {
		if !deployed && strings.HasPrefix(path, "deploy/") {
			want[path], why[path] = content, "deploy/ missing"
			continue
		}
		if !strings.HasPrefix(path, ".forgejo/") && path != "deploy/smoke.sh" {
			continue
		}
		have, _, err := f.FileContent(ctx, repo.Owner, repo.Name, "main", path)
		switch {
		case errors.Is(err, ErrNotFound):
			want[path], why[path] = content, "missing"
		case err != nil:
			return "", err
		case path != "deploy/smoke.sh" && !bytes.Equal(have, content):
			want[path], why[path] = content, "differs"
		}
	}
	pr, err := f.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, f.Fork(repo), TemplateUpdateBranch)
	switch {
	case err != nil:
		return "", err
	case len(want) == 0 && pr == nil:
		return Unchanged, nil
	case len(want) == 0:
		return r.closeTemplateUpdate(ctx, f, repo, pr.Number, "main's files equal the template's")
	case pr == nil:
		return openTemplateUpdate(ctx, f, p, want, templateUpdateBody(p.Template, why))
	}
	return r.keepTemplateUpdate(ctx, f, p, *pr, want, templateUpdateBody(p.Template, why))
}

// openTemplateUpdate cuts the branch from main on the fork, made and
// synced first, with want, and opens the pull request. A branch a closed
// pull request left is deleted instead, and the pull request waits for
// the next pass: Forgejo closes a deleted branch's pull requests in the
// background, which could take a new one with them.
func openTemplateUpdate(ctx context.Context, f *Forgejo, p Project, want map[string][]byte, body string) (Change, error) {
	repo := p.Repo()
	fork := f.Fork(repo)
	_, err := f.BranchHeadSHA(ctx, fork.Owner, fork.Name, TemplateUpdateBranch)
	if err == nil {
		return Change("waiting: deleted the branch " + fork.String() + ":" + TemplateUpdateBranch + " a closed pull request left; the next pass opens a new one"),
			f.DeleteBranch(ctx, fork.Owner, fork.Name, TemplateUpdateBranch)
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if err := f.EnsureFork(ctx, repo); err != nil {
		return "", err
	}
	if err := f.SyncFork(ctx, fork); err != nil {
		return "", err
	}
	changes, err := fileChanges(ctx, f, fork, "main", want, nil)
	if err != nil {
		return "", err
	}
	if _, err := f.CommitFiles(ctx, fork.Owner, fork.Name, "main", TemplateUpdateBranch, "Template update from "+p.Template+" (registration)", changes); err != nil {
		return "", err
	}
	ref, err := f.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+TemplateUpdateBranch, "main", TemplateUpdateTitle, body)
	if err != nil {
		return "", err
	}
	return Change(fmt.Sprintf("created pull request #%d: %s", ref.Number, strings.Join(slices.Sorted(maps.Keys(want)), ", "))), nil
}

// keepTemplateUpdate brings the open pull request's branch to want, main
// merged in first (Forgejo merges nothing into a branch up to date), and
// its description to body. A main that conflicts with the branch closes
// the pull request, which the next pass opens anew from main.
func (r *Registrar) keepTemplateUpdate(ctx context.Context, f *Forgejo, p Project, pr PullRequestRef, want map[string][]byte, body string) (Change, error) {
	repo, fork := p.Repo(), f.Fork(p.Repo())
	changes, err := branchChanges(ctx, f, repo, pr.Number, want)
	if err != nil {
		return "", err
	}
	c := Unchanged
	if len(changes) > 0 {
		err := f.UpdatePullRequest(ctx, repo.Owner, repo.Name, pr.Number)
		if errors.Is(err, ErrUpdateConflict) {
			return r.closeTemplateUpdate(ctx, f, repo, pr.Number, "main conflicts with its branch; the next pass opens it anew from main")
		}
		if err == nil {
			changes, err = branchChanges(ctx, f, repo, pr.Number, want)
		}
		if err == nil && len(changes) > 0 {
			_, err = f.CommitFiles(ctx, fork.Owner, fork.Name, TemplateUpdateBranch, "", "Template update from "+p.Template+" (registration)", changes)
		}
		if err != nil {
			return "", err
		}
		var paths []string
		for _, ch := range changes {
			paths = append(paths, ch.Path)
		}
		c = Change(fmt.Sprintf("updated pull request #%d: %s", pr.Number, strings.Join(paths, ", ")))
	}
	if pr.Body != body {
		if err := f.EditPullRequestBody(ctx, repo.Owner, repo.Name, pr.Number, body); err != nil {
			return "", err
		}
		if c == Unchanged {
			c = Change(fmt.Sprintf("updated pull request #%d: its description", pr.Number))
		}
	}
	return c, nil
}

// branchChanges bring the pull request's branch to want, and every other
// .forgejo/ or deploy/ file it changes back to main's.
func branchChanges(ctx context.Context, f *Forgejo, repo Repo, number int64, want map[string][]byte) ([]FileChange, error) {
	files, err := f.PullRequestFiles(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return nil, err
	}
	target, gone := maps.Clone(want), []string{}
	for _, file := range files {
		path := file.Filename
		if _, ok := target[path]; ok || !strings.HasPrefix(path, ".forgejo/") && !strings.HasPrefix(path, "deploy/") {
			continue
		}
		content, _, err := f.FileContent(ctx, repo.Owner, repo.Name, "main", path)
		switch {
		case errors.Is(err, ErrNotFound):
			gone = append(gone, path)
		case err != nil:
			return nil, err
		default:
			target[path] = content
		}
	}
	return fileChanges(ctx, f, f.Fork(repo), TemplateUpdateBranch, target, gone)
}

// fileChanges are the changes that bring ref of repo to files and delete
// the paths of gone it has.
func fileChanges(ctx context.Context, f *Forgejo, repo Repo, ref string, files map[string][]byte, gone []string) ([]FileChange, error) {
	var out []FileChange
	for _, path := range slices.Concat(slices.Sorted(maps.Keys(files)), gone) {
		have, blob, err := f.FileContent(ctx, repo.Owner, repo.Name, ref, path)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		content, keep := files[path]
		switch {
		case !keep && blob != "":
			out = append(out, FileChange{Path: path, Blob: blob, Delete: true})
		case keep && (blob == "" || !bytes.Equal(have, content)):
			out = append(out, FileChange{Path: path, Content: content, Blob: blob})
		}
	}
	return out, nil
}

// closeTemplateUpdate closes pull request number, saying why in a comment
// by the bot (factory-projects' token has no issue scope), and deletes its
// branch.
func (r *Registrar) closeTemplateUpdate(ctx context.Context, f *Forgejo, repo Repo, number int64, why string) (Change, error) {
	if _, err := r.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, number, "factoryd: closing this pull request: "+why+"."); err != nil {
		return "", err
	}
	if err := f.ClosePullRequest(ctx, repo.Owner, repo.Name, number); err != nil {
		return "", err
	}
	fork := f.Fork(repo)
	return Change(fmt.Sprintf("closed pull request #%d: %s", number, why)), f.DeleteBranch(ctx, fork.Owner, fork.Name, TemplateUpdateBranch)
}

// templateUpdateBody lists each file the pull request writes and why.
func templateUpdateBody(template string, why map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Template %s, rendered for this project as registration seeds it, differs from main in these files:\n\n", template)
	for _, path := range slices.Sorted(maps.Keys(why)) {
		fmt.Fprintf(&b, "- `%s`: %s\n", path, why[path])
	}
	b.WriteString("\nfactoryd keeps this pull request to the template every hour and closes it once main equals it. It is no card: factoryd files no task for it and never merges it, an approver does.\n")
	return b.String()
}
