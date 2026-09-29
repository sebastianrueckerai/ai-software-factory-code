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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// RevertWindow is how long after a task finished a revert of its pull
// request or a refiling of its alert still marks it reverted.
const RevertWindow = 14 * 24 * time.Hour

// RevertEvery is the revert pass's period.
const RevertEvery = 24 * time.Hour

// revertDiffMax is the largest pull request diff the pass judges; a
// longer one (Forgejo's reply is cut at 1 MiB) is never a revert.
const revertDiffMax = 1<<20 - 1

// RevertBook is what the revert pass reads and marks.
type RevertBook interface {
	// RevertCandidates are the merged agent tasks finished since since
	// and not marked: done, with a pull request, not a roll.
	RevertCandidates(ctx context.Context, since time.Time) ([]Task, error)
	// MarkReverted sets task id's reverted_at to at unless it has one,
	// and reports whether it set it.
	MarkReverted(ctx context.Context, id string, at time.Time) (bool, error)
	// MarkRefiledFixes marks every done task finished since since whose
	// alert's fingerprint was filed again after the task finished and
	// within within of it, at the first refiling, and returns the tasks it
	// marked.
	MarkRefiledFixes(ctx context.Context, since time.Time, within time.Duration) ([]Task, error)
	projectLookup
}

// RevertPass is the daily check behind tasks.reverted_at and
// factory.task.reverted{project} (ruling 185). A merged agent pull
// request is reverted by a pull request merged into the same base of the
// same repository after its task finished and within RevertWindow of it
// that undoes every file it changed (reverts); a fix is reverted too when
// its alert is filed again after the task finished (the refile path of
// the SigNoz intake), within the same window.
type RevertPass struct {
	Book    RevertBook
	Forgejo *Forgejo
	Metrics *telemetry.Metrics
	Now     func() time.Time
}

// Loop runs the pass now and every RevertEvery until ctx ends.
func (p *RevertPass) Loop(ctx context.Context) {
	for {
		if err := p.Run(ctx); err != nil {
			log.Printf("revert pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(RevertEvery):
		}
	}
}

// Run is one pass. Candidates reach back one period more than the window,
// so a revert merged just inside the window a day before is still found.
func (p *RevertPass) Run(ctx context.Context) error {
	since := p.Now().Add(-RevertWindow - RevertEvery)
	tasks, err := p.Book.RevertCandidates(ctx, since)
	if err != nil {
		return err
	}
	byRepo := map[string][]Task{}
	for _, t := range tasks {
		byRepo[t.Repository] = append(byRepo[t.Repository], t)
	}
	var errs []error
	for _, repository := range slices.Sorted(maps.Keys(byRepo)) {
		if err := p.repository(ctx, repository, byRepo[repository]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repository, err))
		}
	}
	refiled, err := p.Book.MarkRefiledFixes(ctx, since, RevertWindow)
	for _, t := range refiled {
		log.Printf("revert pass: task %s (%s) reverted: its alert was filed again after the fix", t.ID, t.Repository)
		p.count(ctx, t)
	}
	return errors.Join(append(errs, err)...)
}

