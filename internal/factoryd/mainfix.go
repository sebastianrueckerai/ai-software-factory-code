package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// The backstop of the cheap merge turn (roadmap 222, item 4): a red push
// run of main pauses the project's merge queue, files the fix-forward
// card, and, when no fix lands within mainRedBound, files the revert of
// the red commit as a factory-PR card. A run whose every failed job lost
// its runner (killed, shut down, or its docker daemon restarted) is no
// commit's fault and reverts nothing: factoryd re-runs it once through
// Forgejo's rerun API and pauses with no card. The human card to re-run
// it comes at once when the re-run fails or Forgejo offers none, and when
// the re-run's jobs lost their runner too; a re-run red in a step files
// the fix-forward card. The next green run of main ends the pause, wakes the
// queue's heads and ends the pause's card, unless the card's own merge
// made main green.
const (
	SourceMainFix = "mainfix"
	SourceRerun   = "rerun"
	SourceRevert  = "revert"
	// SourceGroupRevert is the agent card of a plan's group revert whose
	// plain git revert conflicts (RevertGroup).
	SourceGroupRevert = "group-revert"
	mainRedBound      = 2 * time.Hour
)

// QueuePause is a project's paused merge queue: main is red since the run.
// RerunAt is factoryd's one re-run of the run, zero without one; a pause
// on a re-run has no card until the re-run is red again.
// A pause with a Plan is that plan's group revert instead: Reverts are the
// cards whose merges it undoes, RevertTask its card. Main's own runs leave
// it alone.
type QueuePause struct {
	Project             string
	RunID               int64
	SHA                 string
	Since, RerunAt      time.Time
	FixTask, RevertTask string
	Plan                string
	Reverts             []string
}

// PauseQueue writes the project's pause; a pause that stands, a re-run's,
// takes p's run and card.
func (s *PGStore) PauseQueue(ctx context.Context, p QueuePause) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO queue_pauses (project, run_id, sha, since, fix_task, rerun_at) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project) DO UPDATE SET run_id = EXCLUDED.run_id, sha = EXCLUDED.sha, since = EXCLUDED.since, fix_task = EXCLUDED.fix_task, rerun_at = EXCLUDED.rerun_at`,
		p.Project, p.RunID, p.SHA, p.Since, p.FixTask, nullTime(p.RerunAt))
	return err
}

// PauseQueueForRevert pauses the project's queue for a plan's group revert.
// It answers false when a pause stands, whatever its kind.
func (s *PGStore) PauseQueueForRevert(ctx context.Context, p QueuePause) (bool, error) {
	reverts, err := json.Marshal(append([]string{}, p.Reverts...))
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO queue_pauses (project, run_id, sha, since, revert_task, plan, reverts) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project) DO NOTHING`, p.Project, p.RunID, p.SHA, p.Since, p.RevertTask, p.Plan, reverts)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

const queuePauseColumns = `project, run_id, sha, since, rerun_at, fix_task, revert_task, plan, reverts`

type rowScanner interface{ Scan(dest ...any) error }

func scanQueuePause(row rowScanner) (QueuePause, error) {
	var p QueuePause
	var rerun sql.NullTime
	var reverts []byte
	if err := row.Scan(&p.Project, &p.RunID, &p.SHA, &p.Since, &rerun, &p.FixTask, &p.RevertTask, &p.Plan, &reverts); err != nil {
		return QueuePause{}, err
	}
	p.RerunAt = rerun.Time
	if err := json.Unmarshal(reverts, &p.Reverts); err != nil {
		return QueuePause{}, err
	}
	if len(p.Reverts) == 0 {
		p.Reverts = nil
	}
	return p, nil
}

func (s *PGStore) QueuePause(ctx context.Context, project string) (QueuePause, bool, error) {
	p, err := scanQueuePause(s.db.QueryRowContext(ctx, `SELECT `+queuePauseColumns+` FROM queue_pauses WHERE project = $1`, project))
	if errors.Is(err, sql.ErrNoRows) {
		return QueuePause{}, false, nil
	}
	return p, err == nil, err
}

