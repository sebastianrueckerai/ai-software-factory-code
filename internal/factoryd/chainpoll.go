package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceChainRepair is the source of the repair card the chain poller
// files: a red or stalled head of a project's repository is work, not a
// verdict, and the repair runs the escalation ladder as its own agent
// card.
const SourceChainRepair = "chain-repair"

// ChainHead is the newest main run whose pull requests chain_runs records.
func (s *PGStore) ChainHead(ctx context.Context, repository string) (int64, error) {
	var head int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(run_id), 0) FROM chain_runs WHERE repository = $1`, repository).Scan(&head)
	return head, err
}

// ChainCovered lists repository's merged cards a green chain covers, each
// with the newest green run that covers it: a card whose carry row is
// recorded and that the newest green run on main sits at or after — so a
// green run later than the card's own red carry deploys it (the deployed
// digest holds the change), and a card nothing recorded yet does not
// cover. The map is the reconciler's list and the merged inform's
// covered fact.
func (s *PGStore) ChainCovered(ctx context.Context, repository string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, `+chainGreen+` FROM tasks
		WHERE repository = $1 AND status = 'merged' AND pr_number > 0 AND `+chainCovers, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	covered := map[string]int64{}
	for rows.Next() {
		var id string
		var run int64
		if err := rows.Scan(&id, &run); err != nil {
			return nil, err
		}
		covered[id] = run
	}
	return covered, rows.Err()
}

// MergedCardCommits maps those of shas that are the merge commit of a
// card of repository to that card's pull request, whatever the card's
// status: the queue records the commit at the merge, before Finish moves
// the card to merged.
func (s *PGStore) MergedCardCommits(ctx context.Context, repository string, shas []string) (map[string]int64, error) {
	return s.commitCards(ctx, `SELECT merge_commit, pr_number FROM tasks
		WHERE repository = $1 AND pr_number > 0 AND merge_commit = ANY($2)`, repository, shas)
}

// UncarriedMerged lists the merged cards of repository with a pull
// request and no carry row, the merge commit recorded or not: the cards
// the late mapping gives a row, and those a silent chain alerts for.
func (s *PGStore) UncarriedMerged(ctx context.Context, repository string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks t
		WHERE repository = $1 AND status = 'merged' AND pr_number > 0
		AND NOT EXISTS (SELECT 1 FROM chain_runs c WHERE c.repository = t.repository AND c.pr_number = t.pr_number)
		ORDER BY id`, repository)
}

func (s *PGStore) commitCards(ctx context.Context, query string, args ...any) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	commits := map[string]int64{}
	for rows.Next() {
		var sha string
		var pr int64
		if err := rows.Scan(&sha, &pr); err != nil {
			return nil, err
		}
		commits[sha] = pr
	}
	return commits, rows.Err()
}

// OpenRepairCard reports whether repository has a repair card open: one
// card per repository while its head stays red or stalled.
func (s *PGStore) OpenRepairCard(ctx context.Context, repository string) (bool, error) {
	var open bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks
		WHERE repository = $1 AND source = $2 AND status <> ALL($3))`,
		repository, SourceChainRepair, FinalStatuses).Scan(&open)
	return open, err
}

// ChainCarriedIssues are the issue numbers of the cards whose change
// repository's head at sha carried, ascending: what a repair card
// repairs for.
func (s *PGStore) ChainCarriedIssues(ctx context.Context, repository, sha string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.issue_number FROM tasks t
		JOIN chain_runs c ON c.repository = t.repository AND c.pr_number = t.pr_number
		WHERE t.repository = $1 AND c.sha = $2 AND c.pr_number > 0 AND t.issue_number > 0
		ORDER BY t.issue_number`, repository, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1] != n {
			out = append(out, n)
		}
	}
	return out, rows.Err()
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
// already carries leaves it untouched. A pending never unseats a stall
// the row records; a newer run or a real verdict still rewrites.
func (s *PGStore) RecordChainHead(ctx context.Context, repository, sha string, run int64, status string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chain_runs (repository, pr_number, sha, run_id, status, updated_at)
		VALUES ($1, 0, $2, $3, $4, now())
		ON CONFLICT (repository, pr_number) DO UPDATE SET sha = $2, run_id = $3, status = $4, updated_at = now()
		WHERE chain_runs.run_id <= $3 AND (chain_runs.sha <> $2 OR chain_runs.run_id <> $3 OR chain_runs.status <> $4)
		AND NOT (chain_runs.status = 'stalled' AND $4 = 'pending' AND chain_runs.run_id = $3 AND chain_runs.sha = $2)`,
		repository, sha, run, status)
	return err
}

