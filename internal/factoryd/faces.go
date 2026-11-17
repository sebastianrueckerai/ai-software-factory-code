package factoryd

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// storyMax bounds each line of a story, in runes.
const storyMax = 240

// FaceSpec is the face of one kind: its ask, why line and story from the
// facts, the answers to its question by key, and the answer it suggests
// with who suggests it and why. A suggestion without grounds, or a nil
// Suggest, suggests nothing; grounds without an answer are the writer's
// view, a story line that names the writer, and show only with one.
type FaceSpec struct {
	Ask     func(Facts) string
	Why     func(Facts) string
	Story   func(Facts) Story
	Answers func(Facts) []string
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
// other answer that holds in More.
type AskFace struct {
	Ask, Why                        string
	Story                           Story
	Answers                         []Answer
	Suggested, SuggestedBy, Grounds string
	More                            []Answer
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
	}
	if s, ok := CaseFaces[c.Kind]; ok {
		return s
	}
	return CaseFaces[CaseOther]
}

// facing is case c on facts f as its face reads them: f holds c, and a
// question of the agent's at a cap is its cap's case (atCap).
func facing(c Case, f Facts) (Case, Facts) {
	f.Case = c
	if f.Kind == "" {
		f.Kind = c.Kind
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
// over four drops to More. A face left with one answer adds "Not now" and
// "End the card", each unless an answer of its action is there.
func FaceOf(c Case, f Facts) AskFace {
	question := cmp.Or(f.Kind, c.Kind)
	c, f = facing(c, f)
	spec := faceSpecOf(c, f)
	face := AskFace{Ask: spec.Ask(f), Why: spec.Why(f)}
	if spec.Story != nil {
		face.Story = spec.Story(f).without(face.Ask, face.Why)
	}
	if f.Kind != question {
		face.Story.Happened = AgentAsks + agentWhole(f)
	}
	var answers []Answer
	for _, key := range spec.Answers(f) {
		if a, ok := answerFor(key, f); ok && answerHolds(a, f) && !slices.ContainsFunc(answers, func(o Answer) bool { return o.Key() == a.Key() || o.Label == a.Label }) {
			answers = append(answers, a)
		}
	}
	lead := func(key, by, grounds string) bool {
		if key == "" {
			return false
		}
		i := slices.IndexFunc(answers, func(o Answer) bool { return o.Key() == key || o.Value == key })
		if i < 0 {
			a, ok := answerFor(key, f)
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
		face.Suggested, face.SuggestedBy, face.Grounds = a.Key(), by, grounds
		return true
	}
	led := lead(f.Recommend.Action, f.Recommend.By, f.Recommend.Grounds)
	if spec.Suggest != nil {
		// words that lead no answer, beside a recommendation or alone, are
		// their writer's view
		if key, by, grounds := spec.Suggest(f); grounds != "" && by != "" && grounds != face.Grounds && (led || key == "" || !lead(key, by, grounds)) {
			face.Story.View = capitalize(by) + "'s view: " + strings.Join(strings.Fields(grounds), " ")
		}
	}
	if len(answers) > mostAnswers {
		face.More = append(face.More, answers[mostAnswers:]...)
		answers = answers[:mostAnswers]
	}
	if len(answers) < 2 {
		for _, key := range []string{ActNote, ActClose} {
			if a, ok := answerFor(key, f); ok && answerHolds(a, f) && !slices.ContainsFunc(answers, func(o Answer) bool { return o.Action == a.Action }) {
				answers = append(answers, a)
			}
		}
	}
	face.Answers = answers
	for _, a := range moreActions(f) {
		if !slices.ContainsFunc(slices.Concat(answers, face.More), func(o Answer) bool {
			return o.Key() == a.Key() || o.Label == a.Label || o.Action == a.Action && o.Effect == a.Effect
		}) {
			face.More = append(face.More, a)
		}
	}
	return face
}

// answerFor is the answer key names on facts f, its effect line filled from
// the facts where the vocabulary leaves it to them.
func answerFor(key string, f Facts) (Answer, bool) {
	if value, ok := strings.CutPrefix(key, choicePrefix); ok {
		return choiceAnswer(value, f)
	}
	a, ok := AnswerOf(key)
	if !ok {
		return Answer{}, false
	}
	if a.Action == ActConfirmAudit {
		a.Fields = slices.Clone(a.Fields)
		for _, text := range strings.Split(auditFindings(f), "\n") {
			if text != "" {
				a.Fields[0].Options = append(a.Fields[0].Options, Option{text, text})
			}
		}
	}
	if fill, ok := effectFills[a.Key()]; ok {
		if e := fill(f); e != "" {
			a.Effect = e
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
	case ChoiceCase(f.Case, f.Card.Kind) && a.Action != ActClose:
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
		if a, ok := answerFor(name, f); ok && answerHolds(a, f) {
			out = append(out, a)
		}
	}
	return out
}

// MoreOf are the other actions of card t, which holds no case, on facts
// f: a replan, more sessions, a word to the running session, the brief,
// the colour and a thread.
func MoreOf(t Task, f Facts) []Answer {
	f.Card = t
	var out []Answer
	for _, key := range []string{ActReplan, ActExtend, AnswerSteer, AnswerEditBrief, AnswerAccent, AnswerThread} {
		a, ok := answerFor(key, f)
		switch {
		case !ok, a.Action == AnswerSteer && !f.Running:
		case answerHolds(a, f):
			out = append(out, a)
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
	f.CaseFacts = f.atCap()
	get := func(name string) string {
		if v := values[name]; strings.TrimSpace(v) != "" {
			return v
		}
		i := slices.IndexFunc(a.Fields, func(x Field) bool { return x.Name == name })
		if i < 0 || a.Fields[i].Default == nil {
			return ""
		}
		return a.Fields[i].Default(f)
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
