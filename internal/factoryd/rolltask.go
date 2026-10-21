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

	"github.com/jackc/pgx/v5/pgtype"
	"go.temporal.io/sdk/activity"

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

// SourceRollFailed is the human card of the roll ladder's end: rolls
// failed past their bound since the last merged one. factoryd asks, and
// the project agent wakes to attach its analysis.
const SourceRollFailed = "roll-failed"

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
// branch carries them. PRs are the pull requests merged among the commits
// it carries, the link each card reads to track its own deploy (roadmap
// 296).
type RollRequest struct {
	SHA        string
	Repository string
	RunID      int64
	State      string
	TaskID     string
	PRs        []int64
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
	prs := r.PRs
	if prs == nil {
		prs = []int64{}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO roll_requests (sha, repository, run_id, state, task_id, prs, changes, summary)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, r.SHA, r.Repository, r.RunID, r.State, task, prs, changes, r.Summary)
	return err
}

func (s *PGStore) DeleteRollRequest(ctx context.Context, sha string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM roll_requests WHERE sha = $1`, sha)
	return err
}

// TaskRolls are task's roll requests, oldest first: an update writes them
// all again, on main's head (roadmap 279).
func (s *PGStore) TaskRolls(ctx context.Context, taskID string) ([]RollRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha, repository, run_id, state, committed, changes, summary, created_at, prs FROM roll_requests
		WHERE task_id = $1 ORDER BY run_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollRequest
	for rows.Next() {
		r := RollRequest{TaskID: taskID}
		var changes []byte
		// the array arrives as a string through database/sql; pgx's
		// scanner decodes it into the slice (as cloudres does)
		if err := rows.Scan(&r.SHA, &r.Repository, &r.RunID, &r.State, &r.Committed, &changes, &r.Summary, &r.CreatedAt, pgtype.NewMap().SQLScanner(&r.PRs)); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(changes, &r.Changes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UncommittedRolls are the roll requests of task its branch does not carry
// yet, oldest first.
func (s *PGStore) UncommittedRolls(ctx context.Context, taskID string) ([]RollRequest, error) {
	reqs, err := s.TaskRolls(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(reqs, func(r RollRequest) bool { return r.Committed }), nil
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

// SettleRollRequests follows the roll tasks that ended: a done or merged
// task's committed requests merged, and the ones folded in after its
// last commit forgotten, so the next poll judges their images again; a
// task that ended otherwise fails its requests; a request whose task or
// card never got filed is forgotten. It returns the requests it settled,
// whose verdicts the caller reports as commit statuses (roadmap 297).
func (s *PGStore) SettleRollRequests(ctx context.Context) ([]RollRequest, error) {
	rows, err := s.db.QueryContext(ctx, `WITH gone AS (
		DELETE FROM roll_requests r WHERE r.state IN ('opened', 'refused') AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = r.task_id)
			OR r.state = 'opened' AND NOT r.committed AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = r.task_id AND t.status IN ('done', 'merged'))
		RETURNING r.sha)
		UPDATE roll_requests r SET state = CASE WHEN t.status IN ('done', 'merged') THEN 'merged' ELSE 'failed' END
		FROM tasks t WHERE r.state = 'opened' AND t.id = r.task_id AND t.status = ANY($1 || ARRAY['merged'])
		AND r.sha NOT IN (SELECT sha FROM gone)
		RETURNING r.sha, r.state`, FinalStatuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var settled []RollRequest
	for rows.Next() {
		var r RollRequest
		if err := rows.Scan(&r.SHA, &r.State); err != nil {
			return nil, err
		}
		settled = append(settled, r)
	}
	return settled, rows.Err()
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

// FailedRolls are repository's roll requests that failed since the last
// one merged, oldest first: the roll ladder's attempts. None before a
// merged roll counts — the ladder climbs from the last success.
func (s *PGStore) FailedRolls(ctx context.Context, repository string) ([]RollRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha, run_id, task_id, summary FROM roll_requests
		WHERE repository = $1 AND state = 'failed'
		AND run_id > COALESCE((SELECT max(run_id) FROM roll_requests WHERE repository = $1 AND state = 'merged'), 0)
		ORDER BY run_id`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollRequest
	for rows.Next() {
		var r RollRequest
		if err := rows.Scan(&r.SHA, &r.RunID, &r.TaskID, &r.Summary); err != nil {
			return nil, err
		}
		r.Repository = repository
		out = append(out, r)
	}
	return out, rows.Err()
}

