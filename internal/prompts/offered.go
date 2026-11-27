package prompts

import "slices"

// Offered is the gateway tools a session of step may call: the tools the
// task's classes allow (Document.Tools), in the order given, that the step's
// profile names. The brief, SandboxFacts and the harness read this one list.
func Offered(step string, allowed []string) []string { return texts.offered(step, allowed) }

func (c *catalog) offered(step string, allowed []string) []string {
	named := c.profiles[step].Tools.Gateway
	out := []string{}
	for _, tool := range allowed {
		if slices.Contains(named, tool) {
			out = append(out, tool)
		}
	}
	return out
}
