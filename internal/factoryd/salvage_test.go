package factoryd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// failing wraps the rig's store with the one call that fails.
type failing struct {
	*fakeStore
	create, deleteLive, events error
}

func (s failing) CreateTask(ctx context.Context, t Task) error {
	if s.create != nil {
		return s.create
	}
	return s.fakeStore.CreateTask(ctx, t)
}

func (s failing) DeleteLiveSession(ctx context.Context, id string) error {
	if s.deleteLive != nil {
		return s.deleteLive
	}
	return s.fakeStore.DeleteLiveSession(ctx, id)
}

func (s failing) TaskEvents(ctx context.Context, id string, limit int) ([]Event, error) {
	if s.events != nil {
		return nil, s.events
	}
	return s.fakeStore.TaskEvents(ctx, id, limit)
}

func press(r *Runner) (ActResult, error) {
	return r.Act(context.Background(), ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3, Res: Resolution{Action: ActSalvage}})
}

// The order is successor, close, terminate. A successor that cannot be filed
// terminates nothing and closes nothing; a close that fails terminates
// nothing; each leaves the case open and names what stands.
func TestSalvageStopsBeforeTheTerminateWhenAnEarlierStepFails(t *testing.T) {
	for name, tc := range map[string]struct {
		fail failing
		want string
	}{
		"successor": {failing{create: errors.New("store down")}, "the successor of card #6 was not filed and the run goes on: press again"},
		"close":     {failing{deleteLive: errors.New("store down")}, "is filed, but card #6 did not close and its run goes on: press again"},
		"digest":    {failing{events: errors.New("store down")}, "the card's digest could not be read, so nothing was filed: press again"},
	} {
		t.Run(name, func(t *testing.T) {
			r, store, _, _, wf := newRetryRig(t)
			store.cases = []Case{quietFlag()}
			tc.fail.fakeStore = store
			r.store = tc.fail
			_, err := press(r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("press = %v, want %q", err, tc.want)
			}
			if len(wf.terminated) != 0 || store.tasks["t-b"].Status == "closed" || store.cases[0].Status != CaseAtHuman {
				t.Fatalf("terminated %q, card %s, case %s; want no terminate, a live card and the case open", wf.terminated, store.tasks["t-b"].Status, store.cases[0].Status)
			}
			if name == "close" && len(successors(store)) != 1 {
				t.Fatalf("%d successors, want the filed one", len(successors(store)))
			}
		})
	}
}

// A terminate that fails after the close leaves the card closed, the successor
// filed and the case open, naming the state. The press again files nothing,
// terminates, and resolves the case; a heal pass in between starts nothing.
func TestSalvageAfterAFailedTerminateIsRetryable(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, wf := newRetryRig(t)
	store.cases = []Case{quietFlag()}
	wf.terminateErr = errors.New("terminate https://user:pass@host/x failed: Authorization: Bearer abc123def456ghi789jkl")
	_, err := press(r)
	want := "the card #6 is closed and #9 filed; the run did not end: "
	if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), "abc123def456") ||
		!strings.HasSuffix(err.Error(), "Press salvage again to end it") {
		t.Fatalf("press = %v, want %q with userinfo and bearer masked and the action named", err, want)
	}
	if store.tasks["t-b"].Status != "closed" || len(successors(store)) != 1 || store.cases[0].Status != CaseAtHuman {
		t.Fatalf("card %s, %d successors, case %s; want closed, one, open", store.tasks["t-b"].Status, len(successors(store)), store.cases[0].Status)
	}
	// the card is final: the heal pass starts nothing for it
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/timeline") {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `{"state":"open"}`)
	}))
	t.Cleanup(stub.Close)
	forgejo := &Forgejo{BaseURL: stub.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded}
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	before := len(wf.started)
	HealPass(ctx, store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent",
		Timeline: &Timeline{Objects: &fakeObjectStore{}}}, newCompletion(store, fjURL, llURL), noneCompleted)
	if slices.Contains(wf.started[before:], "t-b") {
		t.Fatalf("the heal pass started %q: a closed card's run is never started again", wf.started[before:])
	}
	wf.terminateErr = nil
	got, err := press(r)
	if err != nil {
		t.Fatalf("second press = %v", err)
	}
	if !slices.Equal(wf.terminated, []string{"t-b salvaged by alice"}) || len(successors(store)) != 1 || got.Case.Status != CaseResolved {
		t.Fatalf("terminated %q, %d successors, case %s; want one terminate, one successor, resolved", wf.terminated, len(successors(store)), got.Case.Status)
	}
}

