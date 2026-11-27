package main

import (
	"encoding/json"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// stepParts are the words by part of the prompts a's request is built
// from, each step's on its registered fixture, and the words of the
// standing texts a harness session carries as its system prompt.
func stepParts(a Agent) (sum prompts.Parts, standing int, err error) {
	fixtures := map[string]any{}
	for _, r := range prompts.Registered() {
		fixtures[r.Step] = r.Fixture
	}
	harness := map[string]bool{}
	for _, p := range prompts.Profiles() {
		harness[p.Step] = len(p.Tools.Builtin) > 0
	}
	for _, step := range a.Steps {
		p, err := prompts.Build(step, fixtures[step])
		if err != nil {
			return prompts.Parts{}, 0, err
		}
		got := p.Parts()
		sum.Guide, sum.Forms, sum.Chunks, sum.Paragraph, sum.Facts =
			sum.Guide+got.Guide, sum.Forms+got.Forms, sum.Chunks+got.Chunks, sum.Paragraph+got.Paragraph, sum.Facts+got.Facts
		if harness[step] {
			standing += got.Total() - got.Facts
		}
	}
	return sum, standing, nil
}

// wireWords are the words of a captured request no profile made, for a
// harness agent: its system text and its harness's own tools less the
// standing text of its steps; and the words of the gateway's and the
// chat's tools on the wire. A tool's words are its description's and its
// parameters' descriptions'.
func wireWords(a Agent, b agentdoc.Body, standing int) (harness, tools int) {
	for _, t := range b.Tools {
		n := toolWords(t.Raw)
		switch {
		case !a.Loop || strings.HasPrefix(t.Name, "factory-tools_"):
			tools += n
		default:
			harness += n
		}
	}
	if !a.Loop {
		return 0, tools
	}
	for _, m := range b.Messages {
		if m.Role == "system" {
			harness += len(strings.Fields(messageText(m)))
		}
	}
	return harness - standing, tools
}

func messageText(m agentdoc.Message) string {
	text := m.Content
	for _, p := range m.Parts {
		text += p.Text
	}
	return text
}

// toolWords are a wire tool's words, as prompts.ToolWords counts them.
func toolWords(raw json.RawMessage) int {
	var tool struct {
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tool) != nil {
		return 0
	}
	f := tool.Function
	return prompts.ToolWords(prompts.Tool{Name: f.Name, Description: f.Description, Parameters: f.Parameters})
}
