package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
)

// prBase is the branch every task pull request targets.
const prBase = "main"

// heartbeatEvery is how often a long activity reports progress; the
// workflow's heartbeat timeout is four times this.
const heartbeatEvery = 30 * time.Second

// RegisterActivities registers the runner's activities under the names the
// workflow calls, on a worker or a test environment.
func (r *Runner) RegisterActivities(w interface {
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}) {
	for name, fn := range map[string]any{
		ActivityEnsureIssue:       r.EnsureIssue,
		ActivitySetState:          r.SetState,
		ActivityRunSession:        r.RunSession,
		ActivityOpenPullRequest:   r.OpenPullRequest,
		ActivityWaitChecks:        r.WaitChecks,
		ActivityPullRequestMerged: r.PullRequestMerged,
		ActivityRecordStep:        r.RecordStep,
		ActivityFinish:            r.Finish,
		ActivityReconcile:         r.Reconcile,
		ActivityEscalate:          r.Escalate,
	} {
		w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
}

func (r *Runner) task(ctx context.Context, id string) (Task, Repo, error) {
	t, ok, err := r.store.Task(ctx, id)
	if err != nil {
		return Task{}, Repo{}, err
	}
	if !ok {
		return Task{}, Repo{}, terminalError("task %s not found", id)
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return Task{}, Repo{}, terminalError("%v", err)
	}
	return t, repo, nil
}

// EnsureIssue checks the task is mirrored by a Forgejo issue (both intake
// paths link one before the workflow starts) and opens the timeline.
func (r *Runner) EnsureIssue(ctx context.Context, taskID string) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if t.IssueNumber == 0 {
		return terminalError("task %s has no linked issue", taskID)
	}
	return r.timeline.Append(ctx, taskID, "intake", map[string]any{"source": t.Source, "issue": t.IssueNumber})
}

// SetState moves the task to running (a session starts) or in-review (its
// pull request is open) and mirrors the state onto the issue.
func (r *Runner) SetState(ctx context.Context, taskID, state string) error {
	from := map[string][]string{
		"running":   {"queued", "running", "in-review", "waiting-human"},
		"in-review": {"running", "in-review"},
	}[state]
	if from == nil {
		return terminalError("unknown task state %q", state)
	}
	cur, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if !slices.Contains(from, cur.Status) {
		return terminalError("task %s is %s, cannot move to %s", taskID, cur.Status, state)
	}
	if state == "running" {
		err = r.store.MarkStarted(ctx, taskID)
	} else {
		err = r.store.TransitionTask(ctx, taskID, from, state)
	}
	if err != nil {
		return err
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	mirrorTaskState(ctx, r.forgejo, t, state)
	return nil
}

// sessionProgress is RunSession's heartbeat: a retry after a crashed worker
// reads it to continue the dead session's chain.
type sessionProgress struct {
	SessionID string
	// Running is set once the worker pod runs: a retry after a setup or
	// pod-create failure has no dead session to continue.
	Running  bool
	Events   int
	Findings []string
}

// heartbeat records the latest session progress now and every
// heartbeatEvery until stopped, including through namespace setup and a
// slow image pull, when no events flow.
type heartbeat struct {
	mu   sync.Mutex
	p    sessionProgress
	stop context.CancelFunc
}

func startHeartbeat(ctx context.Context, sessionID string) *heartbeat {
	hbCtx, stop := context.WithCancel(ctx)
	h := &heartbeat{p: sessionProgress{SessionID: sessionID}, stop: stop}
	activity.RecordHeartbeat(ctx, h.snapshot())
	go func() {
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-tick.C:
				activity.RecordHeartbeat(ctx, h.snapshot())
			}
		}
	}()
	return h
}

func (h *heartbeat) update(events int, findings []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p.Running, h.p.Events, h.p.Findings = true, events, findings
}

func (h *heartbeat) snapshot() sessionProgress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sessionProgress{SessionID: h.p.SessionID, Running: h.p.Running, Events: h.p.Events, Findings: slices.Clone(h.p.Findings)}
}

