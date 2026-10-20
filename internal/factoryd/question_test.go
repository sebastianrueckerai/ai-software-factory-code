package factoryd

import (
	"context"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The templates fill from the stops' real reason strings, the ones the
// check-in, the caps, the gates and the sweep compose, with their
// numbers; a reason no template covers shapes nothing.
func TestShapedQuestionFamilies(t *testing.T) {
	facts := QuestionFacts{CheckinCalls: 12, CheckinCap: 12, CheckinSpent: 0.84, CheckinShare: 0.90}
	for name, tt := range map[string]struct {
		reason string
		facts  QuestionFacts
		askHas []string
		whyHas []string
		values []string
		source string
	}{
		"call cap": {
			reason: checkinSpendAsk + "check-in call cap reached",
			facts:  facts,
			askHas: []string{"12 of its 12 check-in calls"},
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"check-in share spent": {
			reason: checkinSpendAsk + "the check-in's share of the budget is spent",
			facts:  facts,
			askHas: []string{"€0.84 of its €0.90 check-in share (12 calls)"},
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"session cap": {
			reason: capReached,
			askHas: []string{"session cap is reached", "one more session"},
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"budget too low": {
			reason: budgetRefused + ": 0.100 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR",
			askHas: []string{"€0.100 of its €2.000 budget", "factory/small needs €0.300000"},
			values: []string{AnswerFund, AnswerClose},
		},
		"month cap": {
			reason: monthCapReached + "a session call needs 0.300000 EUR but the month's remaining headroom is 0.100000 EUR",
			askHas: []string{"month cap is reached", "session was refused"},
			whyHas: []string{"0.300000 EUR", "0.100000 EUR"},
			values: []string{AnswerFund, AnswerPark, AnswerClose},
		},
		"project cap": {
			reason: projectCapReached + "a session call needs 0.300000 EUR but the project's remaining headroom is 0.100000 EUR",
			askHas: []string{"project's month cap is reached", "session was refused"},
			values: []string{AnswerFund, AnswerPark, AnswerClose},
		},
		"WaitChecks past cap": {
			reason: gateStopped + "WaitChecks (attempt 2) past its bound of 30 min: checks still red after 3 runs.\nRecommendation: Rerun the flaky check once, then close the card.",
			askHas: []string{"attempt 2, bound 30 min"},
			values: []string{AnswerKeepWaiting, AnswerClose},
			source: "the gate supervisor's",
		},
		"quiet on its dependencies": {
			reason: "The card was quiet for 61 min while waiting on its dependencies: is its run gone?\n" +
				"Why: nothing reached its timeline or events for that long, and a nudge signal went unanswered.\n" +
				"Recommendation: answer on the issues of the cards it waits on, or close this issue to end the wait.",
			askHas: []string{"quiet 61 min while waiting on its dependencies"},
			values: []string{AnswerKeepWaiting, AnswerClose},
			source: "the stale sweep's",
		},
	} {
		t.Run(name, func(t *testing.T) {
			q := ShapedQuestion(tt.reason, tt.facts)
			if q == nil {
				t.Fatalf("ShapedQuestion(%q) = nil, want a shaped question", tt.reason)
			}
			if strings.ContainsAny(q.Ask, "\n") {
				t.Errorf("the ask runs over one sentence: %q", q.Ask)
			}
			for _, want := range tt.askHas {
				if !strings.Contains(q.Ask, want) {
					t.Errorf("the ask %q misses %q", q.Ask, want)
				}
			}
			if q.Why == "" || q.Info == "" {
				t.Errorf("the why %q or the info %q is empty", q.Why, q.Info)
			}
			for _, want := range tt.whyHas {
				if !strings.Contains(q.Why, want) {
					t.Errorf("the why %q misses %q", q.Why, want)
				}
			}
			if len(q.Choices) != len(tt.values) {
				t.Fatalf("choices %v, want values %v", q.Choices, tt.values)
			}
			seen := map[string]bool{}
			for i, c := range q.Choices {
				if c.Value != tt.values[i] {
					t.Errorf("choice %d is %q, want %q", i, c.Value, tt.values[i])
				}
				if c.Label == "" || seen[c.Label] {
					t.Errorf("choice %d's label %q is empty or repeated", i, c.Label)
				}
				seen[c.Label] = true
			}
			if q.Source != tt.source {
				t.Errorf("source %q, want %q", q.Source, tt.source)
			}
		})
	}
}

// A reason no template covers still gets the five parts: the plain
// question, whose summary is the reason's first line and whose choices
// are the answers a stopped card takes; an agent's ask keeps its own
// question as the headline.
func TestShapedQuestionShapesTheUntemplatedTail(t *testing.T) {
	for _, reason := range []string{
		checkinSpendAsk + "the work is nearly done and one more call would finish it",
		providerRefused + "session 3's first call",
		"The card was moved out of the merge queue: is its place gone?",
	} {
		q := ShapedQuestion(reason, QuestionFacts{})
		if q == nil {
			t.Fatalf("ShapedQuestion(%q) = nil, want the plain question", reason)
		}
		if q.Ask != "This card stopped and needs your decision: how should it go on?" {
			t.Errorf("ShapedQuestion(%q).Ask = %q, want the plain headline", reason, q.Ask)
		}
		if q.Why != escalationQuestion(reason) {
			t.Errorf("ShapedQuestion(%q).Why = %q, want the reason's first line %q", reason, q.Why, escalationQuestion(reason))
		}
		if q.Info != plainInfo || len(q.Choices) != 3 {
			t.Errorf("ShapedQuestion(%q) = %+v, want the plain info and three choices", reason, q)
		}
	}
	if q := ShapedQuestion(AgentAsks+"Which file?\nWhy: open", QuestionFacts{}); q.Ask != "Which file?" {
		t.Errorf("an agent's ask keeps its question as the headline, got %q", q.Ask)
	}
}

// A click's value is the label's, and an unknown label has none.
func TestChoiceValue(t *testing.T) {
	q := &HumanQuestion{Choices: []QuestionChoice{{AnswerGrant, "Grant one session"}, {AnswerPark, "Leave it waiting"}}}
	if got := q.ChoiceValue("Grant one session"); got != AnswerGrant {
		t.Errorf("ChoiceValue = %q, want %q", got, AnswerGrant)
	}
	if got := q.ChoiceValue("no such button"); got != "" {
		t.Errorf("ChoiceValue of an unknown label = %q, want \"\"", got)
	}
}

// BudgetNeed reads the shortfall the budget refusal states, and only it.
func TestBudgetNeed(t *testing.T) {
	reason := budgetRefused + ": 0.100 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR"
	if got := BudgetNeed(reason); got != 0.3 {
		t.Errorf("BudgetNeed = %v, want 0.3", got)
	}
	for _, reason := range []string{
		capReached,
		budgetRefused + ": no numbers in this tail",
		"",
	} {
		if got := BudgetNeed(reason); got != 0 {
			t.Errorf("BudgetNeed(%q) = %v, want 0", reason, got)
		}
	}
}

// The question's numbers are the card's own: the cap and the share as the
// human's answers widened them, not the policy's base — an ask that reads
// "24 of its 12" sends a person hunting for a bug that is not there.
func TestQuestionFactsReportTheRaisedCapAndShare(t *testing.T) {
	store := newFakeStore()
	store.checkinCalls = map[string]int{"t1": 24}
	store.checkinSpent = map[string]float64{"t1": 1.2}
	v := policy.Version{Tree: policy.Tree{Document: policy.Document{Limits: policy.Limits{CheckinCalls: 12, CheckinShare: 20}}}}
	r := &Runner{store: store, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	task := Task{ID: "t1", BudgetEUR: 5, Usage: TaskUsage{ExtraCheckin: 1, ExtraCheckinShare: 2}}
	f := r.questionFacts(context.Background(), task, checkinSpendAsk+"check-in call cap reached")
	if f.CheckinCap != 24 { // 12 calls, raised once
		t.Errorf("CheckinCap = %d, want the raised 24", f.CheckinCap)
	}
	if f.CheckinShare != 3 { // 20% of 5 EUR, raised twice
		t.Errorf("CheckinShare = %v, want the raised 3", f.CheckinShare)
	}
}
