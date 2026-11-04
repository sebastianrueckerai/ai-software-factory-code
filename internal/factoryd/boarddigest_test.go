package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// boardNow is the board fixtures' now; their cards were filed a day before.
var boardNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// boardRig is a runner on a fake store and a fake object store at
// boardNow, whose policy gives sf/app work_in_progress 3, the month cap
// 600 and the day cap 150 EUR.
type boardRig struct {
	r     *Runner
	store *fakeStore
	tl    *Timeline
}

func newBoardRig(project policy.ProjectConfig) boardRig {
	store := newFakeStore()
	objects := &fakeObjectStore{}
	cfg := testRunnerConfig()
	project.Repository = "sf/app"
	cfg.Policy = func() policy.Version {
		v := testPolicyVersion()
		v.Projects = map[string]policy.ProjectConfig{"app": project}
		v.Document.Budget.MonthEUR, v.Document.Budget.DayEUR = 600, 150
		return v
	}
	return boardRig{r: &Runner{store: store, objects: objects, cfg: cfg, now: func() time.Time { return boardNow }}, store: store,
		tl: &Timeline{Objects: objects}}
}

// card files an agent card of sf/app, medium with its budget of 5, filed
// minute m of the day before boardNow.
func (g boardRig) card(id string, issue int64, status, gate string, m int, change func(*Task)) {
	t := Task{ID: id, Kind: "agent", Repository: "sf/app", IssueNumber: issue, Status: status, Gate: gate, Size: "medium", BudgetEUR: 5,
		CreatedAt: boardNow.Add(-24*time.Hour + time.Duration(m)*time.Minute)}
	if change != nil {
		change(&t)
	}
	g.store.tasks[id] = t
}

// child makes a card plan t1344's card under key.
func child(key string) func(*Task) {
	return func(t *Task) { t.Source, t.Parent, t.PlanKey = SourcePlan, "t1344", key }
}

// queuedFor stores reason as admit's refusal of a queued card, first
// given ago before boardNow.
func queuedFor(reason string, ago time.Duration) func(*Task) {
	return func(t *Task) { since := boardNow.Add(-ago); t.QueueReason, t.QueueSince = reason, &since }
}

// moved records card id's move to status and gate ago before boardNow.
func (g boardRig) moved(id, status, gate string, ago time.Duration) {
	g.store.events = append(g.store.events, Event{ID: int64(len(g.store.events) + 1), At: boardNow.Add(-ago), Repository: "sf/app",
		TaskID: id, Kind: EventTaskStatus, Actor: FactoryDecider, Detail: map[string]any{"status": status, "gate": gate}})
}

func (g boardRig) line(t *testing.T, id string, ago time.Duration, step string, detail map[string]any) {
	t.Helper()
	g.tl.now = func() time.Time { return boardNow.Add(-ago) }
	if err := g.tl.Append(context.Background(), id, step, detail); err != nil {
		t.Fatalf("append %s: %v", step, err)
	}
}

func (g boardRig) digest(t *testing.T) BoardDigest {
	t.Helper()
	b, err := g.r.BoardDigest(context.Background(), "sf/app")
	if err != nil {
		t.Fatalf("BoardDigest: %v", err)
	}
	return b
}

