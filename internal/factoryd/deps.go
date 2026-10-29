package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The dependency wait (stage11-design.md section 1.5): a card of a process
// with the dependencies block starts once every card it depends on is done.
const (
	// SignalDepEnded wakes a waiting card: a card it depends on ended.
	SignalDepEnded = "dep_ended"
	// GateDependencies is the gate of a card in waiting-deps.
	// ActivityDependencies asks where a card's dependencies stand.
	ActivityDependencies = "Dependencies"
)

// stepDepsStarted is the timeline step of a human's answer that starts a
// card despite a dependency that ended closed; its reason is the card's
// first finding.
const stepDepsStarted = "dependencies_started"

// depRecheck is how often a waiting card asks again without a dep_ended.
const depRecheck = 10 * time.Minute

// Dependency is a card another waits on, with its status.
type Dependency struct {
	TaskID string
	Issue  int64
	Key    string // its plan key
	Status string
}

// DepState is where a card's dependencies stand: Ended, one that ended
// closed; else Waiting, the open ones' issues ascending, a failed one's
// included until it is retried; neither: the card starts.
type DepState struct {
	Waiting []int64
	Ended   *Dependency
}

// ended is the first dependency that ended closed: a failed one waits
// for its retry (item 119).
func ended(deps []Dependency) *Dependency {
	for _, d := range deps {
		if d.Status == "closed" {
			return &d
		}
	}
	return nil
}

func (s *PGStore) AddTaskDeps(ctx context.Context, id string, dependsOn []string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, id, dependsOn)
	return err
}

func (s *PGStore) RemoveTaskDeps(ctx context.Context, id string, dependsOn []string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_deps WHERE task_id = $1 AND depends_on = ANY($2::text[])`, id, dependsOn)
	return err
}

func (s *PGStore) OpenDependencies(ctx context.Context, id string) ([]Dependency, error) {
	return s.dependencies(ctx, `SELECT t.id, t.issue_number, COALESCE(t.plan_key, ''), t.status
		FROM task_deps d JOIN tasks t ON t.id = d.depends_on
		WHERE d.task_id = $1 AND t.status <> 'done' ORDER BY t.issue_number, t.id`, id)
}

// Dependencies are the cards task id depends on, those not done first.
func (s *PGStore) Dependencies(ctx context.Context, id string) ([]Dependency, error) {
	return s.dependencies(ctx, `SELECT t.id, t.issue_number, COALESCE(t.plan_key, ''), t.status
		FROM task_deps d JOIN tasks t ON t.id = d.depends_on
		WHERE d.task_id = $1 ORDER BY t.status = 'done', t.issue_number, t.id`, id)
}

// Dependents are the cards that depend on task id.
func (s *PGStore) Dependents(ctx context.Context, id string) ([]Dependency, error) {
	return s.dependencies(ctx, `SELECT t.id, t.issue_number, COALESCE(t.plan_key, ''), t.status
		FROM task_deps d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on = $1 ORDER BY t.issue_number, t.id`, id)
}

// dependencies are the cards query selects (id, issue, plan key, status)
// for task id.
func (s *PGStore) dependencies(ctx context.Context, query, id string) ([]Dependency, error) {
	rows, err := s.db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dependency
	for rows.Next() {
		var d Dependency
		if err := rows.Scan(&d.TaskID, &d.Issue, &d.Key, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PGStore) WaitingDependents(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.task_id FROM task_deps d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on = $1 AND t.status = 'waiting-deps' ORDER BY d.task_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *PGStore) ReplaceFailedCard(ctx context.Context, id string, t Task) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	// the key's unique index holds at every statement: the failed card lets go first
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET plan_key = NULL WHERE id = $1 AND status = 'failed' AND plan_key = $2`, id, t.PlanKey)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := insertTask(ctx, tx, t, ""); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) SELECT $2, depends_on FROM task_deps WHERE task_id = $1`,
		id, t.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_deps SET depends_on = $2 WHERE depends_on = $1`, id, t.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PGStore) MoveDependents(ctx context.Context, from, to string) error {
	_, err := s.db.ExecContext(ctx, `WITH moved AS (DELETE FROM task_deps WHERE depends_on = $1 RETURNING task_id)
		INSERT INTO task_deps (task_id, depends_on) SELECT task_id, $2 FROM moved WHERE task_id <> $2 ON CONFLICT DO NOTHING`, from, to)
	return err
}

