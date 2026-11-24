package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// casesFromEvent writes, inside the transaction tx that inserted event e,
// what e does to its card's cases: an ask opens a case or appends a step
// to it, an answer resolves the case it names or else the card's newest
// live ask, a withdrawal withdraws it, a plan answer resolves its
// question's case, a merge request opens its merge case, and a plan's
// close ends its held ask.
func (s *PGStore) casesFromEvent(ctx context.Context, tx *sql.Tx, e Event) error {
	if e.TaskID == "" {
		return nil
	}
	switch e.Kind {
	case EventQuestionAsked:
		_, err := askCase(ctx, tx, e)
		return err
	case EventQuestionAnswered, EventSessionGranted:
		// a grant is the console's answer at a session cap, as
		// question_answered is the webhook's: one row per answer comment.
		// Act's row names the case it resolved itself.
		comment, _ := toInt(e.Detail["comment"])
		id, named := toInt(e.Detail["case_id"])
		if named {
			if mine, err := taskCase(ctx, tx, id, e.TaskID); !mine {
				return err
			}
		} else {
			ask, err := answeredAsk(ctx, tx, e.TaskID, int64(comment))
			if ask == 0 {
				return err
			}
			id = int(ask)
		}
		answer, _ := e.Detail["answer"].(string)
		action, _ := e.Detail["action"].(string)
		// a reply alone starts none: a question_answered row that names
		// no action is the webhook's copy of a comment on the card's
		// issue, so it ends no live ask of a reply-alone kind. The event
		// stands as the recorded comment; a grant is itself the action
		// and refuses nothing.
		if e.Kind == EventQuestionAnswered && action == "" {
			alone, err := replyAloneAsk(ctx, tx, int64(id))
			if err != nil || alone {
				return err
			}
		}
		return caseEnded(resolveCase(ctx, tx, int64(id), CaseResolution{By: e.Actor, Rung: caseRung(e.Actor), Action: cmp.Or(action, CaseAnswer),
			Args: map[string]any{"answer": answer, "comment": comment}}))
	case EventQuestionWithdrawn:
		// a row that names its case withdraws that one, a monitor's beside
		// the card's own ask, else the card's newest ask
		reason, _ := e.Detail["reason"].(string)
		if id, _ := toInt(e.Detail["case_id"]); id != 0 {
			if mine, err := taskCase(ctx, tx, id, e.TaskID); !mine {
				return err
			}
			return caseEnded(withdrawCase(ctx, tx, int64(id), reason))
		}
		c, err := newestAskCase(ctx, tx, e.TaskID)
		if c.ID == 0 {
			return err
		}
		return caseEnded(withdrawCase(ctx, tx, c.ID, reason))
	case EventPlanQuestionAnswered:
		return resolvePlanCase(ctx, tx, e)
	case EventPlanClosed:
		return endHoldCase(ctx, tx, e)
	case EventApprovalRequested:
		id, _ := toInt(e.Detail["approval"])
		_, err := openRequestCase(ctx, tx, int64(id))
		return err
	}
	return nil
}

// WithdrawnRow is the detail of the question_withdrawn row that records
// the factory's close of case c at the human for reason: the case, its
// face's question and, when the reason names a time, the moments said
// holds for it.
func WithdrawnRow(c Case, reason string, said Moments) map[string]any {
	row := map[string]any{"case_id": c.ID, "case_kind": string(c.Kind), "reason": reason}
	face, _ := faceOf(c)
	if q := textutil.Truncate(EscalationQuestion(face), 300); q != "" {
		row["question"] = q
	}
	if m := said.In(reason); len(m) > 0 {
		row["moments"] = m
	}
	return row
}

