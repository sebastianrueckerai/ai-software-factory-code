package factoryd

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// FieldKind is what a field of an answer takes.
type FieldKind int

const (
	FieldText FieldKind = iota
	FieldNumber
	// FieldMoney is an amount in EUR with cents.
	FieldMoney
	FieldChoice
	// FieldCheck is a box: ticked, the field is "yes". With options, it is
	// a box per option, and the field holds the ticked ones, one per line.
	FieldCheck
	// FieldFixed is a value the page fixes, such as the promise it asks
	// about: the press posts its default unseen.
	FieldFixed
)

// Option is one value of a choice field and the words it shows.
type Option struct{ Value, Label string }

// Field is one input an answer opens after its press. A required field
// has no default and its hint says what to write; every other field opens
// with its default filled in.
type Field struct {
	Name, Label, Hint string
	Kind              FieldKind
	Required          bool
	Default           func(Facts) string
	Options           []Option
}

// Answer is one answer a face offers: an action of the table, or the
// card's own choice, with the variant that names its form for one class of
// card, its one label, the line that says what happens next, and the
// fields it opens. Value is what a human card's own choice posts, "" for
// an answer posted by its key.
type Answer struct {
	Action, Variant string
	Label, Effect   string
	Fields          []Field
	Value           string
}

// Story is what a face tells in short lines: what was planned or
// promised, what happened, what is left, and a writer's view whose words
// pick none of the answers, named with its writer.
type Story struct{ Planned, Happened, Left, View string }

// without is story s with no line that repeats one of shown, up to the
// mark that ends it.
func (s Story) without(shown ...string) Story {
	bare := func(t string) string { return strings.TrimRight(strings.TrimSpace(t), ".?!") }
	for _, l := range []*string{&s.Planned, &s.Happened, &s.Left, &s.View} {
		if slices.ContainsFunc(shown, func(t string) bool { return bare(t) == bare(*l) }) {
			*l = ""
		}
	}
	return s
}

// StoryMax bounds each line of a story, in runes. A writer's words
// quoted whole may run longer, and a face shows longer words in a fold.
const StoryMax = 240

// FaceSpec is the face of one kind: its ask, why line and story from the
// facts, the answers to its question by key, the other actions of its own
// by key, and the answer it suggests with who suggests it and why. A
// suggestion without grounds, or a nil Suggest, suggests nothing; grounds
// without an answer are the writer's view, a story line that names the
// writer, and show only with one.
type FaceSpec struct {
	Ask     func(Facts) string
	Why     func(Facts) string
	Story   func(Facts) Story
	Answers func(Facts) []string
	More    func(Facts) []string
	Suggest func(Facts) (action, by, grounds string)
}

// Facts are what a face is written from: the facts the action table
// weighs, the card, the case, and the answer a rung or the case writer
// recommends, if one does.
type Facts struct {
	CaseFacts
	Card      Task
	Case      Case
	Recommend Recommendation
}

// Recommendation is an answer a rung or the case writer recommends: its
// key, who recommends it and on what grounds.
type Recommendation struct{ Action, By, Grounds string }

// AskFace is a face as a human reads it: one question, why it came up, the
// story, at most four answers with the suggested one first, and every
// other answer that holds in More. Findings are the texts of the findings
// its ask and story count.
type AskFace struct {
	Ask, Why                        string
	Story                           Story
	Answers                         []Answer
	Suggested, SuggestedBy, Grounds string
	More                            []Answer
	Findings                        []string
}

// factoryNames are the names of the factory's own machinery, which no face
// shows: a card's id, the chat's and the check-in's names as deciders, an
// activity's name, and an action's or a case kind's key. factoryd names a
// service a card may deploy or watch, so it is no such name.
var factoryNames = func() *regexp.Regexp {
	names := []string{ChatUser, CheckinDecider, ActivityRunSession, ActivityWaitChecks, ActivityGateReproduction,
		ActivityGateCriteria, ActivityModelReview, ActivityGateAudit}
	for _, a := range Actions {
		if strings.Contains(a.Name, "_") {
			names = append(names, a.Name)
		}
	}
	for _, k := range CaseKinds() {
		if strings.Contains(string(k), "_") {
			names = append(names, string(k))
		}
	}
	for i := range names {
		names[i] = regexp.QuoteMeta(names[i])
	}
	return regexp.MustCompile(`\bt-[0-9a-f]{6,}\b|\b(?:` + strings.Join(names, "|") + `)\b`)
}()

