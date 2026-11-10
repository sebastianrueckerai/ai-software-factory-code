package policy

import (
	"slices"
	"strings"
	"testing"
)

func resetSchemaCache() {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	resolved, resolvedErr = nil, nil
}

func withAliases(t *testing.T, a map[string]string) {
	t.Helper()
	old := ToolAliases
	ToolAliases = a
	resetSchemaCache()
	t.Cleanup(func() { ToolAliases = old; resetSchemaCache() })
}

func TestCanonicalTool(t *testing.T) {
	withAliases(t, map[string]string{"ask": "answer"})
	if got := CanonicalTool("ask"); got != "answer" {
		t.Fatalf("CanonicalTool(ask) = %q, want answer", got)
	}
	if got := CanonicalTool("answer"); got != "answer" {
		t.Fatalf("CanonicalTool(answer) = %q, want answer", got)
	}
}

func TestLoaderCanonicalisesChatConfirm(t *testing.T) {
	withAliases(t, map[string]string{"ask": "answer"})
	for name, tc := range map[string]struct{ confirm, err string }{
		"old form": {"[ask]", ""},
		"new form": {"[answer]", ""},
		"unknown":  {"[nosuchtool]", `chat.confirm: unknown tool "nosuchtool"`},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			files["projects/linkshort.yaml"] = append(files["projects/linkshort.yaml"], "chat:\n  confirm: "+tc.confirm+"\n"...)
			tree, err := ParseTree(files)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := tree.Projects["linkshort"].Chat.Confirm; !slices.Equal(got, []string{"answer"}) {
				t.Fatalf("project chat.confirm = %v, want [answer]", got)
			}
		})
	}
}

func TestLoaderCanonicalisesPolicyChatAndClassTools(t *testing.T) {
	withAliases(t, map[string]string{"ask": "answer", "old_escalate": "escalate", "old_scrape": "web_scrape"})
	files := fixtureTree(t)
	p := string(files["policy.yaml"])
	p = strings.Replace(p, "      escalate: {}\n", "      old_escalate: {}\n", 1)
	p = strings.Replace(p, "      web_scrape: {max_requests: 30, max_eur: 0.20}\n", "      old_scrape: {max_requests: 30, max_eur: 0.20}\n", 1)
	p += "chat:\n  confirm: [ask]\n  tools:\n    old_scrape: {max_requests: 5, max_eur: 1}\n"
	files["policy.yaml"] = []byte(p)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range tree.Document.Classes {
		if _, ok := c.Tools["old_escalate"]; ok {
			t.Fatalf("class %s still lists old_escalate", name)
		}
	}
	if _, ok := tree.Document.Classes["coder"].Tools["escalate"]; !ok {
		t.Fatalf("coder tools = %v, want escalate", tree.Document.Classes["coder"].Tools)
	}
	if _, ok := tree.Document.Classes["coder"].Tools["web_scrape"]; !ok {
		t.Fatalf("coder tools = %v, want web_scrape", tree.Document.Classes["coder"].Tools)
	}
	if got := tree.Document.Chat.Confirm; !slices.Equal(got, []string{"answer"}) {
		t.Fatalf("chat.confirm = %v, want [answer]", got)
	}
	if _, ok := tree.Document.Chat.Tools["web_scrape"]; !ok || len(tree.Document.Chat.Tools) != 1 {
		t.Fatalf("chat.tools = %v, want web_scrape only", tree.Document.Chat.Tools)
	}
}

func TestSchemaEnumsHoldAliasKeys(t *testing.T) {
	confirmEnum := func() []any {
		return resolvedSchema(t, "policy.schema.json").Schema().Properties["chat"].Properties["confirm"].Items.Enum
	}
	if got := len(confirmEnum()); got != len(ChatTools) {
		t.Fatalf("empty table: confirm enum has %d values, want %d", got, len(ChatTools))
	}
	withAliases(t, map[string]string{"ask": "answer"})
	got := confirmEnum()
	if len(got) != len(ChatTools)+1 || got[len(got)-1] != "ask" {
		t.Fatalf("confirm enum = %v, want ChatTools plus ask", got)
	}
}

func TestCanonicalisationEdges(t *testing.T) {
	withAliases(t, map[string]string{"ask": "answer", "old_scrape": "web_scrape"})
	c := ChatConfig{Confirm: []string{"ask", "close", "answer"}}
	c.canonicalTools()
	if !slices.Equal(c.Confirm, []string{"answer", "close"}) {
		t.Fatalf("confirm = %v, want [answer close]", c.Confirm)
	}
	own, old := ToolCap{MaxRequests: 7, MaxEUR: 2}, ToolCap{MaxRequests: 1, MaxEUR: 1}
	for range 50 {
		got := canonicalKeys(map[string]ToolCap{"old_scrape": old, "web_scrape": own})
		if len(got) != 1 || got["web_scrape"] != own {
			t.Fatalf("keys = %v, want the new name's own entry", got)
		}
	}
}

func TestDecodeProjectCanonicalises(t *testing.T) {
	withAliases(t, map[string]string{"ask": "answer", "old_scrape": "web_scrape"})
	p, err := DecodeProject([]byte("repository: a/b\nchat:\n  confirm: [ask]\n  tools:\n    old_scrape: {max_requests: 5, max_eur: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Chat.Confirm, []string{"answer"}) {
		t.Fatalf("confirm = %v, want [answer]", p.Chat.Confirm)
	}
	if _, ok := p.Chat.Tools["web_scrape"]; !ok || len(p.Chat.Tools) != 1 {
		t.Fatalf("tools = %v, want web_scrape only", p.Chat.Tools)
	}
}

func TestProjectFileChatToolsAliased(t *testing.T) {
	withAliases(t, map[string]string{"old_scrape": "web_scrape"})
	files := fixtureTree(t)
	files["policy.yaml"] = append(files["policy.yaml"], "chat:\n  tools:\n    web_scrape: {max_requests: 9, max_eur: 2}\n"...)
	files["projects/linkshort.yaml"] = append(files["projects/linkshort.yaml"], "chat:\n  tools:\n    old_scrape: {max_requests: 5, max_eur: 1}\n"...)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tree.Projects["linkshort"].Chat.Tools["web_scrape"]; !ok {
		t.Fatalf("tools = %v, want web_scrape", tree.Projects["linkshort"].Chat.Tools)
	}
}
