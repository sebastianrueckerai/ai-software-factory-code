package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const rungFinding = "3f2a1c9b0e44"

var rungFacts = CaseFacts{Kind: CaseGateStop, Gate: "review", Attempts: 1, SessionCap: 3, AgentTimeS: 0, AgentTimeCapS: 3600,
	BudgetEUR: 5, SpentEUR: 1, LeftEUR: 4, FloorEUR: 0.3, CeilingEUR: 7, PROpen: true, Head: "c1", Checks: "green",
	Findings:     []string{rungFinding},
	CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
	PoolLeft:     policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}

var rungOffered = []string{"resume", "continue", "handover", "reverify", "extend", "fund", "successor", "replan"}

func rungAnswerText(t *testing.T, over map[string]any) string {
	t.Helper()
	a := map[string]any{
		"decision":       "extend",
		"reason_kind":    "gate_stop",
		"verdicts":       map[string]any{rungFinding: map[string]any{"verdict": "dropped", "evidence": "x.go:3 at head: the line is gone"}},
		"note":           "one more session",
		"recommendation": map[string]any{"action": "successor", "grounds": "the finding is dropped"},
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
	return new(Runner).runRung(context.Background(), chat, "t-1", "rung", rungFacts, 1, "system", "statement",
		checkinTools[:2], tools, callCap, time.Hour)
}

func TestRungOfferIsPinned(t *testing.T) {
	if got := answerDecisions(rungFacts, 1); !slices.Equal(got, rungOffered) {
		t.Fatalf("offered = %v, want %v", got, rungOffered)
	}
}

func TestRungForcesTheAnswerToolOnTheLastCallOnly(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{
		{ToolName: "transcript_search"},
		answerCall(t, nil),
	}}
	runRungWith(chat, scriptedTools{out: map[string]string{"transcript_search": "x"}}, 2)
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
	if len(last.Tools) != 1 || last.Tools[0].Name != "answer" || !reflect.DeepEqual(last.ToolChoice, forcedToolChoice("answer")) {
		t.Fatalf("last call = %+v, want the answer tool alone, forced", last)
	}
	want := append(slices.Clone(rungOffered), RungHandUp)
	if got := answerEnum(t, last.Tools[0].Parameters.(map[string]any), "decision"); !slices.Equal(got, want) {
		t.Fatalf("decision enum = %v, want %v", got, want)
	}
}

func TestRungRepairsOneRefusedAnswer(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{answerCall(t, map[string]any{"decision": "teleport"}), answerCall(t, nil)}}
	out := runRungWith(chat, scriptedTools{}, 6)
	if chat.calls != 2 {
		t.Fatalf("calls = %d, want the first and one repair", chat.calls)
	}
	const refusal = `decision "teleport" is not offered at this rung: the rung answers with resume, continue, handover, reverify, extend, fund, successor and replan, or hand_up when it cannot resolve`
	if in := chat.gots[1].Input; !strings.Contains(in, refusal) || !strings.HasSuffix(in, "Answer again through the answer tool.") {
		t.Fatalf("repair input = %q", in)
	}
	a := out.Answer
	if out.HandedUp || a == nil {
		t.Fatalf("outcome = %+v, want an answer", out)
	}
	if a.Decision != "extend" || a.Note != "one more session" || a.Recommendation.Action != "successor" ||
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
	if _, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, hand("withdraw"))); err == nil || !strings.Contains(err.Error(), "not an action for the rungs above") {
		t.Fatalf("a rung 1 hand_up recommending withdraw, which no rung above takes: err = %v", err)
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
		{ToolName: "transcript_search", UsageEUR: 0.1},
		func() CheckinResponse { r := answerCall(t, nil); r.UsageEUR = 0.25; return r }(),
	}}
	out := runRungWith(chat, scriptedTools{out: map[string]string{"transcript_search": "abcd"}}, 6)
	if len(out.Read) != 1 || out.Read[0] != (CaseRead{Tool: "transcript_search", Args: "null", Bytes: 4}) {
		t.Fatalf("read = %+v", out.Read)
	}
	if out.CostEUR != 0.35 {
		t.Fatalf("cost = %v, want 0.35", out.CostEUR)
	}
}

