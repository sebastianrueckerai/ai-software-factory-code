package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The verification plane after green checks (architecture 7, ruling 109):
// the policy gate over the changed paths, the migration flag, the task's
// reproduction command, the model review (evidence, never authority), then
// a human's approval and the merge as factory-merge.

// PolicyGate is GatePolicy's verdict on a pull request.
type PolicyGate struct {
	Class         string
	Outcome       string // policy.OutcomeForbidden or policy.OutcomeRequiresApproval
	Paths         []string
	Migrations    []string
	Changed       int
	PolicyVersion int64
}

// ReproResult is GateReproduction's verdict: Ran false when the task
// carries no reproduction command.
type ReproResult struct {
	Ran     bool
	Command string
	Exit    int
	Output  string
}

func (r ReproResult) Passed() bool { return !r.Ran || r.Exit == 0 }

// GateResults are the gates' verdicts, for the model review and the card.
type GateResults struct {
	Policy       PolicyGate
	Reproduction ReproResult
}

// Review is the model review's verdict; Note says why there is none (no
// headroom, a failed call, an invalid answer).
type Review struct {
	Verdict  string   `json:"verdict"`
	Summary  string   `json:"summary"`
	Findings []string `json:"findings"`
	Note     string   `json:"note,omitempty"`
}

// MergeSpec is a merge approval request's spec: what the human reads on
// the card (ruling 116).
type MergeSpec struct {
	RiskClass      string      `json:"risk_class"`
	Migration      bool        `json:"migration"`
	Migrations     []string    `json:"migrations"`
	VerdictSummary string      `json:"verdict_summary"`
	Review         Review      `json:"review"`
	Reproduction   ReproResult `json:"reproduction"`
	HeadSHA        string      `json:"head_sha"`
	PRURL          string      `json:"pr_url"`
	PolicyVersion  int64       `json:"policy_version"`
}

// ApprovalInput is RequestApproval's argument.
type ApprovalInput struct {
	PR     PullRequest
	Commit string
	Gates  GateResults
	Review Review
}

// MergeResult is how an approved merge went: By names who merged when it
// was not factory-merge (a human in Forgejo); Refusal is why it was
// refused; Failed why it could not be tried (the approval stands).
type MergeResult struct {
	Merged  bool
	By      string
	Refusal string
	Failed  string
}

// MergeUser is the dedicated Forgejo user the Merge activity merges as
// (ruling 109).
const MergeUser = "factory-merge"

// maxListed bounds the paths one gate comment lists.
const maxListed = 50

func listPaths(paths []string) string {
	var b strings.Builder
	for i, p := range paths {
		if i == maxListed {
			fmt.Fprintf(&b, "- … and %d more\n", len(paths)-maxListed)
			break
		}
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	return b.String()
}

// headIs refuses, terminally, a pull request whose head is not the gated
// commit: the gates would judge another tree than the one checked.
func (r *Runner) headIs(ctx context.Context, repo Repo, pr int64, commit string) error {
	st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return err
	}
	if st.HeadSHA != commit {
		return terminalError("pull request #%d's head moved to %s after its checks passed on %s", pr, st.HeadSHA, commit)
	}
	return nil
}

// GatePolicy classifies the pull request's changed paths (a rename counts
// both names) under the policy version in force, records the class on the
// task and comments it on the pull request. A forbidden class is returned,
// not an error: the workflow fails the task with the reason.
func (r *Runner) GatePolicy(ctx context.Context, taskID string, pr int64, commit string) (PolicyGate, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PolicyGate{}, err
	}
	// a workflow from before versionGatesPinned passes no commit
	if commit != "" {
		if err := r.headIs(ctx, repo, pr, commit); err != nil {
			return PolicyGate{}, err
		}
	}
	paths, err := r.forgejo.PullRequestFiles(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return PolicyGate{}, err
	}
	v := r.cfg.Policy()
	c := v.Document.Risk.Classify(paths)
	g := PolicyGate{Class: c.Class.Name, Outcome: c.Class.Outcome, Paths: c.Paths, Migrations: c.Migrations,
		Changed: len(paths), PolicyVersion: v.ID}
	if err := r.store.SetTaskRisk(ctx, t.ID, g.Class); err != nil {
		return PolicyGate{}, err
	}
	msg := fmt.Sprintf("factoryd policy gate (policy version %d): risk class **%s**, %s, over %d changed paths.\n\n", v.ID, g.Class, g.Outcome, g.Changed)
	if g.Outcome == policy.OutcomeForbidden {
		msg += "Agents may not change these paths:\n" + listPaths(g.Paths)
	} else {
		msg += "A human approves the merge."
	}
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr, msg); err != nil {
		return PolicyGate{}, err
	}
	r.record(ctx, t.ID, "gate_policy", map[string]any{"pr": pr, "class": g.Class, "outcome": g.Outcome,
		"paths": g.Paths, "migrations": g.Migrations, "changed": g.Changed, "policy_version": v.ID})
	return g, nil
}

