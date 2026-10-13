package console

import (
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// tell_card is the chat's one tell-the-card (roadmap 183): the member's
// text to a card, which the server routes by the card's state — steer for
// a running session, deny_merge with request_changes at the approval gate,
// answer at waiting-human. The agent has no wrong choice to make; steer,
// answer and deny_merge stay as they were.

// tellKey is one row of the routing table: the card's state, as the
// routing reads it (an in-review card with a pending merge request is at
// its approval gate), with whether that request is there.
type tellKey struct {
	status string
	merge  bool
}

// tellState is the card's state as the routing keys it: an in-review
// card with a pending merge request waits at its approval gate, a state
// of its own.
func tellState(status string, merge bool) string {
	if status == "in-review" && merge {
		return "approval-gate"
	}
	return status
}

// tellMeaning is the routing's state in one line, so a refusal says what
// the card does.
func tellMeaning(state string) string {
	if state == "approval-gate" {
		return "waits at its approval gate, where " + factoryd.GateMeaning(factoryd.GateApproval)
	}
	return factoryd.StatusMeaning(state)
}

// tellBranch is that row: what its about says the text does there, the
// event kind its action records (the answer branch records the answer's
// or the grant's own kind), and the existing action it calls.
type tellBranch struct {
	about string
	event string
	run   func(t *turn, in toolInput, k factoryd.Task, q factoryd.ApprovalRequest) (string, error)
}

// tellRoutes maps (status, has-pending-merge) to the action that fits it;
// a state outside the table refuses the card with 409. The run of each
// branch is the existing action's own: steer for a running session,
// deny_merge with request_changes at the approval gate, answer at
// waiting-human; it answers the action's result and records its event.
var tellRoutes = map[tellKey]tellBranch{
	{status: "running"}: {
		about: "its session reads the message",
		event: factoryd.EventCardSteered,
		run: func(t *turn, in toolInput, _ factoryd.Task, _ factoryd.ApprovalRequest) (string, error) {
			return t.c.doSteer(t.r.Context(), t.a, in.TaskID, in.Text)
		}},
	{status: "in-review", merge: true}: {
		about: "the text denies its merge request, changes requested",
		event: factoryd.EventMergeDenied,
		run: func(t *turn, in toolInput, _ factoryd.Task, q factoryd.ApprovalRequest) (string, error) {
			return t.c.doDeny(t.r.Context(), t.a, q.ID, "request_changes", in.Text)
		}},
	{status: "waiting-human"}: {
		about: "the text answers the question it waits on",
		event: factoryd.EventQuestionAnswered,
		run: func(t *turn, in toolInput, k factoryd.Task, _ factoryd.ApprovalRequest) (string, error) {
			return t.tellAnswer(in, k)
		}},
}

// tellFits names the three states the tool routes in.
const tellFits = "a card running a session, at its approval gate or waiting on a human"

// tellGateStatus is the state whose branch the card's pending merge
// request picks; only an in-review card is asked.
func tellGateStatus(status string) bool { return status == "in-review" }

// tellAnswer answers waiting-human card k with in's text: a plan's open
// question through the answer tool's own path, any other question as
// doAnswer posts it.
func (t *turn) tellAnswer(in toolInput, k factoryd.Task) (string, error) {
	ctx := t.r.Context()
	qs, err := t.c.planQuestions(ctx, k)
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(qs, func(q factoryd.PlanQuestion) bool { return q.Answer == "" }) {
		return t.answer(in)
	}
	return t.c.doAnswer(ctx, t.a, in.TaskID, in.Text)
}

// pendingMergeOf is card k's pending merge approval request of the chat's
// project, false when it holds none; a store that cannot be read answers
// the refusal of the branch that needs it.
func (t *turn) pendingMergeOf(k factoryd.Task) (factoryd.ApprovalRequest, bool, error) {
	all, err := t.c.cfg.Approvals.ApprovalRequests(t.r.Context(), "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		return factoryd.ApprovalRequest{}, false, refuse(http.StatusInternalServerError, "cannot read the approval requests")
	}
	i := slices.IndexFunc(all, func(q factoryd.ApprovalRequest) bool {
		return q.TaskID == k.ID && q.Kind == factoryd.ApprovalMerge && q.Project == t.p.Repository
	})
	if i < 0 {
		return factoryd.ApprovalRequest{}, false, nil
	}
	return all[i], true, nil
}

// tellRoute is the turn's card with the branch its state picks: the card
// read once here, so the routing and the proposal's line agree on it.
func (t *turn) tellRoute(in toolInput) (tellBranch, factoryd.Task, factoryd.ApprovalRequest, error) {
	k, err := t.card(in)
	if err != nil {
		return tellBranch{}, k, factoryd.ApprovalRequest{}, err
	}
	key := tellKey{status: k.Status}
	var q factoryd.ApprovalRequest
	if tellGateStatus(key.status) {
		pending, has, err := t.pendingMergeOf(k)
		if err != nil {
			return tellBranch{}, k, factoryd.ApprovalRequest{}, err
		}
		key.merge, q = has, pending
	} else if key.status == "waiting-human" {
		pending, has, err := t.pendingMergeOf(k)
		if err != nil {
			return tellBranch{}, k, factoryd.ApprovalRequest{}, err
		}
		if has {
			// a card whose merge request came to a human is decided there
			return tellBranch{}, k, pending, t.tellRefuse(k, true)
		}
	}
	branch, ok := tellRoutes[key]
	if !ok {
		return tellBranch{}, k, q, t.tellRefuse(k, key.merge)
	}
	return branch, k, q, nil
}

// tellRefuse refuses card k, whose state tells_card takes no text to,
// with 409: its state, the states the routing knows, and the action that
// fits it, so a wrong call corrects itself.
func (t *turn) tellRefuse(k factoryd.Task, merge bool) error {
	state := tellState(k.Status, merge)
	return refuse(http.StatusConflict, fmt.Sprintf("Card #%d %s is %s (%s), not %s: %s.",
		k.IssueNumber, k.ID, state, tellMeaning(state), tellFits, fitting(k, t.c.capFactsOf(t.r.Context(), k))))
}

// runTellCard is tell_card's run: the state picks the branch, the branch
// calls the existing action and returns its result.
func (t *turn) runTellCard(in toolInput) (string, error) {
	branch, k, q, err := t.tellRoute(in)
	if err != nil {
		return "", err
	}
	note, err := branch.run(t, in, k, q)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Told #%d (%s): %s", k.IssueNumber, branch.about, note), nil
}

// toldLine is tell_card's proposal description: describe's line, with
// where the card's state sends the text added before any warning, so the
// member sees the route and the event before they press Do it.
func toldLine(desc string, branch tellBranch, k factoryd.Task) string {
	route := fmt.Sprintf(" → %s (#%d %s, event %s)", branch.about, k.IssueNumber, tellState(k.Status, branch.event == factoryd.EventMergeDenied), branch.event)
	i := strings.LastIndex(desc, ", text ")
	if i < 0 {
		return desc + route
	}
	j := len(desc)
	if w := strings.Index(desc, ". Warning:"); w >= 0 {
		j = min(i, w)
	}
	return desc[:j] + route + desc[j:]
}
