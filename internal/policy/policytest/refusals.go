// Package policytest holds the refusal fixtures of the configuration
// check (stage11-design.md section 3), which the policy package's
// TestProcessRefusals and factoryd's TestConfigCheckAndLoaderAgree both run.
package policytest

import (
	"fmt"
	"strings"
)

// Refusal is one change to the valid fixture tree and the lines the check
// refuses it with; Exact: the whole refusal is those lines.
type Refusal struct {
	Change func(files map[string][]byte)
	Want   []string
	Exact  bool
}

const (
	spec  = "processes/spec.yaml"
	maint = "processes/maintenance.yaml"
	pol   = "policy.yaml"
	ready = "  - {name: ready, class: coder, moved_by: writers}\n"
	merge = "  - {name: merge, on_findings: human, enabled: true}\n"
	rev   = "  - {name: review, on_findings: agent, enabled: true}\n"
)

// edit replaces old by new once in file; a fixture that lacks old is a
// broken fixture.
func edit(files map[string][]byte, file, old, new string) {
	if !strings.Contains(string(files[file]), old) {
		panic(fmt.Sprintf("%s lacks %q", file, old))
	}
	files[file] = []byte(strings.Replace(string(files[file]), old, new, 1))
}

func in(file, old, new string) func(map[string][]byte) {
	return func(f map[string][]byte) { edit(f, file, old, new) }
}

