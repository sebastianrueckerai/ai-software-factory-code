package factoryd

import (
	"context"
	"strings"
	"testing"
)

// gatePlanCard seeds a criteria plan whose integration card t-int is open
// and holds PR 12 at head c7, and returns the store, the card and the
// plan as filedPlan hands it over. settled gives every criterion a met row
// of that source.
func gatePlanCard(t *testing.T, settled string) (*fakeStore, Task, Plan) {
	t.Helper()
	s := seedVerdictPlan(t, "t-plan", "t-int", "open")
	if settled != "" {
		if err := s.PutCriteriaVerdicts(context.Background(), "t-int", settled, FactoryDecider, []CriterionVerdict{
			{Criterion: "c1", Verdict: "met", Kind: CriterionTest},
			{Criterion: "c2", Verdict: "met", Kind: CriterionHuman},
			{Criterion: "c3", Verdict: "met", Kind: CriterionInvariant},
		}); err != nil {
			t.Fatal(err)
		}
	}
	card := s.tasks["t-int"]
	card.PRNumber, card.SHA = 12, "c7"
	s.tasks["t-int"] = card
	return s, card, criteriaPlan()
}

func TestGateTriggerTable(t *testing.T) {
	const openPR = `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t-int","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	const mergedPR = `{"number":12,"state":"closed","merged":true,"html_url":"u12","head":{"ref":"factory/t-int","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	cases := []struct {
		name      string
		pr        int64
		pull      string
		pullsDown bool
		settled   string
		planKey   string
		notACard  bool
		want      bool
		wantErr   bool
	}{
		{"open PR on the integration card with unsettled criteria", 12, openPR, false, "", "", false, true, false},
		{"no PR", 0, "", false, "", "", false, false, false},
		{"merged PR", 12, mergedPR, false, "", "", false, false, false},
		{"plain feature card", 12, openPR, false, "", "foundation", false, false, false},
		{"criteria settled by the approver", 12, openPR, false, CriteriaSourceApprover, "", false, false, false},
		{"criteria met only at the open head", 12, openPR, false, CriteriaSourceGate, "", false, true, false},
		{"delta card with open PR", 12, openPR, false, "", "delta-2", false, true, false},
		{"card off any plan", 12, openPR, false, "", "", true, false, false},
		{"failed forge read on a card that holds a PR", 12, openPR, true, "", "", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, card, p := gatePlanCard(t, tc.settled)
			if tc.planKey != "" {
				card.PlanKey = tc.planKey
			}
			if tc.notACard {
				card.Source = SourceChat
				card.Parent = ""
			}
			if tc.pr == 0 {
				card.PRNumber = 0
			}
			fj := &prForgejo{pulls: map[int64]string{12: tc.pull}, pullsDown: tc.pullsDown}
			r := &Runner{store: s, forgejo: &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
			got, err := r.gateTrigger(context.Background(), card, p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("gateTrigger = %v, nil; want the forge read's error, never a false", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("gateTrigger: %v", err)
			}
			if got != tc.want {
				t.Fatalf("gateTrigger = %v, want %v", got, tc.want)
			}
		})
	}
}

// A failed forge read preserves the fail direction at the ask too: the
// session-moment PR facts builder errors on a card that holds a pull
// request, and answers green facts for an open one.
func TestCheckinPRFactsReadTheForge(t *testing.T) {
	const openPR = `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t-int","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	s, card, _ := gatePlanCard(t, "")
	forge := &prForgejo{
		pulls: map[int64]string{12: openPR},
		statusesBySHA: map[string][]string{
			"c7": {statusBody("success", [4]string{"success", "pr / test", "", ""})},
		},
	}
	r := &Runner{store: s, forgejo: &Forgejo{BaseURL: forge.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	f, err := r.checkinPRFacts(context.Background(), card)
	if err != nil {
		t.Fatalf("checkinPRFacts: %v", err)
	}
	if f == nil || f.Number != 12 || f.Checks != ChecksSuccess || !f.GateTrigger {
		t.Fatalf("facts = %+v, want PR 12 green and the gate's trigger", f)
	}
	if f, err = r.checkinPRFacts(context.Background(), Task{ID: "plain", Repository: card.Repository}); err != nil || f != nil {
		t.Fatalf("facts for a card off a plan = %+v, %v; want nil, nil", f, err)
	}
	down := &prForgejo{pulls: map[int64]string{12: openPR}, pullsDown: true}
	r2 := &Runner{store: s, forgejo: &Forgejo{BaseURL: down.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	if _, err := r2.checkinPRFacts(context.Background(), card); err == nil {
		t.Fatal("checkinPRFacts with the forge down = nil error, want the error, never a missing PR")
	}
}

// gateTriggerAsk builds the session-moment ask on the seeded card: the PR
// facts come from checkinPRFacts over the fake forge with checks green,
// the read the merge press does — never a hand-built CheckInPRFacts.
func gateTriggerAsk(t *testing.T, s *fakeStore, card Task) *CheckInAsk {
	t.Helper()
	const openPR = `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t-int","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	forge := &prForgejo{
		pulls: map[int64]string{12: openPR},
		statusesBySHA: map[string][]string{
			"c7": {statusBody("success", [4]string{"success", "pr / test", "", ""})},
		},
	}
	r := &Runner{store: s, forgejo: &Forgejo{BaseURL: forge.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	f, err := r.checkinPRFacts(context.Background(), card)
	if err != nil {
		t.Fatalf("checkinPRFacts: %v", err)
	}
	return &CheckInAsk{Moment: CheckInSession, ParentSession: "s-1", Trip: &TripReport{Detector: TripSize, Detail: "over the bound"}, PR: f}
}

// TestCheckinStatementNamesGateTriggerPR: the session check-in's statement
// for a card whose open PR is the gate's trigger names the PR's number, its
// checks state, the trigger and the gate consequence, and what a close
// without it leaves to the gate on main; a plain feature card's PR is
// named without them, and the no-PR ask names none of it.
func TestCheckinStatementNamesGateTriggerPR(t *testing.T) {
	t.Run("open PR on the integration card", func(t *testing.T) {
		s, card, _ := gatePlanCard(t, "")
		got := checkinStatement(gateTriggerAsk(t, s, card), nil, 3, 2.5, 1000)
		for _, want := range []string{"#12", "green", "criteria-gate trigger", "merge press", "its merge press runs the plan's criteria gate on its head",
			"Closing this card without its pull request leaves the plan's criteria to the gate's run on main once every card of the plan has ended, without this pull request's work"} {
			if !strings.Contains(got, want) {
				t.Errorf("statement lacks %q:\n%s", want, got)
			}
		}
	})
	t.Run("open PR on a plain feature card", func(t *testing.T) {
		s, card, _ := gatePlanCard(t, "")
		card.PlanKey = "foundation"
		got := checkinStatement(gateTriggerAsk(t, s, card), nil, 3, 2.5, 1000)
		for _, want := range []string{"#12", "green", "not the plan's criteria-gate trigger"} {
			if !strings.Contains(got, want) {
				t.Errorf("statement lacks %q:\n%s", want, got)
			}
		}
		for _, gone := range []string{"merge press", "Closing this card", "the gate's run on main"} {
			if strings.Contains(got, gone) {
				t.Errorf("non-trigger statement names %q:\n%s", gone, got)
			}
		}
	})
	t.Run("no PR", func(t *testing.T) {
		s, card, _ := gatePlanCard(t, "")
		card.PRNumber, card.SHA = 0, ""
		got := checkinStatement(gateTriggerAsk(t, s, card), nil, 3, 2.5, 1000)
		for _, gone := range []string{"#12", "criteria-gate trigger", "merge press", "changeless round"} {
			if strings.Contains(got, gone) {
				t.Errorf("no-PR statement names %q:\n%s", gone, got)
			}
		}
	})
}
