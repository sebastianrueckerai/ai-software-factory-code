// Package prompts makes every text that enters an agent's context. Each
// step has a profile under profiles/, which names the guide, the forms,
// the chunks, the paragraph and the facts renderer its prompt joins, and
// the tools the step may call. Build is the one way to a Prompt.
package prompts

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// Profile is one step's choices: what enters its context and what it may call.
type Profile struct {
	Step        string   `yaml:"step"`      // the step's id, equal to the file name
	Purpose     string   `yaml:"purpose"`   // a policy.Purposes entry, or PurposeSession
	Reader      string   `yaml:"reader"`    // ReaderHuman, ReaderModel or ReaderCode
	Guide       bool     `yaml:"guide"`     // Sebastian's whole guide
	Forms       []string `yaml:"forms"`     // writingguide form names, in output order
	Chunks      []string `yaml:"chunks"`    // names of chunks/<name>.md, in output order
	Paragraph   string   `yaml:"paragraph"` // name of steps/<name>.md, "" for none
	Facts       string   `yaml:"facts"`     // the registered renderer
	Answer      string   `yaml:"answer"`    // the registered answer type, "" for none
	Tools       Tools    `yaml:"tools"`
	AgentsMD    bool     `yaml:"agents_md"`    // the harness loads the repository's AGENTS.md
	BudgetWords int      `yaml:"budget_words"` // the most words the step's fixture prompt holds
	// BudgetToolWords is the most words the step's chat tools offered on a
	// fresh thread hold, by ToolWords; 0 for a step with no chat tools.
	BudgetToolWords int `yaml:"budget_tool_words"`
}

// Tools are the tools a step may use. Every list is complete: a tool not
// named is not offered.
type Tools struct {
	Builtin  []string `yaml:"builtin"`  // a subset of BuiltinTools. Non-empty marks a harness session.
	Gateway  []string `yaml:"gateway"`  // a subset of policy.PaidTools and policy.FreeTools
	Chat     []string `yaml:"chat"`     // core chat tools: full schema on every turn
	Rare     []string `yaml:"rare"`     // chat tools behind tool_schema: one index line until fetched
	Attached []string `yaml:"attached"` // chat tools offered only while the thread holds an attachment
}

const (
	ReaderHuman    = "human"   // a human reads the output: guide or forms are required
	ReaderModel    = "model"   // another model reads it: no guide, no forms
	ReaderCode     = "code"    // code parses it: no guide, no forms
	PurposeSession = "session" // the purpose of a step the harness runs
)

// BuiltinTools are opencode's built-in tools a profile can keep. `edit`
// stands for edit and write, which one permission switches. The tools of
// alwaysDenied are off for every step and are not listed.
var BuiltinTools = []string{"bash", "edit", "glob", "grep", "read", "todowrite"}

// writing is the guide and the forms a catalog joins.
type writing struct {
	Guide string
	Form  func(string) (string, bool)
}

// catalog is every profile with the texts they name.
type catalog struct {
	writing
	profiles   map[string]Profile
	chunks     map[string]string
	paragraphs map[string]string
}

//go:embed chunks profiles all:steps
var embedded embed.FS

var texts = func() *catalog {
	c, err := load(embedded, writing{writingguide.Guide, writingguide.Form})
	if err != nil {
		panic(err)
	}
	return c
}()

// Profiles returns every profile, sorted by step.
func Profiles() []Profile { return texts.sorted() }

// ProfileOf is step's profile, and whether it has one.
func ProfileOf(step string) (Profile, bool) {
	p, ok := texts.profiles[step]
	return p, ok
}

func (c *catalog) sorted() []Profile {
	var out []Profile
	for _, step := range slices.Sorted(maps.Keys(c.profiles)) {
		out = append(out, c.profiles[step])
	}
	return out
}

func load(fsys fs.FS, w writing) (*catalog, error) {
	c := &catalog{writing: w, profiles: map[string]Profile{}}
	var err error
	if c.chunks, err = readTexts(fsys, "chunks"); err != nil {
		return nil, err
	}
	if c.paragraphs, err = readTexts(fsys, "steps"); err != nil {
		return nil, err
	}
	files, err := fs.Glob(fsys, "profiles/*.yaml")
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		b, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		var p Profile
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("empty document")
			}
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if name := strings.TrimSuffix(path.Base(file), ".yaml"); p.Step != name {
			return nil, fmt.Errorf("%s: step %s, want %s", file, p.Step, name)
		}
		if err := c.check(p); err != nil {
			return nil, err
		}
		c.profiles[p.Step] = p
	}
	return c, nil
}

// readTexts reads dir/<name>.md, trimmed, by name; no dir is none.
func readTexts(fsys fs.FS, dir string) (map[string]string, error) {
	files, err := fs.Glob(fsys, dir+"/*.md")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, file := range files {
		b, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(path.Base(file), ".md")] = strings.TrimSpace(string(b))
	}
	return out, nil
}