// askCase opens the case of question_asked event e, or appends a step to
// the live case it asks again, and tells whether it opened one. An ask
// that names its case (the face) puts that case at the human through the
// door with its comment and appends the human's step, while the case is
// live and the asking card's. One that names none, another card's case or
// a case that ended goes by its words, but a plan's face whose case ended
// opens none: its answer ended it. Every case opens open at rung 0, due at
// the row, with no step: it climbs to its face. One the factory answered as
// it asked it (settled) has no wake, for that answer comes in the same
// filing. An ask that reaches the human at its opening (asksAtOnce) has no
// wake either: the chat's escalation, with the agent's filing as rung 2's
// step, for the agent worked the problem and chose to ask, and a kind the
// kind table names an exception go through the door at once.
func askCase(ctx context.Context, tx *sql.Tx, e Event) (bool, error) {
	if id, _ := toInt(e.Detail["case_id"]); id != 0 {
		mine, err := taskCase(ctx, tx, id, e.TaskID)
		if mine && err == nil {
			if err = toHuman(ctx, tx, int64(id)); err == nil {
				comment, _ := toInt(e.Detail["comment"])
				if err := pendFaced(ctx, tx, int64(id), int64(comment)); err != nil {
					return false, err
				}
				step := CaseStep{CaseID: int64(id), Rung: 3, Actor: FactoryDecider, CommentID: int64(comment)}
				if reason, _ := e.Detail["reason"].(string); askedAgain(reason) {
					step.Found = askedAgainFound(reason)
				}
				_, err = appendCaseStep(ctx, tx, step)
				return false, err
			}
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return false, err
		}
		if revision, _ := toInt(e.Detail["revision"]); revision > 0 {
			return false, nil
		}
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, e.TaskID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var head string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(detail->>'head', '') FROM events WHERE task_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1`,
		t.ID, EventGateHead).Scan(&head)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	c, err := CaseFromAsk(t, e, head)
	if err != nil {
		// a card that asks holds a case: an ask with no words is no ask
		return false, err
	}
	comment, _ := toInt(e.Detail["comment"])
	step := CaseStep{Rung: 3, Actor: FactoryDecider, CommentID: int64(comment)}
	reason, _ := e.Detail["reason"].(string)
	healed := askedAgain(reason)
	if healed {
		// a question asked again in other words: its step goes to the
		// card's live ask, whatever that ask's key
		step.Found = askedAgainFound(reason)
		live, err := newestAskCase(ctx, tx, t.ID)
		if err != nil {
			return false, err
		}
		step.CaseID = live.ID
	}
	atOnce := asksAtOnce(e, c.Kind)
	opened, onRungs := false, false
	if step.CaseID == 0 {
		if c.Follows, err = newestEndedCase(ctx, tx, t.ID); err != nil {
			return false, err
		}
		if _, settled := e.Detail["settled"]; !settled && !atOnce {
			c.NextWakeAt = &e.At
		}
		var got Case
		if got, opened, err = openCase(ctx, tx, c); err != nil {
			return false, err
		}
		step.CaseID, onRungs = got.ID, got.Status == CaseOpen && !(opened && atOnce)
		if !opened && !healed {
			step.Found = "asked again: " + c.Problem
		}
	}
	if err := stampAsk(ctx, tx, e, step.CaseID); err != nil || onRungs {
		return opened, err
	}
	if opened && atOnce {
		if e.Via == ViaChat {
			recommendation, _ := e.Detail["recommendation"].(string)
			why, _ := e.Detail["why"].(string)
			if _, err := appendCaseStep(ctx, tx, CaseStep{CaseID: step.CaseID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp, Grounds: recommendation, Found: why}); err != nil {
				return opened, err
			}
		}
		if err := toHuman(ctx, tx, step.CaseID); err != nil {
			return opened, err
		}
	}
	_, err = appendCaseStep(ctx, tx, step)
	return opened, err
}

// asksAtOnce says ask row e, whose case is of kind k, reaches the human at
// its opening: the chat's escalation, whose agent worked the problem and
// chose to ask, and a kind the kind table names an exception.
func asksAtOnce(e Event, k CaseKind) bool {
	return e.Via == ViaChat || kindRow(k).Exception != ""
}

// HumanCardKind is the kind of an ask's case of source-given kind on card t:
// a human card's ask is the card's own whatever its asker, so the chat's
// escalation is a human card and not an agent's, and the month cap's card
// is the factory's month cap, not an agent card's stop at it; every other
// kind a source gives stands.
func HumanCardKind(kind CaseKind, t Task) CaseKind {
	switch {
	case t.Kind != "human":
		return kind
	case kind == CaseOther || kind == CaseAgentEscalate:
		return CaseHumanCard
	case kind == CaseMonthCap:
		return CaseFactoryMonthCap
	}
	return kind
}

// stampAsk makes the ask row of event e name the case it went to, for the
// face to find it by, when it names another case or none (a case that ended,
// or never was).
func stampAsk(ctx context.Context, tx *sql.Tx, e Event, caseID int64) error {
	stamp := jsonStamp(caseID)
	if named, ok := e.Detail["case_id"]; ok {
		if id, _ := toInt(named); int64(id) == caseID {
			return nil
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE events SET detail = detail || $2::jsonb WHERE id = $1`, e.ID, stamp)
	return err
}

