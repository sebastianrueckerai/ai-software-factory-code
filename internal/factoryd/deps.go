package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
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

// Dependency is a card another waits on, with its status. DeliveredBy,
// set at a plan's filing only, is the issue of the successor that
// delivered the card's work: the dependency is met and no edge stands.
type Dependency struct {
	TaskID      string
	Issue       int64
	Key         string // its plan key
	Status      string
	DeliveredBy int64
}

// DepState is where a card's dependencies stand: Ended, one that ended
// closed; else Waiting, the open ones' issues ascending, a failed one's
// included until it is retried; neither: the card starts. Failed is the
// first that failed and has no replacement (RetryID) yet. Hold is the
// line of a human's hold, which keeps the card waiting whatever its
// dependencies, none of them asked.
type DepState struct {
	Waiting []int64
	Ended   *Dependency
	Failed  *Dependency
	Hold    string
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

// LiveDependents counts the cards of deps that have not ended.
func LiveDependents(deps []Dependency) int {
	n := 0
	for _, d := range deps {
		if !Final(d.Status) {
			n++
		}
	}
	return n
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

func (s *PGStore) ReplaceCard(ctx context.Context, id, status string, t Task) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	// the key's unique index holds at every statement: the replaced card lets go first
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET plan_key = NULL WHERE id = $1 AND status = $3 AND plan_key IS NOT DISTINCT FROM NULLIF($2, '')`,
		id, t.PlanKey, status)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	// a card outside a plan has no key to let go: the row lock orders two
	// retries of it, and the second finds the first's replacement
	var replaced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`, t.ID).Scan(&replaced); err != nil || replaced {
		return false, err
	}
	if err := InsertTask(ctx, tx, t, ""); err != nil {
		return false, err
	}
	if err := takeOverEdges(ctx, tx, id, t.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PGStore) CreateSuccessor(ctx context.Context, id string, t Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	if err := InsertTask(ctx, tx, t, ""); err != nil {
		return err
	}
	if err := takeOverEdges(ctx, tx, id, t.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// takeOverEdges gives card to, inside tx, every dependency of card from,
// and points every card that waits on from at to, a hold whose release
// card from is too.
func takeOverEdges(ctx context.Context, tx *sql.Tx, from, to string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) SELECT $2, depends_on FROM task_deps WHERE task_id = $1`,
		from, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_deps SET depends_on = $2 WHERE depends_on = $1`, from, to); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET hold_release = $2 WHERE hold_release = $1`, from, to)
	return err
}

// showTakeOver shows on Forgejo the waits card to took over from card
// from (takeOverEdges): to's issue depends on the issues of its
// dependencies, and each issue that waited on from's waits on to's. A call
// that fails is logged: the rows hold the waits, the issues show them.
func (r *Runner) showTakeOver(ctx context.Context, from, to Task) {
	repo, err := SplitRepo(to.Repository)
	if err != nil {
		log.Printf("task %s: the waits it took over from %s: %v", to.ID, from.ID, err)
		return
	}
	deps, err := r.store.Dependencies(ctx, to.ID)
	if err != nil {
		log.Printf("task %s: the waits it took over from %s: %v", to.ID, from.ID, err)
	}
	for _, d := range deps {
		if d.Issue == 0 {
			continue
		}
		if err := r.forgejo.SetIssueDependency(ctx, repo, to.IssueNumber, d.Issue, false); err != nil {
			log.Printf("%s: issue #%d on #%d: %v", repo, to.IssueNumber, d.Issue, err)
		}
	}
	dependents, err := r.store.Dependents(ctx, to.ID)
	if err != nil {
		log.Printf("task %s: the waits it took over from %s: %v", to.ID, from.ID, err)
	}
	issues := make([]int64, len(dependents))
	for i, d := range dependents {
		issues[i] = d.Issue
	}
	r.repointIssues(ctx, repo, issues, from.IssueNumber, to.IssueNumber)
}

func (s *PGStore) MoveDependents(ctx context.Context, from, to string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `WITH moved AS (
			DELETE FROM task_deps d USING tasks c
			WHERE d.depends_on = $1 AND c.id = d.task_id AND `+planCardRow+` AND c.parent_task = $2
			RETURNING d.task_id),
		added AS (INSERT INTO task_deps (task_id, depends_on) SELECT task_id, $2 FROM moved ON CONFLICT DO NOTHING)
		SELECT task_id FROM moved ORDER BY task_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var moved []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		moved = append(moved, id)
	}
	return moved, rows.Err()
}

