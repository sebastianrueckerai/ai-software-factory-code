package acceptance

// The face-conformance pin of plan #1850: one file renders every kind of
// card the board can show from its fixture and pins the face contract —
// the asking faces (merge, resource, escalation, plan) hold one decide
// block in the five-part shape with one fund control, one note box and
// one send whose POST lands answer, fund and note in one row; the plain
// kinds whisper (spend muted below 80 percent of budget, prominent from
// 80 on); a bound ask answers by its button alone; and a kind the
// board's set holds without a fixture fails the run naming the kind.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// faceFixture seats one card kind's board fixture: the fixture file per
// kind of the board's set, escalation standing for the human kind's
// asking face.
var faceFixture = map[string]string{
	"agent":          "board-agent",
	"human":          "board-human",
	"chat":           "board-chat",
	"merge":          "board-merge",
	"resource":       "board-resource",
	"plan":           "board-plan-question",
	"criteria":       "board-criteria",
	"criteria-rerun": "board-criteria-rerun",
	"escalation":     "board-agent-escalate",
}

// askingFace says which seated faces ask a human: the case kinds merge,
// resource and plan plus the escalation seating; every other kind
// whispers.
func askingFace(name string) bool {
	return name == "merge" || name == "resource" || name == "plan" || name == "escalation"
}

// faceFixtureDir is the fixtures' home beside the factoryd package.
const faceFixtureDir = "../internal/factoryd/testdata/fixtures"

// seatFace stands fixture name's card on the rig at spend of its
// budget: its waiting card and its at-human cases. It answers the
// card's task id.
func seatFace(t *testing.T, r *AcceptanceRig, name string, spend float64) string {
	t.Helper()
	fx, err := factoryd.LoadFixture(filepath.Join(faceFixtureDir, faceFixture[name]+".json"))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	c := fx.Card
	r.mu.Lock()
	bt := factoryd.BoardTask{Task: factoryd.Task{ID: c.TaskID, Kind: c.Kind,
		Status: "waiting-human", Track: c.Track, Repository: c.Repository, IssueNumber: c.Issue,
		Objective: c.Objective, BudgetEUR: c.BudgetEUR, SpentEUR: spend}}
	if len(fx.Asks) > 0 {
		ask := fx.Asks[0]
		q := &factoryd.HumanQuestion{Ask: ask.Ask, Why: ask.Why, Recommendation: ask.Recommendation, Source: "the run"}
		if q.Choices = ask.Choices; len(q.Choices) == 0 {
			q.Choices = []factoryd.QuestionChoice{{Value: factoryd.AnswerGo, Label: "Go on"}, {Value: factoryd.AnswerPark, Label: "Leave it waiting"}}
		}
		bt.Question = q
	}
	r.tasks = append(r.tasks, bt)
	r.mu.Unlock()
	return c.TaskID
}

// faceMarker answers the fixture's asking words: its ask's question.
func faceMarker(fx factoryd.Fixture) string {
	return fx.Asks[0].Ask
}

// faceOf reads the board and answers the seated card taskID's rendered
// article, the card's own face.
func faceOf(t *testing.T, r *AcceptanceRig, ck *http.Cookie, taskID, marker string) string {
	t.Helper()
	body := r.request(http.MethodGet, "/", nil, ck).Body.String()
	i := strings.Index(body, marker)
	if i < 0 {
		at := strings.Index(body, taskID)
		round := body
		if at >= 0 {
			lo := max(0, at-200)
			round = body[lo:min(len(body), at+900)]
		}
		t.Fatalf("the board does not show %s's marker %q (card at %d):\n%s", taskID, marker, at, round)
	}
	from := strings.LastIndex(body[:i], "<article")
	to := strings.Index(body[i:], "</article>")
	if from < 0 || to < 0 {
		t.Fatalf("the board renders %s outside an article", taskID)
	}
	return body[from : i+to]
}

