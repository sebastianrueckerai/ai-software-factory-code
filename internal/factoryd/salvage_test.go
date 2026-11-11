package factoryd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func quietFlag() Case {
	return Case{ID: 3, TaskID: "t-b", Kind: CaseMonitorFlag, Status: CaseAtHuman,
		Facts: map[string]any{"flag": "wait_over_bound", "wait": "run_quiet"}}
}

func successors(store *fakeStore) []Task {
	var out []Task
	for _, t := range store.tasks {
		if t.Source == SourceSuccessor {
			out = append(out, t)
		}
	}
	return out
}

// A human's salvage on a run_quiet flag terminates the run once, files one
// successor (issue #9: the rig's issues end at #8) and closes card #6 into
// it; the case resolves by the human on rung 3 with salvage, naming the
// successor. A second salvage of the same card files none.
func TestSalvageEndsTheStuckRunAndFilesTheSuccessor(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, wf := newRetryRig(t)
	store.cases = []Case{quietFlag()}
	got, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3, Res: Resolution{Action: ActSalvage}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t-b salvaged by alice"}; !slices.Equal(wf.terminated, want) {
		t.Errorf("terminated %q, want %q", wf.terminated, want)
	}
	succ := successors(store)
	if len(succ) != 1 || succ[0].IssueNumber != 9 || !strings.Contains(succ[0].Objective, "Findings so far:\n- digest: #6") {
		t.Fatalf("successors %+v, want one at #9 carrying the digest as findings", succ)
	}
	if st := store.tasks["t-b"].Status; st != "closed" {
		t.Errorf("card t-b is %s, want closed", st)
	}
	c := got.Case
	if c.Status != CaseResolved || c.Resolution.By != "alice" || c.Resolution.Rung != 3 || c.Resolution.Action != ActSalvage ||
		c.Resolution.Args["successor"] != succ[0].ID || c.Resolution.Args["issue"] != int64(9) {
		t.Errorf("case %+v resolution %+v, want resolved by alice on rung 3 with salvage naming %s", c, c.Resolution, succ[0].ID)
	}
	if want := "Salvaged: the run of card #6 ended, and its successor #9 starts from main's tip."; !strings.Contains(got.Text, want) {
		t.Errorf("text %q lacks %q", got.Text, want)
	}
	if slices.ContainsFunc(wf.signals, func(s string) bool { return strings.Contains(s, SignalCaseResolved) }) {
		t.Errorf("signals %q: a salvaged card's run hears no case_resolved", wf.signals)
	}
	f, err := r.factsAtCase(ctx, store.tasks["t-b"], quietFlag())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.salvage(ctx, store.tasks["t-b"], f, ActInput{TaskID: "t-b", By: "alice", Rung: 3}); err != nil {
		t.Fatal(err)
	}
	if n := len(successors(store)); n != 1 {
		t.Errorf("a second salvage left %d successors, want 1", n)
	}
}

// Rung 2 is refused by rung, and any case of a card whose run lives, other
// than a run_quiet flag, is refused with the table's text; a card whose
// workflow is gone takes the salvage on any case.
func TestSalvageRefusals(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, wf := newRetryRig(t)
	store.cases = []Case{quietFlag(), {ID: 4, TaskID: "t-b", Kind: CaseSessionCap, Status: CaseAtHuman}}
	var refused ErrActRefused
	_, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "factory-agent", Rung: 2, Res: Resolution{Action: ActSalvage}})
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "rung 2 may not pick salvage: the human may") {
		t.Errorf("rung 2's salvage = %v, want the rung's refusal", err)
	}
	_, err = r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 4, By: "alice", Rung: 3, Res: Resolution{Action: ActSalvage}})
	want := "salvage ends a run that neither moves nor answers: the monitor's run_quiet flag, or a live card whose workflow is gone, opens it"
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("salvage on a session cap = %v, want %q", err, want)
	}
	other := quietFlag()
	other.Facts = map[string]any{"flag": "wait_over_bound", "wait": "rung_answer"}
	other.ID = 5
	store.cases = append(store.cases, other)
	_, err = r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 5, By: "alice", Rung: 3, Res: Resolution{Action: ActSalvage}})
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("salvage on a rung_answer flag = %v, want %q", err, want)
	}
	if len(wf.terminated) != 0 || len(successors(store)) != 0 {
		t.Fatalf("a refused salvage terminated %q and filed %d successors", wf.terminated, len(successors(store)))
	}
	wf.gone = map[string]bool{"t-b": true}
	if _, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 4, By: "alice", Rung: 3, Res: Resolution{Action: ActSalvage}}); err != nil {
		t.Errorf("salvage on a card whose workflow is gone = %v, want it taken", err)
	}
}

// The human's stored offers hold the salvage only while the run is gone.
func TestSalvageIsOfferedWhileTheRunIsGone(t *testing.T) {
	has := func(f CaseFacts) bool {
		offers, _ := OffersFor(f)
		return slices.ContainsFunc(offers, func(o AskOffer) bool { return o.Value == ActSalvage })
	}
	if !has(CaseFacts{Kind: CaseSessionCap, RunGone: true}) {
		t.Error("a gone run's case does not offer salvage")
	}
	if has(CaseFacts{Kind: CaseSessionCap}) {
		t.Error("a live run's session cap offers salvage")
	}
}

// A terminate error carrying a URL's userinfo and a bearer value reaches the
// human masked; nothing is filed or closed.
func TestSalvageMasksTheTerminateError(t *testing.T) {
	r, store, _, _, wf := newRetryRig(t)
	wf.terminateErr = errors.New("terminate https://user:pass@host/x failed: Authorization: Bearer abc123def456ghi789jkl")
	store.cases = []Case{quietFlag()}
	_, err := r.Act(context.Background(), ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3, Res: Resolution{Action: ActSalvage}})
	if err == nil || strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), "abc123def456") || !strings.Contains(err.Error(), "could not be terminated") {
		t.Fatalf("err = %v, want the terminate failure with userinfo and bearer masked", err)
	}
	if len(successors(store)) != 0 || store.tasks["t-b"].Status == "closed" {
		t.Error("a failed terminate filed a successor or closed the card")
	}
}
