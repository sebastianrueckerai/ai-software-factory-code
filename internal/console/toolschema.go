package console

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// ToolSchemaName is the one tool that fetches a rare tool's schema.
const ToolSchemaName = "tool_schema"

// chatTiers are the project agent's profile's tool tiers: the core tools,
// the rare ones behind tool_schema and the ones an attachment brings.
func chatTiers() prompts.Tools {
	p, _ := prompts.ProfileOf("project-agent")
	return p.Tools
}

// chatTier is the tier of the project agent's tool name: tool_schema
// stands in every request beside the core tools.
func chatTier(name string) string {
	tiers := chatTiers()
	switch {
	case name == ToolSchemaName || slices.Contains(tiers.Chat, name):
		return agentdoc.TierCore
	case slices.Contains(tiers.Rare, name):
		return agentdoc.TierRare
	case slices.Contains(tiers.Attached, name):
		return agentdoc.TierAttached
	}
	return ""
}

// threadTools are the rare tools the thread has fetched: the names of its
// rows' tool_schema calls that name a rare tool, in policy.ChatTools order.
func threadTools(rows []factoryd.ChatMessage) []string {
	rare := chatTiers().Rare
	fetched := map[string]bool{}
	for _, m := range rows {
		for _, c := range m.ToolCalls {
			if c.Name != ToolSchemaName {
				continue
			}
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal([]byte(c.Arguments), &in) == nil && slices.Contains(rare, in.Name) {
				fetched[in.Name] = true
			}
		}
	}
	return slices.DeleteFunc(slices.Clone(policy.ChatTools), func(name string) bool { return !fetched[name] })
}

// fetch adds name to the turn's fetched rare tools, kept in ChatTools order.
func (t *turn) fetch(name string) {
	if slices.Contains(t.fetched, name) {
		return
	}
	t.fetched = slices.DeleteFunc(slices.Clone(policy.ChatTools), func(n string) bool { return n != name && !slices.Contains(t.fetched, n) })
}

// offered builds the request's tool list: the profile's core tools, the
// attached tools when a row of the thread carries an attachment, the
// fetched rare tools, and tool_schema when any rare tool is not fetched,
// each one the turn's human holds the access table's roles for and none
// an agent tool on a project without agents.
func (t *turn) offered() []factoryd.OperatorTool {
	tiers := chatTiers()
	var core, attached, fetched, index []chatTool
	for _, tool := range t.c.chatTools() {
		if !t.offers(tool.name) || t.p.Agents == policy.AgentsNone && slices.Contains(agentTools, tool.name) {
			continue
		}
		switch {
		case slices.Contains(tiers.Chat, tool.name):
			core = append(core, tool)
		case slices.Contains(tiers.Attached, tool.name) && t.attached:
			attached = append(attached, tool)
		case slices.Contains(tiers.Rare, tool.name) && slices.Contains(t.fetched, tool.name):
			fetched = append(fetched, tool)
		case slices.Contains(tiers.Rare, tool.name):
			index = append(index, tool)
		}
	}
	var defs []factoryd.OperatorTool
	for _, tool := range slices.Concat(core, attached, fetched) {
		defs = append(defs, t.def(tool))
	}
	if len(index) > 0 {
		defs = append(defs, toolSchemaDef(index))
	}
	return defs
}

// def is tool's schema as the turn offers it, marked (needs OK) where the
// human decides it.
func (t *turn) def(tool chatTool) factoryd.OperatorTool {
	return tool.def(tool.memberOnly || slices.Contains(t.cfg.Confirm, tool.name))
}

// toolSchemaDef is tool_schema with the index of the rare tools not yet
// fetched, one name: about line each.
func toolSchemaDef(index []chatTool) factoryd.OperatorTool {
	lines := []string{"Fetch the full schema of one tool below, then call it."}
	for _, tool := range index {
		lines = append(lines, tool.name+": "+tool.about)
	}
	return factoryd.OperatorTool{Name: ToolSchemaName, Description: strings.Join(lines, "\n"), Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"name": str("the tool's name, as the list gives it")}, "required": []string{"name"}}}
}

// toolSchema answers a tool_schema call: the rare tool's schema as the
// next model call holds it, which the turn's tool list gains.
func (t *turn) toolSchema(args string) string {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "The arguments are no JSON object of " + ToolSchemaName + "'s parameters."
	}
	tiers := chatTiers()
	if slices.Contains(tiers.Chat, in.Name) || slices.Contains(tiers.Attached, in.Name) && t.attached || slices.Contains(t.fetched, in.Name) {
		return in.Name + " is already in your tool list."
	}
	rare := t.rareOffered()
	if !slices.Contains(rare, in.Name) {
		return fmt.Sprintf("There is no rare tool %s. The rare tools are: %s.", in.Name, strings.Join(rare, ", "))
	}
	tool, _ := t.c.chatTool(in.Name)
	b, err := json.Marshal(t.def(tool))
	if err != nil {
		return "The schema of " + in.Name + " could not be built: " + err.Error()
	}
	t.fetch(in.Name)
	return string(b)
}

// rareOffered are the rare tools the turn's human may use, in ChatTools
// order: the ones tool_schema fetches.
func (t *turn) rareOffered() []string {
	var rare []string
	for _, tool := range t.c.chatTools() {
		if slices.Contains(chatTiers().Rare, tool.name) && t.offers(tool.name) && (t.p.Agents != policy.AgentsNone || !slices.Contains(agentTools, tool.name)) {
			rare = append(rare, tool.name)
		}
	}
	return rare
}

// unfetched is true for a rare tool the human may use whose schema the
// thread has not fetched; one they may not use meets its own refusal.
func (t *turn) unfetched(name string) bool {
	return !slices.Contains(t.fetched, name) && slices.Contains(t.rareOffered(), name)
}
