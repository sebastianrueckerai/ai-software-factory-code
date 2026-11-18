package acceptance

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat-answer rig: one live case with an in-flight rung step and its
// escalation row (EventQuestionAsked) in a member's thread, beside a
// human card's open choice case (ChoiceKind), both answered through
// POST /projects/{name}/chat/answer — 204, the member's answer on
// the case's record, and the case's ending as the real Act ends it.

// seedAsk stands the escalation of question on card taskID: its
// question_asked row on the spine, then the escalation row it wrote in a
// member's thread, the member's choices as buttons. It answers the
// message's id.
func (a *AcceptanceRig) seedAsk(t *testing.T, taskID string, issue int64, question string, choices ...string) int64 {
	t.Helper()
	card, ok, err := a.Task(t.Context(), taskID)
	if err != nil || !ok {
		t.Fatalf("the seeded card %s: %v, %v", taskID, ok, err)
	}
	if _, err := a.Record(t.Context(), factoryd.Event{Repository: card.Repository, TaskID: taskID, IssueNumber: issue,
		Kind: factoryd.EventQuestionAsked, Actor: "the factory",
		Detail: map[string]any{"question": question}}); err != nil {
		t.Fatal(err)
	}
	id, err := a.AddChatMessage(t.Context(), card.Repository, "sebastian", factoryd.ChatMessage{
		Role: "assistant", Kind: factoryd.MessageEscalation, BirthKind: factoryd.EventQuestionAsked,
		Content: question, TaskID: taskID, Choices: choices})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// SeedRungCase stands one live case on the waiting card taskID, with an
// in-flight rung 3 step due inside the rung answer bound, and its
// escalation row in sebastian's thread. It answers the row's message id
// and the case.
func (a *AcceptanceRig) SeedRungCase(t *testing.T, taskID string, issue int64, question string, choices ...string) (int64, factoryd.Case) {
	t.Helper()
	a.mu.Lock()
	a.caseNext++
	k := factoryd.Case{ID: a.caseNext, TaskID: taskID, Repository: "sf/code", Kind: factoryd.CaseGateStop,
		Status: factoryd.CaseAtHuman, Problem: question}
	now := time.Now()
	due := now.Add(policy.DefaultWaits.RungAnswer)
	a.cases[taskID] = append(a.cases[taskID], k)
	a.caseSteps[k.ID] = []factoryd.CaseStep{{ID: k.ID, CaseID: k.ID, Rung: 3, Actor: "rung 3",
		StartedAt: now, DueAt: &due}}
	a.mu.Unlock()
	return a.seedAsk(t, taskID, issue, question, choices...), k
}

// SeedChoiceCase stands a human card in Needs you with its one open
// choice case (ChoiceKind) and the escalation row in sebastian's thread.
// It answers the card's id, the row's message id and the case.
func (a *AcceptanceRig) SeedChoiceCase(t *testing.T, question string, choices []factoryd.QuestionChoice) (string, int64, factoryd.Case) {
	t.Helper()
	const taskID = "t-h"
	a.mu.Lock()
	a.tasks = append(a.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: taskID, Kind: "human", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 21, Objective: question},
		Question: &factoryd.HumanQuestion{Ask: question, Choices: choices}})
	a.caseNext++
	k := factoryd.Case{ID: a.caseNext, TaskID: taskID, Repository: "sf/code", Kind: factoryd.CaseHumanCard,
		Status: factoryd.CaseAtHuman, Problem: question}
	a.cases[taskID] = append(a.cases[taskID], k)
	a.mu.Unlock()
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.Label
	}
	return taskID, a.seedAsk(t, taskID, 21, question, labels...), k
}

// pressAnswer presses choice on the member's row id in thread 1 through
// POST /projects/{name}/chat/answer and answers the recorder.
func (a *AcceptanceRig) pressAnswer(t *testing.T, path string, ck *http.Cookie, csrf string, id int64, choice string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"conversation": {"1"}, "message": {strconv.FormatInt(id, 10)}, "choice": {choice}, "csrf": {csrf}}
	return a.Post(path+"/answer", ck, form)
}

// CaseOf reads a seeded case by its id through the rig's own seam,
// open or closed (CaseByID): the zero Case with ErrNotFound when no
// case of the id stands.
func (a *AcceptanceRig) CaseOf(t *testing.T, caseID int64) (factoryd.Case, error) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cs := range a.cases {
		for _, k := range cs {
			if k.ID == caseID {
				return k, nil
			}
		}
	}
	return factoryd.Case{}, fmt.Errorf("case %d: %w", caseID, factoryd.ErrNotFound)
}

// OpenCases reads taskID's open cases (OpenCasesOf): the cases the
// expiry and an answer on the card act on.
func (a *AcceptanceRig) OpenCases(t *testing.T, taskID string) []factoryd.Case {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var open []factoryd.Case
	for _, k := range a.cases[taskID] {
		if k.Status == factoryd.CaseOpen || k.Status == factoryd.CaseAtHuman {
			open = append(open, k)
		}
	}
	return open
}

