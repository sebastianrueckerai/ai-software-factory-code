package factoryd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The labels intake reads off a new issue (roadmap 225): human files a
// person's card; agent or bug, or none of these, a card on the direct
// track; plan one on the plan track; backlog or improvement files none,
// the issue holding an interest until a writer's label commits it.
const (
	LabelHuman   = "human"
	LabelAgent   = "agent"
	LabelPlan    = "plan"
	LabelBacklog = "backlog"
)

// trackLabels are the track labels of tracks, which every project's
// repository carries beside BootLabels: plan only where there is a plan
// track.
func trackLabels(tracks policy.Tracks) map[string]string {
	out := map[string]string{LabelBacklog: "#c5def5"}
	if tracks.Plan != nil {
		out[LabelPlan] = "#5319e7"
	}
	return out
}

// IssueTrack is intake's track rule, the same for every process: human
// wins; then agent or bug asks for the direct track; then plan for the
// plan track where the process has one; then backlog or improvement, and
// plan where the process has no plan track, file no card (""); an issue
// with none of these labels is a direct card.
func IssueTrack(names []string, tracks policy.Tracks) string {
	has := func(n string) bool { return slices.Contains(names, n) }
	switch {
	case has(LabelHuman):
		return LabelHuman
	case has(LabelAgent) || has("bug"):
		return policy.TrackDirect
	case has(LabelPlan) && tracks.Plan != nil:
		return policy.TrackPlan
	case has(LabelPlan) || has(LabelBacklog) || has("improvement"):
		return ""
	}
	return policy.TrackDirect
}

// unboundSources are the sources of the agent cards the work bound leaves
// out: a roll and a factory pull request's card verify a pull request, a
// screening reads an issue, and none waits for a work slot.
var unboundSources = []string{SourceRoll, SourceGitHubSync, SourceSchemaCopy, SourceTemplateUpdate, SourceRevert, SourceScreening}

// Unbound reports whether agent card t runs outside the work bound.
func (t Task) Unbound() bool { return slices.Contains(unboundSources, t.Source) }

// boundCards is the SQL condition of the cards the work bound covers.
var boundCards = "kind = 'agent' AND source NOT IN ('" + strings.Join(unboundSources, "', '") + "')"

// trackClass is the class a card on track of project p's process starts.
func trackClass(p Project, track string) (string, bool) {
	tr, ok := p.Tracks.Track(track)
	return tr.Class, ok && tr.Class != ""
}

// errNoTrack refuses a card of project p, whose process has no direct
// track: a project on agents none, or one whose process is not in force.
func errNoTrack(p Project) error {
	return fmt.Errorf("project %s follows no process with a direct track", p.Name)
}

// fileEntry is the entry of agent task t, which factoryd files as the bot
// (FileAgentTask), into project p's process: queued on the direct track
// with its class. A roll and a factory pull request's card follow no
// process of their own: their run verifies the pull request linked to
// them, under the work bound's exemption (FileFactoryPRTask).
func fileEntry(p Project, t Task) (Task, error) {
	class, ok := trackClass(p, policy.TrackDirect)
	if !ok {
		return Task{}, errNoTrack(p)
	}
	t.Kind, t.Status, t.Track, t.RequiredCapabilities = "agent", "queued", policy.TrackDirect, []string{class}
	if !t.Unbound() {
		t.Process = p.Process
	}
	return t, nil
}
