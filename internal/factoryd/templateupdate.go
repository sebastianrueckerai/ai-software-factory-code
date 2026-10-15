package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The template pass (roadmap 165) brings a template's later changes to the
// projects seeded from it as one pull request, TemplateUpdateTitle, from
// TemplateUpdateBranch on factory-projects' fork of the repository: from a
// fork, its jobs hold a read-only token like every other pull request's
// (ruling 97). Since roadmap 187 the pull request is a card: openTemplateUpdate
// files it with FileFactoryPRTask, so the chat, the board and the merge rule
// cover it; the card's merge merges the pull request, its close closes it.
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
		return r.openTemplateUpdate(ctx, p, want, templateUpdateBody(p.Template, why))
	}
	return r.keepTemplateUpdate(ctx, f, p, *pr, want, templateUpdateBody(p.Template, why))
}

// openTemplateUpdate runs the template pass's opening branch of project p:
// it cuts the branch from main on the fork, made and synced first, with
// want, opens the pull request and files its card (roadmap 187). A branch
// a closed pull request left goes to leftBranch instead.
func (r *Registrar) openTemplateUpdate(ctx context.Context, p Project, want map[string][]byte, body string) (Change, error) {
	f, repo := r.Forgejo.As(r.ProjectsUser, r.Forgejo.ProjectsToken), p.Repo()
	fork := f.Fork(repo)
	_, err := f.BranchHeadSHA(ctx, fork.Owner, fork.Name, TemplateUpdateBranch)
	if err == nil {
		return r.leftBranch(ctx, f, p, want)
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
	if _, err := f.CommitFiles(ctx, fork.Owner, fork.Name, "main", TemplateUpdateBranch, Attribution{Role: "registration"}, "Template update from "+p.Template+" (registration)", changes); err != nil {
		return "", err
	}
	ref, err := f.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+TemplateUpdateBranch, "main", TemplateUpdateTitle, body)
	if err != nil {
		// a branch without its pull request would read as the last one closed
		return "", errors.Join(err, f.DeleteBranch(ctx, fork.Owner, fork.Name, TemplateUpdateBranch))
	}
	if _, _, err := r.fileTemplateCard(ctx, p, ref.Number, strings.Join(slices.Sorted(maps.Keys(want)), ", ")); err != nil {
		return "", err
	}
	return Change(fmt.Sprintf("created pull request #%d: %s", ref.Number, strings.Join(slices.Sorted(maps.Keys(want)), ", "))), nil
}

// fileTemplateCard files the card of the template update pull request
// number on project p (roadmap 187): source template-update, so its run
// verifies the open pull request instead of opening one. Without a card
// store it files none.
func (r *Registrar) fileTemplateCard(ctx context.Context, p Project, number int64, files string) (Task, IssueRef, error) {
	if r.Cards == nil {
		return Task{}, IssueRef{}, nil
	}
	title := TemplateUpdateTitle + " for " + p.Template
	body := "factoryd opened pull request #" + strconv.FormatInt(number, 10) + " (" + TemplateUpdateTitle + ") for template " + p.Template +
		": it brings the template's later changes to this project. Its files: " + files + ". The card's merge merges the pull request.\n"
	return FileFactoryPRTask(ctx, r.Cards, r.Forgejo, r.Workflows, r.Timeline, p, title, body, []string{"improvement"},
		Task{ID: NewTaskID(), Source: SourceTemplateUpdate}, number)
}

// cardByPR is the newest card the template update pull request number on
// repo has, if any; a Registrar without a card store has none, and the
// pass behaves as before roadmap 187.
func (r *Registrar) cardByPR(ctx context.Context, repo Repo, number int64) (Task, bool, error) {
	if r.Cards == nil {
		return Task{}, false, nil
	}
	cards, err := r.Cards.TasksByPR(ctx, repo.String(), number)
	if err != nil || len(cards) == 0 {
		return Task{}, false, err
	}
	return cards[0], true, nil
}

