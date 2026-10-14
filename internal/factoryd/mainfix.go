package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The backstop of the cheap merge turn (roadmap 222, item 4): a red push
// run of main pauses the project's merge queue, files the fix-forward
// card, and, when no fix lands within mainRedBound, files the revert of
// the red commit as a factory-PR card. A run whose every failed job lost
// its runner files a human card to re-run them instead, and no revert:
// the commit is not at fault. The next green run of main ends the pause
// and wakes the queue's heads.
const (
	SourceMainFix = "mainfix"
	SourceRerun   = "rerun"
	SourceRevert  = "revert"
	mainRedBound  = 2 * time.Hour
)

// QueuePause is a project's paused merge queue: main is red since the run.
type QueuePause struct {
	Project             string
	RunID               int64
	SHA                 string
	Since               time.Time
	FixTask, RevertTask string
}

func (s *PGStore) PauseQueue(ctx context.Context, p QueuePause) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO queue_pauses (project, run_id, sha, since, fix_task) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project) DO NOTHING`, p.Project, p.RunID, p.SHA, p.Since, p.FixTask)
	return err
}

func (s *PGStore) QueuePause(ctx context.Context, project string) (QueuePause, bool, error) {
	var p QueuePause
	err := s.db.QueryRowContext(ctx, `SELECT project, run_id, sha, since, fix_task, revert_task FROM queue_pauses WHERE project = $1`, project).
		Scan(&p.Project, &p.RunID, &p.SHA, &p.Since, &p.FixTask, &p.RevertTask)
	if errors.Is(err, sql.ErrNoRows) {
		return QueuePause{}, false, nil
	}
	return p, err == nil, err
}

func (s *PGStore) SetQueuePauseRevert(ctx context.Context, project, taskID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE queue_pauses SET revert_task = $2 WHERE project = $1`, project, taskID)
	return err
}