// A workflow that cannot be read is unknown, not gone: the facts are whole,
// so rung 0 acts as before, and no run-gone salvage is offered.
func TestAnAliveReadThatFailsLeavesTheFactsWholeAndOffersNoSalvage(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, wf := newRetryRig(t)
	wf.aliveErr = errors.New("frontend down https://user:pass@host/x")
	c := Case{ID: 4, TaskID: "t-b", Kind: CaseSessionCap, Status: CaseAtHuman}
	f, err := r.factsAtCase(ctx, store.tasks["t-b"], c)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.Partial, "run") || f.RunGone || f.RunStrays {
		t.Fatalf("partial %v, gone %v, strays %v; want whole facts and no run verdict", f.Partial, f.RunGone, f.RunStrays)
	}
	if offers, _ := OffersFor(f); slices.ContainsFunc(offers, func(o AskOffer) bool { return o.Value == ActSalvage }) {
		t.Error("an unread workflow offers the salvage")
	}
}

// runGone reads live agent cards only, plus a closed card for a stray run.
func TestRunGoneReadsTheStatusesAndKindsItOwns(t *testing.T) {
	ctx := context.Background()
	r, _, _, _, wf := newRetryRig(t)
	wf.gone = map[string]bool{"x": true}
	for name, tc := range map[string]struct {
		task         Task
		gone, strays bool
	}{
		"running":       {Task{ID: "x", Kind: "agent", Status: "running"}, true, false},
		"in review":     {Task{ID: "x", Kind: "agent", Status: "in-review"}, true, false},
		"waiting human": {Task{ID: "x", Kind: "agent", Status: "waiting-human"}, true, false},
		"waiting deps":  {Task{ID: "x", Kind: "agent", Status: "waiting-deps"}, true, false},
		"queued":        {Task{ID: "x", Kind: "agent", Status: "queued"}, false, false},
		"merged":        {Task{ID: "x", Kind: "agent", Status: "merged"}, false, false},
		"human card":    {Task{ID: "x", Kind: "human", Status: "waiting-human"}, false, false},
		"chat":          {Task{ID: "x", Kind: KindChat, Status: "running"}, false, false},
		"closed, gone":  {Task{ID: "x", Kind: "agent", Status: "closed"}, false, false},
		"closed, alive": {Task{ID: "y", Kind: "agent", Status: "closed"}, false, true},
		"done":          {Task{ID: "y", Kind: "agent", Status: "done"}, false, false},
	} {
		if gone, strays := r.runGone(ctx, tc.task); gone != tc.gone || strays != tc.strays {
			t.Errorf("%s: gone %v strays %v, want %v and %v", name, gone, strays, tc.gone, tc.strays)
		}
	}
}

// A terminate error carrying a URL's userinfo and a bearer value reaches the
// human masked.
func TestSalvageMasksTheTerminateError(t *testing.T) {
	r, store, _, _, wf := newRetryRig(t)
	wf.terminateErr = errors.New("terminate https://user:pass@host/x failed: Authorization: Bearer abc123def456ghi789jkl")
	store.cases = []Case{quietFlag()}
	_, err := press(r)
	if err == nil || strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), "abc123def456") || !strings.Contains(err.Error(), "the run did not end") {
		t.Fatalf("err = %v, want the terminate failure with userinfo and bearer masked", err)
	}
}