// Refusals are the refusals by subtest name.
var Refusals = map[string]Refusal{
	// S: schema and names
	"unknown_key": {Change: in(spec, "merge: {queue: project}", "merge: {queue: project, mode: fast}"),
		Want: []string{"processes/spec.yaml: yaml: unmarshal errors:\n  line 24: field mode not found in type policy.Merge"}},
	"header": {Change: in(spec, "# yaml-language-server: $schema=../schema/process.schema.json\n", ""),
		Want: []string{"processes/spec.yaml: the first line must be # yaml-language-server: $schema=../schema/process.schema.json"}},
	"process_file_name": {Change: func(f map[string][]byte) { f["processes/Spec.yaml"] = f[spec]; delete(f, spec) },
		Want: []string{"processes/Spec.yaml: the file name: lowercase letters, digits and dashes, at most 37"}},
	"lane_unknown": {Change: in(spec, ready, ready+"  - {name: review}\n"),
		Want: []string{`processes/spec.yaml: lanes: lane 4: unknown name "review" (known: backlog, plan, ready)`}},
	"lane_twice": {Change: in(spec, ready, ready+ready),
		Want: []string{"processes/spec.yaml: lanes: lane ready is listed twice"}},
	"backlog_missing": {Change: in(maint, "  - {name: backlog, write_up: analyzer}\n", ""),
		Want: []string{"processes/maintenance.yaml: lanes: lane backlog is required: the cards that do not start wait there"}},
	"ready_missing": {Change: in(maint, ready, ""),
		Want: []string{"processes/maintenance.yaml: lanes: lane ready is required: it starts the coding tasks"}},
	"class_unknown": {Change: in(spec, "{name: ready, class: coder", "{name: ready, class: builder"),
		Want: []string{`processes/spec.yaml: lanes.ready: class "builder" is not a class of policy.yaml (classes: analyzer, coder, planner)`}},
	"class_missing": {Change: in(spec, "{name: ready, class: coder, moved_by", "{name: ready, moved_by"),
		Want: []string{"processes/spec.yaml: lanes.ready: class is required on a starting lane"}},
	"moved_by_missing": {Change: in(spec, "    moved_by: writers\n", ""),
		Want: []string{"processes/spec.yaml: lanes.plan: moved_by is required on a starting lane, one of [writers approvers]"}},
	"moved_by_value": {Change: in(spec, "class: coder, moved_by: writers}", "class: coder, moved_by: anyone}"),
		Want: []string{`processes/spec.yaml: lanes.ready: moved_by "anyone", want one of [writers approvers]`}},
	"backlog_class": {Change: in(spec, "{name: backlog}", "{name: backlog, class: coder}"),
		Want: []string{"processes/spec.yaml: lanes.backlog: class and moved_by are not allowed: backlog starts nothing and no card moves into it"}},
	"write_up_off_backlog": {Change: in(spec, "moved_by: writers}\ndependencies", "moved_by: writers, write_up: analyzer}\ndependencies"),
		Want: []string{"processes/spec.yaml: lanes.ready: write_up is allowed on lane backlog only"}},
	"plan_off_plan": {Change: in(spec, "moved_by: writers}\ndependencies", "moved_by: writers, plan: {max_cards: 5, foundation: true, integration: true}}\ndependencies"),
		Want: []string{"processes/spec.yaml: lanes.ready: plan is allowed on lane plan only"}},
	"plan_shape_missing": {Change: in(spec, "    plan: {max_cards: 12, min_cards: 1, foundation: true, integration: true}\n", ""),
		Want: []string{"processes/spec.yaml: lanes.plan: plan is required: it bounds the plan the planner answers"}},
	"max_cards": {Change: in(spec, "max_cards: 12", "max_cards: 40"),
		Want: []string{"processes/spec.yaml: lanes.plan: plan.max_cards 40: want 2 to 20"}},
	"min_cards_zero": {Change: in(spec, "min_cards: 1", "min_cards: 0"),
		Want: []string{"processes/spec.yaml: lanes.plan: plan.min_cards 0: want 1 to 20"}},
	"min_cards_over": {Change: in(spec, "min_cards: 1", "min_cards: 25"),
		Want: []string{"processes/spec.yaml: lanes.plan: plan.min_cards 25: want 1 to 20"}},
	"min_cards_above_max": {Change: in(spec, "min_cards: 1", "min_cards: 13"),
		Want: []string{"processes/spec.yaml: lanes.plan: plan.min_cards 13: above plan.max_cards 12"}},
	"queue_value": {Change: in(spec, "merge: {queue: project}", "merge: {queue: area}"),
		Want: []string{`processes/spec.yaml: merge: queue "area", want one of [none project]`}},
	"dependencies_value": {Change: in(spec, "wait_until: done", "wait_until: merged"),
		Want: []string{`processes/spec.yaml: dependencies: wait_until "merged", want one of [done]`}},
	"dependencies_on_failed": {Change: in(spec, "on_failed: human", "on_failed: skip"),
		Want: []string{`processes/spec.yaml: dependencies: on_failed "skip", want one of [human]`}},
	"session_value": {Change: in(pol, "session: plan\n", "session: plans\n"),
		Want: []string{`policy.yaml: classes.planner: session "plans", want one of [code plan report]`}},
	"session_missing": {Change: in(pol, "    session: plan\n", ""),
		Want: []string{"policy.yaml: classes.planner: session is required, one of [code plan report]"}},
	"stage_unknown": {Change: in(spec, merge+"merge:", merge+"  - {name: lint, on_findings: agent, enabled: true}\nmerge:"),
		Want: []string{`processes/spec.yaml: stages: stage 10: unknown name "lint" (known: [checks screening reproduction policy migration review audit approval merge])`}},
	"workflow_yaml": {Change: func(f map[string][]byte) {
		f["workflow.yaml"] = []byte("# yaml-language-server: $schema=schema/workflow.schema.json\nstages:\n" +
			"  - {name: policy, on_findings: fail, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n" + merge)
	}, Want: []string{`workflow.yaml: unknown file "workflow.yaml": the tree holds policy.yaml, commands.yaml, processes/<name>.yaml, projects/<name>.yaml, templates/<name>/, routines/<name>.md and schema/`}},

	// P1 to P3: projects and their processes
	"project_process_unknown": {Change: in("projects/linkshort.yaml", "process: spec", "process: specs"),
		Want: []string{`projects/linkshort.yaml: process "specs": no file processes/specs.yaml (processes: maintenance, spec)`}},
	"coder_without_process": {Change: in("projects/demo-app.yaml", "process: maintenance\n", ""),
		Want: []string{"projects/demo-app.yaml: agents coder needs process, the name of a file under processes/"}},
	"none_with_process": {Change: in("projects/ai-software-factory-infra.yaml", "agents: none\n", "agents: none\nprocess: maintenance\n"),
		Want: []string{"projects/ai-software-factory-infra.yaml: agents none: process is not allowed, no card starts there"}},

	// N: needs met in order
	"ready_session": {Change: in(spec, "{name: ready, class: coder", "{name: ready, class: analyzer"),
		Want: []string{"processes/spec.yaml: lanes.ready: class analyzer has session report; lane ready needs a class with session code"}},
	"plan_session": {Change: in(spec, "    class: planner\n", "    class: coder\n"),
		Want: []string{"processes/spec.yaml: lanes.plan: class coder has session code; lane plan needs a class with session plan"}},
	"write_up_session": {Change: in(maint, "write_up: analyzer", "write_up: coder"),
		Want: []string{"processes/maintenance.yaml: lanes.backlog: write_up coder has session code; a write-up needs a class with session report"}},
	"write_up_unknown": {Change: in(maint, "write_up: analyzer", "write_up: writer"),
		Want: []string{`processes/maintenance.yaml: lanes.backlog: write_up "writer" is not a class of policy.yaml (classes: analyzer, coder, planner)`}},
	"plan_needs_dependencies": {Change: in(spec, "dependencies: {wait_until: done, on_failed: human}\n", ""),
		Want: []string{"processes/spec.yaml: lanes.plan: needs the block dependencies before it: the plan's cards wait on each other in lane ready"}},
	"dependencies_without_plan": {Change: in(maint, ready, ready+"dependencies: {wait_until: done, on_failed: human}\n"),
		Want: []string{"processes/maintenance.yaml: dependencies: no lane plan makes dependencies: remove the block or add the lane"}},
	"audit_stage": {Change: in(maint, "  - {name: audit, on_findings: human, enabled: true}\n", ""),
		Want: []string{"projects/ai-software-factory-code.yaml: audit: process maintenance lists no audit stage"}},
	"review_model": {Change: in(pol, "    review_model: review-frontier\n", ""),
		Want: []string{"processes/maintenance.yaml: lanes.ready: class coder names no review_model, which the review stage calls",
			"processes/spec.yaml: lanes.ready: class coder names no review_model, which the review stage calls"}},
	"audit_model": {Change: in(pol, "    audit_model: coding-frontier\n", ""),
		Want: []string{"processes/maintenance.yaml: lanes.ready: class coder names no audit_model, which the audit stage calls for project ai-software-factory-code (audit: true)"}},
	"supervisor_model": {Change: in(pol, "    session: plan\n    models: [coding-cheap, coding-frontier]\n    supervisor_model: supervisor\n", "    session: plan\n    models: [coding-cheap, coding-frontier]\n"),
		Want: []string{"processes/spec.yaml: lanes.plan: class planner names no supervisor_model, which the session supervisor calls"}},

	// F: the floor no process may lower
	"checks_required": {Change: in(spec, "  - {name: checks, on_findings: agent, enabled: true}\n", ""),
		Want: []string{"processes/spec.yaml: stages: stage checks is required: no merge without green checks (floor)"}},
	"checks_when": {Change: in(spec, "{name: checks, on_findings: agent, enabled: true}", "{name: checks, on_findings: agent, enabled: true, when: outsider}"),
		Want: []string{"processes/spec.yaml: stages.checks: must run for every task (enabled, no when)"}},
	"policy_required": {Change: in(spec, "  - {name: policy, on_findings: fail, enabled: true}\n", ""),
		Want: []string{"processes/spec.yaml: stages: stage policy is required"}},
	"policy_not_fail": {Change: in(spec, "{name: policy, on_findings: fail", "{name: policy, on_findings: agent"),
		Want: []string{"processes/spec.yaml: stages.policy: on_findings must be fail: a forbidden path fails the task (floor)"}},
	"approval_required": {Exact: true, Change: in(spec, "  - {name: approval, on_findings: human, enabled: true}\n", ""),
		Want: []string{"processes/spec.yaml: stages: stage approval is required"}},
	"approval_findings": {Change: in(spec, "{name: approval, on_findings: human", "{name: approval, on_findings: agent"),
		Want: []string{"processes/spec.yaml: stages.approval: on_findings must be human"}},
	"approval_before_merge": {Change: func(f map[string][]byte) {
		edit(f, spec, rev, "")
		edit(f, spec, merge, rev+merge)
	}, Want: []string{"processes/spec.yaml: stages: stage approval must come right before merge"}},
	"merge_last": {Change: func(f map[string][]byte) {
		const repro = "  - {name: reproduction, on_findings: agent, enabled: true}\n"
		edit(f, spec, repro, "")
		edit(f, spec, merge, merge+repro)
	}, Want: []string{"processes/spec.yaml: stages: stage merge must be last"}},
	"review_when": {Change: in(spec, "{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent, enabled: true, when: outsider}"),
		Want: []string{"processes/spec.yaml: stages.review: when is not allowed: a review runs for every task, or enabled: false turns it off"}},
	"project_looser": {Change: in("projects/linkshort.yaml", "agents: coder\n", "agents: coder\nquota: {cpu: \"5\", memory: 1Gi, storage: 1Gi}\n"),
		Want: []string{`projects/linkshort.yaml: project linkshort: quota.cpu "5" is looser than policy.yaml's project_default.quota.cpu "4"`}},
	"checks_findings": {Change: in(spec, "{name: checks, on_findings: agent", "{name: checks, on_findings: human"),
		Want: []string{"processes/spec.yaml: stages.checks: on_findings must be agent or fail: no merge without green checks (floor)"}},

	// the ordering rules and the stage shape, kept from workflow.yaml
	"screening_first": {Change: func(f map[string][]byte) {
		const scr = "  - {name: screening, on_findings: human, enabled: true, when: outsider}\n"
		const pl = "  - {name: policy, on_findings: fail, enabled: true}\n"
		edit(f, spec, scr+pl, pl+scr)
	}, Want: []string{"processes/spec.yaml: stages: stage screening must be first: it runs before the first session"}},
	"migration_after_policy": {Change: func(f map[string][]byte) {
		const pl = "  - {name: policy, on_findings: fail, enabled: true}\n"
		const mig = "  - {name: migration, on_findings: human, enabled: true}\n"
		edit(f, spec, pl+mig, mig+pl)
	}, Want: []string{"processes/spec.yaml: stages: stage migration must come after policy, which finds the migrations"}},
	"audit_order": {Change: func(f map[string][]byte) {
		const audit = "  - {name: audit, on_findings: human, enabled: true}\n"
		edit(f, spec, rev+audit, audit+rev)
	}, Want: []string{"processes/spec.yaml: stages: stage audit must come between review and approval: it reads the reviewed head"}},
	"max_returns_zero": {Change: in(spec, "{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent, enabled: true, max_returns: 0}"),
		Want: []string{"processes/spec.yaml: stages.review: max_returns must be at least 1, is 0"}},
	"max_returns_not_agent": {Change: in(spec, "{name: migration, on_findings: human, enabled: true}", "{name: migration, on_findings: human, enabled: true, max_returns: 2}"),
		Want: []string{"processes/spec.yaml: stages.migration: max_returns needs on_findings agent"}},
	"stage_twice": {Change: in(spec, rev, rev+rev),
		Want: []string{"processes/spec.yaml: stages: stage review is listed twice"}},
	"merge_required": {Change: in(spec, merge, ""),
		Want: []string{"processes/spec.yaml: stages: stage merge is required"}},
	"policy_disabled": {Change: in(spec, "{name: policy, on_findings: fail, enabled: true}", "{name: policy, on_findings: fail, enabled: false}"),
		Want: []string{"processes/spec.yaml: stages.policy: must run for every task (enabled, no when)"}},
	"merge_findings": {Change: in(spec, "{name: merge, on_findings: human", "{name: merge, on_findings: agent"),
		Want: []string{"processes/spec.yaml: stages.merge: on_findings must be human"}},
	"screening_findings": {Change: in(spec, "{name: screening, on_findings: human", "{name: screening, on_findings: fail"),
		Want: []string{"processes/spec.yaml: stages.screening: on_findings must be human"}},
	"audit_findings": {Change: in(spec, "{name: audit, on_findings: human", "{name: audit, on_findings: agent"),
		Want: []string{"processes/spec.yaml: stages.audit: on_findings must be human"}},
	"findings_value": {Change: in(spec, "{name: migration, on_findings: human", "{name: migration, on_findings: maybe"),
		Want: []string{`processes/spec.yaml: stages.migration: on_findings "maybe", want one of [agent human fail]`}},
	"when_value": {Change: in(spec, "when: outsider", "when: insider"),
		Want: []string{`processes/spec.yaml: stages.screening: when "insider", want one of [outsider]`}},
	"checks_disabled": {Exact: true, Change: in(spec, "{name: checks, on_findings: agent, enabled: true}", "{name: checks, on_findings: agent, enabled: false}"),
		Want: []string{"processes/spec.yaml: stages.checks: must run for every task (enabled, no when)"}},
	"merge_when": {Exact: true, Change: in(spec, "{name: merge, on_findings: human, enabled: true}", "{name: merge, on_findings: human, enabled: true, when: outsider}"),
		Want: []string{"processes/spec.yaml: stages.merge: must run for every task (enabled, no when)"}},
	"audit_without_review": {Exact: true, Change: in(spec, rev, ""),
		Want: []string{"processes/spec.yaml: stages: stage audit must come between review and approval: it reads the reviewed head"}},
	"audit_after_approval": {Exact: true, Change: func(f map[string][]byte) {
		const audit = "  - {name: audit, on_findings: human, enabled: true}\n"
		edit(f, spec, audit, "")
		edit(f, spec, merge, audit+merge)
	}, Want: []string{"processes/spec.yaml: stages: stage approval must come right before merge",
		"processes/spec.yaml: stages: stage audit must come between review and approval: it reads the reviewed head"}},
	"ready_session_only": {Exact: true, Change: in(spec, "{name: ready, class: coder", "{name: ready, class: analyzer"),
		Want: []string{"processes/spec.yaml: lanes.ready: class analyzer has session report; lane ready needs a class with session code"}},
	"nested_process": {Change: func(f map[string][]byte) { f["processes/x/spec.yaml"] = f[spec] },
		Want: []string{`processes/x/spec.yaml: unknown file "processes/x/spec.yaml": the tree holds policy.yaml, commands.yaml, processes/<name>.yaml, projects/<name>.yaml, templates/<name>/, routines/<name>.md and schema/`}},
}

