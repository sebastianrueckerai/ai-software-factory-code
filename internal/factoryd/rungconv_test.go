package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const rungFinding = "3f2a1c9b0e44"

var rungFacts = CaseFacts{Kind: CaseGateStop, Gate: "review", Attempts: 1, SessionCap: 3, AgentTimeS: 0, AgentTimeCapS: 3600,
	BudgetEUR: 5, SpentEUR: 1, LeftEUR: 4, FloorEUR: 0.3, CeilingEUR: 7, PROpen: true, Head: "c1", Checks: "green",
	Findings:     []string{rungFinding},
	CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
	PoolLeft:     policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}

var rungOffered = []string{"resume", "continue", "handover", "reverify", "extend", "fund", "successor", "replan", "withdraw", "note"}

func rungAnswerText(t *testing.T, over map[string]any) string {
	t.Helper()
	a := map[string]any{
		"decision":       "extend",
		"reason_kind":    "gate_stop",
		"verdicts":       map[string]any{rungFinding: map[string]any{"verdict": "dropped", "evidence": "x.go:3 at head: the line is gone"}},
		"note":           "one more session",
		"recommendation": map[string]any{"action": "note", "grounds": "the finding is dropped"},
		"draw":           map[string]any{"sessions": 1},
	}
	for k, v := range over {
		a[k] = v
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func answerCall(t *testing.T, over map[string]any) CheckinResponse {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(rungAnswerText(t, over)), &args); err != nil {
		t.Fatal(err)
	}
	return CheckinResponse{ToolName: checkinAnswerTool, ToolArgs: args}
}

func runRungWith(chat checkinChat, tools podTools, callCap int) RungOutcome {
	return runRung(context.Background(), chat, "t-1", "rung", rungFacts, 1, "system", "statement",
		checkinTools[:2], tools, callCap, time.Hour)
}

func TestRungOfferIsPinned(t *testing.T) {
	if got := answerDecisions(rungFacts, 1); !slices.Equal(got, rungOffered) {
		t.Fatalf("offered = %v, want %v", got, rungOffered)
	}
}

func TestRungForcesTheAnswerToolOnTheLastCallOnly(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{
		{ToolName: "card_facts"},
		answerCall(t, nil),
	}}
	runRungWith(chat, scriptedTools{out: map[string]string{"card_facts": "x"}}, 2)
	if len(chat.gots) != 2 {
		t.Fatalf("calls = %d, want 2", len(chat.gots))
	}
	if chat.gots[0].ToolChoice != nil {
		t.Fatalf("the first call forces %v, want none", chat.gots[0].ToolChoice)
	}
	if !slices.Equal(toolNames(chat.gots[0].Tools), []string{"transcript_search", "transcript_read", "answer"}) {
		t.Fatalf("first call tools = %v", toolNames(chat.gots[0].Tools))
	}
	last := chat.gots[1]
	if len(last.Tools) != 1 || last.Tools[0].Name != "answer" || last.ToolChoice == nil {
		t.Fatalf("last call = %+v, want the answer tool alone, forced", last)
	}
	want := append(slices.Clone(rungOffered), RungHandUp)
	if got := answerEnum(t, last.Tools[0].Parameters.(map[string]any), "decision"); !slices.Equal(got, want) {
		t.Fatalf("decision enum = %v, want %v", got, want)
	}
}