// Dependencies tells the workflow where the card's dependencies stand,
// naming the first failed one with no replacement yet. A card that has to
// wait, also a healed one in waiting-human whose question named a
// dependency that failed, which now waits for its retry, enters
// waiting-deps in gate dependencies (once: the gate keeps the wait's
// start) and hands its work slot to the next queued card. One whose wait
// is over records its end. A held card waits as long as its hold
// (cardHoldWait), asking nothing of its dependencies; its dependencies
// are read before the card, so a hold set while one was open is read
// here.
func (r *Runner) Dependencies(ctx context.Context, taskID string) (DepState, error) {
	deps, err := r.store.OpenDependencies(ctx, taskID)
	if err != nil {
		return DepState{}, err
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return DepState{}, err
	}
	var st DepState
	if st.Hold, err = cardHoldWait(ctx, r.store, t); err != nil {
		return DepState{}, err
	}
	if st.Hold == "" {
		for _, d := range deps {
			if d.Status != "failed" {
				continue
			}
			_, retried, err := r.store.Task(ctx, RetryID(d.TaskID))
			if err != nil {
				return DepState{}, err
			}
			if !retried {
				st.Failed = &d
				break
			}
		}
		if d := ended(deps); d != nil {
			st.Ended = d
			return st, nil
		}
		if len(deps) == 0 {
			if t.Status == "waiting-deps" {
				r.record(ctx, taskID, "dependencies_done", gateEnd(t, GateDependencies, map[string]any{}))
			}
			return DepState{}, nil
		}
	}
	for _, d := range deps {
		st.Waiting = append(st.Waiting, d.Issue)
	}
	if t.Status != "waiting-deps" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"queued", "waiting-human"}, "waiting-deps"); err != nil {
			return DepState{}, terminalError("task %s is %s, cannot wait on its dependencies: %v", taskID, t.Status, err)
		}
		// the wait holds no work slot: the next queued card takes it
		pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	// the gate last: a retry after the status moved finishes the entry
	if t.Gate == GateDependencies {
		return st, nil
	}
	if err := r.store.SetGate(ctx, taskID, GateDependencies, time.Now()); err != nil {
		return DepState{}, err
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-deps")
	wait := map[string]any{"on": st.Waiting}
	if st.Hold != "" {
		wait["hold"] = st.Hold
	}
	r.record(ctx, taskID, "dependencies_wait", wait)
	return st, nil
}

// DepEndedReason is the card's question when dependency d ended closed.
func DepEndedReason(d Dependency) string {
	return fmt.Sprintf("dependency #%d (%s) ended %s: close this card, or answer to start it anyway", d.Issue, d.Key, d.Status)
}

// depFailed opens the question of a card whose dependency failed with no
// replacement (versionCases), an ask of source depFailedSource.
const (
	depFailed       = "A dependency failed: "
	depFailedSource = "dependency_failed"
)

// DepFailedReason is the card's question when dependency d failed and no
// retry replaced it.
func DepFailedReason(d Dependency) string {
	return fmt.Sprintf(depFailed+"%s, card %s, ended %s with no replacement. Retry it, close this card, or answer to start it anyway.", depName(d), d.TaskID, d.Status)
}

// planCardFailedReason is a plan's question when its card k failed and the
// plan's close or its next round waits on it.
func planCardFailedReason(k Task) string {
	return fmt.Sprintf(depFailed+"%s, card %s, ended failed with no replacement, and this plan's close and its next round wait on it. Retry the card or close it.",
		depName(Dependency{Issue: k.IssueNumber, Key: k.PlanKey}), k.ID)
}

