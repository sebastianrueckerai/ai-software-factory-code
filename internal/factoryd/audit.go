package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The audit's verdicts.
const (
	AuditClear    = "clear"
	AuditFindings = "findings"
)

// AuditResult is the security audit's verdict on a pull request's head:
// clear, or findings for the human card. Skipped says why the project's
// pull requests are not audited; then there is no verdict.
type AuditResult struct {
	Verdict    string         `json:"verdict,omitempty"`
	Findings   []AuditFinding `json:"findings,omitempty"`
	Model      string         `json:"model,omitempty"`
	Deployment string         `json:"deployment,omitempty"`
	// Thinking is "low" when a part's answer came from the call once
	// more at low thinking depth (SupervisorModel.Chat); "" at the
	// model's own.
	Thinking string `json:"thinking,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
	// MovedTo is the head the pull request held instead of the audited
	// commit, when the audit did not run for it
	MovedTo string `json:"moved_to,omitempty"`
	// Confirmed are the findings a person confirmed (audit confirm),
	// keyed by the finding's text, each with who, why and when.
	// The verdict settles past them; the audit model neither sees nor
	// answers them.
	Confirmed map[string]Adjudication `json:"confirmed,omitempty"`
	// Covered are the findings of this head a confirmation covered, each
	// as recorded; its text keys the confirmation in Confirmed.
	Covered []AuditFinding `json:"covered,omitempty"`
}

// AuditFinding is one checklist item a change touches; Line is in the new
// file, 0 for none.
type AuditFinding struct {
	Item string `json:"item"`
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	Why  string `json:"why"`
}

// Text is the finding's identity a person's confirmation names: its item,
// at its file where it names one. The model's line drifts between runs and
// its why is its wording, so neither is part of it: a confirmation covers
// its item in its file on every head of the task.
func (f AuditFinding) Text() string {
	if f.File == "" {
		return f.Item
	}
	return f.Item + " at " + f.File
}

const (
	auditNoVerdict     = "the audit gave no verdict"
	auditNotSwitchedOn = "the project does not switch the audit on"
	auditListFull      = "the audit model listed its maximum; more may stand"
	maxPreCheck        = 50
	maxAuditField      = 500
	maxAuditWhy        = 300
)

// auditPartFacts tells a later part's call what its part is.
const auditPartFacts = "part names the part you read."

// auditItems are the audit's fixed checklist: the model answers an item by
// its number, and a finding holds the item's name. The rules are the
// answer's item description (AuditAnswerFinding).
var auditItems = []string{"secrets", "network egress", "RBAC and ServiceAccounts", "policy and workflow files", "migrations", "sandbox contract"}

// auditInput is the call on the diff's first part, a diff of one part's
// only call: it alone carries the pull request and the whole diff's
// concurrency summary.
type auditInput struct {
	Repository  string `json:"repository"`
	PR          int64  `json:"pull_request"`
	Commit      string `json:"head"`
	Diff        string `json:"diff"`
	Part        string `json:"part,omitempty"`
	Concurrency string `json:"concurrency"`
}

// auditPartInput is the call on each later part.
type auditPartInput struct {
	Diff string `json:"diff"`
	Part string `json:"part"`
}

// parseAudit reads the model's answer: its findings, or why it gave no
// verdict. No finding is a clear audit.
func parseAudit(content string) ([]AuditFinding, string) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) >= 6 {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s, "```")[3:], "json"))
	}
	var answer struct {
		Findings []struct {
			Item json.RawMessage `json:"item"`
			File string          `json:"file"`
			Line int             `json:"line"`
			Why  string          `json:"why"`
		} `json:"findings"`
	}
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&answer); err != nil {
		return nil, "the answer is not one JSON object: " + err.Error()
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "the answer is not one JSON object: text after it"
	}
	var out []AuditFinding
	for _, f := range answer.Findings {
		var n int
		if json.Unmarshal(f.Item, &n) != nil || n < 1 || n > len(auditItems) {
			return nil, fmt.Sprintf("the answer named checklist item %s, which is not a number from 1 to %d", cmp.Or(string(f.Item), "none"), len(auditItems))
		}
		out = append(out, AuditFinding{Item: auditItems[n-1], File: strings.TrimSpace(f.File), Line: max(f.Line, 0), Why: textutil.Truncate(strings.TrimSpace(f.Why), maxAuditWhy)})
	}
	return out, ""
}

