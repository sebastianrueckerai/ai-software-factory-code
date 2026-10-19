package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// ChainHead is the newest main run whose pull requests chain_runs records.
func (s *PGStore) ChainHead(ctx context.Context, repository string) (int64, error) {
	var head int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(run_id), 0) FROM chain_runs WHERE repository = $1`, repository).Scan(&head)
	return head, err
}

// RecordChainRun upserts one pull request's row to the run that carries
// it; a newer run wins, a fold's carry moving the change to the fold.
func (s *PGStore) RecordChainRun(ctx context.Context, repository string, pr int64, sha string, run int64, status string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chain_runs (repository, pr_number, sha, run_id, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (repository, pr_number) DO UPDATE SET sha = $3, run_id = $4, status = $5, updated_at = now()
		WHERE chain_runs.run_id < $4`, repository, pr, sha, run, status)
	return err
}

// RecordChainHead upserts the head row (pr 0) to sha's chain verdict —
// the AND of every run on the commit and every required context — at
// run, the newest: an older run never unseats it, and a verdict the row
// already carries leaves it untouched.
func (s *PGStore) RecordChainHead(ctx context.Context, repository, sha string, run int64, status string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chain_runs (repository, pr_number, sha, run_id, status, updated_at)
		VALUES ($1, 0, $2, $3, $4, now())
		ON CONFLICT (repository, pr_number) DO UPDATE SET sha = $2, run_id = $3, status = $4, updated_at = now()
		WHERE chain_runs.run_id <= $3 AND (chain_runs.sha <> $2 OR chain_runs.run_id <> $3 OR chain_runs.status <> $4)`,
		repository, sha, run, status)
	return err
}

// RefreshChainStatus moves every row of sha's run to a status the run
// reached.
func (s *PGStore) RefreshChainStatus(ctx context.Context, repository, sha, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chain_runs SET status = $3, updated_at = now()
		WHERE repository = $1 AND sha = $2 AND status <> $3`, repository, sha, status)
	return err
}

// WorkflowList names repo's workflow files, of .forgejo/workflows and
// the .gitea one Forgejo also honours, at main's head: an empty list is
// a repository whose pushes run nothing.
func (f *Forgejo) WorkflowList(ctx context.Context, repo Repo) ([]string, error) {
	var out []string
	for _, dir := range []string{".forgejo/workflows", ".gitea/workflows"} {
		body, err := f.projectsCall(ctx, http.MethodGet,
			fmt.Sprintf("/api/v1/repos/%s/contents/%s?ref=%s", repo, dir, prBase), nil, http.StatusOK)
		if errors.Is(err, ErrNotFound) {
			continue // a repository without the directory
		}
		if err != nil {
			return nil, err
		}
		var entries []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, fmt.Errorf("forgejo workflows %s: %w", repo, err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name, ".yml") || strings.HasSuffix(e.Name, ".yaml") {
				out = append(out, dir+"/"+e.Name)
			}
		}
	}
	return out, nil
}

// ChainStore is the chain's rows: what the poller reads and writes of
// chain_runs.
type ChainStore interface {
	ChainHead(ctx context.Context, repository string) (int64, error)
	RecordChainRun(ctx context.Context, repository string, pr int64, sha string, run int64, status string) error
	RecordChainHead(ctx context.Context, repository, sha string, run int64, status string) error
	RefreshChainStatus(ctx context.Context, repository, sha, status string) error
}

// ChainProjects is the project list the poller serves.
type ChainProjects interface {
	Projects(ctx context.Context) ([]Project, error)
}

// chainModeNone turns a repository's chain recording off by its file.
const chainModeNone = "none"

// chainComplaint names why a project file's chain block cannot run — a
// mode the poller does not know (a typo), a negative stall. "" when the
// block is usable.
func chainComplaint(c policy.ChainConfig) string {
	if c.Mode != "" && c.Mode != policy.ChainModeRun && c.Mode != chainModeNone {
		return fmt.Sprintf("chain mode %q is neither run nor none", c.Mode)
	}
	if c.Stall < 0 {
		return fmt.Sprintf("chain stall %s is negative", c.Stall)
	}
	return ""
}

