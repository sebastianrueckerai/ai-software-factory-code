package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fundRig is a rig whose project holds a pool of 3 EUR and raises of at
// most 1 EUR, over the capped card t-e of 2 EUR; the policy's task maximum
// is 5 EUR.
func fundRig(t *testing.T) *rig {
	r := newRig(t)
	cappedCard(r, t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", Objects: "factory",
		Agents: "coder", Process: "maintenance", Budget: &policy.ProjectBudget{DiscretionaryEUR: 3, RaiseMaxEUR: 1}}}
	return r
}

// runFund runs the fund tool as the chat's turn does.
func runFund(t *testing.T, r *rig, in toolInput) (string, error) {
	t.Helper()
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, ok := r.c.chatTool("fund")
	if !ok {
		t.Fatal("there is no fund tool")
	}
	note, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, in)
	return note, err
}

func fundRefusal(t *testing.T, r *rig, in toolInput) string {
	t.Helper()
	_, err := runFund(t, r, in)
	var no *refusal
	if !errors.As(err, &no) {
		t.Fatalf("fund %+v = %v, want a refusal", in, err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 {
		t.Errorf("a refused fund moved the budget to %v", task.BudgetEUR)
	}
	return no.msg
}

func TestFundWithinThePoolRaisesTheCard(t *testing.T) {
	r := fundRig(t)
	r.tasks.fundedEUR = 1.5
	note, err := runFund(t, r, toolInput{TaskID: "t-e", EUR: 1, Text: "the replica is worth it"})
	if err != nil || note != "Raised #13's budget by 1.00 EUR to 3.00 and posted your note as the answer; the card resumes." {
		t.Fatalf("fund = %q, %v", note, err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 3 {
		t.Errorf("budget_eur = %v, want 3", task.BudgetEUR)
	}
	e := r.events.rows[0]
	if e.Kind != factoryd.EventCardFunded || e.Via != factoryd.ViaChat || e.Detail["amount_eur"] != 1.0 {
		t.Errorf("the raise's row = %+v", e)
	}
}

func TestFundAboveRaiseMaxRefuses(t *testing.T) {
	r := fundRig(t)
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 1.5, Text: "more"})
	if !strings.Contains(got, "raise_max_eur") || !strings.Contains(got, "1.00") || !strings.Contains(got, "attach_analysis") {
		t.Errorf("the refusal = %q, want raise_max_eur, its 1.00 and attach_analysis", got)
	}
}

func TestFundPastThePoolRefuses(t *testing.T) {
	r := fundRig(t)
	r.tasks.fundedEUR = 2.5
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 1, Text: "more"})
	if !strings.Contains(got, "discretionary_eur") || !strings.Contains(got, "0.50 left") || !strings.Contains(got, "attach_analysis") {
		t.Errorf("the refusal = %q, want discretionary_eur, the 0.50 left and attach_analysis", got)
	}
}

func TestFundWithNoPoolRefuses(t *testing.T) {
	r := fundRig(t)
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder"}
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 0.5, Text: "more"})
	if !strings.Contains(got, "sets no pool") || !strings.Contains(got, "discretionary_eur is absent or 0") || !strings.Contains(got, "attach_analysis") {
		t.Errorf("the refusal = %q, want the missing pool named with attach_analysis", got)
	}
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder",
		Budget: &policy.ProjectBudget{DiscretionaryEUR: 3}}
	if got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 0.5, Text: "more"}); !strings.Contains(got, "raise_max_eur") {
		t.Errorf("the refusal = %q, want raise_max_eur named", got)
	}
}

// The card is bounded by the lower of the policy's task_max_eur (5) and
// the project's own (4): the refusal names the one that binds.
func TestFundPastTaskMaxRefuses(t *testing.T) {
	r := fundRig(t)
	file := func(own float64) {
		r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder",
			Budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 4, TaskMaxEUR: own}}
	}
	file(0)
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 3.5, Text: "more"})
	if !strings.Contains(got, "the policy's task_max_eur of €5.00") || !strings.Contains(got, "sizing exception") || strings.Contains(got, "split") {
		t.Errorf("the refusal = %q, want the policy's task_max_eur of 5.00 and a sizing exception", got)
	}
	file(4)
	got = fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 3, Text: "more"})
	if !strings.Contains(got, "the project's budget.task_max_eur of €4.00") || !strings.Contains(got, "at most €2.00") {
		t.Errorf("the refusal = %q, want the project's task_max_eur of 4.00 and at most 2.00", got)
	}
	file(6)
	got = fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 3.5, Text: "more"})
	if !strings.Contains(got, "the policy's task_max_eur of €5.00") {
		t.Errorf("the refusal = %q, want the policy's 5.00 where the project's 6 is looser", got)
	}
}