// auditConfirmedOf reports whether a confirmation covers finding f: one
// recorded under a text of f's id (FindingID), the identity the card's box
// names it by. A finding at no file, the no-verdict one or a count of
// findings past the card's list, is never covered.
func auditConfirmedOf(f AuditFinding, confirmed map[string]Adjudication) bool {
	if f.File == "" {
		return false
	}
	id := FindingID(f.Text())
	for text := range confirmed {
		if FindingID(text) == id {
			return true
		}
	}
	return false
}

// auditConfirmations reads the audit findings a person confirmed on the
// task (audit-confirm): the audit_finding_confirmed steps, each the text
// of a finding a person judged a fixture, with who, why and when; a later
// step's over an earlier one's. A card with no timeline confirmed none.
func (r *Runner) auditConfirmations(ctx context.Context, taskID string) (map[string]Adjudication, error) {
	b, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return nil, err
	}
	out := map[string]Adjudication{}
	for line := range strings.SplitSeq(string(b), "\n") {
		var step struct {
			Step, By, Finding, Why string
			Time                   time.Time
		}
		if json.Unmarshal([]byte(line), &step) != nil || step.Step != stepAuditConfirmed {
			continue
		}
		out[step.Finding] = Adjudication{By: step.By, Why: step.Why, When: step.Time}
	}
	return out, nil
}

