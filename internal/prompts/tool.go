package prompts

import (
	"encoding/json"
	"strings"
)

// Tool is a function tool in the OpenAI form.
type Tool struct {
	Name, Description string
	Parameters        map[string]any
}

// ToolWords is the words of a tool's description and of every description
// under its parameters, as the agent map counts a tool on the wire.
func ToolWords(t Tool) int {
	var params any
	if b, err := json.Marshal(t.Parameters); err == nil {
		json.Unmarshal(b, &params) // #nosec G104 -- the bytes are a marshalled value, which always parse
	}
	return words(t.Description) + descriptionWords(params)
}

func descriptionWords(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "description" {
				n += len(strings.Fields(s))
			} else {
				n += descriptionWords(e)
			}
		}
	case []any:
		for _, e := range x {
			n += descriptionWords(e)
		}
	}
	return n
}
