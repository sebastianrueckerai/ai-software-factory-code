package agentdoc

import (
	"strings"
	"testing"
)

// A span nobody can locate, or one that matches twice, is the map rotting:
// Locate reports it instead of guessing.
func TestLocateNamesEveryUnclaimedByte(t *testing.T) {
	spans := []Span{
		{Name: "guide", Symbol: "factoryd.workGuide", Kind: "builder", Text: "How to work: reproduce first.\n"},
		{Name: "tools line", Symbol: "factoryd.toolsLine", Kind: "builder", Text: "Factory tools: the MCP server factory-tools."},
	}
	s := "How to work: reproduce first.\nFactory tools: the MCP server factory-tools."
	claims, err := Locate(s, spans)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("two spans, got %d claims", len(claims))
	}
	for _, c := range claims {
		if c.Unlabelled {
			t.Errorf("claim %q left unlabelled", c.Name)
		}
		if s[c.Start:c.End] != c.Text {
			t.Errorf("claim %q offsets do not hold its text", c.Name)
		}
	}
}

func TestLocateMarksLeftoversUnlabelled(t *testing.T) {
	spans := []Span{{Name: "guide", Symbol: "factoryd.workGuide", Kind: "builder", Text: "How to work: reproduce first."}}
	s := "How to work: reproduce first.\nand then some words nobody claims"
	claims, err := Locate(s, spans)
	if err != nil {
		t.Fatal(err)
	}
	var unlabelled []Claim
	for _, c := range claims {
		if c.Unlabelled {
			unlabelled = append(unlabelled, c)
		}
	}
	if len(unlabelled) != 1 || unlabelled[0].Text != "\nand then some words nobody claims" {
		t.Fatalf("the leftover is one unlabelled claim, got %v", unlabelled)
	}
}

func TestLocateRefusesAnAbsentSpan(t *testing.T) {
	spans := []Span{{Name: "guide", Symbol: "factoryd.workGuide", Kind: "builder", Text: "not in the string"}}
	if _, err := Locate("something else", spans); err == nil {
		t.Fatal("an absent span must be an error")
	}
}

func TestLocateRefusesAnAmbiguousSpan(t *testing.T) {
	text := "twice"
	spans := []Span{{Name: "guide", Symbol: "factoryd.workGuide", Kind: "builder", Text: text}}
	if _, err := Locate(text+" middle "+text, spans); err == nil {
		t.Fatal("a span matching twice must be an error")
	}
}

// Nested provenance: a shared block inside a larger builder stays a claim of
// its own, nested under its parent, so the duplication matrix can hash it.
func TestLocateNestsASharedBlockInsideItsParent(t *testing.T) {
	guide := "the writing guide text"
	spans := []Span{
		{Name: "statement", Symbol: "factoryd.checkinStatement", Kind: "builder", Text: "head\n" + guide + "\ntail"},
		{Name: "writing guide", Symbol: "writingguide.Guide", Kind: "builder", Text: guide},
	}
	claims, err := Locate("head\n"+guide+"\ntail", spans)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Name != "statement" {
		t.Fatalf("one top claim, got %v", claims)
	}
	if len(claims[0].Within) != 1 || claims[0].Within[0].Name != "writing guide" {
		t.Fatalf("the guide nests inside the statement, got %v", claims[0].Within)
	}
}

// Claims come out in document order whatever order the spans were given in.
func TestLocateOrdersByPosition(t *testing.T) {
	spans := []Span{
		{Name: "b", Symbol: "b", Kind: "builder", Text: "bbb"},
		{Name: "a", Symbol: "a", Kind: "builder", Text: "aaa"},
	}
	claims, err := Locate("aaabbb", spans)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || claims[0].Name != "a" || claims[1].Name != "b" {
		t.Fatalf("two claims in document order, got %v", claims)
	}
}

func TestLocateRawFindsNestedJSON(t *testing.T) {
	tool := `{"type":"function","function":{"name":"grep","description":"git grep the workspace at a ref"}}`
	spans := []Span{{Name: "grep", Symbol: "factoryd.checkinTools", Kind: "builder", Text: tool}}
	raw := []byte(`{"model":"big","tools":[` + tool + `,{"type":"function","function":{"name":"run"}}]}`)
	claims, err := LocateRaw(raw, spans)
	if err != nil {
		t.Fatal(err)
	}
	var grep *Claim
	for i, c := range claims {
		if c.Name == "grep" {
			grep = &claims[i]
		}
	}
	if grep == nil {
		t.Fatalf("the tool json is one raw claim, got %v", claims)
	}
	if !strings.Contains(string(raw[grep.Start:grep.End]), `"grep"`) {
		t.Fatal("the claim's offsets do not hold the tool json")
	}
}
