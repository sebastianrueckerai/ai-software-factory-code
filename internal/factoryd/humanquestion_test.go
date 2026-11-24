package factoryd

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A click's value is the label's, and an unknown label has none.
func TestChoiceValue(t *testing.T) {
	q := &HumanQuestion{Choices: []QuestionChoice{{AnswerGo, "Have the factory do it"}, {AnswerPark, "Not now"}}}
	if got := q.ChoiceValue("have the factory do it."); got != AnswerGo {
		t.Errorf("ChoiceValue = %q, want %q", got, AnswerGo)
	}
	if got := q.ChoiceValue("no such button"); got != "" {
		t.Errorf("ChoiceValue of an unknown label = %q, want \"\"", got)
	}
}

// With no free headroom the first raise is the need rounded up to the
// cent: an opening of 0.123456 EUR asks 0.13, since 0.12 does not cover it
// and the floor would refuse again. A need on a whole cent stays on it: 1.10.
func TestTheRaiseRoundsAnUncoveredNeedUpToTheCent(t *testing.T) {
	if got := fundFor(0.123456, 0); got != 0.13 {
		t.Errorf("the raise = %v, want 0.13", got)
	}
	if got := fundFor(1.1, 0); got != 1.1 {
		t.Errorf("the raise = %v, want 1.1", got)
	}
}