// headKey is one repository's head row, followed between polls.
type headKey struct {
	repository, sha string
}

// chainEpisode is what a head row's watch holds between polls: when its
// pending was first seen, and which of its seams already fired.
type chainEpisode struct {
	since    time.Time
	stalled  bool
	repaired bool
}

// ChainPoller records the CI chain of every run-mode repository (roadmap
// 297): the card's deploy fact, read from the chain itself, for any
// project — the roll stays the factory's own repository's engine and
// writes none of it. Each poll reads the repository's newest main push
// runs, maps the pull requests each new run carries (the compare against
// the run before it), moves the rows of runs that concluded to their
// final status, and writes the head row: the newest run's commit,
// carrying the AND of every push run on that commit and every required
// context of the repository's file, so a fast green run never greens a
// red one and a required context the commit does not carry holds the
// head pending. A head pending past the project's stall bound or
// concluded red is work, not a verdict: stalled and repair are its
// seams. A repository without a workflow degrades to none for itself; a
// chain block the poller cannot read leaves its repository unrecorded.
type ChainPoller struct {
	Forgejo  *Forgejo
	Store    ChainStore
	Projects ChainProjects
	// Policy is the version in force: the repository's project file
	// carries its chain block; a repository no file names runs the
	// default.
	Policy func() policy.Version
	// Timeline is the seam of the stalled and repair work (Task 5).
	Timeline *Timeline
	// Now reads the clock the stall watchdog arms itself on.
	Now  func() time.Time
	Poll time.Duration

	watched  map[string]bool          // repository -> whether its pushes run a workflow, read once
	noted    map[string]bool          // the chain-block complaints logged, once each
	failed   map[string]string        // repository -> the failure it last logged
	episodes map[headKey]chainEpisode // the head rows watched between polls
}

// Run polls until ctx ends. A failed poll logs once until it recovers.
func (p *ChainPoller) Run(ctx context.Context) {
	var last string
	for {
		if err := p.poll(ctx); err != nil && err.Error() != last {
			log.Printf("chain poller: %v", err)
			last = err.Error()
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.Poll):
		}
	}
}

func (p *ChainPoller) poll(ctx context.Context) error {
	if p.watched == nil {
		p.watched, p.noted, p.failed, p.episodes = map[string]bool{}, map[string]bool{}, map[string]string{}, map[headKey]chainEpisode{}
	}
	projects, err := p.Projects.Projects(ctx)
	if err != nil {
		return err
	}
	for _, proj := range projects {
		if !proj.Active() {
			continue
		}
		if err := p.project(ctx, proj); err != nil {
			// one repository's failure stops no other's poll, and logs
			// once until it recovers
			if p.failed[proj.Repository] != err.Error() {
				log.Printf("chain poller: %s: %v", proj.Repository, err)
			}
			p.failed[proj.Repository] = err.Error()
			continue
		}
		delete(p.failed, proj.Repository)
	}
	return nil
}

// project serves one repository: its file's chain block read, its
// workflow list watched, its chain recorded.
func (p *ChainPoller) project(ctx context.Context, proj Project) error {
	_, cfg, _ := p.Policy().ProjectOf(proj.Repository)
	if c := chainComplaint(cfg.Chain); c != "" {
		if !p.noted[proj.Repository+" "+c] {
			p.noted[proj.Repository+" "+c] = true
			log.Printf("chain poller: %s: %s: the repository stays unrecorded until its file is fixed", proj.Repository, c)
		}
		return nil
	}
	chain := cfg.Chain.Resolved()
	if chain.Mode != policy.ChainModeRun {
		return nil
	}
	watching, err := p.watching(ctx, proj.Repo())
	if err != nil || !watching {
		return err
	}
	return p.record(ctx, proj, chain)
}

