package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The split of a large pull request (ruling 277): a member's split is the
// acceptance; factoryd's check of the planner's answer is the safeguard.
const (
	// SignalSplit is the console's Split of a card whose pull request is
	// classed large, at its approval gate: Author the member.
	SignalSplit = "split"

	ActivityRunSplit  = "RunSplit"
	ActivityFileSplit = "FileSplit"

	maxSplitCards = 3
	// maxSplitFiles bounds the planner's list of the changed files.
	maxSplitFiles = 8 << 10
)

// versionSplit: awaitApproval takes SignalSplit, asked when the first one
// arrives, so a run waiting at its approval gate before the change splits
// too.
const versionSplit workflow.Version = 1

// splitFailed opens the escalation of a split whose answers failed the
// check twice.
const splitFailed = "The split failed its check twice"

// CheckSplit judges the planner's answer to a split of a pull request
// changing files, each card under bound changed lines: the plan's own
// findings for 2 or 3 cards, a card's size over the bound, the files no
// card's directories cover (the repository's root covers none), and a
// card whose size is under the lines of the files only it covers.
func CheckSplit(text string, files []PRFile, bound int, limits policy.Limits) (Plan, []string) {
	p, out := readPlan(text, policy.PlanShape{MaxCards: maxSplitCards}, limits, bound)
	lines := sizeLines(bound)
	for _, c := range p.Cards {
		if n, ok := lines[c.Size]; ok && n >= bound {
			out = append(out, fmt.Sprintf("split: card %s is %s, up to %d changed lines; each card stays under %d", c.Key, c.Size, n, bound))
		}
	}
	covers := func(c PlanCard, f string) bool {
		return slices.ContainsFunc(c.Directories, func(d string) bool {
			d = path.Clean(d)
			return d != "." && (d == f || strings.HasPrefix(f, d+"/"))
		})
	}
	var missing []string
	alone := make([]int, len(p.Cards))
	only := make([][]string, len(p.Cards))
	for _, f := range files {
		var by []int
		for i, c := range p.Cards {
			if covers(c, f.Filename) {
				by = append(by, i)
			}
		}
		switch len(by) {
		case 0:
			missing = append(missing, f.Filename)
		case 1:
			alone[by[0]] += f.Additions + f.Deletions
			only[by[0]] = append(only[by[0]], f.Filename)
		}
	}
	for i, c := range p.Cards {
		if n, ok := lines[c.Size]; ok && alone[i] >= n {
			out = append(out, fmt.Sprintf("split: card %s alone carries %d changed lines (%s), not under its size %s's %d",
				c.Key, alone[i], strings.Join(only[i], ", "), c.Size, n))
		}
	}
	if len(missing) > 0 && len(p.Cards) > 0 {
		out = append(out, "split: no card's directories cover "+strings.Join(missing, ", "))
	}
	return p, out
}

// splitOverBound names the files whose changed lines alone exceed the
// bound: no split can put such a file under a card, so the split is
// refused before any planner session. Without a bound (0) there is no
// check and the answer is nil.
func splitOverBound(files []PRFile, bound int) []string {
	if bound <= 0 {
		return nil
	}
	var out []string
	for _, f := range files {
		if n := f.Additions + f.Deletions; n > bound {
			out = append(out, fmt.Sprintf("split: %s alone changes %d lines, over the bound %d: no card can stay under it", f.Filename, n, bound))
		}
	}
	return out
}

