package factoryd

import (
	"reflect"
	"strings"
	"testing"
	"time"

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

// Each human card's question asks one line, fills its why and info, and
// offers exactly its answers in order, in the vocabulary's words.
func TestHumanCardQuestions(t *testing.T) {
	did, notNow, factory, noted := QuestionChoice{AnswerClose, "I did it"}, QuestionChoice{AnswerPark, "Not now"},
		QuestionChoice{AnswerGo, "Have the factory do it"}, QuestionChoice{AnswerClose, "Noted"}
	alert := SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full", "service": "db"}, StartsAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	for name, tt := range map[string]struct {
		q       HumanQuestion
		ask     string
		choices []QuestionChoice
	}{
		"alert": {AlertQuestion(alert, "shop", "prod", "the project runs no agents", "body"),
			"Look into the alert disk full on db", []QuestionChoice{noted, notNow}},
		"flux alert": {AlertQuestion(SigNozAlert{Fingerprint: "rollout/x", Labels: map[string]string{"alertname": "stuck"}, StartsAt: alert.StartsAt}, "shop", "prod", "w", "b"),
			"Look into the alert stuck on prod", []QuestionChoice{noted, notNow}},
		"event alert": {AlertQuestion(SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "Job failed", "event": "true"}, StartsAt: alert.StartsAt}, "infra", "forgejo", "w", "b"),
			"Look into the alert Job failed on forgejo", []QuestionChoice{noted, notNow}},
		"roll refused": {RollRefusedQuestion("abc1234", []string{"gateway", "worker"}, []string{"gateway: x", "worker: y"}, "https://git/run/1"),
			"Fix the signing of the images gateway, worker, then close this card", []QuestionChoice{did, notNow}},
		"rerun": {RerunQuestion(1208, "abc1234def5678", "ci / test", []string{"ci / test: lost"}, "factoryd re-ran it once."),
			"Re-run the check ci / test on main after a lost runner", []QuestionChoice{did, notNow}},
		"config": {ConfigRejectedQuestion("factory", "c9c9c9c9c9", "https://git/tree", "Version 1 stays current.", "bad key"),
			"Fix the rejected configuration in factory/ and push", []QuestionChoice{did, notNow}},
		"month cap": {MonthCapCardQuestion(50, "2026-09", 49.98, "https://git/policy#L7", "refused"),
			"The factory spent its monthly limit of 50.00 EUR. Raise it in policy.yaml, or wait for the month to end?", []QuestionChoice{notNow, noted}},
		"routine": {RoutineQuestion(policy.Routine{Name: "bump", Title: "Bump the pins"}, "brief", "prov", true),
			"Bump the pins", []QuestionChoice{did, factory, notNow}},
		"issue": {IssueQuestion("Two\nlines  here", "text", "sebastian", true),
			"Two lines here", []QuestionChoice{did, factory, notNow}},
		"routine without agents": {RoutineQuestion(policy.Routine{Name: "bump", Title: "Bump the pins"}, "brief", "prov", false),
			"Bump the pins", []QuestionChoice{did, notNow}},
		"issue without agents": {IssueQuestion("Two\nlines  here", "text", "sebastian", false),
			"Two lines here", []QuestionChoice{did, notNow}},
		"failed roll": {RollFailedQuestion([]RollFailure{{RunID: 7, Title: "Roll the images", Issue: 41}}), "",
			[]QuestionChoice{{AnswerRetryRoll, "Try the roll again"}, notNow, {AnswerClose, "End the card"}}},
		"chain stall": {StallQuestion("sf/code", "abc1234def", time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), []string{"ci / test"}, nil), "",
			[]QuestionChoice{{AnswerRerunAgain, "Run the chain again"}, {AnswerCheckAgain, "Look again"}, {AnswerDismiss, "End the card"}}},
		"production held": {HoldQuestion("code", time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
			[]Group{{Plan: "t-plan", Issue: 12, Repository: "sf/code"}}, nil, nil), "",
			[]QuestionChoice{{AnswerHoldWait, "Not now"}, {ActDeployNow, "Deploy now"},
				{AnswerReplanOnTop + ":t-plan", "Replan plan #12 on top"}, {AnswerTakeOffMain + ":t-plan", "Take plan #12 off main and replan"}}},
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
		})
	}
	const lostWhy = "Main CI's push run 1208 of abc1234def5678 failed, but none of its steps did: each failed job lost its runner while it ran " +
		"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card. factoryd re-ran it once."
	if info := RerunQuestion(1208, "abc1234def5678", "ci / test", nil, "factoryd re-ran it once.").Info; !strings.HasPrefix(info, lostWhy+"\n\n") {
		t.Errorf("rerun info = %q, want it to open with %q", info, lostWhy)
	}
}

// An event's card names the failed Job its labels carry, and the CronJob
// whose next run closes the card, and only a name Kubernetes could have
// given: anything else leaves the Job out.
func TestEventAlertCardNamesTheJob(t *testing.T) {
	const note = "w\n\nThe alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card stays open until a human closes it.\n\nbody"
	a := SigNozAlert{Labels: map[string]string{"alertname": "Job failed", "event": "true", "k8s.object.name": "forgejo-dump-29885910"}}
	const cronNote = "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card closes when the CronJob `forgejo-dump` completes a run, or when a human closes it.\n\nbody"
	want := "w\n\nThe Job `forgejo-dump-29885910` in namespace `forgejo` failed: read its pods' logs " +
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
