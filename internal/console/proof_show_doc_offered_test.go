package console

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// TestShowDocOfferedBesideReads proves the show_doc_offered_beside_reads
// criterion of the show-doc card (roadmap 280): the member's chat offers
// show_doc exactly once, beside read_doc and write_doc, its parameters
// the path with the optional heading — the path required, as the reads'
// schemas require it — and its description the member's own document
// view. show_doc and write_doc are rare tools: they stand in tool_schema's
// index until the agent fetches show_doc, whose schema then stands in the
// list once.
func TestShowDocOfferedBesideReads(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, ToolSchemaName, `{"name":"show_doc"}`)}, {say("ok")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	if len(g.calls()) != 2 {
		t.Fatalf("the turn made %d model calls, want 2", len(g.calls()))
	}
	before, after := g.calls()[0]["tools"].([]any), g.calls()[1]["tools"].([]any)
	var names []string
	offered := map[string]map[string]any{}
	for _, d := range after {
		fn := d.(map[string]any)["function"].(map[string]any)
		name := fn["name"].(string)
		names = append(names, name)
		offered[name] = fn
	}
	if got := strings.Count(strings.Join(names, ","), "show_doc"); got != 1 {
		t.Errorf("the offered tools hold show_doc %d times: %s", got, strings.Join(names, " "))
	}
	for _, beside := range []string{"read_doc", "show_doc"} {
		if !slices.Contains(names, beside) {
			t.Errorf("the offered tools hold no %s: %s", beside, strings.Join(names, " "))
		}
	}
	if !inIndex(before, "show_doc") || !inIndex(before, "write_doc") || inIndex(after, "show_doc") || !inIndex(after, "write_doc") {
		t.Errorf("show_doc and write_doc stand in the index before the fetch and write_doc alone after it")
	}
	fn, ok := offered["show_doc"]
	if !ok {
		t.Fatal("show_doc is not offered")
	}
	if about, _ := fn["description"].(string); !strings.Contains(about, "document view") {
		t.Errorf("show_doc's description does not name the member's document view: %q", about)
	}
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	for _, p := range []string{"path", "heading"} {
		if _, ok := props[p]; !ok {
			t.Errorf("show_doc offers no %s parameter: %v", p, params)
		}
	}
	// the path is required, as the reads' schemas require it
	req, _ := params["required"].([]any)
	if len(req) != 1 || req[0] != "path" {
		t.Errorf("show_doc's required parameters are %v, want [path]", req)
	}
}