// jsonStamp is the detail that names case id.
func jsonStamp(id int64) string { return fmt.Sprintf(`{"case_id": %d}`, id) }

// cardMoneyKinds are the cases whose answer pays from the card's own
// budget.
var cardMoneyKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor, CaseCheckinShare, CaseCheckinFailed}

// caseFromAsk maps question_asked event e of task t to the case it opens
// or appends to, open; head is the task's newest gate head, "" for none. A
// plan question's case keys on its words and its revision, which its answer
// finds it by: a later revision asking the same words opens its own.
func CaseFromAsk(t Task, e Event, head string) (Case, error) {
	ask := askSentence(e)
	if strings.TrimSpace(ask) == "" {
		return Case{}, fmt.Errorf("task %s: question_asked names no ask", t.ID)
	}
	str := func(k string) string { s, _ := e.Detail[k].(string); return s }
	revision, _ := toInt(e.Detail["revision"])
	facts := map[string]any{}
	maps.Copy(facts, e.Detail)
	var kind CaseKind
	switch {
	case revision > 0 && str("kind") == QuestionConfirmation:
		kind, head = CasePlanConfirmation, planHead(revision)
	case revision > 0:
		kind, head = CasePlanQuestion, planHead(revision)
	default:
		var named map[string]any
		kind, named = CaseKindOf(str("source"), cmp.Or(str("reason"), ask))
		maps.Copy(facts, named)
		kind = HumanCardKind(kind, t)
	}
	c := Case{TaskID: t.ID, Repository: t.Repository, Kind: kind, StateKey: caseStateKey(kind, ask, head), Status: CaseOpen, Problem: ask, Facts: facts}
	if slices.Contains(cardMoneyKinds, kind) {
		c.Money = &CaseMoney{Kind: string(kind), BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR}
	}
	return c, nil
}

// caseSourceKinds are the case kinds an ask's source names on its own.
var caseSourceKinds = map[string]CaseKind{
	"budget": CaseBudgetFloor, SourceMonthCap: CaseMonthCap, "project_cap": CaseProjectCap, "day_cap": CaseDayCap,
	"provider": CaseProviderRefused, "queue": CaseGateStop, "capacity": CasePodNeverStarted, "no_answer": CaseNoAnswer,
	"content_filter": CaseContentFilter, "image": CaseImageWait, "agent": CaseAgentEscalate, SourceChat: CaseAgentEscalate, "author": CaseAgentRequestInfo,
	"stale": CaseWaitExpired, depFailedSource: CaseDependencyFailed,
	SourceChatBudget: CaseJudgmentBudget, SourceProductionHeld: CaseRollHeld, SourceConfig: CaseConfigRefused, SourceStillRed: CaseRedMain,
	SourceFreezeCard: CaseProviderFreeze, "platform": CasePlatformFault, SourcePlatformDeaths: CasePlatformDeaths,
}

