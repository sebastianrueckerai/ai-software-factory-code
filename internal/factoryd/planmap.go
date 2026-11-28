package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/answers"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// mapKey is the key of the plan draft's object that holds the repository
// map beside the plan parts a planner filed.
const mapKey = "map"

// draftMap is the map step's filing as the plan draft keeps it. Head is the
// commit it describes: "" until planMap stamps it after the session.
type draftMap struct {
	Head   string            `json:"head"`
	Answer answers.MapAnswer `json:"answer"`
}

// splitMap is the draft's plan without its map, and the map when it holds one.
func splitMap(plan json.RawMessage) (json.RawMessage, *draftMap, error) {
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(plan, &parts); err != nil {
		return nil, nil, fmt.Errorf("the plan draft is no object: %w", err)
	}
	raw, ok := parts[mapKey]
	if !ok {
		return plan, nil, nil
	}
	var m draftMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("the plan draft's map: %w", err)
	}
	delete(parts, mapKey)
	rest, err := json.Marshal(parts)
	return rest, &m, err
}

// withMap is plan with m as its map, or without one for nil.
func withMap(plan json.RawMessage, m *draftMap) (json.RawMessage, error) {
	parts := map[string]json.RawMessage{}
	if len(plan) > 0 {
		if err := json.Unmarshal(plan, &parts); err != nil {
			return nil, fmt.Errorf("the plan draft is no object: %w", err)
		}
	}
	delete(parts, mapKey)
	if m != nil {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		parts[mapKey] = raw
	}
	return json.Marshal(parts)
}

// heldMap is the map task taskID's plan draft holds, nil for none.
func (r *Runner) heldMap(ctx context.Context, taskID string) (*draftMap, error) {
	d, ok, err := r.store.PlanDraft(ctx, taskID)
	if err != nil || !ok {
		return nil, err
	}
	_, m, err := splitMap(d.Plan)
	return m, err
}

// setMap keeps m as the map of taskID's plan draft, beside the plan and
// the findings it holds; nil drops the map.
func (r *Runner) setMap(ctx context.Context, taskID string, m *draftMap) error {
	d, _, err := r.store.PlanDraft(ctx, taskID)
	if err != nil {
		return err
	}
	plan, err := withMap(d.Plan, m)
	if err != nil {
		return err
	}
	return r.store.SetPlanDraft(ctx, taskID, plan, append([]string{}, d.Findings...))
}

// settleDraft ends the draft a stored revision settles and keeps its map:
// the next path of the plan on the same commit reads it.
func (r *Runner) settleDraft(ctx context.Context, taskID string, held *draftMap) error {
	if err := r.store.DeletePlanDraft(ctx, taskID); err != nil || held == nil {
		return err
	}
	return r.setMap(ctx, taskID, held)
}

// planRun is what a plan session shares with the map session before it:
// the planner's model, whose provider the map session keeps, the policy
// version the task pinned and the headroom the activity holds for raises.
type planRun struct {
	model       string
	pin         int64
	room, added time.Duration
}

// planMap returns the map for head, running the step when the draft holds none for it.
// The step is a report session of the plan task on the small alias that starts
// from head, reads the repository's root through the worker's map and
// files its map with file_map, which keeps it in the draft; planMap then
// stamps head on it. A session that files none is an error.
func (r *Runner) planMap(ctx context.Context, t Task, repo Repo, head string, run planRun) (string, error) {
	held, err := r.heldMap(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if held != nil && held.Head == head {
		return renderMap(held.Answer), nil
	}
	if held != nil {
		if err := r.setMap(ctx, t.ID, nil); err != nil {
			return "", err
		}
	}
	t.SHA = head
	end, err := r.reportSession(ctx, t, repo, run.model, t.Standing(), "repo-map", run.pin, "", nil, nil,
		reportExtra{role: "repo-map", alias: policy.Small, directories: []string{"."}, room: run.room, added: run.added})
	if err != nil {
		return "", err
	}
	filed, err := r.heldMap(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if filed == nil {
		return "", fmt.Errorf("the map session of %s ended without filing a map: %s", t.ID, textutil.Truncate(strings.TrimSpace(cmp.Or(end.report, end.why)), 300))
	}
	filed.Head = head
	if err := r.setMap(ctx, t.ID, filed); err != nil {
		return "", err
	}
	return renderMap(filed.Answer), nil
}

// renderMap is the planner's text of a map: the directories with their
// roles, the conventions and the check, a section left out when it is empty.
func renderMap(a answers.MapAnswer) string {
	var b strings.Builder
	b.WriteString("Repository map, filed by the map step at the commit planning starts from:\nDirectories:")
	for _, d := range a.Directories {
		fmt.Fprintf(&b, "\n- %s: %s", d.Path, d.Role)
	}
	if len(a.Conventions) > 0 {
		b.WriteString("\nConventions every card follows:")
		for _, c := range a.Conventions {
			b.WriteString("\n- " + c)
		}
	}
	if a.Check != "" {
		b.WriteString("\nCheck: " + a.Check)
	}
	return b.String()
}

// fileMap is the hand-over of file_map: the plan task's running map step
// files its map into the plan draft, where planMap reads it.
func (r *Runner) fileMap(ctx context.Context, t Task, raw json.RawMessage) ([]string, error) {
	switch {
	case t.Track != policy.TrackPlan:
		return []string{policy.ToolFileMap + ": the task is not on the plan track"}, nil
	case t.Status != "running":
		return []string{policy.ToolFileMap + ": the task is " + t.Status + ", not running"}, nil
	}
	a, found := checkMapFiling(raw)
	if len(found) > 0 {
		return found, nil
	}
	return nil, r.setMap(ctx, t.ID, &draftMap{Answer: a})
}

// checkMapFiling is the map's filed answer checked and cleaned: every text
// on one line and cut at its bound, blank entries dropped, the lists
// capped. A map with no directory is refused.
func checkMapFiling(raw json.RawMessage) (answers.MapAnswer, []string) {
	var in answers.MapAnswer
	if found := decodeFiling(policy.ToolFileMap, raw, &in); found != nil {
		return answers.MapAnswer{}, found
	}
	line := func(s string, max int) string {
		return textutil.Truncate(strings.Join(strings.Fields(s), " "), max)
	}
	out := answers.MapAnswer{Directories: []answers.DirNote{}, Conventions: []string{}, Check: line(in.Check, answers.MaxMapCheck)}
	for _, d := range in.Directories {
		if p := line(d.Path, answers.MaxMapDirPath); p != "" && len(out.Directories) < answers.MaxMapDirs {
			out.Directories = append(out.Directories, answers.DirNote{Path: p, Role: line(d.Role, answers.MaxMapRole)})
		}
	}
	for _, c := range in.Conventions {
		if c = line(c, answers.MaxMapConvention); c != "" && len(out.Conventions) < answers.MaxMapConventions {
			out.Conventions = append(out.Conventions, c)
		}
	}
	if len(out.Directories) == 0 {
		return answers.MapAnswer{}, []string{policy.ToolFileMap + ": the map names no directory: file the directories that matter, each with its role"}
	}
	return out, nil
}
