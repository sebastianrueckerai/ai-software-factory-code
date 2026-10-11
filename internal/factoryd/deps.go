package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
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
	GateDependencies = "dependencies"
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
			ask := reason
			if healed {
				ask = healedReason
			}
			final, why, answer := t.awaitHuman(ask, reason, nil)
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
// the replacement gets its issue as CreateChildren files a card's and the
// failed card's branch, if any, to start from; the dependents wake, and the
// failed card's issue closes as factoryd's recorded close. Each step after
// the transaction is safe to repeat: a retry that stopped is finished by
// the next. It returns the replacement's issue, which lane ready takes
// once a human labels it.
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
		reason, _ := lastStep(tl, "final")["reason"].(string)
		objective := old.Objective + "\n\n" + r.untrusted(old, fmt.Sprintf("Replaces #%d, which failed: %s", old.IssueNumber,
			cmp.Or(reason, "the timeline records no reason")))
		t = Task{ID: RetryID(old.ID), Priority: old.Priority, RiskClass: old.RiskClass, BudgetEUR: old.BudgetEUR, Status: "backlog",
			Lane: policy.LaneBacklog, Process: old.Process, Objective: objective, Repository: old.Repository, SHA: old.SHA, Kind: "agent",
			Source: SourcePlan, Parent: old.Parent, PlanKey: old.PlanKey, Author: old.Author, AuthorWriter: old.AuthorWriter}
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
		r.timeline.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": by, "issue": t.IssueNumber, "plan": old.Parent, "replaces": old.ID})
	}
	if t.Status == "backlog" {
		if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber, []string{LabelBacklog, "state/backlog"}); err != nil {
			return 0, stopped("labelling its issue", err)
		}
	}
	if err := r.forgejo.CopyBranch(ctx, r.forgejo.Fork(repo), taskBranch(old), taskBranch(t)); err != nil {
		return 0, stopped("copying the branch", err)
	}
	signalDependents(ctx, r.store, r.cfg.Workflows, t.ID)
	note := fmt.Sprintf("factoryd: Retried as #%d.", t.IssueNumber)
	have, err := r.forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, old.IssueNumber)
	if err != nil {
		return 0, stopped("reading the failed card's comments", err)
	}
	if !slices.ContainsFunc(have, func(c IssueComment) bool { return c.Author == r.forgejo.BotUser && c.Body == note }) {
		if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, old.IssueNumber, note); err != nil {
			return 0, stopped("commenting on the failed card's issue", err)
		}
	}
	if err := recordFactoryClose(ctx, r.store, old); err != nil {
		return 0, stopped("recording the close", err)
	}
	if err := r.forgejo.CloseIssue(ctx, repo.Owner, repo.Name, old.IssueNumber); err != nil {
		return 0, stopped("closing the failed card's issue", err)
	}
	r.record(ctx, old.ID, "retried", map[string]any{"by": by, "as": t.ID, "issue": t.IssueNumber})
	return t.IssueNumber, nil
}