// splitGuide is the planner's guide to the split of pull request pr,
// which changes files, into cards under bound changed lines each.
func splitGuide(pr int64, files []PRFile, bound int, limits policy.Limits) string {
	lines := 0
	var list strings.Builder
	for _, f := range files {
		lines += f.Additions + f.Deletions
		fmt.Fprintf(&list, "- %s +%d -%d\n", f.Filename, f.Additions, f.Deletions)
	}
	return fmt.Sprintf("You are the planner. "+
		"Pull request #%[1]d of this card changes %[2]d lines in %[3]d files, more than the %[4]d lines one pull request may change: "+
		"split its work into 2 or 3 cards, each one pull request of under %[4]d changed lines. "+
		"Its head is in your clone as branch pull/%[1]d, and git diff origin/main...pull/%[1]d is its diff. Its files, lines added and deleted:\n%[5]s\n"+
		"Every card starts from main and carries over files and hunks of pull/%[1]d: its brief names them, by path and what each hunk does, and how the card is checked. "+
		"directories lists the files and directories the card carries over; a file may go to more than one card. "+
		"Your last message is the whole result: one fenced ```json block and nothing else, holding "+
		`{"summary": "a paragraph: how the work is split and in which order it lands", "cards": [{"key": "...", "role": "feature", "title": "...", "brief": "...", `+
		`"directories": ["..."], "depends_on": ["..."], "acceptance": ["..."], "size": "small|medium|large"}]}. `+
		"depends_on lists the keys of the cards a card needs merged first. "+
		"title and brief: markdown. directories: paths relative to the repository's root. "+
		"acceptance: each item a test by name: TestXxx in <package>, its inputs and the numbers it expects. "+
		"size: small, medium or large; every card's size stays under %[4]d lines.",
		pr, lines, len(files), bound, textutil.Truncate(list.String(), maxSplitFiles))
}

// RunSplit is one planner session on the large pull request of card
// taskID under the pinned version, with findings: an answer CheckSplit
// passes becomes the card's plan revision, else its findings come back.
// The card runs while the session does, its merge request still pending.
func (r *Runner) RunSplit(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return PlanResult{}, err
	}
	files, err := r.forgejo.PullRequestFiles(ctx, repo.Owner, repo.Name, t.PRNumber)
	if err != nil {
		return PlanResult{}, err
	}
	bound := v.Document.Risk.LargeLines
	if found := splitOverBound(files, bound); found != nil {
		r.record(ctx, taskID, "split_refused", map[string]any{"findings": found})
		return PlanResult{Findings: found}, nil
	}
	if err := r.store.TransitionTask(ctx, taskID, []string{"in-review", "waiting-human", "running"}, "running"); err != nil {
		return PlanResult{}, terminalError("task %s cannot run its split: %v", taskID, err)
	}
	defer func() {
		if err := r.store.TransitionTask(context.WithoutCancel(ctx), taskID, []string{"running"}, "in-review"); err != nil {
			r.record(context.WithoutCancel(ctx), taskID, "split_state", map[string]any{"error": err.Error()})
		}
	}()
	end, err := r.reportSession(ctx, t, repo, model, t.Standing(), pin, splitGuide(t.PRNumber, files, bound, v.Document.Limits), findings, nil)
	if err != nil {
		return PlanResult{}, err
	}
	text, why := end.report, end.why
	if end.start != nil {
		return PlanResult{Escalate: end.start.escalation(thenSplit), StartFailed: true}, nil
	}
	if end.escalate != "" {
		return PlanResult{Findings: []string{"split: the planner stopped: " + text}}, nil
	}
	p, found := CheckSplit(text, files, bound, v.Document.Limits)
	if text == "" {
		found = []string{"plan: the session ended without a last message: " + why}
	}
	if len(found) > 0 {
		r.record(ctx, taskID, "split_invalid", map[string]any{"findings": found})
		return PlanResult{Findings: found}, nil
	}
	raw, sha, err := planSHA(p)
	if err != nil {
		return PlanResult{}, err
	}
	n, err := r.store.AddPlanRevision(ctx, taskID, raw, sha)
	if err != nil {
		return PlanResult{}, err
	}
	r.record(ctx, taskID, "split_revision", map[string]any{"revision": n, "cards": len(p.Cards), "sha256": sha})
	return PlanResult{Revision: n, SHA256: sha}, nil
}