func (s *PGStore) ClearQueuePause(ctx context.Context, project string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM queue_pauses WHERE project = $1`, project)
	return err
}

// mainState reads main's newest finished push run: a red one pauses the
// queue and files the fix-forward card, a green one ends a pause, and a
// pause past the bound with no revert yet files it. runs are the poll's,
// newest first by id.
func (r *Roller) mainState(ctx context.Context, runs []ActionRun) error {
	var newest *ActionRun
	for i := range runs {
		if x := &runs[i]; x.PrettyRef == prBase && (x.Status == "success" || x.Status == "failure") && (newest == nil || x.ID > newest.ID) {
			newest = x
		}
	}
	if newest == nil {
		return nil
	}
	project := r.Repo.String()
	pause, paused, err := r.Store.QueuePause(ctx, project)
	if err != nil {
		return err
	}
	switch {
	case newest.Status == "success" && paused:
		if err := r.Store.ClearQueuePause(ctx, project); err != nil {
			return err
		}
		log.Printf("roller: main is green again at %s (run %d): the merge queue of %s goes on", newest.CommitSHA, newest.ID, project)
		signalQueueHeads(ctx, r.Store, r.Workflows)
		return nil
	case newest.Status == "failure" && !paused:
		return r.pause(ctx, *newest)
	case paused && pause.RevertTask == "" && time.Since(pause.Since) >= mainRedBound:
		return r.revert(ctx, pause)
	}
	return nil
}

// pause pauses the queue on the red run and files the fix-forward card,
// or the human card to re-run the jobs when every failed job lost its
// runner (lostRunners): the card first, so a crash between the two leaves
// no pause without it.
func (r *Roller) pause(ctx context.Context, run ActionRun) error {
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
	var ref IssueRef
	card := "fix-forward card"
	if lost != nil {
		card = "re-run card"
		title := fmt.Sprintf("main is red at %s: re-run %s (runner lost)", short, failed)
		body := fmt.Sprintf("Main CI's push run %d of %s failed, but none of its steps did: the runner of each failed job shut down while "+
			"the job ran, as its own line below says. No change to the code fixes that, so no agent works on this card.\n\n%s\n\n"+
			"Re-run each job from its page. The project's merge queue is paused until a push run of main is green: the re-run's green "+
			"result ends the pause. Close this issue once it has.\n\nfiled by factoryd",
			run.Index, run.CommitSHA, strings.Join(lost, "\n\n"))
		t, ref, err = FileTask(ctx, r.Store, r.Forgejo, p.Repo(), title, body, []string{"human"}, Task{
			ID: NewTaskID(), Kind: "human", RiskClass: "standard", Objective: title + "\n\n" + body, SHA: run.CommitSHA, Source: SourceRerun,
			Author: r.Forgejo.BotUser,
		})
	} else {
		title := fmt.Sprintf("main is red at %s: %s", short, failed)
		body := fmt.Sprintf("Main CI's push run %d of %s failed: %s. The run: %s/%s/actions/runs/%d.\n\n"+
			"The project's merge queue is paused until a push run of main is green. Read the run's log, find the cause in main at %s, "+
			"fix it on this card's branch from main's head, and push: this card merges ahead of the queue, and main's next green run "+
			"ends the pause. A fix that does not land within %s has factoryd file the revert of %s.",
			run.Index, run.CommitSHA, failed, r.Forgejo.BaseURL, r.Repo, run.Index, short, mainRedBound, short)
		t, ref, err = FileAgentTask(ctx, r.Store, r.Forgejo, r.Workflows, r.Timeline, p, title, body, nil, Task{
			ID: NewTaskID(), RiskClass: "standard", BudgetEUR: r.Budget().ClampBudget(0), Objective: title + "\n\n" + body, SHA: run.CommitSHA, Source: SourceMainFix,
		})
	}
	if err != nil {
		return err
	}
	if err := r.Store.PauseQueue(ctx, QueuePause{Project: p.Repository, RunID: run.ID, SHA: run.CommitSHA, Since: time.Now(), FixTask: t.ID}); err != nil {
		return err
	}
	log.Printf("roller: main is red at %s (run %d): the merge queue of %s pauses; %s %s, issue #%d", run.CommitSHA, run.ID, p.Repository, card, t.ID, ref.Number)
	return nil
}

// lostRunners answers, for each of run's failed statuses, the job's link
// and its runner's line, when every one is a job of run that lost its
// runner (runnerLost); nil when any failed a step, is no job of run, or
// has a log factoryd cannot read: the fix-forward card stays the default.
func (r *Roller) lostRunners(ctx context.Context, run ActionRun, failed []CommitStatus) []string {
	jobs := fmt.Sprintf("/%s/actions/runs/%d/jobs/", r.Repo, run.Index)
	var out []string
	for _, s := range failed {
		if !strings.HasPrefix(s.TargetURL, jobs) {
			return nil
		}
		body, err := r.Forgejo.JobLog(ctx, s.TargetURL)
		if err != nil {
			log.Printf("roller: job log %s: %v", s.TargetURL, err)
			return nil
		}
		line, ok := runnerLost(body)
		if !ok {
			return nil
		}
		out = append(out, fmt.Sprintf("%s, %s%s:\n\n%s", s.Context, r.Forgejo.PublicURL, s.TargetURL, fenced(line)))
	}
	return out
}

// runnerLogPrefix opens a line forgejo-runner writes itself, not a step's
// output (act/runner/logger.go, runners 12 and 13): among them the error
// each failed step ended on.
const runnerLogPrefix = "\u2699\ufe0f [runner]:"

// runnerLost reads a failed job's raw log and answers the runner's line
// when the job lost its runner rather than failed a step. Forgejo marks
// the step failed either way; only the runner's own line for it tells the
// two apart. A red step's names its exit ("exitcode '1': failure", "RUN
// exit status 1"); a step the runner abandoned as it shut down (its pod
// evicted: main's run 1208) names its job's cancelled context, first of
// the errors the line joins. Any other runner line is a red step, and a
// log with none answers false too.
func runnerLost(log []byte) (string, bool) {
	lost := ""
	for _, line := range strings.Split(string(log), "\n") {
		line = stripLogTimestamp(line)
		msg, ok := strings.CutPrefix(line, runnerLogPrefix)
		if !ok {
			continue
		}
		// Forgejo's raw log writes the newlines of a row as \n
		first, _, _ := strings.Cut(msg, `\n`)
		if !strings.Contains(first, context.Canceled.Error()) {
			return "", false
		}
		lost = line
	}
	return lost, lost != ""
}

// revertBranch writes the revert of sha on a new branch of the bot's fork
// and answers the branch; a test substitutes it, git being no fake's.
var revertBranch = func(ctx context.Context, f *Forgejo, repo Repo, sha, branch string) error {
	dir, err := os.MkdirTemp("", "revert-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	login := basicLogin(f.BotUser, f.BotPassword)
	src := filepath.Join(dir, "src")
	if _, err := git(ctx, dir, login, "clone", "-q", "--depth=50", "--branch", prBase, f.BaseURL+"/"+repo.String()+".git", src); err != nil {
		return err
	}
	parents, err := f.CommitParents(ctx, repo.Owner, repo.Name, sha)
	if err != nil {
		return err
	}
	args := []string{"-c", "user.name=" + f.BotUser, "-c", "user.email=" + f.BotUser + "@factory", "revert", "--no-edit"}
	if len(parents) > 1 {
		args = append(args, "-m", "1")
	}
	if _, err := git(ctx, src, login, append(args, sha)...); err != nil {
		return err
	}
	fork := f.Fork(repo)
	_, err = git(ctx, src, login, "push", "-q", f.BaseURL+"/"+fork.String()+".git", "HEAD:refs/heads/"+branch)
	return err
}

// revert files the revert of the pause's red commit: a branch on the
// bot's fork, its pull request, and a factory-PR card the gates and the
// approval take like a roll's, exempt from the pause. A pause on a
// re-run card, its jobs having lost their runner, reverts nothing.
func (r *Roller) revert(ctx context.Context, pause QueuePause) error {
	if fix, ok, err := r.Store.Task(ctx, pause.FixTask); err != nil {
		return err
	} else if ok && fix.Source == SourceRerun {
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
	if err := revertBranch(ctx, r.Forgejo, r.Repo, pause.SHA, branch); err != nil {
		return fmt.Errorf("revert %s: %w", pause.SHA, err)
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
	t, ref, err := FileFactoryPRTask(ctx, r.Store, r.Forgejo, r.Workflows, r.Timeline, p, title, body, nil, Task{
		ID: taskID, RiskClass: "standard", BudgetEUR: r.Budget().ClampBudget(0), Objective: title + "\n\n" + body, SHA: pause.SHA, Source: SourceRevert,
	}, pr.Number)
	if err != nil {
		return err
	}
	if err := r.Store.SetQueuePauseRevert(ctx, p.Repository, t.ID); err != nil {
		return err
	}
	log.Printf("roller: main red since %s with no fix landed: revert card %s, issue #%d, pull request #%d", pause.Since.Format(time.RFC3339), t.ID, ref.Number, pr.Number)
	return nil
}
