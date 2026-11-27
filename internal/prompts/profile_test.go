package prompts

import (
	"strings"
	"testing"
	"testing/fstest"
)

// testWriting is a guide G and the forms line and chat, L and H.
var testWriting = writing{Guide: "G", Form: func(name string) (string, bool) {
	text, ok := map[string]string{"line": "L", "chat": "H"}[name]
	return text, ok
}}

const validProfile = `step: x
purpose: review
reader: human
guide: false
forms: [line]
chunks: [c]
paragraph: p
facts: x
answer: ""
tools:
  builtin: []
  gateway: []
  chat: []
agents_md: false
budget_words: 10
`

func profileFS(profile string) fstest.MapFS {
	return fstest.MapFS{
		"profiles/x.yaml": {Data: []byte(profile)},
		"chunks/c.md":     {Data: []byte("C\n")},
		"steps/p.md":      {Data: []byte("P\n")},
	}
}

func TestProfilesDecodeStrictly(t *testing.T) {
	_, err := load(profileFS(validProfile+"tool_policy: x\n"), testWriting)
	if err == nil || !strings.Contains(err.Error(), "field tool_policy not found") {
		t.Fatalf("load = %v, want the unknown key refused", err)
	}
}

func TestProfileRules(t *testing.T) {
	if _, err := load(profileFS(validProfile), testWriting); err != nil {
		t.Fatalf("the valid profile: %v", err)
	}
	for name, row := range map[string]struct{ from, to, want string }{
		"no purpose":                {"purpose: review", "purpose: \"\"", `step x: purpose "" is neither a policy purpose nor session`},
		"unknown purpose":           {"purpose: review", "purpose: sell", `step x: purpose "sell" is neither a policy purpose nor session`},
		"no reader":                 {"reader: human", "reader: \"\"", `step x: reader "", want human, model or code`},
		"no facts":                  {"facts: x", "facts: \"\"", "step x: facts names no renderer"},
		"no budget":                 {"budget_words: 10", "budget_words: 0", "step x: budget_words is 0, want the most words its fixture prompt holds"},
		"human with nothing":        {"forms: [line]", "forms: []", "step x: reader human carries neither the guide nor a form"},
		"model with forms":          {"reader: human", "reader: model", "step x: reader model carries forms"},
		"code with the guide":       {"reader: human\nguide: false\nforms: [line]", "reader: code\nguide: true\nforms: []", "step x: reader code carries the guide"},
		"unknown form":              {"forms: [line]", "forms: [poem]", `step x: form "poem" is not a form of the writing guide`},
		"unknown chunk":             {"chunks: [c]", "chunks: [d]", `step x: chunk "d" has no file chunks/d.md`},
		"unknown paragraph":         {"paragraph: p", "paragraph: q", `step x: paragraph "q" has no file steps/q.md`},
		"form twice":                {"forms: [line]", "forms: [line, chat, line]", `step x: forms name "line" twice`},
		"chunk twice":               {"chunks: [c]", "chunks: [c, c]", `step x: chunks name "c" twice`},
		"builtin twice":             {"builtin: []", "builtin: [read, read]", `step x: tools.builtin name "read" twice`},
		"gateway twice":             {"builtin: []\n  gateway: []", "builtin: [read]\n  gateway: [digest, digest]", `step x: tools.gateway name "digest" twice`},
		"chat twice":                {"chat: []", "chat: [board, board]", `step x: tools.chat name "board" twice`},
		"unknown builtin":           {"builtin: []", "builtin: [patch]", `step x: builtin tool "patch" is not one of [bash edit glob grep read todowrite]`},
		"an always denied builtin":  {"builtin: []", "builtin: [task]", `step x: builtin tool "task" is not one of [bash edit glob grep read todowrite]`},
		"unknown gateway":           {"builtin: []\n  gateway: []", "builtin: [read]\n  gateway: [teleport]", `step x: gateway tool "teleport" is not a policy tool`},
		"gateway with no harness":   {"gateway: []", "gateway: [digest]", "step x: tools.gateway needs tools.builtin: only a harness session calls the gateway's tools"},
		"agents_md with no harness": {"agents_md: false", "agents_md: true", "step x: agents_md needs tools.builtin: only the harness loads AGENTS.md"},
		"chat in a harness":         {"builtin: []\n  gateway: []\n  chat: []", "builtin: [read]\n  gateway: []\n  chat: [board]", "step x: tools.chat with tools.builtin: a harness session has no chat tools"},
		"an answer type":            {`answer: ""`, "answer: verdict", `step x: answer "verdict" is not a registered answer type`},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(validProfile, row.from) {
				t.Fatalf("the valid profile lacks %q", row.from)
			}
			_, err := load(profileFS(strings.Replace(validProfile, row.from, row.to, 1)), testWriting)
			if err == nil || err.Error() != row.want {
				t.Errorf("load = %v, want %s", err, row.want)
			}
		})
	}
}

func TestStepNameIsTheFileName(t *testing.T) {
	fsys := profileFS(strings.Replace(validProfile, "step: x", "step: b", 1))
	fsys["profiles/a.yaml"] = fsys["profiles/x.yaml"]
	delete(fsys, "profiles/x.yaml")
	_, err := load(fsys, testWriting)
	if want := "profiles/a.yaml: step b, want a"; err == nil || err.Error() != want {
		t.Errorf("load = %v, want %s", err, want)
	}
}