func (c *catalog) check(p Profile) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("step %s: "+format, append([]any{p.Step}, args...)...)
	}
	if p.Purpose != PurposeSession && !slices.Contains(policy.Purposes, p.Purpose) {
		return bad("purpose %q is neither a policy purpose nor %s", p.Purpose, PurposeSession)
	}
	if !slices.Contains([]string{ReaderHuman, ReaderModel, ReaderCode}, p.Reader) {
		return bad("reader %q, want %s, %s or %s", p.Reader, ReaderHuman, ReaderModel, ReaderCode)
	}
	if p.Facts == "" {
		return bad("facts names no renderer")
	}
	if p.BudgetWords <= 0 {
		return bad("budget_words is %d, want the most words its fixture prompt holds", p.BudgetWords)
	}
	if p.Answer != "" {
		return bad("answer %q is not a registered answer type", p.Answer)
	}
	for _, list := range []struct {
		key   string
		names []string
	}{{"forms", p.Forms}, {"chunks", p.Chunks}, {"tools.builtin", p.Tools.Builtin}, {"tools.gateway", p.Tools.Gateway},
		{"tools.chat", p.Tools.Chat}, {"tools.rare", p.Tools.Rare}, {"tools.attached", p.Tools.Attached}} {
		for i, name := range list.names {
			if slices.Contains(list.names[:i], name) {
				return bad("%s name %q twice", list.key, name)
			}
		}
	}
	switch {
	case p.Reader == ReaderHuman && !p.Guide && len(p.Forms) == 0:
		return bad("reader %s carries neither the guide nor a form", p.Reader)
	case p.Reader != ReaderHuman && len(p.Forms) > 0:
		return bad("reader %s carries forms", p.Reader)
	case p.Reader != ReaderHuman && p.Guide:
		return bad("reader %s carries the guide", p.Reader)
	}
	for _, f := range p.Forms {
		if _, ok := c.Form(f); !ok {
			return bad("form %q is not a form of the writing guide", f)
		}
	}
	for _, name := range p.Chunks {
		if _, ok := c.chunks[name]; !ok {
			return bad("chunk %q has no file chunks/%s.md", name, name)
		}
	}
	if _, ok := c.paragraphs[p.Paragraph]; p.Paragraph != "" && !ok {
		return bad("paragraph %q has no file steps/%s.md", p.Paragraph, p.Paragraph)
	}
	for _, tool := range p.Tools.Builtin {
		if !slices.Contains(BuiltinTools, tool) {
			return bad("builtin tool %q is not one of %v", tool, BuiltinTools)
		}
	}
	for _, tool := range p.Tools.Gateway {
		if !slices.Contains(policy.PaidTools, tool) && !slices.Contains(policy.FreeTools, tool) {
			return bad("gateway tool %q is not a policy tool", tool)
		}
	}
	for _, tool := range slices.Concat(p.Tools.Chat, p.Tools.Rare, p.Tools.Attached) {
		if !slices.Contains(policy.ChatTools, tool) {
			return bad("chat tool %q is not one of policy.ChatTools", tool)
		}
	}
	for _, pair := range [][2]string{{"chat", "rare"}, {"chat", "attached"}, {"rare", "attached"}} {
		lists := map[string][]string{"chat": p.Tools.Chat, "rare": p.Tools.Rare, "attached": p.Tools.Attached}
		for _, tool := range lists[pair[0]] {
			if slices.Contains(lists[pair[1]], tool) {
				return bad("chat tool %q is in tools.%s and tools.%s", tool, pair[0], pair[1])
			}
		}
	}
	harness := len(p.Tools.Builtin) > 0
	switch {
	case len(p.Tools.Rare)+len(p.Tools.Attached) > 0 && (len(p.Tools.Chat) == 0 || harness):
		return bad("tools.rare and tools.attached need tools.chat and no tools.builtin: they are the console chat's tiers")
	case len(p.Tools.Gateway) > 0 && !harness:
		return bad("tools.gateway needs tools.builtin: only a harness session calls the gateway's tools")
	case p.AgentsMD && !harness:
		return bad("agents_md needs tools.builtin: only the harness loads AGENTS.md")
	case len(p.Tools.Chat) > 0 && harness:
		return bad("tools.chat with tools.builtin: a harness session has no chat tools")
	case p.BudgetToolWords < 0 || (p.BudgetToolWords > 0) != (len(p.Tools.Chat) > 0):
		return bad("budget_tool_words is %d with %d chat tools, want the most words its chat tools hold, and 0 without chat tools", p.BudgetToolWords, len(p.Tools.Chat))
	}
	return nil
}