// Dependencies tells the workflow where the card's dependencies stand. A
// card that has to wait enters waiting-deps in gate dependencies (once:
// the gate keeps the wait's start), also a healed one in waiting-human
// whose question named a dependency that failed, which now waits for its
// retry; one whose wait is over records its end.
func (r *Runner) Dependencies(ctx context.Context, taskID string) (DepState, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return DepState{}, err
	}
	deps, err := r.store.OpenDependencies(ctx, taskID)
	if err != nil {
		return DepState{}, err
	}
	if d := ended(deps); d != nil {
		return DepState{Ended: d}, nil
	}
	if len(deps) == 0 {
		if t.Status == "waiting-deps" {
			r.record(ctx, taskID, "dependencies_done", gateEnd(t, GateDependencies, map[string]any{}))
		}
		return DepState{}, nil
	}
	var st DepState
	for _, d := range deps {
		st.Waiting = append(st.Waiting, d.Issue)
	}
	if t.Status != "waiting-deps" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"queued", "waiting-human"}, "waiting-deps"); err != nil {
			return DepState{}, terminalError("task %s is %s, cannot wait on its dependencies: %v", taskID, t.Status, err)
		}
	}
	// the gate last: a retry after the status moved finishes the entry
	if t.Gate == GateDependencies {
		return st, nil
	}
	if err := r.store.SetGate(ctx, taskID, GateDependencies, time.Now()); err != nil {
		return DepState{}, err
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-deps")
	r.record(ctx, taskID, "dependencies_wait", map[string]any{"on": st.Waiting})
	return st, nil
}

// depEndedReason is the card's question when dependency d ended closed.
func depEndedReason(d Dependency) string {
	return fmt.Sprintf("dependency #%d (%s) ended %s: close this card, or answer to start it anyway", d.Issue, d.Key, d.Status)
}

// signalDependents wakes the waiting cards that depend on taskID, which
// just turned final. A failed signal is logged: the heal pass and the
// wait's re-check cover it.
func signalDependents(ctx context.Context, store Store, wf Workflows, taskID string) {
	if wf == nil {
		return
	}
	ids, err := store.WaitingDependents(ctx, taskID)
	if err != nil {
		log.Printf("task %s: the cards waiting on it: %v", taskID, err)
		return
	}
	for _, id := range ids {
		if err := wf.Signal(ctx, id, SignalDepEnded, TaskSignal{}); err != nil {
			log.Printf("task %s: signal %s to %s: %v", taskID, SignalDepEnded, id, err)
		}
	}
}

// healDependencyWait signals dep_ended to waiting card t when its wait
// would end: every dependency done, or one ended closed.
func healDependencyWait(ctx context.Context, store Store, wf Workflows, t Task) {
	deps, err := store.OpenDependencies(ctx, t.ID)
	if err != nil {
		log.Printf("heal pass: dependencies of %s: %v", t.ID, err)
		return
	}
	if len(deps) > 0 && ended(deps) == nil {
		return
	}
	if err := wf.Signal(ctx, t.ID, SignalDepEnded, TaskSignal{}); err != nil {
		log.Printf("heal pass: signal %s to %s: %v", SignalDepEnded, t.ID, err)
	}
}

// waitsForDependencies: the run has a card of a process with the
// dependencies block that no session has touched yet (versionDepWait).
func (t *taskRun) waitsForDependencies(st RunState, u TaskUsage) bool {
	return t.depWait >= versionDepWait && t.cfg.Task.Dependencies != nil && !st.Skip && !t.isReport(st) && !st.Roll &&
		u.Attempts == 0 && st.PR.Number == 0 && !st.Screening && !st.Merged && st.Status != "closing"
}