// FactoryName is the first name of the factory's own machinery s shows,
// "" for none.
func FactoryName(s string) string { return factoryNames.FindString(s) }

// GroundsFaults are the faults of a suggestion's grounds: the guide's
// (writingguide.Lint) and a name of the factory's own machinery. Every
// door where a rung or a writer files grounds refuses them with these
// lines, so the writer corrects itself.
func GroundsFaults(grounds string) []string {
	faults := writingguide.Lint(grounds)
	if m := FactoryName(grounds); m != "" {
		faults = append(faults, fmt.Sprintf("The text holds %q, a name of the factory's own machinery. Say what it means for the reader.", m))
	}
	return faults
}

// shownGrounds are grounds as a face shows them: none when they fail
// GroundsFaults, as grounds filed before the doors held them to it may;
// the pick and its writer stay.
func shownGrounds(grounds string) string {
	if len(GroundsFaults(grounds)) > 0 {
		return ""
	}
	return grounds
}

// RecommendationOf is the recommendation an ask row stores: the action it
// preselects, who recommends it and the grounds; none when it preselects
// nothing. The words never name the action.
func RecommendationOf(detail map[string]any) Recommendation {
	var r Recommendation
	if p, ok := detail["preselected"].(map[string]any); ok {
		r.Action, _ = p["action"].(string)
	}
	r.Grounds, _ = detail["recommendation"].(string)
	if r.Action == "" {
		return Recommendation{}
	}
	by, _ := detail["recommended_by"].(string)
	r.By = cmp.Or(by, "the factory")
	return r
}

// mostAnswers bounds the answers of one face.
const mostAnswers = 4

// OwnWords says answer a sends the human's own words.
func (a Answer) OwnWords() bool { return a.Variant == variantOwnWords }

// Key names answer a: its action, its action and variant, or for the words
// an agent offered, "say:" and the words.
func (a Answer) Key() string {
	switch {
	case a.Value != "":
		return choicePrefix + a.Value
	case a.Variant == "":
		return a.Action
	case a.Variant == sayVariant:
		return sayPrefix + a.Label
	case a.Variant == variantPlan:
		return planPrefix + a.Label
	}
	return a.Action + "/" + a.Variant
}

const (
	sayVariant   = "say"
	sayPrefix    = "say:"
	sayEffect    = "Starts a session with this answer."
	planPrefix   = "plan:"
	planEffect   = "Sends this answer to the planner."
	choicePrefix = "choice:"
)

// AnswerOf is the answer key names: its vocabulary row, or for "say:" the
// agent's words and for "plan:" the planner's choice as the label.
func AnswerOf(key string) (Answer, bool) {
	if words, ok := strings.CutPrefix(key, sayPrefix); ok && strings.TrimSpace(words) != "" {
		return Answer{Action: ActContinue, Variant: sayVariant, Label: words, Effect: sayEffect}, true
	}
	if words, ok := strings.CutPrefix(key, planPrefix); ok && strings.TrimSpace(words) != "" {
		return Answer{Action: AnswerPlanChoice, Variant: variantPlan, Label: words, Effect: planEffect}, true
	}
	action, variant, _ := strings.Cut(key, "/")
	i := slices.IndexFunc(Vocabulary, func(a Answer) bool { return a.Action == action && a.Variant == variant })
	if i < 0 {
		return Answer{}, false
	}
	return Vocabulary[i], true
}