// StepsOf reads case caseID's steps (CaseSteps), in progress or ended.
func (a *AcceptanceRig) StepsOf(t *testing.T, caseID int64) []factoryd.CaseStep {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.caseSteps[caseID])
}

// AskRows reads the question rows asked of taskID (Events of kind
// EventQuestionAsked): the asks an answer answers, and the read whose
// count proves the expiry filed no identical ask again.
func (a *AcceptanceRig) AskRows(t *testing.T, taskID string) []factoryd.Event {
	t.Helper()
	var rows []factoryd.Event
	for _, e := range a.Events() {
		if e.TaskID == taskID && e.Kind == factoryd.EventQuestionAsked {
			rows = append(rows, e)
		}
	}
	return rows
}

// expireStep passes the in-progress step of case caseID its due: the
// rung answer bound is over, the state the expiry acts on.
func (a *AcceptanceRig) expireStep(t *testing.T, caseID int64) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	steps := a.caseSteps[caseID]
	for i := range steps {
		if steps[i].EndedAt == nil {
			past := time.Now().Add(-time.Second)
			steps[i].DueAt = &past
		}
	}
	a.caseSteps[caseID] = steps
}

// pressBoardAnswer presses the board's answer button: choice value on
// the live ask caseID of card taskID through POST /tasks/{id}/answer,
// the route that rides doAnswerValue as the chat press does.
func (a *AcceptanceRig) pressBoardAnswer(t *testing.T, taskID string, ck *http.Cookie, csrf string, caseID int64, value string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"project": {accProjectName}, "case": {strconv.FormatInt(caseID, 10)}, "value": {value}, "csrf": {csrf}}
	return a.Post("/tasks/"+taskID+"/answer", ck, form)
}

// TestBoardAnswerRidesOneMapping is the board's answer button end to
// end: the press of a substantive choice on the human card's open
// choice case answers 303 and reaches Act as the member with the
// answer on the case's record, the one mapping the chat press rides.
func TestBoardAnswerRidesOneMapping(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	taskID, _, k := r.SeedChoiceCase(t, "Keep the fund's draw?", []factoryd.QuestionChoice{
		{Value: factoryd.AnswerGo, Label: "Go on"}, {Value: factoryd.AnswerPark, Label: "Leave it waiting"}})
	rec := r.pressBoardAnswer(t, taskID, ck, csrf, k.ID, factoryd.AnswerGo)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the board press = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	acted := r.Acted()
	if len(acted) != 1 || acted[0].TaskID != taskID || acted[0].CaseID != k.ID || acted[0].By != "sebastian" {
		t.Fatalf("Act took %+v, want the board press as sebastian on the card's case", acted)
	}
	var answered []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventQuestionAnswered {
			answered = append(answered, e)
		}
	}
	if len(answered) != 1 || answered[0].TaskID != taskID || answered[0].Actor != "sebastian" ||
		answered[0].Detail["answer"] != "Go on" || answered[0].Detail["case_id"] != k.ID {
		t.Fatalf("the case's record = %+v, want the member's answer on the case", answered)
	}
}

// TestExpiredRungCaseStillReadsOpen is the reader contract: a seeded
// rung case whose step's due has passed the rung answer bound reads
// open with its step in progress and its one ask row — the state the
// expiry acts on, and what a park's tests read to prove no open step
// remains after a press.
func TestExpiredRungCaseStillReadsOpen(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	_, k := r.SeedRungCase(t, "t-e", 13, "#13 Use a replica? The rung stopped the card.", "Go on", "Park it")
	r.expireStep(t, k.ID)
	open := r.OpenCases(t, "t-e")
	if len(open) != 1 || open[0].ID != k.ID {
		t.Fatalf("OpenCasesOf = %+v, want the seeded case %d still open", open, k.ID)
	}
	got, err := r.CaseOf(t, k.ID)
	if err != nil || got.Status != factoryd.CaseAtHuman {
		t.Fatalf("CaseByID = %+v, %v; want the case at the human", got, err)
	}
	steps := r.StepsOf(t, k.ID)
	if len(steps) != 1 || steps[0].EndedAt != nil || steps[0].DueAt == nil || !steps[0].DueAt.Before(time.Now()) {
		t.Fatalf("CaseSteps = %+v, want one step in progress with its due passed", steps)
	}
	rows := r.AskRows(t, "t-e")
	if len(rows) != 1 {
		t.Fatalf("Events = %d question rows, want the one ask", len(rows))
	}
}