// cardDue reports why the template update pull request number on repo is
// due a card, "" when it is not. It is due one when it has none (it came
// before roadmap 187), and one more when its newest card failed by
// factoryd's own doing, no verdict on the pull request; failed is that
// card. Either the policy gate refused it as forbidden to agents (the
// final reason on its timeline), as it refused a factory pull request's
// card before such a pull request was no agent's change, and the card
// filed for it is never refused so. Or it is the pull request's one card
// and failed before any gate judged it (finding 224): a second card that
// fails leaves it be, so the pass files again once per pull request.
// Without a card store no card is due.
func (r *Registrar) cardDue(ctx context.Context, repo Repo, number int64) (failed *Task, why string, err error) {
	if r.Cards == nil {
		return nil, "", nil
	}
	cards, err := r.Cards.TasksByPR(ctx, repo.String(), number)
	switch {
	case err != nil:
		return nil, "", err
	case len(cards) == 0:
		return nil, "it has no card", nil
	case cards[0].Status != "failed":
		return nil, "", nil
	}
	tl, err := r.Timeline.Objects.Get(ctx, "sessions/"+cards[0].ID+"/timeline.jsonl")
	if err != nil {
		return nil, "", err
	}
	if reason, _ := lastStep(tl, "final")["reason"].(string); strings.Contains(reason, refusedToAgents) {
		return &cards[0], "was refused as forbidden to agents, which a factory pull request is not", nil
	}
	if len(cards) > 1 {
		return nil, "", nil
	}
	judged, err := r.Cards.ReachedReview(ctx, cards[0].ID)
	if err != nil || judged {
		return nil, "", err
	}
	return &cards[0], "failed before any gate judged it", nil
}

// refusedToAgents is in the reason of a card the policy gate refused as an
// agent's change to a class forbidden to agents.
const refusedToAgents = " is forbidden to agents: "

// fileDueCard files the card pull request number is due (cardDue). The
// failed card it replaces closes as factoryd's recorded close, its issue
// naming the new card, as a retried plan card's does.
func (r *Registrar) fileDueCard(ctx context.Context, p Project, number int64, failed *Task, want map[string][]byte) error {
	_, ref, err := r.fileTemplateCard(ctx, p, number, strings.Join(slices.Sorted(maps.Keys(want)), ", "))
	if err != nil || failed == nil {
		return err
	}
	repo := p.Repo()
	if _, err := r.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, failed.IssueNumber, fmt.Sprintf("factoryd: Filed again as #%d.", ref.Number)); err != nil {
		return err
	}
	if err := recordFactoryClose(ctx, r.Cards, *failed); err != nil {
		return err
	}
	return r.Forgejo.CloseIssue(ctx, repo.Owner, repo.Name, failed.IssueNumber)
}

// leftBranch keeps the branch a closed pull request left when a person
// closed it unmerged and it still holds want (ruling 296): the close stands
// until the render changes. A close by its card's failure of factoryd's
// own doing (cardDue) is no person's: the pass reopens the pull request, as
// the bot that closed it, and files its card again. Otherwise it
// deletes the branch, and the next pass opens the new pull request: Forgejo
// closes a deleted branch's pull requests in the background, which could
// take a new one with them.
func (r *Registrar) leftBranch(ctx context.Context, f *Forgejo, p Project, want map[string][]byte) (Change, error) {
	repo, fork := p.Repo(), f.Fork(p.Repo())
	last, err := f.LastClosedPullRequestFrom(ctx, repo.Owner, repo.Name, f.BotUser, fork, TemplateUpdateBranch)
	if err != nil {
		return "", err
	}
	if last != nil && !last.Merged {
		changes, err := fileChanges(ctx, f, fork, TemplateUpdateBranch, want, nil)
		if err != nil {
			return "", err
		}
		if len(changes) == 0 {
			failed, why, err := r.cardDue(ctx, repo, last.Number)
			if err != nil {
				return "", err
			}
			if failed == nil {
				return Change(fmt.Sprintf("waiting: #%d closed unmerged; the template's next change reopens it", last.Number)), nil
			}
			if err := r.Forgejo.ReopenPullRequest(ctx, repo.Owner, repo.Name, last.Number); err != nil {
				return "", err
			}
			if err := r.fileDueCard(ctx, p, last.Number, failed, want); err != nil {
				return "", err
			}
			return Change(fmt.Sprintf("reopened pull request #%d: card %s %s, filed again", last.Number, failed.ID, why)), nil
		}
	}
	return Change("waiting: deleted the branch " + fork.String() + ":" + TemplateUpdateBranch + " a closed pull request left; the next pass opens a new one"),
		f.DeleteBranch(ctx, fork.Owner, fork.Name, TemplateUpdateBranch)
}