// GateAudit is the audit stage on the pull request's head,
// for a project that switches it on under the task's pinned version: a
// deterministic scan of the added lines for secret shapes, then the audit
// model the policy names for the task's class, through the gateway on the
// task's account, one call for each part of the diff (callAll runs them in
// parallel), a file larger than a part cut by hunks, the first with the
// concurrency summary of the whole diff. An answer without a verdict is a
// finding, and a refused or failed call one for its reason that names its
// parts; the parts' findings keep the first maxJoinedFindings. A diff the
// card cannot pay for, or past maxDiffRead, is one finding and no call.
// The findings go to the card and the timeline, never to the
// pull request, which the agent reads. The findings a person confirmed
// (audit-confirm) hold no verdict: the audit reads their steps off the
// timeline and settles past them, theirs the who, why and when beside the
// verdict. An audit beside the checks wait
// (gates.Beside) starts its gate at its own schedule. round is the
// stages' pass that runs it, which keys its fact row with the head; 0 is
// an activity scheduled before the workflow passed it.
func (r *Runner) GateAudit(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults, round int) (AuditResult, error) {
	stopBeats := beatUntilDone(ctx, "audit")
	defer stopBeats()
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return AuditResult{}, err
	}
	if err := r.refuseWip(ctx, repo, commit); err != nil {
		return AuditResult{}, err
	}
	v, err := r.pinnedVersion(ctx, gates.Pin)
	if err != nil {
		return AuditResult{}, err
	}
	if _, p, _ := v.ProjectOf(t.Repository); !p.Audit {
		r.record(ctx, t.ID, StepStageSkipped, map[string]any{"stage": policy.StageAudit, "reason": auditNotSwitchedOn})
		return AuditResult{Skipped: auditNotSwitchedOn}, nil
	}
	confirmed, err := r.auditConfirmations(ctx, taskID)
	if err != nil {
		return AuditResult{}, err
	}
	res := AuditResult{Model: r.callModel(t, policy.PurposeAudit), Confirmed: confirmed}
	noVerdict := func(why string) {
		res.Findings = append(res.Findings, AuditFinding{Item: auditNoVerdict, Why: why})
	}
	prFiles, err := r.forgejo.PullRequestFileDiffs(ctx, repo.Owner, repo.Name, pr.Number)
	// the head is checked after the read, which goes by the pull request's
	// number, so a push before it is a moved head, never another head's diff
	if moved, err := r.headIs(ctx, repo, pr.Number, commit); err != nil {
		if !isTerminal(err) && !isHeadMoved(err) {
			return AuditResult{}, err
		}
		noVerdict("the audit did not run: " + errMessage(err))
		res.MovedTo = moved
		return r.recordAudit(ctx, t, pr, commit, round, gates.Beside, res, redact.New())
	}
	if errors.Is(err, ErrDiffTooLong) {
		noVerdict("the audit did not run: " + err.Error())
		return r.recordAudit(ctx, t, pr, commit, round, gates.Beside, res, redact.New())
	}
	if err != nil {
		return AuditResult{}, err
	}
	var secrets []string
	var hits []redact.Hit
	for _, f := range prFiles {
		secrets, hits = append(secrets, redact.DiffSecrets(f.Text)...), append(hits, redact.ScanDiff(f.Text)...)
	}
	scrub := redact.New(secrets...)
	// a confirmed finding leaves the list before its cap, so the card lists
	// the first unconfirmed ones
	covered := func(f AuditFinding) bool {
		rec := asRecorded(f, scrub)
		if !auditConfirmedOf(rec, res.Confirmed) {
			return false
		}
		res.Covered = append(res.Covered, rec)
		return true
	}
	var prechecked []AuditFinding
	for _, h := range hits {
		if f := (AuditFinding{Item: "secret in the diff: " + h.Kind, File: h.File, Line: h.Line,
			Why: "an added line has the shape of a " + h.Kind + " (pre-check)"}); !covered(f) {
			prechecked = append(prechecked, f)
		}
	}
	prechecked, more := capTexts(prechecked, maxPreCheck)
	if more > 0 {
		prechecked = append(prechecked, AuditFinding{Item: fmt.Sprintf("secret in the diff: %d more pre-check hits", more),
			Why: fmt.Sprintf("the card lists the first %d", maxPreCheck)})
	}
	res.Findings = append(res.Findings, prechecked...)
	paths := map[string]bool{}
	for _, f := range prFiles {
		paths[f.Path], paths[f.OldPath] = true, f.OldPath != ""
	}
	parts := diffParts(prFiles, reviewDiffMax)
	first := auditInput{Repository: t.Repository, PR: pr.Number, Commit: commit, Concurrency: concurrencyBlock(concurrencySites(prFiles))}
	if len(parts) > 1 {
		first.Part = partLabel(parts, 0, len(prFiles))
	}
	firstPrompt, err := prompts.Build("security-audit", auditFacts{})
	if err != nil {
		return AuditResult{}, err
	}
	partPrompt, err := prompts.Build("security-audit-part", auditFacts{Part: true})
	if err != nil {
		return AuditResult{}, err
	}
	calls := make([]reviewCall, len(parts))
	for i, part := range parts {
		calls[i] = reviewCall{Label: fmt.Sprintf("part %d of %d", i+1, len(parts)), Prompt: firstPrompt, Input: func() any {
			in := first
			in.Diff = joinFiles(part)
			return in
		}}
		if i > 0 {
			calls[i].Prompt, calls[i].Input = partPrompt, func() any {
				return auditPartInput{Diff: joinFiles(part), Part: partLabel(parts, i, len(prFiles))}
			}
		}
	}
	if why, err := r.unfunded(ctx, t.ID, res.Model, policy.PurposeAudit, calls); err != nil {
		return AuditResult{}, err
	} else if why != "" {
		noVerdict("the audit did not run: " + why + unfundedRemedy)
		stopBeats()
		return r.recordAudit(ctx, t, pr, commit, round, gates.Beside, res, scrub)
	}
	results, err := r.callAll(ctx, t.ID, res.Model, v.Document.Limits.SupervisorTimeout, time.Time{}, calls)
	if err != nil {
		return AuditResult{}, err
	}
	// a refused call is one finding for each refusal, which names its parts
	// (the retry gives every part after a refusal that refusal), and a
	// failed call one for each status the gateway answered with; a finding
	// of one part says its whole text
	var gaps []string
	gapParts, gapText := map[string][]int{}, map[string]string{}
	var found []AuditFinding
	for i, got := range results {
		label := ""
		if len(calls) > 1 {
			label = calls[i].Label + ": "
		}
		res.Deployment = cmp.Or(got.Deployment, res.Deployment)
		var refusal *policy.Refusal
		gap, text := "", ""
		switch {
		case errors.As(got.Err, &refusal):
			gap = "the audit call was refused by the gateway (" + refusal.Decision + "): " + refusal.Reason
			text = gap
		case got.Err != nil:
			gap, text = "the audit call failed: "+cmp.Or(failedStatus.FindString(got.Err.Error()), got.Err.Error()), "the audit call failed: "+got.Err.Error()
		default:
			if got.Low {
				res.Thinking = thinkingLow
			}
			parsed, why := parseAudit(got.Content)
			if why != "" {
				noVerdict(fmt.Sprintf("%s%s (an answer of %d bytes)", label, why, len(got.Content)))
			}
			// an answer of the most one lists may have left findings out
			if len(parsed) >= maxJoinedFindings {
				res.Findings = append(res.Findings, AuditFinding{Item: auditListFull,
					Why: fmt.Sprintf("%sthe answer listed %d findings, and one answer lists at most %d", label, len(parsed), maxJoinedFindings)})
			}
			for _, f := range parsed {
				p := diffPath(f.File, paths)
				if p == "" && f.File != "" {
					f.Why = fmt.Sprintf("%s (at %q, no path of the diff)", f.Why, f.File)
				}
				f.File = p
				found = append(found, f)
			}
			continue
		}
		if gapParts[gap] == nil {
			gaps, gapText[gap] = append(gaps, gap), text
		}
		gapParts[gap] = append(gapParts[gap], i)
	}
	for _, gap := range gaps {
		at := gapParts[gap]
		if len(at) == 1 {
			gap = gapText[gap]
		}
		if len(calls) > 1 {
			gap = partsNamed(at, len(calls)) + ": " + gap
		}
		noVerdict(gap)
	}
	found, more = capTexts(slices.DeleteFunc(found, covered), maxJoinedFindings)
	if more > 0 {
		found = append(found, AuditFinding{Item: fmt.Sprintf("... and %d more", more),
			Why: fmt.Sprintf("the card lists the first %d of the parts' findings", maxJoinedFindings)})
	}
	res.Findings = append(res.Findings, found...)
	stopBeats()
	return r.recordAudit(ctx, t, pr, commit, round, gates.Beside, res, scrub)
}

