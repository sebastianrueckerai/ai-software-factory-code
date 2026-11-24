package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/bump"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// bumpPass is a run of routine rt, a pull-request routine of pass bump, on
// project p: the weekly bump's lookup in the bump pod over main's files,
// its commit on the bot's fork at a new card's branch, the copy of what it
// adds into the store, its pull request, and that pull request's card. The
// card is the routine's agent card with its pull request on its row: its
// run verifies the pull request first and gets a session only when a check
// fails, as any card does. A bump that moves no pin files nothing: the
// zero Task.
func (r *Runner) bumpPass(ctx context.Context, p Project, rt policy.Routine, brief, provenance string) (Task, IssueRef, error) {
	if r.Upstream == nil {
		return Task{}, IssueRef{}, terminalError("routine %s: the bump runs in the upstream pod, and this factoryd has none", rt.Name)
	}
	repo := p.Repo()
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return Task{}, IssueRef{}, fmt.Errorf("main's head: %w", err)
	}
	main := map[string][]byte{}
	for _, path := range slices.Concat(bump.Files, upstreamScripts) {
		if main[path], err = r.forgejo.FileAt(ctx, repo.Owner, repo.Name, head, path); err != nil {
			return Task{}, IssueRef{}, fmt.Errorf("main's %s: %w", path, err)
		}
	}
	files := map[string][]byte{}
	for _, path := range bump.Files {
		files[path] = main[path]
	}
	res, err := r.Upstream.Bump(ctx, files)
	if err != nil {
		return Task{}, IssueRef{}, err
	}
	if len(res.Files) == 0 {
		return Task{}, IssueRef{}, nil
	}
	want := map[string][]byte{}
	for path, s := range res.Files {
		if !slices.Contains(bump.Files, path) {
			return Task{}, IssueRef{}, terminalError("the bump pod handed back %s, which is no file of a bump", path)
		}
		want[path] = []byte(s)
	}
	text := strings.TrimSpace(res.Report.Text())
	if findings := r.copyBump(ctx, main, want); len(findings) > 0 {
		text += "\n\nThe copy into the store before the checks could not copy:\n- " + strings.Join(findings, "\n- ")
	}
	text = r.untrusted(Task{Repository: p.Repository}, text)

	b := RoutineCard(rt, brief)
	b.Summary = bumpSummary(res.Report)
	b.Body = strings.TrimSpace(b.Body + "\n\n" + text)
	t := Task{ID: NewTaskID(), RiskClass: "standard", BudgetEUR: r.cfg.Policy().Document.Budget.ClampBudget(0),
		Objective: IssueObjective(b.Title, b.Body), Source: SourceRoutine}
	b.Body += "\n\n" + provenance
	branch, fork := taskBranch(t), r.forgejo.Fork(repo)
	if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
		return Task{}, IssueRef{}, err
	}
	if err := r.forgejo.SyncFork(ctx, fork); err != nil {
		return Task{}, IssueRef{}, err
	}
	changes, err := fileChanges(ctx, r.forgejo, fork, prBase, want, nil)
	if err != nil {
		return Task{}, IssueRef{}, err
	}
	if _, err := r.forgejo.CommitFiles(ctx, fork.Owner, fork.Name, prBase, branch, Attribution{Role: "routine", Task: t.ID}, rt.Title, changes); err != nil {
		return Task{}, IssueRef{}, err
	}
	pr, err := r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, rt.Title, text)
	if err != nil {
		return Task{}, IssueRef{}, errors.Join(err, r.forgejo.DeleteBranch(context.WithoutCancel(ctx), fork.Owner, fork.Name, branch))
	}
	t.PRNumber = pr.Number
	t, ref, err := FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, p, b, []string{"routine"}, t)
	if err != nil {
		// a pull request without its card would stand open with no one to
		// answer its checks
		c := context.WithoutCancel(ctx)
		return Task{}, IssueRef{}, errors.Join(err, r.forgejo.ClosePullRequest(c, repo.Owner, repo.Name, pr.Number), r.forgejo.DeleteBranch(c, fork.Owner, fork.Name, branch))
	}
	if err := r.forgejo.EditPullRequestBody(ctx, repo.Owner, repo.Name, pr.Number, fmt.Sprintf("%s\n\nCloses #%d\n\nOpened by the factory for card #%d.", text, ref.Number, ref.Number)); err != nil {
		log.Printf("routine %s: pull request #%d: name its card #%d: %v", rt.Name, pr.Number, ref.Number, err)
	}
	return t, ref, nil
}

// bumpSummary is the summary of a bump's card: how many pins moved, that
// an agent works only on a failed check, and each pin the lookup could not
// resolve.
func bumpSummary(r bump.Report) string {
	pins := fmt.Sprintf("%d pins", len(r.Moved))
	if len(r.Moved) == 1 {
		pins = "1 pin"
	}
	s := "The factory's lookup moved " + pins + " in this card's pull request. Its checks and review judge it, and an agent works on it only when a check fails."
	if len(r.Failed) > 0 {
		names := make([]string, len(r.Failed))
		for i, f := range r.Failed {
			name, _, _ := strings.Cut(f, ": ")
			names[i], _, _ = strings.Cut(name, "@")
		}
		s += " Not resolved, kept as they were: " + strings.Join(names, ", ") + "."
	}
	return textutil.Truncate(s, summaryMax)
}

// copyBump copies into the store what the bump's files add, before its
// pull request starts the checks, with main's scripts, and returns the
// lines the copy could not make, its failure among them: the card's own
// verify copies the head again.
func (r *Runner) copyBump(ctx context.Context, main, want map[string][]byte) []string {
	files := map[string][]byte{}
	for _, path := range slices.Concat(upstreamHeadFiles, upstreamScripts) {
		files[path] = main[path]
		if b, ok := want[path]; ok {
			files[path] = b
		}
	}
	findings, err := r.Upstream.Copy(ctx, files)
	if err != nil {
		findings = append(findings, "the copy failed: "+err.Error())
	}
	return findings
}
