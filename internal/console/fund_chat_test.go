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
	if !strings.Contains(got, "raise_max_eur") || !strings.Contains(got, "1.00") {
		t.Errorf("the refusal = %q, want it to name raise_max_eur and its 1.00", got)
	}
}

func TestFundPastThePoolRefuses(t *testing.T) {
	r := fundRig(t)
	r.tasks.fundedEUR = 2.5
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 1, Text: "more"})
	if !strings.Contains(got, "discretionary_eur") || !strings.Contains(got, "0.50") {
		t.Errorf("the refusal = %q, want it to name discretionary_eur and the 0.50 left", got)
	}
}

func TestFundWithNoPoolRefuses(t *testing.T) {
	r := fundRig(t)
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder"}
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 0.5, Text: "more"})
	if !strings.Contains(got, "discretionary_eur") {
		t.Errorf("the refusal = %q, want discretionary_eur named: absent keys read 0", got)
	}
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder",
		Budget: &policy.ProjectBudget{DiscretionaryEUR: 3}}
	if got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 0.5, Text: "more"}); !strings.Contains(got, "raise_max_eur") {
		t.Errorf("the refusal = %q, want raise_max_eur named", got)
	}
}

func TestFundPastTaskMaxRefuses(t *testing.T) {
	r := fundRig(t)
	r.ledger.files["ai-software-factory-code"] = policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder",
		Budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 4}}
	got := fundRefusal(t, r, toolInput{TaskID: "t-e", EUR: 3.5, Text: "more"})
	if !strings.Contains(got, "task_max_eur") || !strings.Contains(got, "5.00") {
		t.Errorf("the refusal = %q, want it to name task_max_eur and its 5.00", got)
	}
}