// CaseKindOf maps the ask's source and reason prefixes to a CaseKind,
// with the facts the reason names: a check-in's reason kind, a stopped
// gate.
func CaseKindOf(source, reason string) (CaseKind, map[string]any) {
	switch source {
	case "cap":
		if AtTimeStop(reason) {
			return CaseAgentTimeCap, nil
		}
		return CaseSessionCap, nil
	case "supervisor":
		switch {
		case failedLabelOf(reason) != "":
			return CaseCheckinFailed, map[string]any{"reason_kind": failedLabelOf(reason)}
		case strings.Contains(reason, DeadlockAskMark):
			return CaseFindingsRepeated, nil
		case strings.HasPrefix(reason, CheckinSpendAsk):
			return CaseCheckinShare, nil
		case strings.HasPrefix(reason, "check-in: "):
			kind, _, _ := strings.Cut(strings.TrimPrefix(reason, "check-in: "), ":")
			if kind == CheckPolicyClass {
				return CasePolicyClass, map[string]any{"reason_kind": kind}
			}
			return CaseCheckinHuman, map[string]any{"reason_kind": kind}
		case strings.HasPrefix(reason, GateStopped):
			gate, _, _ := strings.Cut(strings.TrimPrefix(reason, GateStopped), " ")
			return CaseGateStop, map[string]any{"gate": gate}
		case strings.HasPrefix(reason, screeningAsks):
			return CaseScreeningSuspect, nil
		case strings.HasPrefix(reason, "dependency #"):
			return CaseDependencyClosed, nil
		}
	}
	if kind, ok := caseSourceKinds[source]; ok {
		return kind, nil
	}
	return CaseOther, nil
}

// failedLabelOf is the failure kind of a failed check-in's ask: its own
// label, or for a stop recorded before the labels the cap its text names
// or an unreadable answer, "" for any other ask.
func failedLabelOf(reason string) string {
	for _, k := range []string{CheckTimeCap, CheckCallCap, CheckUnreadable} {
		if strings.HasPrefix(reason, "check-in: "+k+":") {
			return k
		}
	}
	switch {
	case strings.Contains(reason, DeadlockAskMark):
		return ""
	case checkinCapStopOf(reason) == checkinTimeCapStop:
		return CheckTimeCap
	case checkinCapStopOf(reason) == checkinCallCapStop:
		return CheckCallCap
	case strings.HasPrefix(reason, "check-in: ") && strings.Contains(reason, ": check-in answer unreadable: "):
		return CheckUnreadable
	}
	return ""
}

// caseStateKey is sha256(kind + "|" + normalised ask)[:16], plus "@" +
// head when a gate_head fact or a plan revision names one.
func caseStateKey(kind CaseKind, ask, head string) string {
	sum := sha256.Sum256([]byte(string(kind) + "|" + strings.Join(strings.Fields(ask), " ")))
	key := hex.EncodeToString(sum[:])[:16]
	if head != "" {
		key += "@" + head
	}
	return key
}

// planHead is the head of a plan question's state key: its revision.
func planHead(revision int) string { return fmt.Sprintf("r%d", revision) }

// resolvePlanCase resolves the live case of the plan question
// plan_question_answered event e answers, by who answered it as the
// question stores it: the row's actor in a wake is the venue's human. A
// question with no answer stored resolves nothing.
func resolvePlanCase(ctx context.Context, q rowQuerier, e Event) error {
	var text, kind, by string
	revision, _ := toInt(e.Detail["revision"])
	index, _ := toInt(e.Detail["index"])
	err := q.QueryRowContext(ctx, `SELECT text, COALESCE(kind, ''), COALESCE(answered_by, '') FROM plan_questions WHERE task_id = $1 AND revision = $2 AND idx = $3`,
		e.TaskID, revision, index).Scan(&text, &kind, &by)
	if errors.Is(err, sql.ErrNoRows) || err == nil && by == "" {
		return nil
	}
	if err != nil {
		return err
	}
	ck := CasePlanQuestion
	if kind == QuestionConfirmation {
		ck = CasePlanConfirmation
	}
	var id int64
	err = q.QueryRowContext(ctx, `SELECT id FROM cases WHERE task_id = $1 AND kind = $2 AND state_key = $3 AND `+caseLive,
		e.TaskID, ck, caseStateKey(ck, text, planHead(revision))).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	answer, _ := e.Detail["answer"].(string)
	args := map[string]any{"answer": answer}
	if decision, _ := e.Detail["decision"].(string); decision != "" {
		args["decision"] = decision
	}
	return caseEnded(resolveCase(ctx, q, id, CaseResolution{By: by, Rung: caseRung(by), Action: "answer", Args: args}))
}

