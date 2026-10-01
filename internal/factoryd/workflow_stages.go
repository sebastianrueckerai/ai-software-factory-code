package factoryd

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The screening verdicts (ruling 182).
const (
	ScreenClear   = "clear"
	ScreenSuspect = "suspect"
)

// ScreenResult is the screening stage's verdict on the issue; Report is the
// analyzer task that read it.
type ScreenResult struct {
	Verdict string
	Reasons []string
	Report  string
}

// ReportResult is a report session's end: the issue comment it posted.
type ReportResult struct {
	Comment int64
}

// screenedLine is all the issue of a suspect screening shows: its reasons
// would teach the author what to change (review M3).
const screenedLine = "factoryd: screened: a human looks at this report first."

// screeningAsks opens the escalation of a suspect screening verdict.
const screeningAsks = "The screening finds this issue suspect: "

// noVerdict heads the card finding of a review that gave no verdict.
const noVerdict = "the review gave no verdict: "

// capReached heads the card of a task at its session cap.
const capReached = "the session cap is reached: every finding sent to the agent so far goes to a human"

// The session cap's card (versionSessionCap) holds at most maxCard bytes of
// findings, the newest, each at most maxCardLine.
const (
	maxCard     = 8000
	maxCardLine = maxReviewFinding
)

// stages takes a pushed commit, its pull request open, through the pinned
// workflow's stages after the session (versionStages): each stage's
// findings go where its on_findings says; approval and merge end it.
func (t *taskRun) stages(commit string) verdict {
	t.round++
	if t.returns == nil {
		t.returns = map[string]int{}
	}
	g := GateResults{Pin: t.cfg.PolicyVersion}
	var review Review
	reviewed := false
	var capped []string // why findings meant for the agent go to the card
	var card []string
	for _, s := range t.cfg.Stages {
		switch s.Name {
		case policy.StageScreening:
			continue
		case policy.StageAudit:
			if t.v19 < versionAudit {
				continue
			}
		case policy.StageApproval:
			if !t.queued() {
				return t.approval(commit, g, review, reviewed, capped, card)
			}
			findings, v, stop := t.queueTurn(&commit, &g, &review)
			if !stop && len(findings) > 0 {
				i := slices.IndexFunc(t.cfg.Stages, func(s policy.Stage) bool { return s.Name == policy.StageChecks })
				v, stop = t.onFindings(t.cfg.Stages[i], findings, &capped, &card)
			}
			if !stop {
				v = t.approval(commit, g, review, reviewed, capped, card)
			}
			t.leaveQueue(cmp.Or(v.reason, v.final))
			return v
		}
		if why := t.skipReason(s); why != "" {
			t.recordSkip(s.Name, why)
			continue
		}
		findings, v, stop := t.runStage(s.Name, &commit, &g, &review)
		if stop {
			return v
		}
		reviewed = reviewed || s.Name == policy.StageReview
		if s.Name == policy.StageReview && review.Verdict == "" && !t.rolling && t.v20 >= versionReviewVerdict {
			card = append(card, s.Name+": "+noVerdict+cmp.Or(review.Note, "no reason recorded"))
			continue
		}
		if len(findings) == 0 {
			continue
		}
		if v, stop := t.onFindings(s, findings, &capped, &card); stop {
			return v
		}
	}
	return verdict{final: "failed", reason: "the workflow has no approval stage"}
}

// approval takes the head through the approval stage: the card holds the
// findings capped for a human (with the history) and every other card
// finding; reviewed: the last review ran in this pass.
func (t *taskRun) approval(commit string, g GateResults, review Review, reviewed bool, capped, card []string) verdict {
	earlier := t.reviews
	if reviewed {
		earlier = t.reviews[:len(t.reviews)-1]
	}
	if len(capped) > 0 {
		card = append(append(capped, t.history...), card...)
	}
	return t.approve(commit, g, review, slices.Clone(earlier), card)
}

