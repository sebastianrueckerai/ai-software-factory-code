package policy

import (
	"reflect"
	"strings"
	"testing"
)

// A tree without a chat block gives every project DefaultChat, and the
// operator purpose calls its model whatever the task's classes.
func TestChatDefaults(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	want := ChatConfig{Model: "operator", MonthlyEUR: 5,
		Confirm: []string{"new_card", "move_card", "start_plan", "retry", "accept_plan", "request_changes", "reject_plan", "approve_merge", "deny_merge"}}
	if got := tree.Document.ChatOf(tree.Projects["linkshort"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChatOf(linkshort) = %+v, want %+v", got, want)
	}
	if got := tree.Document.FactoryModel([]string{ClassChat}, PurposeOperator); got != "operator" {
		t.Fatalf("FactoryModel(chat, operator) = %q, want operator", got)
	}
	if got := tree.Document.FactoryModels([]string{ClassChat}); !reflect.DeepEqual(got, []string{"operator"}) {
		t.Fatalf("FactoryModels(chat) = %v, want [operator]", got)
	}
	if got := tree.Document.FactoryModel([]string{"coder"}, PurposeOperator); got != "" {
		t.Fatalf("FactoryModel(coder, operator) = %q, want none: only a chat task calls the operator", got)
	}
}

// A project file's keys replace the policy's, one by one: a block of only
// confirm keeps the policy's model and budget, and an empty confirm list
// asks for no OK at all.
func TestChatProjectBlockReplacesThePolicys(t *testing.T) {
	files := fixtureTree(t)
	files["policy.yaml"] = append(files["policy.yaml"], "chat:\n  monthly_eur: 4\n  confirm: [new_card, approve_merge]\n"...)
	files["projects/linkshort.yaml"] = append(files["projects/linkshort.yaml"], "chat:\n  confirm: [deny_merge]\n"...)
	files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], "chat: {monthly_eur: 2.5, confirm: []}\n"...)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]ChatConfig{
		"linkshort":                {Model: "operator", MonthlyEUR: 4, Confirm: []string{"deny_merge"}},
		"demo-app":                 {Model: "operator", MonthlyEUR: 2.5, Confirm: []string{}},
		"ai-software-factory-code": {Model: "operator", MonthlyEUR: 4, Confirm: []string{"new_card", "approve_merge"}},
	} {
		if got := tree.Document.ChatOf(tree.Projects[name]); !reflect.DeepEqual(got, want) {
			t.Errorf("ChatOf(%s) = %#v, want %#v", name, got, want)
		}
	}
}

// The check refuses a tool or a model it does not know, a negative budget,
// a project's budget above the policy's, and a policy class named chat,
// the chat task's own.
func TestChatBlockRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ file, block, want string }{
		"unknown tool": {"projects/linkshort.yaml", "chat:\n  confirm: [aprove_merge]\n",
			`project linkshort: chat.confirm: unknown tool "aprove_merge"`},
		"unknown model":      {"policy.yaml", "chat:\n  model: gpt-5\n", `policy: chat.model "gpt-5": want one of [operator]`},
		"negative budget":    {"policy.yaml", "chat:\n  monthly_eur: -1\n", "policy: chat.monthly_eur must not be negative"},
		"a class named chat": {"policy.yaml", "", `policy: a capability class may not be named "chat"`},
		"project budget above the policy's": {"projects/linkshort.yaml", "chat:\n  monthly_eur: 6\n",
			"project linkshort: chat.monthly_eur 6 is looser than policy.yaml's chat.monthly_eur 5"},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			files[tc.file] = append(files[tc.file], tc.block...)
			if tc.block == "" {
				files[tc.file] = []byte(strings.Replace(string(files[tc.file]), "\n  coder:\n", "\n  chat:\n", 1))
			}
			_, err := ParseTree(files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