const (
	link = "projects/linkshort.yaml"
	ci   = "templates/ts-go/.forgejo/workflows/ci.yml"
)

// TemplateRefusals are the refusals of the templates and of what a
// project file names in them (P4 to P6, T1, T2), by subtest name.
var TemplateRefusals = map[string]Refusal{
	"template_unknown": {Exact: true, Change: in(link, "template: ts-go\n", "template: ts-go2\n"),
		Want: []string{`projects/linkshort.yaml: template "ts-go2": no directory templates/ts-go2/ (templates: ts-go)`}},
	"template_on_seed": {Exact: true, Change: in("projects/ai-software-factory-code.yaml", "process: maintenance\n", "process: maintenance\ntemplate: ts-go\n"),
		Want: []string{"projects/ai-software-factory-code.yaml: objects seed: template is registration's, which a seed project's infra repository owns"}},
	"template_env_path": {Exact: true, Change: in(link, "path: deploy/staging}", "path: deploy/stage}"),
		Want: []string{"projects/linkshort.yaml: environment linkshort-staging: path deploy/stage is not in templates/ts-go"}},
	"template_required_file": {Exact: true, Change: func(f map[string][]byte) { delete(f, "templates/ts-go/.forgejo/workflows/pr.yml") },
		Want: []string{"templates/ts-go: .forgejo/workflows/pr.yml is required: a template carries the pull request checks"}},
	"template_render": {Exact: true, Change: in(ci, "IMAGE: [[.Image]]", "IMAGE: [[.Imagee]]"),
		Want: []string{`templates/ts-go/.forgejo/workflows/ci.yml: template: ci.yml:14:11: executing "ci.yml" at <.Imagee>: can't evaluate field Imagee in type policy.TemplateData`}},
	"template_parse": {Exact: true, Change: in(ci, "branches: [main]", "branches: [[main]]"),
		Want: []string{`templates/ts-go/.forgejo/workflows/ci.yml: template: ci.yml:12: function "main" not defined`}},
	"template_dir_name": {Exact: true, Change: func(f map[string][]byte) { f["templates/TS/AGENTS.md"] = []byte("x\n") },
		Want: []string{"templates/TS/AGENTS.md: the template's directory name: lowercase letters, digits and dashes, at most 37"}},
	"template_loose_file": {Exact: true, Change: func(f map[string][]byte) { f["templates/AGENTS.md"] = []byte("x\n") },
		Want: []string{`templates/AGENTS.md: unknown file "templates/AGENTS.md": the tree holds policy.yaml, commands.yaml, processes/<name>.yaml, projects/<name>.yaml, templates/<name>/, routines/<name>.md and schema/`}},
	"template_not_text": {Exact: true, Change: func(f map[string][]byte) { f["templates/ts-go/logo.png"] = []byte{0x89, 'P', 'N', 'G', 0xff} },
		Want: []string{"templates/ts-go/logo.png: not UTF-8 text: a template holds text files"}},
}
