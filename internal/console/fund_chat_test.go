package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fundRig is a rig over the capped card t-e of 2 EUR, which its budget
// refusal holds; the policy's task maximum is 5 EUR.
func fundRig(t *testing.T) *rig {
	r := newRig(t)
	cappedCard(r, t)
	return r
}

// fundOf is the fund Act takes of eur with note on case 1, the rig's live
// case of a card waiting on a human.
func fundOf(eur float64, note string, rung int, via string) factoryd.ActInput {
	return factoryd.ActInput{TaskID: "t-e", CaseID: 1, By: "sebastian", Via: via, Rung: rung,
		Res: factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": eur}, Note: note}}
}

// A wake's fund needs no human's login: the agent's own decision on rung
// 2, by the woken human, and the note goes on the card as the answer with
// the project's chat token.
func TestFundAtAWakeAnswersAsTheChatBot(t *testing.T) {
	r := fundRig(t)
	p := factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}
	s, err := r.c.wakeSession(p, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	tool, _ := r.c.chatTool("fund")
	if _, _, err := tool.run(&turn{c: r.c, r: req.WithContext(context.WithValue(req.Context(), ctxKey{}, s)),
		a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: true, decider: "agent", level: policy.AutonomyAutonomous}, p: p},
		toolInput{TaskID: "t-e", EUR: 1, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	got, posts := actedOnce(t, r)
	want := fundOf(1, "go", 2, factoryd.ViaChat)
	want.Detail = map[string]any{"autonomy": policy.AutonomyAutonomous, "decider": "agent"}
	if !reflect.DeepEqual(got, want) || !posts {
		t.Fatalf("Act took %+v (post %v), want %+v with a post", got, posts, want)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if !slices.Equal(r.fg.calls, []string{"POST /api/v1/repos/sf/code/issues/13/comments Bearer chat-token"}) ||
		r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"] != "go" {
		t.Errorf("Forgejo calls %q, comments %q: want the note posted on #13 with the chat token", r.fg.calls, r.fg.comments)
	}
}

const needRefusal = "the remaining budget cannot fund a call: 0.170 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.900000 EUR. Raise the budget (Fund on the board) and answer, or close the card."

// needRig is the fund rig over card t-e held by needRefusal, which names
// a need of 0.90 EUR against the card's 2.00 - 1.83 = 0.17 EUR left: a raise
// of 0.73 EUR.
func needRig(t *testing.T) *rig {
	r := fundRig(t)
	r.tasks.set(needRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, SpentEUR: 1.83, ModelAlias: "factory/small"})
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return needRefusal, nil }
	return r
}

// The fund proposal shows the amount the member presses Do it on.
func TestFundProposalNamesTheAmount(t *testing.T) {
	r, g := newWritesRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "fund", `{"task_id":"t-cap","eur":0.5,"text":"go"}`)}}
	_, proposed, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"fund it"}})
	want := `"description":"Fund a card, #14 t-cap \"Trim the logs.\", 0.50 EUR, text go"`
	if len(proposed) == 0 || !strings.Contains(proposed[0], want) {
		t.Fatalf("proposal %v, want it to hold %s", proposed, want)
	}
}
