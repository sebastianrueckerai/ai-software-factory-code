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
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// casesFromEvent writes, inside the transaction tx that inserted event e,
// what e does to its card's cases: an ask opens a case or appends a step
// to it, an answer resolves the case it names or else the card's newest
// live ask, a withdrawal withdraws it, a plan answer resolves its
// question's case, and a merge request opens its merge case.
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
		return caseEnded(resolveCase(ctx, tx, int64(id), CaseResolution{By: e.Actor, Rung: caseRung(e.Actor), Action: cmp.Or(action, caseAnswer),
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
	case EventApprovalRequested:
		id, _ := toInt(e.Detail["approval"])
		_, err := openMergeCase(ctx, tx, int64(id))
		return err
	}
	return nil
}

// askCase opens the case of question_asked event e, or appends a step to
// the live case it asks again, and tells whether it opened one. An ask
// that names its case (OpenTrip's face) puts that case at the human with
// its comment and appends the human's step, while the case is live and
// the asking card's. One that names none, another card's case or a case
// that ended goes by its words.
func askCase(ctx context.Context, tx *sql.Tx, e Event) (bool, error) {
	if id, _ := toInt(e.Detail["case_id"]); id != 0 {
		comment, _ := toInt(e.Detail["comment"])
		err := tx.QueryRowContext(ctx, `UPDATE cases SET status = $3, comment_id = COALESCE(NULLIF($2::bigint, 0), comment_id)
			WHERE id = $1 AND task_id = $4 AND `+caseLive+` RETURNING id`, id, comment, CaseAtHuman, e.TaskID).Scan(&id)
		if err == nil {
			if err := pendFaced(ctx, tx, int64(id), int64(comment)); err != nil {
				return false, err
			}
			step := CaseStep{CaseID: int64(id), Rung: 3, Actor: FactoryDecider}
			if e.Detail["reason"] == healedReason {
				step.Found = "asked again after a restart"
			}
			_, err = appendCaseStep(ctx, tx, step)
			return false, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
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
	c, err := caseFromAsk(t, e, head)
	if err != nil {
		// an ask with no words opens no case: its event stands
		log.Printf("task %s: event %d opens no case: %v", t.ID, e.ID, err)
		return false, nil
	}
	step := CaseStep{Rung: 3, Actor: FactoryDecider}
	healed := e.Detail["reason"] == healedReason
	if healed {
		// a restart asks the question again in other words: its step goes
		// to the card's live ask, whatever that ask's key
		step.Found = "asked again after a restart"
		live, err := newestAskCase(ctx, tx, t.ID)
		if err != nil {
			return false, err
		}
		step.CaseID = live.ID
	}
	opened := false
	if step.CaseID == 0 {
		if c.Follows, err = newestEndedCase(ctx, tx, t.ID); err != nil {
			return false, err
		}
		var got Case
		if got, opened, err = openCase(ctx, tx, c); err != nil {
			return false, err
		}
		step.CaseID = got.ID
		if !opened && !healed {
			step.Found = "asked again: " + c.Problem
		}
	}
	if err := stampAsk(ctx, tx, e, step.CaseID); err != nil {
		return false, err
	}
	_, err = appendCaseStep(ctx, tx, step)
	return opened, err
}

// humanCardKind is the kind of an ask's case of source-given kind on card t:
// a human card's ask is the card's own whatever its asker, so the chat's
// escalation is a human card and not an agent's; every other kind a source
// gives stands.
func humanCardKind(kind CaseKind, t Task) CaseKind {
	if (kind == CaseOther || kind == CaseAgentEscalate) && t.Kind == "human" {
		return CaseHumanCard
	}
	return kind
}

// stampAsk makes every ask row of the case it went to name that case, for
// the face to find it by: the row of event e when it names another case or
// none (a case that ended, or never was), and, for the heal's synthetic
// event, which has no row, the card's newest ask row that names no case,
// the one of its comment when the row has one.
func stampAsk(ctx context.Context, tx *sql.Tx, e Event, caseID int64) error {
	stamp := jsonStamp(caseID)
	if e.ID == 0 {
		comment, _ := toInt(e.Detail["comment"])
		_, err := tx.ExecContext(ctx, `UPDATE events SET detail = detail || $4::jsonb
			WHERE id = (SELECT id FROM events WHERE task_id = $1 AND kind = $2 AND NOT detail ? 'case_id'
				AND (NOT detail ? 'comment' OR detail->>'comment' = $3::text) ORDER BY id DESC LIMIT 1)`,
			e.TaskID, EventQuestionAsked, strconv.Itoa(comment), stamp)
		return err
	}
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
// or appends to; head is the task's newest gate head, "" for none. A plan
// question's case keys on its words and its revision, which its answer
// finds it by: a later revision asking the same words opens its own.
func caseFromAsk(t Task, e Event, head string) (Case, error) {
	ask := askSentence(e)
	if strings.TrimSpace(ask) == "" {
		return Case{}, fmt.Errorf("task %s: question_asked names no ask", t.ID)
	}
	str := func(k string) string { s, _ := e.Detail[k].(string); return s }
	revision, _ := toInt(e.Detail["revision"])
	comment, _ := toInt(e.Detail["comment"])
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
		kind, named = caseKindOf(str("source"), cmp.Or(str("reason"), ask))
		maps.Copy(facts, named)
		kind = humanCardKind(kind, t)
	}
	c := Case{TaskID: t.ID, Repository: t.Repository, Kind: kind, StateKey: caseStateKey(kind, ask, head), Status: CaseAtHuman,
		Problem: ask, Facts: facts, CommentID: int64(comment)}
	if slices.Contains(cardMoneyKinds, kind) {
		c.Money = &CaseMoney{Kind: string(kind), BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR}
	}
	return c, nil
}

// caseSourceKinds are the case kinds an ask's source names on its own.
var caseSourceKinds = map[string]CaseKind{
	"budget": CaseBudgetFloor, SourceMonthCap: CaseMonthCap, "project_cap": CaseProjectCap, "day_cap": CaseDayCap,
	"provider": CaseProviderRefused, "queue": CaseGateStop, "capacity": CasePodNeverStarted, "no_answer": CaseNoAnswer,
	"content_filter": CaseContentFilter, "image": CaseImageWait, "agent": CaseAgentEscalate, SourceChat: CaseAgentEscalate,
	"stale": CaseWaitExpired, SourceDependencyFailed: CaseDependencyFailed,
	SourceChatBudget: CaseJudgmentBudget, SourceProductionHeld: CaseRollHeld, SourceConfig: CaseConfigRefused, SourceStillRed: CaseRedMain,
}

// caseKindOf maps the ask's source and reason prefixes to a CaseKind,
// with the facts the reason names: a check-in's reason kind, a stopped
// gate.
func caseKindOf(source, reason string) (CaseKind, map[string]any) {
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
		case strings.Contains(reason, deadlockAskMark):
			return CaseFindingsRepeated, nil
		case strings.HasPrefix(reason, checkinSpendAsk):
			return CaseCheckinShare, nil
		case strings.HasPrefix(reason, "check-in: "):
			kind, _, _ := strings.Cut(strings.TrimPrefix(reason, "check-in: "), ":")
			if kind == CheckPolicyClass {
				return CasePolicyClass, map[string]any{"reason_kind": kind}
			}
			return CaseCheckinHuman, map[string]any{"reason_kind": kind}
		case strings.HasPrefix(reason, gateStopped):
			gate, _, _ := strings.Cut(strings.TrimPrefix(reason, gateStopped), " ")
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
	case strings.Contains(reason, deadlockAskMark):
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
// plan_question_answered event e answers.
func resolvePlanCase(ctx context.Context, q rowQuerier, e Event) error {
	var text, kind string
	revision, _ := toInt(e.Detail["revision"])
	index, _ := toInt(e.Detail["index"])
	err := q.QueryRowContext(ctx, `SELECT text, COALESCE(kind, '') FROM plan_questions WHERE task_id = $1 AND revision = $2 AND idx = $3`,
		e.TaskID, revision, index).Scan(&text, &kind)
	if errors.Is(err, sql.ErrNoRows) {
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
	return caseEnded(resolveCase(ctx, q, id, CaseResolution{By: e.Actor, Rung: caseRung(e.Actor), Action: "answer", Args: args}))
}

// openMergeCase opens the merge case of approval request id while it is
// a pending merge request: its pull request at the head the gates
// passed. It tells whether it opened one.
func openMergeCase(ctx context.Context, q rowQuerier, id int64) (bool, error) {
	c := Case{Kind: CaseMergeApproval, Status: CaseAtHuman}
	var pr int64
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT task_id, project, COALESCE(pr_number, 0), spec FROM approval_requests
		WHERE id = $1 AND kind = $2 AND status = 'pending'`, id, ApprovalMerge).Scan(&c.TaskID, &c.Repository, &pr, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var spec MergeSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, err
	}
	c.StateKey = fmt.Sprintf("pr:%d@%s", pr, spec.HeadSHA)
	c.Problem = fmt.Sprintf("Pull request #%d waits for a merge approval", pr)
	c.Facts = map[string]any{"approval": id, "pr": pr, "head": spec.HeadSHA, "class": spec.RiskClass,
		"factory": spec.Factory, "red_checks": spec.RedChecks}
	if c.Follows, err = newestEndedCase(ctx, q, c.TaskID); err != nil {
		return false, err
	}
	got, opened, err := openCase(ctx, q, c)
	if err != nil || !opened {
		return false, err
	}
	_, err = appendCaseStep(ctx, q, CaseStep{CaseID: got.ID, Rung: 3, Actor: FactoryDecider})
	return err == nil, err
}

// reopenMergeCase puts the merge case that request id's decision ended
// back at the human, its end and any wake it carried cleared, with a step
// naming reason, the why of the undo; with no such case, or a live one of
// its key, it opens the request's case as openMergeCase does.
func reopenMergeCase(ctx context.Context, q rowQuerier, id int64, reason string) error {
	var caseID int64
	err := q.QueryRowContext(ctx, `UPDATE cases c SET status = $3, resolution = NULL, resolved_at = NULL,
		owner_rung = 3, wake_on = '{}', next_wake_at = NULL
		WHERE c.id = (SELECT max(id) FROM cases WHERE kind = $2 AND (facts->>'approval')::bigint = $1) AND c.status NOT IN `+caseLiveStatuses+`
		AND NOT EXISTS (SELECT 1 FROM cases o WHERE o.task_id = c.task_id AND o.kind = c.kind AND o.state_key = c.state_key
		    AND o.status IN `+caseLiveStatuses+`)
		RETURNING c.id`, id, CaseMergeApproval, CaseAtHuman).Scan(&caseID)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = openMergeCase(ctx, q, id)
		return err
	}
	if err != nil {
		return err
	}
	_, err = appendCaseStep(ctx, q, CaseStep{CaseID: caseID, Rung: 3, Actor: FactoryDecider, Found: "approval undone: " + reason})
	return err
}

// endMergeCase ends task taskID's live merge case by its request's
// decision: status approved or denied, by whom, with note.
func endMergeCase(ctx context.Context, q rowQuerier, taskID, status, by, note string) error {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM cases WHERE task_id = $1 AND kind = $2 AND `+caseLive+` ORDER BY id DESC LIMIT 1`,
		taskID, CaseMergeApproval).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = decideMergeCase(ctx, q, id, status, by, note)
	return caseEnded(err)
}

// decideMergeCase ends live merge case id by its request's decision and
// answers how it ended. The factory's denial (a superseded head, an ended
// card, a queue left) decides no merge: it withdraws the case with note.
func decideMergeCase(ctx context.Context, q rowQuerier, id int64, status, by, note string) (string, error) {
	if by == FactoryDecider && status == "denied" {
		return CaseWithdrawn, withdrawCase(ctx, q, id, note)
	}
	action := "deny"
	if status == "approved" {
		action = "approve"
	}
	return CaseResolved, resolveCase(ctx, q, id, CaseResolution{By: by, Rung: caseRung(by), Action: action, Reason: note})
}

// ownAnswerKinds are the cases an answer on the card does not end: a
// merge, which its request's decision ends, a plan question, which its own
// answer ends, and a monitor flag, which sits beside the card's run and is
// answered by its id. newestAskCase leaves them out.
var ownAnswerKinds = []CaseKind{CaseMergeApproval, CasePlanQuestion, CasePlanConfirmation, CaseMonitorFlag}

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
	kinds := make([]string, len(ownAnswerKinds))
	for i, k := range ownAnswerKinds {
		kinds[i] = string(k)
	}
	c, err := scanCase(q.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE task_id = $1 AND `+caseLive+` AND NOT kind = ANY($2::text[])
		ORDER BY id DESC LIMIT 1`, taskID, kinds))
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
// waits on the case: none for a monitor flag's case, which sits beside the
// run, or a human card's, which runs none.
func pendFaced(ctx context.Context, tx *sql.Tx, id, comment int64) error {
	if comment == 0 {
		return nil
	}
	var taskID string
	err := tx.QueryRowContext(ctx, `SELECT c.task_id FROM cases c JOIN tasks t ON t.id = c.task_id WHERE c.id = $1 AND c.kind <> $2 AND t.kind <> 'human'`,
		id, CaseMonitorFlag).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return pendCaseSignal(ctx, tx, id, taskID, SignalCaseFaced, facedSignal(id, comment))
}

// caseEnded drops ErrNotFound from err: the case had ended already.
func caseEnded(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