// RequestCase is the case pending approval request a opens, open at rung 0
// and due at the request's opening: a merge request's merge case, its pull
// request at the head the gates passed, or a plan's criteria request's
// plan_criteria case, with the words its face posts. A resource request's
// holds the request's spec at the human and opens no row.
func RequestCase(a ApprovalRequest) (Case, error) {
	c := Case{TaskID: a.TaskID, Repository: a.Project, Status: CaseOpen, NextWakeAt: &a.CreatedAt}
	if a.Kind == ApprovalMerge {
		var spec MergeSpec
		if err := json.Unmarshal(a.Spec, &spec); err != nil {
			return Case{}, err
		}
		checks := "green"
		if spec.RedChecks {
			checks = "red"
		}
		c.Kind = CaseMergeApproval
		c.StateKey = fmt.Sprintf("pr:%d@%s", a.PRNumber, spec.HeadSHA)
		c.Problem = fmt.Sprintf("Pull request #%d waits for a merge approval", a.PRNumber)
		c.Facts = map[string]any{"approval": a.ID, "pr": a.PRNumber, "head": spec.HeadSHA, "class": spec.RiskClass, "policy_version": spec.PolicyVersion,
			"factory": spec.Factory, "red_checks": spec.RedChecks, "pr_open": true, "checks": checks,
			"review_approved": spec.Review.Verdict == "approve" && spec.Review.Note == "", "review_findings": len(spec.Review.Findings)}
		if spec.Review.Note == "" {
			c.Facts["review_verdict"] = spec.Review.Verdict
		}
		if spec.Audit != nil && len(spec.Audit.Findings) > 0 {
			c.Facts["audit_findings"] = len(spec.Audit.Findings)
		}
		var failed []string
		for _, t := range spec.FailedTests {
			if name := cmp.Or(t.Test, t.Job); name != "" && !slices.Contains(failed, name) && len(failed) < 3 {
				failed = append(failed, name)
			}
		}
		if len(failed) > 0 {
			c.Facts["failed_checks"] = failed
		}
		if spec.Review.Note != "" {
			_, why := namedReview("", spec.WhyHuman, spec.Review)
			c.Facts["review_not_run"] = why[0]
		}
		if len(spec.HumanOpen) > 0 {
			c = OnOpen(c, spec.HumanOpen, nil, 0)
		}
		return c, nil
	}
	if a.Kind == ApprovalResource {
		// a resource request opens no case: its face is the request's own
		c.Status, c.NextWakeAt = CaseAtHuman, nil
		c.StateKey, c.Problem, c.Facts = fmt.Sprintf("%s:%d", a.Kind, a.ID), a.Reason, map[string]any{}
		if err := json.Unmarshal(a.Spec, &c.Facts); err != nil {
			return Case{}, err
		}
		c.Facts["approval"], c.Facts["approval_kind"] = a.ID, a.Kind
		return c, nil
	}
	c.Kind = CasePlanCriteria
	c.StateKey = fmt.Sprintf("%s:%d", a.Kind, a.ID)
	c.Problem = cmp.Or(a.Reason, "The plan's criteria wait for a verdict")
	c.Facts = map[string]any{"approval": a.ID, "approval_kind": a.Kind}
	var spec CriteriaSpec
	_ = json.Unmarshal(a.Spec, &spec)
	if spec.Face != "" {
		c.Facts["face"] = spec.Face
	}
	if spec.NotRun != "" {
		c.Facts["not_run"] = spec.NotRun
	}
	if len(spec.Runnable) > 0 {
		c.Facts["runnable"] = spec.Runnable
	}
	if len(spec.Open) > 0 {
		c = OnOpen(c, spec.Open, spec.Unchecked, spec.Of)
	}
	return c, nil
}

