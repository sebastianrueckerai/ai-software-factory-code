package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
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

// The answer's fund value draws the shortfall its budget refusal names:
// the agent's own on rung 2 via the chat, the human's on rung 3 by the
// board's button.
func TestAnswerFundValueDrawsTheShortfall(t *testing.T) {
	for _, tc := range []struct {
		via, decider string
		rung         int
	}{{factoryd.ViaChat, "agent", 2}, {factoryd.ViaButton, "", 3}} {
		r := needRig(t)
		_, s := r.loggedIn(t)
		if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: tc.via, decider: tc.decider}, "t-e", "the fix is in", factoryd.AnswerFund, "", 0); err != nil {
			t.Fatalf("%s: %v", tc.via, err)
		}
		got, _ := actedOnce(t, r)
		if want := fundOf(0.73, "the fix is in", tc.rung, tc.via); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Act took %+v, want %+v", tc.via, got, want)
		}
	}
}

// The member's own click on the chat inbox's fund choice runs the real
// answer route, the human's button: a fund of the shortfall on rung 3.
func TestMemberChatInboxFundClickFunds(t *testing.T) {
	r, chat, _ := newChatRig(t)
	r.tasks.set(needRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, SpentEUR: 1.83, ModelAlias: "factory/small"})
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return needRefusal, nil }
	buttons := factoryd.ShapedQuestion(needRefusal, factoryd.QuestionFacts{BudgetEUR: 2, SpentEUR: 1.83, LeftEUR: 0.17}, nil)
	r.tasks.tasks[0].Question = buttons
	ck, s := r.loggedIn(t)
	inbox, _ := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#13 needs you: fund?", Choices: buttons.Labels(), TaskID: "t-e", Conversation: inbox}); err != nil {
		t.Fatal(err)
	}
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(chat.next, 10)}, "choice": {buttons.Choices[0].Label}})
	if code != http.StatusNoContent {
		t.Fatalf("the answer = %d %q", code, body)
	}
	got, _ := actedOnce(t, r)
	if got.Rung != 3 || got.Res.Action != factoryd.ActFund || got.Res.Args["eur"] != 0.73 {
		t.Errorf("Act took %+v, want a fund of 0.73 EUR on rung 3", got)
	}
}

// A premade fund button answers the card alone: its value carries the
// amount, the click needs no note, the line naming the fund is the note
// posted, and a human who is no approver of the project meets the Fund
// form's refusal with nothing acted.
func TestOneClickFundChoiceFundsWithoutANote(t *testing.T) {
	r := needRig(t)
	_, s := r.loggedIn(t)
	if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.FundChoiceValue(0.73), "", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	if want := fundOf(0.73, "Fund 0.73 EUR and go on.", 3, factoryd.ViaButton); !reflect.DeepEqual(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "Fund 0.73 EUR and go on." {
		t.Fatalf("the note posted = %q", got)
	}

	r = needRig(t)
	r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], "morgan") // writes sf/code, yet not an approver
	r.forgetWriters()
	ck, ms := memberLogin(t, r, "morgan")
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {ms.csrf}, "project": {"ai-software-factory-code"},
		"value": {factoryd.FundChoiceValue(0.5)}}), ck)
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project ai-software-factory-code may fund a card." {
		t.Fatalf("a non-approver's one-click fund = %d: %s", rec.Code, rec.Body)
	}
	if len(r.acted) != 0 {
		t.Errorf("a refused one-click fund reached Act: %+v", r.acted)
	}
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

// The fund choice's raise lifts the session ceiling, not the budget: the
// card has 2.00 - 1.83 = 0.17 EUR left, the 0.40 EUR gate reserve takes it
// whole and more, so the ceiling is -0.23 and a need of 0.90 EUR raises
// 0.90 + 0.23 = 1.13 EUR.
func TestAnswerFundValueCountsTheGateReserve(t *testing.T) {
	r := needRig(t)
	r.c.cfg.GateReserve = func() float64 { return 0.4 }
	_, s := r.loggedIn(t)
	if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "the fix is in", factoryd.AnswerFund, "", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	if want := fundOf(1.13, "the fix is in", 3, factoryd.ViaButton); !reflect.DeepEqual(got, want) {
		t.Errorf("Act took %+v, want %+v", got, want)
	}
}