// onFindings sends stage s's findings where its on_findings says: stop
// with the task's verdict (fail, or back to the agent), or onto the card,
// capped when the agent may take no more.
func (t *taskRun) onFindings(s policy.Stage, findings []string, capped, card *[]string) (verdict, bool) {
	switch s.OnFindings {
	case policy.FindingsFail:
		return verdict{final: "failed", reason: s.Name + ": " + strings.Join(findings, "; ")}, true
	case policy.FindingsAgent:
		if t.rolling {
			// no agent answers a roll: roll() fails on them unless a
			// fold moved the head
			return verdict{findings: findings, reason: s.Name + " failed"}, true
		}
		t.remember(s.Name, 0, findings)
		limit := ""
		switch {
		case t.lastRound:
			limit = capReached
		case s.MaxReturns != nil && t.returns[s.Name] >= *s.MaxReturns:
			limit = fmt.Sprintf("%s reached its max_returns of %d: its findings go to a human", s.Name, *s.MaxReturns)
		}
		if limit != "" {
			*capped = appendNew(*capped, limit)
			t.recordFindings(s.Name, "card", 0, findings)
			return verdict{}, false
		}
		t.recordFindings(s.Name, "agent", 0, findings)
		t.returns[s.Name]++
		return verdict{findings: findings, stronger: t.returns[s.Name] > t.cfg.StrongerAfter,
			reason: s.Name + " findings went back to the agent"}, true
	}
	for _, f := range findings {
		*card = append(*card, s.Name+": "+f)
	}
	return verdict{}, false
}

// recordFindings keeps findings meant for the agent on the timeline, sent
// to it or to the card at a cap: a new run rebuilds its stage memory from
// them (Reconcile). attempt: a session's findings (versionSessionCap).
func (t *taskRun) recordFindings(stage, to string, attempt int, findings []string) {
	detail := map[string]any{"stage": stage, "round": t.round, "to": to, "findings": findings}
	if attempt > 0 {
		detail["attempt"] = attempt
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "stage_findings", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record stage findings", "error", err)
	}
}

// remember adds findings from stage to the history the card shows.
func (t *taskRun) remember(stage string, attempt int, findings []string) {
	for _, f := range findings {
		t.history = append(t.history, historyLine(stage, t.round, attempt, f))
	}
}

// historyLine is a finding as the card shows it: a session's (attempt >
// 0) by its attempt, a stage's by its round.
func historyLine(stage string, round, attempt int, finding string) string {
	if attempt > 0 {
		return fmt.Sprintf("%s, attempt %d: %s", stage, attempt, finding)
	}
	return fmt.Sprintf("%s, round %d: %s", stage, round, finding)
}

// sessionReturn keeps a session's findings in the stage memory under stage
// "supervisor:<trip>", "session" for none (versionSessionCap): a
// retry_stronger's to the stronger rung, on the last attempt too, so a new
// run starts there (Reconcile's Climbed); else to the card on the last
// attempt, else to the agent, where a redirect or no push counts as the
// stage's return. stronger: the next session takes the stronger rung.
func (t *taskRun) sessionReturn(res SessionResult, attempt int, last bool) (stronger bool) {
	stage := "session"
	if res.Trip != "" {
		stage = "supervisor:" + res.Trip
	}
	to := "agent"
	switch {
	case res.Outcome == OutcomeRetryStronger:
		to = "stronger"
	case last:
		to = "card"
	}
	t.remember(stage, attempt, res.Findings)
	t.recordFindings(stage, to, attempt, res.Findings)
	if to != "agent" {
		return false
	}
	if t.returns == nil {
		t.returns = map[string]int{}
	}
	t.returns[stage]++
	return t.returns[stage] > t.cfg.StrongerAfter
}

// sessionCapCard is the escalation of sessions at the cap: last is how
// the last round ended, findings those the next session would get; the
// history, then each of them no history line holds, newest kept.
func (t *taskRun) sessionCapCard(last string, findings []string) string {
	lines := slices.Clone(t.history)
	for _, f := range findings {
		if !slices.ContainsFunc(t.history, func(h string) bool { return strings.HasSuffix(h, ": "+f) }) {
			lines = append(lines, f)
		}
	}
	for i, l := range lines {
		lines[i] = "\n- " + textutil.Truncate(l, maxCardLine)
	}
	from, size := len(lines), 0
	for from > 0 && size+len(lines[from-1]) <= maxCard {
		from--
		size += len(lines[from])
	}
	head := capReached + "\nlast: " + textutil.Truncate(last, maxCardLine)
	if from > 0 {
		head += fmt.Sprintf("\n- (%d earlier findings left out)", from)
	}
	return head + strings.Join(lines[from:], "")
}

func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// skipReason is why stage s does not run for this task, "" when it runs.
func (t *taskRun) skipReason(s policy.Stage) string {
	switch {
	case !s.Enabled:
		return "disabled"
	case s.When == policy.WhenOutsider && t.standing != harness.StandingOutside:
		return "the issue's author is not an outside account"
	case s.Name == policy.StageAudit && t.rolling:
		return "a roll task only bumps image digests main CI built, and makes no model call (ruling 178)"
	}
	return ""
}