// RunSession is one attempt: a fresh namespace with this session's own
// Forgejo token and service account (so the gateway mints this session its
// own LiteLLM key), one worker pod, supervision, and the session record.
// A retry after a crashed worker finds the dead session in the heartbeat,
// removes its namespace and pod with its credentials, and starts a new
// session carrying the dead one's findings and id as parent.
func (r *Runner) RunSession(ctx context.Context, in SessionInput) (SessionResult, error) {
	t, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return SessionResult{}, err
	}
	s := sessionSpec{id: newID("s-"), parent: in.ParentSessionID, findings: in.Findings, model: in.Model}
	// the branch head, when the branch exists, is where every attempt
	// continues: the push of an earlier attempt, or of a session that died
	// after pushing, which a non-force push must build on
	if s.startRef, err = r.startRef(ctx, t, repo, in.StartRef); err != nil {
		return SessionResult{}, err
	}
	var dead sessionProgress
	if activity.HasHeartbeatDetails(ctx) && activity.GetHeartbeatDetails(ctx, &dead) == nil && dead.SessionID != "" && dead.Running {
		s.parent = dead.SessionID
		s.findings = slices.Concat(in.Findings, dead.Findings, []string{fmt.Sprintf(
			"session %s stopped after %d events without finishing (its worker was lost); continue from the branch as it is", dead.SessionID, dead.Events)})
	}
	hb := startHeartbeat(ctx, s.id)
	defer hb.stop()

	if err := r.completion.reconcile(ctx, t.ID); err != nil {
		return SessionResult{}, fmt.Errorf("revoke leftover credentials: %w", err)
	}
	cleanup := context.WithoutCancel(ctx)
	defer func() {
		r.forgetNamespace(cleanup, t.ID)
		if err := r.completion.reconcile(cleanup, t.ID); err != nil {
			log.Printf("task %s: revoke session %s credentials: %v", t.ID, s.id, err)
		}
	}()
	tok, err := r.forgejo.MintToken(ctx, TokenName(t.ID), []Repo{repo})
	if err != nil {
		return SessionResult{}, err
	}
	// freshNamespace deletes a leftover namespace first, and with it any
	// pod a dead session left running
	if err := r.setupNamespace(ctx, t, tok); err != nil {
		return SessionResult{}, err
	}
	deadline := earliest(activity.GetInfo(ctx).Deadline.Add(-sessionGrace), time.Now().Add(r.cfg.Supervisor.WallClockCap))
	r.record(ctx, t.ID, "session_start", map[string]any{
		"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt, "start_ref": s.startRef, "findings": s.findings})
	out, err := r.runSession(ctx, t, r.agentNamespace(t.ID), s, deadline, hb.update)
	if err != nil {
		return SessionResult{}, err
	}
	res := sessionResult(s, out)
	r.record(ctx, t.ID, "session_end", map[string]any{
		"session": s.id, "parent": s.parent, "model": s.model, "outcome": res.Outcome, "summary": res.Summary})
	if res.Outcome == OutcomePushed {
		r.record(ctx, t.ID, "pushed", map[string]any{"session": s.id, "branch": r.branch(t), "commit": res.Commit})
	}
	return res, nil
}

func (r *Runner) startRef(ctx context.Context, t Task, repo Repo, given string) (string, error) {
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, r.branch(t))
	switch {
	case err == nil:
		return head, nil
	case !errors.Is(err, ErrNotFound):
		return "", fmt.Errorf("branch head: %w", err)
	case given != "":
		return given, nil
	}
	return t.SHA, nil
}

// sessionResult maps a session's end onto the outcome the workflow acts on.
func sessionResult(s sessionSpec, out sessionOutcome) SessionResult {
	res := SessionResult{SessionID: s.id, Outcome: OutcomeNoPush}
	// a session that finished and pushed goes to verification, even when a
	// stop raced its finish
	if out.result != nil && out.result.Success && out.commit != "" {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, out.commit, out.result.Summary
		res.Findings = out.findings
		return res
	}
	if out.trip != nil {
		res.Commit = out.commit
		res.Findings = slices.Clone(out.trip.Findings)
		if out.commit != "" {
			res.Findings = append(res.Findings, "the stopped session's work in progress is on the branch at "+out.commit)
		}
		switch {
		case out.result != nil:
			res.Findings = append(res.Findings, "the stopped worker reported: "+out.result.Summary)
		case out.noResult != "":
			res.Findings = append(res.Findings, "work in progress not pushed: "+out.noResult)
		}
	}
	switch {
	case out.trip != nil && out.trip.Kind == TripWallClock:
		res.Outcome, res.Summary = OutcomeWallClock, "supervision trip: "+string(TripWallClock)
		return res
	case out.trip != nil && out.decision != nil:
		d := out.decision
		res.Summary = fmt.Sprintf("supervisor: %s after %s: %s", d.Action, out.trip.Kind, out.trip.Detail)
		if d.Guidance != "" {
			res.Findings = append(res.Findings, "supervisor guidance: "+d.Guidance)
		}
		switch d.Action {
		case ActionRedirect:
			res.Outcome = OutcomeRedirect
		case ActionRetryStronger:
			res.Outcome = OutcomeRetryStronger
		default:
			res.Outcome = OutcomeEscalate
			res.Reason = fmt.Sprintf("%s\n\nDetector: %s (%s)\nProgress: %s", d.Reason, out.trip.Kind, out.trip.Detail, out.digest)
		}
		return res
	case out.result != nil:
		res.Summary = out.result.Summary
	default:
		res.Summary = out.noResult
	}
	res.Findings = append(slices.Clone(out.findings), "previous session ended without a pushed commit: "+res.Summary)
	return res
}