func TestRungEndsAreTyped(t *testing.T) {
	hand := map[string]any{"decision": RungHandUp, "recommendation": map[string]any{"action": "close", "grounds": "g"}}
	for name, tc := range map[string]struct {
		chat checkinChat
		want RungEnd
		up   bool
	}{
		"answered":  {&scriptedChat{answers: []CheckinResponse{answerCall(t, nil)}}, RungAnswered, false},
		"handed up": {&scriptedChat{answers: []CheckinResponse{answerCall(t, hand)}}, RungHandedUp, true},
		"refused":   {&errChat{errors.New("down")}, RungRefused, true},
		"unreadable": {&scriptedChat{answers: []CheckinResponse{
			answerCall(t, map[string]any{"decision": "teleport"}), answerCall(t, map[string]any{"decision": "teleport"})}}, RungUnreadable, true},
	} {
		if out := runRungWith(tc.chat, scriptedTools{}, 6); out.End != tc.want || out.HandedUp != tc.up {
			t.Errorf("%s: end %q, handed up %v; want %q, %v", name, out.End, out.HandedUp, tc.want, tc.up)
		}
	}
}

func TestRungTimeCapEndsItHandedUp(t *testing.T) {
	chat := &scriptedChat{delay: time.Second, answers: []CheckinResponse{{ToolName: "transcript_search"}}}
	out := new(Runner).runRung(context.Background(), chat, "t-1", "rung", rungFacts, 1, "system", "statement", checkinTools[:2], scriptedTools{}, 12, 20*time.Millisecond)
	if !out.HandedUp || out.End != RungTimeCap || out.Answer != nil || out.Found != "the rung's time cap of 20ms ended it before an answer" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestRungCallCapEndsItHandedUp(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "transcript_search"}, {ToolName: "transcript_search"}}}
	out := runRungWith(chat, scriptedTools{out: map[string]string{"transcript_search": "x"}}, 2)
	if !out.HandedUp || out.End != RungCallCap || out.Answer != nil || out.Found != "the rung's call cap of 2 calls ended it before an answer" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestRungRepairsABadAnswerOfTheForcedLastCall(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "transcript_search"}, answerCall(t, map[string]any{"decision": "teleport"}), answerCall(t, nil)}}
	out := runRungWith(chat, scriptedTools{out: map[string]string{"transcript_search": "x"}}, 2)
	if chat.calls != 3 || out.Answer == nil || out.HandedUp {
		t.Fatalf("calls = %d, outcome %+v; want the repair past the cap and its answer", chat.calls, out)
	}
	const last = "This is your last call: answer now through the answer tool."
	if in := chat.gots[2].Input; strings.Count(in, last) != 1 || !strings.HasSuffix(in, "Answer again through the answer tool.") {
		t.Fatalf("repair input = %q, want the last-call line once and the refusal with the request last", in)
	}
	if rep := chat.gots[2]; len(rep.Tools) != 1 || !reflect.DeepEqual(rep.ToolChoice, forcedToolChoice("answer")) {
		t.Fatalf("the repair call = %+v, want the answer tool alone, forced", rep)
	}
	bad := &scriptedChat{answers: []CheckinResponse{{ToolName: "transcript_search"}, answerCall(t, map[string]any{"decision": "teleport"}), answerCall(t, map[string]any{"decision": "teleport"})}}
	out = runRungWith(bad, scriptedTools{out: map[string]string{"transcript_search": "x"}}, 2)
	if bad.calls != 3 || out.End != RungUnreadable || !strings.HasPrefix(out.Found, "the rung's answers were refused twice: ") {
		t.Fatalf("calls = %d, outcome %+v", bad.calls, out)
	}
}