func TestBoardDigestRendersLiveCardsQueueSlotsPlansAndCaps(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{WorkInProgress: 3})
	g.card("t1344", 1344, "done", "", 0, func(t *Task) { t.Track, t.PlanName = policy.TrackPlan, "session-metrics" })
	if _, err := g.store.AddPlanRevision(context.Background(), "t1344", mustJSON(Plan{}), "r1"); err != nil {
		t.Fatal(err)
	}
	g.card("t1300", 1300, "done", "", 0, func(t *Task) { t.Track, t.PlanName = policy.TrackPlan, "closed-plan" })
	g.store.closed = map[string]time.Time{"t1300": boardNow.Add(-48 * time.Hour)}
	g.card("t1440", 1440, "done", "", 1, child("k1440"))
	g.card("t1441", 1441, "done", "", 2, child("k1441"))
	g.card("t1451", 1451, "failed", "", 3, child("k1451"))

	admitted := func(t *Task) { at := boardNow.Add(-20 * time.Hour); t.AdmittedAt = &at }
	g.card("t1410", 1410, "in-review", GateApproval, 10, admitted)
	g.moved("t1410", "in-review", GateApproval, 9*time.Hour+40*time.Minute)
	g.line(t, "t1410", 10*time.Hour, "model_review", map[string]any{"verdict": "concerns", "findings": []string{findingA}, "round": 1})
	for _, ago := range []time.Duration{9*time.Hour + 59*time.Minute, 9*time.Hour + 50*time.Minute} {
		g.line(t, "t1410", ago, "checkin", map[string]any{"round": 1, "decision": CheckHandover,
			"verdicts": map[string]CheckVerdict{findingA: {State: CheckConfirmed}},
			"record":   CheckInRecord{Moment: CheckInGate, Decision: CheckHandover}})
	}
	g.store.cases = []Case{{ID: 1, TaskID: "t1410", Kind: CaseBudgetFloor, StateKey: "k-floor", Status: CaseAtHuman,
		Money: &CaseMoney{Kind: string(CaseBudgetFloor), BudgetEUR: 5, SpentEUR: 4.9}}}

	g.card("t1452", 1452, "waiting-deps", "", 11, func(t *Task) { child("k1452")(t); admitted(t) })
	g.moved("t1452", "waiting-deps", "", 3*time.Hour+2*time.Minute)
	g.card("t1472", 1472, "running", GateSession, 12, child("k1472"))
	g.moved("t1472", "queued", GateIntake, 13*time.Minute)
	g.moved("t1472", "running", GateSession, 12*time.Minute)

	g.card("t1414", 1414, "queued", GateIntake, 20, queuedFor("work bound 3 of 3", 2*time.Hour))
	g.card("t1453", 1453, "queued", GateIntake, 21, func(t *Task) { child("k1453")(t); queuedFor("waits on #1451", 40*time.Minute)(t) })
	g.card("t-noissue", 0, "queued", "", 22, child("k-noissue"))
	g.store.deps = map[string][]string{"t1452": {"t1451"}, "t1453": {"t1451"}}
	g.store.enforced = policy.Enforced{Day: 12.4, Month: 544}

	want := "cards 6 live · slots 1 of 3 · queue 3\n" +
		"#1410  in-review, approval   9h40m          flags finding_confirmed_twice(" + idA + "), money_ask_hides_findings\n" +
		"#1452  waiting-deps          3h02m          flags dependency_failed(#1451)\n" +
		"#1472  running, session      0h12m    slot\n" +
		"queue:\n" +
		"#1414  work bound 3 of 3 for 2h\n" +
		"#1453  waits on #1451 for 40m\n" +
		"#0 t-noissue  no issue\n" +
		"plans:\n" +
		"#1344 session-metrics  done 2 · running 1 · waiting-deps 1 · queued 2 · failed 1\n" +
		"caps:\n" +
		"month 544.00 of 600.00 EUR · day 12.40 of 150.00 EUR\n"
	if got := g.digest(t).Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}

// The integration card's line counts its blind-met criteria of all the
// plan's; a feature card's line counts none.
func TestBoardDigestCountsTheBlindRows(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{WorkInProgress: 3})
	g.store.tasks["plan1"] = Task{ID: "plan1", Repository: "sf/app", IssueNumber: 1340, Kind: "agent", Track: policy.TrackPlan,
		Status: "done", PlanName: "blind proofs"}
	var criteria []PlanCriterion
	for _, id := range []string{"t_a", "t_b", "t_c", "t_d", "t_e", "t_f", "group_open", "size_bound", "door"} {
		criteria = append(criteria, PlanCriterion{ID: id, Kind: CriterionTest})
	}
	plan := Plan{Criteria: criteria, Cards: []PlanCard{{Key: "integ", Role: RoleIntegration}, {Key: "sizes", Role: RoleFeature}}}
	if _, err := g.store.AddPlanRevision(context.Background(), "plan1", mustJSON(plan), "r1"); err != nil {
		t.Fatal(err)
	}
	g.card("t1346", 1346, "in-review", GateApproval, 5, func(t *Task) { t.Source, t.Parent, t.PlanKey = SourcePlan, "plan1", "integ" })
	g.moved("t1346", "in-review", GateApproval, 2*time.Hour)
	at := boardNow.Add(-3 * time.Hour)
	var rows []CriterionVerdict
	for _, id := range []string{"t_a", "t_b", "t_c", "t_d", "t_e", "t_f"} {
		rows = append(rows, CriterionVerdict{Criterion: id, Verdict: "met", Evidence: "blind: criterion " + id + " met",
			Kind: CriterionTest, CreatedAt: at})
	}
	rows = append(rows,
		CriterionVerdict{Criterion: "group_open", Verdict: "met", Evidence: "open: criterion group_open met", Kind: CriterionTest, CreatedAt: at},
		CriterionVerdict{Criterion: "size_bound", Verdict: "unmet", Evidence: "blind: criterion size_bound red", Kind: CriterionTest, CreatedAt: at},
		CriterionVerdict{Criterion: "door", Verdict: "unmet", Evidence: "open: criterion door red", Kind: CriterionTest, CreatedAt: at})
	if err := g.store.PutCriteriaVerdicts(context.Background(), "t1346", CriteriaSourceGate, FactoryDecider, rows); err != nil {
		t.Fatal(err)
	}
	g.card("t1472", 1472, "running", GateSession, 6, func(t *Task) { t.Source, t.Parent, t.PlanKey = SourcePlan, "plan1", "sizes" })
	g.moved("t1472", "running", GateSession, time.Hour)
	var integration, feature string
	for _, line := range strings.Split(g.digest(t).Render(), "\n") {
		switch {
		case strings.HasPrefix(line, "#1346"):
			integration = line
		case strings.HasPrefix(line, "#1472"):
			feature = line
		}
	}
	if integration == "" || feature == "" {
		t.Fatalf("Render holds no lines for #1346 and #1472")
	}
	if !strings.Contains(integration, "blind 6 of 9") {
		t.Fatalf("#1346's line %q wants blind 6 of 9", integration)
	}
	if strings.Contains(integration, "slot") {
		t.Fatalf("#1346's line %q wants no slot at the approval wait", integration)
	}
	if strings.Contains(feature, "blind") {
		t.Fatalf("#1472's line %q wants no blind", feature)
	}
}

