package factoryd_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

func TestEveryKindHasAFace(t *testing.T) {
	complete := func(name string, s factoryd.FaceSpec) {
		if s.Ask == nil || s.Why == nil || s.Answers == nil {
			t.Errorf("%s: the face lacks its ask, its why line or its answers", name)
		}
	}
	for _, k := range factoryd.CaseKinds() {
		s, ok := factoryd.CaseFaces[k]
		if !ok {
			t.Errorf("case kind %s has no face", k)
			continue
		}
		complete(string(k), s)
	}
	for _, k := range factoryd.ApprovalKinds() {
		s, ok := factoryd.ApprovalFaces[k]
		if !ok {
			t.Errorf("approval kind %s has no face", k)
			continue
		}
		complete(k, s)
	}
	for _, flag := range factoryd.MonitorFlags() {
		s, ok := factoryd.FlagFaces[flag]
		if !ok {
			t.Errorf("monitor flag %s has no face", flag)
			continue
		}
		complete(flag, s)
	}
}

func TestEveryActionHasOneLabel(t *testing.T) {
	for _, name := range factoryd.HumanActions() {
		var rows []factoryd.Answer
		for _, a := range factoryd.Vocabulary {
			if a.Action == name && a.Variant == "" {
				rows = append(rows, a)
			}
		}
		if len(rows) != 1 {
			t.Errorf("action %s has %d rows without a variant in the vocabulary, want 1", name, len(rows))
		}
	}
	keys := map[string]bool{}
	for _, a := range factoryd.Vocabulary {
		if keys[a.Key()] {
			t.Errorf("%s has two rows", a.Key())
		}
		keys[a.Key()] = true
		if a.Label == "" || a.Effect == "" {
			t.Errorf("%s: label %q, effect %q: each row has both", a.Key(), a.Label, a.Effect)
		}
		for _, text := range []string{a.Label, a.Effect} {
			if faults := writingguide.Lint(text); len(faults) > 0 {
				t.Errorf("%s: %q: %v", a.Key(), text, faults)
			}
		}
		for _, f := range a.Fields {
			switch {
			case f.Label == "":
				t.Errorf("%s: field %s has no label", a.Key(), f.Name)
			case f.Required && f.Hint == "":
				t.Errorf("%s: the required field %s says nothing of what to write", a.Key(), f.Name)
			case !f.Required && f.Default == nil:
				t.Errorf("%s: field %s has no default", a.Key(), f.Name)
			case f.Required && f.Default != nil:
				t.Errorf("%s: the required field %s has a default", a.Key(), f.Name)
			}
		}
	}
}

// anyRefs resolves every reference: the fixtures name cards that exist.
type anyRefs struct{}

func (anyRefs) Issue(int) (string, string, bool) { return "a card", "open", true }
func (anyRefs) Files(string) []string            { return []string{"x"} }
func (anyRefs) Heading(string, string) bool      { return true }
func (anyRefs) Err() error                       { return nil }

// faced is a fixture's face with the facts it was built on.
type faced struct {
	fx    facefixtures.Fixture
	c     factoryd.Case
	facts factoryd.Facts
	face  factoryd.AskFace
}

// facedFixtures are the faces of every fixture that asks: its case, or the
// case its pending request opens.
func facedFixtures(t *testing.T) []faced {
	t.Helper()
	var out []faced
	for _, fx := range facefixtures.All() {
		c := fx.Case
		if c == nil && fx.Approval != nil {
			rc, err := factoryd.RequestCase(*fx.Approval)
			if err != nil {
				t.Fatalf("%s/%s: %v", fx.Group, fx.Kind, err)
			}
			c = &rc
		}
		if c == nil {
			continue
		}
		f := factoryd.Facts{CaseFacts: factoryd.FactsOf(*c), Card: fx.Task}
		if fx.Case != nil {
			f.CaseFacts = fx.Facts
		}
		if fx.Ask != nil {
			f.Recommend = factoryd.RecommendationOf(fx.Ask.Detail)
		}
		out = append(out, faced{fx, *c, f, factoryd.FaceOf(*c, f)})
	}
	return out
}