// A wake with no live member session cannot post the answer: the fund
// refuses before the budget or the pool moves.
func TestFundAtAWakeWithoutALiveSessionRefusesAndChangesNothing(t *testing.T) {
	r := fundRig(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s := &session{user: "sebastian"}
	tool, _ := r.c.chatTool("fund")
	_, _, err := tool.run(&turn{c: r.c, r: req.WithContext(context.WithValue(req.Context(), ctxKey{}, s)),
		a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: true},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, toolInput{TaskID: "t-e", EUR: 1, Text: "go"})
	var no *refusal
	if !errors.As(err, &no) || !strings.Contains(no.msg, "login has lapsed") || !strings.Contains(no.msg, "attach_analysis") {
		t.Fatalf("fund = %v, want the lapsed login refused", err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 || len(r.events.rows) != 0 {
		t.Errorf("budget %v and %d rows after the refusal, want 2 and none", task.BudgetEUR, len(r.events.rows))
	}
}

// Money compares in cents: 0.1 and 0.2 fill a pool of 0.3 exactly.
func TestFundFillsThePoolExactly(t *testing.T) {
	r := fundRig(t)
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder",
		Budget: &policy.ProjectBudget{DiscretionaryEUR: 0.3, RaiseMaxEUR: 1}}
	r.tasks.fundedEUR = 0.1
	if _, err := runFund(t, r, toolInput{TaskID: "t-e", EUR: 0.2, Text: "fill it"}); err != nil {
		t.Fatalf("fund of the pool's last 0.2: %v", err)
	}
}

const needRefusal = "the remaining budget cannot fund a call: 0.170 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.900000 EUR. Raise the budget (Fund on the board) and answer, or close the card."

func needRig(t *testing.T) *rig {
	r := fundRig(t)
	r.tasks.set(needRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"})
	r.c.cfg.SessionFunded = func(ctx context.Context, id, _ string) (string, error) { return r.raisedFloor(ctx, id, ""), nil }
	return r
}

func answerFund(t *testing.T, r *rig, via string) (string, error) {
	t.Helper()
	_, s := r.loggedIn(t)
	r.c.cfg.SessionFunded = func(ctx context.Context, id, _ string) (string, error) {
		if task, _, _ := r.tasks.Task(ctx, id); task.BudgetEUR > 2 {
			return "", nil
		}
		return needRefusal, nil
	}
	return r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: via}, "t-e", "the fix is in", factoryd.AnswerFund)
}

// The chat answer's fund value is a chat-attributed raise: it meets the
// pool, raise_max_eur and the card cap as the fund tool does.
func TestChatAnswerFundPastThePoolRefusesAndChangesNothing(t *testing.T) {
	r := needRig(t)
	r.tasks.fundedEUR = 2.5
	_, err := answerFund(t, r, factoryd.ViaChat)
	var no *refusal
	if !errors.As(err, &no) || !strings.Contains(no.msg, "discretionary_eur") {
		t.Fatalf("answer fund past the pool = %v, want a refusal naming discretionary_eur", err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 || len(r.events.rows) != 0 || len(r.fg.comments) != 0 {
		t.Errorf("budget %v, %d rows, %d comments after the refusal, want 2, none, none", task.BudgetEUR, len(r.events.rows), len(r.fg.comments))
	}
}

func TestChatAnswerFundWithinTheBoundsRaisesAndRecordsOnce(t *testing.T) {
	r := needRig(t)
	if _, err := answerFund(t, r, factoryd.ViaChat); err != nil {
		t.Fatalf("answer fund: %v", err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2.9 {
		t.Errorf("budget_eur = %v, want 2.9", task.BudgetEUR)
	}
	funded := 0
	for _, e := range r.events.rows {
		if e.Kind == factoryd.EventCardFunded {
			funded++
			if e.Via != factoryd.ViaChat || e.Detail["amount_eur"] != 0.9 {
				t.Errorf("the card_funded row = %+v", e)
			}
		}
	}
	if funded != 1 {
		t.Errorf("%d card_funded rows, want 1", funded)
	}
}

// The board's answer keeps no pool: the member's own money.
func TestBoardAnswerFundIgnoresThePool(t *testing.T) {
	r := needRig(t)
	r.tasks.fundedEUR = 2.95
	if _, err := answerFund(t, r, factoryd.ViaButton); err != nil {
		t.Fatalf("board answer fund: %v", err)
	}
}
