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

// roomOf is the room of sessions' worth, with its money when money, that
// Act takes with note on case 1, the rig's live case of a card waiting on
// a human: the project agent's on rung 2, sebastian's on rung 3.
func roomOf(sessions int, money bool, note string, rung int, via string) factoryd.ActInput {
	by := "sebastian"
	if rung == 2 {
		by = factoryd.ChatUser
	}
	return factoryd.ActInput{TaskID: "t-e", CaseID: 1, By: by, Via: via, Rung: rung,
		Res: factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": sessions, "money": money}, Note: note}}
}

// A wake's give_room needs no human's login: the agent's own decision on
// rung 2, as the chat user and not as the woken human, and the note goes
// on the card as the answer with the project's chat token.
func TestGiveRoomAtAWakeAnswersAsTheChatBot(t *testing.T) {
	r := fundRig(t)
	p := factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}
	s, err := r.c.wakeSession(p, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	tool, _ := r.c.chatTool("give_room")
	if _, _, err := tool.run(&turn{c: r.c, r: req.WithContext(context.WithValue(req.Context(), ctxKey{}, s)),
		a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: true, decider: "agent", level: policy.AutonomyAutonomous}, p: p},
		toolInput{TaskID: "t-e", Sessions: 1, Money: true, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	got, posts := actedOnce(t, r)
	want := roomOf(1, true, "go", 2, factoryd.ViaChat)
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

const needRefusal = "the remaining budget cannot fund a call: 0.17 EUR of the 2.00 EUR budget remains, and a session on factory/small needs 0.90 EUR. Raise the budget (Fund on the board) and answer, or close the card."

// needRig is the fund rig over card t-e held by needRefusal, which names
// a need of 0.90 EUR against the card's 2.00 - 1.83 = 0.17 EUR left: a raise
// of 0.73 EUR.
func needRig(t *testing.T) *rig {
	r := fundRig(t)
	r.tasks.set(needRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, SpentEUR: 1.83, ModelAlias: "factory/small"})
	return r
}

// The give_room proposal names the room the member presses Do it on: the
// count the call names, 1 when it names none, and the money left out when
// it says so.
func TestGiveRoomProposalNamesTheRoom(t *testing.T) {
	for args, room := range map[string]string{
		`{"task_id":"t-cap","sessions":2,"text":"go"}`:               `2 sessions' worth`,
		`{"task_id":"t-cap","text":"go"}`:                            `1 session's worth`,
		`{"task_id":"t-cap","sessions":1,"money":false,"text":"go"}`: `1 session's worth without money`,
	} {
		r, g := newWritesRig(t)
		ck, s := r.loggedIn(t)
		g.replies = [][]string{{call(1, "give_room", args)}}
		_, proposed, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"give it room"}})
		want := `"description":"Give a card more room, #14 t-cap \"Trim the logs.\", ` + room + `, text go"`
		if len(proposed) == 0 || !strings.Contains(proposed[0], want) {
			t.Errorf("%s: proposal %v, want it to hold %s", args, proposed, want)
		}
	}
}

// A count off 1 to 5 is refused when the call is made, by Act's own check
// of the count, so the agent corrects it before any human reads a
// proposal: no proposal is kept and nothing reaches Act.
func TestGiveRoomRefusesABadCountBeforeItsProposal(t *testing.T) {
	r, _, _ := newChatRig(t)
	caseCard(r)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.cfg.Confirm = []string{"give_room"}
	for _, n := range []string{"0", "6"} {
		result, proposal, status := trn.call(factoryd.OperatorCall{Name: "give_room", Arguments: `{"task_id":"13","sessions":` + n + `,"text":"go on"}`})
		if proposal != "" || status != http.StatusBadRequest || result != "Refused: a room counts 1 to 5 sessions' worth" {
			t.Errorf("a room of %s = %q, proposal %q, status %d; want 400 with the count's range and no proposal", n, result, proposal, status)
		}
	}
	if len(r.acted) != 0 {
		t.Errorf("a refused count reached Act: %+v", r.acted)
	}
}
