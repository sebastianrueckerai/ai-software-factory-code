package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/answers"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The planner's steps. The map step runs before each path's planner steps.
const (
	stepRepoMap      = "repo-map"
	stepPlanSpec     = "planner-spec"
	stepPlanCriteria = "planner-criteria"
	stepPlanCards    = "planner-cards"
	stepPlanReplan   = "planner-replan"
	stepPlanRevert   = "planner-revert"
)

// plannerSteps are the planner's sessions, in the order of the first path.
var plannerSteps = []string{stepPlanSpec, stepPlanCriteria, stepPlanCards, stepPlanReplan, stepPlanRevert}

// plannerTools is the filing tool of each planner step.
var plannerTools = map[string]string{
	stepPlanSpec:     policy.ToolFileSpec,
	stepPlanCriteria: policy.ToolFileCriteria,
	stepPlanCards:    policy.ToolFileCards,
	stepPlanReplan:   policy.ToolFileReplan,
	stepPlanRevert:   policy.ToolFileRevert,
}

// plannerStepOf is the planner step whose filing tool is tool.
func plannerStepOf(tool string) (string, bool) {
	for step, t := range plannerTools {
		if t == tool {
			return step, true
		}
	}
	return "", false
}

// PlanStepsFor are the steps a planning run executes, in order, for the
// plan's newest revision last (nil: none), the pending replan step and
// whether one is pending: the first path files a spec, criteria and cards,
// a round plans its cards on the filed spec, and a replan files the plan
// again; a replan that takes work off main first names the cards that
// come off.
func PlanStepsFor(last *Plan, step ReplanStep, pending bool) []string {
	switch {
	case last == nil:
		return []string{stepRepoMap, stepPlanSpec, stepPlanCriteria, stepPlanCards}
	case pending && step.Origin == ReplanRound:
		return []string{stepRepoMap, stepPlanCards}
	case pending && step.OffMain:
		return []string{stepPlanRevert, stepRepoMap, stepPlanReplan}
	}
	return []string{stepRepoMap, stepPlanReplan}
}

// planPath is PlanStepsFor under shape: a shape without an integration
// card holds no criteria, so the criteria step does not run.
func planPath(shape policy.PlanShape, last *Plan, step ReplanStep, pending bool) []string {
	steps := PlanStepsFor(last, step, pending)
	if !shape.Integration {
		steps = slices.DeleteFunc(steps, func(s string) bool { return s == stepPlanCriteria })
	}
	return steps
}

// notOnPath is the refusal of a filing of tool when the run holds no step for it: it names the tools of the run.
func notOnPath(tool string, path []string) []string {
	var names []string
	for _, s := range path {
		if t, ok := plannerTools[s]; ok {
			names = append(names, t)
		}
	}
	return []string{fmt.Sprintf("plan: %s files no part of this plan's run now: its steps are %s. File with the tool of your step", tool, strings.Join(names, ", "))}
}

// planFragment reads part into the plan's own fields: p holds the fields of the
// answer the part carries, whose keys are the plan's.
func planFragment(part json.RawMessage) (Plan, error) {
	var p Plan
	return p, json.Unmarshal(part, &p)
}

// specBody is the spec a filed answer holds, nil for one that holds none.
func specBody(s answers.SpecBody) *Spec {
	if strings.TrimSpace(s.Outcome) == "" && len(s.Glossary) == 0 {
		return nil
	}
	return &Spec{Outcome: s.Outcome, Glossary: s.Glossary}
}

// partDoor decodes part as step's answer and checks it at its own door,
// before the plan is whole: the findings, none when the part is sound.
// The first four steps read nothing of the plan's record; the revert
// reads the cards it may take off main.
func (r *Runner) partDoor(step string, part json.RawMessage, shape policy.PlanShape, st replanState) []string {
	tool := plannerTools[step]
	switch step {
	case stepPlanSpec:
		var a answers.SpecAnswer
		if found := decodeFiling(tool, part, &a); found != nil {
			return found
		}
		p, err := planFragment(part)
		if err != nil {
			return []string{tool + ": " + err.Error()}
		}
		p.Spec = specBody(a.Spec)
		return slices.Concat(specFindings(p, shape), questionFindings(p.Questions), questionBounds(p.Questions))
	case stepPlanCriteria:
		var a answers.CriteriaAnswer
		if found := decodeFiling(tool, part, &a); found != nil {
			return found
		}
		p, err := planFragment(part)
		if err != nil {
			return []string{tool + ": " + err.Error()}
		}
		return slices.Concat(criterionFindings(p.Criteria, shape), PromiseBounds(p.Criteria))
	case stepPlanRevert:
		var a answers.RevertAnswer
		if found := decodeFiling(tool, part, &a); found != nil {
			return found
		}
		return revertFindings(st, a)
	case stepPlanCards:
		var a answers.CardsAnswer
		return decodeFiling(tool, part, &a)
	case stepPlanReplan:
		var a answers.ReplanAnswer
		return decodeFiling(tool, part, &a)
	}
	return []string{tool + " files no part"}
}

