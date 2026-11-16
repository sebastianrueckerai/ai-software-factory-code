package acceptance

// The face-conformance pin of plan #1850, the whisper half: every plain
// kind of the board's set renders its face from the fixtures and the
// spend stays muted below 80 percent of the budget and turns prominent
// from 80 on (plain_cards_whisper); a kind the board's set holds
// without a fixture fails the run naming the kind; and a bound ask
// answers by its button alone. The asking faces' own pin — the one
// decide block and its one send — lives in one_face_test.go.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// faceFixture names one card kind's board fixture: the fixture file per
// kind of the board's set (console.FaceKinds).
var faceFixture = map[string]string{
	"agent":          "board-agent",
	"human":          "board-human",
	"chat":           "board-chat",
	"merge":          "board-merge",
	"resource":       "board-resource",
	"plan":           "board-plan-question",
	"criteria":       "board-criteria",
	"criteria-rerun": "board-criteria-rerun",
}

// faceFixtureDir is the fixtures' home beside the factoryd package.
const faceFixtureDir = "../internal/factoryd/testdata/fixtures"

// loadFaceFixture loads the kind's board fixture, failing the run when
// it does not stand.
func loadFaceFixture(t *testing.T, kind string) factoryd.Fixture {
	t.Helper()
	fx, err := factoryd.LoadFixture(filepath.Join(faceFixtureDir, faceFixture[kind]+".json"))
	if err != nil {
		t.Fatalf("the %s fixture: %v", kind, err)
	}
	return fx
}

// seatFace stands the kind's fixture card on the board as id, at status
// with the named budget and spend: the fixture's card, the whisper
// gate's dials. Each seating mints its own id, so one rig holds a kind
// at every spend at once.
func seatFace(t *testing.T, r *AcceptanceRig, kind, id, status string, budget, spend float64) {
	t.Helper()
	c := loadFaceFixture(t, kind).Card
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: c.Kind,
		Status: status, Track: c.Track, Repository: c.Repository, IssueNumber: c.Issue,
		Objective: c.Objective, BudgetEUR: budget, SpentEUR: spend}})
}

// faceOf reads the board and answers the seated card id's rendered
// article, the card's own face.
func faceOf(t *testing.T, r *AcceptanceRig, ck *http.Cookie, id string) string {
	t.Helper()
	body := r.request(http.MethodGet, "/", nil, ck).Body.String()
	at := strings.Index(body, `data-card="`+id+`"`)
	if at < 0 {
		t.Fatalf("the board holds no face of %s", id)
	}
	from := strings.LastIndex(body[:at], "<article")
	to := strings.Index(body[at:], "</article>")
	if from < 0 || to < 0 {
		t.Fatalf("the board renders %s outside an article", id)
	}
	return body[from : at+to+len("</article>")]
}

