package policy

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The chat's commands (roadmap 239): commands.yaml maps a name to the
// canned message the member sends by typing /name. A command is a
// message and nothing more. /compact and /new are the chat's own.

// FileCommands is the tree's commands file; a tree without it has the
// chat's own commands only.
const FileCommands = "commands.yaml"

// BuiltinCommands are the chat's own commands with their words (C14).
var BuiltinCommands = []Command{
	{Name: "compact", Words: "the older turns of this thread become a summary"},
	{Name: "new", Words: "a new thread"},
}

// Commands is commands.yaml: the chat's commands.
type Commands struct {
	// Commands are the commands the chat offers after its own /compact
	// and /new.
	Commands []Command `yaml:"commands" json:"commands"`
}

// Command is one command: a name and the message it sends.
type Command struct {
	// Name is what the member types after the slash: a lowercase letter,
	// then lowercase letters, digits and dashes, at most 24, none of the
	// chat's own.
	Name string `yaml:"name" json:"name"`
	// Words say what the command asks, as one line of the palette.
	Words string `yaml:"words" json:"words"`
	// Message is the text the chat sends for /name; the chat's own
	// commands have none.
	Message string `yaml:"message" json:"message,omitempty"`
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// Validate checks the names (unique, none of the chat's own), the words
// and the messages.
func (c Commands) Validate() error {
	var errs []error
	for i, cmd := range c.Commands {
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("commands[%d]: "+format, append([]any{i}, a...)...))
		}
		switch {
		case !commandName.MatchString(cmd.Name):
			bad("name %q: a lowercase letter, then lowercase letters, digits and dashes, at most 24", cmd.Name)
		case slices.ContainsFunc(BuiltinCommands, func(b Command) bool { return b.Name == cmd.Name }):
			bad("name %q: the chat's own command", cmd.Name)
		case slices.ContainsFunc(c.Commands[:i], func(b Command) bool { return b.Name == cmd.Name }):
			bad("name %q: named twice", cmd.Name)
		}
		if w := strings.TrimSpace(cmd.Words); w == "" || len([]rune(w)) > 80 || strings.Contains(w, "\n") {
			bad("words: one line of 1 to 80 characters")
		}
		if m := strings.TrimSpace(cmd.Message); m == "" || len([]rune(m)) > 4000 {
			bad("message: 1 to 4000 characters")
		}
	}
	return errors.Join(errs...)
}

// ChatCommands are the commands the chat offers: its own, then the
// tree's.
func (t Tree) ChatCommands() []Command {
	return append(slices.Clone(BuiltinCommands), t.Commands.Commands...)
}

// CommandMessage is the message /name sends, false for a name that is no
// command of the tree's.
func (t Tree) CommandMessage(name string) (string, bool) {
	i := slices.IndexFunc(t.Commands.Commands, func(c Command) bool { return c.Name == name })
	if i < 0 {
		return "", false
	}
	return t.Commands.Commands[i].Message, true
}
