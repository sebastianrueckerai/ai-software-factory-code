package policy

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy/policytest"
)

// edit replaces old with new once in file, and fails the test when file
// lacks old: a fixture change that no longer applies is a broken test.
func edit(t *testing.T, files map[string][]byte, file, old, new string) {
	t.Helper()
	if !strings.Contains(string(files[file]), old) {
		t.Fatalf("%s lacks %q", file, old)
	}
	files[file] = []byte(strings.Replace(string(files[file]), old, new, 1))
}

// hasLines reports whether want stands in got as whole lines.
func hasLines(got, want string) bool {
	return strings.Contains("\n"+got+"\n", "\n"+want+"\n")
}

// Every static refusal of the check (stage11-design.md section 3), one
// subtest each: the valid fixture tree plus one change, the exact line.
func TestProcessRefusals(t *testing.T) {
	for name, tc := range policytest.Refusals {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			tc.Change(files)
			_, err := ParseTree(files)
			if err == nil {
				t.Fatal("the tree passes")
			}
			if tc.Exact && err.Error() != strings.Join(tc.Want, "\n") {
				t.Errorf("ParseTree error =\n%v\nwant exactly:\n%s", err, strings.Join(tc.Want, "\n"))
			}
			for _, want := range tc.Want {
				if !hasLines(err.Error(), want) {
					t.Errorf("ParseTree error lacks the line\n%s\ngot:\n%v", want, err)
				}
			}
		})
	}
}

// The fixture tree of section 0 passes, as parsed and as read back.
func TestValidTreePasses(t *testing.T) {
	files := fixtureTree(t)
	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"policy.yaml", "processes/maintenance.yaml", "processes/spec.yaml",
		"projects/ai-software-factory-code.yaml", "projects/ai-software-factory-infra.yaml", "projects/demo-app.yaml", "projects/linkshort.yaml",
		"templates/ts-go/.forgejo/issue_template/bug.yaml", "templates/ts-go/.forgejo/issue_template/improvement.yaml", "templates/ts-go/.forgejo/roll.sh",
		"templates/ts-go/.forgejo/workflows/ci.yml", "templates/ts-go/.forgejo/workflows/pr.yml", "templates/ts-go/.gitignore", "templates/ts-go/AGENTS.md",
		"templates/ts-go/deploy/base/kustomization.yaml", "templates/ts-go/deploy/staging/kustomization.yaml"}) {
		t.Fatalf("fixture files = %v", got)
	}
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	spec := tree.Processes["spec"]
	if len(spec.Lanes) != 3 || spec.Lanes[1].Plan == nil || *spec.Lanes[1].Plan != (PlanShape{MaxCards: 12, Foundation: true, Integration: true}) ||
		spec.Dependencies == nil || *spec.Dependencies != (Dependencies{WaitUntil: "done", OnFailed: "human"}) || spec.Merge.Queue != QueueProject || len(spec.Stages) != 9 {
		t.Fatalf("spec = %+v", spec)
	}
	maint := tree.Processes["maintenance"]
	if maint.Lanes[0] != (Lane{Name: LaneBacklog, WriteUp: ClassAnalyzer}) || maint.Lanes[1] != (Lane{Name: LaneReady, Class: ClassCoder, MovedBy: MovedByWriters}) ||
		maint.Dependencies != nil || maint.Merge.Queue != QueueNone {
		t.Fatalf("maintenance = %+v", maint)
	}
	if tree.Document.Classes["planner"].Session != SessionPlan || tree.Projects["linkshort"].Process != "spec" || tree.Projects["ai-software-factory-infra"].Process != "" {
		t.Fatalf("planner %+v, linkshort %+v", tree.Document.Classes["planner"], tree.Projects["linkshort"])
	}
}

// The infra repository's factory/ passes, with INFRA_FACTORY_DIR set to it.
func TestShippedTreePasses(t *testing.T) {
	dir := os.Getenv("INFRA_FACTORY_DIR")
	if dir == "" {
		t.Skip("INFRA_FACTORY_DIR unset")
	}
	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTree(files); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchemas(files); err != nil {
		t.Fatal(err)
	}
}

// The gateway validates the Document of the version in force; a version
// written before classes had a session stays valid there during the roll
// (section 1.3), while the tree check refuses it.
func TestDocumentValidateNeedsNoSession(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range tree.Document.Classes {
		c.Session = ""
		tree.Document.Classes[name] = c
	}
	if err := tree.Document.Validate(); err != nil {
		t.Fatalf("Document.Validate without sessions: %v", err)
	}
	if err := tree.Validate(); err == nil || !hasLines(err.Error(), "policy.yaml: classes.coder: session is required, one of [code plan report]") {
		t.Fatalf("Tree.Validate without sessions = %v", err)
	}
}

// A stored tree is judged by the same rules, a broken process included.
func TestTreeValidateJudgesAStoredProcess(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := tree.Processes["spec"]
	spec.Lanes = slices.Clone(spec.Lanes)
	spec.Lanes[1].Plan = nil
	tree.Processes["spec"] = spec
	if err := tree.Validate(); err == nil || !hasLines(err.Error(), "processes/spec.yaml: lanes.plan: plan is required: it bounds the plan the planner answers") {
		t.Fatalf("Validate = %v", err)
	}
}

// What the floor leaves open passes: checks failing the task, a disabled
// screening, a return cap on the review.
func TestProcessAcceptsWhatTheFloorAllows(t *testing.T) {
	files := fixtureTree(t)
	for _, c := range [][2]string{
		{"{name: checks, on_findings: agent", "{name: checks, on_findings: fail"},
		{"{name: screening, on_findings: human, enabled: true", "{name: screening, on_findings: human, enabled: false"},
		{"{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent, enabled: true, max_returns: 1}"},
	} {
		edit(t, files, "processes/spec.yaml", c[0], c[1])
	}
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	spec := tree.Processes["spec"]
	checks, _ := spec.Stage(StageChecks)
	screening, _ := spec.Stage(StageScreening)
	review, _ := spec.Stage(StageReview)
	if checks.OnFindings != FindingsFail || screening.Enabled || review.MaxReturns == nil || *review.MaxReturns != 1 {
		t.Fatalf("checks %+v, screening %+v, review %+v", checks, screening, review)
	}
}

// A stored tree from before sessions prints the session lines once and
// nothing that follows from them.
func TestTreeValidateWithoutSessionsSaysItOnce(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range tree.Document.Classes {
		c.Session = ""
		tree.Document.Classes[name] = c
	}
	want := "policy.yaml: classes.analyzer: session is required, one of [code plan report]\n" +
		"policy.yaml: classes.coder: session is required, one of [code plan report]\n" +
		"policy.yaml: classes.planner: session is required, one of [code plan report]"
	if err := tree.Validate(); err == nil || err.Error() != want {
		t.Fatalf("Validate =\n%v\nwant:\n%s", err, want)
	}
}
