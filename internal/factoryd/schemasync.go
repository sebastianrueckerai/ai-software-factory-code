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

// schemaBranchPrefix starts the branch of the schema pull request (roadmap
// 166) on the bot's fork of the configuration repository; the build id
// follows it.
const schemaBranchPrefix = "factory/schema-"

// SchemaCopier brings the configuration repository's copies of the
// generated schemas to this build: once per process it compares main's
// with its own and opens one pull request for the difference, as the bot
// from its fork, the way a roll does, and merges it as the bot once land's
// checks hold. A pull request it does not merge gets a card of its own
// through FileFactoryPRTask (roadmap 187), so the chat, the board and the
// merge rule cover it; an approver merges through that card.
type SchemaCopier struct {
	Forgejo *Forgejo       // the bot
	Config  *ForgejoConfig // reads main's tree
	Build   string         // the build id, 12 hex
	// Commit is the code commit this build's images come from, which the
	// roll writes into factoryd's Deployment; Code is the repository whose
	// history orders it against the commit main's copies name.
	Commit string
	Code   Repo
	// Store holds the tasks the copier files cards in, their projects and
	// the unfinished ones it looks a build's card up in.
	Store interface {
		CreateTask(ctx context.Context, t Task) error
		CreateHumanCard(ctx context.Context, t Task, ask HumanAsk) (Task, bool, error)
		InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
		TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
		EndClosed(ctx context.Context, id string, from []string, reason string) error
		JudgeOpenTaskAlerts(ctx context.Context, taskID, verdict, by string) error
		ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
		ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
	}
	// Workflows starts the workflow of the card it files.
	Workflows Workflows
	// Timeline records the card's events; nil records none.
	Timeline *Timeline
	// Cards ends the human card of a pull request the copier closed, which
	// no workflow sees (Intake.WithdrawPRCard); factoryd wires it with the
	// store.
	Cards interface {
		WithdrawPRCard(ctx context.Context, t Task, pr int64, why string) error
	}
	// Budget is the policy's budget block in force, which sizes the card.
	Budget func() policy.Budget
}

// SyncOnce runs the pass, tried up to tries times, wait apart, while ctx
// lives: a cold Forgejo may refuse the first read. It returns the last
// error.
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
	repo := c.Config.Repo
	head, err := c.Config.Head(ctx)
	if err != nil {
		return err
	}
	schemas, err := policy.Schemas()
	if err != nil {
		return err
	}
	copies := map[string][]byte{}
	for name := range schemas {
		path := policy.DirSchema + "/" + name
		b, err := c.Config.File(ctx, head, path)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			copies[path] = b
		}
	}
	diffs, err := policy.DiffSchemas(copies)
	if err != nil {
		return err
	}
	if len(diffs) > 0 {
		stamp, err := c.Config.File(ctx, head, policy.DirSchema+"/"+policy.SchemaCommit)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		// copies without a commit predate the commit file: any build that
		// names its commit is newer than they are
		if commit := strings.TrimSpace(string(stamp)); err == nil {
			held, err := c.Forgejo.Holds(ctx, c.Code, c.Commit, commit)
			if err != nil {
				return fmt.Errorf("order main's copies of commit %s against factoryd's %s: %w", commit, c.Commit, err)
			}
			if !held {
				// a newer build's roll is still out and its copies are ahead,
				// or another line wrote them: this pass must not undo them, and
				// it closes nothing either, for the newer build's pass sorts
				// every pull request once it runs
				log.Printf("schema copies: main's copies come from commit %s, which factoryd's commit %s does not hold: a newer build or another line wrote them; this pass writes nothing", commit, c.Commit)
				return nil
			}
		}
	}
	open, err := c.Forgejo.OpenPullRequests(ctx, repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	fork, branch := c.Forgejo.Fork(repo), schemaBranchPrefix+c.Build
	var own *PullRequestRef
	var older []PullRequestRef
	for _, pr := range open {
		switch {
		case pr.HeadRepo != fork.String() || !strings.HasPrefix(pr.Head, schemaBranchPrefix):
		case pr.Head == branch && len(diffs) > 0:
			own = &pr
		default:
			older = append(older, pr)
		}
	}
	why := "the copies on main equal the factory's schemas of build " + ShortSHA(c.Build)
	if len(diffs) > 0 {
		if own == nil {
			if own, err = c.open(ctx, fork, branch, diffs); err != nil {
				return err
			}
		}
		why = fmt.Sprintf("#%d replaces it", own.Number)
	}
	for _, pr := range older {
		if err := c.forgetBuildCard(ctx, repo.String(), pr.Number, why); err != nil {
			return err
		}
		if err := c.Forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
			return fmt.Errorf("close #%d: %w", pr.Number, err)
		}
		if _, err := c.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, "Closed by the factory: "+why+"."); err != nil {
			return fmt.Errorf("comment on #%d: %w", pr.Number, err)
		}
		log.Printf("schema copies: closed pull request #%d: %s", pr.Number, why)
	}
	// the copies are the build's own output, so the factory merges them
	// itself; a pull request it does not merge becomes a card of its own
	// (roadmap 187), so the chat, the board and the merge rule cover it.
	// The pass files one per build at most, while no other build's card
	// stands open. The link is the pull request's own: a card whose filing
	// died linking gets the number this pass has, so no second card ever
	// stands.
	if len(diffs) > 0 {
		if refusal := c.land(ctx, own.Number, schemas); refusal != "" {
			c.card(ctx, *own, pathsOf(diffs, c.Config.Dir), refusal)
		}
	}
	return nil
}