// Each human card's question asks one line, fills its why and info, says
// what the factory saw, and offers exactly its answers in order, in the
// vocabulary's words. It keeps each instant its texts name beside their
// words in UTC, in the layout the words take: a summer alert's start, a
// winter stall's and the hold's, and the reset of the month's count on an
// earlier day.
func TestHumanCardQuestions(t *testing.T) {
	did, notNow, factory, noted := QuestionChoice{AnswerDone, "I did it"}, QuestionChoice{AnswerPark, "Not now"},
		QuestionChoice{AnswerGo, "Have the factory do it"}, QuestionChoice{AnswerNoted, "Noted"}
	alert := SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full", "service": "db"}, StartsAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	summer := time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC)
	winter := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	reset := &policy.CapReset{At: time.Date(2026, 11, 2, 14, 5, 0, 0, time.UTC), Login: "sebastian", BeforeEUR: 50}
	fired := Moments{{At: alert.StartsAt, Layout: "2 January at 15:04"}}
	moments := map[string]Moments{"alert": fired, "flux alert": fired, "event alert": fired,
		"summer alert":            {{At: summer, Layout: "2 January at 15:04"}},
		"month cap after a reset": {{At: reset.At, Layout: "2 Jan 15:04"}},
		"chain stall":             {{At: winter, Layout: "2006-01-02 15:04"}, {At: winter, Layout: "2 January at 15:04"}},
		"production held":         {{At: winter, Layout: "2006-01-02 15:04"}}}
	for name, tt := range map[string]struct {
		q        HumanQuestion
		ask      string
		choices  []QuestionChoice
		happened string
	}{
		"alert": {AlertQuestion(alert, "shop", "prod", "the project runs no agents", "body"),
			"SigNoz fires the alert “disk full” on db", []QuestionChoice{noted, notNow}, "It has fired since 1 October at 08:00 UTC."},
		"flux alert": {AlertQuestion(SigNozAlert{Fingerprint: "rollout/x", Labels: map[string]string{"alertname": "stuck"}, StartsAt: alert.StartsAt}, "shop", "prod", "w", "b"),
			"Flux fires the alert “stuck” on prod", []QuestionChoice{noted, notNow}, "It has fired since 1 October at 08:00 UTC."},
		"event alert": {AlertQuestion(SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "Job failed", "event": "true"}, StartsAt: alert.StartsAt}, "infra", "forgejo", "w", "b"),
			"SigNoz fires the alert “Job failed” on forgejo", []QuestionChoice{noted, notNow}, "It has fired since 1 October at 08:00 UTC."},
		"summer alert": {AlertQuestion(SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full", "service": "db"}, StartsAt: summer}, "shop", "prod", "w", "b"),
			"SigNoz fires the alert “disk full” on db", []QuestionChoice{noted, notNow}, "It has fired since 1 July at 10:30 UTC."},
		"month cap after a reset": {MonthCapCardQuestion(50, MonthSpend{Month: "2026-11", EUR: 50.01, AllEUR: 98.5, Reset: reset}, "https://git/policy#L7", time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)),
			"This month's spending reached the factory's limit of 50.00 EUR", []QuestionChoice{noted, notNow},
			"The factory spent 50.01 EUR of its 50.00 EUR limit since sebastian reset the month's count, 98.50 EUR in November."},
		"roll refused": {RollRefusedQuestion("abc1234", []string{"gateway", "worker"}, []string{"gateway: x", "worker: y"}, "https://git/run/1"),
			"Fix the signing of the images gateway, worker, then close this card", []QuestionChoice{did, notNow},
			"Main's run is green at commit abc1234, and 2 images of it carry no signature by the factory's key."},
		"rerun": {RerunQuestion(1208, "abc1234def5678", "ci / test", []string{"ci / test: lost"}, "The factory re-ran it once."),
			"Re-run the check ci / test on main after a lost runner", []QuestionChoice{did, notNow},
			"CI run 1208 on main failed in ci / test, and each failed job lost its runner while it ran."},
		"config": {ConfigRejectedQuestion("factory", "c9c9c9c9c9", "https://git/tree", "Version 1 stays current.", "bad key"),
			"Fix the rejected configuration in factory/ and push", []QuestionChoice{did, notNow}, "The factory refused commit c9c9c9c9c9 of factory/. Version 1 stays current."},
		"month cap": {MonthCapCardQuestion(50, MonthSpend{Month: "2026-09", EUR: 49.98, Refusal: "refused"}, "https://git/policy#L7", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)),
			"This month's spending reached the factory's limit of 50.00 EUR", []QuestionChoice{noted, notNow},
			"The factory spent 49.98 EUR of its 50.00 EUR limit in September."},
		"routine": {RoutineQuestion(policy.Routine{Name: "bump", Title: "Bump the pins"}, "brief", "prov", true),
			"Bump the pins", []QuestionChoice{did, factory, notNow}, ""},
		"issue": {IssueQuestion("Two\nlines  here", "text", "sebastian", true),
			"Two lines here", []QuestionChoice{did, factory, notNow}, ""},
		"routine without agents": {RoutineQuestion(policy.Routine{Name: "bump", Title: "Bump the pins"}, "brief", "prov", false),
			"Bump the pins", []QuestionChoice{did, notNow}, ""},
		"issue without agents": {IssueQuestion("Two\nlines  here", "text", "sebastian", false),
			"Two lines here", []QuestionChoice{did, notNow}, ""},
		"failed roll": {RollFailedQuestion([]RollFailure{{Title: "Roll the images", Issue: 41}}), "",
			[]QuestionChoice{{AnswerRetryRoll, "Try the roll again"}, notNow, {AnswerClose, "End the card"}},
			"The failed roll cards: #41."},
		"chain stall": {StallQuestion("sf/code", "abc1234def", time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), []string{"ci / test"}, nil), "",
			[]QuestionChoice{{AnswerRerunAgain, "Run the chain again"}, {AnswerCheckAgain, "Look again"}, {AnswerDismiss, "End the card"}},
			"The chain at commit abc1234 has been pending since 1 November at 08:00 UTC, waiting on 1 check."},
		"chain repair": {RepairQuestion("sf/code", "abc1234def", []string{"CI run 16 failure", "required context deploy/staging: failure"}, "", nil), "",
			[]QuestionChoice{did, notNow}, "The chain of sf/code is red at commit abc1234, with 2 failed checks."},
		"production held": {HoldQuestion("code", time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
			[]Group{{Plan: "t-plan", Issue: 12, Repository: "sf/code"}}, nil, nil), "",
			[]QuestionChoice{{AnswerHoldWait, "Not now"}, {ActDeployNow, "Deploy now"},
				{AnswerReplanOnTop + ":t-plan", "Replan plan #12 on top"}, {AnswerTakeOffMain + ":t-plan", "Take plan #12 off main and replan"}},
			"Production of code has waited 2 hours: plan #12 is open on main."},
	} {
		t.Run(name, func(t *testing.T) {
			q := tt.q
			if tt.ask != "" && q.Ask != tt.ask || strings.Contains(q.Ask, "\n") {
				t.Errorf("ask = %q, want %q on one line", q.Ask, tt.ask)
			}
			if q.Why == "" || q.Info == "" {
				t.Errorf("why %q, info %q: want both filled", q.Why, q.Info)
			}
			if !reflect.DeepEqual(q.Choices, tt.choices) {
				t.Errorf("choices = %+v, want %+v", q.Choices, tt.choices)
			}
			if q.Happened != tt.happened {
				t.Errorf("happened = %q, want %q", q.Happened, tt.happened)
			}
			if !reflect.DeepEqual(q.Moments, moments[name]) {
				t.Errorf("moments = %+v, want %+v", q.Moments, moments[name])
			}
			for _, m := range q.Moments {
				if !strings.Contains(q.Info+" "+q.Happened, m.Words()) {
					t.Errorf("no text says %q", m.Words())
				}
			}
		})
	}
	for _, tc := range []struct {
		q    HumanQuestion
		info string
	}{
		{StallQuestion("sf/code", "abc1234def", winter, []string{"ci / test"}, nil), "The chain of sf/code has been pending at commit abc1234 since 2026-11-01 08:00 UTC, past the stall bound."},
		{HoldQuestion("code", winter, winter.Add(2*time.Hour), []Group{{Plan: "t-plan", Issue: 12, Repository: "sf/code"}}, nil, nil), "Production has waited since 2026-11-01 08:00 UTC."},
		{MonthCapCardQuestion(50, MonthSpend{Month: "2026-11", EUR: 50.01, AllEUR: 98.5, Reset: reset}, "l", time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)),
			"is 98.50 EUR; counted since 2 Nov 14:05 UTC, when sebastian reset the month's count: 50.01 EUR of the cap"},
	} {
		if !strings.Contains(tc.q.Info, tc.info) {
			t.Errorf("info = %q, want it to hold %q", tc.q.Info, tc.info)
		}
	}
	const lostWhy = "CI run 1208 on main, at commit abc1234, failed, but none of its steps did: each failed job lost its runner while it ran " +
		"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card. The factory re-ran it once."
	if info := RerunQuestion(1208, "abc1234def5678", "ci / test", nil, "The factory re-ran it once.").Info; !strings.HasPrefix(info, lostWhy+"\n\n") {
		t.Errorf("rerun info = %q, want it to open with %q", info, lostWhy)
	}
}

