package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// EscalationStore is what the escalation endpoint reads and records.
type EscalationStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
}

// SessionEscalator hands an escalation to a task's running session.
type SessionEscalator interface {
	EscalateSession(taskID, question, reason string) error
}

// IntakeAgentEscalation is the intake event of an agent's escalate_to_human:
// recorded, never signalled (the session's supervision acts on it).
const IntakeAgentEscalation = "agent_escalation"

// maxEscalationText bounds the question and the reason, as the gateway does.
const maxEscalationText = 4000

// EscalationHandler is POST /internal/escalations, the gateway's hand-over
// of an agent's escalate_to_human (behind the gateway's bearer): the task
// must be running, the escalation is recorded as an intake event and its
// running session stops as on a supervisor's escalate: work in progress
// pushed, waiting-human, the question on the issue, the human's answer
// resumes it.
func EscalationHandler(store EscalationStore, sessions SessionEscalator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TaskID   string `json:"task_id"`
			Question string `json:"question"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		q, why := strings.TrimSpace(in.Question), strings.TrimSpace(in.Reason)
		if in.TaskID == "" || q == "" || why == "" || len(q) > maxEscalationText || len(why) > maxEscalationText {
			http.Error(w, fmt.Sprintf("task_id, question and reason are required, question and reason at most %d bytes", maxEscalationText), http.StatusBadRequest)
			return
		}
		t, ok, err := store.Task(r.Context(), in.TaskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no task "+in.TaskID, http.StatusNotFound)
			return
		}
		if t.Status != "running" {
			http.Error(w, "task "+in.TaskID+" is "+t.Status, http.StatusConflict)
			return
		}
		if _, err := store.InsertIntakeEvent(r.Context(), IntakeEvent{
			TaskID: t.ID, Kind: IntakeAgentEscalation, Author: "agent", Body: q + "\n\nWhy: " + why,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := sessions.EscalateSession(t.ID, q, why); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrNoLiveSession) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		log.Printf("task %s: the agent escalated: %s", t.ID, truncate(q, 200))
		w.WriteHeader(http.StatusAccepted)
	}
}