// PlanQueuePauses are the pauses that name a plan, in project order.
func (s *PGStore) PlanQueuePauses(ctx context.Context) ([]QueuePause, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+queuePauseColumns+` FROM queue_pauses WHERE plan <> '' ORDER BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuePause
	for rows.Next() {
		p, err := scanQueuePause(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PGStore) SetQueuePauseRevert(ctx context.Context, project, taskID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE queue_pauses SET revert_task = $2 WHERE project = $1`, project, taskID)
	return err
}

func (s *PGStore) ClearQueuePause(ctx context.Context, project string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM queue_pauses WHERE project = $1`, project)
	return err
}

// mainState reads main's newest push run: a red one pauses the queue and
// files its card, a green one ends a pause, and a pause past the bound
// with no revert yet files it. A red one on a pause with no card, a
// re-run's, is judged again: the re-run's own red files its card, a newer
// run's red is judged as a first one.
// While the newest runs or waits (a red one's re-run too), or ended
// neither green nor red, nothing changes: an older run says nothing of
// main's head. runs are the poll's, newest first by id.
func (r *Roller) mainState(ctx context.Context, runs []ActionRun) error {
	newest := newestMain(runs)
	if newest == nil || (newest.Status != "success" && newest.Status != "failure") {
		return nil
	}
	project := r.Repo.String()
	pause, paused, err := r.Store.QueuePause(ctx, project)
	if err != nil {
		return err
	}
	if pause.Plan != "" {
		return nil
	}
	switch {
	case newest.Status == "success" && paused:
		if err := r.Store.ClearQueuePause(ctx, project); err != nil {
			return err
		}
		log.Printf("roller: main is green again at %s (run %d): the merge queue of %s goes on", newest.CommitSHA, newest.ID, project)
		signalQueueHeads(ctx, r.Store, r.Workflows)
		if err := r.endPauseCard(ctx, pause, *newest); err != nil {
			log.Printf("roller: end the pause's card %s: %v", pause.FixTask, err)
		}
		return nil
	case newest.Status == "failure" && (!paused || pause.FixTask == ""):
		var rerunAt time.Time
		if pause.RunID == newest.ID {
			rerunAt = pause.RerunAt
		}
		return r.pause(ctx, *newest, rerunAt)
	case paused && pause.RevertTask == "" && time.Since(pause.Since) >= mainRedBound:
		return r.revert(ctx, pause)
	}
	return nil
}

// newestMain is main's newest push run among runs, nil without one.
func newestMain(runs []ActionRun) *ActionRun {
	var newest *ActionRun
	for i := range runs {
		if x := &runs[i]; x.PrettyRef == prBase && (newest == nil || x.ID > newest.ID) {
			newest = x
		}
	}
	return newest
}

// pause pauses the queue on the red run and files the fix-forward card.
// When every failed job lost its runner (lostRunners), it re-runs the run
// and pauses with no card; once rerunAt holds that one re-run, the human
// card to re-run the jobs follows, as it does when the re-run fails. The
// card or the re-run first, so a crash between the two leaves no pause
// without it.
func (r *Roller) pause(ctx context.Context, run ActionRun, rerunAt time.Time) error {
	p, ok, err := r.Store.ProjectByRepository(ctx, r.Repo.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no project has the repository %s", r.Repo)
	}
	failed, lost := "main CI", []string(nil)
	if cs, err := r.Forgejo.CombinedStatus(ctx, r.Repo.Owner, r.Repo.Name, run.CommitSHA); err == nil {
		var red []CommitStatus
		var names []string
		for _, s := range cs.Statuses {
			if s.State == "failure" || s.State == "error" {
				red = append(red, s)
				names = append(names, s.Context)
			}
		}
		if len(names) > 0 {
			failed = strings.Join(names, ", ")
		}
		lost = r.lostRunners(ctx, run, red)
	}
	short := run.CommitSHA[:min(7, len(run.CommitSHA))]
	var t Task
	card := "fix-forward card"
	if lost != nil {
		why := "factoryd re-ran it once, and the re-run's jobs lost their runner too: the runner or its node is at fault, not the code. " +
			"Check the forgejo-runner pod in namespace forgejo (its restarts, its dind container, its node), then re-run the run from its page."
		if rerunAt.IsZero() {
			err := r.Forgejo.RerunActionRun(ctx, r.Forgejo.Access(p).Actions, r.Repo.Owner, r.Repo.Name, run.ID)
			if err == nil {
				if err := r.Store.PauseQueue(ctx, QueuePause{Project: p.Repository, RunID: run.ID, SHA: run.CommitSHA, Since: time.Now(), RerunAt: time.Now()}); err != nil {
					return err
				}
				log.Printf("roller: main is red at %s (run %d), every failed job lost its runner: the merge queue of %s pauses while factoryd re-runs the run", run.CommitSHA, run.ID, p.Repository)
				return nil
			}
			log.Printf("roller: re-run main's run %d: %v: the re-run card at once", run.ID, err)
			why = fmt.Sprintf("factoryd re-runs such a run once itself, but its re-run failed (%v).", err)
			if errors.Is(err, ErrNoRerun) {
				why = "factoryd re-runs such a run once itself, but Forgejo offered no re-run: it has no rerun API (that came with Forgejo 17), or the project's Actions token lacks the right."
			}
		}
		card = "re-run card"
		t, err = EscalateToHuman(ctx, r.Store, r.Forgejo, r.Timeline, p.Repo(), rerunQuestion(run.Index, run.CommitSHA, failed, lost, why), "factoryd", Task{
			SHA: run.CommitSHA, Source: SourceRerun,
		})
	} else {
		title := fmt.Sprintf("main is red at %s: %s", short, failed)
		body := fmt.Sprintf("Main CI's push run %d of %s failed: %s. The run: %s/%s/actions/runs/%d.\n\n"+
			"The project's merge queue is paused until a push run of main is green. Read the run's log, find the cause in main at %s, "+
			"fix it on this card's branch from main's head, and push: this card merges ahead of the queue, and main's next green run "+
			"ends the pause. A fix that does not land within %s has factoryd file the revert of %s.",
			run.Index, run.CommitSHA, failed, r.Forgejo.PublicURL, r.Repo, run.Index, short, mainRedBound, short)
		t, _, err = FileAgentTask(ctx, r.Store, r.Forgejo, r.Workflows, r.Timeline, p, title, body, nil, Task{
			ID: NewTaskID(), RiskClass: "standard", BudgetEUR: r.Budget().ClampBudget(0), Objective: title + "\n\n" + body, SHA: run.CommitSHA, Source: SourceMainFix,
		})
	}
	if err != nil {
		return err
	}
	if err := r.Store.PauseQueue(ctx, QueuePause{Project: p.Repository, RunID: run.ID, SHA: run.CommitSHA, Since: time.Now(), RerunAt: rerunAt, FixTask: t.ID}); err != nil {
		return err
	}
	log.Printf("roller: main is red at %s (run %d): the merge queue of %s pauses; %s %s, issue #%d", run.CommitSHA, run.ID, p.Repository, card, t.ID, t.IssueNumber)
	return nil
}

// endPauseCard ends the pause's card once run, main's green push run,
// ended the pause: a comment naming the run, then the issue's close as
// factoryd's own (issueClosed). The re-run card's human finishes, and a
// fix-forward card still open stops its session: the red it was filed for
// is gone. A fix card that merged made main green itself and is left as
// it is, as is one that ended or is ending; a re-run's pause has none.
func (r *Roller) endPauseCard(ctx context.Context, pause QueuePause, run ActionRun) error {
	t, ok, err := r.Store.Task(ctx, pause.FixTask)
	if err != nil || !ok {
		return err
	}
	green := fmt.Sprintf("factoryd: main's push run %d at %s is green (%s/%s/actions/runs/%d)", run.Index, run.CommitSHA, r.Forgejo.PublicURL, r.Repo, run.Index)
	switch {
	case t.Source == SourceRerun && t.Status == "waiting-human":
		return closeIssueAsFactory(ctx, r.Store, r.Forgejo, t, green+": the merge queue's pause has ended. Closing this issue.")
	case t.Source == SourceMainFix && (t.Status == "failed" || !Final(t.Status) && t.Status != "merged" && t.Status != "closing"):
		return closeIssueAsFactory(ctx, r.Store, r.Forgejo, t, green+": the red this card was filed for is gone. Closing this issue.")
	}
	return nil
}

// lostRunners answers, for each of run's failed statuses, the job's link
// and the record that says it lost its runner (runnerLost), when every one
// is a job of run that did; nil when any failed a step or is no job of
// run: the fix-forward card stays the default. A log factoryd cannot read
// counts as lost: a runner lost before the job's first line leaves none,
// and with no line run, no code failed.
func (r *Roller) lostRunners(ctx context.Context, run ActionRun, failed []CommitStatus) []string {
	jobs := fmt.Sprintf("/%s/actions/runs/%d/jobs/", r.Repo, run.Index)
	var out []string
	for _, s := range failed {
		if !strings.HasPrefix(s.TargetURL, jobs) {
			return nil
		}
		job := s.Context + ", " + r.Forgejo.PublicURL + s.TargetURL
		body, err := r.Forgejo.JobLog(ctx, s.TargetURL)
		if err != nil {
			log.Printf("roller: job log %s: %v", s.TargetURL, err)
			out = append(out, fmt.Sprintf("%s: factoryd cannot read its log (%v); a runner lost before the job's first line leaves none.", job, err))
			continue
		}
		line, silent, ok := runnerLost(body, s.Created)
		switch {
		case !ok:
			return nil
		case line == "":
			out = append(out, job+": its log is empty, its runner lost before the job's first line.")
		case silent > 0:
			out = append(out, fmt.Sprintf("%s: Forgejo failed it at %s, %s after its log's last line, its runner silent since:\n\n%s",
				job, s.Created.UTC().Format(time.RFC3339), silent.Round(time.Second), fenced(line)))
		default:
			out = append(out, job+":\n\n"+fenced(line))
		}
	}
	return out
}

// runnerLogPrefix opens a line forgejo-runner writes itself, not a step's
// output (act/runner/logger.go, runners 12 and 13): among them the error
// each failed step ended on.
const runnerLogPrefix = "\u2699\ufe0f [runner]:"

// dockerLost are the words of the docker client and daemon (moby 28:
// client/errors.go, client/request.go, daemon/errors.go) when the
// runner's daemon is down, or back without the job's container or exec:
// its dind restarted under the job. A step that removed its own container
// through the same daemon reads alike; a re-run tells, at a click's cost.
var dockerLost = []string{
	"Cannot connect to the Docker daemon",
	"error during connect",
	"is not running",
	"No such container",
	"No such exec instance",
}

// runnerSilence tells a job its runner ended from one Forgejo ended for
// it. A runner reports a job's end within seconds of the job's last line
// (49 failed jobs of 2026-10-13 to 15: at most 1.5 s). A runner killed
// outright reports nothing, and its log stops mid-step; Forgejo's
// stop_zombie_tasks (every 5 minutes) fails a job whose runner has been
// silent for ZOMBIE_TASK_TIMEOUT (10 minutes, the chart keeps the
// default), 10 to 15 minutes after the job's last line (main's run 1208:
// 834 s).
const runnerSilence = 5 * time.Minute

// runnerLost reads a failed job's raw log and the time Forgejo recorded
// its failure (its commit status), and answers the line that says the job
// lost its runner rather than failed a step; Forgejo marks the step failed
// either way. A red step's runner line names its exit
// ("exitcode '1': failure", "RUN exit status 1", a timeout's "context
// deadline exceeded"), and any such line answers false. A runner that
// shut down under the job (main's run 1208) names the job's cancelled
// context, first of the errors its line joins; one whose docker daemon
// restarted names the daemon lost (dockerLost). A log with no runner line
// that Forgejo failed runnerSilence or more after its last line lost its
// runner outright: silent is that gap, the line the log's last. A log with
// no line at all lost it before the job's first (the runner writes its
// set-up lines before any step runs): line is then empty.
func runnerLost(log []byte, recorded time.Time) (line string, silent time.Duration, ok bool) {
	last := ""
	for _, l := range strings.Split(string(log), "\n") {
		if strings.TrimSpace(l) != "" {
			last = l
		}
		msg, ok := strings.CutPrefix(stripLogTimestamp(l), runnerLogPrefix)
		if !ok {
			continue
		}
		// Forgejo's raw log writes the newlines of a row as \n
		first, _, _ := strings.Cut(msg, `\n`)
		if !strings.Contains(first, context.Canceled.Error()) && !slices.ContainsFunc(dockerLost, func(w string) bool { return strings.Contains(first, w) }) {
			return "", 0, false
		}
		line = stripLogTimestamp(l)
	}
	if line != "" {
		return line, 0, true
	}
	if last == "" {
		return "", 0, true
	}
	ts, _, _ := strings.Cut(last, " ")
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || recorded.Sub(at) < runnerSilence {
		return "", 0, false
	}
	return stripLogTimestamp(last), recorded.Sub(at), true
}

// revert files the revert of the pause's red commit: a branch on the
// bot's fork, its pull request, and a factory-PR card the gates and the
// approval take like a roll's, exempt from the pause. A pause on a
// re-run or on a re-run card, its jobs having lost their runner, reverts
// nothing.
func (r *Roller) revert(ctx context.Context, pause QueuePause) error {
	if fix, ok, err := r.Store.Task(ctx, pause.FixTask); err != nil {
		return err
	} else if pause.FixTask == "" || ok && fix.Source == SourceRerun {
		return nil
	}
	p, ok, err := r.Store.ProjectByRepository(ctx, r.Repo.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no project has the repository %s", r.Repo)
	}
	if err := r.Forgejo.EnsureFork(ctx, r.Repo); err != nil {
		return err
	}
	taskID := NewTaskID()
	branch := "factory/" + taskID
	conflict, err := groupRevertBranch(ctx, r.Forgejo, r.Repo, []string{pause.SHA}, "", false, branch)
	if err != nil {
		return fmt.Errorf("revert %s: %w", pause.SHA, err)
	}
	if conflict != "" {
		return fmt.Errorf("revert %s: it conflicts with main", pause.SHA)
	}
	short := pause.SHA[:min(7, len(pause.SHA))]
	title := fmt.Sprintf("Revert %s: main red since %s, no fix landed", short, pause.Since.UTC().Format(time.RFC3339))
	body := fmt.Sprintf("Main's push run %d at %s failed and the fix-forward card %s did not land within %s. This pull request reverts %s; "+
		"an approval merges it and main's next green run ends the queue's pause.", pause.RunID, pause.SHA, pause.FixTask, mainRedBound, short)
	fork := r.Forgejo.Fork(r.Repo)
	pr, err := r.Forgejo.CreatePullRequest(ctx, r.Repo.Owner, r.Repo.Name, fork.Owner+":"+branch, prBase, title, body)
	if err != nil {
		return err
	}
	t, ref, err := FileFactoryPRTask(ctx, r.Store, r.Forgejo, r.Workflows, r.Timeline, r.Budget(), p, title, body, nil, Task{
		ID: taskID, RiskClass: "standard", Objective: title + "\n\n" + body, SHA: pause.SHA, Source: SourceRevert,
	}, pr.Number)
	if err != nil {
		if unfiled(ctx, r.Store, taskID) {
			closeOpened(ctx, r.Forgejo, r.Repo, pr.Number)
		}
		return err
	}
	if err := r.Store.SetQueuePauseRevert(ctx, p.Repository, t.ID); err != nil {
		return err
	}
	log.Printf("roller: main red since %s with no fix landed: revert card %s, issue #%d, pull request #%d", pause.Since.Format(time.RFC3339), t.ID, ref.Number, pr.Number)
	return nil
}
