package acceptance

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// fetchCall is Call with a tool_schema call ahead of the first call to each
// rare tool of the project agent, as the agent fetches a schema before it
// calls the tool. The ids count from first over the fetches and the calls
// together.
func fetchCall(first int, nameArgs ...string) string {
	var out, fetched []string
	for i := 0; i < len(nameArgs); i += 2 {
		if name := nameArgs[i]; isRare(name) && !slices.Contains(fetched, name) {
			fetched = append(fetched, name)
			out = append(out, console.ToolSchemaName, fmt.Sprintf(`{"name":%q}`, name))
		}
	}
	return Call(first, append(out, nameArgs...)...)
}

func isRare(tool string) bool {
	profile, _ := prompts.ProfileOf("project-agent")
	return slices.Contains(profile.Tools.Rare, tool)
}

// afterFetch is a turn's stream events without the tool_schema block that
// opens a turn calling a rare tool: it fails when a rare tool's turn does not
// open with that block naming the tool, or a core tool's turn holds one. The
// block's result is the schema in the thread's first turn and a note that it
// stands in a later one, so only its call is asserted.
func afterFetch(t *testing.T, events []string, tool string) []string {
	t.Helper()
	has := len(events) > 0 && strings.HasPrefix(events[0], `tool {"arguments":{"name":"`+tool+`"},"duration_ms":0,"name":"`+console.ToolSchemaName+`"`)
	if has != isRare(tool) {
		t.Fatalf("the turn of %s opens with a tool_schema block = %t, want %t:\n%s", tool, has, isRare(tool), strings.Join(events, "\n"))
	}
	if has {
		return events[1:]
	}
	return events
}

// afterFetchSSE is afterFetch over a parsed stream.
func afterFetchSSE(t *testing.T, stream []sseEvent, tool string) []sseEvent {
	t.Helper()
	has := len(stream) > 0 && stream[0].Kind == "tool" && stream[0].Data["name"] == console.ToolSchemaName &&
		stream[0].Data["arguments"].(map[string]any)["name"] == tool
	if has != isRare(tool) {
		t.Fatalf("the turn of %s opens with a tool_schema block = %t, want %t: %v", tool, has, isRare(tool), kindsOf(stream))
	}
	if has {
		return stream[1:]
	}
	return stream
}
