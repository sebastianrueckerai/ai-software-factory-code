package factoryd

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// readPointer names a session's transcript read by the session and its n.
func readPointer(session string, n int) string { return fmt.Sprintf("read %s n %d", session, n) }

func TestCardStepReturnsASessionsStepsForASessionLine(t *testing.T) {
	g := skeletonRig(t)
	got, err := g.r.CardStep(context.Background(), "t1410", 2, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	want := "#1410 step 2 of 8 at 2026-11-01 00:41:00 UTC\n" +
		"   2 +0:41   session 1 small        → pushed a1b2c3d, 0.41 EUR, 23 calls\n" +
		"session s-1-41, 3 timeline lines:\n" +
		" +0:41   line 1  session_start: attempt 1, model factory/small\n" +
		" +0:55   line 2  session_end: attempt 1, model factory/small, outcome pushed, started 2026-11-01T00:41:00Z, summary done\n" +
		" +0:55   line 3  pushed: branch factory/t1410, commit a1b2c3d4e5f6a7b8\n" +
		"transcript: read s-1-41 n 1\n"
	if got != want {
		t.Fatalf("CardStep =\n%s\nwant\n%s", got, want)
	}
	third, err := g.r.CardStep(context.Background(), "t1410", 8, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	if !strings.HasSuffix(third, "\ntranscript: read s-5-600 n 3\n") {
		t.Fatalf("CardStep of the card's third session =\n%s\nwant its read with n 3", third)
	}
	none, err := g.r.CardStep(context.Background(), "t1410", 8, func(string, int) string { return "" })
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	if strings.Contains(none, "transcript:") {
		t.Fatalf("CardStep with no read to name =\n%s\nwant no transcript line", none)
	}
}

func TestCardStepReturnsTheFivePartsAndTheAnswerForAnAskLine(t *testing.T) {
	ctx := context.Background()
	g := newDigestRig(nil)
	g.event(30, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Card 1410 asks: grant one more session?", "comment": 27001,
		"source": "cap", "reason": "the card used its sessions", "ask": "Grant one more session?", "why": "The card used its 3 sessions.",
		"info": "Session 3 pushed nothing.", "recommendation": "Grant one session.", "choices": []any{"Grant one session", "Stop the card"}})
	g.store.cases = []Case{{ID: 1, TaskID: "t1410", Kind: CaseSessionCap, StateKey: "k1", Status: CaseAtHuman, CommentID: 27001}}
	g.store.caseSteps = map[int64][]CaseStep{1: {{ID: 1, CaseID: 1, Rung: 3, Actor: "sebastian", Outcome: StepInProgress}}}
	open, err := g.r.CardStep(ctx, "t1410", 2, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	want := "#1410 step 2 of 2 at 2026-11-01 00:30:00 UTC\n" +
		"   2 +0:30   ask                    → session_cap: Grant one more session?\n" +
		"Ask: Grant one more session?\n" +
		"Why: The card used its 3 sessions.\n" +
		"Info: Session 3 pushed nothing.\n" +
		"Recommendation: Grant one session.\n" +
		"Choices: Grant one session; Stop the card\n" +
		"Answer: none yet\n" +
		"case 1 session_cap, at_human\n" +
		" rung 3 by sebastian → in_progress\n"
	if open != want {
		t.Fatalf("CardStep of the open ask =\n%s\nwant\n%s", open, want)
	}
	g.event(40, EventQuestionAnswered, "sebastian", map[string]any{"answer": "Grant one session", "comment": 27002})
	g.event(50, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Card 1410 asks: grant one more?", "comment": 27003,
		"source": "cap", "reason": "the card used its sessions"})
	g.event(60, EventQuestionAnswered, "anna", map[string]any{"answer": "Stop the card", "comment": 27004})
	answered, err := g.r.CardStep(ctx, "t1410", 2, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	if !strings.Contains(answered, "\nAnswer by sebastian: Grant one session\n") || strings.Contains(answered, "anna") {
		t.Fatalf("CardStep of the answered ask =\n%s\nwant sebastian's answer, not the next ask's", answered)
	}
	second, err := g.r.CardStep(ctx, "t1410", 4, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	if !strings.Contains(second, "\nAsk: Card 1410 asks: grant one more?\nWhy: none\n") || !strings.Contains(second, "\nAnswer by anna: Stop the card\n") {
		t.Fatalf("CardStep of the second ask =\n%s\nwant its question as the ask and anna's answer", second)
	}
}

func TestCardStepRefusesAnUnknownStep(t *testing.T) {
	g := skeletonRig(t)
	for _, n := range []int{0, 9} {
		_, err := g.r.CardStep(context.Background(), "t1410", n, readPointer)
		if want := "#1410 has steps 1 to 8; the card read numbers them"; err == nil || err.Error() != want {
			t.Fatalf("CardStep(%d) error = %v, want %q", n, err, want)
		}
	}
}

// A check-in line with no record states no calls and no cost: the line
// does not hold them.
func TestCardStepStatesNoCallsForACheckinWithoutARecord(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 10, "checkin", map[string]any{"round": 1, "decision": CheckHandover, "moment": CheckInGate,
		"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}}})
	got, err := g.r.CardStep(context.Background(), "t1410", 2, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	if want := "\nmoment gate, decision handover\n- " + idA + " confirmed: " + findingA + "\n"; !strings.HasSuffix(got, want) {
		t.Fatalf("CardStep =\n%s\nwant it to end\n%s", got, want)
	}
}

// A check-in whose record holds no decision was cut: its line in the
// digest and its step's header say so with the line's decision, the
// detail states the line's decision, and the cut stays flagged.
func TestCardStepKeepsTheLinesDecisionOverAnEmptyRecord(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 10, "checkin", map[string]any{"round": 1, "decision": CheckHandover, "moment": CheckInGate,
		"record": CheckInRecord{Moment: CheckInGate, ToolCalls: 2}})
	got, err := g.r.CardStep(context.Background(), "t1410", 2, readPointer)
	if err != nil {
		t.Fatalf("CardStep: %v", err)
	}
	if want := "\n   2 +0:10   checkin gate           → cut, handover\nmoment gate, decision handover, 2 tool calls, 0.00 EUR\n"; !strings.Contains(got, want) {
		t.Fatalf("CardStep =\n%s\nwant it to hold\n%s", got, want)
	}
	if r := g.digest(t).Render(); !strings.Contains(r, "flags: checkin_cut(+0:10)") {
		t.Fatalf("Render =\n%s\nwant the cut flagged", r)
	}
}

// Each kind's detail is its source row in full: the card row for the
// filing, the timeline line for a gate and a check-in, the event for an
// answer.
func TestCardStepReturnsEachKindsSource(t *testing.T) {
	g := skeletonRig(t)
	for _, tc := range []struct {
		n    int
		want []string
	}{
		{1, []string{"\nfiled by planner, size medium, budget 5.00 EUR\n", "\nplan #1344 session-metrics, revision 2\n",
			"\ndepends on #1402 (done)\n"}},
		{3, []string{"\nfindings:\n- " + idA + " " + findingA + "\n- " + idB + " " + findingB + "\n", "\nsummary: two reads miss the repository\n",
			"\nverdict: concerns\n"}},
		{4, []string{"\nmoment gate, decision handover, 4 tool calls, 0.00 EUR\n", "\n- " + idA + " confirmed: " + findingA + "\n"}},
		{6, []string{"\nanswer: Grant one session\ncomment: 27002\n"}},
	} {
		got, err := g.r.CardStep(context.Background(), "t1410", tc.n, readPointer)
		if err != nil {
			t.Fatalf("CardStep(%d): %v", tc.n, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Fatalf("CardStep(%d) =\n%s\nwant it to hold\n%s", tc.n, got, w)
			}
		}
	}
}
