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
	taskID       = regexp.MustCompile(`\bt-[0-9a-f]{6,}`)
	hash         = regexp.MustCompile(`\b[0-9a-f]{12,}\b`)
	branch       = regexp.MustCompile(`\bcard/\d+`)
	factoryWords = regexp.MustCompile(`(?i)\b(rungs?|cases?|offers?|premade|slots?)\b`)
	snakeCase    = regexp.MustCompile(`\b[a-z]+_[a-z_]+\b`)
	gateNames    = regexp.MustCompile(`\b(` + strings.Join([]string{factoryd.ActivityRunSession, factoryd.ActivityWaitChecks, factoryd.ActivityGateReproduction,
		factoryd.ActivityGateCriteria, factoryd.ActivityModelReview, factoryd.ActivityGateAudit}, "|") + `)\b`)
)

// agentWords leads the story line that quotes the agent's question whole:
// the words are the agent's, so no bound or guide of the face's own copy
// holds them.
const agentWords = "The agent asks: "

// viewLine is a writer's view: the writer named, then the words as they
// wrote them, which no bound or guide of the face's own copy holds.
var viewLine = regexp.MustCompile(`^The [a-z ]+'s view: \S`)

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
		if face.Suggested != "" && face.Answers[0].Key() != face.Suggested {
			t.Errorf("%s: the suggested %s is not the first of the answers %v", name, face.Suggested, labels(face.Answers))
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
		keys, shown := map[string]bool{}, map[string]bool{}
		for _, a := range slices.Concat(face.Answers, face.More) {
			if keys[a.Key()] || shown[a.Label] {
				t.Errorf("%s: %s %q shows twice among the answers %v and More %v", name, a.Key(), a.Label, labels(face.Answers), labels(face.More))
			}
			keys[a.Key()], shown[a.Label] = true, true
		}
		for _, a := range slices.Concat(face.Answers, face.More) {
			if why := actRefusal(x, a); why != "" {
				t.Errorf("%s: the face offers %q, which Act refuses: %s", name, a.Label, why)
			}
		}
		if v := face.Story.View; v != "" && !viewLine.MatchString(v) {
			t.Errorf("%s: the view %q names no writer", name, v)
		}
		story := slices.DeleteFunc([]string{face.Story.Planned, face.Story.Happened, face.Story.Left}, func(s string) bool { return strings.HasPrefix(s, agentWords) })
		for _, s := range story {
			if n := utf8.RuneCountInString(s); n > 240 {
				t.Errorf("%s: the story line %q has %d runes", name, s, n)
			}
		}
		texts := append([]string{face.Ask, face.Why}, story...)
		for _, a := range slices.Concat(face.Answers, face.More) {
			texts = append(texts, a.Label, a.Effect)
		}
		for _, s := range texts {
			if faults := writingguide.Lint(s); len(faults) > 0 {
				t.Errorf("%s: %q: %v", name, s, faults)
			}
			for _, re := range []*regexp.Regexp{taskID, hash, branch, factoryWords, snakeCase, gateNames} {
				if m := re.FindString(s); m != "" {
					t.Errorf("%s: %q names %q", name, s, m)
				}
			}
		}
	}
}

// actRefusal is why Act refuses the human's press of answer a on the
// fixture's case, "" when it takes it: a human card takes its own choices
// and its end alone; a table action, its press with its defaults.
func actRefusal(x faced, a factoryd.Answer) string {
	_, table := factoryd.ActionByName(a.Action)
	switch {
	case a.Value != "", !table:
		return ""
	case factoryd.ChoiceCase(x.c, x.fx.Task.Kind) && a.Action != factoryd.ActClose:
		return "a human card takes one of its choices"
	}
	res, err := a.Press(x.facts, written(a), "")
	if err != nil {
		return err.Error()
	}
	return factoryd.PressRefusal(x.facts, res)
}

func labels(as []factoryd.Answer) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Label)
	}
	return out
}

// sample is what a human writes in a required field.
var sample = map[string]string{"note": "Because the tests show it.", "depends_on": "#2190", "title": "Fix the importer", "criterion": "size_bound_escape"}

