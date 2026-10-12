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
// from its fork, the way a roll does (ruling 303). An approver merges it;
// nothing else does.
type SchemaCopier struct {
	Forgejo *Forgejo       // the bot
	Config  *ForgejoConfig // reads main's tree
	Build   string         // the build id, 12 hex
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
	}
	return nil
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
	_, err = c.Forgejo.CommitFiles(ctx, fork.Owner, fork.Name, "main", branch, title, changes)
	return err
}