var (
	taskID = regexp.MustCompile(`\bt-[0-9a-f]{6,}`)
	hash   = regexp.MustCompile(`\b[0-9a-f]{12,}\b`)
	branch = regexp.MustCompile(`\bcard/\d+`)
)

func TestFaceContract(t *testing.T) {
	for _, x := range facedFixtures(t) {
		name, face := x.fx.Group+"/"+x.fx.Kind, x.face
		if refusals := factoryd.CheckBrief(factoryd.BriefText{Title: face.Ask, Why: face.Why}, factoryd.BriefAsk, anyRefs{}); len(refusals) > 0 {
			t.Errorf("%s: ask %q, why %q: %v", name, face.Ask, face.Why, refusals)
		}
		if n := utf8.RuneCountInString(face.Why); n == 0 || n > 160 {
			t.Errorf("%s: the why line %q has %d runes", name, face.Why, n)
		}
		if n := len(face.Answers); n < 2 || n > 4 {
			t.Errorf("%s: %d answers %v, want 2 to 4", name, n, labels(face.Answers))
		}
		if face.Suggested != "" && !slices.ContainsFunc(face.Answers, func(a factoryd.Answer) bool { return a.Key() == face.Suggested }) {
			t.Errorf("%s: the suggested %s is not among the answers %v", name, face.Suggested, labels(face.Answers))
		}
		if face.Suggested != "" && face.SuggestedBy == "" {
			t.Errorf("%s: the suggestion %s names no one who suggests it", name, face.Suggested)
		}
		seen := map[string]bool{}
		for _, a := range face.Answers {
			if seen[a.Label] {
				t.Errorf("%s: the label %q shows twice", name, a.Label)
			}
			seen[a.Label] = true
			if a.Effect == "" {
				t.Errorf("%s: %q has no effect line", name, a.Label)
			}
		}
		texts := []string{face.Ask, face.Why, face.Story.Planned, face.Story.Happened, face.Story.Left}
		for _, s := range []string{face.Story.Planned, face.Story.Happened, face.Story.Left} {
			if n := utf8.RuneCountInString(s); n > 240 {
				t.Errorf("%s: the story line %q has %d runes", name, s, n)
			}
		}
		for _, a := range face.Answers {
			if a.Value == "" && a.Variant != "say" {
				// an agent's own words pass the escalate door's check; a
				// live capture holds words filed before the door checked them
				texts = append(texts, a.Label)
			}
			texts = append(texts, a.Effect)
		}
		for _, s := range texts {
			if faults := writingguide.Lint(s); len(faults) > 0 {
				t.Errorf("%s: %q: %v", name, s, faults)
			}
		}
		for _, s := range []string{face.Ask, face.Why} {
			for _, re := range []*regexp.Regexp{taskID, hash, branch} {
				if m := re.FindString(s); m != "" {
					t.Errorf("%s: %q names %q", name, s, m)
				}
			}
		}
	}
}

func labels(as []factoryd.Answer) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Label)
	}
	return out
}

// sample is what a human writes in a required field.
var sample = map[string]string{"note": "Because the tests show it.", "depends_on": "#2190", "title": "Fix the importer",
	"why": "So a round trip keeps the accent.", "summary": "The importer keeps the accent.", "criterion": "size_bound_escape"}