// GateMigration flags the pull request's schema migrations for the human's
// review, on the pull request (the approval card lists them too).
func (r *Runner) GateMigration(ctx context.Context, taskID string, pr int64, migrations []string) error {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	msg := "factoryd: **schema migration: human review required**. This pull request changes:\n" + listPaths(migrations)
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr, msg); err != nil {
		return err
	}
	r.record(ctx, t.ID, "gate_migration", map[string]any{"pr": pr, "migrations": migrations})
	return nil
}

// reproOutputTail bounds the reproduction output kept (findings, comment).
const reproOutputTail = 4 << 10

// GateReproduction runs the task's reproduction command against the pull
// request head in a fresh sandbox with no agent: the worker image in its
// repro mode fetches the head with a read token on the task's repository
// and runs the command under the agents' egress policy. No command: not
// run. The namespace and the token go when it ends, however it ends.
func (r *Runner) GateReproduction(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ReproResult{}, err
	}
	if t.ReproCommand == "" {
		r.record(ctx, t.ID, "gate_reproduction", map[string]any{"pr": pr, "commit": commit, "skipped": "no reproduction command"})
		return ReproResult{}, nil
	}
	if err := r.completion.reconcile(ctx, t.ID); err != nil {
		return ReproResult{}, fmt.Errorf("revoke leftover credentials: %w", err)
	}
	cleanup := context.WithoutCancel(ctx)
	defer func() {
		r.forgetNamespace(cleanup, t.ID)
		if err := r.completion.reconcile(cleanup, t.ID); err != nil {
			log.Printf("task %s: revoke the reproduction's credentials: %v", t.ID, err)
		}
	}()
	base, err := r.forgejo.MintToken(ctx, BaseTokenName(t.ID), "read:repository", []Repo{repo})
	if err != nil {
		return ReproResult{}, err
	}
	if err := r.setupNamespace(ctx, t, base, ForgejoToken{}); err != nil {
		return ReproResult{}, err
	}
	ns, podName := r.agentNamespace(t.ID), "repro-"+strings.TrimPrefix(newID("s-"), "s-")
	if err := r.kube.CreatePod(ctx, ns, podName, r.reproPodSpec(t, repo, pr, commit)); err != nil {
		return ReproResult{}, fmt.Errorf("create pod: %w", err)
	}
	activity.RecordHeartbeat(ctx, podName)
	if err := r.awaitPodStart(ctx, ns, podName, 10*time.Minute, nil); err != nil {
		return ReproResult{}, err
	}
	rep, err := r.readRepro(ctx, ns, podName, t.ID)
	if err != nil {
		return ReproResult{}, err
	}
	res := ReproResult{Ran: true, Command: t.ReproCommand, Exit: rep.Exit, Output: tail(rep.Output, reproOutputTail)}
	verdict := "passed"
	if !res.Passed() {
		verdict = "**failed**"
	}
	msg := fmt.Sprintf("factoryd reproduction gate %s (exit %d) on %s:\n\n%s\n\nOutput tail:\n\n%s", verdict, res.Exit, commit, fenced(res.Command), fenced(res.Output))
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr, msg); err != nil {
		log.Printf("task %s: comment the reproduction: %v", t.ID, err)
	}
	r.record(ctx, t.ID, "gate_reproduction", map[string]any{"pr": pr, "commit": commit, "command": res.Command, "exit": res.Exit, "output": res.Output})
	return res, nil
}

