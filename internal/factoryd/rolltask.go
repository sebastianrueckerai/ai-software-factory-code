package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// SourceRoll is the source of a roll task (ruling 178): factoryd writes its
// one commit, the digest bump under k8s/, and no agent session runs.
const SourceRoll = "roll"

// SourceRollRefused is the human card of a commit whose images the roller
// refused unsigned (roadmap 46).
const SourceRollRefused = "roll-refused"

// Roll request states: the task a commit's images opened or were folded
// into, merged or failed with it, skipped when they changed nothing, or
// refused when one carries no signature by the factory's key, its task the
// human card that says so.
const (
	RollOpened  = "opened"
	RollMerged  = "merged"
	RollSkipped = "skipped"
	RollFailed  = "failed"
	RollRefused = "refused"
)

// RollRequest is factoryd's judgement of one main commit whose push run
// went green: Changes maps each roll target whose image changed to the
// digest main CI published for the commit; Committed, that the roll task's
// branch carries them.
type RollRequest struct {
	SHA        string
	Repository string
	RunID      int64
	State      string
	TaskID     string
	Changes    map[string]string
	Summary    string
	Committed  bool
	// Head is the branch commit RollCommit wrote when it committed this.
	Head      string
	CreatedAt time.Time
}

func (s *PGStore) RollRequestSeen(ctx context.Context, sha string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM roll_requests WHERE sha = $1)`, sha).Scan(&found)
	return found, err
}

// RollRuns bounds the push runs the roller judges on repository: first,
// the oldest it judged (0 for none), and carried, the newest a roll task
// carries (opened or merged): an older run going green later would roll
// main back.
func (s *PGStore) RollRuns(ctx context.Context, repository string) (first, carried int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(min(run_id), 0), COALESCE(max(run_id) FILTER (WHERE state IN ('opened', 'merged')), 0)
		FROM roll_requests WHERE repository = $1`, repository).Scan(&first, &carried)
	return first, carried, err
}

// OpenRollTask is repository's roll task still running, if any.
func (s *PGStore) OpenRollTask(ctx context.Context, repository string) (Task, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT r.task_id FROM roll_requests r JOIN tasks t ON t.id = r.task_id
		WHERE r.repository = $1 AND r.state = 'opened' AND t.status <> ALL($2) ORDER BY r.run_id DESC LIMIT 1`,
		repository, FinalStatuses).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return s.Task(ctx, id)
}

func (s *PGStore) InsertRollRequest(ctx context.Context, r RollRequest) error {
	changes, err := json.Marshal(r.Changes)
	if err != nil {
		return err
	}
	var task *string
	if r.TaskID != "" {
		task = &r.TaskID
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO roll_requests (sha, repository, run_id, state, task_id, changes, summary)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, r.SHA, r.Repository, r.RunID, r.State, task, changes, r.Summary)
	return err
}

func (s *PGStore) DeleteRollRequest(ctx context.Context, sha string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM roll_requests WHERE sha = $1`, sha)
	return err
}

