package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// coreTools are the project agent's 46 core tools, rareTools the 30 behind
// tool_schema and attachedTools the 3 an attachment brings, each in
// policy.ChatTools order.
var (
	coreTools = []string{"system", "find_cards", "card", "history", "card_step", "board_digest", "costs", "diff", "plan", "pr", "file", "tree", "grep",
		"web_search", "web_scrape", "research", "session", "session_steps", "session_step", "session_search", "new_card", "answer", "comment", "tell_card",
		"retry", "request_changes", "approve_merge", "deny_merge", "answer_criteria", "close", "edit", "give_room", "hold_card", "release_hold", "read_doc",
		"notes_read", "notes_write", "escalate", "attach_analysis", "active_threads", "chat_search", "inbox", "watch_card", "unwatch_card", "set_timer", "ask"}
	rareTools = []string{"board", "ci", "alerts", "deployment_health", "cluster_facts", "resources", "settings", "policy", "project_images",
		"promote_resource", "release_resource", "request_image", "rerun_checks", "set_accent", "invite", "github_export", "github_sync", "set_dependency",
		"replan", "set_pool", "write_doc", "show_doc", "set_autonomy", "routines", "routine_write", "routine_delete", "routine_run", "routine_skip",
		"board_view", "board_view_reset"}
	attachedTools = []string{"analyze_image", "read_attachment", "summarize_attachment"}
)

// The three tiers split the chat's tools: 46 + 30 + 3 = 79, the 81 tools
// less steer and card_history.
func TestTiersPartitionTheChatTools(t *testing.T) {
	tiers := chatTiers()
	inOrder := func(names []string) []string {
		return slices.DeleteFunc(slices.Clone(policy.ChatTools), func(n string) bool { return !slices.Contains(names, n) })
	}
	equal(t, "core", inOrder(tiers.Chat), coreTools)
	equal(t, "rare", inOrder(tiers.Rare), rareTools)
	equal(t, "attached", inOrder(tiers.Attached), attachedTools)
	if n := len(tiers.Chat) + len(tiers.Rare) + len(tiers.Attached); n != 79 || len(policy.ChatTools) != 79 {
		t.Errorf("the tiers hold %d tools and policy.ChatTools %d, want 79 each", n, len(policy.ChatTools))
	}
	all := slices.Concat(tiers.Chat, tiers.Rare, tiers.Attached)
	slices.Sort(all)
	if want := slices.Sorted(slices.Values(policy.ChatTools)); !slices.Equal(all, want) {
		t.Errorf("the tiers' union = %q, want policy.ChatTools %q", all, want)
	}
}

// agentRig is the chat's rig with alice, who holds every role, logged in
// on the code project.
type agentRig struct {
	r  *rig
	g  *chatGateway
	ck *http.Cookie
	s  *session
}

func newAgentRig(t *testing.T) agentRig {
	t.Helper()
	r, _, g := newChatRig(t)
	members(r)
	ck, s := memberLogin(t, r, "alice")
	return agentRig{r, g, ck, s}
}

// turn sends alice's message with files, the model answering replies, and
// answers the requests of this turn and the stream.
func (a agentRig) turn(t *testing.T, replies [][]string, files ...testFile) ([]map[string]any, []string) {
	t.Helper()
	before := len(a.g.calls())
	a.g.replies = replies
	var events []string
	if len(files) > 0 {
		_, events, _ = chatPostDocs(t, a.r, a.ck, a.s, chatPath, url.Values{"message": {"what is open?"}}, files...)
	} else {
		_, events, _ = chatPost(t, a.r, a.ck, a.s, chatPath, url.Values{"message": {"what is open?"}})
	}
	return a.g.calls()[before:], events
}

// requestTools are a request's tools by name, and their names in order.
func requestTools(t *testing.T, req map[string]any) ([]string, map[string]map[string]any) {
	t.Helper()
	var names []string
	byName := map[string]map[string]any{}
	for _, d := range req["tools"].([]any) {
		f := d.(map[string]any)["function"].(map[string]any)
		names = append(names, f["name"].(string))
		byName[f["name"].(string)] = f
	}
	return names, byName
}

// indexLines are tool_schema's description lines after its head.
func indexLines(t *testing.T, tools map[string]map[string]any) []string {
	t.Helper()
	d, ok := tools[ToolSchemaName]
	if !ok {
		t.Fatal("the request offers no tool_schema")
	}
	lines := strings.Split(d["description"].(string), "\n")
	equal(t, "the index's head", lines[0], "Fetch the full schema of one tool below, then call it.")
	return lines[1:]
}

// toolEvent is a stream's tool line: the tool, its arguments and its result.
func toolEvent(t *testing.T, line string) (string, any, string) {
	t.Helper()
	data, ok := strings.CutPrefix(line, "tool ")
	if !ok {
		t.Fatalf("%q is no tool line", line)
	}
	var e struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
		Result    string `json:"result"`
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		t.Fatal(err)
	}
	return e.Name, e.Arguments, e.Result
}

// A fresh thread with no attachment offers the 46 core tools and
// tool_schema, 47, and no rare or attached tool.
func TestOfferedHoldsTheCoreAndToolSchema(t *testing.T) {
	calls, _ := newAgentRig(t).turn(t, [][]string{{say("ok")}})
	names, _ := requestTools(t, calls[0])
	equal(t, "the tools", names, append(slices.Clone(coreTools), ToolSchemaName))
}