// RequestRow is the approval_requested row of request a: its case's facts
// (RequestCase) and its kind.
func RequestRow(a ApprovalRequest) (map[string]any, error) {
	c, err := RequestCase(a)
	if err != nil {
		return nil, err
	}
	c.Facts["kind"] = a.Kind
	return c.Facts, nil
}

// openRequestCase opens the case of approval request id while it is
// pending, as RequestCase makes it. It tells whether it opened one.
func openRequestCase(ctx context.Context, q rowQuerier, id int64) (bool, error) {
	a := ApprovalRequest{ID: id}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT kind, task_id, project, COALESCE(pr_number, 0), spec, reason, created_at FROM approval_requests
		WHERE id = $1 AND kind IN ($2, $3, $4) AND status = 'pending'`, id, ApprovalMerge, ApprovalCriteria, ApprovalCriteriaRerun).
		Scan(&a.Kind, &a.TaskID, &a.Project, &a.PRNumber, &raw, &a.Reason, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a.Spec = raw
	c, err := RequestCase(a)
	if err != nil {
		return false, err
	}
	if c.Follows, err = newestEndedCase(ctx, q, c.TaskID); err != nil {
		return false, err
	}
	_, opened, err := openCase(ctx, q, c)
	return opened, err
}

// reopenMergeCase puts the merge case that request id's decision ended
// back open at rung 0, due now, its end, its wake events and its brief
// cleared, so it climbs again at once. The undo, "approval undone:" and
// reason, is a step of rung 0 handed up, and the found of each rung 1 and
// rung 2 step the decision ended, which the face then reads as that rung's
// hand-up. With no such case, or a live one of its key, it opens the
// request's case as openRequestCase does.
func reopenMergeCase(ctx context.Context, q rowQuerier, id int64, reason string) error {
	found := "approval undone: " + reason
	var caseID int64
	// the decision ended the rungs' steps resolved with no decision of their own
	err := q.QueryRowContext(ctx, `WITH reopened AS (
		    UPDATE cases c SET status = $3, resolution = NULL, resolved_at = NULL,
		        owner_rung = 0, wake_on = '{}', next_wake_at = now(), brief = '', brief_detail = '', brief_error = ''
		    WHERE c.id = (SELECT max(id) FROM cases WHERE kind = $2 AND (facts->>'approval')::bigint = $1) AND c.status NOT IN `+caseLiveStatuses+`
		    AND NOT EXISTS (SELECT 1 FROM cases o WHERE o.task_id = c.task_id AND o.kind = c.kind AND o.state_key = c.state_key
		        AND o.status IN `+caseLiveStatuses+`)
		    RETURNING c.id
		), steps AS (
		    UPDATE case_steps SET outcome = $4, found = $5
		    WHERE case_id IN (SELECT id FROM reopened) AND rung IN (1, 2) AND outcome = $6 AND decision = ''
		)
		SELECT id FROM reopened`, id, CaseMergeApproval, CaseOpen, StepHandedUp, found, StepResolved).Scan(&caseID)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = openRequestCase(ctx, q, id)
		return err
	}
	if err != nil {
		return err
	}
	_, err = appendCaseStep(ctx, q, CaseStep{CaseID: caseID, Rung: 0, Actor: FactoryDecider, Found: found, Outcome: StepHandedUp})
	return err
}

// requestCaseKinds are the cases the decision of an approval request ends,
// by the request's kind.
var requestCaseKinds = map[string]CaseKind{ApprovalMerge: CaseMergeApproval, ApprovalCriteria: CasePlanCriteria, ApprovalCriteriaRerun: CasePlanCriteria}

// endRequestCase ends task taskID's live case of approval request kind by
// the request's decision: status approved or denied, by whom, with note. A
// plan's request denied by its decider (request_changes) ends every live
// case of the plan's questions with that decider's revise.
func endRequestCase(ctx context.Context, q rowQuerier, taskID, kind, status, by, note string) error {
	if kind == ApprovalPlan && status == "denied" {
		return revisePlanCases(ctx, q, taskID, by, note)
	}
	ck, ok := requestCaseKinds[kind]
	if !ok {
		return nil
	}
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM cases WHERE task_id = $1 AND kind = $2 AND `+caseLive+` ORDER BY id DESC LIMIT 1`,
		taskID, ck).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = decideRequestCase(ctx, q, id, ck, status, by, note)
	return caseEnded(err)
}