// keepTemplateUpdate brings the open pull request's branch to want, and
// its description to body. Main is merged in first when the branch needs
// changes or Forgejo finds it not mergeable (Forgejo merges nothing into a
// branch up to date); a main that conflicts with the branch closes the
// pull request, which the next pass opens anew from main.
func (r *Registrar) keepTemplateUpdate(ctx context.Context, f *Forgejo, p Project, pr PullRequestRef, want map[string][]byte, body string) (Change, error) {
	repo, fork := p.Repo(), f.Fork(p.Repo())
	changes, err := branchChanges(ctx, f, repo, pr.Number, want)
	if err != nil {
		return "", err
	}
	c := Unchanged
	if len(changes) > 0 || !pr.Mergeable {
		err := f.UpdatePullRequest(ctx, repo.Owner, repo.Name, pr.Number)
		if errors.Is(err, ErrUpdateConflict) {
			return r.closeTemplateUpdate(ctx, f, repo, pr.Number, "main conflicts with its branch; the next pass opens it anew from main")
		}
		if err == nil {
			changes, err = branchChanges(ctx, f, repo, pr.Number, want)
		}
		if err == nil && len(changes) > 0 {
			_, err = f.CommitFiles(ctx, fork.Owner, fork.Name, TemplateUpdateBranch, "", Attribution{Role: "registration"}, "Template update from "+p.Template+" (registration)", changes)
		}
		if err != nil {
			return "", err
		}
		paths := []string{}
		for _, ch := range changes {
			paths = append(paths, ch.Path)
		}
		if len(paths) == 0 {
			paths = append(paths, "main merged in")
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
	if failed, why, err := r.cardDue(ctx, repo, pr.Number); err != nil {
		return "", err
	} else if why != "" {
		if err := r.fileDueCard(ctx, p, pr.Number, failed, want); err != nil {
			return "", err
		}
	}
	return c, nil
}

// branchChanges bring the pull request's branch to want, and every other
// file it changes back to main's, so its diff carries nothing else.
func branchChanges(ctx context.Context, f *Forgejo, repo Repo, number int64, want map[string][]byte) ([]FileChange, error) {
	files, err := f.PullRequestFiles(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return nil, err
	}
	target, gone := maps.Clone(want), []string{}
	for _, file := range files {
		path := file.Filename
		if _, ok := target[path]; ok {
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

// closeTemplateUpdate deletes pull request number's branch, so none
// outlives a close of factoryd's own (leftBranch), closes it, which
// Forgejo also does for a deleted branch, and only then says why in a
// comment by the bot (factory-projects' token has no issue scope): no
// comment is posted twice. It closes the card's issue first when one
// exists (roadmap 187): the close ends the card as an approver's would.
func (r *Registrar) closeTemplateUpdate(ctx context.Context, f *Forgejo, repo Repo, number int64, why string) (Change, error) {
	fork := f.Fork(repo)
	if task, ok, err := r.cardByPR(ctx, repo, number); err != nil {
		return "", err
	} else if ok {
		if err := r.Forgejo.CloseIssue(ctx, repo.Owner, repo.Name, task.IssueNumber); err != nil {
			return "", fmt.Errorf("close card %s: %w", task.ID, err)
		}
	}
	if err := f.DeleteBranch(ctx, fork.Owner, fork.Name, TemplateUpdateBranch); err != nil {
		return "", err
	}
	if err := f.ClosePullRequest(ctx, repo.Owner, repo.Name, number); err != nil {
		return "", err
	}
	if _, err := r.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, number, "factoryd: closing this pull request: "+why+"."); err != nil {
		return "", err
	}
	return Change(fmt.Sprintf("closed pull request #%d: %s", number, why)), nil
}

// templateUpdateBody lists each file the pull request writes and why.
func templateUpdateBody(template string, why map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Template %s, rendered for this project as registration seeds it, differs from main in these files:\n\n", template)
	for _, path := range slices.Sorted(maps.Keys(why)) {
		fmt.Fprintf(&b, "- `%s`: %s\n", path, why[path])
	}
	b.WriteString("\nfactoryd keeps this pull request to the template every hour and closes it once main equals it. It is a card: factoryd filed one for it, whose gates an approver's approval passes and whose merge merges this pull request.\n")
	return b.String()
}
