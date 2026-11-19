package factoryd_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Every monitor flag asks in words: a sentence about the card, with no
// flag's name, no parenthesis and no underscore. A notice states its fact
// and offers "Noted" and "Not now"; an ask offers the answer that fits it
// where that can happen, then "It is fine, go on" and "Not now". A wait
// says it lasted more than its limit, which stays true on a stored face.
func TestEveryFlagAsksInWords(t *testing.T) {
	noticed := []string{"Noted", "Not now"}
	fine := []string{"It is fine, go on", "Not now"}
	replan := []string{"Send back to the planner", "It is fine, go on", "Not now"}
	rescue := []string{"Rescue the work into a new card", "It is fine, go on", "Not now"}
	want := map[string]struct {
		ask     string
		answers []string
	}{
		"answer_changed_nothing":  {"This card's last answer changed nothing.", noticed},
		"budget_moved_no_event":   {"This card's budget reads 2.70 EUR, but its records add up to 2.00 EUR. Is that fine?", fine},
		"checkin_cut":             {"This card's last session went unjudged because its check ended early.", noticed},
		"faced_unjudged":          {"This card's question reached a human before the factory's agents judged it.", noticed},
		"checkin_refused":         {"This card's last session went unjudged because its check was refused.", noticed},
		"criterion_unproven":      {"A promise of the plan has no test result yet. Run the plan's tests now?", []string{"Run the tests now", "It is fine, go on", "Not now"}},
		"dependency_failed":       {"A card this one waits for failed. Is that fine?", fine},
		"finding_confirmed_twice": {"The review found the same fault twice on this card. Rescue the work into a new card?", rescue},
		"grant_bought_nothing":    {"This card's last session, which a human or the project agent allowed, changed nothing.", noticed},
		"issue_open_after_end":    {"This card ended, and its issue is still open. Close the issue?", []string{"Close the issue", "Leave the issue open", "Not now"}},
		"opened_proof_changed":    {"A test of the plan changed after the plan was confirmed. Send the plan back to the planner?", replan},
		"opened_proof_red_twice":  {"A test of the plan failed on two versions of this card's work. Send the plan back to the planner?", replan},
		"proofs_missing":          {"The plan's promises have no tests yet. Send the plan back to the planner?", replan},
		"prover_refused_thrice":   {"No test could be written for one of the plan's promises. Send the plan back to the planner?", replan},
		"queued_over_bound":       {"This card has waited more than 2 hours for a free place to run. Is that fine?", fine},
		"review_refused_not_run":  {"This card cannot merge yet because the review did not run on its newest work.", noticed},
		"session_no_push":         {"This card's last sessions changed no code.", noticed},
		"wait_over_bound":         {"This card's run has not moved or answered for more than 30 minutes. Rescue the work into a new card?", rescue},
	}
	faces := map[string]factoryd.AskFace{}
	for _, x := range facedFixtures(t) {
		if x.fx.Group == facefixtures.GroupFlag {
			faces[x.fx.Kind] = x.face
		}
	}
	for _, flag := range factoryd.MonitorFlags() {
		face, ok := faces[flag]
		if !ok {
			t.Errorf("flag %s has no fixture", flag)
			continue
		}
		if strings.Contains(face.Ask, flag) || strings.ContainsAny(face.Ask, "()_") {
			t.Errorf("%s: the ask %q names the flag, or holds a parenthesis or an underscore", flag, face.Ask)
		}
		if len(face.Answers) < 2 {
			t.Errorf("%s: %d answers %v, want at least 2", flag, len(face.Answers), labels(face.Answers))
		}
		w := want[flag]
		if face.Ask != w.ask {
			t.Errorf("%s: ask %q, want %q", flag, face.Ask, w.ask)
		}
		if got := labels(face.Answers); !slices.Equal(got, w.answers) {
			t.Errorf("%s: answers %v, want %v", flag, got, w.answers)
		}
	}
}

// An ended card's open issue asks about the issue, not the work: its lines
// say the card ended and what the open issue means, its story offers no
// session, and each of its answers and other actions can happen on the
// ended card, with no second settle beside "Leave the issue open".
func TestAnEndedCardsOpenIssueCanBeAnswered(t *testing.T) {
	for _, x := range facedFixtures(t) {
		if x.fx.Group != facefixtures.GroupFlag || x.fx.Kind != "issue_open_after_end" {
			continue
		}
		if !factoryd.Final(x.fx.Task.Status) {
			t.Fatalf("the fixture's card is %s, want it ended", x.fx.Task.Status)
		}
		f := x.face
		if f.Ask != "This card ended, and its issue is still open. Close the issue?" ||
			f.Why != "An open issue says the work is still to do, though no card works on it any more." ||
			f.Story != (factoryd.Story{Happened: "It spent 0.96 of 2.00 EUR in 1 session."}) {
			t.Errorf("the face asks %q, why %q, story %+v", f.Ask, f.Why, f.Story)
		}
		var more []string
		for _, a := range f.More {
			more = append(more, a.Label)
		}
		if slices.Contains(more, "Send back to the planner") || slices.Contains(more, "It is fine, go on") {
			t.Errorf("Other actions %v offer a replan Act refuses on an ended card, or a second settle", more)
		}
		for _, a := range slices.Concat(f.Answers, f.Over, f.More) {
			if why := factoryd.OfferRefusal(x.facts, a.Action); why != "" {
				t.Errorf("%q is refused on the ended card: %s", a.Label, why)
			}
			res, err := a.Press(x.facts, written(a), "")
			if err != nil {
				t.Errorf("a press on %q: %v", a.Label, err)
				continue
			}
			if why := factoryd.PressRefusal(x.facts, res); why != "" {
				t.Errorf("a press on %q is refused on the ended card: %s", a.Label, why)
			}
		}
		return
	}
	t.Fatal("no fixture of issue_open_after_end")
}