func toolNames(ts []CheckinTool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestRungRepairsOneRefusedAnswer(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{answerCall(t, map[string]any{"decision": "teleport"}), answerCall(t, nil)}}
	out := runRungWith(chat, scriptedTools{}, 6)
	if chat.calls != 2 {
		t.Fatalf("calls = %d, want the first and one repair", chat.calls)
	}
	const refusal = `decision "teleport" is not offered at this rung: the rung answers with resume, continue, handover, reverify, extend, fund, successor, replan, withdraw and note`
	if in := chat.gots[1].Input; !strings.Contains(in, refusal) || !strings.HasSuffix(in, "Answer again through the answer tool.") {
		t.Fatalf("repair input = %q", in)
	}
	a := out.Answer
	if out.HandedUp || a == nil {
		t.Fatalf("outcome = %+v, want an answer", out)
	}
	if a.Decision != "extend" || a.Note != "one more session" || a.Recommendation.Action != "note" ||
		a.Verdicts[rungFinding].Verdict != "dropped" || a.Draw == nil || a.Draw.Sessions != 1 {
		t.Fatalf("answer = %+v", a)
	}
}

func TestRungHandsUpOnASecondRefusal(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{
		answerCall(t, map[string]any{"decision": "teleport"}),
		answerCall(t, map[string]any{"reason_kind": "nonsense"}),
	}}
	out := runRungWith(chat, scriptedTools{}, 6)
	if !out.HandedUp || out.Answer != nil || chat.calls != 2 {
		t.Fatalf("outcome = %+v after %d calls, want handed up after the first call and one repair", out, chat.calls)
	}
	if !strings.Contains(out.Found, `decision "teleport" is not offered`) || !strings.Contains(out.Found, `reason_kind "nonsense"`) {
		t.Fatalf("found = %q, want both refusals", out.Found)
	}
}

func TestRungHandUpAnswerKeepsItsRecommendation(t *testing.T) {
	hand := func(action string) map[string]any {
		return map[string]any{"decision": RungHandUp, "recommendation": map[string]any{"action": action, "grounds": "g"}}
	}
	out := runRungWith(&scriptedChat{answers: []CheckinResponse{answerCall(t, hand("close"))}}, scriptedTools{}, 6)
	if !out.HandedUp || out.Answer == nil || out.Answer.Recommendation.Action != "close" {
		t.Fatalf("outcome = %+v, want handed up with the recommendation close", out)
	}
	if _, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, hand("teleport"))); err == nil {
		t.Fatal("a hand_up recommending teleport parsed")
	}
	if _, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, map[string]any{"recommendation": map[string]any{"action": "close", "grounds": "g"}})); err == nil {
		t.Fatal("an extend answer recommending close parsed")
	}
}

func TestRungHandoverNeedsItsModel(t *testing.T) {
	if _, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, map[string]any{"decision": "handover"})); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("err = %v, want the missing model named", err)
	}
	a, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, map[string]any{"decision": "handover", "model": "stronger"}))
	if err != nil || a.Model != "stronger" {
		t.Fatalf("answer %+v, err %v", a, err)
	}
	if _, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, map[string]any{"model": "best"})); err == nil {
		t.Fatal("a model outside the enum parsed")
	}
}

func TestRungScrubsRefusedCallErrors(t *testing.T) {
	secret := errors.New("post https://user:pass@host/x: Authorization: Bearer abc123")
	out := runRungWith(&errChat{secret}, scriptedTools{}, 6)
	if !out.HandedUp || out.Found == "" {
		t.Fatalf("outcome = %+v, want handed up with the refusal named", out)
	}
	for _, leak := range []string{"user:pass", "abc123"} {
		if strings.Contains(out.Found, leak) {
			t.Fatalf("found %q leaks %q", out.Found, leak)
		}
	}
}

func TestRungReadsAndCost(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{
		{ToolName: "card_facts", UsageEUR: 0.1},
		func() CheckinResponse { r := answerCall(t, nil); r.UsageEUR = 0.25; return r }(),
	}}
	out := runRungWith(chat, scriptedTools{out: map[string]string{"card_facts": "abcd"}}, 6)
	if len(out.Read) != 1 || out.Read[0] != (CaseRead{Tool: "card_facts", Args: "null", Bytes: 4}) {
		t.Fatalf("read = %+v", out.Read)
	}
	if out.CostEUR != 0.35 {
		t.Fatalf("cost = %v, want 0.35", out.CostEUR)
	}
}