func TestAppendixAnswersApply(t *testing.T) {
	for _, x := range facedFixtures(t) {
		name := x.fx.Group + "/" + x.fx.Kind
		var stored []string
		if x.fx.Ask != nil {
			for _, o := range factoryd.AskOffers(x.fx.Ask.Detail) {
				stored = append(stored, o.Value)
			}
		}
		for _, a := range factoryd.SpecAnswers(x.c, x.facts) {
			switch _, table := factoryd.ActionByName(a.Action); {
			case a.Value != "":
				// a human card's own choice: its question must offer it
				if !slices.Contains(stored, a.Value) && a.Action != factoryd.ActClose {
					t.Errorf("%s: %q posts %s, which the card's question does not offer: %v", name, a.Label, a.Value, stored)
				}
			case !table:
				// the card's own door takes it: a plan's or a request's decision
			default:
				if why := factoryd.OfferRefusal(x.facts, a.Action); why != "" {
					t.Errorf("%s: %q is on the face, and its predicate refuses it: %s", name, a.Label, why)
				}
				res, err := a.Press(x.facts, sample, "")
				if err != nil {
					t.Errorf("%s: a press on %q: %v", name, a.Label, err)
					continue
				}
				if why := factoryd.PressRefusal(x.facts, res); why != "" {
					t.Errorf("%s: a press on %q with its defaults is refused: %s", name, a.Label, why)
				}
			}
		}
	}
}

func TestBudgetFloorWithFindingsAsksOnce(t *testing.T) {
	f := factoryd.CaseFacts{Kind: factoryd.CaseBudgetFloor, Attempts: 3, SessionCap: 5, BudgetEUR: 4, SpentEUR: 4, LeftEUR: 0, FloorEUR: 2, CeilingEUR: 18,
		Head: "7eaf7c73121dd332c52b95bb5b024a08b3757146", Checks: "red", FindingTexts: map[string]string{}}
	var texts []string
	for _, s := range []string{"a literal token in price_floor_test.go:88", "a literal token in price_floor_test.go:107", "secrets at price_floor_test.go:71",
		"secrets at price_floor_test.go:104", "secrets at price_floor_test.go:107", "loopback endpoint at price_floor_test.go:64",
		"loopback endpoint at price_floor_test.go:93", "test-only credential at pool_gateway_rig_test.go:41"} {
		id := factoryd.FindingID(s)
		f.Findings, f.ReviewFindings, f.FindingTexts[id] = append(f.Findings, id), append(f.ReviewFindings, id), s
		texts = append(texts, s)
	}
	c := factoryd.Case{Kind: factoryd.CaseBudgetFloor, Facts: map[string]any{}}
	face := factoryd.FaceOf(c, factoryd.Facts{CaseFacts: f, Card: factoryd.Task{Kind: "agent", Objective: "Prove the three money faults end to end"}})
	if want := "Add 2.00 EUR to fix the 8 open review findings, or end the card?"; face.Ask != want {
		t.Errorf("ask %q, want %q", face.Ask, want)
	}
	shown := strings.Join([]string{face.Ask, face.Why, face.Story.Planned, face.Story.Happened, face.Story.Left}, "\n")
	for _, s := range texts {
		if strings.Contains(shown, s) {
			t.Errorf("the face names the finding %q:\n%s", s, shown)
		}
	}
	if got := labels(face.Answers); !slices.Equal(got, []string{"Add money", "Start a fresh card on this work", "End the card", "Not now"}) {
		t.Errorf("answers %v", got)
	}
}

// A card that waits on a spending limit says when it goes on by itself:
// its Not now names the limit's reset, and it offers no raise to go on.
func TestACapWaitSaysWhenItGoesOnByItself(t *testing.T) {
	for kind, decision := range map[factoryd.CaseKind]string{factoryd.CaseMonthCap: policy.RefusedMonthCap, factoryd.CaseDayCap: policy.RefusedDayCap} {
		face := factoryd.FaceOf(factoryd.Case{Kind: kind}, factoryd.Facts{Card: factoryd.Task{ID: "t1", Kind: "agent", IssueNumber: 3}})
		when := factoryd.CapTurn(decision, time.Now()).UTC().Format("2 January at 15:04 UTC")
		var keys []string
		for _, a := range face.Answers {
			keys = append(keys, a.Key())
		}
		if !slices.Equal(keys, []string{factoryd.ActNote, factoryd.ActClose}) {
			t.Fatalf("%s: answers %v, want not now and end the card", kind, keys)
		}
		if want := "The card waits and goes on by itself when the limit resets on " + when + "."; face.Answers[0].Effect != want {
			t.Errorf("%s: Not now says %q, want %q", kind, face.Answers[0].Effect, want)
		}
	}
}
