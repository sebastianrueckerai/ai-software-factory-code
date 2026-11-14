package acceptance

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat-answer rig: one live case with an in-flight rung step and its
// escalation row (EventQuestionAsked) in a member's thread, beside a
// human card's open choice case (ChoiceCase), both answered through
// POST /projects/{name}/chat/answer — 204, and the member's answer on
// the case's record. The case's ending is the feature card's work.

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
// choice case (ChoiceCase) and the escalation row in sebastian's thread.
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