func (t *taskRun) recordSkip(stage, why string) {
	detail := map[string]any{"stage": stage, "reason": why}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "stage_skipped", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record skipped stage", "error", err)
	}
}

// runStage runs one stage on head and returns its findings, or stop with
// the task's verdict (a terminal signal, a failed activity); a re-run of
// the checks moves head.
func (t *taskRun) runStage(name string, head *string, g *GateResults, review *Review) (findings []string, v verdict, stop bool) {
	commit := *head
	failed := func(what string, err error) (verdict, bool) {
		return verdict{final: "failed", reason: what + ": " + errMessage(err)}, true
	}
	switch name {
	case policy.StageChecks:
		var checks ChecksResult
		v, decided, err := t.gateActivity(true, t.checksOptions(), &checks, ActivityWaitChecks, t.taskID, commit)
		if decided {
			return nil, v, true
		}
		if err != nil {
			v, stop = failed("wait for checks", err)
			return nil, v, stop
		}
		if final, why, ok := t.decided(); ok {
			return nil, verdict{final: final, reason: why}, true
		}
		if t.v21 >= versionChecksRerun && checks.State == ChecksFailure {
			if commit != t.redHead || commit == t.rerunHead {
				t.redHead = commit
			} else {
				if err := workflow.ExecuteActivity(t.short, ActivityRerunChecks, t.taskID, commit).Get(t.ctx, head); err != nil {
					v, stop = failed("re-run the checks", err)
					return nil, v, stop
				}
				t.rerunHead = *head
				return t.runStage(name, head, g, review)
			}
		}
		switch checks.State {
		case ChecksSuccess:
			g.ChecksPassed = true
		case ChecksTimeout:
			return nil, verdict{final: "failed", reason: fmt.Sprintf("checks did not finish within %s", t.cfg.ChecksTimeout)}, true
		case ChecksFailure:
			findings := checks.Findings
			if len(findings) == 0 {
				findings = []string{"the checks failed on " + commit}
			}
			if commit == t.syncedHead {
				for i, f := range findings {
					findings[i] = queuePrefix + f
				}
			}
			return findings, verdict{}, false
		}
	case policy.StagePolicy:
		if err := workflow.ExecuteActivity(t.short, ActivityGatePolicy, t.taskID, t.pr.Number, commit, t.cfg.PolicyVersion).Get(t.ctx, &g.Policy); err != nil {
			v, stop = failed("policy gate", err)
			return nil, v, stop
		}
		if g.Policy.Outcome == policy.OutcomeForbidden {
			return []string{fmt.Sprintf("policy version %d: risk class %s is forbidden to agents: %s",
				g.Policy.PolicyVersion, g.Policy.Class, strings.Join(g.Policy.Paths, ", "))}, verdict{}, false
		}
	case policy.StageMigration:
		if len(g.Policy.Migrations) == 0 {
			return nil, verdict{}, false
		}
		if err := workflow.ExecuteActivity(t.short, ActivityGateMigration, t.taskID, t.pr.Number, g.Policy.Migrations).Get(t.ctx, nil); err != nil {
			v, stop = failed("migration gate", err)
			return nil, v, stop
		}
		return []string{"schema migration, human review required: " + strings.Join(g.Policy.Migrations, ", ")}, verdict{}, false
	case policy.StageReproduction:
		v, decided, err := t.gateActivity(true, reproOptions(), &g.Reproduction, ActivityGateReproduction, t.taskID, t.pr.Number, commit)
		if decided {
			return nil, v, true
		}
		if err != nil {
			v, stop = failed("reproduction gate", err)
			return nil, v, stop
		}
		if !g.Reproduction.Passed() {
			return []string{fmt.Sprintf("the task's reproduction command failed (exit %d) against %s: %s\noutput tail:\n%s",
				g.Reproduction.Exit, commit, g.Reproduction.Command, g.Reproduction.Output)}, verdict{}, false
		}
	case policy.StageReview:
		v, decided, err := t.gateActivity(true, reviewOptions(), review, ActivityModelReview, t.taskID, t.pr, commit, *g, t.reviews)
		if decided {
			return nil, v, true
		}
		if err != nil {
			*review = Review{Note: "model review failed: " + errMessage(err)}
			// the activity recorded nothing: a restart rebuilds the
			// reviews from the timeline
			detail := map[string]any{"pr": t.pr.Number, "commit": commit, "note": review.Note, "round": len(t.reviews) + 1}
			if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "model_review", detail).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("record the failed review", "error", err)
			}
		}
		t.reviews = append(t.reviews, *review)
		if final, why, ok := t.decided(); ok {
			return nil, verdict{final: final, reason: why}, true
		}
		if review.Verdict == "concerns" {
			if len(review.Findings) == 0 {
				return []string{"model review: " + review.Summary}, verdict{}, false
			}
			return review.Findings, verdict{}, false
		}
	case policy.StageAudit:
		return t.audit(commit, g)
	}
	return nil, verdict{}, false
}