// unreadable is an object store whose Get of key fails.
type unreadable struct {
	ObjectStore
	key string
}

func (u unreadable) Get(ctx context.Context, key string) ([]byte, error) {
	if key == u.key {
		return nil, errors.New("s3 down")
	}
	return u.ObjectStore.Get(ctx, key)
}

// A card whose digest cannot be read gets a line that says why, and the
// board goes on to the next card, which has run without a step since its
// filing 23 h 59 m ago.
func TestBoardDigestNamesACardItCannotDigestAndGoesOn(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{WorkInProgress: 3})
	g.card("t1410", 1410, "running", GateSession, 0, nil)
	g.card("t1472", 1472, "running", GateSession, 1, nil)
	g.r.objects = unreadable{g.r.objects, "sessions/t1410/timeline.jsonl"}
	want := "cards 2 live · slots 2 of 3 · queue 0\n" +
		"#1410  running, session      digest unreadable: read the timeline: s3 down\n" +
		"#1472  running, session      23h59m   slot  flags wait_over_bound(run_quiet since +0:00)\n"
	if got := g.digest(t).Render(); !strings.HasPrefix(got, want) {
		t.Fatalf("Render =\n%s\nwant it to start\n%s", got, want)
	}
}

func TestBoardDigestNamesAQueuedCardWithoutAnIssue(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{})
	g.card("t-noissue", 0, "queued", "", 0, child("k1"))
	b := g.digest(t)
	if want := []BoardDigestQueued{{Issue: 0, TaskID: "t-noissue", Reason: "no issue"}}; !slices.Equal(b.Queue, want) || len(b.Cards) != 0 {
		t.Fatalf("Queue = %+v, Cards = %+v; want %+v and no card", b.Queue, b.Cards, want)
	}
	if got, want := b.Render(), "queue:\n#0 t-noissue  no issue\n"; !strings.Contains(got, want) {
		t.Fatalf("Render =\n%s\nwant it to hold\n%s", got, want)
	}
}