// endHoldCase ends the live held ask (CasePlanHeld) of the plan that
// plan_closed row e closes, whoever closed it: a human's Close is the
// ask's answer, their drop of the open promises with the close's words; the
// factory's close found every promise settled, which withdraws it.
func endHoldCase(ctx context.Context, q rowQuerier, e Event) error {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM cases WHERE task_id = $1 AND kind = $2 AND `+caseLive+` ORDER BY id DESC LIMIT 1`, e.TaskID, CasePlanHeld).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.Actor == FactoryDecider {
		return caseEnded(withdrawCase(ctx, q, id, HeldPlanSettled))
	}
	words, _ := e.Detail["words"].(string)
	return caseEnded(resolveCase(ctx, q, id, CaseResolution{By: e.Actor, Rung: caseRung(e.Actor), Action: AnswerDropAndEnd, Reason: words}))
}

// HeldPlanSettled is why the factory's close of a held plan withdraws its
// ask: no promise was left open.
const HeldPlanSettled = "every promise of the plan is settled, and the plan closed"

// revisePlanCases ends plan taskID's live question and confirmation cases
// by by's request for changes, note: resolved with by's revise, or
// withdrawn with note when the factory denied the request.
func revisePlanCases(ctx context.Context, q rowQuerier, taskID, by, note string) error {
	var ids []int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(array_agg(id ORDER BY id), '{}') FROM cases WHERE task_id = $1 AND kind IN ($2, $3) AND `+caseLive,
		taskID, CasePlanQuestion, CasePlanConfirmation).Scan(pgtype.NewMap().SQLScanner(&ids)); err != nil {
		return err
	}
	for _, id := range ids {
		var err error
		if by == FactoryDecider {
			err = withdrawCase(ctx, q, id, note)
		} else {
			err = resolveCase(ctx, q, id, CaseResolution{By: by, Rung: caseRung(by), Action: CaseAnswer,
				Args: map[string]any{"answer": note, "decision": DecisionRevise}})
		}
		if err := caseEnded(err); err != nil {
			return err
		}
	}
	return nil
}

// decideRequestCase ends live case id of kind by its request's decision and
// answers how it ended. The factory's denial (a superseded head, an ended
// card, a queue left) decides nothing: it withdraws the case with note.
func decideRequestCase(ctx context.Context, q rowQuerier, id int64, kind CaseKind, status, by, note string) (string, error) {
	if by == FactoryDecider && status == "denied" {
		return CaseWithdrawn, withdrawCase(ctx, q, id, note)
	}
	action := ActAnswerCriteria
	if kind == CaseMergeApproval {
		action = ActDenyMerge
		if status == "approved" {
			action = ActApproveMerge
		}
	}
	return CaseResolved, resolveCase(ctx, q, id, CaseResolution{By: by, Rung: caseRung(by), Action: action, Reason: note})
}

// ownAnswerKinds are the cases an answer on the card does not end: a
// merge or a plan's criteria, which their request's decision ends, a plan question, which its own
// answer ends, and a monitor flag, which sits beside the card's run and is
// answered by its id. newestAskCase leaves them out.
var ownAnswerKinds = []CaseKind{CaseMergeApproval, CasePlanCriteria, CasePlanHeld, CasePlanQuestion, CasePlanConfirmation, CaseMonitorFlag}

// ownAnswerKindsSQL is ownAnswerKinds as an SQL list.
var ownAnswerKindsSQL = func() string {
	names := make([]string, len(ownAnswerKinds))
	for i, k := range ownAnswerKinds {
		names[i] = string(k)
	}
	return "('" + strings.Join(names, "', '") + "')"
}()