// awaitDependencies holds the card until its dependencies are done, at no
// cost: it asks, waits for dep_ended or depRecheck, and asks again. A
// dependency that ended closed goes to a human (healed: asked
// again as the healed question): an answer starts the card with the reason
// as a finding, and the timeline keeps it (dependencies_started), so a
// later run starts the card without asking again; a close ends it closed,
// also one found closed by Reconcile.
func (t *taskRun) awaitDependencies(st RunState) (final, why string, findings []string) {
	switch {
	case st.IssueClosed:
		_, why, _ := t.issueClosed()
		return "closed", why, nil
	case st.DepStarted != "":
		return "", "", []string{st.DepStarted}
	}
	healed := st.Status == "waiting-human"
	for {
		var d DepState
		if err := workflow.ExecuteActivity(t.short, ActivityDependencies, t.taskID).Get(t.ctx, &d); err != nil {
			return "failed", "dependencies: " + errMessage(err), nil
		}
		if d.Ended != nil {
			reason := depEndedReason(*d.Ended)
			var final, why string
			var answer []string
			if healed {
				final, why, answer = t.awaitHumanAgain(escalationQuestion(reason), nil)
			} else {
				final, why, answer = t.awaitHuman(reason, nil)
			}
			if final != "" {
				return closedBeforeStart(final, why)
			}
			if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, stepDepsStarted, map[string]any{"reason": reason}).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("record dependencies_started", "task", t.taskID, "error", err)
			}
			return "", "", append([]string{reason}, answer...)
		}
		if len(d.Waiting) == 0 {
			return "", "", nil
		}
		// back in the wait, a later question is the dependency's own
		healed = false
		timerCtx, cancel := workflow.WithCancel(t.ctx)
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalDepEnded, SignalIssueClosed} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.AddFuture(workflow.NewTimer(timerCtx, depRecheck), func(workflow.Future) {})
		sel.Select(t.ctx)
		cancel()
		for workflow.GetSignalChannel(t.ctx, SignalDepEnded).ReceiveAsync(nil) {
		}
		if final, why, ok := t.decided(); ok {
			return closedBeforeStart(final, why)
		}
	}
}

// closedBeforeStart: a card's issue closed before it started ends the card
// closed; any other end stays as it is.
func closedBeforeStart(final, why string) (string, string, []string) {
	if final == "failed" && strings.HasPrefix(why, ReasonIssueClosed) {
		return "closed", why, nil
	}
	return final, why, nil
}

// ErrNoRetry refuses the retry of a task that is no failed card of a plan
// or was retried; its message is the console's.
type ErrNoRetry string

func (e ErrNoRetry) Error() string { return string(e) }

// RetryID is the id of failed card id's replacement, so a retry that
// stopped part way finds it.
func RetryID(id string) string {
	sum := sha256.Sum256([]byte("retry " + id))
	return "t-" + hex.EncodeToString(sum[:6])
}

