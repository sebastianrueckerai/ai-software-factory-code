package factoryd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// The receipt of a merged task (roadmap 66): the deterministic facts of
// one merge, which anyone can check again from the task's records, kept
// apart from the model's assessments, which carry their own label. One
// receipt per task, and the first write stands: a merge path that runs
// again changes nothing. The document is versioned, for the compliance
// evidence export (roadmap 328) reads it by that version.

// MergeReceiptVersion is the receipt document's format version. Version 2
// moved the task inside facts and named the diff's hash diff_sha256.
const MergeReceiptVersion = 2

// MergeReceipt is one merged task's receipt: its deterministic facts,
// every one derived from the task's records, and beside them, labelled
// by source, the model's assessments (evidence, never authority).
type MergeReceipt struct {
	Version int `json:"version"`
	// Facts are the deterministic facts of the merge, one nested
	// document of declared fields.
	Facts       ReceiptFacts        `json:"facts"`
	Assessments []ReceiptAssessment `json:"assessments,omitempty"`
}

// ReceiptFacts are the receipt's facts: every field derived again from
// the task's records.
type ReceiptFacts struct {
	TaskID     string `json:"task"`
	Repository string `json:"repository"`
	Issue      int64  `json:"issue"`
	PR         int64  `json:"pr"`
	// Base is the commit the branch was cut from (the task's base SHA),
	// Head the head the merge merged, and MergeCommit the commit the
	// merge made on the base.
	Base        string `json:"base"`
	Head        string `json:"head"`
	MergeCommit string `json:"merge_commit,omitempty"`
	// DiffSHA256 is sha256 over the pull request's unified diff; "" for
	// a diff past the most factoryd reads (PolicyGate.Unread).
	DiffSHA256 string `json:"diff_sha256,omitempty"`
	// PolicyVersion is the policy version the policy gate classified the
	// diff under, the newest of its runs.
	PolicyVersion int64 `json:"policy_version,omitempty"`
	// Gates holds every gate run the timeline recorded, oldest first:
	// the command it ran and that command's exit code where it ran one,
	// and its verdict.
	Gates []ReceiptGate `json:"gates,omitempty"`
	// CostEUR is the task's booked spend (the settled ledger sum) at the
	// merge.
	CostEUR float64 `json:"cost_eur"`
	// Decisions are the human decisions on the task's road to its merge,
	// oldest first.
	Decisions []ReceiptDecision `json:"decisions,omitempty"`
}

// ReceiptGate is one gate run's facts in the receipt.
type ReceiptGate struct {
	Gate string `json:"gate"`
	// Command is the command the gate ran (the reproduction's); "" when
	// it ran none.
	Command string `json:"command,omitempty"`
	// Exit is the command's exit code at the head; nil when the gate ran
	// no command.
	Exit *int `json:"exit,omitempty"`
	// Verdict is the gate's own outcome word.
	Verdict string `json:"verdict"`
}

// ReceiptDecision is one human decision on the task's road to its merge:
// an approval request a person decided, or a case the human (rung 3)
// answered.
type ReceiptDecision struct {
	Kind     string `json:"kind"`
	Decision string `json:"decision"`
	// Draw is what an answer drew when it drew: its sessions, agent time
	// or money, in the factory's own words.
	Draw string     `json:"draw,omitempty"`
	By   string     `json:"by"`
	Note string     `json:"note,omitempty"`
	At   *time.Time `json:"at,omitempty"`
}