// faceSpecOf is the face spec of case c on facts f: a flag's own, an approval
// kind's own, else its kind's, and the face of an open problem for a kind
// with none.
func faceSpecOf(c Case, f Facts) FaceSpec {
	switch {
	case c.Kind == CaseMonitorFlag:
		if s, ok := FlagFaces[f.Flag]; ok {
			return s
		}
	case f.ApprovalKind != "":
		if s, ok := ApprovalFaces[f.ApprovalKind]; ok {
			return s
		}
	case ChoiceCase(c, f.Card.Kind):
		return humanFace
	}
	if s, ok := CaseFaces[c.Kind]; ok {
		return s
	}
	return CaseFaces[CaseOther]
}

// facing is case c on facts f as its face reads them: f holds c, an
// ended card's facts say how it ended, and a question of the agent's at a
// cap is its cap's case (atCap).
func facing(c Case, f Facts) (Case, Facts) {
	f.Case = c
	if f.Kind == "" {
		f.Kind = c.Kind
	}
	if f.Final == "" && Final(f.Card.Status) {
		f.Final = f.Card.Status
	}
	if capped := f.atCap(); capped.Kind != f.Kind {
		f.CaseFacts, c.Kind = capped, capped.Kind
	}
	return c, f
}

// ownAsk is case c's own question on card t, with no cap in front of it:
// the case's problem, as the rungs and the digest read it.
func ownAsk(c Case, t Task) string {
	f := Facts{CaseFacts: FactsOf(c), Card: t, Case: c}
	return faceSpecOf(c, f).Ask(f)
}

// FaceOf is the face of case c on facts f. An answer whose press with its
// defaults fails on f leaves the face. A rung's or the writer's
// recommendation leads, else the kind's own suggestion with its grounds;
// an answer of another kind's question is put first and the last answer
// over four drops to More. A face of the card's own ask left with one
// answer adds "Not now" and "End the card", each unless an answer of its
// action is there; a request's and a plan's ask offer only their own.
func FaceOf(c Case, f Facts) AskFace {
	question := cmp.Or(f.Kind, c.Kind)
	c, f = facing(c, f)
	spec := faceSpecOf(c, f)
	face := AskFace{Ask: spec.Ask(f), Why: spec.Why(f), Findings: openFindingTexts(f.CaseFacts)}
	if spec.Story != nil {
		face.Story = spec.Story(f).without(face.Ask, face.Why)
	}
	if f.Kind != question {
		face.Story.Happened = asks(question) + agentWhole(f)
	}
	var answers []Answer
	for _, key := range spec.Answers(f) {
		if a, ok := AnswerFor(key, f); ok && answerHolds(a, f) && !slices.ContainsFunc(answers, func(o Answer) bool { return o.Key() == a.Key() || o.Label == a.Label }) {
			answers = append(answers, a)
		}
	}
	lead := func(key, by, grounds string) bool {
		if key == "" {
			return false
		}
		i := slices.IndexFunc(answers, func(o Answer) bool { return o.Key() == key || o.Value == key })
		if i < 0 {
			a, ok := AnswerFor(key, f)
			if !ok {
				return false
			}
			if i = slices.IndexFunc(answers, func(o Answer) bool { return o.Label == a.Label }); i < 0 {
				if !answerHolds(a, f) {
					return false
				}
				answers, i = append(answers, a), len(answers)
			}
		}
		a := answers[i]
		answers = slices.Insert(slices.Delete(answers, i, i+1), 0, a)
		face.Suggested, face.SuggestedBy, face.Grounds = a.Key(), by, shownGrounds(grounds)
		return true
	}
	led := lead(f.Recommend.Action, f.Recommend.By, f.Recommend.Grounds)
	if spec.Suggest != nil {
		// words that lead no answer, beside a recommendation or alone, are
		// their writer's view
		if key, by, grounds := spec.Suggest(f); grounds != "" && by != "" && grounds != face.Grounds && (led || key == "" || !lead(key, by, grounds)) && shownGrounds(grounds) != "" {
			face.Story.View = capitalize(by) + "'s view: " + strings.Join(strings.Fields(grounds), " ")
		}
	}
	if len(answers) > mostAnswers {
		face.More = append(face.More, answers[mostAnswers:]...)
		answers = answers[:mostAnswers]
	}
	if len(answers) < 2 && f.ApprovalKind == "" && AnsweredOnCard(c.Kind) {
		for _, key := range []string{ActNote, ActClose} {
			if a, ok := AnswerFor(key, f); ok && answerHolds(a, f) && !slices.ContainsFunc(answers, func(o Answer) bool { return o.Action == a.Action }) {
				answers = append(answers, a)
			}
		}
	}
	face.Answers = answers
	more := moreActions(f)
	switch c.Kind {
	case CaseMergeApproval:
		// the merge's answers are its whole say on the merge, on its own page
		more = slices.DeleteFunc(more, func(a Answer) bool {
			return a.Action == ActApproveMerge || a.Action == ActDenyMerge || a.Action == ActRerunChecks
		})
	case CasePlanHeld:
		// its answers are its whole say on the plan's close and its hold
		more = slices.DeleteFunc(more, func(a Answer) bool { return a.Action == ActClose || a.Action == ActSettle })
	}
	if spec.More != nil {
		for _, key := range spec.More(f) {
			if a, ok := AnswerFor(key, f); ok && answerHolds(a, f) {
				more = append(more, a)
			}
		}
	}
	for _, a := range more {
		if !slices.ContainsFunc(slices.Concat(answers, face.More), func(o Answer) bool {
			// a settle ends the case and does nothing else: one is enough
			return o.Key() == a.Key() || o.Label == a.Label || o.Action == a.Action && (o.Effect == a.Effect || a.Action == ActSettle)
		}) {
			face.More = append(face.More, a)
		}
	}
	if step := HelperStep(f.Card.Source); step != "" {
		return face.ofStep(step)
	}
	return face
}

