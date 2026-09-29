package policy

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func workflowOf(t *testing.T, doc string) Workflow {
	t.Helper()
	var w Workflow
	if err := yaml.Unmarshal([]byte(doc), &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// The rules Task 74's engine needs on top of the stage list's shape.
func TestWorkflowValidateRefuses(t *testing.T) {
	tail := "  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n"
	for name, c := range map[string]struct{ doc, want string }{
		"unknown stage":        {"stages:\n  - {name: lint, on_findings: agent, enabled: true}\n" + tail, `unknown name "lint"`},
		"twice":                {"stages:\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: review, on_findings: agent, enabled: true}\n" + tail, "stage review is listed twice"},
		"no policy":            {"stages:\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n", "stage policy is required"},
		"policy disabled":      {"stages:\n  - {name: policy, on_findings: fail, enabled: false}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n", "stage policy must run for every task"},
		"checks disabled":      {"stages:\n  - {name: checks, on_findings: agent, enabled: false}\n" + tail, "stage checks must run for every task"},
		"checks for outsiders": {"stages:\n  - {name: checks, on_findings: agent, enabled: true, when: outsider}\n" + tail, "stage checks must run for every task"},
		"review for outsiders": {"stages:\n  - {name: review, on_findings: agent, enabled: true, when: outsider}\n" + tail, "stage review: when is not allowed"},
		"merge when":           {"stages:\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true, when: outsider}\n", "stage merge must run for every task"},
		"merge not last":       {"stages:\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n  - {name: review, on_findings: agent, enabled: true}\n", "stage merge must be last"},
		"approval early":       {"stages:\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n", "approval must come right before merge"},
		"merge to agent":       {"stages:\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: agent, enabled: true}\n", "stage merge: on_findings must be human"},
		"screening to agent":   {"stages:\n  - {name: screening, on_findings: agent, enabled: true}\n" + tail, "stage screening: on_findings must be human"},
		"screening fails":      {"stages:\n  - {name: screening, on_findings: fail, enabled: true}\n" + tail, "stage screening: on_findings must be human"},
		"screening late":       {"stages:\n  - {name: checks, on_findings: agent, enabled: true}\n  - {name: screening, on_findings: human, enabled: true}\n" + tail, "screening must be first"},
		"migration first":      {"stages:\n  - {name: migration, on_findings: human, enabled: true}\n" + tail, "migration must come after policy"},
		"max_returns 0":        {"stages:\n  - {name: review, on_findings: agent, enabled: true, max_returns: 0}\n" + tail, "max_returns must be at least 1"},
		"max_returns on fail":  {"stages:\n  - {name: review, on_findings: fail, enabled: true, max_returns: 2}\n" + tail, "max_returns needs on_findings agent"},
		"audit to agent":       {"stages:\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: audit, on_findings: agent, enabled: true}\n" + tail, "stage audit: on_findings must be human"},
		"audit fails":          {"stages:\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: audit, on_findings: fail, enabled: true}\n" + tail, "stage audit: on_findings must be human"},
		"audit before review":  {"stages:\n  - {name: audit, on_findings: human, enabled: true}\n  - {name: review, on_findings: agent, enabled: true}\n" + tail, "stage audit must come between review and approval"},
		"audit without review": {"stages:\n  - {name: audit, on_findings: human, enabled: true}\n" + tail, "stage audit must come between review and approval"},
		"audit after approval": {"stages:\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: audit, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n", "stage audit must come between review and approval"},
	} {
		err := workflowOf(t, c.doc).Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestWorkflowValidateAcceptsADisabledStageMaxReturnsAndAnyOrderInBetween(t *testing.T) {
	w := workflowOf(t, "stages:\n"+
		"  - {name: screening, on_findings: human, enabled: false, when: outsider}\n"+
		"  - {name: review, on_findings: agent, enabled: true, max_returns: 1}\n"+
		"  - {name: policy, on_findings: agent, enabled: true}\n"+
		"  - {name: checks, on_findings: fail, enabled: true}\n"+
		"  - {name: reproduction, on_findings: fail, enabled: false}\n"+
		"  - {name: approval, on_findings: human, enabled: true}\n"+
		"  - {name: merge, on_findings: human, enabled: true}\n")
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	if s, ok := w.Stage("review"); !ok || s.MaxReturns == nil || *s.MaxReturns != 1 {
		t.Fatalf("review = %+v %v", s, ok)
	}
	if _, ok := w.Stage("migration"); ok {
		t.Fatal("migration listed")
	}
}

// The audit (ruling 188) reads the reviewed head before the approval,
// its findings for a human only; disabled, it may stand anywhere the
// rules put it.
func TestWorkflowValidateAcceptsTheAuditBetweenReviewAndApproval(t *testing.T) {
	for _, doc := range []string{
		"stages:\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: audit, on_findings: human, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n",
		"stages:\n  - {name: review, on_findings: agent, enabled: true}\n  - {name: policy, on_findings: fail, enabled: true}\n  - {name: audit, on_findings: human, enabled: false}\n  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n",
	} {
		if err := workflowOf(t, doc).Validate(); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}
