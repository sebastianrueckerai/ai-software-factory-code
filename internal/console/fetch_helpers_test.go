package console

import (
	"encoding/json"
	"fmt"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"slices"
	"strings"
	"testing"
)

// fetchCall is call with a tool_schema call ahead of the first call to each
// rare tool, as the agent fetches a schema before it calls the tool. The
// ids count from first over the fetches and the calls together.
func fetchCall(first int, nameArgs ...string) string {
	return call(first, withFetches(nameArgs)...)
}

// withFetches is nameArgs with the tool_schema calls ahead of the first call
// to each rare tool.
func withFetches(nameArgs []string) []string {
	rare := chatTiers().Rare
	var out, fetched []string
	for i := 0; i < len(nameArgs); i += 2 {
		if name := nameArgs[i]; slices.Contains(rare, name) && !slices.Contains(fetched, name) {
			fetched = append(fetched, name)
			out = append(out, ToolSchemaName, fmt.Sprintf(`{"name":%q}`, name))
		}
	}
	return append(out, nameArgs...)
}

// inIndex is true when tool_schema's description of the request's tools
// indexes name.
func inIndex(tools []any, name string) bool {
	for _, tool := range tools {
		f := tool.(map[string]any)["function"].(map[string]any)
		if f["name"] == ToolSchemaName {
			return slices.ContainsFunc(strings.Split(f["description"].(string), "\n")[1:], func(l string) bool { return strings.HasPrefix(l, name+": ") })
		}
	}
	return false
}

// schemaResult is the result of a tool_schema call for name in project's
// chat: the tool's function JSON, marked (needs OK) as that chat's confirm
// list decides.
func schemaResult(t *testing.T, r *rig, project, name string) string {
	t.Helper()
	v, _ := r.c.inForce(t.Context())
	tool, ok := r.c.chatTool(name)
	if !ok {
		t.Fatalf("there is no tool %s", name)
	}
	cfg := v.Document.ChatOf(v.Projects[project])
	b, err := json.Marshal(tool.def(tool.memberOnly || slices.Contains(cfg.Confirm, name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedFetch stores an earlier turn of login's thread in which the agent
// fetched the schemas of names, for a human whose access no longer lets the
// turn offer the tools, or a test that needs every schema in the list.
func seedFetch(t *testing.T, chat *fakeChat, repository, login string, names ...string) {
	t.Helper()
	var calls []factoryd.ChatToolCall
	for i, name := range names {
		calls = append(calls, factoryd.ChatToolCall{ID: fmt.Sprintf("call_%d", i), Name: ToolSchemaName, Arguments: fmt.Sprintf(`{"name":%q}`, name), Result: "{}"})
	}
	for _, m := range []factoryd.ChatMessage{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "fetched", ToolCalls: calls}} {
		if _, err := chat.AddChatMessage(t.Context(), repository, login, m); err != nil {
			t.Fatal(err)
		}
	}
}

// schemaEvent is the stream's tool line of a tool_schema call for name in
// project's chat, its result as the stream cuts it to 300 characters.
func schemaEvent(t *testing.T, r *rig, project, name string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"arguments": map[string]string{"name": name}, "duration_ms": 0, "name": ToolSchemaName,
		"result": textutil.Truncate(schemaResult(t, r, project, name), 300)})
	if err != nil {
		t.Fatal(err)
	}
	return "tool " + string(b)
}

// fetchIn has the agent of a turn built by hand fetch the schemas of names
// through tool_schema, as a model does before it calls a rare tool; a core
// tool needs none.
func fetchIn(t *testing.T, trn *turn, names ...string) {
	t.Helper()
	for _, name := range names {
		if !slices.Contains(chatTiers().Rare, name) {
			continue
		}
		if res, _, _ := trn.call(factoryd.OperatorCall{Name: ToolSchemaName, Arguments: fmt.Sprintf(`{"name":%q}`, name)}); !strings.HasPrefix(res, `{"function":`) {
			t.Fatalf("the fetch of %s = %q, want its schema", name, res)
		}
	}
}