// The queue reads what admit stored, each card's reason and how long it
// has held, and works out none of its own: #1500's dependency is open and
// the project's cap has room, and a slot is free for #1501. A card admit
// never refused is next, or has no issue. An age under a minute shows
// none.
func TestBoardDigestReadsTheStoredQueueReason(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{WorkInProgress: 3, Budget: &policy.ProjectBudget{MonthEUR: 10}})
	g.card("t9", 9, "running", GateSession, 0, nil)
	g.card("t1500", 1500, "queued", GateIntake, 1, queuedFor("project month cap", 2*time.Hour))
	g.card("t1501", 1501, "queued", GateIntake, 2, queuedFor("work bound 3 of 3", 90*time.Minute+40*time.Second))
	g.card("t1502", 1502, "queued", GateIntake, 3, nil)
	g.card("t-noissue", 0, "queued", "", 4, child("k1"))
	g.card("t1503", 1503, "queued", GateIntake, 5, queuedFor("waits on #9", 40*time.Second))
	g.store.deps = map[string][]string{"t1500": {"t9"}}
	g.store.projectSpend = map[string]float64{"sf/app": 4}
	b := g.digest(t)
	want := []BoardDigestQueued{{Issue: 1500, TaskID: "t1500", Reason: "project month cap", Age: 2 * time.Hour},
		{Issue: 1501, TaskID: "t1501", Reason: "work bound 3 of 3", Age: 90*time.Minute + 40*time.Second},
		{Issue: 1502, TaskID: "t1502", Reason: "next"}, {TaskID: "t-noissue", Reason: "no issue"},
		{Issue: 1503, TaskID: "t1503", Reason: "waits on #9", Age: 40 * time.Second}}
	if !slices.Equal(b.Queue, want) {
		t.Fatalf("Queue = %+v, want %+v", b.Queue, want)
	}
	if got, want := b.Render(), "queue:\n"+
		"#1500  project month cap for 2h\n"+
		"#1501  work bound 3 of 3 for 1h30m\n"+
		"#1502  next\n"+
		"#0 t-noissue  no issue\n"+
		"#1503  waits on #9\n"+
		"caps:\n"+
		"month 0.00 of 600.00 EUR · day 0.00 of 150.00 EUR · project 4.00 of 10.00 EUR\n"; !strings.HasSuffix(got, want) {
		t.Fatalf("Render =\n%s\nwant it to end\n%s", got, want)
	}
}

func TestBoardDigestPlanLineCountsTheProofs(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{WorkInProgress: 3})
	g.card("t1344", 1344, "done", "", 0, func(t *Task) { t.Track, t.PlanName = policy.TrackPlan, "session-metrics" })
	g.card("t1440", 1440, "done", "", 1, child("k1440"))
	if _, err := g.store.AddPlanRevision(context.Background(), "t1344", mustJSON(proverPlan()), "r1"); err != nil {
		t.Fatal(err)
	}
	g.store.proofs = map[string][]PlanProof{"t1344": {proofRow(1, "size_bound", ProofStored), keyLog()}}
	b := g.digest(t)
	if len(b.Plans) != 1 || b.Plans[0].ProofsStored != 1 || b.Plans[0].ProofsWanted != 4 {
		t.Fatalf("Plans = %+v, want one plan with proofs 1 of 4", b.Plans)
	}
	if want := "\nplans:\n#1344 session-metrics  done 1 · proofs 1 of 4 stored\ncaps:\n"; !strings.Contains(b.Render(), want) {
		t.Fatalf("Render =\n%s\nwant it to hold\n%s", b.Render(), want)
	}
}

// A plan whose revision does not decode is one line that says so, and the
// digest goes on, as it does past a card it cannot read. A store read
// that fails is the digest's error.
func TestBoardDigestNamesAnUnreadablePlan(t *testing.T) {
	g := newBoardRig(policy.ProjectConfig{WorkInProgress: 3})
	g.card("t1344", 1344, "done", "", 0, func(t *Task) { t.Track, t.PlanName = policy.TrackPlan, "session-metrics" })
	g.card("t1345", 1345, "done", "", 1, func(t *Task) { t.Track, t.PlanName = policy.TrackPlan, "door-keys" })
	if _, err := g.store.AddPlanRevision(context.Background(), "t1344", mustJSON(proverPlan()), "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.AddPlanRevision(context.Background(), "t1345", json.RawMessage(`{"criteria": 5}`), "r2"); err != nil {
		t.Fatal(err)
	}
	b, err := g.r.BoardDigest(context.Background(), "sf/app")
	if err != nil {
		t.Fatalf("BoardDigest: %v", err)
	}
	if want := "\nplans:\n#1344 session-metrics  no cards · proofs 0 of 3 stored\n#1345 door-keys  unreadable\ncaps:\n"; !strings.Contains(b.Render(), want) {
		t.Fatalf("Render =\n%s\nwant it to hold\n%s", b.Render(), want)
	}
	// a store read that fails is the digest's error: a retry may read it
	g.r.store = &proofsUnread{g.store}
	if _, err := g.r.BoardDigest(context.Background(), "sf/app"); err == nil || !strings.Contains(err.Error(), "plan_proofs: connection reset") {
		t.Fatalf("BoardDigest with the proofs unread = %v, want the read's error", err)
	}
}