// StandingQuestion is repository's human card still waiting whose ask
// carries marker: the question the marker names stands, no ladder
// escalates twice while it does, and its answer, when one comes, is the
// card's to consume.
func (s *PGStore) StandingQuestion(ctx context.Context, repository, marker string) (Task, bool, error) {
	return s.oneTask(ctx, `repository = $1 AND kind = 'human' AND status = 'waiting-human' AND objective LIKE '%' || $2 || '%'`,
		repository, marker)
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
	MergedCardCommits(ctx context.Context, repository string, shas []string) (map[string]int64, error)
	SetAgentSummary(ctx context.Context, id, summary string) error
	RollRequestSeen(ctx context.Context, sha string) (bool, error)
	RollRuns(ctx context.Context, repository string) (first, carried int64, err error)
	OpenRollTask(ctx context.Context, repository string) (Task, bool, error)
	InsertRollRequest(ctx context.Context, r RollRequest) error
	DeleteRollRequest(ctx context.Context, sha string) error
	SettleRollRequests(ctx context.Context) ([]RollRequest, error)
	LastMergedRoll(ctx context.Context, repository string) (string, bool, error)
	FailedRolls(ctx context.Context, repository string) ([]RollRequest, error)
	StandingQuestion(ctx context.Context, repository, marker string) (Task, bool, error)
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
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
}

// Roller opens the roll of the code repository (ruling 178): for the push
// runs of main whose three image jobs are green (the run itself may still
// run) and it has not
// judged, newest first and none older than a commit a roll carries, it
// compares the images the run published
// (the registry tags of the commit's short sha) with the ones k8s/ pins,
// and for a changed image files a task of source roll whose one commit
// bumps the digests; while a roll task is open, a newer set of images is
// folded into it, its branch cut from main's head again. A changed image
// without a signature by SigningKey refuses the commit: a human card says
// why, and nothing of it rolls. It judges each commit once.
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

