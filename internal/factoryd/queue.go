package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The merge queue (stage11-design.md 1.4, merge: {queue: project}): one
// pull request of a project merges at a time. A task enters at the
// approval stage and holds its turn through the approval wait and the
// merge; the oldest entry of a project holds the turn. The project is the
// task's repository, as an approval request's is.

// SignalQueueTurn wakes a task waiting in the merge queue: the entry ahead
// of it left.
const SignalQueueTurn = "queue_turn"

// GateMergeQueue is the gate of a task waiting behind others in its
// project's merge queue.

// queueRecheck is how long a waiting task waits for queue_turn before it
// asks for its place again.
const queueRecheck = 5 * time.Minute

// Activity names of the merge queue.
const (
	ActivityEnterQueue       = "EnterQueue"
	ActivitySyncWithMain     = "SyncWithMain"
	ActivityLeaveQueue       = "LeaveQueue"
	ActivityWithdrawApproval = "WithdrawApproval"
)

// queuePrefix heads the findings of red checks on the head the queue's
// sync made, in every round that checks that head.
const queuePrefix = "after main was merged in: "

// QueueSpot is a task's place in its project's merge queue.
type QueueSpot struct {
	Ahead     int // entries before it; 0 holds the turn
	EnteredAt time.Time
	New       bool // this call put it there
}

// EnterQueue puts taskID at the end of project's merge queue unless it is
// in it, and answers its place.
func (s *PGStore) EnterQueue(ctx context.Context, project, taskID string) (QueueSpot, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO merge_queue (project, task_id) VALUES ($1, $2) ON CONFLICT (task_id) DO NOTHING`, project, taskID)
	if err != nil {
		return QueueSpot{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return QueueSpot{}, err
	}
	spot := QueueSpot{New: n == 1}
	err = s.db.QueryRowContext(ctx, `SELECT me.entered_at, (SELECT count(*) FROM merge_queue o WHERE o.project = me.project
		AND (o.entered_at, o.task_id) < (me.entered_at, me.task_id)) FROM merge_queue me WHERE me.task_id = $1`, taskID).Scan(&spot.EnteredAt, &spot.Ahead)
	return spot, err
}

// QueueHead is the task holding project's turn.
func (s *PGStore) QueueHead(ctx context.Context, project string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT task_id FROM merge_queue WHERE project = $1 ORDER BY entered_at, task_id LIMIT 1`, project).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// LeaveQueue takes taskID out of its project's merge queue; left: it was
// in one.
func (s *PGStore) LeaveQueue(ctx context.Context, taskID string) (project string, left bool, err error) {
	err = s.db.QueryRowContext(ctx, `DELETE FROM merge_queue WHERE task_id = $1 RETURNING project`, taskID).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return project, err == nil, err
}