// AnswerFor is the answer key names on facts f, its effect line filled from
// the facts where the vocabulary leaves it to them.
func AnswerFor(key string, f Facts) (Answer, bool) {
	if value, ok := strings.CutPrefix(key, choicePrefix); ok {
		return choiceAnswer(value, f)
	}
	a, ok := AnswerOf(key)
	if !ok || a.Key() == ActClose+"/"+variantDone && len(f.Dependencies) > 0 {
		return Answer{}, false
	}
	if a.Action == ActExtend && f.Kind == CaseAgentTimeCap {
		// the stop asks agent time: its minutes lead
		a.Fields = slices.Clone(a.Fields)
		if i := slices.IndexFunc(a.Fields, func(x Field) bool { return x.Name == "minutes" }); i > 0 {
			minutes := a.Fields[i]
			a.Fields = slices.Insert(slices.Delete(a.Fields, i, i+1), 0, minutes)
		}
	}
	if a.Action == ActConfirmCriteria && f.ClosedCard {
		a.Fields = []Field{requiredNote("What shows it", "The line that shows main meets the promises")}
	}
	if p, page := promiseOn(f); a.Action == ActAnswerCriteria && page {
		if a.Variant != "waive" && p.Kind != CriterionHuman {
			// a human judges only a human's promise
			return Answer{}, false
		}
		// the page is the promise's: its press names it unseen
		a.Fields = slices.Clone(a.Fields)
		for i, field := range a.Fields {
			if field.Name == "criterion" {
				field.Kind, field.Default, field.Options = FieldFixed, fixed(p.ID), nil
				a.Fields[i] = field
			}
		}
	} else if names := promisesOpen(f); a.Action == ActAnswerCriteria && len(names) > 0 {
		// the request names its open promises: the human picks one
		a.Fields = slices.Clone(a.Fields)
		for i, field := range a.Fields {
			if field.Name == "criterion" {
				field.Kind, field.Default, field.Options = FieldChoice, fixed(names[0]), nil
				for _, n := range names {
					field.Options = append(field.Options, Option{n, n})
				}
				a.Fields[i] = field
			}
		}
	}
	if a.Action == ActConfirmAudit {
		// a box per finding of the head's newest audit, which posts the
		// finding's id
		a.Fields = slices.Clone(a.Fields)
		for _, id := range f.AuditFindings {
			if text := f.FindingTexts[id]; text != "" {
				a.Fields[0].Options = append(a.Fields[0].Options, Option{id, text})
			}
		}
	}
	if a.Action == AnswerPlanChoice && a.Variant == variantPlan {
		// the planner's line of what the choice changes; a question stored
		// before it was asked sends the choice to the planner
		choices, changes := factStrings(f.Case, "choices"), factStrings(f.Case, "changes")
		if i := slices.Index(choices, a.Label); i >= 0 && i < len(changes) && changes[i] != "" {
			a.Effect = changes[i]
		}
	}
	if fill, ok := effectFills[a.Key()]; ok {
		if e := fill(f); e != "" {
			a.Effect = e
		}
	}
	if step := HelperStep(f.Card.Source); step != "" && a.Variant != f.Card.Source {
		// a helper's ask stands on its parent: its end is the step's own
		if a.Key() == ActClose {
			return AnswerOf(ActClose + "/" + f.Card.Source)
		}
		a.Effect = stepWords(a.Effect, step)
		if !strings.Contains(a.Effect, step) {
			// a session it starts is the step's, not the card's
			a.Effect = aSession.ReplaceAllString(a.Effect, "a session of "+step)
			a.Effect = nextSession.ReplaceAllString(a.Effect, step+"'s next session")
		}
	}
	return a, true
}