// revertFindings are the findings on the cards a revert names: each key a
// merged card of the plan's record that may come off main, with a reason.
func revertFindings(st replanState, a answers.RevertAnswer) []string {
	old := map[string]Task{}
	for _, k := range st.old {
		old[fromRef(k)] = k
	}
	var found []string
	seen := map[string]bool{}
	for _, c := range a.Cards {
		if seen[c.Key] {
			found = append(found, fmt.Sprintf("plan: the card %s is named twice", c.Key))
			continue
		}
		seen[c.Key] = true
		if refusal := takeOffRefusal(st, old, c.Key); refusal != "" {
			found = append(found, "plan: "+refusal)
		}
		if strings.TrimSpace(c.Reason) == "" {
			found = append(found, fmt.Sprintf("plan: the card %s has no reason: say what breaks if its work stays on main, or leave %s out", c.Key, c.Key))
		}
	}
	return found
}

// planFiling is what a filing reads of its plan task once: the task, the
// version in force, the plan's record and the steps the run holds.
type planFiling struct {
	t     Task
	repo  Repo
	v     policy.Version
	shape policy.PlanShape
	st    replanState
	path  []string
}

func (r *Runner) readPlanFiling(ctx context.Context, t Task, repo Repo) (planFiling, error) {
	f := planFiling{t: t, repo: repo, v: r.cfg.Policy()}
	var err error
	if f.shape, _, err = planShape(f.v, t); err != nil {
		return planFiling{}, err
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
	if err != nil {
		return planFiling{}, fmt.Errorf("the timeline of %s: %w", t.ID, err)
	}
	if f.st, err = r.replanState(ctx, t, tl, 0); err != nil {
		return planFiling{}, err
	}
	step, pending, err := pendingIn(tl, t.ID)
	if err != nil {
		return planFiling{}, err
	}
	f.path = planPath(f.shape, f.st.newest, step, pending)
	return f, nil
}

// FilePart is the hand-over of a planner step's filing tools: part, the
// answer of step, checked at its own door and kept in the plan draft
// between sessions. The last step of the run's path judges the whole plan
// against the version in force and stores it as the task's next revision,
// which the run takes as its answer (storePlan). A refusal carries every
// finding back into the session and keeps the part with them. A filing of
// a step other than the one the run is at is refused and kept nowhere.
func (r *Runner) FilePart(ctx context.Context, taskID, step string, part json.RawMessage) (PlanResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	tool := plannerTools[step]
	switch {
	case t.Status != "running":
		return PlanResult{Findings: []string{"plan: the task is " + t.Status + ", not running"}}, nil
	case t.Track != policy.TrackPlan:
		return PlanResult{Findings: []string{"plan: the task is not on the plan track"}}, nil
	}
	f, err := r.readPlanFiling(ctx, t, repo)
	if err != nil {
		return PlanResult{}, err
	}
	if !slices.Contains(f.path, step) {
		return PlanResult{Findings: notOnPath(tool, f.path)}, nil
	}
	draft, err := r.planDraftState(ctx, taskID, f.path)
	if err != nil {
		return PlanResult{}, err
	}
	if at := draft.at(f.path); step != at {
		return PlanResult{Findings: []string{fmt.Sprintf("%s: the run is at its %s step: file it with %s", tool, strings.TrimPrefix(at, "planner-"), plannerTools[at])}}, nil
	}
	refuse := func(found []string) (PlanResult, error) {
		return PlanResult{Findings: found}, r.setPart(ctx, taskID, step, part, found)
	}
	if found := r.partDoor(step, part, f.shape, f.st); len(found) > 0 {
		return refuse(found)
	}
	if step != f.path[len(f.path)-1] {
		return PlanResult{}, r.setPart(ctx, taskID, step, part, nil)
	}
	parts, err := r.heldParts(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	parts[step] = part
	p, err := assemblePlan(f.path, f.st.newest, parts)
	if err != nil {
		return PlanResult{}, err
	}
	res, err := r.storePlan(ctx, f, p)
	if err == nil && len(res.Findings) > 0 {
		err = r.setPart(ctx, taskID, step, part, res.Findings)
	}
	return res, err
}

// assemblePlan is the plan the parts of a run's path make, as the planner
// files it: a first path's spec, criteria and cards, a round's cards on the
// spec and criteria of the newest revision, and a replan's plan with the
// cards a revert names.
func assemblePlan(path []string, newest *Plan, parts map[string]json.RawMessage) (Plan, error) {
	var p Plan
	if newest != nil && slices.Contains(path, stepPlanCards) {
		p = *newest
		p.Outline, p.Cards, p.Questions, p.TakeOffMain, p.TakeOffReasons = nil, nil, nil, nil, nil
	}
	for _, step := range path {
		raw, ok := parts[step]
		if !ok {
			continue
		}
		frag, err := planFragment(raw)
		if err != nil {
			return Plan{}, fmt.Errorf("the %s part: %w", step, err)
		}
		switch step {
		case stepPlanSpec:
			var a answers.SpecAnswer
			if err := json.Unmarshal(raw, &a); err != nil {
				return Plan{}, err
			}
			p.Spec, p.Summary = specBody(a.Spec), a.Summary
			p.Questions = append(p.Questions, frag.Questions...)
		case stepPlanCriteria:
			p.Criteria = frag.Criteria
		case stepPlanCards:
			p.Outline, p.Cards = frag.Outline, frag.Cards
			p.Questions = append(p.Questions, frag.Questions...)
		case stepPlanReplan:
			var a answers.ReplanAnswer
			if err := json.Unmarshal(raw, &a); err != nil {
				return Plan{}, err
			}
			p.Spec, p.Summary = specBody(a.Spec), a.Summary
			p.Outline, p.Criteria, p.Cards, p.Questions = frag.Outline, frag.Criteria, frag.Cards, frag.Questions
		case stepPlanRevert:
			var a answers.RevertAnswer
			if err := json.Unmarshal(raw, &a); err != nil {
				return Plan{}, err
			}
			for _, c := range a.Cards {
				p.TakeOffMain = append(p.TakeOffMain, c.Key)
				if p.TakeOffReasons == nil {
					p.TakeOffReasons = map[string]string{}
				}
				p.TakeOffReasons[c.Key] = c.Reason
			}
		}
	}
	return p, nil
}

// heldParts are the parts task taskID's plan draft holds, by step: the draft
// object's keys but the map's.
func (r *Runner) heldParts(ctx context.Context, taskID string) (map[string]json.RawMessage, error) {
	d, ok, err := r.store.PlanDraft(ctx, taskID)
	parts := map[string]json.RawMessage{}
	if err != nil || !ok {
		return parts, err
	}
	if err := json.Unmarshal(d.Plan, &parts); err != nil {
		return nil, fmt.Errorf("the plan draft is no object: %w", err)
	}
	delete(parts, mapKey)
	return parts, nil
}

// setPart keeps part as the draft's part of step, beside the map and the
// parts it holds, with findings: the ones the part was refused for, none
// for a sound part.
func (r *Runner) setPart(ctx context.Context, taskID, step string, part json.RawMessage, findings []string) error {
	d, _, err := r.store.PlanDraft(ctx, taskID)
	if err != nil {
		return err
	}
	all := map[string]json.RawMessage{}
	if len(d.Plan) > 0 {
		if err := json.Unmarshal(d.Plan, &all); err != nil {
			return fmt.Errorf("the plan draft is no object: %w", err)
		}
	}
	all[step] = part
	plan, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return r.store.SetPlanDraft(ctx, taskID, plan, append([]string{}, findings...))
}

// planDraftState is what task taskID's draft tells a run over path: the
// steps whose part it holds, and the step it was refused at, the last held
// step of path while the draft holds findings, "" for none.
type planDraftState struct {
	d       PlanDraft
	held    map[string]bool
	refused string
}

func (r *Runner) planDraftState(ctx context.Context, taskID string, path []string) (planDraftState, error) {
	d, _, err := r.store.PlanDraft(ctx, taskID)
	if err != nil {
		return planDraftState{}, err
	}
	parts, err := r.heldParts(ctx, taskID)
	if err != nil {
		return planDraftState{}, err
	}
	s := planDraftState{d: d, held: map[string]bool{}}
	for _, step := range path {
		if _, ok := parts[step]; ok {
			s.held[step] = true
			if len(d.Findings) > 0 {
				s.refused = step
			}
		}
	}
	if s.d.Plan != nil {
		if s.d.Plan, _, err = splitMap(d.Plan); err != nil {
			return planDraftState{}, err
		}
	}
	return s, nil
}

// runs tells whether a run over path runs its step i: the last always, an
// earlier one when the draft holds no part of it or its part was refused.
func (s planDraftState) runs(path []string, i int) bool {
	return i == len(path)-1 || !s.held[path[i]] || s.refused == path[i]
}

// at is the planner step a run over path is at: the first one it runs.
func (s planDraftState) at(path []string) string {
	for i, step := range path {
		if step != stepRepoMap && s.runs(path, i) {
			return step
		}
	}
	return ""
}

// earlierParts are the parts of the steps before step on the first path,
// as the planner of step reads them: what the spec and the criteria
// say, "" when step has none before it.
func (r *Runner) earlierParts(ctx context.Context, taskID, step string) (string, error) {
	var before []string
	switch step {
	case stepPlanCriteria:
		before = []string{stepPlanSpec}
	case stepPlanCards:
		before = []string{stepPlanSpec, stepPlanCriteria}
	}
	parts, err := r.heldParts(ctx, taskID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, s := range before {
		if raw, ok := parts[s]; ok {
			fmt.Fprintf(&b, "What the %s step filed:\n```json\n%s\n```\n\n", strings.TrimPrefix(s, "planner-"), raw)
		}
	}
	return strings.TrimSpace(b.String()), nil
}