// Escalate hands the task to a human (architecture 6.6): state
// waiting-human, mirrored on the issue, and a comment with the reason. It
// returns the comment's id: only later comments answer it.
func (r *Runner) Escalate(ctx context.Context, taskID, reason string) (int64, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	if t.Status != "waiting-human" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"running", "in-review"}, "waiting-human"); err != nil {
			return 0, terminalError("task %s is %s, cannot wait for a human: %v", taskID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	}
	id, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, "factoryd: task "+taskID+
		" needs a human.\n\n"+reason+"\n\nReply on this issue to start a new attempt with your comment as guidance, or close the issue to fail the task.")
	if err != nil {
		return 0, err
	}
	r.record(ctx, taskID, "escalated", map[string]any{"reason": reason, "comment": id})
	return id, nil
}

// record appends to the timeline; a failed append is logged, never fails
// the step it describes.
func (r *Runner) record(ctx context.Context, taskID, step string, detail map[string]any) {
	if err := r.timeline.Append(ctx, taskID, step, detail); err != nil {
		log.Printf("task %s: timeline %s: %v", taskID, step, err)
	}
}

// RecordStep appends a workflow-side step (a received signal) to the
// timeline.
func (r *Runner) RecordStep(ctx context.Context, taskID, step string, detail map[string]any) error {
	return r.timeline.Append(ctx, taskID, step, detail)
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// OpenPullRequest opens the task's pull request from its branch into main,
// as the bot, or reuses the one already open for the branch (the
// idempotency key: a retried or repeated call never opens a second). A
// commit whose .forgejo/ differs from the task's base gets no pull request
// (ruling 90): PR workflows run from the PR's own tree, so a task branch
// must never carry an edit to its own gate. If an earlier push already
// opened one, it is closed with the reason.
func (r *Runner) OpenPullRequest(ctx context.Context, taskID, commit string) (PullRequest, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PullRequest{}, err
	}
	branch := r.branch(t)
	existing, err := r.forgejo.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, branch)
	if err != nil {
		return PullRequest{}, err
	}
	if err := r.refuseForgejoChange(ctx, t, repo, commit); err != nil {
		if existing != nil && isTerminal(err) {
			msg := "factoryd: closing this pull request: " + errMessage(err)
			if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, existing.Number, msg); err != nil {
				return PullRequest{}, err
			}
			if err := r.forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, existing.Number); err != nil {
				return PullRequest{}, err
			}
		}
		return PullRequest{}, err
	}
	var ref PullRequestRef
	if existing != nil {
		ref = *existing
	} else {
		body := fmt.Sprintf("%s\n\nCloses #%d\n\nOpened by factoryd for task %s.", t.Objective, t.IssueNumber, t.ID)
		if ref, err = r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, branch, prBase, FirstLine(t.Objective), body); err != nil {
			return PullRequest{}, err
		}
	}
	if err := r.store.SetTaskPR(ctx, taskID, ref.Number); err != nil {
		return PullRequest{}, err
	}
	r.record(ctx, taskID, "pr_opened", map[string]any{"number": ref.Number, "url": ref.URL, "commit": commit, "reused": existing != nil})
	return PullRequest{Number: ref.Number, URL: ref.URL}, nil
}

// refuseForgejoChange returns a terminal error when commit's .forgejo tree
// entry differs from the task base's.
func (r *Runner) refuseForgejoChange(ctx context.Context, t Task, repo Repo, commit string) error {
	base := t.SHA
	if base == "" {
		head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
		if err != nil {
			return err
		}
		base = head
	}
	was, err := r.forgejo.TreeEntry(ctx, repo.Owner, repo.Name, base, ".forgejo")
	if err != nil {
		return err
	}
	now, err := r.forgejo.TreeEntry(ctx, repo.Owner, repo.Name, commit, ".forgejo")
	if err != nil {
		return err
	}
	if was != now {
		return terminalError("branch %s changes .forgejo/ (commit %s against base %s); factoryd opens no pull request that edits .forgejo/", r.branch(t), commit, base)
	}
	return nil
}