// TestChatAnswerSettlesRungCase is the live rung case: the press of a
// choice on the member's escalation row answers 204, Act takes the
// member's answer on the case at rung 3 via chat, and the case's record
// carries the answer.
func TestChatAnswerSettlesRungCase(t *testing.T) {
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	id, k := r.SeedRungCase(t, "t-e", 13, "#13 Use a replica? The rung stopped the card.", "Go on", "Park it")
	rec := r.pressAnswer(t, path, ck, csrf, id, "Go on")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the answer = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	acted := r.Acted()
	if len(acted) != 1 {
		t.Fatalf("Act took %d answers, want the one press", len(acted))
	}
	in := acted[0]
	if in.TaskID != "t-e" || in.CaseID != k.ID || in.By != "sebastian" || in.Rung != 3 {
		t.Fatalf("the press rode %+v, want t-e's case %d as sebastian at rung 3", in, k.ID)
	}
	if in.Res.Action != factoryd.ActContinue || in.Res.Note != "Go on" {
		t.Fatalf("the answer's resolution = %+v, want continue with the member's words", in.Res)
	}
	var answered []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventQuestionAnswered {
			answered = append(answered, e)
		}
	}
	if len(answered) != 1 || answered[0].TaskID != "t-e" || answered[0].Actor != "sebastian" ||
		answered[0].Detail["answer"] != "Go on" || answered[0].Detail["case_id"] != k.ID {
		t.Fatalf("the case's record = %+v, want sebastian's answer on the case", answered)
	}
}

// TestChatAnswerSettlesChoiceCase is the human card's own open choice
// case: the press of its choice answers 204 and the member's answer
// lands on the case's record.
func TestChatAnswerSettlesChoiceCase(t *testing.T) {
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	taskID, id, k := r.SeedChoiceCase(t, "Keep the fund's draw?", []factoryd.QuestionChoice{
		{Value: factoryd.AnswerGo, Label: "Go on"}, {Value: factoryd.AnswerPark, Label: "Leave it waiting"}})
	rec := r.pressAnswer(t, path, ck, csrf, id, "Go on")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the answer = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	acted := r.Acted()
	if len(acted) != 1 || acted[0].TaskID != taskID || acted[0].CaseID != k.ID || acted[0].By != "sebastian" {
		t.Fatalf("Act took %+v, want sebastian's press on the card's case", acted)
	}
	var answered []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventQuestionAnswered {
			answered = append(answered, e)
		}
	}
	if len(answered) != 1 || answered[0].Detail["answer"] != "Go on" {
		t.Fatalf("the case's record = %+v, want the member's answer on it", answered)
	}
}

// TestChatAnswerSettlesTheCaseItAnswers is the park's own press: "Park
// it" on the rung case's escalation row settles the case it answers —
// Act takes the settle with the park arg as sebastian, the case resolves
// and its in-flight rung step ends, the card keeps its wait with no open
// case, and the one ask stands asked: the rung answer bound has nothing
// left to expire and re-ask.
func TestChatAnswerSettlesTheCaseItAnswers(t *testing.T) {
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	id, k := r.SeedRungCase(t, "t-e", 13, "#13 Use a replica? The rung stopped the card.", "Go on", "Park it")
	r.mu.Lock()
	for i := range r.tasks {
		if r.tasks[i].ID == "t-e" {
			r.tasks[i].Question = &factoryd.HumanQuestion{Ask: "The rung stopped the card.", Choices: []factoryd.QuestionChoice{
				{Value: factoryd.AnswerGo, Label: "Go on"}, {Value: factoryd.AnswerPark, Label: "Park it"}}}
		}
	}
	r.mu.Unlock()
	rec := r.pressAnswer(t, path, ck, csrf, id, "Park it")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the answer = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	acted := r.Acted()
	if len(acted) != 1 || acted[0].TaskID != "t-e" || acted[0].CaseID != k.ID || acted[0].By != "sebastian" || acted[0].Rung != 3 {
		t.Fatalf("the press rode %+v, want t-e's case %d as sebastian at rung 3", acted, k.ID)
	}
	if res := acted[0].Res; res.Action != factoryd.ActSettle || res.Args["park"] != factoryd.AnswerPark {
		t.Fatalf("the answer's resolution = %+v, want the settle of the park", res)
	}
	var answered []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventQuestionAnswered {
			answered = append(answered, e)
		}
	}
	if len(answered) != 1 || answered[0].Actor != "sebastian" || answered[0].Detail["case_id"] != k.ID ||
		answered[0].Detail["action"] != factoryd.ActSettle || answered[0].Detail["answer"] != "Park it" {
		t.Fatalf("the case's record = %+v, want sebastian's settle of the case", answered)
	}
	got, err := r.CaseOf(t, k.ID)
	if err != nil || got.Status != factoryd.CaseResolved {
		t.Fatalf("the case = %+v, %v; want it resolved by the park", got, err)
	}
	if steps := r.StepsOf(t, k.ID); len(steps) != 1 || steps[0].EndedAt == nil {
		t.Fatalf("the case's steps = %+v, want the in-flight rung step ended", steps)
	}
	if open := r.OpenCases(t, "t-e"); len(open) != 0 {
		t.Fatalf("the card's open cases = %+v, want none: the card keeps its wait", open)
	}
	r.expireStep(t, k.ID)
	if rows := r.AskRows(t, "t-e"); len(rows) != 1 {
		t.Fatalf("%d ask rows stand, want the one ask: no identical ask re-filed", len(rows))
	}
}