// ReceiptAssessment is one model assessment the receipt carries beside
// the facts, labelled by its source.
type ReceiptAssessment struct {
	Source string `json:"source"` // review, audit, judgement
	// Criterion is the criterion a judgement assessment judged; "" for a
	// review's or an audit's.
	Criterion string `json:"criterion,omitempty"`
	Verdict   string `json:"verdict"`
	// Note is why a review gave no verdict, Evidence a judgement's
	// evidence.
	Note     string `json:"note,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// BuildMergeReceipt builds the task's receipt from its records: the task
// row (the base SHA, the cost), the pull request (the head, the merge
// commit, the diff), the timeline's gate steps (the policy version,
// every gate's command, exit code and verdict), the decided approval
// requests and the cases the human answered on rung 3 (every human
// decision) and the verdict rows (the judgement criteria's assessments).
// It reads, never writes: the merge path stores what it builds
// (PutMergeReceipt).
func (r *Runner) BuildMergeReceipt(ctx context.Context, taskID string, pr int64) (MergeReceipt, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return MergeReceipt{}, err
	}
	st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return MergeReceipt{}, err
	}
	rec := MergeReceipt{Version: MergeReceiptVersion, Facts: ReceiptFacts{TaskID: t.ID, Repository: t.Repository,
		Issue: t.IssueNumber, PR: pr, Base: t.SHA, Head: st.HeadSHA, MergeCommit: st.MergeCommit, CostEUR: t.SpentEUR}}
	files, err := r.forgejo.PullRequestFileDiffs(ctx, repo.Owner, repo.Name, pr)
	switch {
	case errors.Is(err, ErrDiffTooLong):
	case err != nil:
		return MergeReceipt{}, err
	default:
		rec.Facts.DiffSHA256 = diffHash(files)
	}
	receiptTimeline(ctx, r.timeline, taskID, &rec)
	// every human decision: the task's decided approval requests, a
	// person's, oldest first, and the cases the human answered on rung 3
	// (the typed action, what it drew, the note). A case the merge
	// approval ended is that approval's own record, so its row adds no
	// second decision, and a lower rung's answer — the factory's, the
	// agent's, the agent's for a member over the chat — is no human one,
	// whoever's name it carries.
	var decided []ApprovalRequest
	for _, status := range []string{"approved", "denied"} {
		as, err := r.store.ApprovalRequests(ctx, status)
		if err != nil {
			return MergeReceipt{}, err
		}
		for _, a := range as {
			if a.TaskID == taskID && isPerson(a.DecidedBy) {
				decided = append(decided, a)
			}
		}
	}
	slices.SortStableFunc(decided, func(a, b ApprovalRequest) int { return int(a.ID - b.ID) })
	for _, a := range decided {
		rec.Facts.Decisions = append(rec.Facts.Decisions, ReceiptDecision{Kind: a.Kind, Decision: a.Status, By: a.DecidedBy,
			Note: a.DecisionNote, At: a.DecidedAt})
	}
	cases, err := r.store.CasesOf(ctx, taskID)
	if err != nil {
		return MergeReceipt{}, err
	}
	for _, c := range cases {
		res := c.Resolution
		if res == nil || res.Rung != 3 || !isPerson(res.By) || c.Kind == CaseMergeApproval {
			continue
		}
		d, _ := Resolution{Action: res.Action, Args: res.Args}.draw()
		rec.Facts.Decisions = append(rec.Facts.Decisions, ReceiptDecision{Kind: "case", Decision: res.Action, By: res.By,
			Draw: drawWords(d), Note: res.Reason, At: c.ResolvedAt})
	}
	slices.SortStableFunc(rec.Facts.Decisions, func(a, b ReceiptDecision) int { return receiptWhen(a).Compare(receiptWhen(b)) })
	// the judgement criteria's verdicts: the model's assessments on the
	// task's rows (an integration card's own, a card of a plan without
	// them has none)
	rows, err := r.store.CriteriaVerdicts(ctx, taskID)
	if err != nil {
		return MergeReceipt{}, err
	}
	for _, v := range rows {
		if v.Source != CriteriaSourceJudgement {
			continue
		}
		rec.Assessments = append(rec.Assessments, ReceiptAssessment{Source: v.Source, Criterion: v.Criterion,
			Verdict: v.Verdict, Evidence: v.Evidence})
	}
	return rec, nil
}

// RecordMergeReceipt builds the task's receipt from its records and
// stores it (PutMergeReceipt), the first write standing: a merge path
// that runs again — the merge queue, the merge webhook, the finish heal
// — writes nothing over the receipt it already wrote. Intake's
// MergeReceipt callback is this.
func (r *Runner) RecordMergeReceipt(ctx context.Context, taskID string, pr int64) error {
	_, err := r.storeMergeReceipt(ctx, taskID, pr)
	return err
}

// storeMergeReceipt builds the task's receipt and stores it, reporting
// whether this call stored it: the first write stands, so a later call
// stores nothing and answers false.
func (r *Runner) storeMergeReceipt(ctx context.Context, taskID string, pr int64) (bool, error) {
	rec, err := r.BuildMergeReceipt(ctx, taskID, pr)
	if err != nil {
		return false, err
	}
	return r.store.PutMergeReceipt(ctx, taskID, rec)
}

// recordReceipt records the task's merge receipt, posts it as the pull
// request's one receipt comment on the run that first wrote it, and
// never fails its merge path: the merge stands whatever the receipt's
// build, write or post does, and the first write stands, so a path that
// runs again writes nothing over the receipt it already wrote and posts
// nothing either.
func (r *Runner) recordReceipt(ctx context.Context, taskID string, pr int64) {
	stored, err := r.storeMergeReceipt(ctx, taskID, pr)
	if err != nil {
		log.Printf("task %s: record the merge receipt: %v", taskID, err)
		return
	}
	if !stored {
		return
	}
	if err := r.postReceiptComment(ctx, taskID, pr); err != nil {
		log.Printf("task %s: post the receipt comment: %v", taskID, err)
	}
}

// isPerson tells whether by is a person: the factory's own decisions
// (factoryd, the check-in, the project agent, the merge bot) and the merge
// rule's (the gate's, the chat's, the auto approver's) are facts of the
// gates, not decisions.
func isPerson(by string) bool {
	return by != "" && by != FactoryDecider && by != CheckinDecider && by != AgentDecider && by != AutoApprover && by != ChatUser && by != MergeUser
}

// receiptMarker marks the receipt's comment on the pull request.
func receiptMarker(taskID string) string {
	return fmt.Sprintf("<!-- factoryd receipt %s -->", taskID)
}

// receiptComment renders the receipt as its comment's body: the facts
// first, one plain line each — every fact anyone can check again from
// the task's records — and the model's assessments apart, in their own
// section, every line labelled by its source.
func receiptComment(rec MergeReceipt) string {
	f := rec.Facts
	var b strings.Builder
	fmt.Fprintf(&b, "Merge receipt of task %s in %s (issue #%d, pull request #%d), document version %d.",
		f.TaskID, f.Repository, f.Issue, f.PR, rec.Version)
	fmt.Fprintf(&b, "\nBase: %s", f.Base)
	fmt.Fprintf(&b, "\nHead: %s", f.Head)
	if f.MergeCommit != "" {
		fmt.Fprintf(&b, "\nMerge commit: %s", f.MergeCommit)
	}
	if f.DiffSHA256 != "" {
		fmt.Fprintf(&b, "\nDiff sha256: %s", f.DiffSHA256)
	}
	if f.PolicyVersion != 0 {
		fmt.Fprintf(&b, "\nPolicy version: %d", f.PolicyVersion)
	}
	for _, g := range f.Gates {
		switch {
		case g.Command == "":
			fmt.Fprintf(&b, "\nGate %s: %s", g.Gate, g.Verdict)
		case g.Exit == nil:
			fmt.Fprintf(&b, "\nGate %s: %s: %s", g.Gate, g.Command, g.Verdict)
		default:
			fmt.Fprintf(&b, "\nGate %s: %s (exit %d): %s", g.Gate, g.Command, *g.Exit, g.Verdict)
		}
	}
	fmt.Fprintf(&b, "\nCost: EUR %.2f", f.CostEUR)
	for _, d := range f.Decisions {
		fmt.Fprintf(&b, "\nDecision: %s %s by %s", d.Kind, d.Decision, d.By)
		if d.Draw != "" {
			fmt.Fprintf(&b, ", drew %s", d.Draw)
		}
		if d.Note != "" {
			fmt.Fprintf(&b, " — %s", d.Note)
		}
	}
	b.WriteString("\n\nAssessments — the model's own, apart from the facts above:")
	for _, a := range rec.Assessments {
		fmt.Fprintf(&b, "\n%s", a.Source)
		if a.Criterion != "" {
			fmt.Fprintf(&b, " (%s)", a.Criterion)
		}
		fmt.Fprintf(&b, ": %s", a.Verdict)
		if a.Note != "" {
			fmt.Fprintf(&b, " — %s", a.Note)
		}
		if a.Evidence != "" {
			fmt.Fprintf(&b, " — %s", a.Evidence)
		}
	}
	return b.String()
}

// postReceiptComment posts the task's stored receipt as its comment on
// the pull request, by the create-or-edit pattern: it edits the bot
// comment its marker finds, else it posts one — a retried post never
// makes two.
func (r *Runner) postReceiptComment(ctx context.Context, taskID string, pr int64) error {
	rec, ok, err := r.store.MergeReceipt(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	_, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	marker := receiptMarker(taskID)
	body := receiptComment(rec) + "\n" + marker
	have, err := r.forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, pr)
	if err != nil {
		return err
	}
	for _, c := range have {
		if c.Author == r.forgejo.BotUser && strings.Contains(c.Body, marker) {
			if c.Body == body {
				return nil
			}
			return r.forgejo.EditIssueComment(ctx, repo.Owner, repo.Name, c.ID, body)
		}
	}
	_, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr, body)
	return err
}

// receiptWhen is decision d's time, the zero time while it names none.
func receiptWhen(d ReceiptDecision) time.Time {
	if d.At == nil {
		return time.Time{}
	}
	return *d.At
}

// receiptTimeline reads the task's timeline into the receipt: the policy
// gate's runs (the policy version, the verdict), every reproduction
// gate's (the command, the exit code, the verdict) and the review's and
// the audit's verdicts (assessments). A timeline that cannot be read
// reads as no steps, as hasEvent reads it.
func receiptTimeline(ctx context.Context, tl *Timeline, taskID string, rec *MergeReceipt) {
	if tl == nil || tl.Objects == nil {
		return
	}
	body, err := tl.Objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return
	}
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e["step"] {
		case "gate_policy":
			if v, ok := receiptNumber(e, "policy_version"); ok {
				rec.Facts.PolicyVersion = int64(v)
			}
			rec.Facts.Gates = append(rec.Facts.Gates, ReceiptGate{Gate: GatePolicy, Verdict: receiptText(e, "outcome")})
		case "gate_reproduction":
			g := ReceiptGate{Gate: GateReproduction, Command: receiptText(e, "command"), Verdict: "not run"}
			if exit, ok := receiptNumber(e, "exit"); ok {
				g.Exit = new(int)
				*g.Exit = int(exit)
				if *g.Exit == 0 {
					g.Verdict = "passed"
				} else {
					g.Verdict = "failed"
				}
			}
			rec.Facts.Gates = append(rec.Facts.Gates, g)
		case "model_review":
			rec.Assessments = append(rec.Assessments, ReceiptAssessment{Source: CriteriaSourceReview,
				Verdict: receiptText(e, "verdict"), Note: receiptText(e, "note")})
		case "audit":
			rec.Assessments = append(rec.Assessments, ReceiptAssessment{Source: "audit", Verdict: receiptText(e, "verdict")})
		}
	}
}

// receiptText is entry's key as text, "" at any other type.
func receiptText(e map[string]any, key string) string {
	s, _ := e[key].(string)
	return s
}

// receiptNumber is entry's key as a number, false at any other type.
func receiptNumber(e map[string]any, key string) (float64, bool) {
	n, ok := e[key].(float64)
	return n, ok
}

// diffHash is sha256 over the pull request's unified diff, its files'
// texts in the order the diff carries them: byte for byte the diff the
// gate read, which anyone can read and hash again.
func diffHash(files []fileDiff) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.Text))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PutMergeReceipt stores the task's merge receipt; the first write
// stands (ON CONFLICT DO NOTHING), so a merge path that runs again — the
// merge queue, the merge webhook — writes nothing over the receipt it
// already wrote. It reports true only when this call stored the receipt.
func (s *PGStore) PutMergeReceipt(ctx context.Context, taskID string, receipt MergeReceipt) (bool, error) {
	b, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO merge_receipts (task_id, document) VALUES ($1, $2)
		ON CONFLICT (task_id) DO NOTHING`, taskID, string(b))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MergeReceipt is the task's stored receipt; false when it has none.
func (s *PGStore) MergeReceipt(ctx context.Context, taskID string) (MergeReceipt, bool, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT document FROM merge_receipts WHERE task_id = $1`, taskID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return MergeReceipt{}, false, nil
	}
	if err != nil {
		return MergeReceipt{}, false, err
	}
	var r MergeReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return MergeReceipt{}, false, err
	}
	return r, true, nil
}