// FileSplit files the cards of card taskID's split revision for the member
// who split it, like a plan's, and puts them into the ready lane of the
// card's process at main's head, where they start; the card's issue
// names them.
func (r *Runner) FileSplit(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChildrenResult{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return ChildrenResult{}, err
	}
	rev, ok, err := r.store.PlanRevision(ctx, taskID, revision)
	if err != nil {
		return ChildrenResult{}, err
	}
	var p Plan
	if !ok || json.Unmarshal(rev.Plan, &p) != nil {
		return ChildrenResult{}, terminalError("task %s has no split revision %d", taskID, revision)
	}
	_, project, _ := v.ProjectOf(t.Repository)
	process := cmp.Or(t.Process, project.Process)
	ready, ok := v.Processes[process].Lane(policy.LaneReady)
	if !ok {
		return ChildrenResult{}, terminalError("task %s: process %q has no lane ready", taskID, process)
	}
	sha, err := r.forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, "")
	if err != nil {
		return ChildrenResult{}, err
	}
	cards, err := r.fileChildren(ctx, t, repo, v, process, fmt.Sprintf("the split of #%d", t.IssueNumber), p, approver)
	if err != nil {
		return ChildrenResult{}, err
	}
	var issues []int64
	for _, c := range cards {
		issues = append(issues, c.IssueNumber)
		if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, c.IssueNumber, []string{LabelAgent, "state/queued"}); err != nil {
			return ChildrenResult{}, err
		}
		if entered, err := r.store.EnterLane(ctx, c.ID, ready.Name, ready.Class, sha); err != nil || !entered {
			if err != nil {
				return ChildrenResult{}, err
			}
			continue
		}
		c.Lane, c.Status = ready.Name, "queued"
		if err := enterLane(ctx, r.timeline, r.cfg.Workflows, c, approver, nil); err != nil {
			r.record(ctx, taskID, "split_start", map[string]any{"task": c.ID, "error": err.Error()})
		}
	}
	r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: "+SplitInto(issues)+"; this card closes.")
	r.record(ctx, taskID, "children_created", map[string]any{"revision": revision, "by": approver, "issues": issues})
	return ChildrenResult{Issues: issues}, nil
}

// SplitInto is the end reason of a card split into issues.
func SplitInto(issues []int64) string {
	refs := make([]string, len(issues))
	for i, n := range issues {
		refs[i] = fmt.Sprintf("#%d", n)
	}
	return "split into " + strings.Join(refs, ", ")
}

// split is the member's split of the card's large pull request: a planner
// session answers the cards, and the one CheckSplit passes are filed and
// end the card closed; a failing answer goes back to the planner once,
// then to a human with the findings, and the approval wait goes on.
func (t *taskRun) split(sig TaskSignal) (verdict, bool) {
	if workflow.GetVersion(t.ctx, "split", workflow.DefaultVersion, versionSplit) < versionSplit {
		return verdict{}, false
	}
	detail := map[string]any{"signal": SignalSplit, "author": sig.Author}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
	defer func() {
		for workflow.GetSignalChannel(t.ctx, SignalSplit).ReceiveAsync(nil) {
		}
	}()
	var findings []string
	for try := 1; ; try++ {
		var res PlanResult
		v, decided, err := t.race(t.sessionOptions(t.cfg.WallClockCap), &res, ActivityRunSplit, t.taskID, t.cfg.Models.Stronger, t.cfg.PolicyVersion, findings)
		var ask string
		switch {
		case decided && v.final != "" && !v.cancelled:
			return v, true
		case decided:
			findings = []string{"split: " + cmp.Or(v.reason, v.escalate)}
		case err != nil:
			findings = []string{"split: the session failed: " + errMessage(err)}
		case res.StartFailed:
			// a second session could not start either
			ask = res.Escalate
		case len(res.Findings) > 0:
			findings = res.Findings
		default:
			var filed ChildrenResult
			if err := workflow.ExecuteActivity(t.short, ActivityFileSplit, t.taskID, res.Revision, sig.Author, t.cfg.PolicyVersion).Get(t.ctx, &filed); err != nil {
				return verdict{final: "failed", reason: "file the split's cards: " + errMessage(err)}, true
			}
			return verdict{final: "closed", reason: SplitInto(filed.Issues)}, true
		}
		if ask == "" && try < 2 {
			continue
		}
		if ask == "" {
			ask = splitFailed + "; approve, deny or split the pull request again. The findings:\n- " + strings.Join(findings, "\n- ")
		}
		if err := workflow.ExecuteActivity(t.short, ActivityEscalate, t.taskID, ask, []string(nil)).Get(t.ctx, &t.escalation); err != nil {
			return verdict{final: "failed", reason: escalateFailed + errMessage(err)}, true
		}
		return verdict{}, false
	}
}