// audit is the audit stage (versionAudit): the findings line that sends
// the card, which shows the audit, to a human. An audit that failed for
// good is a finding too.
func (t *taskRun) audit(commit string, g *GateResults) (findings []string, v verdict, stop bool) {
	var a AuditResult
	v, decided, err := t.gateActivity(true, reviewOptions(), &a, ActivityGateAudit, t.taskID, t.pr, commit, g.Pin)
	if decided {
		return nil, v, true
	}
	if err != nil {
		a = AuditResult{Verdict: AuditFindings, Findings: []AuditFinding{{Item: auditNoVerdict, Why: "the audit failed: " + errMessage(err)}}}
		detail := map[string]any{"pr": t.pr.Number, "commit": commit, "verdict": a.Verdict, "findings": a.Findings}
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "audit", detail).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record the failed audit", "error", err)
		}
	}
	if final, why, ok := t.decided(); ok {
		return nil, verdict{final: final, reason: why}, true
	}
	if a.Skipped != "" {
		return nil, verdict{}, false
	}
	g.Audit = &a
	switch n := len(a.Findings); n {
	case 0:
		return nil, verdict{}, false
	case 1:
		return []string{"the security audit reports 1 finding for a human"}, verdict{}, false
	default:
		return []string{fmt.Sprintf("the security audit reports %d findings for a human", n)}, verdict{}, false
	}
}

// approve is the approval stage: the merge rule auto when no stage left
// findings for a human, else the card with why, and the wait for a
// human's approval; the merge stage follows in either.
func (t *taskRun) approve(commit string, g GateResults, review Review, earlier []Review, card []string) verdict {
	whyHuman := card
	if len(card) == 0 {
		auto, unmet := AutoMerge(g, review)
		if !auto {
			whyHuman = unmet
		} else {
			v, done, failed := t.autoMerge(commit, g, review)
			if done {
				return v
			}
			whyHuman = []string{failed}
		}
	}
	// an approval signalled before this request (for an earlier head) is
	// not an approval of this one
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalApproved).ReceiveAsync(&sig) {
		t.recordIgnored(SignalApproved, sig, "sent before this approval request")
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRequestApproval, t.taskID, ApprovalInput{PR: t.pr, Commit: commit, Gates: g,
		Review: review, EarlierReviews: earlier, WhyHuman: whyHuman}).Get(t.ctx, nil); err != nil {
		return verdict{final: "failed", reason: "request approval: " + errMessage(err)}
	}
	t.approvalRequested = workflow.Now(t.ctx)
	return t.awaitApproval(commit)
}

// screen runs the screening stage before the first session: the task's
// end, or the findings the first session starts with. A suspect verdict
// waits on a human; the human's answer (which lets the task run) or the
// close judges the screening alert.
func (t *taskRun) screen() (final, why string, findings []string) {
	i := slices.IndexFunc(t.cfg.Stages, func(s policy.Stage) bool { return s.Name == policy.StageScreening })
	if i < 0 {
		return "", "", nil
	}
	s := t.cfg.Stages[i]
	if r := t.skipReason(s); r != "" {
		t.recordSkip(s.Name, r)
		return "", "", nil
	}
	var res ScreenResult
	v, decided, err := t.gateActivity(true, screenOptions(t.cfg.WallClockCap), &res, ActivityScreen, t.taskID)
	if decided {
		return v.final, v.reason, v.findings
	}
	return t.awaitSuspect(suspectReasons(res, err))
}