// An event's card names the failed Job its labels carry, and the CronJob
// whose next run closes the card, and only a name Kubernetes could have
// given: anything else leaves the Job out.
func TestEventAlertCardNamesTheJob(t *testing.T) {
	const note = "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card stays open until a human closes it.\n\nbody"
	a := SigNozAlert{Labels: map[string]string{"alertname": "Job failed", "event": "true", "k8s.object.name": "forgejo-dump-29885910"}}
	const cronNote = "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card closes when the CronJob `forgejo-dump` completes a run, or when a human closes it.\n\nbody"
	want := "The Job `forgejo-dump-29885910` in namespace `forgejo` failed: read its pods' logs " +
		"(`kubectl -n forgejo logs job/forgejo-dump-29885910 --all-containers`). " + cronNote
	if got := AlertQuestion(a, "infra", "forgejo", "w", "body").Info; got != want {
		t.Errorf("info = %q, want %q", got, want)
	}
	a.Labels["k8s.object.name"] = "dump`; rm -rf /"
	if got := AlertQuestion(a, "infra", "forgejo", "w", "body").Info; got != note {
		t.Errorf("info with a name no Job carries = %q, want %q", got, note)
	}
}

// A room that already covers the need of 0.30 EUR needs no raise.
func TestTheRaiseIsZeroWhenTheRoomCoversTheNeed(t *testing.T) {
	if got := fundFor(0.3, policy.SessionHeadroom(0.7, 0.4)); got != 0 {
		t.Errorf("the raise = %v, want 0: 0.70 - 0.40 = 0.30 covers 0.30", got)
	}
	if got := fundFor(0.3, policy.SessionHeadroom(0.7, 0.41)); got != 0.01 {
		t.Errorf("the raise = %v, want 0.01", got)
	}
}

// A room above the need needs no raise either: 1.00 - 0.40 = 0.60 covers
// 0.30, and a raise of -0.30 is none.
func TestTheRaiseIsZeroWhenTheRoomExceedsTheNeed(t *testing.T) {
	if got := fundFor(0.3, policy.SessionHeadroom(1, 0.4)); got != 0 {
		t.Errorf("the raise = %v, want 0", got)
	}
}

