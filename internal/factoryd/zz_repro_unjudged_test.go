package factoryd

import (
	"context"
	"testing"
)

func TestReproFinishClosesHumanAlerts(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
	task := prTask
	task.Status = "in-review"
	r, store, _ := newActivityRunner(t, task, fj)
	ctx := context.Background()
	store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "t1"})
	store.InsertAlert(ctx, Alert{Kind: AlertInfoRequest, Source: "agent", TaskID: "t1"})
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "pull request closed without merge"); err != nil {
		t.Fatal(err)
	}
	for _, a := range store.alertRows() {
		if a.Verdict == "" || a.Verdict != "unjudged" || a.ResolvedBy != "factoryd" || a.ResolvedAt == nil {
			t.Fatalf("task %s %s alert ended verdict %q resolved_by %q resolved_at %v; want \"unjudged\" by factoryd at its task's end", a.TaskID, a.Kind, a.Verdict, a.ResolvedBy, a.ResolvedAt)
		}
	}
}