// readRepro follows the repro pod's log to its report, heartbeating.
func (r *Runner) readRepro(ctx context.Context, ns, podName, taskID string) (harness.Repro, error) {
	logCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := followPodLog(logCtx, r.kube, ns, podName, taskID)
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return harness.Repro{}, ctx.Err()
		case <-beat.C:
			activity.RecordHeartbeat(ctx, podName)
		case line, ok := <-lines.lines:
			if !ok {
				if err := lines.err(); err != nil {
					return harness.Repro{}, err
				}
				return harness.Repro{}, errors.New("the reproduction pod exited without a report")
			}
			var env harness.WorkerEnvelope
			if json.Unmarshal(line, &env) != nil || env.Repro == nil {
				log.Printf("task %s: repro: %s", taskID, line)
				continue
			}
			return *env.Repro, nil
		}
	}
}

// fenced puts s in a markdown code fence longer than any backtick run in
// it, so the text cannot close the fence early.
func fenced(s string) string {
	run, longest := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "\n" + strings.TrimRight(s, "\n") + "\n" + f
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// reproPodSpec is the worker pod in repro mode: no gateway token, no push
// token, the same labels (so the sandbox egress policy selects it), image
// and confinement.
func (r *Runner) reproPodSpec(t Task, repo Repo, pr int64, commit string) map[string]any {
	return r.sandboxPodSpec(t, []map[string]any{
		{"name": "MODE", "value": "repro"},
		{"name": "TASK_ID", "value": t.ID},
		{"name": "REPO_URL", "value": r.repoURL(repo)},
		{"name": "REPO_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": "task-git", "key": "REPO_TOKEN"}}},
		{"name": "PR_REF", "value": fmt.Sprintf("refs/pull/%d/head", pr)},
		{"name": "REPO_SHA", "value": commit},
		{"name": "REPRO_COMMAND", "value": t.ReproCommand},
		{"name": "REPRO_TIMEOUT", "value": strconv.Itoa(int(r.cfg.ReproTimeout.Seconds()))},
	}, false)
}

// reviewDiffMax caps the diff the model review reads.
const reviewDiffMax = 48 << 10

const reviewPrompt = `You review a pull request an autonomous coding agent opened for a task. Read the task brief, the pull request's diff (it may be cut) and the results of the factory's gates (the risk class of the changed paths under the factory's policy, schema migrations, the task's reproduction command). A human decides whether to merge; your review is evidence for that human, it never merges and never blocks. Judge whether the change does what the brief asks, correctly, without unrelated or unsafe changes (credentials, weakened checks, disabled tests, network or permission changes the brief does not ask for).
Answer with one JSON object and nothing else: {"verdict": "approve|concerns", "summary": "...", "findings": ["..."]}. verdict approve when you would merge it as it is, concerns otherwise. summary: under 500 characters. findings: each one concrete issue naming the file, at most 10; empty for approve without remarks.`

type reviewInput struct {
	Brief  harness.TaskBrief `json:"brief"`
	PR     int64             `json:"pull_request"`
	Commit string            `json:"head"`
	Diff   string            `json:"diff"`
	Gates  GateResults       `json:"gates"`
}

const (
	maxReviewSummary  = 1000
	maxReviewFindings = 10
	maxReviewFinding  = 1000
)