// Reconcile reads where a task stands before a run acts, so that a run
// started by the heal pass or by a signal continues the task instead of
// redoing it: the row's status, the task's pull request (the recorded one,
// else the branch's open one) and whether it merged or closed, and whether
// the issue closed.
func (r *Runner) Reconcile(ctx context.Context, taskID string) (RunState, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return RunState{}, err
	}
	st := RunState{Status: t.Status}
	if t.Status == "done" || t.Status == "failed" || t.Kind == "human" {
		st.Skip = true
		return st, nil
	}
	var pr *PullRequestRef
	if t.PRNumber != 0 {
		got, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if err != nil {
			return RunState{}, err
		}
		pr = &got
	} else if pr, err = r.forgejo.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, r.branch(t)); err != nil {
		return RunState{}, err
	}
	switch {
	case pr == nil:
	case pr.Merged:
		st.Merged = true
	case pr.State == "closed":
		st.PRClosed = true
	default:
		st.PR, st.Head = PullRequest{Number: pr.Number, URL: pr.URL}, pr.HeadSHA
	}
	if t.IssueNumber != 0 {
		if st.IssueClosed, err = r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil {
			return RunState{}, err
		}
	}
	r.record(ctx, taskID, "reconcile", map[string]any{"status": st.Status, "pr": st.PR.Number, "head": st.Head,
		"merged": st.Merged, "pr_closed": st.PRClosed, "issue_closed": st.IssueClosed, "skip": st.Skip})
	return st, nil
}

// FirstLine is an objective's first line: the issue and pull request title.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// WaitChecks polls the combined commit status of sha (Forgejo Actions is the
// first verification gate, architecture 7) until it settles or the checks
// timeout passes. Forgejo combines worst-state-wins, so a failed job
// settles the verdict while slower jobs still run.
func (r *Runner) WaitChecks(ctx context.Context, taskID, sha string) (ChecksResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChecksResult{}, err
	}
	deadline := earliest(activity.GetInfo(ctx).Deadline.Add(-time.Minute), time.Now().Add(r.cfg.ChecksTimeout))
	for {
		cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
		if err != nil {
			return ChecksResult{}, err
		}
		activity.RecordHeartbeat(ctx, cs.State)
		res := checksResult(cs, r.forgejo.PublicURL)
		if res.State == "" && !time.Now().Before(deadline) {
			res.State = ChecksTimeout
		}
		if res.State != "" {
			r.record(ctx, t.ID, "checks", map[string]any{"commit": sha, "state": res.State, "findings": res.Findings})
			return res, nil
		}
		select {
		case <-ctx.Done():
			return ChecksResult{}, ctx.Err()
		case <-time.After(min(r.cfg.ChecksPoll, time.Until(deadline))):
		}
	}
}

// checksResult maps a combined status onto a verdict; empty means still
// pending (no status reported yet counts as pending, not as success).
func checksResult(cs CombinedStatus, baseURL string) ChecksResult {
	if len(cs.Statuses) == 0 {
		return ChecksResult{}
	}
	switch cs.State {
	case "success", "warning":
		return ChecksResult{State: ChecksSuccess}
	case "failure", "error":
		res := ChecksResult{State: ChecksFailure}
		for _, s := range cs.Statuses {
			if s.State != "failure" && s.State != "error" {
				continue
			}
			target := s.TargetURL
			if strings.HasPrefix(target, "/") {
				target = strings.TrimSuffix(baseURL, "/") + target
			}
			res.Findings = append(res.Findings, fmt.Sprintf("check %q: %s — %s (%s)", s.Context, s.State, s.Description, target))
		}
		return res
	}
	return ChecksResult{}
}

// PullRequestMerged tells a merge (which closes the issue through "Closes
// #N") from a plain close of the issue.
func (r *Runner) PullRequestMerged(ctx context.Context, taskID string, number int64) (bool, error) {
	_, repo, err := r.task(ctx, taskID)
	if err != nil {
		return false, err
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, number)
	return pr.Merged, err
}

// Finish is the terminal step: completion (credentials revoked, keys
// blocked, final status, issue label), and for a failure the reason as an
// issue comment. Finishing an already-finished task is a no-op, so a
// retried Finish never double-completes.
func (r *Runner) Finish(ctx context.Context, taskID, final, reason string) error {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok || t.Status == final || (t.Status == "done" || t.Status == "failed") {
		return nil // nothing to finish: gone, or already final
	}
	r.forgetNamespace(ctx, taskID)
	if err := r.completion.Complete(ctx, taskID, final); err != nil {
		return err
	}
	// a malformed repository still completes the task, only uncommented
	if repo, err := SplitRepo(t.Repository); err == nil && final == "failed" && t.IssueNumber != 0 {
		if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, "factoryd: task "+taskID+" failed: "+reason); err != nil {
			log.Printf("task %s: comment failure reason: %v", taskID, err)
		}
	}
	r.record(ctx, taskID, "final", map[string]any{"state": final, "reason": reason})
	return nil
}