// depName names dependency d by its issue and its plan key.
func depName(d Dependency) string {
	name := fmt.Sprintf("#%d", d.Issue)
	if d.Key != "" {
		name += " (" + d.Key + ")"
	}
	return name
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
// would end: every dependency done, or one ended closed, and no hold
// keeps it but one its release card ended.
func healDependencyWait(ctx context.Context, store Store, wf Workflows, t Task) {
	if t.Held() {
		v, err := holdView(ctx, store, t)
		if err != nil {
			log.Printf("heal pass: the hold of %s: %s", t.ID, maskedErr(err))
		}
		if err != nil || !v.Released {
			return
		}
	}
	deps, err := store.OpenDependencies(ctx, t.ID)
	if err != nil {
		log.Printf("heal pass: dependencies of %s: %s", t.ID, maskedErr(err))
		return
	}
	if len(deps) > 0 && ended(deps) == nil {
		return
	}
	if err := wf.Signal(ctx, t.ID, SignalDepEnded, TaskSignal{}); err != nil {
		log.Printf("heal pass: signal %s to %s: %s", SignalDepEnded, t.ID, maskedErr(err))
	}
}

// waitsForDependencies: the run has a card of a process with the
// dependencies block that no session has touched yet.
func (t *taskRun) waitsForDependencies(st RunState, u TaskUsage) bool {
	return t.cfg.Task.Dependencies != nil && !st.Skip && !t.isReport() && !st.Roll &&
		u.Attempts == 0 && st.PR.Number == 0 && !st.Screening && !st.Merged && st.Status != "closing"
}

// awaitDependencies holds the card until its dependencies are done, at no
// cost: it asks, waits for dep_ended or depRecheck, and asks again. A
// dependency that ended closed goes to a human (healed: asked
// again as the healed question): an answer starts the card with the reason
// as a finding, and the timeline keeps it (dependencies_started), so a
// later run starts the card without asking again; a close ends it closed,
// also one found closed by Reconcile. One that failed with no replacement
// is a trip of its own (versionCases): a retry or a replan of it, rung 0's
// or a human's, puts the card back in the wait on what replaced it, a
// close waits for the issue's close, and any other answer starts the card
// as a closed one's does. A human's hold keeps the card in the wait, with
// no question, until it ends (versionCardHold).
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
		failed := d.Ended == nil && d.Failed != nil
		if d.Ended != nil || failed {
			var reason string
			if failed {
				reason = DepFailedReason(*d.Failed)
			} else {
				reason = DepEndedReason(*d.Ended)
			}
			var final, why string
			var answer []string
			if healed {
				final, why, answer = t.awaitHumanAgain(EscalationQuestion(reason), nil)
			} else {
				final, why, answer = t.awaitHuman(reason, nil)
			}
			if final != "" {
				return closedBeforeStart(final, why)
			}
			switch res := t.takeResolution(); {
			case res != nil && res.Action == ActClose:
				return closedBeforeStart(t.awaitClose())
			case failed && res != nil && (res.Action == ActRetry || res.Action == ActReplan):
				healed = false
				continue
			}
			if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, stepDepsStarted, map[string]any{"reason": reason}).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("record dependencies_started", "task", t.taskID, "error", err)
			}
			return "", "", append([]string{reason}, answer...)
		}
		held := d.Hold != "" && workflow.GetVersion(t.ctx, "card-hold", workflow.DefaultVersion, versionCardHold) >= versionCardHold
		if len(d.Waiting) == 0 && !held {
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

// ErrNoRetry refuses the retry of a card that did not fail or was
// retried; its message is the console's.
type ErrNoRetry string

func (e ErrNoRetry) Error() string { return string(e) }

// RetryObjective is the objective of the retry of failed card old: old's
// title, its text opening with "Continues #N." (ContinuesBody), then the
// line with reason, its final reason ("" for none), masked by mask.
func RetryObjective(old Task, reason string, mask func(string) string) string {
	return IssueObjective(CardTitle(old), ContinuesBody(old)) + "\n\n" + mask(fmt.Sprintf("It failed: %s", cmp.Or(reason, "the timeline records no reason")))
}

// RetryID is the id of failed card id's replacement, so a retry that
// stopped part way finds it.
func RetryID(id string) string {
	sum := sha256.Sum256([]byte("retry " + id))
	return "t-" + hex.EncodeToString(sum[:6])
}

// retryMarker is the marker on a retry's issue, so a retry that stopped
// between the filing and the link finds the issue it filed.
func retryMarker(id string) string {
	return fmt.Sprintf("<!-- factoryd retry of %s -->", id)
}

// RetryCard files failed work card taskID again for by (item 119): a
// card on the direct track that a person, a routine or a plan filed, or
// that carries a plan card's key (Task.PlanCard), and not cut into a
// successor, which it names in its refusal. The
// replacement keeps the card's source, its plan and its key, if any, its
// rung and its size, so it is not sized again. One transaction puts the
// replacement, by its deterministic id RetryID, in its place, once: the
// key, the dependencies and the dependents; its base stays the failed
// card's. Then the replacement gets its issue as fileChildren files a
// card's and the failed card's branch, if any, to start from; the
// dependents wake, the issues show the waits it took over, and the failed
// card's issue closes as factoryd's
// recorded close. Each step after the transaction is safe to repeat: a
// retry that stopped is finished by the next. The press holds the failed
// card's retry lock from its first read to its end, so however many
// presses race it, as the answers on two dependents' cases do, one files
// the replacement's issue and starts its workflow; the rest read its
// retried step. The replacement is queued on
// the direct track, where it starts under the work bound once its branch
// is copied. It returns its issue.
func (r *Runner) RetryCard(ctx context.Context, taskID, by string) (int64, error) {
	// one press at a time per card: a second that races the first waits
	// here, then reads its retried step, so it files no second issue
	lock, _ := r.retryLocks.LoadOrStore(taskID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	old, repo, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	retried := ErrNoRetry(fmt.Sprintf("Card #%d was already retried.", old.IssueNumber))
	switch {
	case old.Track == policy.TrackPlan:
		return 0, ErrNoRetry(fmt.Sprintf("Card #%d is a plan: ask for changes to it, or replan it.", old.IssueNumber))
	case old.Report():
		return 0, ErrNoRetry(fmt.Sprintf("Task %s is the %s report of task %s, not a work card: retry that card if it failed.", old.ID, old.Source, old.Parent))
	case old.Track != policy.TrackDirect || !(old.PlanCard() || slices.Contains([]string{SourceWebhook, SourceAPI, SourceRoutine}, old.Source)):
		return 0, ErrNoRetry(fmt.Sprintf("Card #%d is no work card of a person, a routine or a plan: file the work as a new card if it is still needed.", old.IssueNumber))
	case old.Status != "failed":
		return 0, ErrNoRetry(fmt.Sprintf("Card #%d did not fail: it is %s.", old.IssueNumber, old.Status))
	}
	// a card cut into a successor handed it its work and its dependents
	switch succ, cut, err := r.store.Task(ctx, SuccessorID(old.ID)); {
	case err != nil:
		return 0, err
	case cut:
		return 0, ErrNoRetry(fmt.Sprintf("Card #%d was cut into successor #%d, which carries its work and its dependents: answer that card instead.",
			old.IssueNumber, succ.IssueNumber))
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
		p, ok, err := r.store.ProjectByRepository(ctx, old.Repository)
		if err != nil {
			return 0, err
		}
		class, has := trackClass(p, policy.TrackDirect)
		if !ok || !has {
			return 0, errNoTrack(p)
		}
		reason, _ := lastStep(tl, "final")["reason"].(string)
		objective := RetryObjective(old, reason, func(s string) string { return r.untrusted(old, s) })
		t = Task{ID: RetryID(old.ID), Priority: old.Priority, RiskClass: old.RiskClass, BudgetEUR: old.BudgetEUR, Status: "queued",
			Track: policy.TrackDirect, RequiredCapabilities: []string{class}, Process: old.Process, Objective: objective, Repository: old.Repository,
			SHA: old.SHA, Kind: "agent", Source: old.Source, Parent: old.Parent, PlanKey: old.PlanKey, Rung: old.Rung, Size: old.Size, Image: old.Image, Author: old.Author, AuthorWriter: old.AuthorWriter,
			ToolImage: old.ToolImage, ChatFiled: old.ChatFiled}
		if err := admitCarry(&t, old); err != nil {
			return 0, err
		}
		switch ok, err := r.store.ReplaceCard(ctx, old.ID, "failed", t); {
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
		all, err := r.forgejo.Issues(ctx, repo.Owner, repo.Name)
		if err != nil {
			return 0, stopped("searching its issue", err)
		}
		title, body, _ := strings.Cut(t.Objective, "\n\n")
		marker := retryMarker(old.ID)
		if i := slices.IndexFunc(all, func(is webhookIssue) bool {
			return is.Poster.UserName == r.forgejo.BotUser && strings.Contains(is.Body, marker)
		}); i >= 0 {
			t.IssueNumber = all[i].Index
		} else {
			ref, err := r.forgejo.CreateIssue(ctx, repo.Owner, repo.Name, title, IssueText(BriefText{Why: t.Why, Summary: t.Summary, Body: body})+"\n\n"+marker)
			if err != nil {
				return 0, stopped("filing its issue", err)
			}
			t.IssueNumber = ref.Number
		}
		if err := r.store.SetTaskIssue(ctx, t.ID, t.IssueNumber); err != nil {
			return 0, stopped("linking its issue #"+fmt.Sprint(t.IssueNumber), err)
		}
		created := map[string]any{"by": by, "issue": t.IssueNumber, "replaces": old.ID, "track": t.Track, "budget_eur": t.BudgetEUR}
		if old.Parent != "" {
			created["plan"] = old.Parent
		}
		r.timeline.recordEvent(ctx, t.ID, EventCardCreated, created)
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
	// the sizing step reads a recorded step first: the work was sized once
	if lastStep(tl, StepSizing) != nil {
		if _, err := r.timeline.AppendOnce(ctx, t.ID, StepSizing, map[string]any{"size": t.Size,
			"skipped": fmt.Sprintf("it replaces #%d, which was sized", old.IssueNumber)}); err != nil {
			return 0, stopped("recording its size", err)
		}
	}
	if err := r.cfg.Workflows.Start(ctx, t.ID); err != nil {
		log.Printf("task %s: start the replacement %s: %v", old.ID, t.ID, err)
	}
	signalDependents(ctx, r.store, r.cfg.Workflows, t.ID)
	r.showTakeOver(ctx, old, t)
	if err := closeIssueAsFactory(ctx, r.store, r.forgejo, old, fmt.Sprintf("Retried as #%d.", t.IssueNumber)); err != nil {
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
// returns its issue: the card's own brief becomes the agent's, its text
// opening with "Continues #N.", a routine's card with the skill it names
// and who handed it over, and
// the run's row moves to the new card, so the hold keeps holding and the
// run ends with the card that ran the work. The hand-over is claimed
// before the filing, so of two racing goes only the first files; a
// filing that fails releases the claim for the retry. The card itself is
// left as it was: the close that follows records the hand-over. A plan's
// card's agent card takes its place in the plan (handOver), and the issues
// show the waits it took over. A card whose
// every directory is a workflow directory goes to no agent: the worker's
// push refuses its changes.
func (r *Runner) Go(ctx context.Context, taskID, by string) (int64, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	switch {
	case t.Kind != "human":
		return 0, ErrNoGo(fmt.Sprintf("Card %s is an agent card: the factory runs it.", ShownName(t)))
	case !slices.Contains(humanCardWaits, t.Status):
		return 0, ErrNoGo(fmt.Sprintf("Card %s does not wait on you: it is %s.", ShownName(t), t.Status))
	}
	// the card's directories: its plan card's, else its text's line
	pc, _, planned, err := planCardOf(ctx, r.store, t)
	if err != nil {
		return 0, err
	}
	_, body, _ := strings.Cut(t.Objective, "\n\n")
	dirs := askedDirectories(body)
	if planned {
		dirs = pc.Directories
	}
	if workflowOnly(dirs) {
		return 0, ErrNoGo(fmt.Sprintf("Card #%d changes only files under a workflow directory (%s), and the factory's push refuses changes to workflow files: make the change yourself and press Done.",
			t.IssueNumber, strings.Join(dirs, ", ")))
	}
	v := r.cfg.Policy()
	run, filed, err := r.Routines.RoutineRunOfTask(ctx, taskID)
	if err != nil {
		return 0, err
	}
	// the run row left the card: the go already filed its hand-over
	if t.Source == SourceRoutine && !filed {
		return 0, ErrNoGo(fmt.Sprintf("Card %s was already handed over.", ShownName(t)))
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
	b := BriefText{Title: CardTitle(t), Why: t.Why, Summary: t.Summary, Body: ContinuesBody(t)}
	if filed {
		// a card with no issue continues none, and its text may be empty
		parts := slices.DeleteFunc([]string{b.Body}, func(s string) bool { return s == "" })
		if rt, ok := v.Routine(run.Project, run.Routine); ok && rt.Skill != "" {
			parts = append(parts, "Run the skill "+rt.Skill+".")
		}
		b.Body = strings.Join(append(parts, "handed over by "+by), "\n\n")
	}
	// the agent card keeps what a retry's replacement keeps: the work's
	// priority, size and rung, and the author whose standing its text has.
	// The factory's own cards outside a plan have none: the bot wrote their
	// text, which no alert's telemetry fills. Its budget is the one its text
	// asked for, else its size's
	_, asked := askedBudget(t.Objective)
	agent := Task{ID: NewTaskID(), Priority: t.Priority, RiskClass: t.RiskClass, BudgetEUR: policy.CardBudget(v.Document.Budget, p.Budget, t.Size, t.Rung == RungFrontier, asked),
		Objective: IssueObjective(b.Title, b.Body), Source: t.Source, Rung: t.Rung, Size: t.Size, Image: t.Image, ToolImage: t.ToolImage, ChatFiled: t.ChatFiled}
	inPlace := t.Source == SourcePlan && t.Parent != ""
	if t.Author != r.forgejo.BotUser || inPlace || t.Source == SourceAlert {
		agent.Author, agent.AuthorWriter = t.Author, t.AuthorWriter
	}
	var into interface {
		CreateTask(ctx context.Context, t Task) error
	} = r.store
	if inPlace {
		// a plan's card: the agent card takes its place, as a retry's
		// replacement does, so the plan and its dependents wait on the work
		plan, _, err := r.task(ctx, t.Parent)
		if err != nil {
			return 0, err
		}
		agent.Parent, agent.PlanKey = t.Parent, t.PlanKey
		into = handOver{r.store, t, plan.Process}
	}
	claim, err := r.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: taskID, Kind: IntakeHandover, Author: by, Body: "handed to the factory"})
	if err != nil {
		return 0, err
	}
	// the claim lost: another hand filed this card's hand-over already
	if claim == 0 {
		return 0, ErrNoGo(fmt.Sprintf("Card %s was already handed over.", ShownName(t)))
	}
	n, ref, err := FileAgentTask(ctx, into, r.forgejo, r.cfg.Workflows, r.timeline, p, b, labels, agent)
	if err != nil {
		if rerr := r.store.ReleaseHandover(context.WithoutCancel(ctx), taskID); rerr != nil {
			log.Printf("factoryd: release the hand-over claim of %s: %v", taskID, rerr)
		}
		return 0, err
	}
	// past the filing, a request whose client left must not leave the run on the card the close ends
	ctx = context.WithoutCancel(ctx)
	if inPlace {
		r.showTakeOver(ctx, t, n)
	}
	if err := r.Routines.RepointRoutineRun(ctx, taskID, n.ID, ref.Number); err != nil {
		return 0, fmt.Errorf("task %s: hand its routine run to %s: %w", taskID, n.ID, err)
	}
	return ref.Number, nil
}

// handOver files the agent card a human hands plan card card to the
// factory with in card's place (ReplaceCard): its key, its dependencies
// and its dependents. The agent card follows process, its plan's, as the
// plan's other cards do; the project's when the plan names none.
type handOver struct {
	s       Store
	card    Task
	process string
}

func (h handOver) CreateTask(ctx context.Context, t Task) error {
	t.Process = cmp.Or(h.process, t.Process)
	switch ok, err := h.s.ReplaceCard(ctx, h.card.ID, "waiting-human", t); {
	case err != nil:
		return err
	case !ok:
		return ErrNoGo(fmt.Sprintf("Card #%d no longer waits on you in its plan's place.", h.card.IssueNumber))
	}
	return nil
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