// Every answer the face of an ended card's case offers, among its answers
// or under Other actions, is one Act takes on an ended card, and its press
// with its defaults is refused by no rule: the face and Act read one rule.
func TestNoAnswerOnAnEndedCardIsRefused(t *testing.T) {
	seen := 0
	for _, x := range facedFixtures(t) {
		if x.fx.Case == nil || !factoryd.Final(x.fx.Task.Status) {
			continue
		}
		seen++
		name := x.fx.Group + "/" + x.fx.Kind
		for _, a := range slices.Concat(x.face.Answers, x.face.Over, x.face.More) {
			if _, table := factoryd.ActionByName(a.Action); !table || a.Value != "" {
				continue
			}
			if why := factoryd.EndedRefusal(x.fx.Task, *x.fx.Case, a.Action); why != "" {
				t.Errorf("%s: %q is on the face, and Act refuses it: %s", name, a.Label, why)
			}
			res, err := a.Press(x.facts, written(a), "")
			if err != nil {
				t.Errorf("%s: a press on %q: %v", name, a.Label, err)
				continue
			}
			if why := factoryd.PressRefusal(x.facts, res); why != "" {
				t.Errorf("%s: a press on %q with its defaults is refused: %s", name, a.Label, why)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no fixture holds a case on an ended card")
	}
}

// A helper's ask stands as a page of its parent, so its face names the
// step where it would name the card: the sizing's session, "End the
// sizing" with what its end leaves the card, the step's wait, and the
// session an extra answer starts. Its
// inbox message names the parent, the card the page stands on, and the
// step that asks.
func TestAHelpersPageNamesItsStep(t *testing.T) {
	want := map[string]struct{ ask, end, effect string }{
		factoryd.SourceScreening: {"No machine could start the screening's session. Try again, or end the screening?", "End the screening",
			"Ends the screening. You then decide whether an agent may work on this card."},
		factoryd.SourceSizing: {"No machine could start the sizing's session. Try again, or end the sizing?", "End the sizing",
			"Ends the sizing. This card goes on at the size it was filed with."},
		factoryd.SourceProver: {"No machine could start the proof writing's session. Try again, or end the proof writing?", "End the proof writing",
			"Ends the proof writing. The plan goes on without its tests."},
	}
	seen := 0
	for _, x := range facedFixtures(t) {
		w, ok := want[x.fx.Task.Source]
		if !ok || x.fx.Group != facefixtures.GroupSource || x.fx.Case == nil {
			continue
		}
		seen++
		step := strings.ToLower(strings.TrimPrefix(w.end, "End "))
		if x.face.Ask != w.ask {
			t.Errorf("%s: ask %q, want %q", x.fx.Kind, x.face.Ask, w.ask)
		}
		wantAnswers := []string{"Run one more session", w.end, "Not now"}
		if got := labels(x.face.Answers); !slices.Equal(got, wantAnswers) {
			t.Errorf("%s: answers %v, want %v", x.fx.Kind, got, wantAnswers)
			continue
		}
		effects := []string{"Starts the next session on " + step + "'s model.", w.effect, strings.ToUpper(step[:1]) + step[1:] + " keeps waiting for your answer."}
		for i, a := range x.face.Answers {
			if a.Effect != effects[i] {
				t.Errorf("%s: %q's effect %q, want %q", x.fx.Kind, a.Label, a.Effect, effects[i])
			}
		}
		// a session an extra answer starts is the step's
		if i := slices.IndexFunc(x.face.More, func(a factoryd.Answer) bool { return a.Action == factoryd.ActContinue }); i < 0 ||
			x.face.More[i].Effect != "Starts a session of "+step+" that reads your note first." {
			t.Errorf("%s: the extra answers %v, want “Go on with my note” to start a session of %s", x.fx.Kind, x.face.More, step)
		}
		parent := x.fx.Parent
		m, ok := factoryd.PokeOf(factoryd.Event{Kind: factoryd.EventQuestionAsked, TaskID: x.fx.Task.ID, IssueNumber: parent.IssueNumber, Detail: x.fx.Ask.Detail},
			x.fx.Task, *parent)
		head := factoryd.CardName(*parent, 0) + " needs you. " + strings.ToUpper(step[:1]) + step[1:] + " asks."
		if !ok || !strings.HasPrefix(m.Content, head) {
			t.Errorf("%s: the inbox says %q, want it to start %q", x.fx.Kind, m.Content, head)
		}
	}
	if seen != len(want) {
		t.Errorf("%d helper asks among the fixtures, want %d", seen, len(want))
	}
}
