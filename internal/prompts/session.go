package prompts

import (
	"fmt"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SessionConfig is the opencode config fragment for harness step: agent.build.prompt
// (the step's Standing) and permission, with a deny for every BuiltinTools entry
// the profile lacks and for every factory-tools_<tool> over policy.PaidTools and
// policy.FreeTools the profile lacks.
func SessionConfig(step string) (map[string]any, error) { return texts.sessionConfig(step) }

// SessionEnv is the process environment: OPENCODE_DISABLE_PROJECT_CONFIG=1
// when the profile has agents_md false.
func SessionEnv(step string) ([]string, error) { return texts.sessionEnv(step) }

func (c *catalog) session(step string) (Profile, error) {
	p, ok := c.profiles[step]
	switch {
	case !ok:
		return Profile{}, fmt.Errorf("step %s has no profile: write internal/prompts/profiles/%s.yaml", step, step)
	case len(p.Tools.Builtin) == 0:
		return Profile{}, fmt.Errorf("step %s is no harness session: its profile lists no tools.builtin", step)
	}
	return p, nil
}

func (c *catalog) sessionConfig(step string) (map[string]any, error) {
	p, err := c.session(step)
	if err != nil {
		return nil, err
	}
	standing, err := c.build(step, nil)
	if err != nil {
		return nil, err
	}
	permission := map[string]any{}
	for _, tool := range BuiltinTools {
		if !slices.Contains(p.Tools.Builtin, tool) {
			permission[tool] = "deny"
		}
	}
	for _, tool := range slices.Concat(policy.PaidTools, policy.FreeTools) {
		if !slices.Contains(p.Tools.Gateway, tool) {
			permission["factory-tools_"+tool] = "deny"
		}
	}
	return map[string]any{
		"agent":      map[string]any{"build": map[string]any{"prompt": standing.Standing()}},
		"permission": permission,
	}, nil
}

func (c *catalog) sessionEnv(step string) ([]string, error) {
	p, err := c.session(step)
	if err != nil || p.AgentsMD {
		return nil, err
	}
	return []string{"OPENCODE_DISABLE_PROJECT_CONFIG=1"}, nil
}
