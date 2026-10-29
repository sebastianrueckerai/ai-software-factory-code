package policy

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A tree without a chat block gives every project DefaultChat, and the
// operator purpose calls its model on a chat task, which calls the pulse's
// alias too.
func TestChatDefaults(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	want := ChatConfig{Model: Big, VisionModel: "vision", MonthlyEUR: 5,
		Confirm: []string{"new_card", "request_image", "answer", "tell_card", "steer", "retry", "request_changes",
			"approve_merge", "deny_merge", "invite", "github_export", "github_sync", "close", "edit", "grant_session", "fund", "set_dependency",
			"alert_verdict", "promote_resource", "release_resource", "write_doc", "set_autonomy", "routine_write", "routine_delete", "routine_run", "routine_skip"}}
	if got := tree.Document.ChatOf(tree.Projects["linkshort"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChatOf(linkshort) = %+v, want %+v", got, want)
	}
	if got := tree.Document.FactoryModel([]string{ClassChat}, PurposeOperator); got != Big {
		t.Fatalf("FactoryModel(chat, operator) = %q, want operator", got)
	}
	if got := tree.Document.FactoryModels([]string{ClassChat}); !reflect.DeepEqual(got, []string{Big, Small, "vision"}) {
		t.Fatalf("FactoryModels(chat) = %v, want [operator pulse vision]", got)
	}
	if got := tree.Document.FactoryModel([]string{ClassChat}, PurposeVision); got != "vision" {
		t.Fatalf("FactoryModel(chat, vision) = %q, want vision", got)
	}
	if got := tree.Document.FactoryModel([]string{"coder"}, PurposeVision); got != "" {
		t.Fatalf("FactoryModel(coder, vision) = %q, want none: the fixture's coder names no vision_model", got)
	}
	if !slices.Contains(Purposes, PurposeVision) {
		t.Fatalf("Purposes = %v, want vision among them", Purposes)
	}
	if got := tree.Document.FactoryModel([]string{"coder"}, PurposeOperator); got != "" {
		t.Fatalf("FactoryModel(coder, operator) = %q, want none: only a chat task calls the operator", got)
	}
}

// A class that lists analyze_image names its vision model, which its
// sessions' looks call on the card's account beside the class's other
// factoryd models.
func TestClassVisionModel(t *testing.T) {
	files := fixtureTree(t)
	files["policy.yaml"] = []byte(strings.Replace(string(files["policy.yaml"]), "    supervisor_model: supervisor\n    tools:\n",
		"    supervisor_model: supervisor\n    vision_model: vision\n    tools:\n      analyze_image: {}\n", 1))
	files["policy.yaml"] = []byte(strings.Replace(string(files["policy.yaml"]), "pulse: 2048}", "pulse: 2048, vision: 4096}", 1))
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	d := tree.Document
	if got := d.FactoryModel([]string{"coder"}, PurposeVision); got != "vision" {
		t.Fatalf("FactoryModel(coder, vision) = %q, want vision", got)
	}
	if got := d.FactoryModels([]string{"coder"}); !reflect.DeepEqual(got, []string{"big", "supervisor", "vision"}) {
		t.Fatalf("FactoryModels(coder) = %v, want [big supervisor vision]", got)
	}
	if !d.AllowsTool([]string{"coder"}, ToolAnalyzeImage) {
		t.Fatal("coder does not allow analyze_image")
	}
	if got := d.FactoryModel([]string{ClassChat}, PurposeVision); got != "vision" {
		t.Fatalf("FactoryModel(chat, vision) = %q, want the chat block's vision", got)
	}
}

// A project file's keys replace the policy's, one by one: a block of only
// confirm keeps the policy's model and budget, and an empty confirm list
// asks an OK only for the always-confirm set, which every list holds.
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
		"linkshort": {Model: Big, VisionModel: "vision", MonthlyEUR: 4, Confirm: []string{"deny_merge", "approve_merge", "invite", "alert_verdict", "promote_resource",
			"release_resource", "github_export", "github_sync", "write_doc", "set_autonomy", "routine_write", "routine_delete", "fund"}},
		"demo-app":                 {Model: Big, VisionModel: "vision", MonthlyEUR: 2.5, Confirm: append(slices.Clone(AlwaysConfirm), "fund")},
		"ai-software-factory-code": {Model: Big, VisionModel: "vision", MonthlyEUR: 4, Confirm: append(append([]string{"new_card"}, AlwaysConfirm...), "fund")},
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
		"unknown model":        {"policy.yaml", "chat:\n  model: gpt-5\n", `policy: chat.model "gpt-5": want one of [big small premium]`},
		"unknown vision model": {"policy.yaml", "chat:\n  vision_model: gpt-5\n", `policy: chat.vision_model "gpt-5": want one of [vision]`},
		"negative budget":      {"policy.yaml", "chat:\n  monthly_eur: -1\n", "policy: chat.monthly_eur must not be negative"},
		"a class named chat":   {"policy.yaml", "", `policy: a capability class may not be named "chat"`},
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

// The policy store keeps the tree as JSON: an empty confirm list comes
// back empty, so it confirms only the always-confirm set, and an absent
// one still takes the policy's.
func TestChatConfirmSurvivesTheStoresJSON(t *testing.T) {
	tree := Tree{Document: Document{Chat: &ChatConfig{Confirm: []string{"new_card"}}},
		Projects: map[string]ProjectConfig{"empty": {Chat: &ChatConfig{Confirm: []string{}}}, "absent": {Chat: &ChatConfig{MonthlyEUR: 2}}}}
	b, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var back Tree
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Document.ChatOf(back.Projects["empty"]).Confirm; !reflect.DeepEqual(got, append(slices.Clone(AlwaysConfirm), "fund")) {
		t.Fatalf("an empty confirm after the round trip = %#v, want the always-confirm set and fund", got)
	}
	if got := back.Document.ChatOf(back.Projects["absent"]).Confirm; !reflect.DeepEqual(got, append(append([]string{"new_card"}, AlwaysConfirm...), "fund")) {
		t.Fatalf("an absent confirm after the round trip = %#v, want the policy's [new_card] and the always-confirm set", got)
	}
}