// parseReview validates the model's verdict: one JSON object, a known
// verdict, a summary, no other field; a markdown fence is tolerated. An
// invalid answer is a note, never a failure.
func parseReview(content string) Review {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) >= 6 {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s, "```")[3:], "json"))
	}
	invalid := func(why string) Review {
		return Review{Note: "model review answer invalid (" + why + "): " + truncate(content, 300)}
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var rv Review
	if err := dec.Decode(&rv); err != nil {
		return invalid(err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("text after the JSON object")
	}
	if rv.Verdict != "approve" && rv.Verdict != "concerns" {
		return invalid(fmt.Sprintf("unknown verdict %q", rv.Verdict))
	}
	if rv.Note != "" {
		return invalid("note is not an answer field")
	}
	if rv.Summary = truncate(strings.TrimSpace(rv.Summary), maxReviewSummary); rv.Summary == "" {
		return invalid("no summary")
	}
	var findings []string
	for _, f := range rv.Findings {
		if f = strings.TrimSpace(f); f != "" && len(findings) < maxReviewFindings {
			findings = append(findings, truncate(f, maxReviewFinding))
		}
	}
	rv.Findings = findings
	return rv
}

// ModelReview asks the review model for a verdict over the brief, the
// diff and the gate results, on the task's account within its headroom,
// and posts it on the pull request. It is evidence only: no answer (no
// headroom, a failed call, an invalid answer) is a note, and nothing it
// says skips a gate or merges.
func (r *Runner) ModelReview(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults) (Review, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return Review{}, err
	}
	if err := r.headIs(ctx, repo, pr.Number, commit); err != nil {
		if !isTerminal(err) {
			return Review{}, err
		}
		rv := Review{Note: "model review not run: " + errMessage(err)}
		r.comment(ctx, t, repo, pr.Number, reviewComment(r.cfg.ReviewModel, commit, rv))
		r.record(ctx, t.ID, "model_review", map[string]any{"pr": pr.Number, "commit": commit, "note": rv.Note})
		return rv, nil
	}
	diff, err := r.forgejo.PullRequestDiff(ctx, repo.Owner, repo.Name, pr.Number, reviewDiffMax)
	if err != nil {
		return Review{}, err
	}
	// the heartbeats carry the workflow's cancel (a terminal signal during
	// the review) into the call: it is cut, and its hold booked
	beatCtx, stopBeats := context.WithCancel(ctx)
	go func() {
		tick := time.NewTicker(reviewHeartbeatEvery)
		defer tick.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case <-tick.C:
				recordReviewHeartbeat(ctx, "review")
			}
		}
	}()
	content, err := r.supervisor.Chat(ctx, t.ID, r.cfg.ReviewModel, "review", reviewPrompt, reviewInput{
		Brief: harness.TaskBrief{Objective: t.Objective, Repository: t.Repository, SHA: t.SHA},
		PR:    pr.Number, Commit: commit, Diff: diff, Gates: gates,
	})
	stopBeats()
	if ctx.Err() != nil {
		return Review{}, ctx.Err()
	}
	var rv Review
	var refusal *policy.Refusal
	switch {
	case errors.As(err, &refusal):
		rv.Note = "model review skipped: the task's headroom does not pay for it (" + refusal.Reason + ")"
	case err != nil:
		rv.Note = "model review failed: " + truncate(err.Error(), 500)
	default:
		rv = parseReview(content)
	}
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, reviewComment(r.cfg.ReviewModel, commit, rv)); err != nil {
		log.Printf("task %s: comment the model review: %v", t.ID, err)
	}
	r.record(ctx, t.ID, "model_review", map[string]any{"pr": pr.Number, "commit": commit, "model": r.cfg.ReviewModel,
		"verdict": rv.Verdict, "summary": rv.Summary, "findings": rv.Findings, "note": rv.Note})
	return rv, nil
}

func reviewComment(model, commit string, rv Review) string {
	head := fmt.Sprintf("factoryd model review (%s) of %s. Evidence for the human who approves the merge, not a decision.\n\n", model, commit)
	if rv.Verdict == "" {
		return head + rv.Note
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sVerdict: **%s**. %s\n", head, rv.Verdict, rv.Summary)
	for _, f := range rv.Findings {
		fmt.Fprintf(&b, "\n- %s", f)
	}
	return b.String()
}

// RequestApproval opens the task's merge approval request, the console's
// card: the pull request, its risk class, migrations, reproduction and the
// model review. It replaces any earlier pending one of the task.
func (r *Runner) RequestApproval(ctx context.Context, taskID string, in ApprovalInput) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	g := in.Gates.Policy
	spec, err := json.Marshal(MergeSpec{
		RiskClass: g.Class, Migration: len(g.Migrations) > 0, Migrations: g.Migrations,
		VerdictSummary: cmp.Or(in.Review.Summary, in.Review.Note), Review: in.Review, Reproduction: in.Gates.Reproduction,
		HeadSHA: in.Commit, PRURL: in.PR.URL, PolicyVersion: g.PolicyVersion,
	})
	if err != nil {
		return err
	}
	id, err := r.store.OpenApproval(ctx, ApprovalRequest{
		Kind: "merge", TaskID: t.ID, Project: t.Repository, PRNumber: in.PR.Number, Spec: spec,
		Reason: fmt.Sprintf("pull request #%d at %s passed its gates (risk class %s); a human approves the merge", in.PR.Number, in.Commit, g.Class),
	})
	if err != nil {
		return err
	}
	r.record(ctx, t.ID, "approval_requested", map[string]any{"approval": id, "pr": in.PR.Number, "commit": in.Commit, "class": in.Gates.Policy.Class})
	return nil
}

