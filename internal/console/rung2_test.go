package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// failingCosts is a Costs whose enforced spend cannot be read.
type failingCosts struct{ *fakeLedger }

func (failingCosts) Enforced(context.Context, time.Time) (policy.Enforced, error) {
	return policy.Enforced{}, errors.New("the ledger is down")
}

// wakeTurnOf is a wake turn of birth on a tree whose premium cap is cap EUR
// and whose chat wakes on wake_model small.
func wakeTurnOf(r *rig, birth string, cap float64, premiumLimit bool) *turn {
	doc := policy.Document{Budget: policy.Budget{PremiumMonthEUR: cap}}
	if premiumLimit {
		doc.Prices.MaxOutputTokens = map[string]int{policy.Premium: 8192}
	}
	return &turn{c: r.c, r: httptest.NewRequest(http.MethodGet, "/", nil), tree: policy.Tree{Document: doc},
		cfg: policy.ChatConfig{Model: policy.Big, WakeModel: policy.Small},
		a:   actor{wake: true, birth: birth}}
}

// A wake that names a case runs on premium while the month's premium spend
// is under 85 percent of premium's cap, and on big from then. By hand: 85
// percent of 50.00 EUR is 42.50, so 42.49 is premium and 42.50, the
// threshold, is big. A spend that cannot be read, and a premium with no
// output limit, run on big. A watch's wake keeps wake_model, and a cap of 0
// (premium has none of its own) leaves premium always.
func TestACaseWakeRunsOnPremiumUntilItsShareIsSpent(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		name, birth string
		spent, cap  float64
		limit       bool
		want        string
	}{
		{"under the share", "wake_ask", 42.49, 50, true, policy.Premium},
		{"at the share", "wake_ask", 42.50, 50, true, policy.Big},
		{"over the share", "case_climbed", 49, 50, true, policy.Big},
		{"a climbed case under the share", "case_climbed", 0, 50, true, policy.Premium},
		{"a watch's wake", "wake_card", 0, 50, true, policy.Small},
		{"a timer's wake", "wake_timer", 49, 50, true, policy.Small},
		{"the refill", "queue_refill", 0, 50, true, policy.Small},
		{"no cap of its own", "wake_ask", 1000, 0, true, policy.Premium},
		{"no output limit", "wake_ask", 0, 50, false, policy.Big},
	} {
		r.ledger.premium = tc.spent
		if got := wakeTurnOf(r, tc.birth, tc.cap, tc.limit).model(); got != tc.want {
			t.Errorf("%s: the wake runs on %s, want %s", tc.name, got, tc.want)
		}
	}
	r.c.cfg.Costs = failingCosts{r.ledger}
	if got := wakeTurnOf(r, "wake_ask", 50, true).model(); got != policy.Big {
		t.Errorf("a spend that cannot be read: %s, want big", got)
	}
	r.c.cfg.Costs = nil
	if got := wakeTurnOf(r, "wake_ask", 50, true).model(); got != policy.Big {
		t.Errorf("no ledger: %s, want big", got)
	}
	member := wakeTurnOf(r, "wake_ask", 50, true)
	member.a.wake = false
	if got := member.model(); got != policy.Big {
		t.Errorf("a member's turn: %s, want its own model", got)
	}
}

// The analysis of a case rung 2 holds hands it up: the seam gets the card,
// the case, the found text, the typed recommendation with its grounds and the
// turn's reads, and the tool answers that the card waits at its face. A
// refusal comes back as a 409 with its words; a case rung 2 does not hold
// records the analysis in the ask as before.
func TestAttachAnalysisHandsUpACaseRungTwoHolds(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.noteRead("card_facts", `{"task_id":"13"}`, 42)
	tool, _ := r.c.chatTool("attach_analysis")

	type handed struct {
		task   string
		caseID int64
		found  string
		rec    factoryd.RungTwoRecommendation
		reads  []factoryd.CaseRead
	}
	var got []handed
	own := true
	var handErr error
	r.c.cfg.HandUpRung2 = func(_ context.Context, task string, caseID int64, found string, rec factoryd.RungTwoRecommendation, reads []factoryd.CaseRead) (bool, error) {
		got = append(got, handed{task, caseID, found, rec, reads})
		return own, handErr
	}
	var attached int
	r.c.cfg.AttachAnalysis = func(context.Context, string, string, string, factoryd.Event) error { attached++; return nil }

	in := toolInput{TaskID: "13", CaseID: 7, Found: "The pull request merged.", Recommendation: "nothing is left",
		Recommended: &recommended{Action: "close", Args: map[string]any{"why": "done"}}}
	if err := trn.byTaskID(&in); err != nil {
		t.Fatal(err)
	}
	note, _, err := tool.run(trn, in)
	want := handed{"t-e", 7, "The pull request merged.", factoryd.RungTwoRecommendation{Action: "close", Grounds: "nothing is left", Args: map[string]any{"why": "done"}},
		[]factoryd.CaseRead{{Tool: "card_facts", Args: `{"task_id":"13"}`, Bytes: 42}}}
	if err != nil || note != "Handed #13 to the human with your analysis and recommendation; the card waits at its face." || !reflect.DeepEqual(got, []handed{want}) || attached != 0 {
		t.Fatalf("note %q, err %v, handed %+v, attached %d; want the hand-up %+v and no ask analysis", note, err, got, attached, want)
	}

	handErr = factoryd.ErrActRefused(`"withdraw" is no action the human may take: recommend close`)
	_, _, err = tool.run(trn, in)
	if r := refusedWith(err); r == nil || r.status != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 refusal", err)
	}

	own, handErr = false, nil
	note, _, err = tool.run(trn, toolInput{TaskID: "t-e", Found: "found"})
	if err != nil || attached != 1 || note != "Recorded the analysis inside the ask of #13; the humans read it with the ask." {
		t.Fatalf("note %q, err %v, attached %d; want the old behaviour on a case no rung 2 holds", note, err, attached)
	}
}

func refusedWith(err error) *refusal {
	var no *refusal
	if errors.As(err, &no) {
		return no
	}
	return nil
}

// A rung 2 answer records the turn's reads on its step: Act gets them with
// the answer. A human's answer carries none.
func TestRungTwoAnswerCarriesTheTurnsReads(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	_, s := r.loggedIn(t)
	task, _, _ := r.c.cfg.Tasks.Task(t.Context(), "t-e")
	reads := []factoryd.CaseRead{{Tool: "card_facts", Bytes: 10}}
	a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", reads: func() []factoryd.CaseRead { return reads }}
	if _, err := r.c.callAct(t.Context(), a, task, 5, factoryd.Resolution{Action: factoryd.ActNote, Note: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	a.decider = "member"
	if _, err := r.c.callAct(t.Context(), a, task, 5, factoryd.Resolution{Action: factoryd.ActNote, Note: "y"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.acted) != 2 || r.acted[0].Rung != 2 || !reflect.DeepEqual(r.acted[0].Read, reads) || r.acted[1].Rung != 3 || r.acted[1].Read != nil {
		t.Fatalf("Act calls = %+v, want the agent's with the reads on rung 2 and the human's with none", r.acted)
	}
}