// watching reads whether repository's pushes run a workflow at all,
// once: a repository whose workflow list is empty has no chain to record
// — PR-only CI and mode run cannot produce one — and degrades to none
// for itself, noted once.
func (p *ChainPoller) watching(ctx context.Context, repo Repo) (bool, error) {
	if watching, ok := p.watched[repo.String()]; ok {
		return watching, nil
	}
	files, err := p.Forgejo.WorkflowList(ctx, repo)
	if err != nil {
		return false, err
	}
	watching := len(files) > 0
	p.watched[repo.String()] = watching
	if !watching {
		log.Printf("chain poller: %s has no workflow: its chain degrades to none, nothing recorded", repo)
	}
	return watching, nil
}

// record writes the repository's chain: one row per pull request whose
// change a new run carries, the run's status the row's, and the head row
// of the newest run's commit. A run newer than the recorded head is
// mapped once, its carry read from the compare against the run before
// it; the first recording takes the newest run only, the runs before it
// belong to the era before the chain was recorded. A run already
// recorded only moves to a status it finished at; the head row is
// written last, so the refresh of a concluded sibling cannot leave a
// single run's status standing for the commit's verdict.
func (p *ChainPoller) record(ctx context.Context, proj Project, cfg policy.ChainConfig) error {
	repo := proj.Repo()
	runs, err := p.Forgejo.ActionRuns(ctx, p.Forgejo.Access(proj).Actions, repo.Owner, repo.Name,
		url.Values{"event": {"push"}, "limit": {strconv.Itoa(rollRunsRead)}})
	if err != nil {
		return err
	}
	var seen []ActionRun // main's runs, oldest first: each carry reads the run before it
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].PrettyRef == prBase && len(runs[i].CommitSHA) >= 7 {
			seen = append(seen, runs[i])
		}
	}
	if len(seen) == 0 {
		return nil
	}
	head, err := p.Store.ChainHead(ctx, proj.Repository)
	if err != nil {
		return err
	}
	main := seen
	if head == 0 {
		main = main[len(main)-1:] // the first recording: the newest only
	}
	prev := ""
	for _, run := range main {
		if run.ID <= head {
			if run.Finished() {
				if err := p.Store.RefreshChainStatus(ctx, proj.Repository, run.CommitSHA, run.Status); err != nil {
					return err
				}
			}
			prev = run.CommitSHA
			continue
		}
		prs, err := p.chainCarried(ctx, repo, run, prev)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if err := p.Store.RecordChainRun(ctx, proj.Repository, pr.Number, run.CommitSHA, run.ID, run.Status); err != nil {
				return err
			}
		}
		prev = run.CommitSHA
	}
	newest := seen[len(seen)-1]
	verdict, findings, err := p.headVerdict(ctx, repo, cfg, seen, newest.CommitSHA)
	if err != nil {
		return err
	}
	if err := p.Store.RecordChainHead(ctx, proj.Repository, newest.CommitSHA, newest.ID, verdict); err != nil {
		return err
	}
	p.watch(ctx, proj.Repository, newest.CommitSHA, verdict, cfg.Stall, newest, findings)
	return nil
}

// chainCarried lists the pull requests run's new commits merged. The
// first run recorded has no run before it to compare: it reads its own
// first parent, so its merge's pull requests still map.
func (p *ChainPoller) chainCarried(ctx context.Context, repo Repo, run ActionRun, prev string) ([]MergedPullRequest, error) {
	base := prev
	if base == "" {
		parent, err := p.Forgejo.CommitParent(ctx, repo.Owner, repo.Name, run.CommitSHA)
		if err != nil {
			return nil, err
		}
		base = parent
	}
	merged, err := p.Forgejo.RecentMergedPullRequests(ctx, repo.Owner, repo.Name)
	if err != nil {
		return nil, err
	}
	if base == "" { // a root commit: no pull request rides it
		return nil, nil
	}
	c, err := p.Forgejo.Compare(ctx, repo.Owner, repo.Name, base, run.CommitSHA)
	if err != nil {
		return nil, err
	}
	in := make(map[string]bool, len(c.Commits))
	for _, cm := range c.Commits {
		in[cm.SHA] = true
	}
	var prs []MergedPullRequest
	for _, pr := range merged {
		if in[pr.MergeCommit] {
			prs = append(prs, pr)
		}
	}
	slices.SortFunc(prs, func(a, b MergedPullRequest) int { return cmp.Compare(a.Number, b.Number) })
	return prs, nil
}