// TestPlainCardsWhisper names the claim: plain cards whisper. Every
// plain kind of the board's set renders at 60, 79.99 and 80 percent of
// its budget and the spend stays muted below 80 percent and turns
// prominent from 80 on — the chat kind alone holds no board card, its
// absence shown — and the rendered faces land in the artifacts file the
// plain_cards_whisper judgement reads.
func TestPlainCardsWhisper(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	ck, _ := r.Login(t)
	// a budget of 100.00 EUR makes the named spends whole cents on
	// their own sides of the gate's line: 79.99 percent of it sits one
	// cent below the 80 percent nearBudget's whole-cent rule draws
	const budget = 100
	spends := []struct {
		name      string
		spend     float64
		prominent bool
	}{{"60 percent", 60, false}, {"79.99 percent", 79.99, false}, {"80 percent", 80, true}}
	var artifact strings.Builder
	for _, kind := range console.FaceKinds {
		// the asking kinds hold their decide pin in one_face_test.go:
		// neither holds a board card whose spend could whisper
		if kind == "merge" || kind == "resource" || kind == "plan" {
			continue
		}
		for _, s := range spends {
			name := kind + " at " + s.name
			id := fmt.Sprintf("t-%s-%g", kind, s.spend)
			seatFace(t, r, kind, id, "running", budget, s.spend)
			// the standing chat task rides the chat surface: the board
			// holds no card of kind chat at any spend (the store drops
			// the kind, as the rig's BoardTasks mirrors), so no spend of
			// it could whisper — the absence shown, the near check kept
			// out for that reason
			if kind == "chat" {
				if body := r.request(http.MethodGet, "/", nil, ck).Body.String(); strings.Contains(body, `data-card="`+id+`"`) {
					t.Errorf("%s: the chat kind holds a board card after all: seat it with the others and pin its whisper gate", name)
				}
				artifact.WriteString("<section id=\"" + name + "\">\n<p>no board card: the standing chat task rides the chat surface, so no spend of it could whisper</p>\n</section>\n")
				continue
			}
			face := faceOf(t, r, ck, id)
			artifact.WriteString("<section id=\"" + name + "\">\n" + face + "\n</section>\n")
			near := strings.Contains(face, `class="spend near"`)
			if s.prominent && !near {
				t.Errorf("%s: the spend stays muted at 80 percent of budget", name)
			}
			if !s.prominent && near {
				t.Errorf("%s: the spend turns prominent below 80 percent of budget", name)
			}
		}
	}
	// the asking face beside the whispers, for the judgement's contrast:
	// the plan's question row, seated as the one-face pin seats it
	pfx := loadFaceFixture(t, "plan")
	r.mu.Lock()
	r.tasks = append(r.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: pfx.Card.TaskID, Kind: "agent",
		Status: "waiting-human", Track: pfx.Card.Track, Repository: pfx.Card.Repository,
		IssueNumber: pfx.Card.Issue, Objective: pfx.Card.Objective}})
	r.caseNext++
	r.cases[pfx.Card.TaskID] = []factoryd.Case{{ID: r.caseNext, TaskID: pfx.Card.TaskID,
		Repository: pfx.Card.Repository, Kind: factoryd.CasePlanQuestion, Status: factoryd.CaseAtHuman,
		Problem: pfx.Asks[0].Ask,
		Facts: map[string]any{"revision": float64(2), "index": float64(1),
			"choices": []any{"Redis", "In memory"}, "recommended": pfx.Asks[0].Recommendation},
		OpenedAt: time.Now().Add(-80 * time.Second)}}
	r.mu.Unlock()
	artifact.WriteString("<section id=\"plan asking face\">\n" +
		faceOf(t, r, ck, pfx.Card.TaskID) + "\n</section>\n")
	if err := os.WriteFile("plain_cards_whisper.html", []byte(artifact.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFaceConformancePinned pins the registry itself: every kind of the
// board's own set has its fixture, so a stub kind added to the set
// without a fixture fails the run with a message naming it.
func TestFaceConformancePinned(t *testing.T) {
	for _, kind := range console.FaceKinds {
		name, ok := faceFixture[kind]
		if !ok {
			t.Errorf("the face conformance does not cover the board kind %q: add its fixture and its face rules", kind)
			continue
		}
		if _, err := factoryd.LoadFixture(filepath.Join(faceFixtureDir, name+".json")); err != nil {
			t.Errorf("the board kind %q names fixture %s, which does not load: %v", kind, name, err)
		}
	}
}

// TestBoundAskAnswersByItsButtonAlone pins the chat's bound ask: while
// it stands open a typed message delivers the message but resolves
// nothing — the ask stays open with its binding unused — and the
// button's press settles the ask and runs the bound call exactly once.
func TestBoundAskAnswersByItsButtonAlone(t *testing.T) {
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	// the standing ask row and its binding, as the ask tool leaves them
	const token = "bound-ask-token"
	r.mu.Lock()
	r.proposals = append(r.proposals, &accProposal{factoryd.ChatProposal{Token: token, Kind: "ask",
		Repository: "sf/code", Login: "sebastian", Conversation: 1, Tool: "close",
		Args: json.RawMessage(`{"task_id":"t-m"}`), Choice: "close it",
		ExpiresAt: time.Now().Add(time.Minute)}, time.Time{}})
	r.mu.Unlock()
	id, err := r.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant",
		Kind: factoryd.MessageAsk, Content: "Close #5 now?", TaskID: "t-m",
		Choices: []string{"close it", "hold"}, Proposal: token})
	if err != nil {
		t.Fatal(err)
	}
	before := len(r.ToolCalls("sf/code", "sebastian"))
	// the typed message: the member's words, no press
	r.Reply([]string{Say("Noted.")})
	if code, body := postChat(t, r, path, ck, url.Values{"message": {"close it, do it now"}, "csrf": {csrf}}); code != http.StatusOK {
		t.Fatalf("the typed message = %d: %s", code, body)
	}
	ask := chatAskOf(t, r, path, ck, token)
	if ask["settled"] == true {
		t.Fatalf("the typed message settled the bound ask: %v", ask)
	}
	if choices, _ := ask["choices"].([]any); len(choices) != 2 {
		t.Errorf("the open ask lost its buttons: %v", ask)
	}
	if got := len(r.ToolCalls("sf/code", "sebastian")) - before; got != 0 {
		t.Fatalf("%d bound tool calls ran on the typed message, want none", got)
	}
	// the press of the bound button alone settles it and runs the call
	press := url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(id, 10)},
		"choice": {"close it"}, "csrf": {csrf}}
	if code, body := postChat(t, r, path+"/answer", ck, press); code != http.StatusOK {
		t.Fatalf("the press = %d: %s", code, body)
	}
	if ask := chatAskOf(t, r, path, ck, token); ask["settled"] != true {
		t.Fatalf("the press did not settle the ask: %v", ask)
	}
	if got := len(r.ToolCalls("sf/code", "sebastian")) - before; got != 1 {
		t.Fatalf("%d bound tool calls ran on the press, want exactly one", got)
	}
}

// chatAskOf reads the chat and answers the bound ask row of token as
// its JSON fields, failing when no ask row stands.
func chatAskOf(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, token string) map[string]any {
	t.Helper()
	var page struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(r.request(http.MethodGet, path, nil, ck).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for i := len(page.Messages) - 1; i >= 0; i-- {
		if page.Messages[i]["proposal"] == token {
			return page.Messages[i]
		}
	}
	t.Fatalf("no ask row of proposal %s stands in the chat", token)
	return nil
}
