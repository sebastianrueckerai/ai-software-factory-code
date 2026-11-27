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

// A harness session is offered only what its profile names: 4 of the 6
// built-in tools a profile may keep, the 3 tools no step keeps and 21 of
// the 22 gateway tools (4 paid, 18 free) are denied, 4 + 3 + 21 = 28
// keys, and its standing text replaces the harness preamble.
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
	if len(permission) != 28 {
		t.Errorf("permission holds %d keys, want 28: %v", len(permission), permission)
	}
	for _, tool := range []string{"edit", "glob", "grep", "todowrite", "skill", "task", "webfetch", "factory-tools_web_search", "factory-tools_escalate"} {
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

// skill, task and webfetch are off in every harness session, whatever
// its profile lists.
func TestSessionConfigAlwaysDeniesTheConstants(t *testing.T) {
	sessions := 0
	for _, p := range Profiles() {
		if len(p.Tools.Builtin) == 0 {
			continue
		}
		sessions++
		cfg, err := SessionConfig(p.Step)
		if err != nil {
			t.Fatalf("step %s: %v", p.Step, err)
		}
		permission := cfg["permission"].(map[string]any)
		for _, tool := range []string{"skill", "task", "webfetch"} {
			if permission[tool] != "deny" {
				t.Errorf("step %s: permission %s = %v, want deny", p.Step, tool, permission[tool])
			}
		}
	}
	if sessions != 6 {
		t.Errorf("%d profiles list built-in tools, want the 6 session steps", sessions)
	}
}