// AnsweredOnCard reports whether an answer on the card ends a case of
// kind k: every kind but ownAnswerKinds, whose own answers end them.
func AnsweredOnCard(k CaseKind) bool { return !slices.Contains(ownAnswerKinds, k) }

// NewestAskCase is task taskID's newest live case an answer on the card
// ends, the zero Case for none (newestAskCase): the one read the trips,
// Act and the console find a card's live ask by.
func (s *PGStore) NewestAskCase(ctx context.Context, taskID string) (Case, error) {
	return newestAskCase(ctx, s.db, taskID)
}

// newestAskCase is task taskID's newest live case an answer on the card
// ends, the zero Case for none: not one of ownAnswerKinds.
func newestAskCase(ctx context.Context, q rowQuerier, taskID string) (Case, error) {
	c, err := scanCase(q.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE task_id = $1 AND `+caseLive+` AND NOT kind = ANY($2::text[])
		ORDER BY id DESC LIMIT 1`, taskID, kindNames(ownAnswerKinds)))
	if errors.Is(err, sql.ErrNoRows) {
		return Case{}, nil
	}
	return c, err
}

// taskCase reports whether case id is task taskID's: a row ends its own
// card's cases only.
func taskCase(ctx context.Context, q rowQuerier, id int, taskID string) (bool, error) {
	var mine bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM cases WHERE id = $1 AND task_id = $2)`, id, taskID).Scan(&mine)
	return mine, err
}

// answeredAsk is the case the answer comment comment (0 for none) of task
// taskID ends when its row names none: the newest live ask, or none when
// the comment answered a case already, as when Act resolved its case
// before the webhook's copy of the comment came.
func answeredAsk(ctx context.Context, q rowQuerier, taskID string, comment int64) (int64, error) {
	if comment != 0 {
		var answered bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM cases WHERE task_id = $1 AND NOT (`+caseLive+`)
			AND resolution->'args'->>'comment' = $2::bigint::text)`, taskID, comment).Scan(&answered); err != nil || answered {
			return 0, err
		}
	}
	c, err := newestAskCase(ctx, q, taskID)
	return c.ID, err
}

// replyAloneAsk reports whether case id is a live ask of a reply-alone
// kind: the ones an answer row that names no action ends never, for a
// reply alone starts none of them.
func replyAloneAsk(ctx context.Context, q rowQuerier, id int64) (bool, error) {
	var kind CaseKind
	err := q.QueryRowContext(ctx, `SELECT kind FROM cases WHERE id = $1`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return slices.Contains(replyAloneKinds, kind), err
}

// newestEndedCase is the id of task taskID's newest resolved or withdrawn
// case, the one a new case follows; 0 for none.
func newestEndedCase(ctx context.Context, q rowQuerier, taskID string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(max(id), 0) FROM cases WHERE task_id = $1 AND NOT (`+caseLive+`)`, taskID).Scan(&id)
	return id, err
}

// caseRung is the rung of who ended a case: factoryd's code is rung 0,
// the project agent rung 2, a person rung 3.
func caseRung(by string) int {
	switch by {
	case FactoryDecider:
		return 0
	case ChatUser:
		return 2
	}
	return 3
}

// pendFaced writes the case_faced of case id's face at comment as pending,
// in the transaction that gives the case that comment, for a card whose run
// hears the case (runHears) and waits on its face: none for a case beside
// the run (caseBesideRun).
func pendFaced(ctx context.Context, tx *sql.Tx, id, comment int64) error {
	if comment == 0 {
		return nil
	}
	c, err := scanCase(tx.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = $1`, id))
	if err != nil {
		return err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, c.TaskID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || !runHears(t, c) || caseBesideRun(c, t) {
		return err
	}
	return pendCaseSignal(ctx, tx, id, t.ID, SignalCaseFaced, facedSignal(c, comment))
}

// caseEnded drops ErrNotFound from err: the case had ended already.
func caseEnded(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