// Merge merges the approved pull request as factory-merge, at the head the
// gates passed, into main only. Forgejo refuses it unless a human on main's
// approvals whitelist approved that head; a refusal is commented and
// returned, not an error: the workflow keeps waiting. A pull request found
// merged (a retry after a merge whose answer was lost, or a human's merge)
// is a merge, credited to whoever merged it. Any other failure is retried;
// the last attempt reports it (timeline, comment) as Failed instead, so the
// workflow keeps the approval and tries again later.
func (r *Runner) Merge(ctx context.Context, taskID string, pr int64, commit, approver string) (MergeResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return MergeResult{}, err
	}
	return r.mergeAttempt(ctx, t, repo, pr, commit, approver, lastAttempt(ctx))
}

// mergeAttempt is one Merge attempt; last reports a transient failure
// instead of returning it.
func (r *Runner) mergeAttempt(ctx context.Context, t Task, repo Repo, pr int64, commit, approver string, last bool) (MergeResult, error) {
	res, err := r.merge(ctx, t, repo, pr, commit, approver)
	if err != nil && !isTerminal(err) && !forgejoTerminal(err) && last {
		res = MergeResult{Failed: truncate(err.Error(), 500)}
		r.reportMergeFailure(ctx, t, repo, pr, commit, approver, res.Failed)
		return res, nil
	}
	return res, err
}

// ReportMergeFailure tells a merge that failed without Merge reporting it
// (a 404, a timeout of its last attempt): a PR comment and a merge_failed
// step, as Merge's own last attempt does; the workflow tries again later.
func (r *Runner) ReportMergeFailure(ctx context.Context, taskID string, pr int64, commit, approver, reason string) error {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	r.reportMergeFailure(ctx, t, repo, pr, commit, approver, truncate(reason, 500))
	return nil
}

func (r *Runner) reportMergeFailure(ctx context.Context, t Task, repo Repo, pr int64, commit, approver, reason string) {
	r.comment(ctx, t, repo, pr, fmt.Sprintf("factoryd: the merge of %s after %s's approval failed: %s\n\nThe approval stands; factoryd tries again in %s.",
		commit, approver, reason, mergeRetryAfter))
	r.record(ctx, t.ID, "merge_failed", map[string]any{"pr": pr, "commit": commit, "approver": approver, "error": reason})
}

// lastAttempt reports whether this activity attempt is the last its retry
// policy allows.
func lastAttempt(ctx context.Context) bool {
	info := activity.GetInfo(ctx)
	return info.RetryPolicy != nil && info.RetryPolicy.MaximumAttempts > 0 && info.Attempt >= info.RetryPolicy.MaximumAttempts
}