// tool_schema's description indexes the 30 rare tools, one name: about
// line each, in policy.ChatTools order.
func TestToolSchemaDescriptionIndexesEveryRareTool(t *testing.T) {
	a := newAgentRig(t)
	calls, _ := a.turn(t, [][]string{{say("ok")}})
	_, byName := requestTools(t, calls[0])
	var want []string
	for _, name := range rareTools {
		tool, _ := a.r.c.chatTool(name)
		want = append(want, name+": "+tool.about)
	}
	equal(t, "the index", indexLines(t, byName), want)
}

// A fetched schema is the tool's whole function JSON. It joins the tool
// list on the turn's next call and on a later turn read from the stored
// rows, and leaves the index.
func TestToolSchemaFetchesAndRemembers(t *testing.T) {
	a := newAgentRig(t)
	calls, events := a.turn(t, [][]string{{call(1, ToolSchemaName, `{"name":"board"}`)}, {say("ok")}})
	board, _ := a.r.c.chatTool("board")
	def, _ := json.Marshal(board.def(false))
	name, args, result := toolEvent(t, events[0])
	equal(t, "the fetch", []any{name, args, result}, []any{ToolSchemaName, map[string]any{"name": "board"}, string(def)})
	later, _ := a.turn(t, [][]string{{say("ok")}})
	for i, req := range append(calls[1:], later...) {
		names, byName := requestTools(t, req)
		if !slices.Contains(names, "board") {
			t.Fatalf("request %d offers no board after its fetch: %q", i, names)
		}
		equal(t, "board's parameters", byName["board"]["parameters"], map[string]any{"type": "object", "properties": map[string]any{}})
		index := indexLines(t, byName)
		if len(index) != 29 || slices.ContainsFunc(index, func(l string) bool { return strings.HasPrefix(l, "board: ") }) {
			t.Errorf("request %d's index holds %d lines, want the 29 others", i, len(index))
		}
	}
	names, _ := requestTools(t, calls[0])
	if slices.Contains(names, "board") {
		t.Error("the request before the fetch offers board")
	}
}

// A rare tool called before its schema is fetched is refused with the call
// that fetches it, before its arguments are read.
func TestRareCallBeforeFetchIsRefused(t *testing.T) {
	_, events := newAgentRig(t).turn(t, [][]string{{call(1, "board", `not json`)}, {say("ok")}})
	name, _, result := toolEvent(t, events[0])
	equal(t, "the refusal", []string{name, result}, []string{"board",
		`Refused: board is a rare tool and its schema is not in this thread. Call tool_schema with name "board", then call board.`})
}

// The attached tools stand in the list while a row of the thread carries an
// attachment, and nowhere otherwise, not in the index either.
func TestAttachedToolsFollowTheAttachment(t *testing.T) {
	a := newAgentRig(t)
	calls, _ := a.turn(t, [][]string{{say("ok")}})
	names, byName := requestTools(t, calls[0])
	for _, name := range attachedTools {
		if slices.Contains(names, name) || slices.ContainsFunc(indexLines(t, byName), func(l string) bool { return strings.HasPrefix(l, name+": ") }) {
			t.Errorf("a thread with no attachment offers or indexes %s", name)
		}
	}
	calls, _ = a.turn(t, [][]string{{say("ok")}}, testFile{name: "spec.md", data: []byte("one line\n")})
	names, _ = requestTools(t, calls[0])
	equal(t, "the tools with an attachment", names, slices.Concat(coreTools, attachedTools, []string{ToolSchemaName}))
}

// tool_schema of a core tool or of a name no rare tool has answers what
// the agent can do instead.
func TestToolSchemaOfACoreOrUnknownName(t *testing.T) {
	calls, _ := newAgentRig(t).turn(t, [][]string{{call(1, ToolSchemaName, `{"name":"card"}`, ToolSchemaName, `{"name":"teleport"}`)}, {say("ok")}})
	var results []any
	for _, m := range messages(t, calls[1]) {
		if m["role"] == "tool" {
			results = append(results, m["content"])
		}
	}
	equal(t, "the two answers the model reads", results, []any{"card is already in your tool list.",
		"There is no rare tool teleport. The rare tools are: " + strings.Join(rareTools, ", ") + "."})
}

// toolWords are the words of a request's tools, as the agent map counts
// them.
func toolWords(t *testing.T, defs []factoryd.OperatorTool) int {
	t.Helper()
	n := 0
	for _, d := range defs {
		var params map[string]any
		b, _ := json.Marshal(d.Parameters)
		if err := json.Unmarshal(b, &params); err != nil {
			t.Fatal(err)
		}
		n += prompts.ToolWords(prompts.Tool{Name: d.Name, Description: d.Description, Parameters: params})
	}
	return n
}

// A fresh thread's tools fill the project agent's tool budget to between
// 90 and 100 percent, and the researcher's tools the research step's.
func TestToolBudget(t *testing.T) {
	a := newAgentRig(t)
	calls, _ := a.turn(t, [][]string{{say("ok")}})
	var defs []factoryd.OperatorTool
	for _, d := range calls[0]["tools"].([]any) {
		f := d.(map[string]any)["function"].(map[string]any)
		defs = append(defs, factoryd.OperatorTool{Name: f["name"].(string), Description: f["description"].(string), Parameters: f["parameters"]})
	}
	var research []factoryd.OperatorTool
	for _, name := range researchTools() {
		tool, _ := a.r.c.chatTool(name)
		research = append(research, tool.def(false))
	}
	for step, offer := range map[string][]factoryd.OperatorTool{"project-agent": defs, "research": research} {
		p, _ := prompts.ProfileOf(step)
		if n := toolWords(t, offer); n > p.BudgetToolWords || n*10 < p.BudgetToolWords*9 {
			t.Errorf("step %s: its tools hold %d words, want between 90 and 100 percent of budget_tool_words %d", step, n, p.BudgetToolWords)
		}
	}
}