// answerHolds reports whether answer a can happen on facts f: the human's
// press of a table action with its defaults, each required field written,
// is one the table takes. Any other answer is the card's own, and its own
// door decides it. A human card takes its own choices and its end alone.
func answerHolds(a Answer, f Facts) bool {
	act, ok := ActionByName(a.Action)
	switch {
	case !ok || a.Value != "":
		return true
	case ChoiceCase(f.Case, f.Card.Kind) && a.Action != ActClose && a.Key() != ActNote+"/"+variantOwnWords:
		return false
	}
	written := map[string]string{}
	for _, field := range a.Fields {
		if field.Required {
			written[field.Name] = "written"
			if len(field.Options) > 0 {
				written[field.Name] = field.Options[0].Value
			}
		}
	}
	res, err := a.Press(f, written, "")
	return err == nil && act.refusal(f.CaseFacts, 3, res) == ""
}

// moreActions are the actions of the table a human may pick on f, in the
// vocabulary's words.
func moreActions(f Facts) []Answer {
	var out []Answer
	for _, name := range HumanActions() {
		if a, ok := AnswerFor(name, f); ok && answerHolds(a, f) {
			out = append(out, a)
		}
	}
	return out
}

// depWait says which cards card f waits for, none of them done yet: each
// filed one by its issue, and the count of the ones not filed yet; ""
// while it waits for none.
func depWait(f Facts) string {
	var refs []string
	for _, d := range f.Dependencies {
		if d.Issue > 0 {
			refs = append(refs, fmt.Sprintf("#%d", d.Issue))
		}
	}
	switch n := len(f.Dependencies) - len(refs); {
	case n == 1:
		refs = append(refs, "another card")
	case n > 1:
		refs = append(refs, fmt.Sprintf("%d other cards", n))
	}
	switch n := len(refs); {
	case n == 0:
		return ""
	case n > 1:
		refs = []string{strings.Join(refs[:n-1], ", ") + " and " + refs[n-1]}
	}
	return "It waits for " + refs[0] + " to be done first."
}

// openFindingTexts are the texts of the findings open on f's head
// (openIDs), in the order the gates raised them.
func openFindingTexts(f CaseFacts) []string {
	open := openIDs(f)
	var out []string
	seen := map[string]bool{}
	for _, id := range slices.Concat(f.FindingOrder, open) {
		if text := f.FindingTexts[id]; !seen[id] && slices.Contains(open, id) && text != "" {
			seen[id] = true
			out = append(out, text)
		}
	}
	return out
}

