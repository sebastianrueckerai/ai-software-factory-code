package factoryd

import (
	"context"
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
	var err error
	switch state {
	case "running":
		err = r.store.MarkStarted(ctx, taskID)
	case "in-review":
		err = r.store.TransitionTask(ctx, taskID, []string{"running", "in-review"}, "in-review")
	default:
		return terminalError("unknown task state %q", state)
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
	Events    int
	Findings  []string
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
	h.p.Events, h.p.Findings = events, findings
}

func (h *heartbeat) snapshot() sessionProgress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sessionProgress{SessionID: h.p.SessionID, Events: h.p.Events, Findings: slices.Clone(h.p.Findings)}
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
	s := sessionSpec{id: newID("s-"), parent: in.ParentSessionID, findings: in.Findings, model: in.Model, startRef: in.StartRef}
	if s.startRef == "" {
		s.startRef = t.SHA
	}
	var dead sessionProgress
	if activity.HasHeartbeatDetails(ctx) && activity.GetHeartbeatDetails(ctx, &dead) == nil && dead.SessionID != "" {
		s.parent = dead.SessionID
		s.findings = slices.Concat(in.Findings, dead.Findings, []string{fmt.Sprintf(
			"session %s was interrupted after %d events when factoryd restarted; its pushed work, if any, is on the branch", dead.SessionID, dead.Events)})
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
		"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt, "start_ref": s.startRef})
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

// sessionResult maps a session's end onto the outcome the workflow acts on.
func sessionResult(s sessionSpec, out sessionOutcome) SessionResult {
	res := SessionResult{SessionID: s.id, Outcome: OutcomeNoPush}
	switch {
	case out.trip != nil:
		res.Summary = "supervision trip: " + string(out.trip.Kind)
		res.Findings = out.trip.Findings
		switch {
		case out.trip.Kind == TripWallClock:
			res.Outcome = OutcomeWallClock
		case out.trip.RetryStrongerModel:
			res.Outcome = OutcomeRetryStronger
		}
		return res
	case out.result != nil && out.result.Success && out.commit != "":
		res.Outcome, res.Commit, res.Summary = OutcomePushed, out.commit, out.result.Summary
		res.Findings = out.findings
		return res
	case out.result != nil:
		res.Summary = out.result.Summary
	default:
		res.Summary = out.noResult
	}
	res.Findings = append(slices.Clone(out.findings), "previous session ended without a pushed commit: "+res.Summary)
	return res
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
// branch whose diff touches .forgejo/ gets no pull request (ruling 90): PR
// workflows run from the PR's own tree, so a task branch must never carry
// an edit to its own gate.
func (r *Runner) OpenPullRequest(ctx context.Context, taskID, commit string) (PullRequest, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PullRequest{}, err
	}
	branch := r.branch(t)
	files, err := r.forgejo.CompareBranches(ctx, repo.Owner, repo.Name, prBase, commit)
	if err != nil {
		return PullRequest{}, fmt.Errorf("compare %s...%s: %w", prBase, commit, err)
	}
	for _, f := range files {
		if f == ".forgejo" || strings.HasPrefix(f, ".forgejo/") {
			return PullRequest{}, terminalError("branch %s changes %s; factoryd opens no pull request that edits .forgejo/", branch, f)
		}
	}
	existing, err := r.forgejo.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, branch)
	if err != nil {
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
	r.record(ctx, taskID, "pr_opened", map[string]any{"number": ref.Number, "url": ref.URL, "commit": commit, "reused": existing != nil})
	return PullRequest{Number: ref.Number, URL: ref.URL}, nil
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
		res := checksResult(cs, r.forgejo.BaseURL)
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
	return r.forgejo.PullRequestMerged(ctx, repo.Owner, repo.Name, number)
}

// Finish is the terminal step: completion (credentials revoked, keys
// blocked, final status, issue label), and for a failure the reason as an
// issue comment. Finishing an already-finished task is a no-op, so a
// retried Finish never double-completes.
func (r *Runner) Finish(ctx context.Context, taskID, final, reason string) error {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status == final {
		return nil
	}
	r.forgetNamespace(ctx, taskID)
	if err := r.completion.Complete(ctx, taskID, final); err != nil {
		return err
	}
	if final == "failed" && t.IssueNumber != 0 {
		if err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, "factoryd: task "+taskID+" failed: "+reason); err != nil {
			log.Printf("task %s: comment failure reason: %v", taskID, err)
		}
	}
	r.record(ctx, taskID, "final", map[string]any{"state": final, "reason": reason})
	return nil
}
