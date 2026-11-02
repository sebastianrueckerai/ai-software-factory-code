package console

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tokenSessionOf opens login's chat token on repository, named laptop.
func tokenSessionOf(t *testing.T, r *rig, login, repository string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(r.c)
	t.Cleanup(srv.Close)
	return opened(t, srv, &door{auth: "Bearer " + r.chatTokenOf(t, login, repository, nil)})
}

// Every consequential action a chat token's session confirms records
// events that name the token, by its id and name.
func TestTokenStampsEveryDecisionEvent(t *testing.T) {
	for _, tc := range []struct {
		tool, args string
		rig        func(t *testing.T) (*rig, *chatGateway)
		login      string
		repository string
	}{
		{"approve_merge", `{"task_id":"t-m"}`, func(t *testing.T) (*rig, *chatGateway) {
			r, _, g := newChatRig(t)
			return r, g
		}, "sebastian", "sf/code"},
		{"fund", `{"task_id":"t-e","eur":1,"text":"go"}`, func(t *testing.T) (*rig, *chatGateway) {
			r, _, g := newChatRig(t)
			cappedCard(r, t)
			r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", Objects: "factory",
				Agents: "coder", Process: "maintenance", Budget: &policy.ProjectBudget{DiscretionaryEUR: 3, RaiseMaxEUR: 1}}}
			r.c.cfg.AnswerWhole = func(context.Context, string, int64, string) error { return nil }
			return r, g
		}, "sebastian", "sf/code"},
		{"invite", `{"login":"dave"}`, func(t *testing.T) (*rig, *chatGateway) {
			r, _, g := newChatRig(t)
			members(r)
			g.project = "demo"
			return r, g
		}, "bob", "demo/demo"},
		{"set_autonomy", `{"autonomy":"assisted"}`, func(t *testing.T) (*rig, *chatGateway) {
			r, g := newWritesRig(t)
			r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", QueueCap: 5}}
			infra := withInfra(t, r)
			infra.files = map[string]string{"factory/projects/ai-software-factory-code.yaml": "# the code project\nrepository: sf/code\nmerge_rule: human\n"}
			r.c.cfg.PolicyReload = func(context.Context) error { return nil }
			r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) { return nil, nil }
			return r, g
		}, "sebastian", "sf/code"},
		{"set_pool", `{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"}`, func(t *testing.T) (*rig, *chatGateway) {
			r, g, _ := poolChatRig(t, 8, 3)
			poolChatReload(r, 10)
			return r, g
		}, "sebastian", "sf/code"},
		{"new_card", `{"title":"Cache metrics","body":"Track the cache hit rate.\n\nAcceptance:\n- The metrics report works\n","track":"direct","track_reason":"a small read","size":"small"}`,
			func(t *testing.T) (*rig, *chatGateway) {
				r, _, g := newChatRig(t)
				r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyManual, MergeRule: policy.MergeHuman}}
				return r, g
			}, "sebastian", "sf/code"},
		{"answer", `{"task_id":"t-p","revision":1,"question":1,"text":"60 seconds"}`, func(t *testing.T) (*rig, *chatGateway) {
			r, g := newReadsRig(t)
			r.tasks.mu.Lock()
			for i := range r.tasks.tasks {
				if r.tasks.tasks[i].ID == "t-p" {
					r.tasks.tasks[i].Status = "waiting-human"
				}
			}
			r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": askedQuestions()}
			r.tasks.mu.Unlock()
			r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: "t-p", Project: "sf/code",
				Status: "pending", Spec: []byte(`{"revision":1,"sha256":"5e1f"}`)})
			return r, g
		}, "sebastian", "sf/code"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			r, g := tc.rig(t)
			session := tokenSessionOf(t, r, tc.login, tc.repository)
			g.replies = [][]string{{call(1, tc.tool, tc.args)}}
			id := raised(t, session, "do it")
			before := r.events.count()
			g.replies = [][]string{}
			if a := confirmed(t, session, id); a.Error != "" || len(a.Applied) != 1 {
				t.Fatalf("chat_confirm = %+v, want %s applied", a, tc.tool)
			}
			r.chatTokens.mu.Lock()
			want := map[string]any{"id": r.chatTokens.rows[len(r.chatTokens.rows)-1].ID, "name": "laptop"}
			r.chatTokens.mu.Unlock()
			rows := r.events.all()[before:]
			if len(rows) == 0 {
				t.Fatalf("%s recorded no event", tc.tool)
			}
			for _, e := range rows {
				equal(t, e.Kind+" detail token", e.Detail["token"], want)
			}
		})
	}
}

// Every row of a chat token's turn, the human's message and the agent's
// reply, names the token; the token's own name is the human's laptop.
func TestTokenStampsChatRows(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("hello back")}}
	session := tokenSession(t, r)
	answerOf(t, session, "chat_send", map[string]any{"message": "hello"})
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 2, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("the token's thread holds %+v, want its message and the reply", rows)
	}
	for _, m := range rows {
		if m.Token != "laptop" {
			t.Errorf("%s row %q has Token %q, want laptop", m.Role, m.Content, m.Token)
		}
	}
}

// A browser's turn writes rows and events no token names.
func TestCookieRowsCarryNoStamp(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyManual, MergeRule: policy.MergeHuman}}
	args := `{"title":"Cache metrics","body":"Track the cache hit rate.\n\nAcceptance:\n- The metrics report works\n","track":"direct","track_reason":"a small read","size":"small"}`
	before := r.events.count()
	proposeAndDo(t, r, g, "new_card", args)
	rows := r.events.all()[before:]
	if len(rows) == 0 {
		t.Fatal("the browser's Do it recorded no event")
	}
	for _, e := range rows {
		if _, ok := e.Detail["token"]; ok {
			t.Errorf("%s event carries %v", e.Kind, e.Detail["token"])
		}
	}
	convs, err := chat.Conversations(t.Context(), "sf/code", "sebastian")
	if err != nil || len(convs) == 0 {
		t.Fatalf("conversations = %v, %v", convs, err)
	}
	for _, c := range convs {
		msgs, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", c.Number, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.Token != "" {
				t.Errorf("browser row %q has Token %q", m.Content, m.Token)
			}
		}
	}
}
