package factoryd

import (
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
// from its fork, the way a roll does (ruling 303). The pull request gets a
// card of its own through FileFactoryPRTask (roadmap 187), so the chat, the
// board and the merge rule cover it; an approver merges through that card.
// Nothing else merges.
type SchemaCopier struct {
	Forgejo *Forgejo       // the bot
	Config  *ForgejoConfig // reads main's tree
	Build   string         // the build id, 12 hex
	// Store holds the tasks the copier files cards in, their projects and
	// the unfinished ones it looks a build's card up in.
	Store interface {
		CreateTask(ctx context.Context, t Task) error
		TransitionTask(ctx context.Context, id string, from []string, to string) error
		ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
		ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
	}
	// Workflows starts the workflow of the card it files.
	Workflows Workflows
	// Timeline records the card's events; nil records none.
	Timeline *Timeline
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
	why := "the copies on main equal the schemas of factoryd " + c.Build
	if len(diffs) > 0 {
		if own == nil {
			if own, err = c.open(ctx, fork, branch, diffs); err != nil {
				return err
			}
		}
		why = fmt.Sprintf("#%d replaces it", own.Number)
	}
	for _, pr := range older {
		if err := c.Forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
			return fmt.Errorf("close #%d: %w", pr.Number, err)
		}
		if _, err := c.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, "Closed by factoryd: "+why+"."); err != nil {
			return fmt.Errorf("comment on #%d: %w", pr.Number, err)
		}
		log.Printf("schema copies: closed pull request #%d: %s", pr.Number, why)
		c.forgetBuildCard(ctx, repo.String(), pr.Number)
	}
	// the pull request becomes a card of its own (roadmap 187), so the
	// chat, the board and the merge rule cover it; the pass files one per
	// build at most, while no other build's card stands open. The link is
	// the pull request's own: a card whose filing died linking gets the
	// number this pass has, so no second card ever stands.
	if len(diffs) > 0 {
		c.card(ctx, *own, pathsOf(diffs, c.Config.Dir))
	}
	return nil
}

// schemaCardBody tells what the card carries and how it ends.
func schemaCardBody(pr PullRequestRef, repo, build string, paths []string) string {
	return fmt.Sprintf("factoryd opened pull request [#%d](%s) on %s: main's copies of the generated schemas differ from what build %s generates.\n\nFiles: %s\n\nThe card's work is that very pull request: its gates verify it, an approval merges it as the merge rule of the project says, and the merge closes this card. factoryd files it and never merges anything itself.\n",
		pr.Number, pr.URL, repo, build, strings.Join(paths, ", "))
}

// card files the card of the schema pull request ref (roadmap 187) through
// FileFactoryPRTask, as the roller files its roll task: source
// schema-copy, small's budget, the open pull request linked to the row. An
// unfinished card of this build covers the pull request already: the pass
// files none twice. A failure costs only the log: the pull request stands,
// and the next pass files the card again.
func (c *SchemaCopier) card(ctx context.Context, ref PullRequestRef, paths []string) {
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
	title := "Schema copies for " + c.Build
	if _, _, err := FileFactoryPRTask(ctx, c.Store, c.Forgejo, c.Workflows, c.Timeline, c.budget(), p, title,
		schemaCardBody(ref, repo, c.Build, paths), nil,
		Task{ID: NewTaskID(), RiskClass: "standard", Objective: title, SHA: c.Build, Source: SourceSchemaCopy}, ref.Number); err != nil {
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
var openStatuses = []string{"queued", "running", "in-review", "waiting-human", "waiting-poster", "waiting-deps"}

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
// request number factoryd just closed: nothing is left for its gates to
// verify, so the chat holds no dead approval card.
func (c *SchemaCopier) forgetBuildCard(ctx context.Context, repo string, number int64) {
	if c.Store == nil {
		return
	}
	for _, status := range openStatuses {
		tasks, err := c.Store.ListTasksByStatus(ctx, status)
		if err != nil {
			log.Printf("schema copies: the %s cards of %s: %v", status, repo, err)
			return
		}
		for _, t := range tasks {
			if t.Source != SourceSchemaCopy || t.Repository != repo || t.SHA != c.Build || t.PRNumber != number {
				continue
			}
			if err := c.Store.TransitionTask(ctx, t.ID, []string{status}, "closed"); err != nil {
				log.Printf("schema copies: close the card %s of pull request #%d: %v", t.ID, number, err)
				continue
			}
			mirrorTaskState(ctx, c.Forgejo, t, "closed")
			log.Printf("schema copies: closed the card %s: its pull request #%d is closed", t.ID, number)
		}
	}
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
	repo, title := c.Config.Repo, "Schema copies for "+c.Build
	want, why := map[string][]byte{}, map[string]string{}
	for _, d := range diffs {
		path := c.Config.Dir + "/" + d.Path
		want[path], why[path] = d.Want, "differs"
		if d.Missing {
			why[path] = "missing"
		}
	}
	if err := c.Forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, branch); err != nil {
		return nil, err
	}
	if err := c.cut(ctx, fork, branch, title, want); err != nil {
		return nil, err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Main's copies of these schemas differ from what factoryd build %s generates; this pull request writes them whole:\n\n", c.Build)
	for _, path := range slices.Sorted(maps.Keys(why)) {
		fmt.Fprintf(&body, "- `%s`: %s\n", path, why[path])
	}
	body.WriteString("\nfactoryd opens it when it starts and never merges it, an approver does. `factoryd config check` only warns about a stale copy.\n")
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