// land merges the schema pull request number as the bot, which merges the
// imager's pins on the configuration repository too, at the head it
// checked: Forgejo reports it mergeable, no check of that head fails or
// runs, and it changes this build's copies alone (foreign). Forgejo checks
// the mergeability of a new head for a moment, so land reads the pull
// request again for up to mergeWaitBound. It answers why a human merges
// it, "" when no human needs to: it merged, or it closed.
func (c *SchemaCopier) land(ctx context.Context, number int64, schemas map[string][]byte) string {
	repo := c.Config.Repo
	pr, err := c.Forgejo.PullRequestState(ctx, repo.Owner, repo.Name, number)
	for waits := 0; err == nil && pr.State == "open" && !pr.Mergeable && waits < maxMergeWaits; waits++ {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(mergeWaitBound / maxMergeWaits):
			pr, err = c.Forgejo.PullRequestState(ctx, repo.Owner, repo.Name, number)
		}
	}
	switch {
	case err != nil:
		return fmt.Sprintf("pull request #%d did not read: %v", number, err)
	case pr.State != "open":
		log.Printf("schema copies: pull request #%d is no longer open (merged: %v)", number, pr.Merged)
		return ""
	case !pr.Mergeable:
		return fmt.Sprintf("Forgejo still reported pull request #%d as not mergeable after %s", number, mergeWaitBound)
	}
	switch why, err := c.foreign(ctx, pr, schemas); {
	case err != nil:
		return fmt.Sprintf("the files of pull request #%d did not read: %v", number, err)
	case why != "":
		return why
	}
	st, err := c.Forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, pr.HeadSHA)
	if err != nil {
		return fmt.Sprintf("the checks of its head %s did not read: %v", ShortSHA(pr.HeadSHA), err)
	}
	for _, s := range st.Statuses {
		if s.State == "pending" || s.State == "failure" || s.State == "error" {
			return fmt.Sprintf("the check “%s” of its head %s has the state %s", s.Context, ShortSHA(pr.HeadSHA), s.State)
		}
	}
	if err := c.Forgejo.MergePullRequestAsBot(ctx, repo.Owner, repo.Name, number, pr.HeadSHA, Attribution{Role: "schema-copy"}, c.title()); err != nil {
		return "the bot's merge failed: " + err.Error()
	}
	log.Printf("schema copies: merged pull request #%d at %s", number, ShortSHA(pr.HeadSHA))
	return ""
}

