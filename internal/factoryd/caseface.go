package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Face is the five parts of one case as the human reads them: the ask, the
// money line beneath it, why, the info and the last answer's time, the
// recommendation with the one who wrote it, and the premade answers, the one a rung proposed named in Preselected.
type Face struct {
	Ask, Money, Why, Info, Recommendation, Source string
	LastAnswerAt                                  *time.Time
	Choices                                       []QuestionChoice
	Offers                                        []AskOffer
	Refusals                                      []ActionRefusal
	Preselected                                   string
}

// CaseAsk is the newest question_asked of a case: when it was asked and
// its detail, which holds the ask's info, its offers and what rung 2
// preselected.
type CaseAsk struct {
	At     time.Time
	Detail map[string]any
}

// CaseFace is case c's face, read from its record: its steps, the facts of
// its card, its newest ask and when a human last answered a case of the
// card. The money line is the ask only for a budget floor with no finding
// and green checks, where money is the one matter.
func CaseFace(c Case, steps []CaseStep, f CaseFacts, ask CaseAsk, answered *time.Time) Face {
	face := Face{Ask: c.Problem, Why: caseWhy(c, f, steps), LastAnswerAt: answered}
	if c.Money != nil {
		face.Money = c.Money.line()
		if c.Kind == CaseBudgetFloor && len(f.Findings) == 0 && f.Checks == "green" {
			face.Ask, face.Money = face.Money, ""
		}
	}
	face.Info = cmp.Or(c.Brief, askInfo(ask))
	if answered != nil {
		face.Info = strings.TrimSpace(face.Info + "\n\nLast answer processed: " + answered.UTC().Format("2006-01-02 15:04") + " UTC")
	}
	if by, _ := ask.Detail["recommended_by"].(string); by != "" {
		face.Recommendation, _ = ask.Detail["recommendation"].(string)
		face.Source = by
		if action := rungAction(steps); action != "" && c.BriefDetail != "" {
			face.Recommendation = action + ": " + c.BriefDetail
		}
	}
	face.Choices = askOffers(ask.Detail)
	face.Offers, face.Refusals = askTableOffers(ask.Detail), askRefusals(ask.Detail)
	if p, ok := ask.Detail["preselected"].(map[string]any); ok {
		face.Preselected, _ = p["action"].(string)
	}
	return face
}

func askInfo(ask CaseAsk) string {
	info, _ := ask.Detail["info"].(string)
	return info
}

// rungAction is the action the newest step that a rung handed up with a
// decision recommended, "" when none did.
func rungAction(steps []CaseStep) string {
	for _, s := range newestFirst(steps) {
		if s.Outcome == StepHandedUp && s.Rung >= 1 && s.Rung < 3 && s.Decision != "" && s.Decision != CheckHuman {
			return s.Decision
		}
	}
	return ""
}

func newestFirst(steps []CaseStep) []CaseStep {
	out := slices.Clone(steps)
	slices.Reverse(out)
	return out
}

// line is the money fact as one sentence.
func (m CaseMoney) line() string {
	s := fmt.Sprintf("%.2f of %.2f EUR spent", m.SpentEUR, m.BudgetEUR)
	if m.NeedEUR > 0 {
		s += fmt.Sprintf("; the next step needs %.2f EUR", m.NeedEUR)
		if m.Buys != "" {
			s += " for " + m.Buys
		}
	}
	return s
}

// caseWhy is the kind's template sentence for the case's reason, or the
// kind in words where the case holds no reason, then the first two
// sentences of what the newest handed-up step found.
func caseWhy(c Case, f CaseFacts, steps []CaseStep) string {
	var why string
	if reason, _ := c.Facts["reason"].(string); reason != "" {
		why = ShapedQuestion(reason, questionFactsOf(f), nil).Why
	} else if class, _ := c.Facts["class"].(string); c.Kind == CaseMergeApproval && class != "" {
		why = fmt.Sprintf("The merge gate classes the change %s: a human decides its merge.", class)
	} else {
		words, ok := caseKindWords[c.Kind]
		why = "The case is the " + cmp.Or(words, "open problem") + "."
		if !ok {
			why = "The case is " + strings.ReplaceAll(string(c.Kind), "_", " ") + "."
		}
	}
	for _, s := range newestFirst(steps) {
		if s.Outcome == StepHandedUp && strings.TrimSpace(s.Found) != "" {
			return strings.TrimSpace(why + " " + firstSentences(s.Found, 2))
		}
	}
	return why
}

func questionFactsOf(f CaseFacts) QuestionFacts {
	return QuestionFacts{PlanTrack: f.Planner, SessionCap: f.SessionCap, Attempts: f.Attempts, BudgetEUR: f.BudgetEUR,
		SpentEUR: f.SpentEUR, LeftEUR: f.LeftEUR, ReserveEUR: f.ReserveEUR, FloorEUR: f.FloorEUR, PR: f.PR}
}

