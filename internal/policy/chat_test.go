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
	want := ChatConfig{Model: Big, MCPModel: Big, WakeModel: Big, VisionModel: "vision", MonthlyEUR: 5,
		Confirm: []string{"new_card", "request_image", "answer", "tell_card", "steer", "retry", "request_changes",
			"approve_merge", "deny_merge", "invite", "github_export", "github_sync", "close", "edit", "grant_session", "fund", "set_dependency", "replan",
			"alert_verdict", "promote_resource", "release_resource", "write_doc", "set_autonomy", "routine_write", "routine_delete", "routine_run", "routine_skip", "set_pool"}}
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
	if got := tree.Document.FactoryModel([]string{ClassChat}, PurposeResearch); got != Big {
		t.Fatalf("FactoryModel(chat, research) = %q, want the mid tier, hard-coded", got)
	}
	if got := tree.Document.FactoryModel([]string{"coder"}, PurposeResearch); got != "" {
		t.Fatalf("FactoryModel(coder, research) = %q, want none: only a chat task researches", got)
	}
	if !slices.Contains(Purposes, PurposeResearch) {
		t.Fatalf("Purposes = %v, want research among them", Purposes)
	}
	if !slices.Contains(ChatTools, "research") {
		t.Fatalf("ChatTools = %v, want research among them", ChatTools)
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
	if got := d.FactoryModels([]string{"coder"}); !reflect.DeepEqual(got, []string{"big", "small", "supervisor", "vision"}) {
		t.Fatalf("FactoryModels(coder) = %v, want [big small supervisor vision]", got)
	}
	if !d.AllowsTool([]string{"coder"}, ToolAnalyzeImage) {
		t.Fatal("coder does not allow analyze_image")
	}
	if got := d.FactoryModel([]string{ClassChat}, PurposeVision); got != "vision" {
		t.Fatalf("FactoryModel(chat, vision) = %q, want the chat block's vision", got)
	}
}

// A class lists digest free and names no model for it: a session's
// digest is Small on the card's own task, as the chat's is.
func TestClassListsDigest(t *testing.T) {
	files := fixtureTree(t)
	files["policy.yaml"] = []byte(strings.Replace(string(files["policy.yaml"]), "    supervisor_model: supervisor\n    tools:\n",
		"    supervisor_model: supervisor\n    tools:\n      digest: {}\n", 1))
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Document.AllowsSessionTool([]string{"coder"}, "", ToolDigest) {
		t.Fatal("coder does not allow digest")
	}
	if got := tree.Document.FactoryModel([]string{"coder"}, PurposeDigest); got != Small {
		t.Fatalf("FactoryModel(coder, digest) = %q, want small", got)
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
		"linkshort": {Model: Big, MCPModel: Big, WakeModel: Big, VisionModel: "vision", MonthlyEUR: 4, Confirm: []string{"deny_merge", "approve_merge", "invite", "alert_verdict", "promote_resource",
			"release_resource", "github_export", "github_sync", "write_doc", "set_autonomy", "routine_write", "routine_delete", "set_pool", "fund"}},
		"demo-app":                 {Model: Big, MCPModel: Big, WakeModel: Big, VisionModel: "vision", MonthlyEUR: 2.5, Confirm: append(slices.Clone(AlwaysConfirm), "fund")},
		"ai-software-factory-code": {Model: Big, MCPModel: Big, WakeModel: Big, VisionModel: "vision", MonthlyEUR: 4, Confirm: append(append([]string{"new_card"}, AlwaysConfirm...), "fund")},
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
		"unknown chat tool": {"policy.yaml", "chat:\n  tools: {replicate_predict: {max_requests: 5, max_eur: 0.05}}\n",
			`policy: chat.tools: unknown tool "replicate_predict" (known: [web_search web_scrape])`},
		"chat tool without caps": {"policy.yaml", "chat:\n  tools: {web_search: {}}\n",
			`policy: chat.tools "web_search" needs a positive max_requests and max_eur`},
		"project chat tool the policy lacks": {"projects/linkshort.yaml", "chat:\n  tools: {web_scrape: {max_requests: 5, max_eur: 0.05}}\n",
			"project linkshort: chat.tools lists web_scrape, which policy.yaml's chat.tools does not"},
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

// The chat block lists its web tools with caps like a class's, and a
// project file's block replaces the policy's whole map, only tighter:
// the project's numbers may not exceed the policy's.
func TestChatWebTools(t *testing.T) {
	files := fixtureTree(t)
	files["policy.yaml"] = append(files["policy.yaml"],
		"chat:\n  tools:\n    web_search: {max_requests: 40, max_eur: 0.40}\n    web_scrape: {max_requests: 60, max_eur: 0.30}\n"...)
	files["projects/linkshort.yaml"] = append(files["projects/linkshort.yaml"],
		"chat:\n  tools: {web_search: {max_requests: 10, max_eur: 0.10}}\n"...)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]map[string]ToolCap{
		"linkshort": {ToolWebSearch: {10, 0.10}, ToolWebScrape: {60, 0.30}},
		"demo-app":  {ToolWebSearch: {40, 0.40}, ToolWebScrape: {60, 0.30}},
	} {
		if got := tree.Document.ChatOf(tree.Projects[name]).Tools; !reflect.DeepEqual(got, want) {
			t.Errorf("ChatOf(%s).Tools = %v, want %v", name, got, want)
		}
	}
	if DefaultChat.Tools != nil {
		t.Fatalf("DefaultChat.Tools = %v, want none: web is opt-in", DefaultChat.Tools)
	}
	files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"],
		"chat:\n  tools: {web_search: {max_requests: 50, max_eur: 0.10}}\n"...)
	_, err = ParseTree(files)
	if err == nil || !strings.Contains(err.Error(),
		"project demo-app: chat.tools.web_search.max_requests 50 is looser than policy.yaml's chat.tools.web_search.max_requests 40") {
		t.Fatalf("ParseTree error = %v, want the looser project cap named", err)
	}
}

// A policy that lists a chat web tool must carry its Firecrawl prices, as
// a class's listing must.
func TestChatWebToolPrices(t *testing.T) {
	files := fixtureTree(t)
	files["policy.yaml"] = []byte(strings.Replace(string(files["policy.yaml"]),
		"prices:\n  firecrawl: {eur_per_credit: 0.005, scrape_credits: 1, search_credits: 2}\n", "prices:\n", 1))
	files["policy.yaml"] = append(files["policy.yaml"], "chat:\n  tools: {web_search: {max_requests: 5, max_eur: 0.05}}\n"...)
	_, err := ParseTree(files)
	if err == nil || !strings.Contains(err.Error(), "policy: chat lists web_search: prices.firecrawl needs a positive eur_per_credit") {
		t.Fatalf("ParseTree error = %v, want the firecrawl prices to be named", err)
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