// An escalation names its cap in its words: the ladder's wording and the
// old one a session cap, the time cap's an agent time cap, and a plain ask
// none.
func TestQuestionCapIsTheEscalationsWords(t *testing.T) {
	for _, tc := range []struct {
		name, reason, want string
	}{
		{"the ladder's wording", CapReachedLadder + "\nlast: x", CapSession},
		{"the old wording", CapReached + "\nlast: x", CapSession},
		{"the time cap", TimeCapReached + "\nlast: x", CapTime},
		{"a plain ask", "a plain ask", ""},
	} {
		if got := QuestionCap(tc.reason); got != tc.want {
			t.Errorf("%s: QuestionCap = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A routine's why line is its cards' why: the human card it files and the
// agent card's brief. A routine whose file holds none says that it files
// the card.
func TestARoutinesWhyLeadsItsCards(t *testing.T) {
	rt := policy.Routine{Name: "bump", Title: "Bump the pins", Why: "Pinned inputs miss their fixes until a bump."}
	if q := RoutineQuestion(rt, "brief", "prov", true); q.Why != rt.Why {
		t.Errorf("the human card's why = %q, want %q", q.Why, rt.Why)
	}
	if b := RoutineCard(rt, "brief"); b.Why != rt.Why {
		t.Errorf("the agent card's why = %q, want %q", b.Why, rt.Why)
	}
	rt.Why = ""
	if b := RoutineCard(rt, "brief"); b.Why != "The routine bump files this card on its schedule." {
		t.Errorf("the why of a routine with none = %q", b.Why)
	}
}

// The month cap's card after a reset states the whole month, then the
// count since the reset and the press, which its story names too. Hand-
// derived at 20 November 15:00 UTC with a cap of 800: the count of 800
// since sebastian's 14:02 reset, 1700 in all.
func TestMonthCapCardNamesTheReset(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	reset := &policy.CapReset{ID: 3, Cap: policy.CapMonth, At: time.Date(2026, 11, 20, 14, 2, 0, 0, time.UTC), Login: "sebastian", BeforeEUR: 900}
	q := MonthCapCardQuestion(800, MonthSpend{Month: "2026-11", EUR: 800, AllEUR: 1700, Reset: reset}, "https://git/policy#L7", now)
	if want := "The factory's variable spend in 2026-11 (UTC, over the cost ledger: model calls, paid tools, commissioned servers) is 1700.00 EUR; " +
		"counted since 14:02 UTC, when sebastian reset the month's count: 800.00 EUR of the cap of 800.00 EUR (`budget.month_eur`). "; !strings.HasPrefix(q.Info, want) {
		t.Errorf("info = %q, want it to open %q", q.Info, want)
	}
	if want := "The factory spent 800.00 EUR of its 800.00 EUR limit since sebastian reset the month's count, 1700.00 EUR in November."; q.Happened != want {
		t.Errorf("happened = %q, want %q", q.Happened, want)
	}
}

// LiteLLM's outer guard after a reset: the guard counts the whole month,
// which the reset does not move, so the card names that cause and the
// guard's file, not a counter that ran ahead. Hand-derived with a cap of
// 800: 300 counted since anna's reset of 18 November 09:00, 1500 in all.
func TestOuterGuardCardNamesTheReset(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	reset := &policy.CapReset{ID: 3, Cap: policy.CapMonth, At: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), Login: "anna", BeforeEUR: 1200}
	m := MonthSpend{Month: "2026-11", EUR: 300, AllEUR: 1500, Refusal: gateway.ProxyBudgetRefusal + "Budget has been exceeded!", Reset: reset}
	q := MonthCapCardQuestion(800, m, "https://git/helmrelease.yaml", now)
	if want := "LiteLLM's outer guard refused model calls at 300.00 EUR since a reset, below the month cap of 800.00 EUR"; q.Ask != want {
		t.Errorf("ask = %q, want %q", q.Ask, want)
	}
	for _, want := range []string{
		"The factory's variable spend this month (2026-11, UTC) is 1500.00 EUR; counted since 18 Nov 09:00 UTC, when anna reset the month's count: 300.00 EUR, " +
			"below the cap of 800.00 EUR (`budget.month_eur`). Yet LiteLLM's proxy-wide `max_budget`, the outer guard behind the cap, refused a call: " +
			"it counts the whole month, and a reset of the count does not move it.",
		"Raise `max_budget` in k8s/platform/litellm/helmrelease.yaml, the LiteLLM helmrelease, which stays at 1.25 times the cap or more: https://git/helmrelease.yaml",
	} {
		if !strings.Contains(q.Info, want) {
			t.Errorf("info lacks %q:\n%s", want, q.Info)
		}
	}
	if strings.Contains(q.Info, "ran ahead") {
		t.Errorf("info blames LiteLLM's counter after a reset:\n%s", q.Info)
	}
	if want := "LiteLLM refused a model call at 1500.00 EUR of spend this month, 300.00 EUR of it since anna reset the month's count."; q.Happened != want {
		t.Errorf("happened = %q, want %q", q.Happened, want)
	}
	if refusals := CheckBrief(BriefText{Title: q.Ask, Why: q.Why}, BriefAsk, nil); len(refusals) > 0 {
		t.Errorf("the door refuses the ask %q: %q", q.Ask, refusals)
	}
}