func TestRungRefusesANullVerdictByItsFinding(t *testing.T) {
	_, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, map[string]any{"verdicts": map[string]any{rungFinding: nil}}))
	if err == nil || !strings.Contains(err.Error(), "the verdict on "+rungFinding+" is null; give it as an object with verdict and evidence") {
		t.Fatalf("err = %v", err)
	}
}

func TestRungRefusesAFieldTheSchemaLacks(t *testing.T) {
	for name, over := range map[string]map[string]any{
		"draw":      {"draw": map[string]any{"session": 1}},
		"top level": {"extra": 1},
		"recommend": {"recommendation": map[string]any{"action": "successor", "grounds": "g", "rung": 2}},
	} {
		_, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, over))
		if err == nil || !strings.Contains(err.Error(), "which the schema does not have") {
			t.Errorf("%s: err = %v, want the unknown field refused", name, err)
		}
	}
	_, err := ParseRungAnswer(rungFacts, 1, rungAnswerText(t, map[string]any{"draw": map[string]any{"session": 1}}))
	if err == nil || !strings.Contains(err.Error(), `"session"`) {
		t.Errorf("err = %v, want the field named", err)
	}
}

func TestRungRefusesAToolItDidNotPost(t *testing.T) {
	var ran []string
	chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "run", ToolArgs: map[string]any{"command": "ls"}}, answerCall(t, nil)}}
	out := runRungWith(chat, recordTools{&ran}, 6)
	if len(ran) != 0 {
		t.Fatalf("the pod ran %v, want nothing", ran)
	}
	if in := chat.gots[1].Input; !strings.Contains(in, "tool error: run is not a tool of this conversation; its tools are transcript_search, transcript_read and answer") {
		t.Fatalf("input = %q", in)
	}
	if out.Answer == nil {
		t.Fatalf("outcome = %+v", out)
	}
}

type recordTools struct{ ran *[]string }

func (r recordTools) Tool(_ context.Context, name string, _ map[string]any) (string, bool, error) {
	*r.ran = append(*r.ran, name)
	return "ran", false, nil
}

func TestRungDueLineIsItsOwnLine(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{answerCall(t, nil)}}
	runRungWith(chat, scriptedTools{}, 6)
	if in := chat.gots[0].Input; !regexp.MustCompile(`^statement\nCalls left 6; your answer is due within [0-9hms.]+\.$`).MatchString(in) {
		t.Fatalf("input = %q", in)
	}
}

func TestRungScrubsWhatTheDaemonScrubs(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef01234567"
	r := &Runner{Scrub: redact.New(token, "hunter2secretvalue")}
	err := errors.New("forgejo: 401 for header Authorization: token " + token + "; database password hunter2secretvalue")
	out := r.runRung(context.Background(), &errChat{err}, "t-1", "rung", rungFacts, 1, "system", "statement", nil, scriptedTools{}, 6, time.Hour)
	const want = "the rung's calls were refused: forgejo: 401 for header Authorization: token [redacted]; database password [redacted]"
	if out.Found != want {
		t.Fatalf("found %q, want %q", out.Found, want)
	}
}

func TestRungCapAfterARefusalKeepsTheRefusal(t *testing.T) {
	chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "transcript_search"}, answerCall(t, map[string]any{"decision": "teleport"}), {ToolName: "transcript_search"}}}
	out := runRungWith(chat, scriptedTools{out: map[string]string{"transcript_search": "x"}}, 2)
	const want = "the rung's call cap of 2 calls ended it before an answer; its answer was refused: " +
		`decision "teleport" is not offered at this rung: the rung answers with resume, continue, handover, reverify, extend, fund, successor and replan, or hand_up when it cannot resolve`
	if out.End != RungCallCap || out.Found != want {
		t.Fatalf("outcome = %+v, want found %q", out, want)
	}
}
