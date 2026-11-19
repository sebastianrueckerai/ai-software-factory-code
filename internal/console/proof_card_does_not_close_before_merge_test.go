package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The close press on the criteria card is refused while the plan's pull
// request is open and unmerged: the press answers 409, the card stays
// open in review, and no criteria verdict is recorded on it.
func TestCardDoesNotCloseBeforeMerge(t *testing.T) {
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The criteria gate", Criteria: []factoryd.PlanCriterion{
		{ID: "confirm_lead", Kind: factoryd.CriterionTest, Suite: "./internal/console", Name: "TestConfirmationLead"},
		{ID: "guide_sentence", Kind: factoryd.CriterionTest, Suite: "./internal/factoryd", Name: "TestGuideAsksOneSentence"},
	}, Cards: []factoryd.PlanCard{
		{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integration"},
	}})
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "The criteria gate",
			Repository: "sf/code", IssueNumber: 3}},
		{Task: factoryd.Task{ID: "t-int", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 5, PRNumber: 12,
			Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "integration"}},
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}}
	ck, s := r.loggedIn(t)
	press := r.do(post("/tasks/t-int/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.ActClose}}), ck)
	if press.Code != http.StatusConflict {
		t.Errorf("the close press on the open criteria card: %d %q; want 409", press.Code, press.Body)
	}
	if i := slices.IndexFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == "t-int" }); i < 0 || r.tasks.tasks[i].Status != "in-review" {
		t.Errorf("the criteria card did not stay open in review: %+v", r.tasks.tasks)
	}
	if n := len(r.tasks.criteria["t-int"]); n != 0 {
		t.Errorf("the refused press recorded %d criteria verdicts: %+v", n, r.tasks.criteria["t-int"])
	}
}

// The successor that carries the criteria card's key is that card: its
// close is refused while its pull request is open and unmerged.
func TestASuccessorOfTheCriteriaCardDoesNotCloseBeforeMerge(t *testing.T) {
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The criteria gate", Criteria: []factoryd.PlanCriterion{
		{ID: "confirm_lead", Kind: factoryd.CriterionTest, Suite: "./internal/console", Name: "TestConfirmationLead"},
	}, Cards: []factoryd.PlanCard{
		{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integration"},
	}})
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "The criteria gate",
			Repository: "sf/code", IssueNumber: 3}},
		{Task: factoryd.Task{ID: "t-int", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 5,
			Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "integration"}},
		{Task: factoryd.Task{ID: "t-is", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 6, PRNumber: 12,
			Parent: "t-plan", Source: factoryd.SourceSuccessor, PlanKey: "integration"}},
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}}
	ck, s := r.loggedIn(t)
	press := r.do(post("/tasks/t-is/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.ActClose}}), ck)
	if press.Code != http.StatusConflict {
		t.Errorf("the close press on the open successor of the criteria card: %d %q; want 409", press.Code, press.Body)
	}
}

// The refusal holds only for plans that hold criteria: the same open
// unmerged pull request on a criteria-less plan card does not refuse the
// close, and the card closes as any card does.
func TestCardWithoutCriteriaClosesBeforeMerge(t *testing.T) {
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The criteria gate", Criteria: []factoryd.PlanCriterion{}, Cards: []factoryd.PlanCard{
		{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integration"},
	}})
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "The criteria gate",
			Repository: "sf/code", IssueNumber: 3}},
		{Task: factoryd.Task{ID: "t-int", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 5, PRNumber: 12,
			Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "integration"}},
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}}
	ck, s := r.loggedIn(t)
	press := r.do(post("/tasks/t-int/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.ActClose}}), ck)
	if press.Code != http.StatusSeeOther {
		t.Errorf("the close press on the criteria-less card: %d %q; want a close, not a refusal", press.Code, press.Body)
	}
}
