package factoryd

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The templates fill from the stops' real reason strings, the ones the
// check-in, the caps, the gates and the sweep compose, with their
// numbers; a reason no template covers shapes nothing.
func TestShapedQuestionFamilies(t *testing.T) {
	facts := QuestionFacts{CheckinCap: 20, CheckinSpent: 0.84, CheckinShare: 0.90, CheckinGrant: 0.30, FloorEUR: 0.20, BudgetEUR: 5, SpentEUR: 4.10, LeftEUR: 0.90}
	timeFacts := facts
	timeFacts.CheckinTime = 5 * time.Minute
	prFacts := facts
	prFacts.PR = 1482
	for name, tt := range map[string]struct {
		reason string
		facts  QuestionFacts
		askHas []string
		whyHas []string
		info   string // the whole info, where the row pins it
		values []string
		source string
		rec    []string
	}{
		"call cap": {
			reason: checkinSpendAsk + "check-in call cap reached",
			facts:  facts,
			askHas: []string{"no answer within its 20 calls"},
			whyHas: []string{"spent its calls on evidence and gave no decision", "€4.10 of its €5.00 budget", "€0.84 of it the check-in's €0.90 share"},
			info:   "An answer starts the session again where the check-in stopped it, with your comment as guidance, and the card's budget must cover its rung's session floor of €0.20. The spent budget stays spent.",
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"check-in share spent": {
			reason: checkinSpendAsk + CheckinShareSpent,
			facts:  facts,
			askHas: []string{"€0.84 of its €0.90 check-in share. May it go on?"},
			whyHas: []string{"€4.10 of its €5.00 budget"},
			info:   "Go on grants the card one more check-in share (€0.30) and starts the session again with your comment as guidance. The spent budget stays spent.",
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"check-in share spent, pull request open": {
			reason: checkinSpendAsk + CheckinShareSpent,
			facts:  prFacts,
			askHas: []string{"€0.84 of its €0.90 check-in share. May it go on?"},
			info:   "An answer starts the attempt again: with a pull request it re-verifies it through the gates, without one your comment rides along as guidance. The spent budget stays spent.",
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"check-in time cap": {
			reason: checkinSpendAsk + checkinTimeCapStop,
			facts:  timeFacts,
			askHas: []string{"no answer within its 5 minutes"},
			whyHas: []string{"spent its time on evidence and gave no decision", "€4.10 of its €5.00 budget"},
			info:   "An answer starts the session again where the check-in stopped it, with your comment as guidance, and the card's budget must cover its rung's session floor of €0.20. The spent budget stays spent.",
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"time cap": {
			reason: timeCapReached + "\nlast: review findings went back to the agent\n- the query lacks the repository",
			facts:  QuestionFacts{SessionCap: 3, Attempts: 2, BudgetEUR: 15, LeftEUR: 9.68, GrantTime: 30 * time.Minute},
			askHas: []string{"used up its agent time after 2 of its 3 sessions", "one more session"},
			whyHas: []string{"review findings went back to the agent", "the query lacks the repository"},
			info:   "A grant starts one more session with 30 minutes of agent time; your comment goes with it as guidance. No answer leaves the card waiting here. The card holds €9.68 of its €15.00 budget.",
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"session cap": {
			reason: capReached + "\nlast: no push: the tests fail\n- the flag is read before it is set",
			facts:  QuestionFacts{SessionCap: 6, SessionGrants: 2, BudgetEUR: 5, LeftEUR: 1.6},
			askHas: []string{"session cap of 6 sessions", "2 granted by your answers", "one more session"},
			whyHas: []string{"no push: the tests fail", "the flag is read before it is set"},
			info:   "A grant starts one more session; your comment goes with it as guidance. No answer leaves the card waiting here. The card holds €1.60 of its €5.00 budget.",
			values: []string{AnswerGrant, AnswerPark, AnswerClose},
		},
		"budget too low": {
			reason: budgetRefused + ": 0.100 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR",
			facts:  QuestionFacts{PR: 4711},
			askHas: []string{"€0.100 of its €2.000 budget", "factory/small needs €0.300000"},
			values: []string{FundChoiceValue(0.2), FundChoiceValue(2), AnswerPark},
			source: "the factory's",
			rec:    []string{"Fund the €0.20 raise: the card holds pull request #4711"},
		},
		"month cap": {
			reason: capRefused(policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "a session call needs 0.300000 EUR but the month's remaining headroom is 0.100000 EUR"}),
			facts:  QuestionFacts{MonthSpentEUR: 544.12, MonthCapEUR: 600, Month: "2026-11"},
			askHas: []string{"month cap of €600.00 is reached", "€544.12 spent", "2026-11", "session was refused"},
			whyHas: []string{"0.300000 EUR", "0.100000 EUR"},
			values: []string{AnswerPark, AnswerFund, AnswerClose},
		},
		"project cap": {
			reason: capRefused(policy.Refusal{Decision: policy.RefusedProjectCap, Reason: "a session call needs 0.300000 EUR but the project's remaining headroom is 0.100000 EUR"}),
			askHas: []string{"project's month cap is reached", "session was refused"},
			values: []string{AnswerPark, AnswerFund, AnswerClose},
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
			q := ShapedQuestion(tt.reason, tt.facts, nil)
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
			if tt.info != "" && q.Info != tt.info {
				t.Errorf("the info = %q, want %q", q.Info, tt.info)
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
			for _, want := range tt.rec {
				if !strings.Contains(q.Recommendation, want) {
					t.Errorf("the recommendation %q misses %q", q.Recommendation, want)
				}
			}
		})
	}
}

// The spend stops' fifth part rides the work state the stop already
// carries: an approved head recommends going on with its money held, a
// concerned or missing verdict recommends nothing.
func TestSpendStopsRecommendFromTheApproveVerdict(t *testing.T) {
	f := QuestionFacts{CheckinSpent: 0.84, CheckinShare: 0.90, BudgetEUR: 5, LeftEUR: 0.9}
	want := "Go on: the last review approved the head, and the card holds €0.90 of its €5.00."
	for _, family := range []string{checkinSpendAsk + "check-in call cap reached", checkinSpendAsk + CheckinShareSpent} {
		q := ShapedQuestion(family+"\n- the head is green\n- review: approve", f, nil)
		if q.Recommendation != want {
			t.Errorf("%s: recommendation %q, want %q", family, q.Recommendation, want)
		}
		if q.Source != "the factory's" {
			t.Errorf("%s: source %q, want the factory's", family, q.Source)
		}
		for _, body := range []string{"\n- review: concerns", "\n- the head is green"} {
			if q := ShapedQuestion(family+body, f, nil); q.Recommendation != "" {
				t.Errorf("%s with %q: recommendation %q, want none", family, body, q.Recommendation)
			}
		}
	}
}

// The budget stop recommends the raise only when a pull request waits on
// it: without one the money alone grounds nothing.
func TestBudgetRaiseRecommendationNamesThePullRequest(t *testing.T) {
	reason := budgetRefused + ": 0.100 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR"
	if q := ShapedQuestion(reason, QuestionFacts{}, nil); q.Recommendation != "" {
		t.Errorf("recommendation without a pull request = %q, want none", q.Recommendation)
	}
	q := ShapedQuestion(reason, QuestionFacts{PR: 4711}, nil)
	want := "Fund the €0.20 raise: the card holds pull request #4711, and the raise funds its next session."
	if q.Recommendation != want {
		t.Errorf("recommendation = %q, want %q", q.Recommendation, want)
	}
}

// The plain check-in tail carries its pace: the card's spend against its
// caps, read at the stop. Other stops carry no such line.
func TestPlainCheckinStopCarriesThePace(t *testing.T) {
	f := QuestionFacts{BudgetEUR: 5, SpentEUR: 4.1, SessionCap: 6, Attempts: 3}
	q := ShapedQuestion("check-in: goal conflict: the card edits the wrong module\n- finding one", f, nil)
	want := "The card has spent €4.10 of its €5.00 budget, 3 attempts against its session cap of 6."
	if !strings.HasSuffix(q.Why, want) {
		t.Errorf("why %q misses the pace %q", q.Why, want)
	}
	if q := ShapedQuestion(gateStopped+"something else\nWhy: x", f, nil); strings.Contains(q.Why, "3 attempts") {
		t.Errorf("a gate stop carries the check-in pace: %q", q.Why)
	}
}

// The stop templates' numbers, read per prefix: the month's spend and
// cap at a month-cap stop, the card's caps, grants and money at the
// caps and the plain check-in tail, the pull request at a budget stop,
// the grant's share at the check-in spend stops.
func TestQuestionFactsFillTheStopNumbers(t *testing.T) {
	store := newFakeStore()
	store.checkinSpent = map[string]float64{"t1": 1.2}
	store.monthSpend = MonthSpend{Month: "2026-11", EUR: 544.12}
	v := policy.Version{Tree: policy.Tree{Document: policy.Document{
		Limits: policy.Limits{CheckinShare: 20, Sessions: 3},
		Risk:   policy.Risk{LargeLines: 1000},
		Budget: policy.Budget{MonthEUR: 600},
	}}}
	r := &Runner{store: store, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	task := Task{ID: "t1", Size: "small", ModelAlias: "factory/small", BudgetEUR: 5, SpentEUR: 4.5, PRNumber: 4711, Usage: TaskUsage{ExtraAttempts: 1}}

	if f := r.questionFacts(context.Background(), task, monthCapReached+"detail"); f.MonthSpentEUR != 544.12 || f.MonthCapEUR != 600 || f.Month != "2026-11" {
		t.Errorf("month numbers = %v of %v (%s), want 544.12 of 600 (2026-11)", f.MonthSpentEUR, f.MonthCapEUR, f.Month)
	}
	if f := r.questionFacts(context.Background(), task, projectCapReached+"detail"); f.MonthCapEUR != 0 || f.Month != "" || f.MonthSpentEUR != 0 {
		t.Errorf("a project cap stop reads the factory month: %+v", f)
	}
	if f := r.questionFacts(context.Background(), task, capReached+"\nlast: x"); f.SessionCap != 4 || f.SessionGrants != 1 || f.Attempts != 0 || f.LeftEUR != 0.5 {
		t.Errorf("cap numbers = cap %d grants %d attempts %d left %v, want 4, 1, 0, 0.5", f.SessionCap, f.SessionGrants, f.Attempts, f.LeftEUR)
	}
	if f := r.questionFacts(context.Background(), task, "check-in: goal conflict: x"); f.BudgetEUR != 5 || f.SessionCap != 4 {
		t.Errorf("the plain check-in tail reads no money or cap: %+v", f)
	}
	if f := r.questionFacts(context.Background(), task, budgetRefused+": detail"); f.PR != 4711 {
		t.Errorf("the budget stop reads no pull request: %d", f.PR)
	}
	if f := r.questionFacts(context.Background(), task, checkinSpendAsk+CheckinShareSpent); f.CheckinGrant != 1 {
		t.Errorf("CheckinGrant = %v, want the base share 1", f.CheckinGrant)
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
		q := ShapedQuestion(reason, QuestionFacts{}, nil)
		if q == nil {
			t.Fatalf("ShapedQuestion(%q) = nil, want the plain question", reason)
		}
		if q.Ask != "This card stopped and needs your decision: how should it go on?" {
			t.Errorf("ShapedQuestion(%q).Ask = %q, want the plain headline", reason, q.Ask)
		}
		if q.Why != escalationQuestion(reason) || q.Recommendation != "" {
			t.Errorf("ShapedQuestion(%q).Why = %q, want the reason's first line %q", reason, q.Why, escalationQuestion(reason))
		}
		if q.Info != plainInfo || len(q.Choices) != 3 {
			t.Errorf("ShapedQuestion(%q) = %+v, want the plain info and three choices", reason, q)
		}
	}
	if q := ShapedQuestion(AgentAsks+"Which file?\nWhy: open", QuestionFacts{}, nil); q.Ask != "Which file?" {
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

// The budget refusal's numbers parse wherever its state puts its own
// numbers between the headroom and the need (holds among them), and the
// raise covers the shortfall to the need in whole cents.
func TestBudgetNumbersSurviveTheHoldsDetail(t *testing.T) {
	reason := "the remaining budget cannot fund a call: raise it.\n" +
		"0.170 EUR of the 5.000 EUR budget remains, outstanding holds take 0.100000 EUR of it (1, the earliest expiring 2026-10-14T19:35:03Z), so 0.000000 EUR is free, and a session on factory/small needs 0.300000 EUR (the floor of its rung, budget.session_floor_cheap_eur in policy.yaml); a session started now would end before it could push."
	if got := BudgetNeed(reason); got != 0.3 {
		t.Errorf("BudgetNeed = %v, want 0.3", got)
	}
	if got := BudgetRemains(reason); got != 0.17 {
		t.Errorf("BudgetRemains = %v, want 0.17", got)
	}
	if got := FundRaise(reason); got != 0.13 {
		t.Errorf("FundRaise = %v, want 0.13, the shortfall in whole cents", got)
	}
}

// The budget question's buttons are the one-click fund amounts: the
// shortfall, the policy's session floor, a round 2.00 EUR, and park;
// a reason with no amount offers the floor and the round raise alone.
func TestBudgetQuestionButtons(t *testing.T) {
	reason := budgetRefused + ": 0.170 EUR of the 5.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR (the floor of its rung, budget.session_floor_cheap_eur in policy.yaml); it cannot start."
	q := ShapedQuestion(reason, QuestionFacts{FloorEUR: 0.3}, nil)
	want := []QuestionChoice{
		{FundChoiceValue(0.13), "Fund 0.13 EUR and go on"},
		{FundChoiceValue(0.3), "Fund 0.30 EUR and go on"},
		{FundChoiceValue(2), "Fund 2.00 EUR and go on"},
		{AnswerPark, "Park it"},
	}
	if !slices.Equal(q.Choices, want) {
		t.Errorf("choices = %v, want %v", q.Choices, want)
	}
	if want := "The card has €0.170 of its €5.000 budget left, and one more session on factory/small needs €0.300000. Fund it, or park it?"; q.Ask != want {
		t.Errorf("ask = %q, want %q", q.Ask, want)
	}
	// the floor the reason itself names stands in when the facts carry none
	q = ShapedQuestion(reason, QuestionFacts{}, nil)
	if q.Choices[1].Value != FundChoiceValue(0.3) {
		t.Errorf("choices = %v, want the refusal's own floor as the second amount", q.Choices)
	}
	// no amount at all: the round raise and park alone
	q = ShapedQuestion(budgetRefused+": the cheapest call needs more. Raise the budget and answer.", QuestionFacts{}, nil)
	if want := []QuestionChoice{{FundChoiceValue(2), "Fund 2.00 EUR and go on"}, {AnswerPark, "Park it"}}; !slices.Equal(q.Choices, want) {
		t.Errorf("choices = %v, want %v", q.Choices, want)
	}
}

// The question's numbers: the call cap is the policy's, one check-in
// conversation's calls, and the share is the card's own as the human's
// answers widened it.
func TestQuestionFactsReportTheCapAndTheGrantedShare(t *testing.T) {
	store := newFakeStore()
	store.checkinSpent = map[string]float64{"t1": 1.2}
	v := policy.Version{Tree: policy.Tree{Document: policy.Document{Limits: policy.Limits{CheckinShare: 20}}}}
	r := &Runner{store: store, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	task := Task{ID: "t1", BudgetEUR: 5, Usage: TaskUsage{ExtraCheckinShare: 2}}
	f := r.questionFacts(context.Background(), task, checkinSpendAsk+"check-in call cap reached")
	if f.CheckinCap != 20 { // the policy's default calls
		t.Errorf("CheckinCap = %d, want the policy's 20", f.CheckinCap)
	}
	if f.CheckinShare != 3 { // 20% of 5 EUR, raised twice
		t.Errorf("CheckinShare = %v, want the raised 3", f.CheckinShare)
	}
}

// Each human-card template asks one line under MaxAsk, fills its why and
// info, and offers exactly its answers in order.
func TestHumanCardQuestions(t *testing.T) {
	alert := SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full", "service": "db"}, StartsAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	for name, tt := range map[string]struct {
		q       HumanQuestion
		ask     string
		choices []QuestionChoice
	}{
		"alert": {alertQuestion(alert, "shop", "prod", "the project runs no agents", "body"),
			"SigNoz fires the alert disk full on service db in prod of shop since 2026-10-01T08:00:00Z: handle it, or wait for it to clear?",
			[]QuestionChoice{{AnswerPark, "Wait for it to clear"}, {AnswerClose, "Handled, close"}}},
		"flux alert": {alertQuestion(SigNozAlert{Fingerprint: "rollout/x", Labels: map[string]string{"alertname": "stuck"}, StartsAt: alert.StartsAt}, "shop", "prod", "w", "b"),
			"Flux fires the alert stuck in prod of shop since 2026-10-01T08:00:00Z: handle it, or wait for it to clear?",
			[]QuestionChoice{{AnswerPark, "Wait for it to clear"}, {AnswerClose, "Handled, close"}}},
		"event alert": {alertQuestion(SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "Job failed", "event": "true"}, StartsAt: alert.StartsAt}, "infra", "forgejo", "w", "b"),
			"SigNoz fired the alert Job failed in forgejo of infra at 2026-10-01T08:00:00Z: handle it, then close.",
			[]QuestionChoice{{AnswerClose, "Handled, close"}}},
		"roll refused": {rollRefusedQuestion("abc1234", []string{"gateway", "worker"}, []string{"gateway: x", "worker: y"}, "https://git/run/1"),
			"roll abc1234 refused: gateway, worker not signed with the factory's key. Fix the signing, then close.",
			[]QuestionChoice{{AnswerClose, "Fixed, close"}}},
		"rerun": {rerunQuestion(1208, "abc1234def5678", "ci / test", []string{"ci / test: lost"}, "factoryd re-ran it once."),
			"main is red at abc1234: re-run ci / test, its runner was lost.",
			[]QuestionChoice{{AnswerClose, "Re-ran it, close"}}},
		"config": {configRejectedQuestion("factory", "c9c9c9c9c9", "https://git/tree", "Version 1 stays current.", "bad key"),
			"config: factory/ at c9c9c9c9c9 rejected: fix the files and push.",
			[]QuestionChoice{{AnswerClose, "Fixed, close"}}},
		"month cap": {monthCapCardQuestion(50, "2026-09", 49.98, "https://git/policy#L7", "refused"),
			"The month cap of 50.00 EUR is reached (2026-09): raise it, or let the factory stand until the month ends?",
			[]QuestionChoice{{AnswerPark, "Let it stand"}, {AnswerClose, "Decided, close"}}},
		"routine": {routineQuestion(policy.Routine{Name: "bump", Title: "Bump the pins"}, "brief", "prov", true),
			"Bump the pins", []QuestionChoice{{AnswerGo, "Have the factory do it"}, {AnswerClose, "Done"}}},
		"issue": {issueQuestion("Two\nlines  here", "text", "sebastian", true),
			"Two lines here", []QuestionChoice{{AnswerGo, "Have the factory do it"}, {AnswerClose, "Done"}}},
		"routine without agents": {routineQuestion(policy.Routine{Name: "bump", Title: "Bump the pins"}, "brief", "prov", false),
			"Bump the pins", []QuestionChoice{{AnswerClose, "Done"}}},
		"issue without agents": {issueQuestion("Two\nlines  here", "text", "sebastian", false),
			"Two lines here", []QuestionChoice{{AnswerClose, "Done"}}},
	} {
		t.Run(name, func(t *testing.T) {
			q := tt.q
			if q.Ask != tt.ask || len(q.Ask) > MaxAsk || strings.Contains(q.Ask, "\n") {
				t.Errorf("ask = %q, want %q on one line under %d bytes", q.Ask, tt.ask, MaxAsk)
			}
			if q.Why == "" || q.Info == "" {
				t.Errorf("why %q, info %q: want both filled", q.Why, q.Info)
			}
			if !reflect.DeepEqual(q.Choices, tt.choices) {
				t.Errorf("choices = %+v, want %+v", q.Choices, tt.choices)
			}
		})
	}
	const lostWhy = "Main CI's push run 1208 of abc1234def5678 failed, but none of its steps did: each failed job lost its runner while it ran " +
		"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card. factoryd re-ran it once."
	if why := rerunQuestion(1208, "abc1234def5678", "ci / test", nil, "factoryd re-ran it once.").Why; why != lostWhy {
		t.Errorf("rerun why = %q, want %q", why, lostWhy)
	}
}

// An event's card names the failed Job its labels carry, and only a name
// Kubernetes could have given: anything else leaves the Job out.
func TestEventAlertCardNamesTheJob(t *testing.T) {
	const note = "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card stays open until a person closes it.\n\nbody"
	a := SigNozAlert{Labels: map[string]string{"alertname": "Job failed", "event": "true", "k8s.object.name": "forgejo-dump-29885910"}}
	want := "The Job `forgejo-dump-29885910` in namespace `forgejo` failed: read its pods' logs " +
		"(`kubectl -n forgejo logs job/forgejo-dump-29885910 --all-containers`). " + note
	if got := alertQuestion(a, "infra", "forgejo", "w", "body").Info; got != want {
		t.Errorf("info = %q, want %q", got, want)
	}
	a.Labels["k8s.object.name"] = "dump`; rm -rf /"
	if got := alertQuestion(a, "infra", "forgejo", "w", "body").Info; got != note {
		t.Errorf("info with a name no Job carries = %q, want %q", got, note)
	}
}

// A variable part of 300 bytes cuts the head, never the question tail,
// and every ask is one line within the title's bound.
func TestAsksFitTheTitle(t *testing.T) {
	long := strings.Repeat("x", 300)
	alert := SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": long}, StartsAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	for name, tt := range map[string]struct{ ask, tail string }{
		"alert":        {alertQuestion(alert, "shop", "prod", "w", "b").Ask, ": handle it, or wait for it to clear?"},
		"roll refused": {rollRefusedQuestion("abc1234", []string{long}, []string{"l"}, "u").Ask, " not signed with the factory's key. Fix the signing, then close."},
		"rerun":        {rerunQuestion(1208, "abc1234", long, []string{"l"}, "w").Ask, ", its runner was lost."},
		"config":       {configRejectedQuestion(long, "c9c9c9c9c9", "l", "s", "r").Ask, " rejected: fix the files and push."},
		"routine":      {routineQuestion(policy.Routine{Name: "n", Title: long[:150] + "\n" + long[:150]}, "b", "p", true).Ask, ""},
		"issue":        {issueQuestion(long, "b", "p", true).Ask, ""},
		"repair":       {repairQuestion("sf/demo-app", "9d9d9d9d9d", []string{long}, "u", nil).Ask, ". Fix it from main's head and push."},
	} {
		if len(tt.ask) > MaxAsk || strings.Contains(tt.ask, "\n") || !strings.HasSuffix(tt.ask, tt.tail) || !strings.Contains(tt.ask, "…") {
			t.Errorf("%s: ask %q (%d bytes), want one line within %d, cut with the tail %q", name, tt.ask, len(tt.ask), MaxAsk, tt.tail)
		}
	}
}

func TestPlainQuestionReadsTheAgentsParts(t *testing.T) {
	q := plainQuestion(AgentAsks+"Ship A or B?\nWhy: both pass.\nRecommendation: A, it is smaller.", QuestionFacts{}, []string{"A", "B", "End the card"})
	if q.Ask != "Ship A or B?" || q.Why != "both pass." || q.Recommendation != "A, it is smaller." || q.Source != "the agent's" {
		t.Errorf("parts = %+v", q)
	}
	if want := []string{"A", "B", "Leave it waiting", "End the card"}; !slices.Equal(q.Labels(), want) {
		t.Errorf("labels = %q, want %q", q.Labels(), want)
	}
	if q.Choices[0].Value != "say:A" || q.Choices[1].Value != "say:B" || q.Choices[2].Value != AnswerPark || q.Choices[3].Value != AnswerClose {
		t.Errorf("values = %+v", q.Choices)
	}
	if q := plainQuestion(AgentAsks+"Which?", QuestionFacts{}, []string{"A", " ", ""}); !slices.Equal(q.Labels(), []string{"A", "Leave it waiting", "End the card"}) {
		t.Errorf("empty choices offer buttons: %q", q.Labels())
	}
	if q := plainQuestion(AgentAsks+"Which?", QuestionFacts{}, []string{"park"}); q.Choices[0].Value != "say:park" {
		t.Errorf("an agent's park label must not carry the park value: %+v", q.Choices)
	}
}

// A fabricated spend stop carries the work state its ask held: the
// findings and the last review verdict ride the reason under its first
// line, and the spend templates lead their Why with them; a bare reason
// keeps today's Why.
func TestSpendStopQuestionCarriesTheWorkState(t *testing.T) {
	body := "\n- the migration runs outside its transaction\n- the flag is read before it is set\n- review: concerns"
	share := ShapedQuestion(checkinSpendAsk+CheckinShareSpent+body, QuestionFacts{CheckinSpent: 0.84, CheckinShare: 0.90}, nil)
	if share.Why != strings.TrimPrefix(body, "\n") {
		t.Errorf("the share why = %q, want the reason's body %q", share.Why, strings.TrimPrefix(body, "\n"))
	}
	callCap := ShapedQuestion(checkinSpendAsk+"check-in call cap reached"+body, QuestionFacts{CheckinCap: 20}, nil)
	if callCap.Why != strings.TrimPrefix(body, "\n") {
		t.Errorf("the call-cap why = %q, want the reason's body %q", callCap.Why, strings.TrimPrefix(body, "\n"))
	}
	bare := ShapedQuestion(checkinSpendAsk+CheckinShareSpent, QuestionFacts{}, nil)
	if bare.Why != "The check-in's share of the budget is spent: the check-in stopped the session, and its checks from here go to a person." {
		t.Errorf("a bare reason's why = %q, want today's text", bare.Why)
	}
}

func TestPlainQuestionReadsASupervisorRecommendation(t *testing.T) {
	for _, tt := range []struct{ reason, source string }{
		{gateStopped + "the run.\nRecommendation: retry the gate.", "the gate supervisor's"},
		{"check-in: the work is done.\nRecommendation: go on.", "the check-in's"},
		{"supervisor escalated: stuck\nRecommendation: close it.", "the factory's"},
	} {
		q := plainQuestion(tt.reason, QuestionFacts{}, nil)
		if q.Recommendation == "" || q.Source != tt.source || !slices.Equal(q.Labels(), (&HumanQuestion{Choices: continueChoices()}).Labels()) {
			t.Errorf("%q: %+v, want source %q", tt.reason, q, tt.source)
		}
	}
}

// The deadlock ask hands the owner's decision to the human as buttons,
// with the card's caps named from the facts the extend form prefills.
func TestDeadlockAskOffersTheExits(t *testing.T) {
	reason := checkinSpendAsk + deadlockAskMark + ": the head a1b2c3d and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting."
	exits := []QuestionChoice{{AnswerExtend, "Extend the card"}, {AnswerOverrule, "Overrule the review"}, {AnswerClose, "End the card"}, {AnswerSuccessor, SuccessorChoice}}
	q := ShapedQuestion(reason, QuestionFacts{SessionCap: 4, LineBound: 1000, BudgetEUR: 9, SpentEUR: 8.6, LeftEUR: 0.4}, nil)
	if !slices.Equal(q.Choices, exits) {
		t.Errorf("choices = %v, want %v", q.Choices, exits)
	}
	if want := "No reply can change this: the card holds a 4-session cap, a 1000-line bound and €0.40 of its €9.00 budget. Extend the card, overrule the review, end the card, or leave it waiting."; q.Ask != want {
		t.Errorf("ask = %q, want %q", q.Ask, want)
	}
	q = ShapedQuestion(reason, QuestionFacts{}, nil)
	if want := "No reply can change this: the head and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting."; q.Ask != want || !slices.Equal(q.Choices, exits) {
		t.Errorf("the bare ask = %q, choices %v", q.Ask, q.Choices)
	}
}

// A stop whose work state carries a refused review names the overrule
// decision: whatever stop it rode, its choices become the exits. An
// approved review keeps the ordinary answers.
func TestReviewRefusedStopOffersTheExits(t *testing.T) {
	exits := []QuestionChoice{{AnswerExtend, "Extend the card"}, {AnswerOverrule, "Overrule the review"}, {AnswerClose, "End the card"}, {AnswerSuccessor, SuccessorChoice}}
	q := ShapedQuestion(checkinSpendAsk+CheckinShareSpent+"\n- the migration runs outside its transaction\n- "+reviewConcernsMark, QuestionFacts{CheckinSpent: 0.84, CheckinShare: 0.90}, nil)
	if !slices.Equal(q.Choices, exits) {
		t.Errorf("the share stop's choices = %v, want %v", q.Choices, exits)
	}
	if want := "The card has spent €0.84 of its €0.90 check-in share. May it go on?"; q.Ask != want {
		t.Errorf("ask = %q, want the share ask unchanged", q.Ask)
	}
	plain := ShapedQuestion("check-in: gate: the checks need a hand\n- "+reviewConcernsMark, QuestionFacts{}, nil)
	if !slices.Equal(plain.Choices, exits) {
		t.Errorf("an unshaped refusal's choices = %v, want the exits", plain.Choices)
	}
	approved := ShapedQuestion(checkinSpendAsk+CheckinShareSpent+"\n- review: approve", QuestionFacts{}, nil)
	if slices.Equal(approved.Choices, exits) {
		t.Errorf("an approved review offers the exits")
	}
}

// The exits' numbers, as the extend form prefills them: the card's own
// caps and money, read from the policy and the row.
func TestQuestionFactsFillTheExitsLimits(t *testing.T) {
	store := newFakeStore()
	store.checkinSpent = map[string]float64{"t1": 1.2}
	v := policy.Version{Tree: policy.Tree{Document: policy.Document{Limits: policy.Limits{CheckinShare: 20, Sessions: 3}, Risk: policy.Risk{LargeLines: 1000}}}}
	r := &Runner{store: store, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	task := Task{ID: "t1", Size: "small", BudgetEUR: 5, SpentEUR: 4.5, Usage: TaskUsage{ExtraAttempts: 1, ExtraLines: 200}}
	f := r.questionFacts(context.Background(), task, checkinSpendAsk+CheckinShareSpent+"\n- "+reviewConcernsMark)
	if f.SessionCap != 4 { // the policy's 3 sessions and one granted
		t.Errorf("SessionCap = %d, want 4", f.SessionCap)
	}
	if f.LineBound != 500 { // small plans 300, the extend raised 200
		t.Errorf("LineBound = %d, want 500", f.LineBound)
	}
	if f.BudgetEUR != 5 || f.SpentEUR != 4.5 || f.LeftEUR != 0.5 {
		t.Errorf("money = %v of %v (left %v), want 4.5 of 5, 0.5 left", f.SpentEUR, f.BudgetEUR, f.LeftEUR)
	}
}

func capQuestionFixture() QuestionFacts {
	return QuestionFacts{MonthSpentEUR: 543.73, MonthCapEUR: 544, Month: "2026-10",
		MonthTurn: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), ProjectFile: "projects/shop.yaml", CapCard: 1290}
}

func choiceValues(q *HumanQuestion) []string {
	var out []string
	for _, c := range q.Choices {
		out = append(out, c.Value+"="+c.Label)
	}
	return out
}

// A card that waits on the month cap on its own says when it resumes, who
// refused it and where the raise goes; its choices never claim a raise is
// needed to go on.
func TestMonthCapQuestionResumesOnItsOwn(t *testing.T) {
	turn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	state := "a session call needs 0.300000 EUR but the month's remaining headroom is 0.100000 EUR"
	choices := []string{AnswerPark + "=Leave it waiting", AnswerFund + "=Retry now", AnswerClose + "=End the card"}
	tail := " The gateway refuses every further call until the cap is raised or the month ends."
	for name, tt := range map[string]struct {
		reason, ask, why, info string
		facts                  QuestionFacts
	}{
		"gateway": {
			reason: capWaitReason(policy.RefusedMonthCap, capGateway(policy.RefusedMonthCap), state, turn),
			facts:  capQuestionFixture(),
			ask:    "The month cap of €544.00 is reached (€543.73 spent, 2026-10). The card resumes on its own when the cap is raised or on 2026-11-01 00:00 UTC. Raise it now?",
			why:    "The gateway refused this session: " + state + tail,
			info:   "Raising the cap is a commit to budget.month_eur in policy.yaml. The cap card #1290 on the board links its line.",
		},
		"floor": {
			reason: capWaitReason(policy.RefusedMonthCap, capFloor(policy.RefusedMonthCap), state, turn),
			facts:  capQuestionFixture(),
			ask:    "The month cap of €544.00 is reached (€543.73 spent, 2026-10). The card resumes on its own when the cap is raised or on 2026-11-01 00:00 UTC. Raise it now?",
			why:    "The attempt's floor refused the session before it started: " + state + tail,
			info:   "Raising the cap is a commit to budget.month_eur in policy.yaml. The cap card #1290 on the board links its line.",
		},
		"no figures, no card": {
			reason: capWaitReason(policy.RefusedMonthCap, capGateway(policy.RefusedMonthCap), state, turn),
			facts:  QuestionFacts{MonthTurn: turn},
			ask:    "The month cap is reached. The card resumes on its own when the cap is raised or on 2026-11-01 00:00 UTC. Raise it now?",
			why:    "The gateway refused this session: " + state + tail,
			info:   "Raising the cap is a commit to budget.month_eur in policy.yaml.",
		},
		"project": {
			reason: capWaitReason(policy.RefusedProjectCap, capFloor(policy.RefusedProjectCap), "projects/shop.yaml: "+state, turn),
			facts:  capQuestionFixture(),
			ask:    "The project's month cap in projects/shop.yaml is reached. The card resumes on its own when it is raised or on 2026-11-01 00:00 UTC. Raise it now?",
			why:    "The attempt's floor refused the session before it started: projects/shop.yaml: " + state + tail,
			info:   "Raising the cap is a commit to budget.month_eur in projects/shop.yaml.",
		},
		"project without a file": {
			reason: capWaitReason(policy.RefusedProjectCap, capGateway(policy.RefusedProjectCap), state, turn),
			facts:  QuestionFacts{MonthTurn: turn},
			ask:    "The project's month cap is reached. The card resumes on its own when it is raised or on 2026-11-01 00:00 UTC. Raise it now?",
			why:    "The gateway refused this session: " + state + tail,
			info:   "Raising the cap is a commit to budget.month_eur in the project's file.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			q := ShapedQuestion(tt.reason, tt.facts, nil)
			if q.Ask != tt.ask || q.Why != tt.why || q.Info != tt.info {
				t.Errorf("question = %q / %q / %q, want %q / %q / %q", q.Ask, q.Why, q.Info, tt.ask, tt.why, tt.info)
			}
			if got := choiceValues(q); !slices.Equal(got, choices) {
				t.Errorf("choices = %v, want %v", got, choices)
			}
		})
	}
}

// The reason of a run from before the cap resumed on its own still waits
// for an answer, and its question says so.
func TestMonthCapQuestionOfAnOldRunWaitsForAnAnswer(t *testing.T) {
	state := "a session call needs 0.300000 EUR but the month's remaining headroom is 0.100000 EUR"
	choices := []string{AnswerPark + "=Leave it waiting", AnswerFund + "=Retry now", AnswerClose + "=End the card"}
	tail := " The gateway refuses every further call until the cap is raised or the month ends."
	q := ShapedQuestion(capRefused(policy.Refusal{Decision: policy.RefusedMonthCap, Reason: state}), capQuestionFixture(), nil)
	if want := "The month cap of €544.00 is reached (€543.73 spent, 2026-10) and the card's session was refused. Raise the cap and answer to let it finish?"; q.Ask != want {
		t.Errorf("ask = %q, want %q", q.Ask, want)
	}
	if want := "The gateway refused this session: " + state + tail; q.Why != want {
		t.Errorf("why = %q, want %q", q.Why, want)
	}
	if want := "Raising the cap is a commit to budget.month_eur in policy.yaml. The cap card #1290 on the board links its line. This card waits for an answer."; q.Info != want {
		t.Errorf("info = %q, want %q", q.Info, want)
	}
	if got := choiceValues(q); !slices.Equal(got, choices) {
		t.Errorf("choices = %v, want %v", got, choices)
	}
	q = ShapedQuestion(capRefused(policy.Refusal{Decision: policy.RefusedProjectCap, Reason: state}), capQuestionFixture(), nil)
	if want := "The project's month cap in projects/shop.yaml is reached and the card's session was refused. Raise the cap and answer to let it finish?"; q.Ask != want {
		t.Errorf("project ask = %q, want %q", q.Ask, want)
	}
	if want := "Raising the cap is a commit to budget.month_eur in projects/shop.yaml. This card waits for an answer."; q.Info != want {
		t.Errorf("project info = %q, want %q", q.Info, want)
	}
}

// questionFacts finds the open cap card through the alert of the cap in
// force, the turn of the month, and for a project cap the project's file.
func TestQuestionFactsOfTheMonthCapStops(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.monthSpend = MonthSpend{Month: "2026-10", EUR: 543.73}
	store.tasks["cap1"] = Task{ID: "cap1", Status: "waiting-human", IssueNumber: 1290}
	store.tasks["cap0"] = Task{ID: "cap0", Status: "done", IssueNumber: 1200}
	insert := func(fp, taskID string) {
		id, _ := store.InsertAlert(ctx, Alert{Kind: AlertMonthCap, Source: "100", Fingerprint: fp, Verdict: VerdictReal})
		_ = store.SetAlertTask(ctx, id, taskID, nil)
	}
	insert("2026-10/100/500", "cap0")
	insert("2026-10/100/544", "cap1")
	v := policy.Version{Tree: policy.Tree{
		Document: policy.Document{Budget: policy.Budget{MonthEUR: 544}},
		Projects: map[string]policy.ProjectConfig{"shop": {Repository: "acme/shop"}},
	}}
	r := &Runner{store: store, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
	task := Task{ID: "t1", Repository: "acme/shop"}

	f := r.questionFacts(ctx, task, monthCapResumes+"\nx")
	if f.MonthSpentEUR != 543.73 || f.MonthCapEUR != 544 || f.Month != "2026-10" || f.CapCard != 1290 {
		t.Errorf("month facts = %+v, want 543.73 of 544 (2026-10) with cap card 1290", f)
	}
	if want := monthTurn(time.Now()); !f.MonthTurn.Equal(want) {
		t.Errorf("MonthTurn = %v, want %v", f.MonthTurn, want)
	}

	store.tasks["cap1"] = Task{ID: "cap1", Status: "done", IssueNumber: 1290}
	if f := r.questionFacts(ctx, task, monthCapResumes+"\nx"); f.CapCard != 0 {
		t.Errorf("a done cap card is open: %d", f.CapCard)
	}
	v.Document.Budget.MonthEUR = 600
	store.tasks["cap1"] = Task{ID: "cap1", Status: "waiting-human", IssueNumber: 1290}
	if f := r.questionFacts(ctx, task, monthCapResumes+"\nx"); f.CapCard != 0 {
		t.Errorf("the card of another cap is the cap in force's: %d", f.CapCard)
	}

	f = r.questionFacts(ctx, task, projectCapResumes+"\nx")
	if f.ProjectFile != "projects/shop.yaml" || f.MonthCapEUR != 0 || f.Month != "" || f.CapCard != 0 || !f.MonthTurn.Equal(monthTurn(time.Now())) {
		t.Errorf("project facts = %+v, want projects/shop.yaml, the turn, no factory month", f)
	}

	bare := &Runner{store: store}
	f = bare.questionFacts(ctx, task, projectCapResumes+"\nx")
	if f.ProjectFile != "" || !f.MonthTurn.Equal(monthTurn(time.Now())) {
		t.Errorf("project facts without a policy = %+v, want the turn and no file", f)
	}
	if q := ShapedQuestion(projectCapResumes+"\nx", f, nil); !strings.HasPrefix(q.Ask, "The project's month cap is reached. ") || strings.Contains(q.Ask, "0001") {
		t.Errorf("ask without a policy = %q", q.Ask)
	}
}
