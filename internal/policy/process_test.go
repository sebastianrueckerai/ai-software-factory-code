package policy

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
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
	const (
		spec  = "processes/spec.yaml"
		maint = "processes/maintenance.yaml"
		pol   = "policy.yaml"
		ready = "  - {name: ready, class: coder, moved_by: writers}\n"
		merge = "  - {name: merge, on_findings: human, enabled: true}\n"
		rev   = "  - {name: review, on_findings: agent, enabled: true}\n"
	)
	type change func(t *testing.T, f map[string][]byte)
	// the rows whose error is exactly their lines, no other
	exact := map[string]bool{"approval_required": true, "checks_disabled": true, "merge_when": true, "audit_without_review": true,
		"audit_after_approval": true, "ready_session_only": true}
	in := func(file, old, new string) change {
		return func(t *testing.T, f map[string][]byte) { edit(t, f, file, old, new) }
	}
	for name, tc := range map[string]struct {
		change change
		want   []string
	}{
		// S: schema and names
		"unknown_key": {in(spec, "merge: {queue: project}", "merge: {queue: project, mode: fast}"),
			[]string{"processes/spec.yaml: yaml: unmarshal errors:\n  line 24: field mode not found in type policy.Merge"}},
		"header": {in(spec, "# yaml-language-server: $schema=../schema/process.schema.json\n", ""),
			[]string{"processes/spec.yaml: the first line must be # yaml-language-server: $schema=../schema/process.schema.json"}},
		"process_file_name": {func(t *testing.T, f map[string][]byte) { f["processes/Spec.yaml"] = f[spec]; delete(f, spec) },
			[]string{"processes/Spec.yaml: the file name: lowercase letters, digits and dashes, at most 37"}},
		"lane_unknown": {in(spec, ready, ready+"  - {name: review}\n"),
			[]string{`processes/spec.yaml: lanes: lane 4: unknown name "review" (known: backlog, plan, ready)`}},
		"lane_twice": {in(spec, ready, ready+ready),
			[]string{"processes/spec.yaml: lanes: lane ready is listed twice"}},
		"backlog_missing": {in(maint, "  - {name: backlog, write_up: analyzer}\n", ""),
			[]string{"processes/maintenance.yaml: lanes: lane backlog is required: the cards that do not start wait there"}},
		"ready_missing": {in(maint, ready, ""),
			[]string{"processes/maintenance.yaml: lanes: lane ready is required: it starts the coding tasks"}},
		"class_unknown": {in(spec, "{name: ready, class: coder", "{name: ready, class: builder"),
			[]string{`processes/spec.yaml: lanes.ready: class "builder" is not a class of policy.yaml (classes: analyzer, coder, planner)`}},
		"class_missing": {in(spec, "{name: ready, class: coder, moved_by", "{name: ready, moved_by"),
			[]string{"processes/spec.yaml: lanes.ready: class is required on a starting lane"}},
		"moved_by_missing": {in(spec, "    moved_by: writers\n", ""),
			[]string{"processes/spec.yaml: lanes.plan: moved_by is required on a starting lane, one of [writers approvers]"}},
		"moved_by_value": {in(spec, "class: coder, moved_by: writers}", "class: coder, moved_by: anyone}"),
			[]string{`processes/spec.yaml: lanes.ready: moved_by "anyone", want one of [writers approvers]`}},
		"backlog_class": {in(spec, "{name: backlog}", "{name: backlog, class: coder}"),
			[]string{"processes/spec.yaml: lanes.backlog: class and moved_by are not allowed: backlog starts nothing and no card moves into it"}},
		"write_up_off_backlog": {in(spec, "moved_by: writers}\ndependencies", "moved_by: writers, write_up: analyzer}\ndependencies"),
			[]string{"processes/spec.yaml: lanes.ready: write_up is allowed on lane backlog only"}},
		"plan_off_plan": {in(spec, "moved_by: writers}\ndependencies", "moved_by: writers, plan: {max_cards: 5, foundation: true, integration: true}}\ndependencies"),
			[]string{"processes/spec.yaml: lanes.ready: plan is allowed on lane plan only"}},
		"plan_shape_missing": {in(spec, "    plan: {max_cards: 12, foundation: true, integration: true}\n", ""),
			[]string{"processes/spec.yaml: lanes.plan: plan is required: it bounds the plan the planner answers"}},
		"max_cards": {in(spec, "max_cards: 12", "max_cards: 40"),
			[]string{"processes/spec.yaml: lanes.plan: plan.max_cards 40: want 2 to 20"}},
		"queue_value": {in(spec, "merge: {queue: project}", "merge: {queue: area}"),
			[]string{`processes/spec.yaml: merge: queue "area", want one of [none project]`}},
		"dependencies_value": {in(spec, "wait_until: done", "wait_until: merged"),
			[]string{`processes/spec.yaml: dependencies: wait_until "merged", want one of [done]`}},
		"dependencies_on_failed": {in(spec, "on_failed: human", "on_failed: skip"),
			[]string{`processes/spec.yaml: dependencies: on_failed "skip", want one of [human]`}},
		"session_value": {in(pol, "session: plan\n", "session: plans\n"),
			[]string{`policy.yaml: classes.planner: session "plans", want one of [code plan report]`}},
		"session_missing": {in(pol, "    session: plan\n", ""),
			[]string{"policy.yaml: classes.planner: session is required, one of [code plan report]"}},
		"stage_unknown": {in(spec, merge+"merge:", merge+"  - {name: lint, on_findings: agent, enabled: true}\nmerge:"),
			[]string{`processes/spec.yaml: stages: stage 10: unknown name "lint" (known: [checks screening reproduction policy migration review audit approval merge])`}},
		"workflow_yaml": {func(t *testing.T, f map[string][]byte) {
			f["workflow.yaml"] = []byte("# yaml-language-server: $schema=schema/workflow.schema.json\nstages:\n" +
				"  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n" + merge)
		}, []string{`workflow.yaml: unknown file "workflow.yaml": the tree holds policy.yaml, processes/<name>.yaml, projects/<name>.yaml, templates/<name>/ and schema/`}},

		// P1 to P3: projects and their processes
		"project_process_unknown": {in("projects/linkshort.yaml", "process: spec", "process: specs"),
			[]string{`projects/linkshort.yaml: process "specs": no file processes/specs.yaml (processes: maintenance, spec)`}},
		"coder_without_process": {in("projects/demo-app.yaml", "process: maintenance\n", ""),
			[]string{"projects/demo-app.yaml: agents coder needs process, the name of a file under processes/"}},
		"none_with_process": {in("projects/ai-software-factory-infra.yaml", "agents: none\n", "agents: none\nprocess: maintenance\n"),
			[]string{"projects/ai-software-factory-infra.yaml: agents none: process is not allowed, no card starts there"}},

		// N: needs met in order
		"ready_session": {in(spec, "{name: ready, class: coder", "{name: ready, class: analyzer"),
			[]string{"processes/spec.yaml: lanes.ready: class analyzer has session report; lane ready needs a class with session code"}},
		"plan_session": {in(spec, "    class: planner\n", "    class: coder\n"),
			[]string{"processes/spec.yaml: lanes.plan: class coder has session code; lane plan needs a class with session plan"}},
		"write_up_session": {in(maint, "write_up: analyzer", "write_up: coder"),
			[]string{"processes/maintenance.yaml: lanes.backlog: write_up coder has session code; a write-up needs a class with session report"}},
		"write_up_unknown": {in(maint, "write_up: analyzer", "write_up: writer"),
			[]string{`processes/maintenance.yaml: lanes.backlog: write_up "writer" is not a class of policy.yaml (classes: analyzer, coder, planner)`}},
		"plan_needs_dependencies": {in(spec, "dependencies: {wait_until: done, on_failed: human}\n", ""),
			[]string{"processes/spec.yaml: lanes.plan: needs the block dependencies before it: the plan's cards wait on each other in lane ready"}},
		"dependencies_without_plan": {in(maint, ready, ready+"dependencies: {wait_until: done, on_failed: human}\n"),
			[]string{"processes/maintenance.yaml: dependencies: no lane plan makes dependencies: remove the block or add the lane"}},
		"audit_stage": {in(maint, "  - {name: audit, on_findings: human, enabled: true}\n", ""),
			[]string{"projects/ai-software-factory-code.yaml: audit: process maintenance lists no audit stage"}},
		"review_model": {in(pol, "    review_model: review-frontier\n", ""),
			[]string{"processes/maintenance.yaml: lanes.ready: class coder names no review_model, which the review stage calls",
				"processes/spec.yaml: lanes.ready: class coder names no review_model, which the review stage calls"}},
		"audit_model": {in(pol, "    audit_model: coding-frontier\n", ""),
			[]string{"processes/maintenance.yaml: lanes.ready: class coder names no audit_model, which the audit stage calls for project ai-software-factory-code (audit: true)"}},
		"supervisor_model": {in(pol, "    session: plan\n    models: [coding-cheap, coding-frontier]\n    supervisor_model: supervisor\n", "    session: plan\n    models: [coding-cheap, coding-frontier]\n"),
			[]string{"processes/spec.yaml: lanes.plan: class planner names no supervisor_model, which the session supervisor calls"}},

		// F: the floor no process may lower
		"checks_required": {in(spec, "  - {name: checks, on_findings: agent, enabled: true}\n", ""),
			[]string{"processes/spec.yaml: stages: stage checks is required: no merge without green checks (floor)"}},
		"checks_when": {in(spec, "{name: checks, on_findings: agent, enabled: true}", "{name: checks, on_findings: agent, enabled: true, when: outsider}"),
			[]string{"processes/spec.yaml: stages.checks: must run for every task (enabled, no when)"}},
		"policy_required": {in(spec, "  - {name: policy, on_findings: fail, enabled: true}\n", ""),
			[]string{"processes/spec.yaml: stages: stage policy is required"}},
		"policy_not_fail": {in(spec, "{name: policy, on_findings: fail", "{name: policy, on_findings: agent"),
			[]string{"processes/spec.yaml: stages.policy: on_findings must be fail: a forbidden path fails the task (floor)"}},
		"approval_required": {in(spec, "  - {name: approval, on_findings: human, enabled: true}\n", ""),
			[]string{"processes/spec.yaml: stages: stage approval is required"}},
		"approval_findings": {in(spec, "{name: approval, on_findings: human", "{name: approval, on_findings: agent"),
			[]string{"processes/spec.yaml: stages.approval: on_findings must be human"}},
		"approval_before_merge": {func(t *testing.T, f map[string][]byte) {
			edit(t, f, spec, rev, "")
			edit(t, f, spec, merge, rev+merge)
		}, []string{"processes/spec.yaml: stages: stage approval must come right before merge"}},
		"merge_last": {func(t *testing.T, f map[string][]byte) {
			const repro = "  - {name: reproduction, on_findings: agent, enabled: true}\n"
			edit(t, f, spec, repro, "")
			edit(t, f, spec, merge, merge+repro)
		}, []string{"processes/spec.yaml: stages: stage merge must be last"}},
		"review_when": {in(spec, "{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent, enabled: true, when: outsider}"),
			[]string{"processes/spec.yaml: stages.review: when is not allowed: a review runs for every task, or enabled: false turns it off"}},
		"project_looser": {in("projects/linkshort.yaml", "agents: coder\n", "agents: coder\nquota: {cpu: \"5\", memory: 1Gi, storage: 1Gi}\n"),
			[]string{`projects/linkshort.yaml: project linkshort: quota.cpu "5" is looser than policy.yaml's project_default.quota.cpu "4"`}},
		"checks_findings": {in(spec, "{name: checks, on_findings: agent", "{name: checks, on_findings: human"),
			[]string{"processes/spec.yaml: stages.checks: on_findings must be agent or fail: no merge without green checks (floor)"}},

		// the ordering rules and the stage shape, kept from workflow.yaml
		"screening_first": {func(t *testing.T, f map[string][]byte) {
			const scr = "  - {name: screening, on_findings: human, enabled: true, when: outsider}\n"
			const checks = "  - {name: checks, on_findings: agent, enabled: true}\n"
			edit(t, f, spec, scr+checks, checks+scr)
		}, []string{"processes/spec.yaml: stages: stage screening must be first: it runs before the first session"}},
		"migration_after_policy": {func(t *testing.T, f map[string][]byte) {
			const pl = "  - {name: policy, on_findings: fail, enabled: true}\n"
			const mig = "  - {name: migration, on_findings: human, enabled: true}\n"
			edit(t, f, spec, pl+mig, mig+pl)
		}, []string{"processes/spec.yaml: stages: stage migration must come after policy, which finds the migrations"}},
		"audit_order": {func(t *testing.T, f map[string][]byte) {
			const audit = "  - {name: audit, on_findings: human, enabled: true}\n"
			edit(t, f, spec, rev+audit, audit+rev)
		}, []string{"processes/spec.yaml: stages: stage audit must come between review and approval: it reads the reviewed head"}},
		"max_returns_zero": {in(spec, "{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent, enabled: true, max_returns: 0}"),
			[]string{"processes/spec.yaml: stages.review: max_returns must be at least 1, is 0"}},
		"max_returns_not_agent": {in(spec, "{name: migration, on_findings: human, enabled: true}", "{name: migration, on_findings: human, enabled: true, max_returns: 2}"),
			[]string{"processes/spec.yaml: stages.migration: max_returns needs on_findings agent"}},
		"stage_twice": {in(spec, rev, rev+rev),
			[]string{"processes/spec.yaml: stages: stage review is listed twice"}},
		"merge_required": {in(spec, merge, ""),
			[]string{"processes/spec.yaml: stages: stage merge is required"}},
		"policy_disabled": {in(spec, "{name: policy, on_findings: fail, enabled: true}", "{name: policy, on_findings: fail, enabled: false}"),
			[]string{"processes/spec.yaml: stages.policy: must run for every task (enabled, no when)"}},
		"merge_findings": {in(spec, "{name: merge, on_findings: human", "{name: merge, on_findings: agent"),
			[]string{"processes/spec.yaml: stages.merge: on_findings must be human"}},
		"screening_findings": {in(spec, "{name: screening, on_findings: human", "{name: screening, on_findings: fail"),
			[]string{"processes/spec.yaml: stages.screening: on_findings must be human"}},
		"audit_findings": {in(spec, "{name: audit, on_findings: human", "{name: audit, on_findings: agent"),
			[]string{"processes/spec.yaml: stages.audit: on_findings must be human"}},
		"findings_value": {in(spec, "{name: migration, on_findings: human", "{name: migration, on_findings: maybe"),
			[]string{`processes/spec.yaml: stages.migration: on_findings "maybe", want one of [agent human fail]`}},
		"when_value": {in(spec, "when: outsider", "when: insider"),
			[]string{`processes/spec.yaml: stages.screening: when "insider", want one of [outsider]`}},
		"checks_disabled": {in(spec, "{name: checks, on_findings: agent, enabled: true}", "{name: checks, on_findings: agent, enabled: false}"),
			[]string{"processes/spec.yaml: stages.checks: must run for every task (enabled, no when)"}},
		"merge_when": {in(spec, "{name: merge, on_findings: human, enabled: true}", "{name: merge, on_findings: human, enabled: true, when: outsider}"),
			[]string{"processes/spec.yaml: stages.merge: must run for every task (enabled, no when)"}},
		"audit_without_review": {in(spec, rev, ""),
			[]string{"processes/spec.yaml: stages: stage audit must come between review and approval: it reads the reviewed head"}},
		"audit_after_approval": {func(t *testing.T, f map[string][]byte) {
			const audit = "  - {name: audit, on_findings: human, enabled: true}\n"
			edit(t, f, spec, audit, "")
			edit(t, f, spec, merge, audit+merge)
		}, []string{"processes/spec.yaml: stages: stage approval must come right before merge",
			"processes/spec.yaml: stages: stage audit must come between review and approval: it reads the reviewed head"}},
		"ready_session_only": {in(spec, "{name: ready, class: coder", "{name: ready, class: analyzer"),
			[]string{"processes/spec.yaml: lanes.ready: class analyzer has session report; lane ready needs a class with session code"}},
		"nested_process": {func(t *testing.T, f map[string][]byte) { f["processes/x/spec.yaml"] = f[spec] },
			[]string{`processes/x/spec.yaml: unknown file "processes/x/spec.yaml": the tree holds policy.yaml, processes/<name>.yaml, projects/<name>.yaml, templates/<name>/ and schema/`}},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			tc.change(t, files)
			_, err := ParseTree(files)
			if err == nil {
				t.Fatal("the tree passes")
			}
			if exact[name] && err.Error() != strings.Join(tc.want, "\n") {
				t.Errorf("ParseTree error =\n%v\nwant exactly:\n%s", err, strings.Join(tc.want, "\n"))
			}
			for _, want := range tc.want {
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
		"projects/ai-software-factory-code.yaml", "projects/ai-software-factory-infra.yaml", "projects/demo-app.yaml", "projects/linkshort.yaml"}) {
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