// WaitingQueueHeads are the heads of every merge queue still in gate
// merge-queue: their turn came and no signal has woken them yet.
func (s *PGStore) WaitingQueueHeads(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.task_id FROM (SELECT DISTINCT ON (project) task_id FROM merge_queue
		ORDER BY project, entered_at, task_id) h JOIN tasks t ON t.id = h.task_id WHERE t.gate = $1 ORDER BY h.task_id`, GateMergeQueue)
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

// EnterQueue is the activity that puts the task into its project's merge
// queue and answers how many entries are ahead of it. A task behind
// others is in gate merge-queue since it entered; the head records its
// turn.
func (r *Runner) EnterQueue(ctx context.Context, taskID string) (int, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	spot, err := r.store.EnterQueue(ctx, t.Repository, t.ID)
	if err != nil {
		return 0, err
	}
	if spot.New {
		r.record(ctx, t.ID, "queue_entered", map[string]any{"project": t.Repository, "ahead": spot.Ahead})
	}
	// a paused queue (main is red, mainfix.go) counts as one entry ahead
	// of every task but the pause's own cards
	if p, paused, err := r.store.QueuePause(ctx, t.Repository); err != nil {
		return 0, err
	} else if paused && (t.ID == p.FixTask || t.ID == p.RevertTask) {
		spot.Ahead = 0
	} else if paused {
		if t.Gate != GateMergeQueue {
			r.record(ctx, t.ID, "queue_paused", map[string]any{"project": t.Repository, "run": p.RunID, "sha": p.SHA, "fix": p.FixTask})
		}
		spot.Ahead++
	}
	if spot.Ahead > 0 {
		return spot.Ahead, r.store.SetGate(ctx, t.ID, GateMergeQueue, spot.EnteredAt)
	}
	r.record(ctx, t.ID, "queue_turn", gateEnd(t, GateMergeQueue, map[string]any{"project": t.Repository, "entered": rfc3339(spot.EnteredAt)}))
	if t.Gate == GateMergeQueue {
		return 0, r.store.SetGate(ctx, t.ID, "", time.Now())
	}
	return 0, nil
}

// SyncResult is SyncWithMain's answer: the head From the task passed its
// stages on, the head To after main (at Main) was merged in, equal when
// nothing moved; Conflict is Forgejo's refusal of a main that conflicts.
// Overlap lists the pull request's files that main's new commits (from
// Base, the merge base before the sync) changed too: none, and the
// checks of From hold for To (the cheap turn, roadmap 222, item 4).
type SyncResult struct {
	From, To, Main string
	Conflict       string
	Base           string
	Overlap        []string
}

// SyncWithMain merges main into the head branch of the task's pull
// request pr as the bot, unless its merge base is main's head. from is the
// head the task's stages passed: a retry after an update that went
// through finds the branch up to date and still answers the move.
func (r *Runner) SyncWithMain(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return SyncResult{}, err
	}
	main, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return SyncResult{}, fmt.Errorf("main's head: %w", err)
	}
	st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return SyncResult{}, err
	}
	res := SyncResult{From: from, To: st.HeadSHA, Main: main, Base: st.MergeBase}
	// Forgejo recomputes the merge base in the background: a stale one
	// costs an update that moves nothing
	if st.MergeBase != main {
		err := r.forgejo.UpdatePullRequest(ctx, repo.Owner, repo.Name, pr)
		if errors.Is(err, ErrUpdateConflict) {
			res.Conflict = err.Error()
			return res, nil
		}
		if err != nil {
			return SyncResult{}, err
		}
		if st, err = r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr); err != nil {
			return SyncResult{}, err
		}
		res.To = st.HeadSHA
	}
	if res.To != from {
		if res.Overlap, err = r.syncOverlap(ctx, repo, pr, res.Base, main); err != nil {
			return SyncResult{}, err
		}
		r.record(ctx, t.ID, "queue_synced", map[string]any{"from": from, "to": res.To, "main": main, "overlap": res.Overlap})
	}
	return res, nil
}

// syncOverlap lists the pull request's files (a rename's old name too)
// that main changed between base and main.
func (r *Runner) syncOverlap(ctx context.Context, repo Repo, pr int64, base, main string) ([]string, error) {
	files, err := r.forgejo.PullRequestFiles(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return nil, err
	}
	c, err := r.forgejo.Compare(ctx, repo.Owner, repo.Name, base, main)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, f := range c.Files {
		changed[f] = true
	}
	overlap := []string{}
	for _, f := range files {
		for _, name := range []string{f.Filename, f.PreviousFilename} {
			if name != "" && changed[name] && !slices.Contains(overlap, name) {
				overlap = append(overlap, name)
			}
		}
	}
	return overlap, nil
}

// LeaveQueue is the activity that takes the task out of its project's
// merge queue, why being the way out, and wakes the new head.
func (r *Runner) LeaveQueue(ctx context.Context, taskID, why string) error {
	return r.leaveQueue(ctx, taskID, why)
}

func (r *Runner) leaveQueue(ctx context.Context, taskID, why string) error {
	project, left, err := r.store.LeaveQueue(ctx, taskID)
	if err != nil || !left {
		return err
	}
	r.record(ctx, taskID, "queue_left", map[string]any{"project": project, "why": why})
	head, ok, err := r.store.QueueHead(ctx, project)
	if err != nil || !ok {
		return err
	}
	if err := r.cfg.Workflows.Signal(ctx, head, SignalQueueTurn, TaskSignal{}); err != nil {
		// the heal pass signals a waiting head within the minute
		log.Printf("task %s: signal the queue's new head %s: %v", taskID, head, err)
	}
	return nil
}

// signalQueueHeads wakes every queue head still waiting for its turn (a
// lost queue_turn); a head at its turn is not signalled, so its history
// takes no signal a minute through a long approval wait.
func signalQueueHeads(ctx context.Context, store interface {
	WaitingQueueHeads(ctx context.Context) ([]string, error)
}, wf Workflows) {
	heads, err := store.WaitingQueueHeads(ctx)
	if err != nil {
		log.Printf("heal pass: queue heads: %v", err)
		return
	}
	for _, id := range heads {
		if err := wf.Signal(ctx, id, SignalQueueTurn, TaskSignal{}); err != nil {
			log.Printf("heal pass: signal queue head %s: %v", id, err)
		}
	}
}

// queued: the task's process merges one pull request of the project at a
// time (versionMergeQueue); a roll takes no turn (versionRollNoQueue).
func (t *taskRun) queued() bool {
	if t.rolling && t.rollQueue >= versionRollNoQueue {
		return false
	}
	return t.mergeQueue >= versionMergeQueue && t.cfg.Task.Queue == policy.QueueProject
}

// turn waits, at no cost, for the task's turn in its project's merge
// queue, then merges main into head (SyncWithMain). stop ends the stages
// with v (a terminal signal, a change request, a conflict, a failed
// activity).
func (t *taskRun) turn(head string) (sync SyncResult, v verdict, stop bool) {
	for {
		var ahead int
		if err := workflow.ExecuteActivity(t.short, ActivityEnterQueue, t.taskID).Get(t.ctx, &ahead); err != nil {
			return sync, verdict{final: "failed", reason: "enter the merge queue: " + errMessage(err)}, true
		}
		if ahead == 0 {
			break
		}
		timerCtx, cancel := workflow.WithCancel(t.ctx)
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalQueueTurn, SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.AddFuture(workflow.NewTimer(timerCtx, queueRecheck), func(workflow.Future) {})
		sel.Select(t.ctx)
		cancel()
		for workflow.GetSignalChannel(t.ctx, SignalQueueTurn).ReceiveAsync(nil) {
		}
		if final, why, ok := t.decided(); ok {
			return sync, verdict{final: final, reason: why}, true
		}
		if changes := t.drainChanges(); len(changes) > 0 {
			return sync, verdict{findings: changes, reason: "changes requested", requested: true}, true
		}
	}
	if err := workflow.ExecuteActivity(t.short, ActivitySyncWithMain, t.taskID, t.pr.Number, head).Get(t.ctx, &sync); err != nil {
		return sync, verdict{final: "failed", reason: "merge main into the pull request: " + errMessage(err)}, true
	}
	if sync.Conflict != "" {
		return sync, conflictVerdict(sync.Conflict), true
	}
	return sync, verdict{}, false
}

// queueTurn takes the task's turn (turn) and runs the checks stage again
// on a head that moved, recorded as factoryd's own move like a re-run's.
// findings are the red checks on the synced head; stop ends the stages
// with v. The caller leaves the queue.
func (t *taskRun) queueTurn(head *string, g *GateResults, review *Review) (findings []string, v verdict, stop bool) {
	sync, v, stop := t.turn(*head)
	if stop {
		return nil, v, true
	}
	if sync.To == *head {
		return nil, verdict{}, false
	}
	if t.cheapTurn >= versionCheapTurn && len(sync.Overlap) == 0 {
		// the cheap turn: main's new commits touch no file of the pull
		// request, so the checks that passed on the old head hold, and
		// the merge reads their green there (checkedHead)
		t.checkedHead, *head, t.syncedHead = *head, sync.To, sync.To
		return nil, verdict{}, false
	}
	*head, t.rerunHead, t.syncedHead = sync.To, sync.To, sync.To
	g.ChecksPassed = false
	return t.runStage(policy.StageChecks, head, g, review)
}

// maxRestages bounds the re-runs of a queue head red at its merge, per
// queue entry (versionRedHeadRestage).
const maxRestages = 3

// leftQueue opens the escalation of a queue head still red after its
// re-runs, the red checks below it.
const leftQueue = "the checks are still red after the merge queue's re-runs, so the pull request left the queue until a human answers"

// approvalWithdrawn is the note of a merge approval request the
// re-stage of its red head withdrew.
const approvalWithdrawn = "withdrawn: the checks were red at the merge; the merge queue runs them again, and a green head gets a new request"

// restageable: a queue head red at its merge goes back through its turn
// (versionRedHeadRestage), after its last re-run once more, to leave the
// queue on its red checks.
func (t *taskRun) restageable() bool {
	return t.redRestage >= versionRedHeadRestage && t.queued() && t.restages <= maxRestages
}

// restage takes a queue head whose checks were red at its merge back
// through its turn; the approval request the red merge left open is
// withdrawn first. The turn merges main in: a red main pauses the queue,
// so the turn comes back once a fix made main green, and a head that
// moved runs its checks. With nothing new from main the head's own checks
// are waited on and the red ones run again on an empty commit
// (RerunChecks), at most maxRestages times; red checks that pass there,
// on the same tree, are flaky (checks_flaky), never findings. A green
// head goes to the approval again. A head still red after the last
// re-run leaves the queue, so the cards behind it merge, and waits
// outside it for a human, the escalation naming its red checks: an
// answer puts it back at the queue's end, a new entry with its re-runs
// afresh, and a change request sends it to the agent, whose fix enters
// the queue anew. The re-staged head's own checks decide its merge (no
// checkedHead).
func (t *taskRun) restage(commit string, g GateResults, review Review, reviewed bool, capped, card, covered []string) verdict {
	defer func() { t.restages = 0 }()
	for t.restages = 1; ; t.restages++ {
		if err := workflow.ExecuteActivity(t.short, ActivityWithdrawApproval, t.taskID).Get(t.ctx, nil); err != nil {
			return verdict{final: "failed", reason: "withdraw the approval request: " + errMessage(err)}
		}
		sync, v, stop := t.turn(commit)
		if stop {
			return v
		}
		from, moved := commit, sync.To != commit
		t.checkedHead = ""
		var red []string
		if !moved {
			// the turn re-runs the head's red checks itself, not the
			// checks stage
			t.redHead = ""
			if red, v, stop = t.runStage(policy.StageChecks, &commit, &g, &review); stop {
				return v
			}
		}
		if !moved && len(red) > 0 && t.restages > maxRestages {
			t.leaveQueue(leftQueue)
			if v, stop := t.gateEscalated(leftQueue + "\n\n" + strings.Join(red, "\n\n")); stop {
				return v
			}
			t.restages = 0
			continue
		}
		if moved || len(red) > 0 {
			if moved {
				commit, t.syncedHead = sync.To, sync.To
			} else if err := workflow.ExecuteActivity(t.short, ActivityRerunChecks, t.taskID, commit).Get(t.ctx, &commit); err != nil {
				return verdict{final: "failed", reason: "re-run the checks: " + errMessage(err)}
			}
			t.rerunHead, g.ChecksPassed = commit, false
			findings, v, stop := t.runStage(policy.StageChecks, &commit, &g, &review)
			switch {
			case stop:
				return v
			case len(findings) > 0:
				continue
			case len(red) > 0:
				checks := make([]string, len(red))
				for i, f := range red {
					checks[i] = FirstLine(f)
				}
				detail := map[string]any{"commit": from, "head": commit, "checks": checks}
				if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "checks_flaky", detail).Get(t.ctx, nil); err != nil {
					workflow.GetLogger(t.ctx).Warn("record flaky checks", "task", t.taskID, "error", err)
				}
			}
		}
		if v = t.approval(commit, g, review, reviewed, capped, card, covered); !v.restage {
			return v
		}
	}
}

// WithdrawApproval withdraws the task's pending merge approval request
// while the merge queue re-stages its red head (versionRedHeadRestage):
// an approval sent meanwhile finds no request open, and a green head gets
// a new one.
func (r *Runner) WithdrawApproval(ctx context.Context, taskID string) error {
	return r.store.ResolveApprovals(ctx, taskID, ApprovalMerge, "denied", FactoryDecider, approvalWithdrawn)
}

// leaveQueue takes the task out of its project's merge queue on its way
// out of the turn; a failure is logged: Finish leaves the queue too.
func (t *taskRun) leaveQueue(why string) {
	if err := workflow.ExecuteActivity(t.short, ActivityLeaveQueue, t.taskID, why).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("leave the merge queue", "task", t.taskID, "error", err)
	}
}