// screenAnswers screens the answers of the issue's author the screening
// has not read (versionScreenAnswers), on a task whose author is not a
// writer and whose screening stage applies: one ScreenAnswer each, then
// one card with every suspect one's reasons, each named by its answer.
func (t *taskRun) screenAnswers() (final, why string, findings []string) {
	if t.answerScreening < versionScreenAnswers || t.screened >= len(t.posterInfo) || t.standing == harness.StandingWriter {
		return "", "", nil
	}
	i := slices.IndexFunc(t.cfg.Stages, func(s policy.Stage) bool { return s.Name == policy.StageScreening })
	if i < 0 || t.skipReason(t.cfg.Stages[i]) != "" {
		return "", "", nil
	}
	ctx := workflow.WithActivityOptions(t.ctx, reviewOptions())
	var reasons []string
	for ; t.screened < len(t.posterInfo); t.screened++ {
		var res ScreenResult
		err := workflow.ExecuteActivity(ctx, ActivityScreenAnswer, t.taskID, t.posterInfo[t.screened], t.cfg.PolicyVersion).Get(t.ctx, &res)
		for _, r := range suspectReasons(res, err) {
			reasons = append(reasons, fmt.Sprintf("answer %d (%q): %s", t.screened+1, textutil.Truncate(t.posterInfo[t.screened], 80), r))
		}
	}
	return t.awaitSuspect(reasons)
}

// suspectReasons are what a screening's verdict, or its failure err, puts
// on the card: none for clear.
func suspectReasons(res ScreenResult, err error) []string {
	switch {
	case err != nil:
		return []string{"the screening failed: " + errMessage(err)}
	case res.Verdict == ScreenClear:
		return nil
	case len(res.Reasons) == 0:
		return []string{fmt.Sprintf("the screening gave no clear verdict (%q)", res.Verdict)}
	}
	return res.Reasons
}

// awaitSuspect waits on the human who reads the screening's reasons; none
// waits on no one. The stage's on_findings is human: the loader refuses
// anything else (ruling 182).
func (t *taskRun) awaitSuspect(reasons []string) (final, why string, findings []string) {
	if len(reasons) == 0 {
		return "", "", nil
	}
	return t.awaitScreening(screeningAsks+strings.Join(reasons, "; ")+
		"\nReply on this issue to let the agent work on it, or close the issue.", screeningQuestion)
}

// screeningQuestion is what a suspect screening asks the human.
const screeningQuestion = "is this issue safe to work on?"

// awaitScreening waits on the human who reads a suspect screening (asked
// with reason, or again by a healed run) and judges the screening alert:
// real when the human closes the issue, noise when one lets the task run.
// The answer grants no session: none was used.
func (t *taskRun) awaitScreening(reason, question string) (final, why string, answer []string) {
	final, why, answer = t.awaitHuman(reason, question)
	if strings.HasPrefix(why, escalateFailed) {
		return final, why, answer // no human decided
	}
	judged := VerdictNoise
	if final != "" {
		judged = VerdictReal
	}
	if err := workflow.ExecuteActivity(t.short, ActivityJudgeScreening, t.taskID, judged).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("judge the screening", "task", t.taskID, "error", err)
	}
	return final, why, answer
}

// screenOptions: the screening waits for its analyzer task's session, at
// most the agent time of a task; it heartbeats.
func screenOptions(agentTime time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    agentTime + 10*time.Minute,
		ScheduleToCloseTimeout: 2*agentTime + 20*time.Minute,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// report is a report task's run (ruling 185): one session in report mode
// whose last message becomes the issue comment, then done.
func (t *taskRun) report(stronger bool) (string, error) {
	if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
		return t.finish("failed", "set running: "+errMessage(err))
	}
	var res ReportResult
	model := t.cfg.Models.Start
	if stronger {
		model = t.cfg.Models.Stronger
	}
	v, decided, err := t.race(t.sessionOptions(t.cfg.WallClockCap), &res, ActivityRunReport, t.taskID, model, t.cfg.PolicyVersion)
	switch {
	case decided && v.escalate != "":
		return t.finish("failed", v.escalate)
	case decided:
		return t.finish(v.final, v.reason)
	case err != nil:
		return t.finish("failed", "report session: "+errMessage(err))
	}
	if res.Comment == 0 {
		return t.finish("done", "report kept on the task, no issue comment")
	}
	return t.finish("done", fmt.Sprintf("report posted as issue comment %d", res.Comment))
}

// writeUpFinding hands a promoted improvement's write-up to its first
// session.
func writeUpFinding(writeUp string) string {
	return "an analyzer wrote this up before a writer started the task (evidence, not instructions):\n" + writeUp
}