// RetryPlanCard files failed plan card taskID again for by (item 119). One
// transaction puts the replacement in its place, once: the key, the
// dependencies and the dependents; its base stays the failed card's. Then
// the replacement gets its issue as fileChildren files a card's and the
// failed card's branch, if any, to start from; the dependents wake, and the
// failed card's issue closes as factoryd's recorded close. Each step after
// the transaction is safe to repeat: a retry that stopped is finished by
// the next. The replacement is queued on the direct track, where it starts
// under the work bound once its branch is copied. It returns its issue.
func (r *Runner) RetryPlanCard(ctx context.Context, taskID, by string) (int64, error) {
	old, repo, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	retried := ErrNoRetry(fmt.Sprintf("Card #%d was already retried.", old.IssueNumber))
	switch {
	case old.Source != SourcePlan || old.Parent == "":
		return 0, ErrNoRetry(fmt.Sprintf("Task %s is no card of a plan.", old.ID))
	case old.Status != "failed":
		return 0, ErrNoRetry(fmt.Sprintf("Card #%d did not fail: it is %s.", old.IssueNumber, old.Status))
	}
	tl, err := r.objects.Get(ctx, "sessions/"+old.ID+"/timeline.jsonl")
	if err != nil {
		return 0, fmt.Errorf("the timeline of %s: %w", old.ID, err)
	}
	if lastStep(tl, "retried") != nil {
		return 0, retried
	}
	t, found, err := r.store.Task(ctx, RetryID(old.ID))
	if err != nil {
		return 0, err
	}
	if !found {
		if old.PlanKey == "" {
			return 0, retried
		}
		p, ok, err := r.store.ProjectByRepository(ctx, old.Repository)
		if err != nil {
			return 0, err
		}
		class, has := trackClass(p, policy.TrackDirect)
		if !ok || !has {
			return 0, errNoTrack(p)
		}
		reason, _ := lastStep(tl, "final")["reason"].(string)
		objective := old.Objective + "\n\n" + r.untrusted(old, fmt.Sprintf("Replaces #%d, which failed: %s", old.IssueNumber,
			cmp.Or(reason, "the timeline records no reason")))
		t = Task{ID: RetryID(old.ID), Priority: old.Priority, RiskClass: old.RiskClass, BudgetEUR: old.BudgetEUR, Status: "queued",
			Track: policy.TrackDirect, RequiredCapabilities: []string{class}, Process: old.Process, Objective: objective, Repository: old.Repository,
			SHA: old.SHA, Kind: "agent", Source: SourcePlan, Parent: old.Parent, PlanKey: old.PlanKey, Image: old.Image, Author: old.Author, AuthorWriter: old.AuthorWriter}
		switch ok, err := r.store.ReplaceFailedCard(ctx, old.ID, t); {
		case err != nil:
			return 0, err
		case !ok:
			return 0, retried
		}
	}
	// past the commit, a request whose client left must not stop the retry half done
	ctx = context.WithoutCancel(ctx)
	stopped := func(step string, err error) error {
		return fmt.Errorf("the retry of %s as %s stopped at %s (Retry finishes it): %w", old.ID, t.ID, step, err)
	}
	if t.IssueNumber == 0 {
		title, body, _ := strings.Cut(t.Objective, "\n\n")
		ref, err := r.forgejo.CreateIssue(ctx, repo.Owner, repo.Name, title, body)
		if err != nil {
			return 0, stopped("filing its issue", err)
		}
		if err := r.store.SetTaskIssue(ctx, t.ID, ref.Number); err != nil {
			return 0, stopped("linking its issue #"+fmt.Sprint(ref.Number), err)
		}
		t.IssueNumber = ref.Number
		r.timeline.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": by, "issue": t.IssueNumber, "plan": old.Parent, "replaces": old.ID, "track": t.Track})
	}
	// a retry that stopped at the labels labels it the next time
	if t.Status == "queued" {
		if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber, []string{LabelAgent, "state/queued"}); err != nil {
			return 0, stopped("labelling its issue", err)
		}
	}
	if err := r.forgejo.CopyBranch(ctx, r.forgejo.Fork(repo), taskBranch(old), taskBranch(t)); err != nil {
		return 0, stopped("copying the branch", err)
	}
	if err := r.cfg.Workflows.Start(ctx, t.ID); err != nil {
		log.Printf("task %s: start the replacement %s: %v", old.ID, t.ID, err)
	}
	signalDependents(ctx, r.store, r.cfg.Workflows, t.ID)
	if err := closeIssueAsFactory(ctx, r.store, r.forgejo, old, fmt.Sprintf("factoryd: Retried as #%d.", t.IssueNumber)); err != nil {
		return 0, stopped("closing the failed card's issue", err)
	}
	r.record(ctx, old.ID, "retried", map[string]any{"by": by, "as": t.ID, "issue": t.IssueNumber})
	return t.IssueNumber, nil
}

// ErrNoGo refuses the go of a card that is no human card waiting on a
// person; its message is the console's.
type ErrNoGo string

func (e ErrNoGo) Error() string { return string(e) }

