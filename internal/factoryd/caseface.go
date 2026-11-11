package factoryd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Face is the five parts of one case as the human reads them: the ask, the
// money line beneath it, why, the digest and the last answer's time, the
// recommendation with its source, and the premade answers, the one the
// rung proposed named in Preselected.
type Face struct {
	Ask, Money, Why, Info, Recommendation, Source string
	LastAnswerAt                                  *time.Time
	Choices                                       []QuestionChoice
	Preselected                                   string
}

// CaseFace is case c's face, read from its record: its steps, the facts of
// its card and the card's digest. The money line is the ask only for a
// budget floor with no finding and green checks, where money is the one
// matter.
func CaseFace(c Case, steps []CaseStep, f CaseFacts, digest CardDigest) Face {
	face := Face{Ask: c.Problem, Why: caseWhy(c, f, steps)}
	if c.Money != nil {
		face.Money = c.Money.line()
		if c.Kind == CaseBudgetFloor && len(f.Findings) == 0 && f.Checks == "green" {
			face.Ask, face.Money = face.Money, ""
		}
	}
	if len(digest.Steps) > 0 || digest.Head.Issue > 0 {
		face.Info = strings.TrimSpace(digest.Render())
	}
	if at := lastAnswerAt(digest); at != nil {
		face.LastAnswerAt = at
		face.Info = strings.TrimSpace(face.Info + "\nLast answer processed: " + at.UTC().Format("2006-01-02 15:04") + " UTC")
	}
	recommend := func(rung int) bool {
		for _, s := range newestFirst(steps) {
			if s.Rung == rung && s.Outcome == StepHandedUp && s.Decision != "" && s.Decision != CheckHuman {
				face.Recommendation = s.Decision + ": " + s.Grounds
				return true
			}
		}
		return false
	}
	switch {
	case recommend(2):
		face.Source = "the factory agent"
	case recommend(1):
		face.Source = "the check-in"
	}
	face.Choices = factsOffers(c.Facts)
	if p, ok := c.Facts["preselected"].(map[string]any); ok {
		face.Preselected, _ = p["action"].(string)
	}
	return face
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

// factsOffers are the premade answers the case's ask recorded.
func factsOffers(facts map[string]any) []QuestionChoice {
	raw, ok := facts["offers"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []QuestionChoice
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// lastAnswerAt is when the newest case of the card that a human answered
// ended, nil for none.
func lastAnswerAt(d CardDigest) *time.Time {
	var at *time.Time
	for _, c := range d.cases {
		if c.Status == CaseResolved && c.ResolvedAt != nil && (at == nil || c.ResolvedAt.After(*at)) {
			at = c.ResolvedAt
		}
	}
	return at
}