func (p *RevertPass) repository(ctx context.Context, repository string, tasks []Task) error {
	repo, err := SplitRepo(repository)
	if err != nil {
		return err
	}
	since := *tasks[0].FinishedAt
	for _, t := range tasks {
		if t.FinishedAt.Before(since) {
			since = *t.FinishedAt
		}
	}
	prs, err := p.Forgejo.MergedPullRequestsSince(ctx, repo.Owner, repo.Name, since)
	if err != nil {
		return err
	}
	slices.SortFunc(prs, func(a, b MergedPullRequest) int { return a.MergedAt.Compare(b.MergedAt) })
	diffs := map[int64]string{}
	diff := func(n int64) (string, error) {
		if d, ok := diffs[n]; ok {
			return d, nil
		}
		d, err := p.Forgejo.PullRequestDiff(ctx, repo.Owner, repo.Name, n, revertDiffMax)
		if err != nil {
			return "", err
		}
		if len(d) > revertDiffMax {
			d = ""
		}
		diffs[n] = d
		return d, nil
	}
	var errs []error
	for _, t := range tasks {
		if err := p.task(ctx, t, prs, diff); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (p *RevertPass) task(ctx context.Context, t Task, prs []MergedPullRequest, diff func(int64) (string, error)) error {
	agent, err := diff(t.PRNumber)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if pr.Number == t.PRNumber || pr.Base != prBase || !pr.MergedAt.After(*t.FinishedAt) || pr.MergedAt.After(t.FinishedAt.Add(RevertWindow)) {
			continue
		}
		later, err := diff(pr.Number)
		if err != nil {
			return err
		}
		if !reverts(agent, later) {
			continue
		}
		marked, err := p.Book.MarkReverted(ctx, t.ID, pr.MergedAt)
		if err != nil || !marked {
			return err
		}
		log.Printf("revert pass: task %s (%s #%d) reverted by #%d, merged %s", t.ID, t.Repository, t.PRNumber, pr.Number, pr.MergedAt.Format(time.RFC3339))
		p.count(ctx, t)
		return nil
	}
	return nil
}

func (p *RevertPass) count(ctx context.Context, t Task) {
	project, _ := projectLabels(ctx, p.Book, t.Repository)
	p.Metrics.TaskReverted.Add(ctx, 1, metric.WithAttributes(attribute.String("project", project)))
}

// diffFiles reads a git diff into its files (the "diff --git" header's
// new path, the whole header for a quoted path) and each file's net
// change: per line text, the times it was added less the times it was
// removed, zeros left out. Context, positions and how the diff aligned a
// moved block do not count.
func diffFiles(diff string) map[string]map[string]int {
	files := map[string]map[string]int{}
	var cur map[string]int
	hunk := false
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			hunk = false
			// git quotes a path with spaces or non-ASCII characters; the
			// same path gives the same header in both diffs
			path := line
			if i := strings.LastIndex(line, " b/"); i >= 0 && !strings.HasSuffix(line, `"`) {
				path = line[i+3:]
			}
			if files[path] == nil {
				files[path] = map[string]int{}
			}
			cur = files[path]
		case strings.HasPrefix(line, "@@"):
			hunk = true
		case hunk && cur != nil && strings.HasPrefix(line, "+"):
			cur[line[1:]]++
		case hunk && cur != nil && strings.HasPrefix(line, "-"):
			cur[line[1:]]--
		}
	}
	for _, net := range files {
		maps.DeleteFunc(net, func(_ string, n int) bool { return n == 0 })
	}
	return files
}

// reverts is the revert rule: later reverts agent when agent's net change
// is not empty and, for every file agent changed, later's net change of
// the same file is its exact opposite (it adds back what agent removed and
// removes what agent added, however git aligned either diff). Files later
// changes besides are allowed (one pull request may revert several); a
// file of agent's that later leaves alone or changes otherwise is no
// revert.
func reverts(agent, later string) bool {
	a, l := diffFiles(agent), diffFiles(later)
	changed := false
	for path, an := range a {
		ln, ok := l[path]
		if !ok || len(ln) != len(an) {
			return false
		}
		for text, n := range an {
			if ln[text] != -n {
				return false
			}
		}
		changed = changed || len(an) > 0
	}
	return changed
}

func (s *PGStore) RevertCandidates(ctx context.Context, since time.Time) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE status = 'done' AND pr_number <> 0 AND reverted_at IS NULL AND source <> $2 AND finished_at >= $1
		ORDER BY finished_at, id`, since, SourceRoll)
}

func (s *PGStore) MarkReverted(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET reverted_at = $2 WHERE id = $1 AND reverted_at IS NULL`, id, at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *PGStore) MarkRefiledFixes(ctx context.Context, since time.Time, within time.Duration) ([]Task, error) {
	return s.tasks(ctx, `UPDATE tasks SET reverted_at = r.first
		FROM (SELECT a1.task_id, min(a2.raised_at) AS first
		      FROM alerts a1
		      JOIN alerts a2 ON a2.kind = a1.kind AND a2.fingerprint = a1.fingerprint AND a2.id > a1.id
		      JOIN tasks f ON f.id = a1.task_id
		      WHERE a1.kind = $1 AND f.status = 'done' AND f.reverted_at IS NULL AND f.finished_at >= $3
		        AND a2.raised_at > f.finished_at AND a2.raised_at <= f.finished_at + make_interval(secs => $2)
		      GROUP BY a1.task_id) r
		WHERE tasks.id = r.task_id AND tasks.reverted_at IS NULL
		RETURNING `+taskColumns, AlertSigNoz, within.Seconds(), since)
}

var _ RevertBook = (*PGStore)(nil)