// Go files human card taskID's work as an agent card (the go action) and
// returns its issue: the card's own words become the agent's objective,
// a routine's card with the skill it names and who handed it over, and
// the run's row moves to the new card, so the hold keeps holding and the
// run ends with the card that ran the work. The hand-over is claimed
// before the filing, so of two racing goes only the first files; a
// filing that fails releases the claim for the retry. The card itself is
// left as it was: the close that follows records the hand-over.
func (r *Runner) Go(ctx context.Context, taskID, by string) (int64, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	switch {
	case t.Kind != "human":
		return 0, ErrNoGo(fmt.Sprintf("Card #%d is an agent card: the factory runs it.", t.IssueNumber))
	case t.Status != "waiting-human":
		return 0, ErrNoGo(fmt.Sprintf("Card #%d does not wait on you: it is %s.", t.IssueNumber, t.Status))
	}
	v := r.cfg.Policy()
	run, filed, err := r.Routines.RoutineRunOfTask(ctx, taskID)
	if err != nil {
		return 0, err
	}
	// the run row left the card: the go already filed its hand-over
	if t.Source == SourceRoutine && !filed {
		return 0, ErrNoGo(fmt.Sprintf("Card #%d was already handed over.", t.IssueNumber))
	}
	name, labels := "", []string(nil)
	if filed {
		name, labels = run.Project, []string{"routine"}
	} else {
		var ok bool
		if name, _, ok = v.ProjectOf(t.Repository); !ok {
			return 0, fmt.Errorf("task %s: no project of %s in the configuration", taskID, t.Repository)
		}
	}
	p, ok, err := r.Routines.Project(ctx, name)
	if err != nil || !ok {
		return 0, cmp.Or(err, fmt.Errorf("the go of %s: no project %s in the store", taskID, name))
	}
	if p.Agents == policy.AgentsNone {
		return 0, ErrNoGo(fmt.Sprintf("project %s runs no agents: the factory cannot do this card; do it yourself and press Done", name))
	}
	title, body, _ := strings.Cut(t.Objective, "\n\n")
	if filed {
		parts := make([]string, 0, 3)
		if rt, ok := v.Routine(run.Project, run.Routine); ok && rt.Skill != "" {
			parts = append(parts, "Run the skill "+rt.Skill+".")
		}
		if body != "" {
			parts = append(parts, body)
		}
		parts = append(parts, "handed over by "+by)
		body = strings.Join(parts, "\n\n")
	}
	objective := title
	if body != "" {
		objective += "\n\n" + body
	}
	claim, err := r.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: taskID, Kind: IntakeHandover, Author: by, Body: "handed to the factory"})
	if err != nil {
		return 0, err
	}
	// the claim lost: another hand filed this card's hand-over already
	if claim == 0 {
		return 0, ErrNoGo(fmt.Sprintf("Card #%d was already handed over.", t.IssueNumber))
	}
	n, ref, err := FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, p, title, body, labels,
		Task{ID: NewTaskID(), RiskClass: t.RiskClass, BudgetEUR: v.Document.Budget.ClampBudget(0), Objective: objective, Source: t.Source})
	if err != nil {
		if rerr := r.store.ReleaseHandover(context.WithoutCancel(ctx), taskID); rerr != nil {
			log.Printf("factoryd: release the hand-over claim of %s: %v", taskID, rerr)
		}
		return 0, err
	}
	// past the filing, a request whose client left must not leave the run on the card the close ends
	ctx = context.WithoutCancel(ctx)
	if err := r.Routines.RepointRoutineRun(ctx, taskID, n.ID, ref.Number); err != nil {
		return 0, fmt.Errorf("task %s: hand its routine run to %s: %w", taskID, n.ID, err)
	}
	return ref.Number, nil
}

// ErrNoBranch means the branch of the card to continue is gone.
var ErrNoBranch = errors.New("the branch of the card to continue does not exist")

// ContinueBranch puts card to's branch on the bot's fork at the head of card
// from's branch and returns that head. to's first session then starts there
// (startRef reads the branch first); the caller files to with from's base. A
// branch of to that exists is kept: a filing that is repeated finds it.
func (r *Runner) ContinueBranch(ctx context.Context, from, to Task) (string, error) {
	_, repo, err := r.task(ctx, from.ID)
	if err != nil {
		return "", err
	}
	fork := r.forgejo.Fork(repo)
	if head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(to)); err == nil {
		return head, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("branch head of %s: %w", to.ID, err)
	}
	if _, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(from)); errors.Is(err, ErrNotFound) {
		return "", ErrNoBranch
	} else if err != nil {
		return "", fmt.Errorf("branch head of %s: %w", from.ID, err)
	}
	if err := r.forgejo.CopyBranch(ctx, fork, taskBranch(from), taskBranch(to)); err != nil {
		return "", fmt.Errorf("copy the branch of %s: %w", from.ID, err)
	}
	head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(to))
	if errors.Is(err, ErrNotFound) {
		return "", ErrNoBranch
	}
	return head, err
}
