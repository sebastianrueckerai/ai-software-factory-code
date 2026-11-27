package prompts

import (
	"slices"
	"testing"
	"testing/fstest"
)

const sessionProfile = `step: s
purpose: session
reader: model
guide: false
forms: []
chunks: [s]
paragraph: ""
facts: text
answer: ""
tools:
  builtin: [bash, read]
  gateway: [digest]
  chat: []
agents_md: false
budget_words: 1
`

// A harness session is offered only what its profile names: 6 of the 8
// built-in tools and 21 of the 22 gateway tools (4 paid, 18 free) are
// denied, 27 keys, and its standing text replaces the harness preamble.
func TestSessionConfigDeniesWhatTheProfileDrops(t *testing.T) {
	c, err := load(fstest.MapFS{"profiles/s.yaml": {Data: []byte(sessionProfile)}, "chunks/s.md": {Data: []byte("S\n")}}, testWriting)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.sessionConfig("s")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg["agent"].(map[string]any)["build"].(map[string]any)["prompt"]; got != "S" {
		t.Errorf("agent.build.prompt = %q, want S", got)
	}
	permission := cfg["permission"].(map[string]any)
	if len(permission) != 27 {
		t.Errorf("permission holds %d keys, want 27: %v", len(permission), permission)
	}
	for _, tool := range []string{"edit", "glob", "grep", "task", "todowrite", "webfetch", "factory-tools_web_search", "factory-tools_escalate"} {
		if permission[tool] != "deny" {
			t.Errorf("permission %s = %v, want deny", tool, permission[tool])
		}
	}
	for _, tool := range []string{"bash", "read", "factory-tools_digest"} {
		if _, ok := permission[tool]; ok {
			t.Errorf("permission denies %s, which the profile keeps", tool)
		}
	}
	env, err := c.sessionEnv("s")
	if err != nil || !slices.Equal(env, []string{"OPENCODE_DISABLE_PROJECT_CONFIG=1"}) {
		t.Errorf("sessionEnv = %q, %v; want the project config off", env, err)
	}
	c.profiles["s"] = Profile{Step: "s", Tools: Tools{Builtin: []string{"bash"}}, AgentsMD: true}
	if env, err := c.sessionEnv("s"); err != nil || len(env) != 0 {
		t.Errorf("sessionEnv with agents_md = %q, %v; want none", env, err)
	}
}