// MoreOf are the actions of card t, which holds no case, by its state: a
// live card's replan, more sessions, money, a word to its running session
// and its end; a failed card's retry and its close; a plan at rest its
// close, whose note waives the promises it leaves open; a held card's
// release first, and the hold of a card that has not started; and on every
// card its brief, its colour and a thread.
// The card's own replan says why it goes back, and a plan's card may take
// the plan's merged cards off main first. Who may press which is the
// console's to judge.
func MoreOf(t Task, f Facts) []Answer {
	f.Card = t
	var keys []string
	switch {
	case ClosablePlan(t):
		keys = []string{ActClose}
	case t.Status == "failed":
		keys = []string{ActRetry, ActClose}
	case !Final(t.Status):
		keys = []string{ActReplan, ActExtend, ActFund, AnswerSteer, ActClose}
	}
	switch {
	case t.Held():
		keys = append([]string{AnswerRelease}, keys...)
	case t.Holdable():
		keys = append(keys, AnswerHold)
	}
	var out []Answer
	for _, key := range append(keys, AnswerEditBrief, AnswerAccent, AnswerThread) {
		a, ok := AnswerFor(key, f)
		switch {
		case !ok, a.Action == AnswerSteer && t.Status != "running":
			continue
		case a.Action == ActReplan:
			a.Fields = []Field{requiredNote("Why it goes back", "What the planner should do differently")}
			if InPlan(t) {
				a.Fields = append(a.Fields, Field{Name: OffMainField, Label: "Take the plan's merged cards off main first", Kind: FieldCheck, Default: none})
			}
		}
		out = append(out, a)
	}
	return out
}

// OffMainField is the box of a card's own replan that takes its plan's
// merged cards off main first.
const OffMainField = "off_main"

// Defaults are the values answer a's fields open with on facts f, as its
// press reads a field left empty; a required field has none.
func (a Answer) Defaults(f Facts) map[string]string {
	f.CaseFacts = f.atCap()
	out := map[string]string{}
	for _, field := range a.Fields {
		if field.Default != nil {
			out[field.Name] = field.Default(f)
		}
	}
	return out
}

// ArgsOf parses the fields of answer a from get, a field's posted value by
// name: the action's arguments, the variant's own among them, and the
// amount in EUR a money field holds. An empty field is left out.
func ArgsOf(a Answer, get func(name string) string) (map[string]any, float64, error) {
	args := map[string]any{}
	switch a.Action {
	case ActDenyMerge:
		args["action"] = cmp.Or(a.Variant, "request_changes")
	case ActAnswerCriteria:
		args["verdict"] = cmp.Or(a.Variant, "met")
	}
	var eur float64
	for _, field := range a.Fields {
		v := strings.TrimSpace(get(field.Name))
		if v == "" || field.Name == noteField {
			continue
		}
		switch field.Kind {
		case FieldMoney:
			amount, err := strconv.ParseFloat(v, 64)
			if err != nil || amount < 0 || math.IsInf(amount, 0) {
				return nil, 0, fmt.Errorf("%s takes an amount in EUR, such as 2.50, and you wrote %q", field.Label, v)
			}
			eur = amount
		case FieldNumber:
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, 0, fmt.Errorf("%s takes a whole number, such as 1, and you wrote %q", field.Label, v)
			}
			if field.Name == "minutes" {
				args["agent_time_s"] = n * 60
			} else {
				args[field.Name] = n
			}
		default:
			args[field.Name] = v
		}
	}
	if len(args) == 0 {
		args = nil
	}
	return args, eur, nil
}