// failedStatus is a failed call's status, which keys its finding: the
// bodies of a burst of upstream 429s differ.
var failedStatus = regexp.MustCompile(`^status \d+`)

// recordAudit scrubs the item and why of res's findings, the model's and
// the errors', of the diff's secret values and the generic shapes (a
// finding's file is a path of the diff, its own, or none), settles its
// verdict and puts it on the timeline and
// in its fact row (auditFact); beside: the audit ran beside the checks
// wait (besideEnd). The findings a person confirmed hold no verdict: each
// covered one leaves the findings for the covered list, beside the
// confirmation that covers it, whose who and why are scrubbed the same way.
func (r *Runner) recordAudit(ctx context.Context, t Task, pr PullRequest, commit string, round int, beside bool, res AuditResult, scrub *redact.Scrubber) (AuditResult, error) {
	for i, f := range res.Findings {
		res.Findings[i] = asRecorded(f, scrub)
	}
	var kept []AuditFinding
	for _, f := range res.Findings {
		if auditConfirmedOf(f, res.Confirmed) {
			res.Covered = append(res.Covered, f)
			continue
		}
		kept = append(kept, f)
	}
	res.Findings = kept
	if covered, more := capTexts(res.Covered, maxJoinedFindings); more > 0 {
		res.Covered = append(covered, AuditFinding{Item: fmt.Sprintf("... and %d more", more), Why: fmt.Sprintf("the step lists the first %d covered findings", maxJoinedFindings)})
	} else {
		res.Covered = covered
	}
	for k, adj := range res.Confirmed {
		res.Confirmed[k] = Adjudication{By: adj.By, Why: textutil.Truncate(scrub.String(adj.Why), maxAuditField), When: adj.When}
	}
	res.Verdict = AuditClear
	if len(res.Findings) > 0 {
		res.Verdict = AuditFindings
	}
	detail := map[string]any{"pr": pr.Number, "commit": commit, "model": res.Model,
		"deployment": res.Deployment, "verdict": res.Verdict, "findings": res.Findings, "confirmed": res.Confirmed, "covered": res.Covered}
	if res.Thinking != "" {
		detail["thinking"] = res.Thinking
	}
	r.record(ctx, t.ID, "audit", besideEnd(ctx, t, GateAudit, beside, detail))
	return res, r.auditFact(ctx, t, pr, commit, round, res)
}