// Run polls until ctx ends. A failed poll logs once until it recovers.
func (r *Roller) Run(ctx context.Context) {
	var last string
	for {
		if err := r.poll(ctx); err != nil && err.Error() != last {
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
	settled, err := r.Store.SettleRollRequests(ctx)
	if err != nil {
		return err
	}
	stuck := false
	for _, req := range settled {
		if req.State == RollMerged {
			r.rollStatus(ctx, req.SHA, "success", "roll merged: the change runs live")
		} else {
			r.rollStatus(ctx, req.SHA, "failure", "roll task failed: the change does not run live")
			stuck = true
		}
	}
	if stuck {
		r.escalateRolls(ctx)
	}
	if err := r.consumeRetry(ctx); err != nil {
		return err
	}
	runs, err := r.Forgejo.ActionRuns(ctx, r.Token, r.Repo.Owner, r.Repo.Name, url.Values{"event": {"push"}, "limit": {strconv.Itoa(rollRunsRead)}})
	if err != nil {
		return err
	}
	if err := r.mainState(ctx, runs); err != nil {
		return err
	}
	first, carried, err := r.Store.RollRuns(ctx, r.Repo.String())
	if err != nil {
		return err
	}
	var green []ActionRun
	for _, run := range runs {
		if run.PrettyRef != prBase || len(run.CommitSHA) < 7 {
			continue
		}
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
		ok, err := r.imagesGreen(ctx, run.CommitSHA)
		if err != nil {
			return err
		}
		if ok {
			green = append(green, run)
		}
	}
	if len(green) == 0 {
		return nil
	}
	// newest first: the newest changed images file (or fold) the roll, and
	// the older runs fall below it
	slices.SortFunc(green, func(a, b ActionRun) int { return cmp.Compare(b.ID, a.ID) })
	if first == 0 {
		green = green[:1] // the first judgement: the newest only
	}
	for _, run := range green {
		if done, err := r.judge(ctx, run); err != nil || done {
			return err
		}
	}
	return nil
}

// imageContexts are main CI's three image jobs' commit-status contexts:
// the images they publish are what a roll rolls.
var imageContexts = []string{"ci / image (push)", "ci / image-sandbox (push)", "ci / image-runner (push)"}

// imagesGreen reads whether sha's three image jobs report success. The
// roller judges a run on this, not on the run's own state: the run may
// still be running when its images are green.
func (r *Roller) imagesGreen(ctx context.Context, sha string) (bool, error) {
	cs, err := r.Forgejo.CombinedStatus(ctx, r.Repo.Owner, r.Repo.Name, sha)
	if err != nil {
		return false, err
	}
	green := map[string]bool{}
	for _, s := range cs.Statuses {
		if s.State == "success" && slices.Contains(imageContexts, s.Context) {
			green[s.Context] = true
		}
	}
	return len(green) == len(imageContexts), nil
}

// rollStatusContext is the commit-status context the roller reports its
// verdict under, and the factory's project file requires: success means the
// commit's change runs live, failure means it will not, pending means a
// roll task is carrying it.
const rollStatusContext = "factory/roll"

// rollStatus reports the roller's verdict on sha as a commit status. A
// status the API refuses is logged, not fatal: the card and the roll
// request, not the status, are the record.
func (r *Roller) rollStatus(ctx context.Context, sha, state, description string) {
	if err := r.Forgejo.CreateCommitStatus(ctx, r.Repo.Owner, r.Repo.Name, sha,
		CommitStatus{State: state, Context: rollStatusContext, Description: description}); err != nil {
		log.Printf("roller: %s: report %s: %v", sha, state, err)
	}
}

// rollAttemptBound is how many roll requests fail since the last merged
// roll before the ladder asks a human: three attempts, each its own card
// with its own failure, is a stuck pipeline no fourth automatic attempt
// solves. A merged roll resets the ladder to zero.
const rollAttemptBound = 3

// rollAskMarker names the roll ladder's question in the card it files,
// so StandingQuestion recognises it while it waits. rollRetryLabel is
// the retry choice's label: the comment a press posts (the console's
// doHumanAnswer), and what consumeRetry answers.
const (
	rollAskMarker  = "rolls failed since the last one merged"
	rollRetryLabel = "Retry the roll"
)

// escalateRolls is the roll ladder's end. A roll request settled failed
// this poll (each settles once) and the failures since the last merged
// roll at the bound: the five-part question reaches a person — retry the
// roll, park it or close it — once per climb, never twice while the
// question stands. A failure that settles after the card closed climbs
// again.
func (r *Roller) escalateRolls(ctx context.Context) {
	reqs, err := r.Store.FailedRolls(ctx, r.Repo.String())
	if err != nil {
		log.Printf("roller: read the failed rolls: %v", err)
		return
	}
	if len(reqs) < rollAttemptBound {
		return
	}
	if _, standing, err := r.Store.StandingQuestion(ctx, r.Repo.String(), rollAskMarker); err != nil || standing {
		if err != nil {
			log.Printf("roller: read the standing question: %v", err)
		}
		return
	}
	var facts []string
	for _, req := range reqs {
		title, _, _ := strings.Cut(req.Summary, "\n\n")
		fact := fmt.Sprintf("- run %d: %s — roll task failed: the change does not run live", req.RunID, title)
		if t, ok, err := r.Store.Task(ctx, req.TaskID); err == nil && ok && t.IssueNumber > 0 {
			fact += fmt.Sprintf(" (card #%d)", t.IssueNumber)
		}
		facts = append(facts, fact)
	}
	q := HumanQuestion{
		Ask:            fmt.Sprintf("%d %s: main's newest images are not live. Retry the roll, park it or close it?", len(reqs), rollAskMarker),
		Why:            "The roll tasks that carry main's images into k8s/ keep failing, so the repository pins the last images that went live and every card merged since waits on a green chain.",
		Info:           fmt.Sprintf("The failed attempts, oldest first:\n%s\n\nRetry the roll starts one roll task of main's newest green images; park leaves this card waiting; close ends it — the images stay pinned as they are until a new roll merges.", strings.Join(facts, "\n")),
		Recommendation: "Retry the roll, after reading the newest failed card's reason",
		Source:         "the roller's",
		Choices: []QuestionChoice{
			{Value: AnswerRetryRoll, Label: rollRetryLabel},
			{Value: AnswerPark, Label: "Park"},
			{Value: AnswerClose, Label: "Close"},
		},
	}
	if _, err := EscalateToHuman(ctx, r.Store, r.Forgejo, r.Timeline, r.Repo, q, "the roller", Task{Source: SourceRollFailed}); err != nil {
		log.Printf("roller: escalate the failed rolls: %v", err)
	}
}

// consumeRetry acts on the roll ladder's answered question: a member's
// Retry the roll — a press posts the label as their comment — resets the
// ladder. The failed attempts' requests go,
// their commits unjudged again, and the card closes as factoryd's own,
// so the judge of this very poll files one fresh roll of main's newest
// green images; a failure after it climbs from zero. Park and close need
// no consumer: park leaves the card waiting, close ends it.
func (r *Roller) consumeRetry(ctx context.Context) error {
	t, standing, err := r.Store.StandingQuestion(ctx, r.Repo.String(), rollAskMarker)
	if err != nil {
		log.Printf("roller: read the standing question: %v", err)
		return nil
	}
	if !standing {
		return nil
	}
	events, err := r.Store.IntakeEvents(ctx, t.ID)
	if err != nil {
		log.Printf("roller: read the standing question's answers: %v", err)
		return nil
	}
	if botCloseRecorded(events, t) {
		return nil // the answer consumed already; the issue's close is on its way
	}
	if !slices.ContainsFunc(events, func(e IntakeEvent) bool {
		return e.Kind == "issue_comment" && e.Trusted && e.Body == rollRetryLabel
	}) {
		return nil
	}
	reqs, err := r.Store.FailedRolls(ctx, r.Repo.String())
	if err != nil {
		return err
	}
	for _, req := range reqs {
		if err := r.Store.DeleteRollRequest(ctx, req.SHA); err != nil {
			return err
		}
	}
	msg := "factoryd: retry ordered: the failed attempts' bound is reset and one roll task of main's newest green images starts. Closing this issue."
	if _, err := r.Forgejo.CreateIssueComment(ctx, r.Repo.Owner, r.Repo.Name, t.IssueNumber, msg); err != nil {
		return err
	}
	if _, err := recordFactoryClose(ctx, r.Store, t); err != nil {
		return err
	}
	log.Printf("roller: the question's retry: %d failed attempts reset, the roll starts again", len(reqs))
	return r.Forgejo.CloseIssue(ctx, r.Repo.Owner, r.Repo.Name, t.IssueNumber)
}

// judge decides one run of main whose image jobs are green: a skip, a roll
// task, or a fold into the open one. done: it filed or folded, or waits
// while the open roll has no pull request yet; the older runs wait for the
// next poll.
func (r *Roller) judge(ctx context.Context, run ActionRun) (done bool, err error) {
	open, folding, err := r.Store.OpenRollTask(ctx, r.Repo.String())
	if err != nil {
		return false, err
	}
	if folding && open.PRNumber == 0 {
		return true, nil // folded in once its pull request is open
	}
	// the pull requests the run's commits merged since the last roll: every
	// state carries them, so a card tracks its own deploy through a skip or
	// a refusal too (roadmap 296)
	commits, merged, err := r.carried(ctx, run.CommitSHA)
	if err != nil {
		return false, err
	}
	prs := make([]int64, len(merged))
	for i, p := range merged {
		prs[i] = p.Number
	}
	req := RollRequest{SHA: run.CommitSHA, Repository: r.Repo.String(), RunID: run.ID, State: RollSkipped, PRs: prs}
	if only, err := r.onlyK8s(ctx, run.CommitSHA); err != nil {
		return false, err
	} else if only {
		log.Printf("roller: %s (run %d) changes nothing outside k8s/; nothing to roll", run.CommitSHA, run.ID)
		if err := r.Store.InsertRollRequest(ctx, req); err != nil {
			return false, err
		}
		r.rollStatus(ctx, req.SHA, "success", "nothing to roll outside k8s/: already live")
		return false, nil
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
		if err := r.Store.InsertRollRequest(ctx, req); err != nil {
			return false, err
		}
		r.rollStatus(ctx, req.SHA, "success", "no image changed: already live")
		return false, nil
	}
	req.Changes = changes
	if len(unsigned) > 0 {
		return false, r.refuse(ctx, run, req, unsigned)
	}
	req.State = RollOpened
	if req.Summary, err = r.summary(ctx, run, changes, refs, commits, merged); err != nil {
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
func (r *Roller) summary(ctx context.Context, run ActionRun, changes, refs map[string]string, commits []ComparedCommit, merged []MergedPullRequest) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "roll %s: %s\n\nMain CI's push run %d published the images of %s. The digests under k8s/ that change:\n",
		run.CommitSHA[:7], strings.Join(rollNames(changes), ", "), run.ID, run.CommitSHA)
	for _, name := range rollNames(changes) {
		fmt.Fprintf(&b, "\n- %s: %s", name, refs[name])
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
	if len(merged) > 0 {
		b.WriteString("\n\nMerged pull requests it carries:\n")
		for _, p := range merged {
			fmt.Fprintf(&b, "\n- #%d", p.Number)
			pr, err := r.Forgejo.PullRequestState(ctx, r.Repo.Owner, r.Repo.Name, p.Number)
			if err != nil {
				log.Printf("roller: pull request #%d: title: %v", p.Number, err)
				continue
			}
			fmt.Fprintf(&b, " %s", sanitize.Outbound(pr.Title, hosts))
		}
	}
	return b.String(), nil
}

// rollSubject is the first line of a commit RollCommit writes.
var rollSubject = regexp.MustCompile(`^roll [0-9a-f]{7}: `)

// carried lists what head brings since the last roll (the commit the last
// roll task rolled, or before any, the newest commit that touched k8s/,
// a hand roll): its commits oldest first, merges and roll commits left
// out, and the pull requests of the cards whose own merge commit lies
// among them, the roll tasks' own left out.
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
	cards, err := r.Store.MergedCardCommits(ctx, r.Repo.String(), shas)
	if err != nil {
		return nil, nil, err
	}
	rolls, err := r.Store.RollTaskPRs(ctx, r.Repo.String())
	if err != nil {
		return nil, nil, err
	}
	var numbers []int64
	for _, sha := range shas {
		if n, ok := cards[sha]; ok && !slices.Contains(rolls, n) {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	prs := make([]MergedPullRequest, len(numbers))
	for i, n := range numbers {
		prs[i].Number = n
	}
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
	r.rollStatus(ctx, req.SHA, "pending", "roll task "+t.ID+" is taking the change live")
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
	names := rollNames(unsigned)
	q := rollRefusedQuestion(run.CommitSHA[:7], names, lines, fmt.Sprintf("%s/%s/actions/runs/%d", r.Forgejo.PublicURL, r.Repo, run.Index))
	title := askTitle(q)
	req.State, req.TaskID, req.Summary = RollRefused, NewTaskID(), title
	if err := r.Store.InsertRollRequest(ctx, req); err != nil {
		return err
	}
	t, err := EscalateToHuman(ctx, r.Store, r.Forgejo, r.Timeline, r.Repo, q, "factoryd", Task{
		ID: req.TaskID, SHA: run.CommitSHA, Source: SourceRollRefused,
	})
	if err != nil {
		return errors.Join(err, r.Store.DeleteRollRequest(ctx, req.SHA))
	}
	log.Printf("roller: %s (run %d): refused, human card %s, issue #%d: %s", run.CommitSHA, run.ID, req.TaskID, t.IssueNumber, title)
	r.rollStatus(ctx, req.SHA, "failure", "roll refused: "+strings.Join(rollNames(unsigned), ", ")+" not signed")
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
	r.rollStatus(ctx, req.SHA, "pending", "folded into roll task "+open.ID)
	return nil
}

// RollCommit writes a roll task's requests its branch does not carry yet
// onto the branch on the bot's fork, always based on main's head (the
// fork synced first): the first write opens the branch, and every update
// rebases it onto main's head again — the pull request's update, style
// rebase, an endpoint of the factory's Forgejo — and then writes the
// pins as one fresh commit of all the task's requests on it, so the pull
// request's base is always main's head and its diff only the pins
// (roadmap 279). A branch whose rebase conflicts is cut from main's head
// again and its pull request filed fresh (refile). It returns the branch
// head, the branch as it is when nothing is left to write. It is the
// roll's only work: no session, no sandbox, no model.
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
				// before, or — a re-cut (roadmap 279) — the one fresh
				// commit of every pin on the base it cut the branch from
				own, err := r.ownCommit(ctx, fork, head, cmp.Or(written, t.SHA), pending)
				if err != nil {
					return "", err
				}
				if !own && written != "" {
					var all []RollRequest
					if all, err = r.store.TaskRolls(ctx, t.ID); err != nil {
						return "", err
					}
					if own, err = r.ownCommit(ctx, fork, head, t.SHA, all); err != nil {
						return "", err
					}
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
	if !exists {
		if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
			return "", err
		}
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
	// an update carries every request of the task: its branch rebased
	// onto main's head, the pins written on it as one fresh commit, each
	// file at the blob the rebased branch holds
	reqs, ref := pending, prBase
	if exists {
		if reqs, err = r.store.TaskRolls(ctx, t.ID); err != nil {
			return "", err
		}
		if t.PRNumber == 0 {
			return "", terminalError("the roll's branch %s is open with no pull request to rebase onto main's head", branch)
		}
		if err := r.forgejo.RebasePullRequest(ctx, repo.Owner, repo.Name, t.PRNumber); err != nil {
			if errors.Is(err, ErrUpdateConflict) {
				return r.refile(ctx, t, repo, fork, branch, reqs)
			}
			return "", err
		}
		ref = branch
	}
	changes, err := r.rollEdits(ctx, fork, ref, reqs)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", terminalError("main already pins every digest of roll task %s", t.ID)
	}
	var messages []string
	for _, req := range reqs {
		messages = append(messages, req.Summary)
	}
	attribution := Attribution{Role: "roll", Task: t.ID, Human: t.Author}
	newBranch := ""
	if !exists {
		newBranch = branch
	}
	commit, err := r.forgejo.CommitFiles(ctx, fork.Owner, fork.Name, ref, newBranch, attribution,
		strings.Join(messages, "\n\n"), changes)
	if err != nil {
		return "", err
	}
	if err := r.store.MarkRollsCommitted(ctx, t.ID, rollSHAs(reqs), commit); err != nil {
		return "", err
	}
	r.record(ctx, t.ID, "roll_commit", map[string]any{"commit": commit, "rolls": rollSHAs(reqs), "files": len(changes)})
	return commit, nil
}

// refile answers a roll branch that cannot be rebased onto main's head
// (roadmap 279) by cutting it from main's head again: the superseded pull
// request closes with a note, the branch is deleted and written as its
// first commit — one fresh commit of every pin of the task on main's
// head — and a fresh pull request is filed for it, which the roll card
// names. It returns the fresh branch head.
func (r *Runner) refile(ctx context.Context, t Task, repo, fork Repo, branch string, reqs []RollRequest) (string, error) {
	changes, err := r.rollEdits(ctx, fork, prBase, reqs)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", terminalError("main already pins every digest of roll task %s", t.ID)
	}
	if st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber); err != nil {
		return "", err
	} else if st.State == "open" {
		note := "factoryd: closing this roll: its branch cannot be rebased onto main's head, so a fresh one is filed from main's head"
		if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.PRNumber, note); err != nil {
			return "", err
		}
		if err := r.forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, t.PRNumber); err != nil {
			return "", err
		}
	}
	if err := r.forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, branch); err != nil {
		return "", err
	}
	var messages []string
	for _, req := range reqs {
		messages = append(messages, req.Summary)
	}
	attribution := Attribution{Role: "roll", Task: t.ID, Human: t.Author}
	commit, err := r.forgejo.CommitFiles(ctx, fork.Owner, fork.Name, prBase, branch, attribution,
		strings.Join(messages, "\n\n"), changes)
	if err != nil {
		return "", err
	}
	objective := r.untrusted(t, t.Objective)
	body := fmt.Sprintf("%s\n\nCloses #%d\n\nOpened by factoryd for task %s.", objective, t.IssueNumber, t.ID)
	fresh, err := r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, FirstLine(objective), body)
	if err != nil {
		return "", err
	}
	if err := r.store.SetTaskPR(ctx, t.ID, fresh.Number); err != nil {
		return "", err
	}
	card := fmt.Sprintf("factoryd: main moved and this roll's branch could not be rebased onto its head; pull request #%d is superseded by #%d: %s",
		t.PRNumber, fresh.Number, fresh.URL)
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, card); err != nil {
		log.Printf("roller: task %s: name the fresh pull request on the card: %v", t.ID, err)
	}
	if err := r.store.MarkRollsCommitted(ctx, t.ID, rollSHAs(reqs), commit); err != nil {
		return "", err
	}
	r.record(ctx, t.ID, "roll_commit", map[string]any{"commit": commit, "rolls": rollSHAs(reqs), "files": len(changes), "refiled": true, "pr": fresh.Number})
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
// parents within ownCommitDepth — for a re-cut (roadmap 279), the base it
// cut the branch from and every request of the task. A queue-staged merge
// moves the tip off factoryd's commit, so tip equality is stronger than
// the hazard this guard tests; a tip that does not descend from the
// recorded commit still refuses. On the tip itself, where the walk ends,
// the roll's files must hold exactly what the requests make of them; a
// merge tip is not compared for diff or content beyond that.
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