// firstSentences is the first n sentences of s on one line: a sentence ends
// at ".", "!" or "?" before a space or the end.
func firstSentences(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(".!?", s[i]) < 0 || (i+1 < len(s) && s[i+1] != ' ') {
			continue
		}
		if n--; n == 0 {
			return s[:i+1]
		}
	}
	return s
}

// askOffers are the premade answers the ask recorded: the offers that carry no
// effect line; the table's actions among them are askTableOffers.
func askOffers(detail map[string]any) []QuestionChoice {
	raw, ok := detail["offers"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var all []AskOffer
	if json.Unmarshal(b, &all) != nil {
		return nil
	}
	var out []QuestionChoice
	for _, o := range all {
		if o.Effect == "" {
			out = append(out, QuestionChoice{Value: o.Value, Label: o.Label})
		}
	}
	return out
}

// CaseDesk is what a board read needs of one repository's cases: its live
// cases, oldest first, the steps and newest ask of each, and when a human
// last answered a case of each card, read in four queries however many
// cards wait.
type CaseDesk struct {
	Cases    []Case
	Steps    map[int64][]CaseStep
	Asks     map[int64]CaseAsk
	Answered map[string]time.Time
}

// CaseDesk reads repository's CaseDesk.
func (s *PGStore) CaseDesk(ctx context.Context, repository string) (CaseDesk, error) {
	var d CaseDesk
	var err error
	if d.Cases, err = s.OpenCasesIn(ctx, repository); err != nil {
		return d, err
	}
	steps, err := s.caseSteps(ctx, `SELECT `+caseStepColumns+` FROM case_steps WHERE case_id IN (SELECT id FROM cases WHERE repository = $1 AND `+caseLive+`) ORDER BY id`, repository)
	if err != nil {
		return d, err
	}
	d.Steps = map[int64][]CaseStep{}
	for _, st := range steps {
		d.Steps[st.CaseID] = append(d.Steps[st.CaseID], st)
	}
	if d.Asks, err = s.caseAsks(ctx, repository); err != nil {
		return d, err
	}
	d.Answered, err = s.caseAnswers(ctx, repository)
	return d, err
}

// caseAsks are the newest question_asked of each live case of repository,
// by case id: every ask names the case it opened or went to.
func (s *PGStore) caseAsks(ctx context.Context, repository string) (map[int64]CaseAsk, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT ON (c.id) c.id, e.at, e.detail
		FROM cases c JOIN events e ON e.task_id = c.task_id AND e.kind = $2
		AND e.detail->>'case_id' = c.id::text
		WHERE c.repository = $1 AND c.`+caseLive+` ORDER BY c.id, e.id DESC`, repository, EventQuestionAsked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	asks := map[int64]CaseAsk{}
	for rows.Next() {
		var id int64
		var a CaseAsk
		var detail []byte
		if err := rows.Scan(&id, &a.At, &detail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &a.Detail); err != nil {
			return nil, err
		}
		asks[id] = a
	}
	return asks, rows.Err()
}

// caseAnswers is when a human last resolved a case of each card of
// repository, by task id.
func (s *PGStore) caseAnswers(ctx context.Context, repository string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, max(resolved_at) FROM cases
		WHERE repository = $1 AND status = $2 AND resolution->>'rung' = '3' AND resolved_at IS NOT NULL GROUP BY task_id`, repository, CaseResolved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// askRefusals are the refusals the ask recorded beside its offers.
func askRefusals(detail map[string]any) []ActionRefusal {
	b, err := json.Marshal(detail["refusals"])
	if err != nil {
		return nil
	}
	var out []ActionRefusal
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// askTableOffers are the table actions the ask stored as the human's buttons;
// an ask whose offers are a question's choices carries no effect line and
// stores none of these.
func askTableOffers(detail map[string]any) []AskOffer {
	b, err := json.Marshal(detail["offers"])
	if err != nil {
		return nil
	}
	var out []AskOffer
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return slices.DeleteFunc(out, func(o AskOffer) bool { return o.Effect == "" })
}

// questionOfAsk is the five-part question of a question_asked row's detail,
// nil for an ask whose face no template shaped.
func questionOfAsk(detail map[string]any) *HumanQuestion {
	ask, _ := detail["ask"].(string)
	if ask == "" {
		return nil
	}
	q := &HumanQuestion{Ask: ask, Choices: askOffers(detail)}
	q.Why, _ = detail["why"].(string)
	q.Info, _ = detail["info"].(string)
	q.Recommendation, _ = detail["recommendation"].(string)
	q.Source, _ = detail["recommended_by"].(string)
	q.Cap, _ = detail["cap"].(string)
	return q
}