// written is sample in the required fields of answer a alone: every other
// field keeps its default.
func written(a factoryd.Answer) map[string]string {
	out := map[string]string{}
	for _, f := range a.Fields {
		if f.Required {
			out[f.Name] = sample[f.Name]
			if len(f.Options) > 0 {
				out[f.Name] = f.Options[0].Value
			}
		}
	}
	return out
}

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
				if !slices.Contains(stored, a.Value) {
					t.Errorf("%s: %q posts %s, which the card's question does not offer: %v", name, a.Label, a.Value, stored)
				}
			case !table:
				// the card's own door takes it: a plan's or a request's decision
			default:
				if why := factoryd.OfferRefusal(x.facts, a.Action); why != "" {
					t.Errorf("%s: %q is on the face, and its predicate refuses it: %s", name, a.Label, why)
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
// its Not now names the reset after the stop, and it offers no raise to
// go on. A stop at 18:00 on 30 November resets on the next day and month
// alike.
func TestACapWaitSaysWhenItGoesOnByItself(t *testing.T) {
	opened := time.Date(2026, 11, 30, 18, 0, 0, 0, time.UTC)
	for kind, when := range map[factoryd.CaseKind]string{factoryd.CaseMonthCap: "1 December at 00:00 UTC", factoryd.CaseDayCap: "1 December at 00:00 UTC",
		factoryd.CaseProjectCap: "1 December at 00:00 UTC"} {
		face := factoryd.FaceOf(factoryd.Case{Kind: kind, OpenedAt: opened}, factoryd.Facts{Card: factoryd.Task{ID: "t1", Kind: "agent", IssueNumber: 3}})
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

// fixture is the faced fixture of group and kind.
func fixture(t *testing.T, group, kind string) faced {
	t.Helper()
	for _, x := range facedFixtures(t) {
		if x.fx.Group == group && x.fx.Kind == kind {
			return x
		}
	}
	t.Fatalf("no fixture %s/%s", group, kind)
	return faced{}
}

// No card has one button. A human card's question with one choice is born
// with the card's "Not now" beside it, unless that is its choice, and a
// face with one answer gets "End the card" where no answer ends the card:
// "I did it" gets "Not now", "Not now" gets "End the card".
func TestAOneAnswerFaceAddsNoSecondEnd(t *testing.T) {
	card := factoryd.Task{ID: "t1", Kind: "human", IssueNumber: 7, Source: factoryd.SourceWebhook}
	face := func(value, label string) []string {
		q := factoryd.HumanQuestion{Ask: "Is the bot token rotated?", Choices: []factoryd.QuestionChoice{{Value: value, Label: label}}}
		c := factoryd.Case{Kind: factoryd.CaseHumanCard, Facts: factoryd.HumanAsk{Q: q, By: "factoryd", Asker: factoryd.SourceWebhook, Via: factoryd.ViaFactoryd}.Event(card).Detail}
		return labels(factoryd.FaceOf(c, factoryd.Facts{Card: card}).Answers)
	}
	if got, want := face(factoryd.AnswerPark, "Not now"), []string{"Not now", "End the card"}; !slices.Equal(got, want) {
		t.Errorf("a card that keeps waiting: answers %v, want %v", got, want)
	}
	if got, want := face(factoryd.ActClose, "I did it"), []string{"I did it", "Not now"}; !slices.Equal(got, want) {
		t.Errorf("a card that ends: answers %v, want %v", got, want)
	}
}

// The agent's own answers start a session. Where none can start, the face
// is the cap's, one decision at a time: the cap's ask and answers, the
// agent's question as one story line, whole, and none of its answers. A
// draw there leaves the question open and says so. #2091 pushed nothing,
// so no fresh card can go on from its branch.
func TestAnAgentsAnswersLeaveAFaceWhereNoSessionCanStart(t *testing.T) {
	x := fixture(t, "case", string(factoryd.CaseAgentEscalate))
	f := x.facts
	f.Recommend = factoryd.Recommendation{}
	open := factoryd.FaceOf(x.c, f)
	if !slices.ContainsFunc(open.Answers, func(a factoryd.Answer) bool { return a.Variant == "say" }) {
		t.Fatalf("the open face's answers %v hold none of the agent's", labels(open.Answers))
	}
	if !slices.ContainsFunc(open.More, func(a factoryd.Answer) bool { return a.Key() == factoryd.ActContinue }) {
		t.Errorf("More %v lacks Go on with my note", labels(open.More))
	}
	f.Attempts, f.SessionCap = 3, 3
	capped := factoryd.FaceOf(x.c, f)
	if want := "The card used all 3 sessions. Allow more, or end the card?"; capped.Ask != want {
		t.Errorf("at the session cap the ask is %q, want %q", capped.Ask, want)
	}
	if got, want := labels(capped.Answers), []string{"Allow more sessions", "End the card", "Not now"}; !slices.Equal(got, want) {
		t.Errorf("at the session cap the answers are %v, want %v", got, want)
	}
	if want := agentWords + "The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, " +
		"registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), none of which resolve in this sandbox. " +
		"How should the new digests, sha256 sums and versions be obtained?"; capped.Story.Happened != want {
		t.Errorf("at the session cap the story's line is %q, want the agent's question whole", capped.Story.Happened)
	}
	if want := "Gives the card one more session, and the agent's question comes back to you."; capped.Answers[0].Effect != want {
		t.Errorf("the extend's effect %q, want %q", capped.Answers[0].Effect, want)
	}
}

// Nothing suggests an answer on its own: a face suggests one only where a
// rung, a writer or the kind's own rule decided it with grounds.
func TestAFaceSuggestsOnlyWhatSomeoneDecided(t *testing.T) {
	for _, kind := range []string{string(factoryd.CaseSessionCap), string(factoryd.CaseScreeningSuspect), string(factoryd.CaseHumanCard), string(factoryd.CaseMonitorFlag)} {
		if x := fixture(t, "case", kind); x.face.Suggested != "" {
			t.Errorf("%s suggests %s by %s with no one deciding it", kind, x.face.Suggested, x.face.SuggestedBy)
		}
	}
	x := fixture(t, "case", string(factoryd.CasePlanQuestion))
	f := x.facts
	f.Recommend = factoryd.Recommendation{Action: factoryd.ActSalvage, By: "the factory agent", Grounds: "the run is gone"}
	if face := factoryd.FaceOf(x.c, f); face.Suggested != "plan:The 500 newest rows" || face.SuggestedBy != "the planner" {
		t.Errorf("a recommendation that cannot happen leaves %s by %s, want the planner's", face.Suggested, face.SuggestedBy)
	}
}

// A recommendation of an action the face holds in a card's own form leads
// with that form: one "Send back to the planner", the plan's.
func TestARecommendationLeadsWithTheFacesOwnForm(t *testing.T) {
	x := fixture(t, "case", string(factoryd.CaseRollHeld))
	f := x.facts
	f.Recommend = factoryd.Recommendation{Action: factoryd.ActReplan, By: "the factory agent", Grounds: "the plan is stuck"}
	face := factoryd.FaceOf(x.c, f)
	if got := labels(face.Answers); len(got) != 4 || got[0] != "Send back to the planner" || slices.Index(got[1:], got[0]) >= 0 {
		t.Fatalf("answers %v, want the plan's replan first and once", got)
	}
	if !strings.HasPrefix(face.Answers[0].Value, factoryd.AnswerReplanOnTop+":") {
		t.Errorf("the first answer posts %q, want the plan's replan", face.Answers[0].Value)
	}
}

// A real gate stop names its step in plain words, never the activity's
// code name.
func TestAGateStopNamesItsStepInWords(t *testing.T) {
	x := fixture(t, "case", string(factoryd.CaseGateStop))
	if want := "The review ran past its time limit. Check the work again, or tell the card how to go on?"; x.face.Ask != want {
		t.Errorf("ask %q, want %q", x.face.Ask, want)
	}
}

// At its spend limit a card out of money asks for the raise of the limit,
// the one answer that can happen, and offers no "Add money". One press
// raises the limit by the 2.00 EUR the next session passes it by and adds
// the 2.00 EUR it needs, and the effect says so.
func TestBudgetFloorAtTheSpendLimitAsksForTheRaise(t *testing.T) {
	f := factoryd.CaseFacts{Kind: factoryd.CaseBudgetFloor, Attempts: 2, SessionCap: 3, BudgetEUR: 4, SpentEUR: 4, LeftEUR: 0, FloorEUR: 2, CeilingEUR: 4,
		Head: "7eaf7c73121dd332c52b95bb5b024a08b3757146", PR: 9, PROpen: true, Pushed: true, Checks: "green",
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	face := factoryd.FaceOf(factoryd.Case{Kind: factoryd.CaseBudgetFloor}, factoryd.Facts{CaseFacts: f, Card: factoryd.Task{ID: "t1", Kind: "agent", IssueNumber: 5}})
	if want := "The card is out of money and at its spend limit. Raise the limit and add 2.00 EUR, or end the card?"; face.Ask != want {
		t.Errorf("ask %q, want %q", face.Ask, want)
	}
	raise := face.Answers[0]
	if want := "Raises the spend limit by 2.00 EUR and adds 2.00 EUR, and the card goes on."; raise.Effect != want {
		t.Errorf("effect %q, want %q", raise.Effect, want)
	}
	facts := factoryd.Facts{CaseFacts: f}
	res, err := raise.Press(facts, nil, "")
	if err != nil || res.Args["eur"] != 2.0 || res.Fund == nil || res.Fund.EUR != 2 || factoryd.PressRefusal(facts, res) != "" {
		t.Errorf("the raise presses %+v, %v, refusal %q; want a raise of 2.00 EUR and a fund of 2.00 EUR the table takes", res, err, factoryd.PressRefusal(facts, res))
	}
	if got, want := labels(face.Answers), []string{"Raise the spend limit", "Start a fresh card on this work", "End the card", "Not now"}; !slices.Equal(got, want) {
		t.Errorf("answers %v, want %v", got, want)
	}
	if want := "The card has 0.00 EUR left, and the next session needs 2.00 EUR to start."; face.Why != want {
		t.Errorf("why %q, want %q", face.Why, want)
	}
}

// A human card whose ask is no question asks whether its titled work is
// done, the whole title quoted when that passes the ask's check, else the
// card's work: no part of a title is cut away.
func TestAHumanCardAsksWhetherItsTitledWorkIsDone(t *testing.T) {
	ask := func(title string) string {
		card := factoryd.Task{ID: "t1", Kind: "human", IssueNumber: 7, Objective: factoryd.IssueObjective(title, "Make a new one in Forgejo."), Source: factoryd.SourceWebhook}
		c := factoryd.Case{Kind: factoryd.CaseHumanCard, Facts: map[string]any{"offers": []any{map[string]any{"value": factoryd.ActClose, "label": "I did it"}}}}
		return factoryd.FaceOf(c, factoryd.Facts{Card: card}).Ask
	}
	for title, want := range map[string]string{
		"Rotate the bot token before Friday":       "Is \u201cRotate the bot token before Friday\u201d done?",
		"Rotate the bot token: it expires Friday":  "Is the work of this card done?",
		"Fix CI: it fails on arm64":                "Is the work of this card done?",
		"Board export: keep the nightly snapshots": "Is the work of this card done?",
	} {
		if got := ask(title); got != want {
			t.Errorf("%q asks %q, want %q", title, got, want)
		}
	}
}

// Clear the audit's findings clears the findings ticked and nothing else:
// a box per finding in More, no box ticked is refused before anything
// clears, and two ticked boxes name those two findings alone.
func TestClearingTheAuditClearsOnlyTheTickedFindings(t *testing.T) {
	x := fixture(t, "case", string(factoryd.CaseBudgetFloor))
	i := slices.IndexFunc(x.face.More, func(a factoryd.Answer) bool { return a.Action == factoryd.ActConfirmAudit })
	if i < 0 {
		t.Fatalf("More %v lacks the audit's clearing", labels(x.face.More))
	}
	clear := x.face.More[i]
	if got := len(clear.Fields[0].Options); got != 8 {
		t.Fatalf("%d boxes, want one per audit finding, 8", got)
	}
	if _, err := clear.Press(x.facts, map[string]string{"findings": ""}, "The rig's fakes."); err == nil || err.Error() != "Tick at least one finding to clear" {
		t.Errorf("no box ticked = %v, want the refusal to tick at least one", err)
	}
	two := "secrets at acceptance/price_floor_acceptance_test.go:71\nloopback endpoint at acceptance/price_floor_acceptance_test.go:64"
	res, err := clear.Press(x.facts, map[string]string{"findings": two}, "The rig's fakes.")
	if err != nil || res.Args["findings"] != two || factoryd.PressRefusal(x.facts, res) != "" {
		t.Fatalf("two ticked = %+v, %v, refusal %q; want the two findings alone", res, err, factoryd.PressRefusal(x.facts, res))
	}
	res.Args["findings"] = ""
	if got := factoryd.PressRefusal(x.facts, res); got != "no finding is ticked: tick at least one finding to clear" {
		t.Errorf("the table takes no finding with %q, want the refusal to tick one", got)
	}
}

// An agent's question the ask cannot carry shows whole on the face: the
// ask falls back, and the story quotes the question, cut nowhere.
func TestAnAgentsQuestionShowsWholeOnItsFace(t *testing.T) {
	x := fixture(t, "case", string(factoryd.CaseAgentEscalate))
	if want := "The agent asked a question it cannot answer alone. How should it go on?"; x.face.Ask != want {
		t.Errorf("ask %q, want %q", x.face.Ask, want)
	}
	want := agentWords + "The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, " +
		"registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), none of which resolve in this sandbox. " +
		"How should the new digests, sha256 sums and versions be obtained?"
	if x.face.Story.Happened != want {
		t.Errorf("the story's line %q, want %q", x.face.Story.Happened, want)
	}
}

// A suggestion is the answer a writer stored, never one matched from its
// words: a human card's recommendation that names a label in words
// preselects nothing and shows as a story line naming its writer, words
// no one signs show nowhere, and the roller's stored pick leads its card.
func TestASuggestionIsTheStoredPickAlone(t *testing.T) {
	q := factoryd.HumanQuestion{Ask: "Is the bot token rotated?", Recommendation: "Rotate it now, the old one expires Friday", Source: "the project agent",
		Choices: []factoryd.QuestionChoice{{Value: "say:Rotate it now", Label: "Rotate it now"}, {Value: "say:Wait a week", Label: "Wait a week"}}}
	card := factoryd.Task{ID: "t1", Kind: "human", IssueNumber: 7, Source: factoryd.SourceChat}
	face := func(q factoryd.HumanQuestion) factoryd.AskFace {
		detail := factoryd.HumanAsk{Q: q, By: "sebastian", Asker: "agent", Via: factoryd.ViaChat}.Event(card).Detail
		return factoryd.FaceOf(factoryd.Case{Kind: factoryd.CaseHumanCard, Facts: detail}, factoryd.Facts{Card: card, Recommend: factoryd.RecommendationOf(detail)})
	}
	if got := face(q); got.Suggested != "" || got.Grounds != "" || got.SuggestedBy != "" || got.Story.View != "The project agent's view: Rotate it now, the old one expires Friday" {
		t.Errorf("words alone suggest %q, by %q on %q, view %q; want no suggestion and the words as the writer's view", got.Suggested, got.SuggestedBy, got.Grounds, got.Story.View)
	}
	detail := factoryd.HumanAsk{Q: q, By: "sebastian", Asker: "agent", Via: factoryd.ViaChat}.Event(card).Detail
	delete(detail, "recommended_by")
	if got := factoryd.FaceOf(factoryd.Case{Kind: factoryd.CaseHumanCard, Facts: detail}, factoryd.Facts{Card: card, Recommend: factoryd.RecommendationOf(detail)}); got.Story.View != "" || got.Grounds != "" {
		t.Errorf("words no one signs show as %q, %q", got.Story.View, got.Grounds)
	}
	q.Recommended = "say:Wait a week"
	if got := face(q); got.Suggested != "choice:say:Wait a week" || got.Answers[0].Label != "Wait a week" || got.Grounds != q.Recommendation {
		t.Errorf("the stored pick suggests %q first among %v on %q; want Wait a week", got.Suggested, labels(got.Answers), got.Grounds)
	}
	q.Recommended = "say:Rotate it later"
	if got := face(q); got.Suggested != "" {
		t.Errorf("a pick that is none of the choices suggests %q", got.Suggested)
	}
	if x := fixture(t, "source", factoryd.SourceRollFailed+"/"+facefixtures.Columns[0].Name); x.face.Suggested != "choice:"+factoryd.AnswerRetryRoll || x.face.SuggestedBy != "the roller" {
		t.Errorf("the roll card suggests %q by %q, want the roll again by the roller", x.face.Suggested, x.face.SuggestedBy)
	}
}

// At every cap that keeps a session off, the agent's question is the cap's
// face: the cap's ask and answers, the question whole as one story line, no
// answer of the agent's, and every answer one Act takes. #2091 pushed
// nothing, so no fresh card goes on from it; its budget leaves room under
// its ceiling, so a fund needs no raise. A limit that resets by itself
// says the question comes back when it does.
func TestAnAgentsQuestionAtEveryCapIsTheCapsFace(t *testing.T) {
	x := fixture(t, "case", string(factoryd.CaseAgentEscalate))
	question := agentWords + "The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, " +
		"registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), none of which resolve in this sandbox. " +
		"How should the new digests, sha256 sums and versions be obtained?"
	capWait := func(decision string) func(*factoryd.CaseFacts) {
		return func(f *factoryd.CaseFacts) {
			f.CapWait, _, _ = strings.Cut(factoryd.CapWaitReason(decision, "the gateway refused the spend", "state", time.Now().Add(time.Hour)), "\n")
		}
	}
	wait := []string{"Not now", "End the card"}
	for _, tc := range []struct {
		cap     string
		over    func(*factoryd.CaseFacts)
		ask     string
		answers []string
	}{
		{"day cap", capWait(policy.RefusedDayCap), "The factory's spending limit for today is reached. Wait for it to reset, or end the card?", wait},
		{"month cap", capWait(policy.RefusedMonthCap), "The factory's spending limit for this month is reached. Wait for it to reset, or end the card?", wait},
		{"project cap", capWait(policy.RefusedProjectCap), "This project's spending limit for the month is reached. Wait for it to reset, or end the card?", wait},
		{"budget floor", func(f *factoryd.CaseFacts) { f.LeftEUR = 0.1 }, "The card is out of money. Add 0.38 EUR, or end the card?",
			[]string{"Add money", "End the card", "Not now"}},
		{"session cap", func(f *factoryd.CaseFacts) { f.Attempts = f.SessionCap }, "The card used all 3 sessions. Allow more, or end the card?",
			[]string{"Allow more sessions", "End the card", "Not now"}},
		{"agent time cap", func(f *factoryd.CaseFacts) { f.AgentTimeS = f.AgentTimeCapS }, "The card used its agent time of 120 minutes. Allow more, or end the card?",
			[]string{"Allow more sessions", "End the card", "Not now"}},
	} {
		y := x
		tc.over(&y.facts.CaseFacts)
		y.facts.Recommend = factoryd.Recommendation{}
		face := factoryd.FaceOf(y.c, y.facts)
		if face.Ask != tc.ask || !slices.Equal(labels(face.Answers), tc.answers) || face.Story.Happened != question {
			t.Errorf("%s: %q with %v and the story line %q; want %q with %v and the question whole", tc.cap, face.Ask, labels(face.Answers), face.Story.Happened, tc.ask, tc.answers)
		}
		for _, a := range slices.Concat(face.Answers, face.More) {
			if a.Variant == "say" {
				t.Errorf("%s: the agent's answer %q shows at the cap", tc.cap, a.Label)
			}
			if why := actRefusal(y, a); why != "" {
				t.Errorf("%s: the face offers %q, which Act refuses: %s", tc.cap, a.Label, why)
			}
		}
		if len(tc.answers) == 2 && (!strings.HasPrefix(face.Answers[0].Effect, "The card waits, and the agent's question comes back to you when the limit resets on ") ||
			!strings.HasSuffix(face.Why, ", and the agent's question comes back to you when it resets.")) {
			t.Errorf("%s: Not now says %q and the why %q; want the question back at the reset", tc.cap, face.Answers[0].Effect, face.Why)
		}
	}
}

// A rung's recommendation leads, and the agent's own words still show as
// its view: one suggestion, and no writer's words lost.
func TestTheAgentsViewShowsBesideARungsRecommendation(t *testing.T) {
	c := factoryd.Case{Kind: factoryd.CaseAgentEscalate, Facts: map[string]any{"reason": factoryd.AgentAsks + "Which store keeps the snapshots?\nWhy: two fit\nRecommendation: SQLite, one file is enough",
		"choices": []any{"Postgres", "SQLite"}}}
	f := factoryd.Facts{CaseFacts: factoryd.CaseFacts{Kind: factoryd.CaseAgentEscalate, Attempts: 1, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, SessionLeastS: 300,
		BudgetEUR: 2, LeftEUR: 1.5, FloorEUR: 0.3, CeilingEUR: 18}, Card: factoryd.Task{ID: "t1", Kind: "agent", IssueNumber: 4},
		Recommend: factoryd.Recommendation{Action: factoryd.ActClose, By: "the factory agent", Grounds: "the work is on main"}}
	face := factoryd.FaceOf(c, f)
	if face.Suggested != factoryd.ActClose || face.Story.View != "The agent's view: SQLite, one file is enough" {
		t.Errorf("suggested %q, view %q; want the rung's close and the agent's view", face.Suggested, face.Story.View)
	}
}