// UncommittedRolls are the roll requests of task its branch does not carry
// yet, oldest first.
func (s *PGStore) UncommittedRolls(ctx context.Context, taskID string) ([]RollRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha, repository, run_id, state, changes, summary, created_at FROM roll_requests
		WHERE task_id = $1 AND NOT committed ORDER BY run_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollRequest
	for rows.Next() {
		r := RollRequest{TaskID: taskID}
		var changes []byte
		if err := rows.Scan(&r.SHA, &r.Repository, &r.RunID, &r.State, &changes, &r.Summary, &r.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(changes, &r.Changes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PGStore) MarkRollsCommitted(ctx context.Context, taskID string, shas []string, head string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET committed = true, head = $3 WHERE task_id = $1 AND sha = ANY($2)`, taskID, shas, head)
	return err
}

// RollHead is the branch commit RollCommit last wrote for task, "" before
// its first.
func (s *PGStore) RollHead(ctx context.Context, taskID string) (string, error) {
	var head sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT head FROM roll_requests WHERE task_id = $1 AND committed ORDER BY run_id DESC LIMIT 1`, taskID).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return head.String, err
}

// RebaseRolls puts task's roll markings back where its branch is: two
// RollCommit runs can overlap (an attempt outliving its timeout), and the
// later one then finds the branch at an earlier commit the store itself
// wrote, its requests marked past it. Only a marking at that head is on
// the branch; the ones past it go back to pending, the next write
// carrying them on top. It reports whether any request of the task is
// marked at head at all: false means the branch is at a commit factoryd
// does not know, another writer's move.
func (s *PGStore) RebaseRolls(ctx context.Context, taskID, head string) (bool, error) {
	var recorded bool
	err := s.db.QueryRowContext(ctx, `WITH stale AS (
		UPDATE roll_requests SET committed = false, head = ''
		WHERE task_id = $1 AND committed AND head <> $2 AND EXISTS (
			SELECT 1 FROM roll_requests r WHERE r.task_id = $1 AND r.committed AND r.head = $2)
		RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM roll_requests WHERE task_id = $1 AND committed AND head = $2)`,
		taskID, head).Scan(&recorded)
	return recorded, err
}

// RollTaskPRs are the pull requests of repository's roll tasks.
func (s *PGStore) RollTaskPRs(ctx context.Context, repository string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pr_number FROM tasks WHERE source = $1 AND repository = $2 AND pr_number <> 0 ORDER BY pr_number`,
		SourceRoll, repository)
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
		out = append(out, n)
	}
	return out, rows.Err()
}

// SettleRollRequests follows the roll tasks that ended: a merged task's
// committed requests merged, and the ones folded in after its last commit
// forgotten, so the next poll judges their images again; a task that ended
// otherwise fails its requests; a request whose task or card never got
// filed is forgotten.
func (s *PGStore) SettleRollRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `WITH gone AS (
		DELETE FROM roll_requests r WHERE r.state IN ('opened', 'refused') AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = r.task_id)
			OR r.state = 'opened' AND NOT r.committed AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = r.task_id AND t.status = 'done')
		RETURNING r.sha)
		UPDATE roll_requests r SET state = CASE WHEN t.status = 'done' THEN 'merged' ELSE 'failed' END
		FROM tasks t WHERE r.state = 'opened' AND t.id = r.task_id AND t.status = ANY($1)
		AND r.sha NOT IN (SELECT sha FROM gone)`, FinalStatuses)
	return err
}

// LastMergedRoll is the newest commit whose images a roll task of
// repository merged.
func (s *PGStore) LastMergedRoll(ctx context.Context, repository string) (string, bool, error) {
	var sha string
	err := s.db.QueryRowContext(ctx, `SELECT sha FROM roll_requests WHERE repository = $1 AND state = 'merged'
		ORDER BY run_id DESC LIMIT 1`, repository).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return sha, err == nil, err
}

// bumpDigest points the one pinned reference to image in a manifest at
// digest, and changes nothing else.
func bumpDigest(content []byte, image, digest string) ([]byte, error) {
	ref, old, err := pinnedRef(content, image)
	if err != nil {
		return nil, err
	}
	return bytes.Replace(content, []byte(ref), []byte(strings.TrimSuffix(ref, old)+digest), 1), nil
}

// registryPath is a pinned reference's repository path in its registry:
// git.example/sf/ci-runner@sha256:... is sf/ci-runner.
func registryPath(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	_, path, _ := strings.Cut(name, "/")
	return path
}

func rollTarget(name string) (RollTarget, bool) {
	for _, t := range RollTargets {
		if t.Name == name {
			return t, true
		}
	}
	return RollTarget{}, false
}

// RollerStore is what the roller keeps: the roll requests and the tasks
// it files.
type RollerStore interface {
	CreateTask(ctx context.Context, t Task) error
	RollTaskPRs(ctx context.Context, repository string) ([]int64, error)
	SetAgentSummary(ctx context.Context, id, summary string) error
	RollRequestSeen(ctx context.Context, sha string) (bool, error)
	RollRuns(ctx context.Context, repository string) (first, carried int64, err error)
	OpenRollTask(ctx context.Context, repository string) (Task, bool, error)
	InsertRollRequest(ctx context.Context, r RollRequest) error
	DeleteRollRequest(ctx context.Context, sha string) error
	SettleRollRequests(ctx context.Context) error
	LastMergedRoll(ctx context.Context, repository string) (string, bool, error)
	ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
	// the backstop of the cheap merge turn (mainfix.go)
	Task(ctx context.Context, id string) (Task, bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	SetTaskPR(ctx context.Context, id string, number int64) error
	WaitingQueueHeads(ctx context.Context) ([]string, error)
	PauseQueue(ctx context.Context, p QueuePause) error
	QueuePause(ctx context.Context, project string) (QueuePause, bool, error)
	SetQueuePauseRevert(ctx context.Context, project, taskID string) error
	ClearQueuePause(ctx context.Context, project string) error
	// the weekly upstream card (upstream.go)
	NewestTaskOf(ctx context.Context, repository, source string) (Task, bool, error)
}

// Roller opens the roll of the code repository (ruling 178): for the
// green push runs of main it has not judged, newest first and none older
// than a commit a roll carries, it compares the images the run published
// (the registry tags of the commit's short sha) with the ones k8s/ pins,
// and for a changed image files a task of source roll whose one commit
// bumps the digests; while a roll task is open, a newer set of images is
// folded into it as a second commit. A changed image without a signature
// by SigningKey refuses the commit: a human card says why, and nothing of
// it rolls. It judges each commit once.
type Roller struct {
	Forgejo  *Forgejo
	Registry *Registry
	// SigningKey is the public half of main CI's cosign key (k8s/cosign.pub).
	SigningKey *ecdsa.PublicKey
	Store      RollerStore
	Workflows  Workflows
	// Timeline records the events of the tasks it files.
	Timeline *Timeline
	Repo     Repo
	Token    string // reads the repository's Actions
	Budget   func() policy.Budget
	// LinkHosts are policy.yaml's link_hosts: links in the carried
	// subjects and titles may point there or at the Forgejo host
	LinkHosts func() []string
	Poll      time.Duration
}

// Run polls until ctx ends, and files the weekly upstream card; a failed
// poll logs once until it recovers.
func (r *Roller) Run(ctx context.Context) {
	var last string
	for {
		if err := errors.Join(r.poll(ctx), r.upstream(ctx)); err != nil && err.Error() != last {
			log.Printf("roller: %v", err)
			last = err.Error()
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.Poll):
		}
	}
}

// rollRunsRead is how many of the newest push runs a poll reads for the
// newest green one.
const rollRunsRead = 20

func (r *Roller) poll(ctx context.Context) error {
	if err := r.Store.SettleRollRequests(ctx); err != nil {
		return err
	}
	runs, err := r.Forgejo.ActionRuns(ctx, r.Token, r.Repo.Owner, r.Repo.Name, url.Values{"event": {"push"}, "limit": {strconv.Itoa(rollRunsRead)}})
	if err != nil {
		return err
	}
	if err := r.mainState(ctx, runs); err != nil {
		return err
	}
	var green []ActionRun
	for _, x := range runs {
		if x.PrettyRef == prBase && x.Status == "success" && len(x.CommitSHA) >= 7 {
			green = append(green, x)
		}
	}
	if len(green) == 0 {
		return nil
	}
	// newest first: the newest changed images file (or fold) the roll, and
	// the older runs fall below it
	slices.SortFunc(green, func(a, b ActionRun) int { return cmp.Compare(b.ID, a.ID) })
	first, carried, err := r.Store.RollRuns(ctx, r.Repo.String())
	if err != nil {
		return err
	}
	if first == 0 {
		green = green[:1] // the first judgement: the newest only
	}
	for _, run := range green {
		// a run older than the first judged, or than a commit a roll
		// carries, would roll main back
		if run.ID < first || run.ID <= carried {
			continue
		}
		if seen, err := r.Store.RollRequestSeen(ctx, run.CommitSHA); err != nil {
			return err
		} else if seen {
			continue
		}
		if done, err := r.judge(ctx, run); err != nil || done {
			return err
		}
	}
	return nil
}

// judge decides one green run of main: a skip, a roll task, or a fold
// into the open one. done: it filed or folded, or waits while the open
// roll has no pull request yet; the older runs wait for the next poll.
func (r *Roller) judge(ctx context.Context, run ActionRun) (done bool, err error) {
	open, folding, err := r.Store.OpenRollTask(ctx, r.Repo.String())
	if err != nil {
		return false, err
	}
	if folding && open.PRNumber == 0 {
		return true, nil // folded in once its pull request is open
	}
	req := RollRequest{SHA: run.CommitSHA, Repository: r.Repo.String(), RunID: run.ID, State: RollSkipped}
	if only, err := r.onlyK8s(ctx, run.CommitSHA); err != nil {
		return false, err
	} else if only {
		log.Printf("roller: %s (run %d) changes nothing outside k8s/; nothing to roll", run.CommitSHA, run.ID)
		return false, r.Store.InsertRollRequest(ctx, req)
	}
	at, ref := r.Repo, prBase
	if folding {
		at, ref = r.Forgejo.Fork(r.Repo), "factory/"+open.ID
	}
	changes, refs, unsigned, err := r.changes(ctx, at, ref, run.CommitSHA[:7])
	if err != nil {
		return false, err
	}
	if len(changes) == 0 {
		log.Printf("roller: %s (run %d) changed no image; nothing to roll", run.CommitSHA, run.ID)
		return false, r.Store.InsertRollRequest(ctx, req)
	}
	req.Changes = changes
	if len(unsigned) > 0 {
		return false, r.refuse(ctx, run, req, unsigned)
	}
	req.State = RollOpened
	if req.Summary, err = r.summary(ctx, run, changes, refs); err != nil {
		return false, err
	}
	if folding {
		return true, r.fold(ctx, open, req)
	}
	return true, r.file(ctx, req)
}

// onlyK8s reports whether sha changes nothing outside k8s/ against the
// commit the last merged roll rolled, or, before any roll has merged,
// against sha's first parent: a roll's own merge or a hand roll, whose
// images are rebuilds of what is pinned whatever the build cache made of
// them. Comparing against the last merged roll, not the first parent,
// keeps a code merge whose main CI run a roll merge cancelled from being
// mistaken for k8s-only: its tree already carries the code change, but
// its first parent (the cancelled run's own commit) does too.
func (r *Roller) onlyK8s(ctx context.Context, sha string) (bool, error) {
	base, ok, err := r.Store.LastMergedRoll(ctx, r.Repo.String())
	if err != nil {
		return false, err
	}
	if !ok {
		parents, err := r.Forgejo.CommitParents(ctx, r.Repo.Owner, r.Repo.Name, sha)
		if err != nil || len(parents) == 0 {
			return false, err
		}
		base = parents[0]
	}
	c, err := r.Forgejo.Compare(ctx, r.Repo.Owner, r.Repo.Name, base, sha)
	if err != nil {
		return false, err
	}
	for _, f := range c.Files {
		if !strings.HasPrefix(f, "k8s/") {
			return false, nil
		}
	}
	// an empty commit is compared: it is how a failed roll is followed
	return len(c.Files) > 0, nil
}

// changes compares each target's image published for tag with the one
// pinned at ref of repo, by the linux/amd64 manifest, and returns the
// changed targets' new digests and their new references, and for each new
// digest without a signature by SigningKey, why, by target.
func (r *Roller) changes(ctx context.Context, repo Repo, ref, tag string) (changes, refs, unsigned map[string]string, err error) {
	files := map[string][]byte{}
	resolved := map[string]ImageManifest{}
	token := ""
	resolve := func(path, reference string) (ImageManifest, error) {
		key := path + "@" + reference
		if m, ok := resolved[key]; ok {
			return m, nil
		}
		if token == "" {
			var err error
			if token, err = r.Registry.Token(ctx); err != nil {
				return ImageManifest{}, err
			}
		}
		m, err := r.Registry.Resolve(ctx, token, path, reference)
		if err == nil {
			resolved[key] = m
		}
		return m, err
	}
	verified := map[string]error{}
	changes, refs, unsigned = map[string]string{}, map[string]string{}, map[string]string{}
	for _, t := range RollTargets {
		content, ok := files[t.File]
		if !ok {
			if content, err = r.Forgejo.FileAt(ctx, repo.Owner, repo.Name, ref, t.File); err != nil {
				return nil, nil, nil, err
			}
			files[t.File] = content
		}
		pinned, digest, err := pinnedRef(content, t.Image)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s at %s: %w", t.File, ref, err)
		}
		path := registryPath(pinned)
		now, err := resolve(path, tag)
		if err != nil {
			return nil, nil, nil, err
		}
		was, err := resolve(path, digest)
		if err != nil {
			return nil, nil, nil, err
		}
		if now.Platform == was.Platform {
			continue
		}
		changes[t.Name] = now.Digest
		refs[t.Name] = strings.TrimSuffix(pinned, digest) + now.Digest
		key := path + "@" + now.Digest
		verr, ok := verified[key]
		if !ok {
			verr = r.Registry.Verify(ctx, token, path, now.Digest, r.SigningKey)
			verified[key] = verr
		}
		var refusal ErrUnsigned
		if errors.As(verr, &refusal) {
			unsigned[t.Name] = refusal.Error()
		} else if verr != nil {
			return nil, nil, nil, verr
		}
	}
	return changes, refs, unsigned, nil
}

func rollNames(changes map[string]string) []string {
	var names []string
	for _, t := range RollTargets {
		if _, ok := changes[t.Name]; ok {
			names = append(names, t.Name)
		}
	}
	return names
}

// summary is the roll's text: its title line, the new references, the
// commits since the last roll and the pull requests merged among them.
func (r *Roller) summary(ctx context.Context, run ActionRun, changes, refs map[string]string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "roll %s: %s\n\nMain CI's push run %d published the images of %s. The digests under k8s/ that change:\n",
		run.CommitSHA[:7], strings.Join(rollNames(changes), ", "), run.ID, run.CommitSHA)
	for _, name := range rollNames(changes) {
		fmt.Fprintf(&b, "\n- %s: %s", name, refs[name])
	}
	commits, prs, err := r.carried(ctx, run.CommitSHA)
	if err != nil {
		return "", err
	}
	// subjects and titles can be an agent's or an issue author's: the
	// summary goes into the roll issue, comments and commit messages
	hosts := []string{}
	if r.LinkHosts != nil {
		hosts = append(hosts, r.LinkHosts()...)
	}
	if u, err := url.Parse(r.Forgejo.PublicURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	b.WriteString("\n\nCommits it carries since the last roll:\n")
	if len(commits) == 0 {
		b.WriteString("\n- none found")
	}
	for _, c := range commits {
		fmt.Fprintf(&b, "\n- %s %s", c.SHA[:min(7, len(c.SHA))], sanitize.Outbound(c.Subject, hosts))
	}
	if len(prs) > 0 {
		b.WriteString("\n\nMerged pull requests it carries:\n")
		for _, p := range prs {
			fmt.Fprintf(&b, "\n- #%d %s", p.Number, sanitize.Outbound(p.Title, hosts))
		}
	}
	return b.String(), nil
}

// rollSubject is the first line of a commit RollCommit writes.
var rollSubject = regexp.MustCompile(`^roll [0-9a-f]{7}: `)

// carried lists what head brings since the last roll (the commit the last
// roll task rolled, or before any, the newest commit that touched k8s/,
// a hand roll): its commits oldest first, merges and roll commits left
// out, and the pull requests merged among them, the roll tasks' own left
// out.
func (r *Roller) carried(ctx context.Context, head string) ([]ComparedCommit, []MergedPullRequest, error) {
	base, ok, err := r.Store.LastMergedRoll(ctx, r.Repo.String())
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		if base, _, err = r.Forgejo.LatestCommit(ctx, r.Repo.Owner, r.Repo.Name, prBase, "k8s"); err != nil {
			return nil, nil, err
		}
	}
	c, err := r.Forgejo.Compare(ctx, r.Repo.Owner, r.Repo.Name, base, head)
	if err != nil {
		return nil, nil, err
	}
	var commits []ComparedCommit
	var shas []string
	for _, cm := range slices.Backward(c.Commits) {
		shas = append(shas, cm.SHA)
		if cm.Parents < 2 && !rollSubject.MatchString(cm.Subject) {
			commits = append(commits, cm)
		}
	}
	merged, err := r.Forgejo.RecentMergedPullRequests(ctx, r.Repo.Owner, r.Repo.Name)
	if err != nil {
		return nil, nil, err
	}
	rolls, err := r.Store.RollTaskPRs(ctx, r.Repo.String())
	if err != nil {
		return nil, nil, err
	}
	var prs []MergedPullRequest
	for _, p := range merged {
		if slices.Contains(shas, p.MergeCommit) && !slices.Contains(rolls, p.Number) {
			prs = append(prs, p)
		}
	}
	slices.SortFunc(prs, func(a, b MergedPullRequest) int { return cmp.Compare(a.Number, b.Number) })
	return commits, prs, nil
}

// file opens the roll task: its request first, so a crash before the
// task exists leaves a request the next settle forgets, then the issue,
// the task and its workflow.
func (r *Roller) file(ctx context.Context, req RollRequest) error {
	p, ok, err := r.Store.ProjectByRepository(ctx, r.Repo.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no project has the repository %s", r.Repo)
	}
	req.TaskID = NewTaskID()
	if err := r.Store.InsertRollRequest(ctx, req); err != nil {
		return err
	}
	title, body, _ := strings.Cut(req.Summary, "\n\n")
	t, ref, err := FileAgentTask(ctx, r.Store, r.Forgejo, r.Workflows, r.Timeline, p, title, body, nil, Task{
		ID: req.TaskID, RiskClass: "standard", BudgetEUR: r.Budget().ClampBudget(0),
		Objective: req.Summary, SHA: req.SHA, Source: SourceRoll,
	})
	if err != nil {
		return errors.Join(err, r.Store.DeleteRollRequest(ctx, req.SHA))
	}
	if err := r.Store.SetAgentSummary(ctx, t.ID, textutil.Truncate(req.Summary, maxAgentSummary)); err != nil {
		log.Printf("roller: task %s: summary: %v", t.ID, err)
	}
	log.Printf("roller: %s (run %d): roll task %s, issue #%d: %s", req.SHA, req.RunID, t.ID, ref.Number, title)
	return nil
}

// refuse records the commit refused and files the human card that names
// each unsigned image and why: its request first, linked to the card, so
// a crash before the card exists leaves a request the next settle
// forgets. No roll task is filed and nothing is folded.
func (r *Roller) refuse(ctx context.Context, run ActionRun, req RollRequest, unsigned map[string]string) error {
	var lines []string
	for _, name := range rollNames(unsigned) {
		lines = append(lines, name+": "+unsigned[name])
	}
	title := fmt.Sprintf("roll %s refused: %s not signed with the factory's key", run.CommitSHA[:7], strings.Join(rollNames(unsigned), ", "))
	body := fmt.Sprintf("Main CI's push run %d of %s (%s/%s/actions/runs/%d) is green, and factoryd rolls none of its images: an image it "+
		"rolls must carry a cosign signature by the factory's key (k8s/cosign.pub), and these do not:\n\n- %s\n\n"+
		"Main CI's image jobs sign each image they push. No signature means a job did not sign it or the tag was pushed outside main CI; "+
		"a signature by another key means the code repository's Actions secret COSIGN_PRIVATE_KEY is not the key k8s/cosign.pub holds. "+
		"Find which, fix it, and close this issue. The next green push run of main is judged again; an empty commit on main starts one."+
		"\n\nfiled by factoryd",
		run.Index, run.CommitSHA, r.Forgejo.PublicURL, r.Repo, run.Index, strings.Join(lines, "\n- "))
	req.State, req.TaskID, req.Summary = RollRefused, NewTaskID(), title
	if err := r.Store.InsertRollRequest(ctx, req); err != nil {
		return err
	}
	_, ref, err := FileTask(ctx, r.Store, r.Forgejo, r.Repo, title, body, []string{"human"}, Task{
		ID: req.TaskID, Kind: "human", RiskClass: "standard", Objective: title + "\n\n" + body, SHA: run.CommitSHA,
		Source: SourceRollRefused, Author: r.Forgejo.BotUser,
	})
	if err != nil {
		return errors.Join(err, r.Store.DeleteRollRequest(ctx, req.SHA))
	}
	log.Printf("roller: %s (run %d): refused, human card %s, issue #%d: %s", run.CommitSHA, run.ID, req.TaskID, ref.Number, title)
	return nil
}

// fold adds a newer set of images to the open roll task: the request, a
// comment on its pull request, and the roll_folded signal that has its
// workflow commit them and run the stages again.
func (r *Roller) fold(ctx context.Context, open Task, req RollRequest) error {
	req.TaskID = open.ID
	if err := r.Store.InsertRollRequest(ctx, req); err != nil {
		return err
	}
	title, _, _ := strings.Cut(req.Summary, "\n\n")
	if _, err := r.Forgejo.CreateIssueComment(ctx, r.Repo.Owner, r.Repo.Name, open.PRNumber, "factoryd: folded into this roll:\n\n"+req.Summary); err != nil {
		log.Printf("roller: task %s: comment the fold of %s: %v", open.ID, req.SHA, err)
	}
	if err := r.Workflows.Signal(ctx, open.ID, SignalRollFolded, TaskSignal{Author: r.Forgejo.BotUser, Body: title + " folded in", PR: open.PRNumber}); err != nil {
		log.Printf("roller: task %s: signal the fold of %s: %v", open.ID, req.SHA, err)
	}
	log.Printf("roller: %s (run %d): folded into roll task %s: %s", req.SHA, req.RunID, open.ID, title)
	return nil
}

// RollCommit writes a roll task's requests its branch does not carry yet
// onto the branch on the bot's fork, as one commit: the first from main's
// head (the fork synced first), a fold on top. It returns the branch head,
// the branch as it is when nothing is left to write. It is the roll's
// only work: no session, no sandbox, no model.
func (r *Runner) RollCommit(ctx context.Context, taskID string) (string, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	if t.Source != SourceRoll {
		return "", terminalError("task %s is no roll task (source %q)", t.ID, t.Source)
	}
	pending, err := r.store.UncommittedRolls(ctx, t.ID)
	if err != nil {
		return "", err
	}
	fork, branch := r.forgejo.Fork(repo), taskBranch(t)
	head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, branch)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if exists {
		// every agent task's token writes the fork, this branch included
		written, err := r.store.RollHead(ctx, t.ID)
		if err != nil {
			return "", err
		}
		if head != written {
			// the branch at a head the store itself wrote for this task
			// (an earlier commit of the roll, not the newest) is
			// factoryd's own: the markings past it are not on the branch,
			// and the write below carries them on top of it — a
			// concurrent RollCommit whose write this call read the branch
			// before and the store after
			recorded, err := r.store.RebaseRolls(ctx, t.ID, head)
			if err != nil {
				return "", err
			}
			if !recorded {
				// a run that died between its write and its record finds
				// its own commit: the one it would write, on what it wrote
				// before or on the base it cut the branch from
				own, err := r.ownCommit(ctx, fork, head, cmp.Or(written, t.SHA), pending)
				if err != nil {
					return "", err
				}
				if !own {
					return "", terminalError("the roll's branch %s is at %s, not at %s, the commit factoryd wrote: a roll builds on and adopts only its own commits",
						branch, head, cmp.Or(written, "(none recorded)"))
				}
				if err := r.store.MarkRollsCommitted(ctx, t.ID, rollSHAs(pending), head); err != nil {
					return "", err
				}
				r.record(ctx, t.ID, "roll_commit", map[string]any{"commit": head, "rolls": rollSHAs(pending), "adopted": true})
				return head, nil
			}
			// the rebase freed requests the branch lacks; the write
			// builds on the branch as it is now and marks at the head it
			// writes, never at the one it read first
			if head, err = r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, branch); err != nil {
				return "", err
			}
			if pending, err = r.store.UncommittedRolls(ctx, t.ID); err != nil {
				return "", err
			}
		}
	}
	if len(pending) == 0 {
		if !exists {
			return "", terminalError("roll task %s has no roll request to commit", t.ID)
		}
		return head, nil
	}
	from, newBranch := branch, ""
	if !exists {
		if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
			return "", err
		}
		if err := r.forgejo.SyncFork(ctx, fork); err != nil {
			return "", err
		}
		// the workflow-directory guard compares the branch with its base
		base, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, prBase)
		if err != nil {
			return "", err
		}
		if err := r.store.SetTaskSHA(ctx, t.ID, base); err != nil {
			return "", err
		}
		from, newBranch = prBase, branch
	}
	changes, err := r.rollEdits(ctx, fork, from, pending)
	if err != nil {
		return "", err
	}
	commit := head
	switch {
	case len(changes) > 0:
		var messages []string
		for _, req := range pending {
			messages = append(messages, req.Summary)
		}
		if commit, err = r.forgejo.CommitFiles(ctx, fork.Owner, fork.Name, from, newBranch,
			Attribution{Role: "roll", Task: t.ID, Human: t.Author}, strings.Join(messages, "\n\n"), changes); err != nil {
			return "", err
		}
	case !exists:
		return "", terminalError("main already pins every digest of roll task %s", t.ID)
	}
	if err := r.store.MarkRollsCommitted(ctx, t.ID, rollSHAs(pending), commit); err != nil {
		return "", err
	}
	r.record(ctx, t.ID, "roll_commit", map[string]any{"commit": commit, "rolls": rollSHAs(pending), "files": len(changes)})
	return commit, nil
}

func rollSHAs(reqs []RollRequest) []string {
	var out []string
	for _, req := range reqs {
		out = append(out, req.SHA)
	}
	return out
}

// rollEdits are the file changes that bring ref of fork to the digests of
// reqs, in order: only the files whose content changes.
func (r *Runner) rollEdits(ctx context.Context, fork Repo, ref string, reqs []RollRequest) ([]FileChange, error) {
	type file struct {
		content, was []byte
		blob         string
	}
	files := map[string]*file{}
	var order []string
	for _, req := range reqs {
		for _, name := range rollNames(req.Changes) {
			target, _ := rollTarget(name)
			f, ok := files[target.File]
			if !ok {
				content, blob, err := r.forgejo.FileContent(ctx, fork.Owner, fork.Name, ref, target.File)
				if err != nil {
					return nil, err
				}
				f = &file{content: content, was: content, blob: blob}
				files[target.File] = f
				order = append(order, target.File)
			}
			var err error
			if f.content, err = bumpDigest(f.content, target.Image, req.Changes[name]); err != nil {
				return nil, terminalError("%s: %v", target.File, err)
			}
		}
	}
	var changes []FileChange
	for _, path := range order {
		if f := files[path]; !bytes.Equal(f.content, f.was) {
			changes = append(changes, FileChange{Path: path, Content: f.content, Blob: f.blob})
		}
	}
	return changes, nil
}

// ownCommitDepth caps the ancestry walk: a tip further than this from the
// recorded commit is not factoryd's own.
const ownCommitDepth = 16

// ownCommit reports whether head carries the commit RollCommit wrote for
// pending: the recorded parent reachable from head by following first
// parents within ownCommitDepth. A queue-staged merge moves the tip off
// factoryd's commit, so tip equality is stronger than the hazard this
// guard tests; a tip that does not descend from the recorded commit still
// refuses. On the tip itself, where the walk ends, the roll's files must
// hold exactly what the requests make of them; a merge tip is not compared
// for diff or content beyond that.
func (r *Runner) ownCommit(ctx context.Context, fork Repo, head, parent string, pending []RollRequest) (bool, error) {
	if len(pending) == 0 || parent == "" {
		return false, nil
	}
	cur := head
	for depth := 0; depth <= ownCommitDepth; depth++ {
		parents, err := r.forgejo.CommitParents(ctx, fork.Owner, fork.Name, cur)
		if err != nil {
			return false, err
		}
		if slices.Contains(parents, parent) {
			if cur == head && len(parents) == 1 {
				// the tip is the commit factoryd would have written:
				// the roll's files the only ones it changes, each
				// holding what the requests make of it
				changes, err := r.rollEdits(ctx, fork, parent, pending)
				if err != nil || len(changes) == 0 {
					return false, err
				}
				diff, err := r.forgejo.Compare(ctx, fork.Owner, fork.Name, parent, head)
				if err != nil {
					return false, err
				}
				want := map[string][]byte{}
				for _, c := range changes {
					want[c.Path] = c.Content
				}
				for _, path := range diff.Files {
					if _, ok := want[path]; !ok {
						return false, nil
					}
				}
				for path, content := range want {
					got, _, err := r.forgejo.FileContent(ctx, fork.Owner, fork.Name, head, path)
					if err != nil || !bytes.Equal(got, content) {
						return false, err
					}
				}
				return true, nil
			}
			// a merge tip carrying the recorded commit (the queue's
			// staging merge): ancestry and the roll's pinned digests
			// at the tip are enough to adopt it
			for _, req := range pending {
				for name, digest := range req.Changes {
					target, _ := rollTarget(name)
					content, _, err := r.forgejo.FileContent(ctx, fork.Owner, fork.Name, head, target.File)
					if err != nil {
						return false, err
					}
					ref, _, err := pinnedRef(content, target.Image)
					if err != nil || !strings.HasSuffix(ref, "@"+digest) {
						return false, err
					}
				}
			}
			return true, nil
		}
		if len(parents) == 0 {
			return false, nil
		}
		cur = parents[0]
	}
	return false, nil
}