// RecordChainStall marks repository's head row of sha stalled: pending
// past the project's stall bound. It moves no updated_at — the row keeps
// the time its pending was first recorded, the durable stall clock a
// restarted factoryd reads.
func (s *PGStore) RecordChainStall(ctx context.Context, repository, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chain_runs SET status = 'stalled'
		WHERE repository = $1 AND sha = $2 AND pr_number = 0 AND status = 'pending'`, repository, sha)
	return err
}

// ChainHeadSince is the head row of sha's last change: with a pending
// verdict standing still it is the time the pending was first recorded.
func (s *PGStore) ChainHeadSince(ctx context.Context, repository, sha string) (time.Time, bool, error) {
	var since time.Time
	err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM chain_runs
		WHERE repository = $1 AND sha = $2 AND pr_number = 0`, repository, sha).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return since, err == nil, err
}

// RefreshChainStatus moves the rows of sha's pull requests to a status
// the run reached — or to the stall their head records, so a merged
// card the chain holds reads it. The head row is never a run's to move:
// only its own verdict (RecordChainHead) or its stall (RecordChainStall)
// writes it.
func (s *PGStore) RefreshChainStatus(ctx context.Context, repository, sha, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chain_runs SET status = $3, updated_at = now()
		WHERE repository = $1 AND sha = $2 AND pr_number > 0 AND status <> $3`, repository, sha, status)
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

// ChainStore is the chain's rows — what the poller reads and writes of
// chain_runs, the covered merged cards among them — and the repair card
// its red or stalled head files.
type ChainStore interface {
	ChainHead(ctx context.Context, repository string) (int64, error)
	ChainCovered(ctx context.Context, repository string) (map[string]int64, error)
	MergedCardCommits(ctx context.Context, repository string, shas []string) (map[string]int64, error)
	UncarriedMerged(ctx context.Context, repository string) ([]Task, error)
	RecordChainRun(ctx context.Context, repository string, pr int64, sha string, run int64, status string) error
	RecordChainHead(ctx context.Context, repository, sha string, run int64, status string) error
	RefreshChainStatus(ctx context.Context, repository, sha, status string) error
	RecordChainStall(ctx context.Context, repository, sha string) error
	ChainHeadSince(ctx context.Context, repository, sha string) (time.Time, bool, error)
	CreateTask(ctx context.Context, t Task) error
	OpenRepairCard(ctx context.Context, repository string) (bool, error)
	ChainCarriedIssues(ctx context.Context, repository, sha string) ([]int64, error)
	InsertAlert(ctx context.Context, a Alert) (int64, error)
	ResolveChainAlerts(ctx context.Context, repository string) error
	ResolveChainUncarried(ctx context.Context, repository string, standing []string) error
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
// pending was first seen, and whether its stall already fired.
type chainEpisode struct {
	since   time.Time
	stalled bool
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
// head pending; then every merged card a green run covers ends through
// End. A head pending past the project's stall bound or concluded red
// is work, not a verdict: stalled and repair are its
// seams, and each files the one repair card per repository the ladder
// starts with — never for the factory's own, whose red is mainfix's and
// the roll task's. A repository without a workflow degrades to none for
// itself; a chain block the poller cannot read leaves its repository
// unrecorded.
type ChainPoller struct {
	Forgejo  *Forgejo
	Store    ChainStore
	Projects ChainProjects
	// Policy is the version in force: the repository's project file
	// carries its chain block; a repository no file names runs the
	// default.
	Policy func() policy.Version
	// Workflows starts the repair card a red or stalled head files.
	Workflows Workflows
	// End ends a covered merged card: the runner's Finish, the one
	// function that ends a task.
	End func(ctx context.Context, taskID, final, reason string) error
	// Timeline records the events of the repair cards the poller files;
	// nil records none.
	Timeline *Timeline
	// Own is the factory's own repository: a red or stalled head there
	// files no repair card — mainfix owns a red push run, the roll task a
	// red roll.
	Own Repo
	// Now reads the clock the stall watchdog arms itself on.
	Now  func() time.Time
	Poll time.Duration

	ranges   map[[3]string][]string       // repository, base, head -> the commits of that compare
	watched  map[string]bool              // repository -> true once its pushes run a workflow
	noted    map[string]bool              // the chain-block complaints logged, once each
	failed   map[string]string            // repository -> the failure it last logged
	episodes map[headKey]chainEpisode     // the head rows watched between polls
	ends     map[string]map[string]string // repository -> covered card -> the end failure it last logged

	pollMu sync.Mutex
	polled map[string]time.Time // repository -> its last successful tick, read by the poll age gauge
}

// Run polls until ctx ends. A failed poll logs once until it recovers.
func (p *ChainPoller) Run(ctx context.Context) {
	p.once()
	var last string
	for {
		if err := p.Tick(ctx); err != nil && err.Error() != last {
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

// once initialises the maps a pass keeps between polls.
func (p *ChainPoller) once() {
	if p.watched == nil {
		p.watched, p.noted, p.failed, p.episodes = map[string]bool{}, map[string]bool{}, map[string]string{}, map[headKey]chainEpisode{}
		p.ranges = map[[3]string][]string{}
		p.pollMu.Lock()
		p.polled = map[string]time.Time{}
		p.pollMu.Unlock()
		p.ends = map[string]map[string]string{}
	}
}

// Tick polls every active project's chain once; one repository's failure
// logs and stops no other's.
func (p *ChainPoller) Tick(ctx context.Context) error {
	p.once()
	projects, err := p.Projects.Projects(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, proj := range projects {
		if !proj.Active() {
			continue
		}
		seen[proj.Repository] = true
		p.polledSince(proj.Repository)
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
		p.polledNow(proj.Repository)
	}
	p.pollMu.Lock()
	maps.DeleteFunc(p.polled, func(repository string, _ time.Time) bool { return !seen[repository] })
	p.pollMu.Unlock()
	return nil
}

// polledSince starts a repository's poll age at its first sight, so a
// repository whose every tick fails still ages.
func (p *ChainPoller) polledSince(repository string) {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	if _, ok := p.polled[repository]; !ok {
		p.polled[repository] = p.now()
	}
}

func (p *ChainPoller) polledNow(repository string) {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	p.polled[repository] = p.now()
}

// project serves one repository: its file's chain block read, its
// workflow list watched, its chain recorded and the merged cards that
// recording covers ended. The merged cards the chain still does not carry
// are watched whether the repository is served or recording fails.
func (p *ChainPoller) project(ctx context.Context, proj Project) error {
	serving, chain, err := p.serves(ctx, proj)
	var cards uncarriedCards
	if err == nil && serving {
		if err = p.record(ctx, proj, chain, &cards); err == nil {
			p.reconcile(ctx, proj.Repository)
		}
	}
	p.uncarried(ctx, proj.Repository, cmp.Or(chain.Stall, policy.DefaultChainStall), &cards)
	return err
}

// uncarriedCards is a tick's one read of a repository's merged cards
// without a carry row: the late mapping takes from it the cards it gives
// a row, and the alert watches what is left.
type uncarriedCards struct {
	cards  []Task
	loaded bool
}

func (u *uncarriedCards) load(ctx context.Context, store ChainStore, repository string) error {
	if u.loaded {
		return nil
	}
	cards, err := store.UncarriedMerged(ctx, repository)
	if err != nil {
		return err
	}
	u.cards, u.loaded = cards, true
	return nil
}

// uncarried raises chain_uncarried for every merged card of repository
// still without a carry row a stall bound after its merge — no row, no
// run in sight: a repository not served, a token that cannot read it, a
// run that never came — and resolves the alert of a card that gained a
// row or left merged. The alert's own key holds it to one open row per
// card.
func (p *ChainPoller) uncarried(ctx context.Context, repository string, bound time.Duration, read *uncarriedCards) {
	if err := read.load(ctx, p.Store, repository); err != nil {
		log.Printf("chain poller: %s: uncarried cards: %v", repository, err)
		return
	}
	standing := []string{}
	for _, c := range read.cards {
		since := *c.FinishedAt
		if p.now().Sub(since) <= bound {
			continue
		}
		fingerprint := repository + " " + c.ID
		standing = append(standing, fingerprint)
		if _, err := p.Store.InsertAlert(ctx, Alert{Kind: AlertChainUncarried, Source: repository, Fingerprint: fingerprint,
			Detail: map[string]any{"card": c.ID, "since": since}}); err != nil {
			log.Printf("chain poller: %s: raise %s for %s: %v", repository, AlertChainUncarried, c.ID, err)
		}
	}
	if err := p.Store.ResolveChainUncarried(ctx, repository, standing); err != nil {
		log.Printf("chain poller: %s: resolve %s: %v", repository, AlertChainUncarried, err)
	}
}

// reconcile ends every merged card of repository a green run covers, on
// every tick, so a card that reached merged after its green run was
// recorded ends all the same. End on a card already done is a no-op; a
// failed end leaves the card merged for the next tick and logs once
// until the card ends or its End succeeds.
func (p *ChainPoller) reconcile(ctx context.Context, repository string) {
	covered, err := p.Store.ChainCovered(ctx, repository)
	if err != nil {
		log.Printf("chain poller: %s: covered cards: %v", repository, err)
		return
	}
	failed := map[string]string{}
	for id, run := range covered {
		if err := p.End(ctx, id, "done", fmt.Sprintf("live: covered by run %d", run)); err != nil {
			if p.ends[repository][id] != err.Error() {
				log.Printf("chain poller: %s: end %s: %v", repository, id, err)
			}
			failed[id] = err.Error()
		}
	}
	p.ends[repository] = failed
}

// serves reads whether the poller serves proj's repository — its file's
// chain block usable, its mode run, its pushes running a workflow — and
// answers the chain config it serves with.
func (p *ChainPoller) serves(ctx context.Context, proj Project) (bool, policy.ChainConfig, error) {
	_, cfg, _ := p.Policy().ProjectOf(proj.Repository)
	if c := chainComplaint(cfg.Chain); c != "" {
		if !p.noted[proj.Repository+" "+c] {
			p.noted[proj.Repository+" "+c] = true
			log.Printf("chain poller: %s: %s: the repository stays unrecorded until its file is fixed", proj.Repository, c)
		}
		return false, policy.ChainConfig{}, nil
	}
	chain := cfg.Chain.Resolved()
	if chain.Mode != policy.ChainModeRun {
		return false, chain, nil
	}
	watching, err := p.watching(ctx, proj.Repo())
	if err != nil || !watching {
		return false, chain, err
	}
	return true, chain, nil
}

// RepoVisible reports whether the projects token can see repo at all:
// a contents call answers 404 alike for a missing workflows directory
// and a repository the token has no access to, and the two must not
// read alike on the board.
func (f *Forgejo) RepoVisible(ctx context.Context, repo Repo) (bool, error) {
	_, err := f.projectsCall(ctx, http.MethodGet, "/api/v1/repos/"+repo.String(), nil, http.StatusOK)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	}
	return false, err
}

// watching reads whether repository's pushes run a workflow at all: a
// repository whose workflow list is empty has no chain to record —
// PR-only CI and mode run cannot produce one — and degrades to none for
// itself, noted once, its list read again on every tick until it gains
// a workflow. Only a workflow found is cached: a served repository is
// never read again, so a failing read cannot unserve it. A repository
// whose list is empty because the projects token cannot see it at all is
// no degradation: a configuration fault, which logs every poll and
// alerts, and whose verdict is never cached — access restored, the
// chain records.
func (p *ChainPoller) watching(ctx context.Context, repo Repo) (bool, error) {
	if p.watched[repo.String()] {
		return true, nil
	}
	files, err := p.Forgejo.WorkflowList(ctx, repo)
	if err != nil {
		return false, err
	}
	if len(files) > 0 {
		p.watched[repo.String()] = true
		delete(p.noted, repo.String()+" no workflow")
		return true, nil
	}
	// an empty list is ambiguous: a repository without workflows and a
	// repository the projects token cannot see answer alike. Ask the
	// repository itself before degrading — an invisible one is a
	// configuration fault, logged every poll and alerted once, never
	// cached; silence is how it hides.
	visible, err := p.Forgejo.RepoVisible(ctx, repo)
	if err != nil {
		return false, err
	}
	if !visible {
		log.Printf("chain poller: %s is invisible to the projects token: the chain cannot record — restore the token's access (ruling 164)", repo)
		p.raiseChainAlert(ctx, repo.String(), AlertChainStalled, map[string]any{"reason": "the projects token cannot see the repository"})
		return false, nil
	}
	if key := repo.String() + " no workflow"; !p.noted[key] {
		p.noted[key] = true
		log.Printf("chain poller: %s has no workflow: its chain degrades to none, nothing recorded", repo)
	}
	return false, nil
}

// record writes the repository's chain: one row per pull request whose
// change a new run carries, and the head row of the newest run's
// commit. Every row's status is its commit's verdict — the AND of the
// runs and required contexts on that commit, the same value the head
// row gets — so every reader of the rows (the coverage read, the
// inform, the facts) inherits the required contexts. A run newer than
// the recorded head is mapped once, its carry read from the compare
// against the run before it; the first recording takes the newest run
// only, the runs before it belong to the era before the chain was
// recorded. A run already recorded only moves to a status it finished
// at.
func (p *ChainPoller) record(ctx context.Context, proj Project, cfg policy.ChainConfig, uncarried *uncarriedCards) error {
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
	verdicts := map[string]string{}
	// verdictOf is sha's head verdict — the AND the head row writes —
	// computed once per commit the pass writes a row of.
	verdictOf := func(sha string) (string, error) {
		if v, ok := verdicts[sha]; ok {
			return v, nil
		}
		v, _, err := p.headVerdict(ctx, repo, cfg, seen, sha)
		if err != nil {
			return "", err
		}
		verdicts[sha] = v
		return v, nil
	}
	main := seen
	if head == 0 {
		main = main[len(main)-1:] // the first recording: the newest only
	}
	prev := ""
	for _, run := range main {
		if run.ID <= head {
			if run.Finished() {
				v, err := verdictOf(run.CommitSHA)
				if err != nil {
					return err
				}
				if err := p.Store.RefreshChainStatus(ctx, proj.Repository, run.CommitSHA, v); err != nil {
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
		if len(prs) > 0 {
			v, err := verdictOf(run.CommitSHA)
			if err != nil {
				return err
			}
			for _, pr := range prs {
				if err := p.Store.RecordChainRun(ctx, proj.Repository, pr, run.CommitSHA, run.ID, v); err != nil {
					return err
				}
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
	// the late path never gates the head, the watch or the reconciler
	key := proj.Repository + " late"
	if err := p.recordLate(ctx, proj, seen, verdictOf, uncarried); err != nil {
		if p.failed[key] != err.Error() {
			log.Printf("chain poller: %s: late carry rows: %v", proj.Repository, err)
		}
		p.failed[key] = err.Error()
	} else {
		delete(p.failed, key)
	}
	return nil
}

// chainCarried lists the pull requests of the cards whose own merge
// commit lies in run's compare range, ascending.
func (p *ChainPoller) chainCarried(ctx context.Context, repo Repo, run ActionRun, prev string) ([]int64, error) {
	shas, err := p.rangeSHAs(ctx, repo, run, prev)
	if err != nil || len(shas) == 0 {
		return nil, err
	}
	cards, err := p.Store.MergedCardCommits(ctx, repo.String(), shas)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Values(cards)), nil
}

// rangeSHAs are the commits run brings over prev. The first run recorded
// has no run before it to compare: it reads its own first parent, so its
// merge's pull requests still map. A compare of two commits never
// changes, so its answer is kept for the cards a later tick maps late.
func (p *ChainPoller) rangeSHAs(ctx context.Context, repo Repo, run ActionRun, prev string) ([]string, error) {
	key := [3]string{repo.String(), prev, run.CommitSHA}
	if shas, ok := p.ranges[key]; ok {
		return shas, nil
	}
	base := prev
	if base == "" {
		parent, err := p.Forgejo.CommitParent(ctx, repo.Owner, repo.Name, run.CommitSHA)
		if err != nil {
			return nil, err
		}
		base = parent
	}
	var shas []string
	if base != "" { // a root commit: no pull request rides it
		c, err := p.Forgejo.Compare(ctx, repo.Owner, repo.Name, base, run.CommitSHA)
		if err != nil {
			return nil, err
		}
		for _, cm := range c.Commits {
			shas = append(shas, cm.SHA)
		}
	}
	if len(p.ranges) >= chainRangesKept {
		clear(p.ranges)
	}
	p.ranges[key] = shas
	return shas, nil
}

// chainRangesKept bounds the compare answers the poller keeps: a few
// windows of runs over the served repositories.
const chainRangesKept = 512

// recordLate gives the merged cards no carry row yet theirs: the first
// run of the window whose range holds the card's merge commit records
// it, with the commit's verdict. Only a card without a row is written,
// so no row moves, and nothing is read while no such card exists.
func (p *ChainPoller) recordLate(ctx context.Context, proj Project, seen []ActionRun, verdictOf func(string) (string, error), uncarried *uncarriedCards) error {
	if err := uncarried.load(ctx, p.Store, proj.Repository); err != nil {
		return err
	}
	late := map[string]int64{}
	for _, c := range uncarried.cards {
		if c.MergeCommit != "" {
			late[c.MergeCommit] = c.PRNumber
		}
	}
	if len(late) == 0 {
		return nil
	}
	prev := ""
	for _, run := range seen {
		shas, err := p.rangeSHAs(ctx, proj.Repo(), run, prev)
		if err != nil {
			return err
		}
		for _, sha := range shas {
			pr, ok := late[sha]
			if !ok {
				continue
			}
			v, err := verdictOf(run.CommitSHA)
			if err != nil {
				return err
			}
			if err := p.Store.RecordChainRun(ctx, proj.Repository, pr, run.CommitSHA, run.ID, v); err != nil {
				return err
			}
			delete(late, sha)
			uncarried.cards = slices.DeleteFunc(uncarried.cards, func(c Task) bool { return c.PRNumber == pr })
			if len(late) == 0 {
				return nil
			}
		}
		prev = run.CommitSHA
	}
	return nil
}

// headVerdict is sha's head verdict: the AND of every main run the poll
// sees on the commit and every required context of the repository. A run
// concluded failure, or a context concluded failure or error, fails the
// head; anything else not green — a run still running, a run the chain
// skipped (the change never ran on the commit), a run cancelled by the
// next push's superseding run (the common transient: it advances the
// head), a required context missing from the commit or still pending —
// holds it pending; only all green is success. The cancelled and skipped
// cases are the stall watchdog's, not repair's. findings name each red
// run and context.
func (p *ChainPoller) headVerdict(ctx context.Context, repo Repo, cfg policy.ChainConfig, seen []ActionRun, sha string) (verdict string, findings []string, err error) {
	verdict = "success"
	for _, run := range seen {
		if run.CommitSHA != sha {
			continue
		}
		switch {
		case run.Status == "success":
		case run.Status == "failure":
			verdict = "failure"
			findings = append(findings, fmt.Sprintf("run %d %s", run.ID, run.Status))
		case verdict != "failure": // running, skipped, cancelled
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
// bound calls stalled, concluded failure calls repair, and a green head
// ends the episode. A head superseded while pending leaves its episode
// behind, and a red head's episode ends with its repair card filed: the
// open card, not the episode, holds the next red tick back. The clock
// arms from the head row's own updated_at — frozen while its verdict
// stands still — so a factoryd restart reads the time the pending was
// first recorded instead of re-arming the stall.
func (p *ChainPoller) watch(ctx context.Context, repository, sha, verdict string, stall time.Duration, run ActionRun, findings []string) {
	for k := range p.episodes {
		if k.repository == repository && k.sha != sha {
			delete(p.episodes, k)
		}
	}
	key := headKey{repository, sha}
	e := p.episodes[key]
	switch verdict {
	case "pending":
		if e.since.IsZero() {
			e.since = p.now()
			if since, ok, err := p.Store.ChainHeadSince(ctx, repository, sha); err != nil {
				log.Printf("chain poller: %s: read the head row's clock: %v", repository, err)
			} else if ok && !since.IsZero() {
				e.since = since
			}
		}
		if p.now().Sub(e.since) > stall && !e.stalled {
			e.stalled = true
			p.stalled(ctx, repository, sha)
		}
		p.episodes[key] = e
	case "failure":
		delete(p.episodes, key)
		p.repair(ctx, repository, run, findings)
	default: // success: the head concluded green, its episode ended
		delete(p.episodes, key)
		if err := p.Store.ResolveChainAlerts(ctx, repository); err != nil {
			log.Printf("chain poller: %s: resolve the chain alerts: %v", repository, err)
		}
	}
}

func (p *ChainPoller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// stalled is the stall watchdog's seam: the head row of repository at
// sha has been pending past the project's stall bound. The stall is
// recorded on the head row — what a restarted factoryd reads — and on
// the carried rows of the sha, so a merged card the chain holds reads
// it on the board; the repair card is the work it files.
func (p *ChainPoller) stalled(ctx context.Context, repository, sha string) {
	log.Printf("chain poller: %s head %s stalled: pending past the stall bound", repository, sha)
	if err := p.Store.RecordChainStall(ctx, repository, sha); err != nil {
		log.Printf("chain poller: %s: record the stall of %s: %v", repository, sha, err)
	}
	if err := p.Store.RefreshChainStatus(ctx, repository, sha, "stalled"); err != nil {
		log.Printf("chain poller: %s: carry the stall of %s to its cards: %v", repository, sha, err)
	}
	_, cfg, _ := p.Policy().ProjectOf(repository)
	bound := cfg.Chain.Resolved().Stall
	p.raiseChainAlert(ctx, repository, AlertChainStalled, map[string]any{"sha": sha, "bound": bound.String()})
	p.fileRepair(ctx, repository, ActionRun{CommitSHA: sha},
		[]string{fmt.Sprintf("the chain stalled past %s at %s", bound, sha)})
}

// repair is the red head's seam: the head row of repository that run
// concludes failed; findings name each red run and each red required
// context.
func (p *ChainPoller) repair(ctx context.Context, repository string, run ActionRun, findings []string) {
	log.Printf("chain poller: %s head %s red: %s", repository, run.CommitSHA, strings.Join(findings, "; "))
	p.raiseChainAlert(ctx, repository, AlertChainRed, map[string]any{"sha": run.CommitSHA, "run": run.ID, "findings": findings})
	p.fileRepair(ctx, repository, run, findings)
}

// raiseChainAlert files the repository's open alert of kind — one row per
// project while it stands — before the repair card, so a repository whose
// own repair machinery files no card alerts all the same.
func (p *ChainPoller) raiseChainAlert(ctx context.Context, repository, kind string, detail map[string]any) {
	if _, err := p.Store.InsertAlert(ctx, Alert{Kind: kind, Source: repository, Fingerprint: repository, Detail: detail}); err != nil {
		log.Printf("chain poller: %s: raise %s: %v", repository, kind, err)
	}
}

// fileRepair files the repair card of repository's red or stalled head:
// one per repository while one stands (a second red tick joins the open
// card's work), an agent card in the project's lane ready whose
// objective carries the findings, the failing run's link and the issue
// numbers of the cards the head carried. The factory's own repository
// files none: mainfix owns a red push run there and the roll task a red
// roll, each with its own card.
func (p *ChainPoller) fileRepair(ctx context.Context, repository string, run ActionRun, findings []string) {
	if p.Own.Name != "" && repository == p.Own.String() {
		return
	}
	open, err := p.Store.OpenRepairCard(ctx, repository)
	if err != nil {
		log.Printf("chain poller: %s: read the open repair card: %v", repository, err)
		return
	}
	if open {
		return
	}
	var proj Project
	projects, err := p.Projects.Projects(ctx)
	if err != nil {
		log.Printf("chain poller: %s: read the projects: %v", repository, err)
		return
	}
	for _, cand := range projects {
		if cand.Repository == repository && cand.Active() {
			proj = cand
		}
	}
	if proj.Repository == "" {
		return
	}
	issues, err := p.Store.ChainCarriedIssues(ctx, repository, run.CommitSHA)
	if err != nil {
		log.Printf("chain poller: %s: read the carried cards: %v", repository, err)
		return
	}
	short := run.CommitSHA[:min(7, len(run.CommitSHA))]
	title := fmt.Sprintf("chain of %s at %s: %s", repository, short, findings[0])
	var b strings.Builder
	b.WriteString(fmt.Sprintf("The chain of %s is red or stalled at %s:\n", repository, run.CommitSHA))
	for _, f := range findings {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	if run.Index > 0 {
		fmt.Fprintf(&b, "\nThe run: %s/%s/actions/runs/%d.\n", p.Forgejo.PublicURL, repository, run.Index)
	}
	if len(issues) > 0 {
		nums := make([]string, len(issues))
		for i, n := range issues {
			nums[i] = "#" + strconv.FormatInt(n, 10)
		}
		fmt.Fprintf(&b, "\nThe cards whose change this head carried: %s. They stay merged until a green chain covers them; this card is their repair.\n",
			strings.Join(nums, ", "))
	} else {
		b.WriteString("\nNo card's change rides this head; the chain alone is the work.\n")
	}
	b.WriteString("\nRead the run's log, find the cause, fix it on this card's branch from main's head, and push: the fix rides the chain again, and a green chain ends every wait.\n\nfiled by factoryd")
	body := b.String()
	t, ref, err := FileAgentTask(ctx, p.Store, p.Forgejo, p.Workflows, p.Timeline, proj, title, body, nil, Task{
		ID: NewTaskID(), RiskClass: "standard", BudgetEUR: p.Policy().Document.Budget.ClampBudget(0),
		Objective: title + "\n\n" + body, SHA: run.CommitSHA, Source: SourceChainRepair,
	})
	if err != nil {
		log.Printf("chain poller: %s: file the repair card: %v", repository, err)
		return
	}
	log.Printf("chain poller: %s: head %s %s: repair card %s, issue #%d", repository, run.CommitSHA, title, t.ID, ref.Number)
}