// headVerdict is sha's head verdict: the AND of every main run the poll
// sees on the commit and every required context of the repository. Any
// run or context concluded red fails the head; anything not concluded
// green — a run still running, a run the chain skipped (the change never
// ran on the commit), a required context missing from the commit or
// still pending — holds it pending; only all green is success. findings
// name each red run and context.
func (p *ChainPoller) headVerdict(ctx context.Context, repo Repo, cfg policy.ChainConfig, seen []ActionRun, sha string) (verdict string, findings []string, err error) {
	verdict = "success"
	for _, run := range seen {
		if run.CommitSHA != sha {
			continue
		}
		switch {
		case run.Status == "success":
		case run.Finished(): // failure, cancelled
			verdict = "failure"
			findings = append(findings, fmt.Sprintf("run %d %s", run.ID, run.Status))
		case verdict != "failure":
			verdict = "pending"
		}
	}
	if len(cfg.Requires) > 0 {
		combined, err := p.Forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
		if errors.Is(err, ErrNotFound) {
			combined, err = CombinedStatus{}, nil // a commit nothing reported on yet
		}
		if err != nil {
			return "", nil, err
		}
		reported := make(map[string]string, len(combined.Statuses))
		for _, s := range combined.Statuses {
			reported[s.Context] = s.State
		}
		for _, want := range cfg.Requires {
			switch reported[want] {
			case "success":
			case "failure", "error":
				verdict = "failure"
				findings = append(findings, "required context "+want+": "+reported[want])
			default: // missing from the commit, or pending
				if verdict != "failure" {
					verdict = "pending"
				}
			}
		}
	}
	return verdict, findings, nil
}

// watch follows the head row's verdict: pending past the project's stall
// bound calls stalled, concluded failure calls repair (Task 5 fills
// both), and a green head ends the episode. The clock is the first poll
// that saw the row pending, held in memory — chain_runs carries no
// first-recording column, its updated_at moving with every verdict — so
// a factoryd restart re-arms it.
func (p *ChainPoller) watch(ctx context.Context, repository, sha, verdict string, stall time.Duration, run ActionRun, findings []string) {
	key := headKey{repository, sha}
	e := p.episodes[key]
	switch verdict {
	case "pending":
		if e.since.IsZero() {
			e.since = p.now()
		}
		if p.now().Sub(e.since) > stall && !e.stalled {
			e.stalled = true
			p.stalled(ctx, repository, sha)
		}
		p.episodes[key] = e
	case "failure":
		if !e.repaired {
			e.repaired = true
			p.repair(ctx, repository, run, findings)
		}
		p.episodes[key] = e
	default: // success: the head concluded green, its episode ended
		delete(p.episodes, key)
	}
}

func (p *ChainPoller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// stalled is the stall watchdog's seam (Task 5): the head row of
// repository at sha has been pending past the project's stall bound.
func (p *ChainPoller) stalled(ctx context.Context, repository, sha string) {
	log.Printf("chain poller: %s head %s stalled: pending past the stall bound", repository, sha)
}

// repair is the red head's seam (Task 5): the head row of repository
// that run concludes failed; findings name each red run and each red
// required context.
func (p *ChainPoller) repair(ctx context.Context, repository string, run ActionRun, findings []string) {
	log.Printf("chain poller: %s head %s red: %s", repository, run.CommitSHA, strings.Join(findings, "; "))
}