func (r *Runner) merge(ctx context.Context, t Task, repo Repo, pr int64, commit, approver string) (MergeResult, error) {
	st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return MergeResult{}, err
	}
	if st.Merged {
		return r.mergedAlready(ctx, t, pr, commit, approver, st), nil
	}
	if st.Base != prBase {
		return r.refuseMerge(ctx, t, repo, pr, commit, approver, fmt.Sprintf("the pull request merges into %q, not %s", st.Base, prBase)), nil
	}
	msg := fmt.Sprintf("factoryd task %s (issue #%d): approved by %s, merged by %s", t.ID, t.IssueNumber, approver, MergeUser)
	err = r.forgejo.MergePullRequest(ctx, repo.Owner, repo.Name, pr, commit, msg)
	if errors.Is(err, ErrRefused) {
		if st, serr := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr); serr == nil && st.Merged {
			return r.mergedAlready(ctx, t, pr, commit, approver, st), nil
		}
		return r.refuseMerge(ctx, t, repo, pr, commit, approver, err.Error()), nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	if err := r.store.ResolveApprovals(ctx, t.ID, "merge", "approved", approver, "merged by "+MergeUser+" at "+commit); err != nil {
		log.Printf("task %s: resolve the approval request: %v", t.ID, err)
	}
	r.record(ctx, t.ID, "merged", map[string]any{"pr": pr, "commit": commit, "approver": approver, "merged_by": MergeUser})
	return MergeResult{Merged: true}, nil
}

// mergedAlready credits a pull request found merged to whoever merged it:
// factory-merge (an earlier attempt whose answer was lost) on approver's
// approval, or a human, at whatever head they merged.
func (r *Runner) mergedAlready(ctx context.Context, t Task, pr int64, commit, approver string, st PullRequestRef) MergeResult {
	if st.MergedBy == MergeUser {
		if err := r.store.ResolveApprovals(ctx, t.ID, "merge", "approved", approver, "merged by "+MergeUser+" at "+commit); err != nil {
			log.Printf("task %s: resolve the approval request: %v", t.ID, err)
		}
		r.record(ctx, t.ID, "merged", map[string]any{"pr": pr, "commit": commit, "approver": approver, "merged_by": MergeUser})
		return MergeResult{Merged: true}
	}
	by := cmp.Or(st.MergedBy, "an unknown user")
	note := fmt.Sprintf("merged in Forgejo by %s at %s", by, st.HeadSHA)
	if st.HeadSHA != commit {
		note += ", not the gated " + commit
	}
	if err := r.store.ResolveApprovals(ctx, t.ID, "merge", "approved", by, note); err != nil {
		log.Printf("task %s: resolve the approval request: %v", t.ID, err)
	}
	r.record(ctx, t.ID, "merged", map[string]any{"pr": pr, "commit": st.HeadSHA, "gated": commit, "merged_by": by})
	return MergeResult{Merged: true, By: by}
}

func (r *Runner) refuseMerge(ctx context.Context, t Task, repo Repo, pr int64, commit, approver, why string) MergeResult {
	res := MergeResult{Refusal: truncate(why, 500)}
	r.comment(ctx, t, repo, pr, fmt.Sprintf(
		"factoryd: the merge of %s after %s's approval signal was refused: %s\n\nThe task keeps waiting: a human on main's approvals whitelist approves this head (the console's approve), or merges it here.",
		commit, approver, res.Refusal))
	r.record(ctx, t.ID, "merge_refused", map[string]any{"pr": pr, "commit": commit, "approver": approver, "refusal": res.Refusal})
	return res
}

// comment posts on the pull request; a failure is logged, the comment is
// evidence, not a step.
func (r *Runner) comment(ctx context.Context, t Task, repo Repo, pr int64, body string) {
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr, body); err != nil {
		log.Printf("task %s: comment on #%d: %v", t.ID, pr, err)
	}
}

// ApproveMerge is the console's approve, after it submitted the human's
// APPROVED review on the pull request through the human's own token: it
// signals the task's workflow, which merges as factory-merge. Only a
// pending merge request of the task, for that pull request, is approved.
func ApproveMerge(ctx context.Context, store Store, wf Workflows, taskID string, pr int64, approver string) error {
	pending, err := store.ApprovalRequests(ctx, "pending")
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(pending, func(a ApprovalRequest) bool {
		return a.Kind == "merge" && a.TaskID == taskID && a.PRNumber == pr
	}) {
		return fmt.Errorf("task %s has no pending merge approval for pull request #%d", taskID, pr)
	}
	return wf.Signal(ctx, taskID, SignalApproved, TaskSignal{Author: approver, PR: pr})
}

// reviewHeartbeatEvery is how often ModelReview heartbeats during its
// model call; variables for the tests.
var (
	reviewHeartbeatEvery  = heartbeatEvery
	recordReviewHeartbeat = activity.RecordHeartbeat
)
