package console

import (
	"context"
	"log"
)

// alertsLimit bounds the alerts one chat read shows.
const alertsLimit = 200

// alertRepository is the repository of alert id's task and the task's id,
// both "" when the alert, its task or their repository cannot be read.
func (c *Console) alertRepository(ctx context.Context, id int64) (repo, taskID string) {
	a, ok, err := c.cfg.Alerts.AlertByID(ctx, id)
	if err != nil {
		log.Printf("console: alert %d: %v", id, err)
		return "", ""
	}
	if !ok || a.TaskID == "" {
		return "", ""
	}
	t, ok, err := c.cfg.Tasks.Task(ctx, a.TaskID)
	if err != nil {
		log.Printf("console: alert %d: task %s: %v", id, a.TaskID, err)
		return "", ""
	}
	if !ok {
		return "", ""
	}
	return t.Repository, a.TaskID
}