// foreign names what pull request pr changes besides this build's copies:
// a file that is neither a copy of one of schemas nor the commit file, or
// one whose bytes at pr's head are not this build's. "" when it changes the
// copies alone.
func (c *SchemaCopier) foreign(ctx context.Context, pr PullRequestRef, schemas map[string][]byte) (string, error) {
	dir := c.Config.Dir + "/" + policy.DirSchema + "/"
	want := map[string][]byte{dir + policy.SchemaCommit: []byte(c.Commit + "\n")}
	for name, b := range schemas {
		want[dir+name] = b
	}
	files, err := c.Forgejo.PullRequestFiles(ctx, c.Config.Repo.Owner, c.Config.Repo.Name, pr.Number)
	if err != nil {
		return "", err
	}
	var names, other []string
	for _, f := range files {
		names = append(names, f.Filename)
		if f.PreviousFilename != "" && f.PreviousFilename != f.Filename {
			names = append(names, f.PreviousFilename)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		if _, ok := want[name]; !ok {
			other = append(other, name)
		}
	}
	if len(other) > 0 {
		return fmt.Sprintf("it changes %s besides the copies of build %s", strings.Join(other, ", "), c.Build), nil
	}
	for _, name := range names {
		b, err := c.Config.File(ctx, pr.HeadSHA, strings.TrimPrefix(name, c.Config.Dir+"/"))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		if !bytes.Equal(b, want[name]) {
			return fmt.Sprintf("%s at its head %s is not the copy of build %s", name, ShortSHA(pr.HeadSHA), c.Build), nil
		}
	}
	return "", nil
}

// title is the title of the schema pull request and the subject of its
// merge.
func (c *SchemaCopier) title() string { return "Schema copies for " + c.Build }

// SchemaCopyCard is the brief of the card of pull request pr, which copies
// the schemas build generates to paths of repo and which factoryd did not
// merge for refusal: its text tells what the card carries and how it ends.
func SchemaCopyCard(pr PullRequestRef, repo, build string, paths []string, refusal string) BriefText {
	b := SourceBriefs[SourceSchemaCopy](BriefFacts{Name: repo})
	b.Body = fmt.Sprintf("The factory opened pull request [#%d](%s) on %s: main's copies of the generated schemas differ from what build %s generates.\n\nFiles: %s\n\nThe factory did not merge it itself: %s.\n\nThe card's work is that very pull request: its gates verify it, an approval merges it as the merge rule of the project says, and the merge closes this card.\n",
		pr.Number, pr.URL, repo, build, strings.Join(paths, ", "), refusal)
	return b
}

// card files the card of the schema pull request ref (roadmap 187), which
// factoryd did not merge for refusal, through FileFactoryPRTask, as the
// roller files its roll task: source schema-copy, small's budget, the open
// pull request linked to the row. An unfinished card of this build covers
// the pull request already: the pass files none twice. A failure costs
// only the log: the pull request stands, and the next pass files the card
// again.
func (c *SchemaCopier) card(ctx context.Context, ref PullRequestRef, paths []string, refusal string) {
	if !c.filing() {
		return
	}
	repo := c.Config.Repo.String()
	if open, err := c.buildCards(ctx, repo); err != nil {
		log.Printf("schema copies: the cards of build %s on %s: %v", c.Build, repo, err)
		return // a store that will not answer must not cost a second card
	} else if len(open) > 0 {
		return
	}
	p, ok, err := c.Store.ProjectByRepository(ctx, repo)
	if err != nil {
		log.Printf("schema copies: the project of %s: %v", repo, err)
		return
	}
	if !ok {
		log.Printf("schema copies: no project has the repository %s: pull request #%d stays uncarded", repo, ref.Number)
		return
	}
	b := SchemaCopyCard(ref, repo, c.Build, paths, refusal)
	if _, _, err := FileFactoryPRTask(ctx, c.Store, c.Forgejo, c.Workflows, c.Timeline, c.budget(), p, b, nil,
		Task{ID: NewTaskID(), RiskClass: "standard", Objective: IssueObjective(b.Title, b.Body), SHA: c.Build, Source: SourceSchemaCopy}, ref.Number, refusal); err != nil {
		log.Printf("schema copies: file the card of pull request #%d: %v", ref.Number, err)
	} else {
		log.Printf("schema copies: filed the card of pull request #%d on %s", ref.Number, repo)
	}
}

// filing is whether the copier can file cards at all: factoryd wires a
// store and workflows, a bare copier only opens pull requests.
func (c *SchemaCopier) filing() bool { return c.Store != nil && c.Workflows != nil }

// budget is the policy's budget block in force; where factoryd wired none,
// the card gets no budget rather than a wrong one.
func (c *SchemaCopier) budget() policy.Budget {
	if c.Budget == nil {
		return policy.Budget{}
	}
	return c.Budget()
}

// openStatuses are the statuses of a card whose work is not over.
var openStatuses = []string{"queued", "running", "in-review", "waiting-human", "waiting-agent", "waiting-poster", "waiting-deps", "waiting-image"}

// buildCards are the unfinished cards of this copier's build on repository
// repo: the pass files one per build at most, however often it runs and
// whichever boot's process runs it.
func (c *SchemaCopier) buildCards(ctx context.Context, repo string) ([]Task, error) {
	var out []Task
	for _, status := range openStatuses {
		tasks, err := c.Store.ListTasksByStatus(ctx, status)
		if err != nil {
			return nil, fmt.Errorf("the %s cards: %w", status, err)
		}
		for _, t := range tasks {
			if t.Source == SourceSchemaCopy && t.Repository == repo && t.SHA == c.Build {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// forgetBuildCard gives up this build's still-open card of the schema pull
// request number factoryd just closed for why: nothing is left for its
// gates to verify, so the chat holds no dead approval card. A human card of
// the pull request, whichever build filed it, ends through Cards: no
// workflow sees the bot's close.
//
// Every step on a card (its alerts, its withdrawal, its transition) runs
// before the pull request closes, and a failure of one is returned with the
// pull request still open, so the next pass runs the whole close again; each
// step is safe to run twice.
func (c *SchemaCopier) forgetBuildCard(ctx context.Context, repo string, number int64, why string) error {
	if c.Store == nil {
		return nil
	}
	for _, status := range openStatuses {
		tasks, err := c.Store.ListTasksByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("the %s cards of %s: %w", status, repo, err)
		}
		for _, t := range tasks {
			if t.Source != SourceSchemaCopy || t.Repository != repo || t.PRNumber != number {
				continue
			}
			if t.Kind == "human" && c.Cards == nil {
				log.Printf("schema copies: no card ender wired: the card %s of pull request #%d stays", t.ID, number)
				continue
			}
			if t.Kind == "human" {
				if err := c.Cards.WithdrawPRCard(ctx, t, number, why); err != nil {
					return fmt.Errorf("end the card %s of pull request #%d: %w", t.ID, number, err)
				}
				continue
			}
			if t.SHA != c.Build {
				continue
			}
			if err := c.Store.JudgeOpenTaskAlerts(ctx, t.ID, VerdictUnjudged, FactoryDecider); err != nil {
				return fmt.Errorf("close the alerts of the card %s: %w", t.ID, err)
			}
			if err := c.Store.EndClosed(ctx, t.ID, []string{status}, fmt.Sprintf("the factory closed pull request #%d: %s", number, why)); err != nil {
				return fmt.Errorf("close the card %s of pull request #%d: %w", t.ID, number, err)
			}
			mirrorTaskState(ctx, c.Forgejo, t, "closed")
			log.Printf("schema copies: closed the card %s: its pull request #%d is closing", t.ID, number)
		}
	}
	return nil
}

// pathsOf are the repository paths the difference writes, sorted, under
// dir of the configuration repository.
func pathsOf(diffs []policy.SchemaDifference, dir string) []string {
	paths := make([]string, 0, len(diffs))
	for _, d := range diffs {
		paths = append(paths, dir+"/"+d.Path)
	}
	slices.Sort(paths)
	return paths
}

// open opens the pull request from branch on fork, which it cuts afresh
// from main with the files of diffs: a branch a closed pull request left
// may hold other content, or none once main took it.
func (c *SchemaCopier) open(ctx context.Context, fork Repo, branch string, diffs []policy.SchemaDifference) (*PullRequestRef, error) {
	repo, title := c.Config.Repo, c.title()
	want, why := map[string][]byte{}, map[string]string{}
	for _, d := range diffs {
		path := c.Config.Dir + "/" + d.Path
		want[path], why[path] = d.Want, "differs"
		if d.Missing {
			why[path] = "missing"
		}
	}
	stamp := c.Config.Dir + "/" + policy.DirSchema + "/" + policy.SchemaCommit
	want[stamp] = []byte(c.Commit + "\n")
	if err := c.Forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, branch); err != nil {
		return nil, err
	}
	if err := c.cut(ctx, fork, branch, title, want); err != nil {
		return nil, err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Main's copies of these schemas differ from what the factory's build %s generates. This pull request writes them whole:\n\n", c.Build)
	for _, path := range slices.Sorted(maps.Keys(why)) {
		fmt.Fprintf(&body, "- `%s`: %s\n", path, why[path])
	}
	fmt.Fprintf(&body, "\n`%s` names the build's commit %s: a later build writes over these copies, an older one leaves them.\n", stamp, c.Commit)
	body.WriteString("\nThe factory merges it itself once Forgejo reports it mergeable, no check of its head fails or runs, and it changes only these files, each to the build's bytes. " +
		"Otherwise a card asks a human to merge it. `factoryd config check` only warns about a stale copy.\n")
	ref, err := c.Forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, "main", title, body.String())
	if err != nil {
		return nil, err
	}
	log.Printf("schema copies: created pull request #%d: %s", ref.Number, strings.Join(slices.Sorted(maps.Keys(want)), ", "))
	return &ref, nil
}

// cut makes the bot's fork unless it stands, brings its main to the
// configuration repository's and cuts branch from it with want.
func (c *SchemaCopier) cut(ctx context.Context, fork Repo, branch, title string, want map[string][]byte) error {
	if err := c.Forgejo.EnsureFork(ctx, c.Config.Repo); err != nil {
		return err
	}
	if err := c.Forgejo.SyncFork(ctx, fork); err != nil {
		return err
	}
	changes, err := fileChanges(ctx, c.Forgejo, fork, "main", want, nil)
	if err != nil {
		return err
	}
	_, err = c.Forgejo.CommitFiles(ctx, fork.Owner, fork.Name, "main", branch, Attribution{Role: "schema-copy"}, title, changes)
	return err
}