// asRecorded is f as the timeline records it: its item and why scrubbed of
// the diff's secret values and the generic shapes, each field cut.
func asRecorded(f AuditFinding, scrub *redact.Scrubber) AuditFinding {
	return AuditFinding{Item: textutil.Truncate(scrub.String(f.Item), maxAuditField), File: textutil.Truncate(f.File, maxAuditField),
		Line: f.Line, Why: textutil.Truncate(scrub.String(f.Why), maxAuditField)}
}

// capTexts is fs up to limit texts, each text once: its first finding, its
// why counting the others at its file. A finding at no file is a text of
// its own. more counts the findings past the limit.
func capTexts(fs []AuditFinding, limit int) (kept []AuditFinding, more int) {
	first, others := map[string]int{}, []int{}
	for _, f := range fs {
		if k, ok := first[f.Text()]; ok && f.File != "" {
			others[k]++
			continue
		}
		if len(kept) == limit {
			more++
			continue
		}
		if f.File != "" {
			first[f.Text()] = len(kept)
		}
		kept, others = append(kept, f), append(others, 0)
	}
	for k, n := range others {
		if n > 0 {
			kept[k].Why += fmt.Sprintf("; %d more at this file", n)
		}
	}
	return kept, more
}

// diffPath is the path of the diff file names: file itself, or file without
// a leading a/, b/, ./ or /; "" when it names none.
func diffPath(file string, paths map[string]bool) string {
	for _, p := range []string{file, strings.TrimPrefix(file, "a/"), strings.TrimPrefix(file, "b/"), strings.TrimPrefix(file, "./"), strings.TrimPrefix(file, "/")} {
		if paths[p] {
			return p
		}
	}
	return ""
}

// auditFact writes the audit attempt's fact row (EventGateAudit): ran
// false when res holds the no-verdict finding, its why the bounded
// reason. A failed write is the caller's, and the activity's retry
// writes the row the identity keeps single.
func (r *Runner) auditFact(ctx context.Context, t Task, pr PullRequest, commit string, round int, res AuditResult) error {
	detail := map[string]any{"pr": pr.Number, "commit": commit, "model": aliasOf(res.Model), "ran": true, "verdict": res.Verdict}
	if i := slices.IndexFunc(res.Findings, func(f AuditFinding) bool { return f.Item == auditNoVerdict }); i >= 0 {
		detail["ran"], detail["verdict"], detail["reason"] = false, "", textutil.Truncate(res.Findings[i].Why, maxFactReason)
	}
	return r.timeline.RecordFactRow(ctx, Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Kind: EventGateAudit,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail, IdentityKey: IdentityKey(EventGateAudit, t.ID, commit, strconv.Itoa(round))})
}