// TestOneFaceEveryAskingKind pins the asking face: every asking kind —
// merge, resource, plan and the escalation seating — rendered from its
// fixture holds exactly one decide form of the shared five-part face
// with exactly one fund control and one note box, and its one send's
// POST lands the answer, the fund and the note together in one row on
// the card.
func TestOneFaceEveryAskingKind(t *testing.T) {
	for _, name := range []string{"merge", "resource", "plan", "escalation"} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := NewAcceptanceRig(t, false, nil)
			ck, csrf := r.Login(t)
			fx, err := factoryd.LoadFixture(filepath.Join(faceFixtureDir, faceFixture[name]+".json"))
			if err != nil {
				t.Fatalf("fixture %s: %v", name, err)
			}
			taskID := seatFace(t, r, name, 0)
			marker := faceMarker(fx)
			face := faceOf(t, r, ck, taskID, marker)
			wantRec := fx.Asks[0].Recommendation
			if got := strings.Count(face, `<div class="parts">`); got != 1 {
				t.Errorf("%s's face holds %d five-part headers, want the one shared partial", name, got)
			}
			if got := strings.Count(face, `name="fund_eur"`); got != 1 {
				t.Errorf("%s's face holds %d fund controls, want exactly one", name, got)
			}
			if got := strings.Count(face, `name="note"`); got != 1 {
				t.Errorf("%s's face holds %d note boxes, want exactly one", name, got)
			}
			forms := regexp.MustCompile(`<form method="post" action="([^"]*)"[^>]*class="decide`).FindStringSubmatch(face)
			if forms == nil {
				t.Fatalf("%s's face holds no decide send:\n%s", name, face)
			}
			action, err := url.PathUnescape(forms[1])
			if err != nil {
				t.Fatalf("the decide send's action %q: %v", forms[1], err)
			}
			if wantRec != "" && !strings.Contains(face, wantRec) {
				t.Errorf("%s's face lacks the recommendation %q", name, wantRec)
			}
			form := url.Values{"project": {accProjectName}, "value": {factoryd.AnswerGo},
				"fund_eur": {"1.50"}, "note": {"one row"}, "csrf": {csrf}}
			rec := r.Post(action, ck, form)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("the decide send of %s = %d: %s", name, rec.Code, rec.Body)
			}
			var answered []*factoryd.Event
			for _, e := range r.Events() {
				if e.Kind == factoryd.EventQuestionAnswered && e.TaskID == taskID {
					answered = append(answered, &e)
				}
			}
			if len(answered) != 1 {
				t.Fatalf("%d answered rows on %s's card, want the one send's", len(answered), taskID)
			}
			row := answered[0]
			fund := fmt.Sprint(row.Detail["fund_eur"])
			if fund != "1.5" && fund != "1.50" {
				t.Errorf("the send's row carries fund %v, want 1.50", row.Detail["fund_eur"])
			}
			if note, _ := row.Detail["note"].(string); note != "one row" {
				t.Errorf("the send's row carries note %v, want the one note", row.Detail["note"])
			}
		})
	}
}

// TestPlainCardsWhisper names the claim: plain cards whisper. Every
// plain kind of the board's set renders at 60, 79.99 and 80 percent of
// its budget and the spend stays muted below 80 percent and turns
// prominent from 80 on, and the rendered faces land in the artifacts
// file the plain_cards_whisper judgement reads.
func TestPlainCardsWhisper(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	ck, _ := r.Login(t)
	spends := []struct {
		name      string
		spend     float64
		prominent bool
	}{{"60 percent", 6, false}, {"79.99 percent", 7.999, false}, {"80 percent", 8, true}}
	var artifact strings.Builder
	for _, kind := range console.FaceKinds {
		if kind == "merge" || kind == "resource" || kind == "plan" {
			continue
		}
		for _, s := range spends {
			name := kind + " at " + s.name
			taskID := seatFace(t, r, kind, s.spend)
			face := faceOf(t, r, ck, taskID, seatObjective(t, r, taskID))
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
	planFx, err := factoryd.LoadFixture(filepath.Join(faceFixtureDir, faceFixture["plan"]+".json"))
	if err != nil {
		t.Fatal(err)
	}
	artifact.WriteString("<section id=\"plan asking face\">\n" +
		faceOf(t, r, ck, seatFace(t, r, "plan", 8), faceMarker(planFx)) + "\n</section>\n")
	if err := os.WriteFile(filepath.Join("plain_cards_whisper.html"), []byte(artifact.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seatObjective answers the seated card's objective, a marker its face
// surely renders.
func seatObjective(t *testing.T, r *AcceptanceRig, taskID string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, bt := range r.tasks {
		if bt.ID == taskID {
			return bt.Objective
		}
	}
	t.Fatalf("no seated card %s", taskID)
	return ""
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
	const token = "bound-ask-token"
	r.mu.Lock()
	r.proposals = append(r.proposals, &accProposal{factoryd.ChatProposal{Token: token, Kind: "ask",
		Repository: "sf/code", Login: "sebastian", Tool: "close", Args: json.RawMessage(`{"task_id":"t-m"}`),
		Choice: "close it"}, time.Time{}})
	r.mu.Unlock()
	id, err := r.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant",
		Kind: factoryd.MessageAsk, Content: "Close #5 now?", TaskID: "t-m",
		Choices: []string{"close it", "hold"}, Proposal: token})
	if err != nil {
		t.Fatal(err)
	}
	before := len(r.ToolCalls("sf/code", "sebastian"))
	if rec := r.Post(path, ck, url.Values{"message": {"close it, do it now"}, "csrf": {csrf}}); rec.Code != http.StatusOK {
		t.Fatalf("the typed message = %d: %s", rec.Code, rec.Body)
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
	if rec := r.pressAnswer(t, path, ck, csrf, id, "close it"); rec.Code != http.StatusNoContent {
		t.Fatalf("the press = %d: %s", rec.Code, rec.Body)
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
