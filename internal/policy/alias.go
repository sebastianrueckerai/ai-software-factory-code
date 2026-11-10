package policy

import (
	"maps"
	"slices"
)

// ToolAliases maps a tool's old name to its new one across a rename. The
// code that renames a tool adds old: new here in the same commit. The tree
// keeps the old name until that code runs everywhere, then moves to the
// new one. An alias leaves the table only when no tree names the old form.
var ToolAliases = map[string]string{}

// CanonicalTool returns the new name of an aliased old tool name, else the
// name itself.
func CanonicalTool(name string) string {
	if n, ok := ToolAliases[name]; ok {
		return n
	}
	return name
}

func canonicalKeys(m map[string]ToolCap) map[string]ToolCap {
	if m == nil {
		return nil
	}
	out := make(map[string]ToolCap, len(m))
	for k, v := range m {
		out[CanonicalTool(k)] = v
	}
	return out
}

func (c *ChatConfig) canonicalTools() {
	if c == nil {
		return
	}
	for i, t := range c.Confirm {
		c.Confirm[i] = CanonicalTool(t)
	}
	c.Tools = canonicalKeys(c.Tools)
}

// canonicalTools rewrites every tool name the document holds to its new
// form, so validation and every reader see new names only.
func (d *Document) canonicalTools() {
	d.Chat.canonicalTools()
	for name, c := range d.Classes {
		c.Tools = canonicalKeys(c.Tools)
		d.Classes[name] = c
	}
}

func (p *ProjectConfig) canonicalTools() { p.Chat.canonicalTools() }

// enumOf is the values the schema lists for a field: tool-name fields
// accept the old forms in ToolAliases too.
func enumOf(id string) ([]string, bool) {
	values, ok := enums[id]
	if ok && (id == "Class.Tools" || id == "ChatConfig.Confirm") {
		values = append(slices.Clone(values), slices.Sorted(maps.Keys(ToolAliases))...)
	}
	return values, ok
}
