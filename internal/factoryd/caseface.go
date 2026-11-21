package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Face is one case as the board shows it: the ask, why, the story, the
// agent's view, the suggestion with the one who made it and its grounds,
// the answers, the suggested one named in Preselected with the args its
// proposal names, and the findings the ask and the story count. Money is
// the money line an approval adds.
type Face struct {
	Ask, Money, Why, Info, Recommendation, Source string
	Story                                         Story
	Offers                                        []AskOffer
	Preselected                                   string
	PreselectedArgs                               map[string]any
	Findings                                      []string
}

// CaseAsk is the newest question_asked of a case: when it was asked and
// its detail, which holds the face the register wrote and the agent's view,
// and the project agent's newest analysis since the case opened.
type CaseAsk struct {
	At       time.Time
	Detail   map[string]any
	Analysis string
}

// CaseFace is case c's face on card t: the face its newest ask stored,
// with the lines the case writer wrote after it, or for a case no ask
// stored one, the register's on the case's own facts.
func CaseFace(c Case, t Task, ask CaseAsk) Face {
	af, ok := storedFace(ask.Detail)
	if !ok {
		af = FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: t, Recommend: RecommendationOf(ask.Detail)})
	}
	if slices.Contains(storyWriterKinds, c.Kind) {
		af.Story = writerLines(af.Story, c).without(af.Ask, af.Why)
	}
	face := Face{Ask: af.Ask, Why: af.Why, Story: af.Story, Offers: offersOfAnswers(af.Answers), Preselected: af.Suggested, PreselectedArgs: af.SuggestedArgs,
		Source: af.SuggestedBy, Recommendation: af.Grounds, Findings: af.Findings}
	face.Info, _ = ask.Detail["info"].(string)
	if ask.Analysis != "" {
		face.Info = strings.TrimSpace(face.Info + "\n\n" + Quoted(ask.Analysis))
	}
	return face
}

// storedFace is the face an ask row's detail stores, false for a row the
// register wrote none on.
func storedFace(detail map[string]any) (AskFace, bool) {
	if _, ok := detail["story"]; !ok {
		return AskFace{}, false
	}
	var row struct {
		Ask, Why string
		Story    Story
		Offers   []AskOffer
		Findings []string
	}
	b, err := json.Marshal(detail)
	if err != nil || json.Unmarshal(b, &row) != nil {
		return AskFace{}, false
	}
	face := AskFace{Ask: row.Ask, Why: row.Why, Story: row.Story, Findings: row.Findings}
	for _, o := range row.Offers {
		a, _ := AnswerOf(o.Value)
		a.Label, a.Effect = o.Label, o.Effect
		if a.Action == "" {
			a.Action = o.Value
		}
		face.Answers = append(face.Answers, a)
	}
	r := RecommendationOf(detail)
	face.Suggested, face.SuggestedBy, face.Grounds, face.SuggestedArgs = r.Action, r.By, shownGrounds(r.Grounds), r.Args
	return face, true
}

// AskOffers are the choices a human card's ask recorded: the offers that
// carry no effect line, as the card was filed, and the choices of a face
// stored on the row again (SetCaseFace), which carry their effect.
func AskOffers(detail map[string]any) []QuestionChoice {
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
		if v, stored := strings.CutPrefix(o.Value, choicePrefix); stored || o.Effect == "" {
			out = append(out, QuestionChoice{Value: v, Label: o.Label})
		}
	}
	return out
}

// CaseDesk is what a board read needs of one repository's cases: its live
// cases, oldest first, and the newest ask of each, read in two queries
// however many cards wait.
type CaseDesk struct {
	Cases []Case
	Asks  map[int64]CaseAsk
}

// CaseDesk reads repository's CaseDesk.
func (s *PGStore) CaseDesk(ctx context.Context, repository string) (CaseDesk, error) {
	var d CaseDesk
	var err error
	if d.Cases, err = s.OpenCasesIn(ctx, repository); err != nil {
		return d, err
	}
	if d.Asks, err = s.caseAsks(ctx, repository); err != nil {
		return d, err
	}
	return d, s.caseAnalyses(ctx, repository, d.Asks)
}

// caseAnalyses adds to asks the project agent's newest analysis of each
// live case of repository, one recorded on its card since the case opened
// that names the case by its id (AttachAnalysis).
func (s *PGStore) caseAnalyses(ctx context.Context, repository string, asks map[int64]CaseAsk) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT ON (c.id) c.id, e.detail
		FROM cases c JOIN events e ON e.task_id = c.task_id AND e.kind = $2 AND e.at >= c.opened_at
		AND e.detail->>'case_id' = c.id::text
		WHERE c.repository = $1 AND c.`+caseLive+` ORDER BY c.id, e.id DESC`, repository, EventAnalysisAttached)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			return err
		}
		a := asks[id]
		a.Analysis = analysisLine(detail)
		asks[id] = a
	}
	return rows.Err()
}

// analysisLine is an analysis_attached row's words as the card shows
// them: what the project agent found, and what it recommends.
func analysisLine(detail map[string]any) string {
	found, _ := detail["found"].(string)
	line := "The project agent found: " + strings.TrimSpace(found)
	if rec, _ := detail["recommendation"].(string); strings.TrimSpace(rec) != "" {
		line += "\n\nIt recommends: " + strings.TrimSpace(rec)
	}
	return line
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

// questionOfAsk is the five-part question of a question_asked row's detail,
// nil for an ask whose face no template shaped.
func questionOfAsk(detail map[string]any) *HumanQuestion {
	ask, _ := detail["ask"].(string)
	if ask == "" {
		return nil
	}
	q := &HumanQuestion{Ask: ask, Choices: AskOffers(detail)}
	q.Why, _ = detail["why"].(string)
	q.Info, _ = detail["info"].(string)
	q.Recommendation, _ = detail["recommendation"].(string)
	q.Source, _ = detail["recommended_by"].(string)
	q.Happened, _ = detail["happened"].(string)
	q.Waits, _ = detail["waits"].(string)
	q.Check, _ = detail["check"].(string)
	return q
}
