package policy

import (
	"reflect"
	"strings"
	"testing"
)

const commandsFile = `# yaml-language-server: $schema=schema/commands.schema.json
commands:
  - name: standup
    words: what moved since yesterday and what waits on me
    message: Tell me in three lines what moved since yesterday and what waits on me.
`

// commands.yaml (roadmap 239): a name to a canned message, after the
// chat's own /compact and /new; a tree without the file has those only.
func TestCommandsFile(t *testing.T) {
	files := fixtureTree(t)
	bare, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.ChatCommands(); !reflect.DeepEqual(got, BuiltinCommands) {
		t.Fatalf("the commands of a tree without the file = %+v, want the chat's own", got)
	}
	files[FileCommands] = []byte(commandsFile)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{{Name: "compact", Words: "the older turns of this thread become a summary"}, {Name: "new", Words: "a new thread"},
		{Name: "standup", Words: "what moved since yesterday and what waits on me",
			Message: "Tell me in three lines what moved since yesterday and what waits on me."}}
	if got := tree.ChatCommands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChatCommands = %+v,\nwant %+v", got, want)
	}
	if m, ok := tree.CommandMessage("standup"); !ok || m != want[2].Message {
		t.Errorf("CommandMessage(standup) = %q, %v", m, ok)
	}
	if _, ok := tree.CommandMessage("compact"); ok {
		t.Errorf("CommandMessage(compact) found a message: the chat's own command has none")
	}
	if err := tree.Validate(); err != nil {
		t.Errorf("the stored tree does not validate: %v", err)
	}
}

// Each refusal names the file, the command and the rule.
func TestCommandsRefusals(t *testing.T) {
	for _, tt := range []struct {
		name, old, new, want string
	}{
		{"an upper-case name", "name: standup", "name: Standup",
			`commands.yaml: commands[0]: name "Standup": a lowercase letter, then lowercase letters, digits and dashes, at most 24`},
		{"the chat's own", "name: standup", "name: compact", `commands.yaml: commands[0]: name "compact": the chat's own command`},
		{"no words", "words: what moved since yesterday and what waits on me", `words: ""`, "commands.yaml: commands[0]: words: one line of 1 to 80 characters"},
		{"no message", "message: Tell me in three lines what moved since yesterday and what waits on me.", `message: " "`,
			"commands.yaml: commands[0]: message: 1 to 4000 characters"},
		{"a name twice", "    message: Tell me in three lines what moved since yesterday and what waits on me.\n",
			"    message: Tell me in three lines what moved since yesterday and what waits on me.\n  - name: standup\n    words: again\n    message: Again.\n",
			`commands.yaml: commands[1]: name "standup": named twice`},
		{"an unknown key", "    words:", "    hotkey: s\n    words:", "commands.yaml: yaml: unmarshal errors:"},
		{"the wrong first line", "schema/commands.schema.json", "schema/policy.schema.json",
			"commands.yaml: the first line must be # yaml-language-server: $schema=schema/commands.schema.json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := fixtureTree(t)
			files[FileCommands] = []byte(strings.Replace(commandsFile, tt.old, tt.new, 1))
			_, err := ParseTree(files)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseTree error = %v, want the line %q", err, tt.want)
			}
		})
	}
}
