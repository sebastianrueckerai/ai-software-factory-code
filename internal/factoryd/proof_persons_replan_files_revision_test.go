package factoryd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// recutRig is plan p (issue 10, done, its replan named its next revision
// and its planner filed it, criteria unsettled) with its re-cut
// integration card cardID (issue 11, revision 1) in review holding pull
// request 12, open and green through the fake forge.
func recutRig(t *testing.T, cardID string) (*Runner, *fakeStore, *prForgejo) {
	t.Helper()
	fj := gateGreenForge()
	r, store, _ := newActivityRunner(t, Task{ID: "p", Kind: "agent", Track: policy.TrackPlan, Source: "webhook", Repository: "sf/repo",
		IssueNumber: 10, Status: "done", BudgetEUR: 5, Usage: TaskUsage{Attempts: 1}}, fj)
	b, err := json.Marshal(criteriaPlan())
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{"sha-1", "sha-2"} {
		if _, err := store.AddPlanRevision(context.Background(), "p", b, sha); err != nil {
			t.Fatal(err)
		}
	}
	c := Task{ID: cardID, Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "p", Repository: "sf/repo",
		PlanKey: "integration", PlanRevision: 1, IssueNumber: 11, Status: "in-review", PRNumber: 12, SHA: "c7"}
	if err := store.CreateTask(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return r, store, fj
}

// TestPersonsReplanFilesRevision: a person presses Replan naming a card
// that holds the plan's criteria-gate trigger: the card stops, and the
// replan the press cut files the change it named — the named revision is
// recorded before the card's end, and the press's note names the revision.
func TestPersonsReplanFilesRevision(t *testing.T) {
	r, store, _ := recutRig(t, "t1")
	note, err := r.Replan(context.Background(), "t1", ReplanAsk{By: "ana", Comment: "re-file the integration"})
	if err != nil {
		t.Fatalf("Replan naming the card: %v", err)
	}
	if c := store.tasks["t1"]; c.Status != "closed" {
		t.Fatalf("the card %s, want the person's replan to stop it", c.Status)
	}
	revs := store.plans["p"]
	if len(revs) != 2 {
		t.Fatalf("the plan's revisions %+v, want the replan's named revision filed", revs)
	}
	filed := revs[len(revs)-1]
	if filed.Revision != 2 {
		t.Fatalf("the filed revision %+v, want revision 2", filed)
	}
	if !strings.Contains(note, "revision") {
		t.Fatalf("the press's note %q, want it to name the revision the replan files", note)
	}
	steps := replanSteps(t, r, "p")
	if len(steps) != 1 {
		t.Fatalf("the plan's replan steps %+v, want the press's one step", steps)
	}
	// the named revision is recorded before the card's end
	fin := stepsNamed(t, r, "t1", "final")
	if len(fin) != 1 {
		t.Fatalf("the card's final steps: %d, want one", len(fin))
	}
	var end struct {
		Time time.Time `json:"time"`
	}
	if err := json.Unmarshal(fin[0], &end); err != nil {
		t.Fatal(err)
	}
	if filed.CreatedAt.After(end.Time) {
		t.Fatalf("revision 2 filed at %v, the card's end at %v; want the filing first", filed.CreatedAt, end.Time)
	}
}