// RolloutResult is what a roll's wait for its rollout answers:
// Confirmed once every image its merge changed runs on its Deployment
// and is available, Live the Deployments that went live.
type RolloutResult struct {
	Confirmed bool
	Live      []string
}

// AwaitRollout is a roll's last wait (ruling 178): after its merge it
// waits until the merge's rollout is confirmed — for every roll target
// whose pinned reference the merge changed, read on the merge commit
// against its first parent as the roll watcher judges it, its
// Deployment carries the reference and is available after Flux applied
// it — or the bound passes, when it answers Confirmed: false. A
// transient error keeps the wait; a factoryd that stops has the next
// one take the wait over from its heartbeat.
func (r *Runner) AwaitRollout(ctx context.Context, taskID string, pr int64, bound time.Duration) (RolloutResult, error) {
	_, repo, err := r.task(ctx, taskID)
	if err != nil {
		return RolloutResult{}, err
	}
	until := r.clock().Add(bound)
	for {
		confirmed, names, err := r.rolloutConfirmed(ctx, repo, pr)
		if err != nil {
			if !r.clock().Before(until) {
				return RolloutResult{}, err
			}
			log.Printf("roll %s: rollout: %v", taskID, err)
		} else if confirmed {
			slices.Sort(names)
			return RolloutResult{Confirmed: true, Live: names}, nil
		}
		now := r.clock()
		if !now.Before(until) {
			return RolloutResult{}, nil
		}
		activity.RecordHeartbeat(ctx)
		if err := r.pause(ctx, min(rollPoll, until.Sub(now))); err != nil {
			return RolloutResult{}, err
		}
	}
}