// Press is the resolution a human's press of answer a makes on facts f,
// every field at values' entry or else its default, and note: the words an
// agent offered lead the note, a money field goes to the fund's or the
// raise's amount, and an overrule drops the review's findings with the
// note as its reason. A required field left empty refuses the press,
// saying what to write.
func (a Answer) Press(f Facts, values map[string]string, note string) (Resolution, error) {
	defaults := a.Defaults(f)
	f.CaseFacts = f.atCap()
	get := func(name string) string {
		if v := values[name]; strings.TrimSpace(v) != "" {
			return v
		}
		return defaults[name]
	}
	for _, field := range a.Fields {
		v := get(field.Name)
		if field.Name == noteField {
			v = cmpNote(v, note)
		}
		switch {
		case field.Required && strings.TrimSpace(v) == "" && field.Kind == FieldCheck:
			return Resolution{}, errors.New(field.Hint)
		case field.Required && strings.TrimSpace(v) == "":
			return Resolution{}, fmt.Errorf("%s is empty: %s", field.Label, field.Hint)
		}
	}
	args, eur, err := ArgsOf(a, get)
	if err != nil {
		return Resolution{}, err
	}
	res := Resolution{Action: a.Action, Args: args, Note: strings.TrimSpace(cmpNote(get(noteField), note))}
	switch {
	case a.Variant == sayVariant:
		res.Note = strings.TrimSpace(a.Label + "\n\n" + res.Note)
	case a.Action == ActFund || a.Action == ActRaiseCeiling || a.Action == AnswerFundChat:
		if eur > 0 {
			if res.Args == nil {
				res.Args = map[string]any{}
			}
			res.Args["eur"] = eur
		}
		if need := raiseFund(f.CaseFacts); a.Action == ActRaiseCeiling && need > 0 {
			res.Fund = &policy.Draw{EUR: need}
		}
	case a.Action == ActReverify && a.Variant == variantOverrule:
		res.Verdicts = Overruled(f.CaseFacts, res.Note)
	}
	return res, nil
}

func cmpNote(field, note string) string {
	if strings.TrimSpace(note) != "" {
		return note
	}
	return field
}

// Overruled are the drop verdicts a human's overrule of the review carries
// on facts f: one on each finding the head's newest review raised, the
// human's reason its evidence; nil when the review raised none.
func Overruled(f CaseFacts, reason string) map[string]CheckVerdict {
	var out map[string]CheckVerdict
	for _, id := range f.ReviewFindings {
		if text := f.FindingTexts[id]; text != "" {
			if out == nil {
				out = map[string]CheckVerdict{}
			}
			out[text] = CheckVerdict{State: CheckDropped, Evidence: "the human overruled the review: " + reason}
		}
	}
	return out
}

// stepNames are the steps of a card its helper tasks are, as a line
// names them.
var stepNames = map[string]string{SourceScreening: "the screening", SourceSizing: "the sizing", SourceProver: "the proof writing"}

// HelperStep is the step of its parent a helper task of source is, as a
// line names it ("the sizing"); "" for a source of no helper.
func HelperStep(source string) string { return stepNames[source] }

// theCard finds "the card" and "this card", not "the cards"; aSession
// and nextSession the session an effect line starts.
var (
	theCard     = regexp.MustCompile(`\b(?:[Tt]his|[Tt]he) card\b`)
	aSession    = regexp.MustCompile(`\ba session\b`)
	nextSession = regexp.MustCompile(`\bthe next session\b`)
)

// stepWords is s with "the card" said as step: a helper's ask stands on
// its parent's card, where "the card" reads as the parent. Quoted words
// stay as written.
func stepWords(s, step string) string {
	parts := strings.Split(s, "\u201c")
	for i, p := range parts {
		quoted, rest := "", p
		if i > 0 {
			q, after, ok := strings.Cut(p, "\u201d")
			if !ok {
				continue
			}
			quoted, rest = q+"\u201d", after
		}
		parts[i] = quoted + theCard.ReplaceAllStringFunc(rest, func(m string) string {
			if m[0] == 'T' {
				return capitalize(step)
			}
			return step
		})
	}
	return strings.Join(parts, "\u201c")
}

// ofStep is face as the page of a helper's ask on its parent: its ask,
// why line and story name the step where they name the card. Its planned
// line is the helper's title, whose "the card" is the parent.
func (face AskFace) ofStep(step string) AskFace {
	face.Ask, face.Why = stepWords(face.Ask, step), stepWords(face.Why, step)
	face.Story.Happened, face.Story.Left = stepWords(face.Story.Happened, step), stepWords(face.Story.Left, step)
	return face
}
