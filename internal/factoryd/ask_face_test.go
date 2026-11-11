package factoryd

import (
	"slices"
	"time"
)

// face puts a face of ask case caseID of card taskID into the fake as the
// events writer leaves it: the case (at the human, its reason the stop's
// words), a face step carrying comment, and the question_asked row that
// names the case.
func (s *fakeStore) face(taskID string, caseID int64, kind CaseKind, comment int64, body string, at time.Time, choices ...string) {
	i := slices.IndexFunc(s.cases, func(c Case) bool { return c.ID == caseID })
	if i < 0 {
		facts := map[string]any{"reason": body, "face": body}
		if len(choices) > 0 {
			facts["choices"] = choices
		}
		s.cases = append(s.cases, Case{ID: caseID, TaskID: taskID, Kind: kind, Status: CaseAtHuman, OpenedAt: at, Problem: escalationQuestion(body), Facts: facts})
		i = len(s.cases) - 1
	}
	s.events = append(s.events, Event{TaskID: taskID, Kind: EventQuestionAsked, At: at,
		Detail: map[string]any{"case_id": caseID, "comment": comment, "question": escalationQuestion(body)}})
	s.faced(caseID, s.events[len(s.events)-1])
}