// rolloutConfirmed reads whether pull request pr's merge is rolled out
// on the factory's own Deployments: confirmed once every roll target
// whose pinned reference the merge changed — its file on the merge
// commit against the merge's first parent — is carried by its
// Deployment and is available after Flux applied it. names are the
// Deployments that went live; a merge Forgejo has not answered yet is
// simply not confirmed yet.
func (r *Runner) rolloutConfirmed(ctx context.Context, repo Repo, pr int64) (confirmed bool, names []string, err error) {
	state, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return false, nil, err
	}
	if !state.Merged || state.MergeCommit == "" {
		return false, nil, nil
	}
	parents, err := r.forgejo.CommitParents(ctx, repo.Owner, repo.Name, state.MergeCommit)
	if err != nil {
		return false, nil, err
	}
	if len(parents) == 0 {
		return false, nil, fmt.Errorf("merge %s of pull request %d has no parent", state.MergeCommit, pr)
	}
	pending := map[string][]rollChange{}
	for _, t := range RollTargets {
		now, err := r.rollRefAt(ctx, repo, state.MergeCommit, t)
		if err != nil {
			return false, nil, err
		}
		before, err := r.rollRefAt(ctx, repo, parents[0], t)
		if err != nil {
			return false, nil, err
		}
		if before != now {
			pending[t.Deployment] = append(pending[t.Deployment], rollChange{target: t, ref: now})
		}
	}
	live := []string{}
	for dep, changes := range pending {
		d, err := r.kube.Deployment(ctx, r.cfg.FactoryNamespace, dep)
		if err != nil {
			return false, nil, err
		}
		ready := true
		for _, c := range changes {
			ready = ready && d.carries(c.ref)
		}
		applied, ok := d.applied()
		if !ok {
			return false, nil, fmt.Errorf("deployment %s has no kustomize-controller entry", dep)
		}
		liveAt, ok := d.live()
		if ready && ok && !liveAt.Before(applied) {
			live = append(live, dep)
		}
	}
	return len(live) == len(pending), live, nil
}

// rollRefAt is a roll target's pinned reference in its file at ref.
func (r *Runner) rollRefAt(ctx context.Context, repo Repo, ref string, t RollTarget) (string, error) {
	raw, err := r.forgejo.FileAt(ctx, repo.Owner, repo.Name, ref, t.File)
	if err != nil {
		return "", err
	}
	pinned, _, err := pinnedRef(raw, t.Image)
	if err != nil {
		return "", fmt.Errorf("%s at %s: %w", t.File, ref, err)
	}
	return pinned, nil
}
