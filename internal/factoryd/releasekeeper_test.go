package factoryd

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func keeperAt(h, m int) time.Time { return time.Date(2026, 10, 29, h, m, 0, 0, time.UTC) }

func keeperAtPtr(h, m int) *time.Time {
	at := keeperAt(h, m)
	return &at
}

const (
	holdInfoTail = "Wait keeps production held and asks again in 6 hours. Deploy anyway sends main as it is to production, half-built plans included. " +
		"Replan on top plans the rest again on top of what is merged. Take it off main and replan reverts the plan's merged cards first, so production goes ahead without them."
	holdWhy = "Merged is not live: while a plan is open on main, no merge of sf-demo-app goes to production, so a half-built feature never goes live. " +
		"Production has waited since 2026-10-29 10:00 UTC."
	holdPlanLine = "- plan #12: 2 of 4 cards merged; waits on #45 (failed), #46 (queued)."
	holdEndNote  = "factoryd: no plan is open on main any more, so production goes ahead. Closing this issue."
)

var holdChoices = []QuestionChoice{
	{AnswerHoldWait, "Wait"},
	{AnswerDeployAnyway, "Deploy anyway"},
	{AnswerReplanOnTop + ":t-plan", "Replan plan #12 on top"},
	{AnswerTakeOffMain + ":t-plan", "Take plan #12 off main and replan"},
}

// newKeeper is the keeper's rig on 2026-10-29: demoRepo's CI asks /release
// since 09:00, and plan t-plan (#12) is open on its main: t-a merged at
// 10:00, t-b failed (#45), t-c queued (#46), t-d done at 10:30, t-e closed
// (#48). Own is the code repository. The returned clock is the keeper's
// now, 11:00 until a test sets it.
func newKeeper(t *testing.T, f *chainForgejo) (*ReleaseKeeper, *fakeStore, *time.Time) {
	t.Helper()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", IssueNumber: 12, Repository: demoRepo, Track: policy.TrackPlan}
	child := func(id, status string, issue int64, commit string, finished *time.Time) {
		store.tasks[id] = Task{ID: id, Source: SourcePlan, Parent: "t-plan", PlanKey: id, Repository: demoRepo, Status: status,
			IssueNumber: issue, MergeCommit: commit, FinishedAt: finished}
	}
	child("t-a", "merged", 43, "m1", keeperAtPtr(10, 0))
	child("t-b", "failed", 45, "", nil)
	child("t-c", "queued", 46, "", nil)
	child("t-d", "done", 47, "m2", keeperAtPtr(10, 30))
	child("t-e", "closed", 48, "", nil)
	if err := store.RecordReleaseAsk(context.Background(), demoRepo, "c0c0c0c", false, keeperAt(9, 0)); err != nil {
		t.Fatal(err)
	}
	now := keeperAt(11, 0)
	own, _ := SplitRepo(codeRepo)
	k := &ReleaseKeeper{Store: store, Forgejo: f.srv(t), Timeline: &Timeline{Store: store, Events: store}, Own: own,
		Now: func() time.Time { return now }}
	return k, store, &now
}

// openPlanIn opens plan id (issue) on repository's main: its one child
// merged at merged.
func openPlanIn(store *fakeStore, repository, id string, issue int64, merged time.Time) {
	store.tasks[id] = Task{ID: id, IssueNumber: issue, Repository: repository, Track: policy.TrackPlan}
	store.tasks[id+"-1"] = Task{ID: id + "-1", Source: SourcePlan, Parent: id, PlanKey: "a", Repository: repository, Status: "merged",
		MergeCommit: "a1a1", FinishedAt: &merged}
}

func keeperPass(t *testing.T, k *ReleaseKeeper) {
	t.Helper()
	if err := k.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func releaseRow(store *fakeStore, repository string) Release {
	r, _ := store.Release(context.Background(), repository)
	return r
}

// askOfHold is the question the newest ask card of demoRepo carries, as its
// intake row records it.
func askOfHold(t *testing.T, store *fakeStore) (Task, HumanQuestion) {
	t.Helper()
	card, ok, _ := store.ChainCard(context.Background(), demoRepo, SourceProductionHeld)
	if !ok {
		t.Fatal("no ask card")
	}
	for _, e := range store.intakeEvents {
		if e.TaskID == card.ID && e.Kind == IntakeEscalation && e.Question != nil {
			return card, *e.Question
		}
	}
	t.Fatalf("ask card %s has no recorded question", card.ID)
	return card, HumanQuestion{}
}

func releasedRows(store *fakeStore) []Event {
	var out []Event
	for _, e := range store.events {
		if e.Kind == EventProductionReleased {
			out = append(out, e)
		}
	}
	return out
}

func TestReleaseKeeperStartsTheClockAtTheFirstMerge(t *testing.T) {
	f := &chainForgejo{}
	k, store, _ := newKeeper(t, f)
	openPlanIn(store, "sf/other", "t-other", 5, keeperAt(9, 0))
	openPlanIn(store, codeRepo, "t-code", 7, keeperAt(9, 40))
	keeperPass(t, k)
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(10, 0)}); !reflect.DeepEqual(got, want) {
		t.Errorf("%s's row %+v, want %+v: the clock starts at the first merge", demoRepo, got, want)
	}
	if len(f.issues) != 0 {
		t.Errorf("issues %q, want none an hour into the hold", f.issues)
	}
	if _, ok := store.releases["sf/other"]; ok {
		t.Errorf("sf/other got a row, but its CI never asked")
	}
	if got, want := releaseRow(store, codeRepo), (Release{Repository: codeRepo, HeldSince: keeperAt(9, 40)}); !reflect.DeepEqual(got, want) {
		t.Errorf("own row %+v, want %+v", got, want)
	}
}

func TestReleaseKeeperAsksAfterSixHours(t *testing.T) {
	f := &chainForgejo{}
	k, store, clock := newKeeper(t, f)
	*clock = keeperAt(15, 59)
	keeperPass(t, k)
	if len(f.issues) != 0 {
		t.Fatalf("issues %q at 15:59, want none", f.issues)
	}

	*clock = keeperAt(16, 0)
	keeperPass(t, k)
	if len(f.issues) != 1 {
		t.Fatalf("issues %q at 16:00, want one", f.issues)
	}
	title, body, _ := strings.Cut(f.issues[0], "\n\n")
	if want := "Production of sf-demo-app has waited 6 hours: plan #12 is open on main. Wait, deploy anyway, or replan?"; title != want {
		t.Errorf("title %q, want %q", title, want)
	}
	for _, want := range []string{holdPlanLine, "Choices:\n- Wait\n- Deploy anyway\n- Replan plan #12 on top\n- Take plan #12 off main and replan\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
	card, q := askOfHold(t, store)
	if card.Kind != "human" || card.Status != "waiting-human" || card.IssueNumber != 31 {
		t.Errorf("ask card %+v, want a human card waiting on #31", card)
	}
	want := HumanQuestion{
		Ask:     title,
		Why:     holdWhy,
		Info:    "The open plans and what each waits on:\n" + holdPlanLine + "\n\n" + holdInfoTail,
		Choices: holdChoices,
	}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("question\n%+v, want\n%+v", q, want)
	}
	if got := releaseRow(store, demoRepo).AskedAt; !got.Equal(keeperAt(16, 0)) {
		t.Errorf("AskedAt %v, want 16:00", got)
	}

	*clock = keeperAt(16, 1)
	keeperPass(t, k)
	if len(f.issues) != 1 {
		t.Fatalf("issues %d at 16:01, want the one standing ask", len(f.issues))
	}

	card.Status, card.FinishedAt = "closed", keeperAtPtr(17, 0)
	store.tasks[card.ID] = card
	*clock = keeperAt(22, 59)
	keeperPass(t, k)
	if len(f.issues) != 1 {
		t.Fatalf("issues %d at 22:59, want none new 6 hours less a minute after the ask closed", len(f.issues))
	}
	*clock = keeperAt(23, 0)
	keeperPass(t, k)
	if len(f.issues) != 2 {
		t.Fatalf("issues %d at 23:00, want a second ask 6 hours after the first closed", len(f.issues))
	}
}

func TestReleaseKeeperAsksAtOnceOnAFix(t *testing.T) {
	ctx := context.Background()
	f := &chainForgejo{}
	k, store, clock := newKeeper(t, f)
	if _, err := store.PutReleaseHold(ctx, demoRepo, keeperAt(10, 0), time.Time{}, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	fix := func(id, source string, issue int64, merged *time.Time) {
		store.tasks[id] = Task{ID: id, Source: source, IssueNumber: issue, Repository: demoRepo, Status: "merged", MergeCommit: "f" + id, FinishedAt: merged}
	}
	fix("t-fix", SourceAlert, 40, keeperAtPtr(10, 45))
	keeperPass(t, k)
	if len(f.issues) != 1 {
		t.Fatalf("issues %q, want the fix's ask at once", f.issues)
	}
	if title, _, _ := strings.Cut(f.issues[0], "\n\n"); title != "Fix #40 merged, but production of sf-demo-app is held: plan #12 is open on main. Deploy anyway, wait, or replan?" {
		t.Errorf("title %q", title)
	}
	if _, q := askOfHold(t, store); q.Why != "#40 fixes an alert. "+holdWhy {
		t.Errorf("why %q", q.Why)
	}

	fix("t-fix2", SourceMainFix, 41, keeperAtPtr(11, 10))
	*clock = keeperAt(11, 15)
	keeperPass(t, k)
	wantComments := []string{demoRepo + "#31 factoryd: fix #41 merged too and waits for production."}
	if len(f.issues) != 1 || !slices.Equal(f.comments, wantComments) {
		t.Fatalf("issues %d, comments %q; want one issue and %q", len(f.issues), f.comments, wantComments)
	}
	if got := releaseRow(store, demoRepo).AskedAt; !got.Equal(keeperAt(11, 15)) {
		t.Errorf("AskedAt %v, want 11:15", got)
	}
	*clock = keeperAt(11, 16)
	keeperPass(t, k)
	fix("t-revert", SourceRevert, 39, keeperAtPtr(11, 20))
	*clock = keeperAt(11, 25)
	keeperPass(t, k)
	if len(f.issues) != 1 || !slices.Equal(f.comments, wantComments) {
		t.Fatalf("issues %d, comments %q; want nothing new for no new fix and for a revert", len(f.issues), f.comments)
	}

	// the ask closed, two fixes at once ask again with both
	card, _ := askOfHold(t, store)
	card.Status, card.FinishedAt = "closed", keeperAtPtr(11, 30)
	store.tasks[card.ID] = card
	fix("t-fix3", SourceChainRepair, 42, keeperAtPtr(11, 40))
	fix("t-fix4", SourceAlert, 44, keeperAtPtr(11, 41))
	*clock = keeperAt(11, 45)
	keeperPass(t, k)
	if len(f.issues) != 2 {
		t.Fatalf("issues %d, want a second ask", len(f.issues))
	}
	if title, _, _ := strings.Cut(f.issues[1], "\n\n"); title != "Fixes #42 and #44 merged, but production of sf-demo-app is held: plan #12 is open on main. Deploy anyway, wait, or replan?" {
		t.Errorf("title %q", title)
	}
	if _, q := askOfHold(t, store); q.Why != "#42 fixes a red main. #44 fixes an alert. "+holdWhy {
		t.Errorf("why %q", q.Why)
	}
	fix("t-fix5", SourceAlert, 49, keeperAtPtr(11, 50))
	fix("t-fix6", SourceMainFix, 50, keeperAtPtr(11, 51))
	*clock = keeperAt(11, 55)
	keeperPass(t, k)
	if got := f.comments[len(f.comments)-1]; got != demoRepo+"#32 factoryd: fixes #49 and #50 merged too and wait for production." {
		t.Errorf("comment %q", got)
	}
}

// With several plans open the ask keeps to MaxChoices: wait, deploy anyway
// and the two replans of the plan that holds longest, the oldest first
// merge; it names each other plan, to be replanned from its drawer.
func TestReleaseKeeperOffersTheReplansOfTheLongestHeldPlan(t *testing.T) {
	why := "Merged is not live: while a plan is open on main, no merge of sf-demo-app goes to production, so a half-built feature never goes live. " +
		"Production has waited since 2026-10-29 09:50 UTC."
	choices := []QuestionChoice{
		{AnswerHoldWait, "Wait"},
		{AnswerDeployAnyway, "Deploy anyway"},
		{AnswerReplanOnTop + ":t-zplan", "Replan plan #15 on top"},
		{AnswerTakeOffMain + ":t-zplan", "Take plan #15 off main and replan"},
	}
	for _, c := range []struct {
		name, reason, lines, others string
		third                       bool
	}{
		{"two plans", "plans #15 and #12", "- plan #15: 1 of 1 cards merged; waits for the plan to close.\n" + holdPlanLine,
			"Plan #12 is open on main too: replan it from its drawer.", false},
		{"three plans", "plans #15, #12 and #18", "- plan #15: 1 of 1 cards merged; waits for the plan to close.\n" + holdPlanLine +
			"\n- plan #18: 1 of 1 cards merged; waits for the plan to close.",
			"Plans #12 and #18 are open on main too: replan them from their drawers.", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &chainForgejo{}
			k, store, clock := newKeeper(t, f)
			openPlanIn(store, demoRepo, "t-zplan", 15, keeperAt(9, 50))
			if c.third {
				openPlanIn(store, demoRepo, "t-yplan", 18, keeperAt(10, 20))
			}
			*clock = keeperAt(16, 0)
			keeperPass(t, k)
			if len(f.issues) != 1 {
				t.Fatalf("issues %d, want one ask", len(f.issues))
			}
			if !strings.Contains(f.issues[0], c.others) {
				t.Errorf("body %q does not name the other plans with %q", f.issues[0], c.others)
			}
			_, q := askOfHold(t, store)
			want := HumanQuestion{
				Ask:     "Production of sf-demo-app has waited 6 hours: " + c.reason + " are open on main. Wait, deploy anyway, or replan?",
				Why:     why,
				Info:    "The open plans and what each waits on:\n" + c.lines + "\n\n" + holdInfoTail + "\n\n" + c.others,
				Choices: choices,
			}
			if !reflect.DeepEqual(q, want) {
				t.Errorf("question\n%+v, want\n%+v", q, want)
			}
			if len(q.Choices) > MaxChoices {
				t.Errorf("%d choices, over MaxChoices %d", len(q.Choices), MaxChoices)
			}
		})
	}
}

// A pass whose ask write lost to a writer that landed after its read files
// no ask: a Deploy now's reset, on a hold's first ask (asked_at still none,
// the clock restarted) as on a later one, or another pass's ask.
func TestReleaseKeeperFilesNoAskWhereItsWriteLost(t *testing.T) {
	ctx := context.Background()
	// rig holds demoRepo since 10:00, last asked at asked (zero: never, else
	// that ask's card closed at 10:30), and passes at 16:30 with race run
	// between the pass's read and its write
	rig := func(t *testing.T, asked time.Time, race func(*fakeStore)) (*chainForgejo, *fakeStore) {
		f := &chainForgejo{}
		k, store, clock := newKeeper(t, f)
		if _, err := store.PutReleaseHold(ctx, demoRepo, keeperAt(10, 0), asked, time.Time{}, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if !asked.IsZero() {
			store.tasks["t-ask0"] = Task{ID: "t-ask0", Kind: "human", Source: SourceProductionHeld, Status: "closed", Repository: demoRepo,
				IssueNumber: 70, CreatedAt: asked, FinishedAt: keeperAtPtr(10, 30)}
		}
		if race != nil {
			k.Store = &releaseRaceStore{fakeStore: store, race: func() { race(store) }}
		}
		*clock = keeperAt(16, 30)
		keeperPass(t, k)
		return f, store
	}
	deployNow := func(s *fakeStore) { s.SetDeployNow(ctx, demoRepo, demoCommit, keeperAt(16, 30)) }
	pressed := Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(16, 30), DeployNow: demoCommit}
	for name, asked := range map[string]time.Time{"the first ask": {}, "a later ask": keeperAt(10, 20)} {
		t.Run(name, func(t *testing.T) {
			if f, _ := rig(t, asked, nil); len(f.issues) != 1 {
				t.Fatalf("issues %d with no writer between, want the ask", len(f.issues))
			}
			f, store := rig(t, asked, deployNow)
			if len(f.issues) != 0 {
				t.Errorf("issues %q, want none after a Deploy now landed mid-pass", f.issues)
			}
			if got := releaseRow(store, demoRepo); !reflect.DeepEqual(got, pressed) {
				t.Errorf("row %+v, want the Deploy now's %+v", got, pressed)
			}
		})
	}
	f, store := rig(t, keeperAt(10, 20), func(s *fakeStore) {
		s.PutReleaseHold(ctx, demoRepo, keeperAt(10, 0), keeperAt(16, 30), keeperAt(10, 20), keeperAt(10, 0))
	})
	if card, _, _ := store.ChainCard(ctx, demoRepo, SourceProductionHeld); len(f.issues) != 0 || card.ID != "t-ask0" {
		t.Errorf("issues %q, newest ask %s; want no ask after another pass asked mid-pass", f.issues, card.ID)
	}
}

// releaseRaceStore runs race after every Releases read, as a writer that lands
// between a pass's read and its write.
type releaseRaceStore struct {
	*fakeStore
	race func()
}

func (s *releaseRaceStore) Releases(ctx context.Context) ([]Release, error) {
	rows, err := s.fakeStore.Releases(ctx)
	s.race()
	return rows, err
}

// An ask whose filing fails puts its ask time back, so the next pass asks.
func TestReleaseKeeperAsksAgainAfterAFailedFiling(t *testing.T) {
	logs := captureLog(t)
	f := &chainForgejo{}
	k, store, clock := newKeeper(t, f)
	store.createFails = 1
	*clock = keeperAt(16, 0)
	if err := k.Pass(context.Background()); err == nil || !strings.Contains(logs.String(), "release keeper: sf/demo-app: insert failed") {
		t.Fatalf("Pass = %v, log %q; want the failed filing returned and logged", err, logs.String())
	}
	if got := releaseRow(store, demoRepo).AskedAt; !got.IsZero() {
		t.Fatalf("AskedAt %v after the failed filing, want it put back", got)
	}
	*clock = keeperAt(16, 1)
	keeperPass(t, k)
	if _, ok, _ := store.ChainCard(context.Background(), demoRepo, SourceProductionHeld); !ok || !releaseRow(store, demoRepo).AskedAt.Equal(keeperAt(16, 1)) {
		t.Errorf("ask card %v, AskedAt %v; want the ask at 16:01", ok, releaseRow(store, demoRepo).AskedAt)
	}
	if len(f.issues) != 2 || !slices.Equal(f.closed, []string{demoRepo + "#31"}) {
		t.Errorf("issues %d, closed %q; want the failed filing's #31 closed, one ask open", len(f.issues), f.closed)
	}
}

// An ask Forgejo refuses for good keeps its time: no retry each minute,
// the next ask comes HoldAskAfter later, and the refusal is logged.
func TestReleaseKeeperKeepsTheAskTimeOfARefusedFiling(t *testing.T) {
	logs := captureLog(t)
	f := &chainForgejo{fileIssue: http.StatusUnprocessableEntity}
	k, store, clock := newKeeper(t, f)
	*clock = keeperAt(16, 0)
	keeperPass(t, k)
	if got := releaseRow(store, demoRepo).AskedAt; !got.Equal(keeperAt(16, 0)) {
		t.Errorf("AskedAt %v, want 16:00 kept", got)
	}
	if !strings.Contains(logs.String(), "release keeper: sf/demo-app: Forgejo refused the ask, which is not filed: ") {
		t.Errorf("log %q, want the refusal said", logs.String())
	}
	f.fileIssue = 0
	*clock = keeperAt(16, 1)
	keeperPass(t, k)
	if len(f.issues) != 0 {
		t.Errorf("issues %q at 16:01, want none before 22:00", f.issues)
	}
}

// A Deploy now that lands between the ask's write and its filing closes the
// ask just filed: the pass reads the row again, and its ask time is gone.
func TestReleaseKeeperClosesAnAskADeployNowOvertook(t *testing.T) {
	f := &chainForgejo{}
	k, store, clock := newKeeper(t, f)
	k.Store = &deployNowAfterAsk{store}
	*clock = keeperAt(16, 0)
	keeperPass(t, k)
	note := demoRepo + "#31 factoryd: Deploy now sent main at 9d9d9d9 to production while this ask was filed. Closing this issue."
	if len(f.issues) != 1 || !slices.Equal(f.closed, []string{demoRepo + "#31"}) || !slices.Equal(f.comments, []string{note}) {
		t.Errorf("issues %d, closed %q, comments %q; want #31 filed and closed with %q", len(f.issues), f.closed, f.comments, note)
	}
	want := Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(16, 0), DeployNow: demoCommit}
	if got := releaseRow(store, demoRepo); !reflect.DeepEqual(got, want) {
		t.Errorf("row %+v, want the Deploy now's %+v", got, want)
	}
}

// deployNowAfterAsk presses Deploy now on demoCommit right after an ask's
// write lands, before the pass files the ask.
type deployNowAfterAsk struct{ *fakeStore }

func (s *deployNowAfterAsk) PutReleaseHold(ctx context.Context, repository string, since, asked, was, wasHeld time.Time) (bool, error) {
	landed, err := s.fakeStore.PutReleaseHold(ctx, repository, since, asked, was, wasHeld)
	if landed && !asked.IsZero() {
		err = errors.Join(err, s.SetDeployNow(ctx, repository, demoCommit, asked))
	}
	return landed, err
}

// The pass compares the ask time it wrote with the row it reads back, so
// its now keeps the microseconds Postgres keeps: a time at nanoseconds
// reads back cut to them, one cut to them reads back equal.
func TestPGReleaseTimesKeepMicroseconds(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_release_micro"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 29, 16, 0, 0, 123456789, time.UTC)
	for _, c := range []struct {
		repository string
		write      time.Time
		equal      bool
	}{{"sf/nano", at, false}, {"sf/micro", at.Truncate(time.Microsecond), true}} {
		if _, err := s.PutReleaseHold(ctx, c.repository, c.write, c.write, time.Time{}, time.Time{}); err != nil {
			t.Fatal(err)
		}
		r, err := s.Release(ctx, c.repository)
		if err != nil {
			t.Fatal(err)
		}
		if r.AskedAt.Equal(c.write) != c.equal || !r.AskedAt.Equal(at.Truncate(time.Microsecond)) {
			t.Errorf("%s: wrote %s, read %s; want it equal %v, cut to microseconds", c.repository, c.write.Format(time.RFC3339Nano), r.AskedAt.Format(time.RFC3339Nano), c.equal)
		}
	}
}

// endedHold is a hold of demoRepo since 10:00 whose plan closed, its
// promote job told held at demoCommit, ask #77 standing, at 18:00. Run 21's
// jobs and run 22's are listed; runs are main's push runs.
func endedHold(t *testing.T, told string, runs ...map[string]any) (*ReleaseKeeper, *chainForgejo, *fakeStore, *time.Time) {
	t.Helper()
	ctx := context.Background()
	f := &chainForgejo{runs: map[string][]map[string]any{demoRepo: runs}, jobs: map[int64][]map[string]any{
		21: {actionJob(211, "checks", "success", 1), actionJob(212, "promote", "success", 1)},
		22: {actionJob(221, "checks", "success", 1), actionJob(222, "promote", "success", 1)},
	}}
	k, store, clock := newKeeper(t, f)
	if _, err := store.PutReleaseHold(ctx, demoRepo, keeperAt(10, 0), time.Time{}, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if told != "" {
		if err := store.RecordReleaseAsk(ctx, demoRepo, told, true, keeperAt(9, 0)); err != nil {
			t.Fatal(err)
		}
	}
	store.tasks["t-ask"] = Task{ID: "t-ask", Kind: "human", Source: SourceProductionHeld, Status: "waiting-human", Repository: demoRepo,
		IssueNumber: 77, CreatedAt: keeperAt(10, 0)}
	store.closed["t-plan"] = time.Now()
	*clock = keeperAt(18, 0)
	return k, f, store, clock
}

func TestReleaseKeeperEndsTheHoldAndRunsThePromoteAgain(t *testing.T) {
	k, f, store, clock := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
	openPlanIn(store, codeRepo, "t-code", 7, keeperAt(9, 40))
	store.PutReleaseHold(context.Background(), codeRepo, keeperAt(9, 40), time.Time{}, time.Time{}, time.Time{})
	store.closed["t-code"] = time.Now()
	keeperPass(t, k)
	if !slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) || len(f.reruns) != 0 {
		t.Errorf("job re-runs %q, run re-runs %q; want only run 21's promote job", f.jobReruns, f.reruns)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0)}); !reflect.DeepEqual(got, want) {
		t.Errorf("row %+v, want %+v", got, want)
	}
	if !slices.Equal(f.closed, []string{demoRepo + "#77"}) || !slices.Equal(f.comments, []string{demoRepo + "#77 " + holdEndNote}) {
		t.Errorf("closed %q, comments %q; want #77 closed with the end note", f.closed, f.comments)
	}
	if slices.Contains(f.runReads, codeRepo) {
		t.Errorf("run reads %q: the keeper re-runs nothing of its own repository", f.runReads)
	}
	rows := releasedRows(store)
	if len(rows) != 2 {
		t.Fatalf("production_released rows %+v, want one per ended hold", rows)
	}
	for _, want := range []Event{
		{At: keeperAt(18, 0), Repository: demoRepo, Kind: EventProductionReleased, Actor: "factoryd", Via: ViaFactoryd,
			Detail: map[string]any{"how": "closed", "held_since": "2026-10-29T10:00:00Z", "hours": 8}},
		{At: keeperAt(18, 0), Repository: codeRepo, Kind: EventProductionReleased, Actor: "factoryd", Via: ViaFactoryd,
			Detail: map[string]any{"how": "closed", "held_since": "2026-10-29T09:40:00Z", "hours": 8}},
	} {
		if i := slices.IndexFunc(rows, func(e Event) bool { return e.Repository == want.Repository }); i < 0 || !reflect.DeepEqual(rows[i], want) {
			t.Errorf("rows %+v, want one %+v", rows, want)
		}
	}

	*clock = keeperAt(18, 1)
	keeperPass(t, k)
	if len(f.jobReruns) != 1 || len(releasedRows(store)) != 2 || len(f.closed) != 1 {
		t.Errorf("job re-runs %q, %d rows, closed %q after a second pass; want nothing more", f.jobReruns, len(releasedRows(store)), f.closed)
	}
}

// The hold's end closes its ask first: a close that fails leaves the hold
// as it stood, and the next pass ends it.
func TestReleaseKeeperEndsTheHoldOnceItsAskClosed(t *testing.T) {
	logs := captureLog(t)
	k, f, store, clock := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
	store.failIntakeEvent = errors.New("the database is gone")
	if err := k.Pass(context.Background()); err == nil || !strings.Contains(logs.String(), "release keeper: sf/demo-app: the database is gone") {
		t.Fatalf("Pass = %v, log %q; want the failed close returned and logged", err, logs.String())
	}
	if got := releaseRow(store, demoRepo).HeldSince; !got.Equal(keeperAt(10, 0)) || len(releasedRows(store)) != 0 || len(f.closed) != 0 || len(f.jobReruns) != 0 {
		t.Fatalf("held since %v, %d rows, closed %q, job re-runs %q; want the hold as it stood", got, len(releasedRows(store)), f.closed, f.jobReruns)
	}
	store.failIntakeEvent = nil
	*clock = keeperAt(18, 1)
	keeperPass(t, k)
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0)}); !reflect.DeepEqual(got, want) {
		t.Errorf("row %+v, want %+v", got, want)
	}
	if rows := releasedRows(store); len(rows) != 1 || rows[0].Detail["hours"] != 8 || !slices.Equal(f.closed, []string{demoRepo + "#77"}) ||
		!slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) {
		t.Errorf("rows %+v, closed %q, job re-runs %q; want the hold ended once", rows, f.closed, f.jobReruns)
	}
}

func TestReleaseKeeperRunsThePromoteOnlyOfTheToldCommit(t *testing.T) {
	ctx := context.Background()
	t.Run("a newer commit", func(t *testing.T) {
		k, f, store, _ := endedHold(t, demoCommit, pushRun(22, "e2e2e2e2e2", "success"), pushRun(21, demoCommit, "success"))
		keeperPass(t, k)
		if len(f.jobReruns) != 0 || releaseRow(store, demoRepo).ToldHeld != "" {
			t.Errorf("job re-runs %q, told held %q; want none and cleared: run 22 asks itself", f.jobReruns, releaseRow(store, demoRepo).ToldHeld)
		}
	})
	t.Run("a running run", func(t *testing.T) {
		k, f, store, clock := endedHold(t, demoCommit, pushRun(21, demoCommit, "running"))
		keeperPass(t, k)
		if len(f.jobReruns) != 0 || releaseRow(store, demoRepo).ToldHeld != demoCommit {
			t.Fatalf("job re-runs %q, told held %q; want none and kept while run 21 runs", f.jobReruns, releaseRow(store, demoRepo).ToldHeld)
		}
		f.runs[demoRepo] = []map[string]any{pushRun(21, demoCommit, "success")}
		for _, at := range []int{1, 2} {
			*clock = keeperAt(18, at)
			keeperPass(t, k)
		}
		if !slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) || releaseRow(store, demoRepo).ToldHeld != "" {
			t.Errorf("job re-runs %q, told held %q; want run 21's promote job once, then cleared", f.jobReruns, releaseRow(store, demoRepo).ToldHeld)
		}
	})
	t.Run("no re-run in Forgejo", func(t *testing.T) {
		logs := captureLog(t)
		k, f, store, _ := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
		f.rerun = 404
		keeperPass(t, k)
		if !slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) || releaseRow(store, demoRepo).ToldHeld != "" {
			t.Errorf("job re-runs %q, told held %q; want one try, then cleared", f.jobReruns, releaseRow(store, demoRepo).ToldHeld)
		}
		if !strings.Contains(logs.String(), "release keeper: sf/demo-app: job 212 of sf/demo-app: forgejo offers no re-run") {
			t.Errorf("log %q, want the refused re-run said", logs.String())
		}
	})
	t.Run("a re-run Forgejo refuses", func(t *testing.T) {
		logs := captureLog(t)
		k, f, store, _ := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
		f.rerun = http.StatusUnprocessableEntity
		keeperPass(t, k)
		if !slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) || releaseRow(store, demoRepo).ToldHeld != "" {
			t.Errorf("job re-runs %q, told held %q; want one try, then cleared", f.jobReruns, releaseRow(store, demoRepo).ToldHeld)
		}
		if !strings.Contains(logs.String(), "release keeper: sf/demo-app: forgejo POST /api/v1/repos/sf/demo-app/actions/jobs/212/rerun: forgejo: refused") {
			t.Errorf("log %q, want the refused re-run said", logs.String())
		}
	})
	t.Run("a re-run that fails", func(t *testing.T) {
		captureLog(t)
		k, f, store, _ := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
		store.RecordReleaseAsk(ctx, "sf/zed", "c0c0c0c", false, keeperAt(9, 0))
		openPlanIn(store, "sf/zed", "t-zed", 9, keeperAt(11, 0))
		f.rerun = 500
		if err := k.Pass(ctx); err == nil || !strings.HasPrefix(err.Error(), "sf/demo-app: ") {
			t.Errorf("Pass = %v, want sf/demo-app's failed re-run", err)
		}
		if got := releaseRow(store, "sf/zed").HeldSince; !got.Equal(keeperAt(11, 0)) {
			t.Errorf("sf/zed's clock %v, want 11:00: one repository's error stops no other", got)
		}
		if releaseRow(store, demoRepo).ToldHeld != demoCommit {
			t.Errorf("told held %q, want it kept for the next pass", releaseRow(store, demoRepo).ToldHeld)
		}
	})
	// a failed ask record leaves ToldHeld unwritten: the hold's end runs the
	// promote job of main's newest commit again, which asks again
	t.Run("told held unknown", func(t *testing.T) {
		k, f, _, clock := endedHold(t, "", pushRun(21, demoCommit, "success"))
		keeperPass(t, k)
		*clock = keeperAt(18, 1)
		keeperPass(t, k)
		if !slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) {
			t.Errorf("job re-runs %q, want run 21's promote job once at the hold's end", f.jobReruns)
		}
	})
}

func TestReleaseKeeperDeployNow(t *testing.T) {
	ctx := context.Background()
	k, f, store, clock := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
	delete(store.closed, "t-plan")
	*clock = keeperAt(12, 0)
	commit, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton)
	if err != nil || commit != demoCommit {
		t.Fatalf("DeployNow = %q, %v; want %s", commit, err, demoCommit)
	}
	if got, want := releaseRow(store, demoRepo), (Release{Repository: demoRepo, AsksAt: keeperAt(9, 0), HeldSince: keeperAt(12, 0), ToldHeld: demoCommit, DeployNow: demoCommit}); !reflect.DeepEqual(got, want) {
		t.Errorf("row %+v, want %+v", got, want)
	}
	note := demoRepo + "#77 factoryd: sebastian chose Deploy now: main at 9d9d9d9 goes to production. Closing this issue."
	if !slices.Equal(f.closed, []string{demoRepo + "#77"}) || !slices.Equal(f.comments, []string{note}) {
		t.Errorf("closed %q, comments %q; want #77 closed with %q", f.closed, f.comments, note)
	}
	want := []Event{{At: keeperAt(12, 0), Repository: demoRepo, Kind: EventProductionReleased, Actor: "sebastian", Via: ViaButton,
		Detail: map[string]any{"how": "deploy_now", "held_since": "2026-10-29T10:00:00Z", "hours": 2, "commit": demoCommit}}}
	if got := releasedRows(store); !reflect.DeepEqual(got, want) {
		t.Errorf("rows %+v, want %+v", got, want)
	}

	*clock = keeperAt(12, 1)
	keeperPass(t, k)
	if !slices.Equal(f.jobReruns, []string{demoRepo + " 212"}) || releaseRow(store, demoRepo).ToldHeld != "" {
		t.Errorf("job re-runs %q, told held %q; want run 21's promote job run again while the plan is open", f.jobReruns, releaseRow(store, demoRepo).ToldHeld)
	}
	if len(f.issues) != 0 {
		t.Errorf("issues %q, want no ask", f.issues)
	}

	if _, err := k.DeployNow(ctx, "sf/other", "sebastian", ViaButton); !errors.Is(err, ErrNotHeld) {
		t.Errorf("DeployNow(sf/other) = %v, want ErrNotHeld", err)
	}
	f.runs[demoRepo] = nil
	*clock = keeperAt(12, 5)
	if _, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton); err == nil || errors.Is(err, ErrNotHeld) {
		t.Errorf("DeployNow with no push run = %v, want an error", err)
	}
	if got := releaseRow(store, demoRepo).HeldSince; !got.Equal(keeperAt(12, 0)) {
		t.Errorf("HeldSince %v after a Deploy now with no run, want 12:00 kept", got)
	}
}

// A Deploy now outlives a move of main: while the plan is open, the next
// pass runs the promote job of the released commit's newest run, though
// another commit's run is main's newest now; it waits while that run runs,
// and clears a commit no run read names.
func TestReleaseKeeperDeployNowOutlivesAMoveOfMain(t *testing.T) {
	ctx := context.Background()
	moved := pushRun(22, "e2e2e2e2e2", "running")
	for _, c := range []struct {
		name   string
		runs   []map[string]any
		reruns []string
		told   string
	}{
		{"the released run done", []map[string]any{moved, pushRun(21, demoCommit, "success")}, []string{demoRepo + " 212"}, ""},
		{"the released run running", []map[string]any{moved, pushRun(21, demoCommit, "running")}, nil, demoCommit},
		{"the released run not read", []map[string]any{moved}, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, f, store, clock := endedHold(t, demoCommit, pushRun(21, demoCommit, "success"))
			delete(store.closed, "t-plan")
			*clock = keeperAt(12, 0)
			if _, err := k.DeployNow(ctx, demoRepo, "sebastian", ViaButton); err != nil {
				t.Fatal(err)
			}
			f.runs[demoRepo] = c.runs
			*clock = keeperAt(12, 1)
			keeperPass(t, k)
			if !slices.Equal(f.jobReruns, c.reruns) || releaseRow(store, demoRepo).ToldHeld != c.told {
				t.Errorf("job re-runs %q, told held %q; want %q and %q", f.jobReruns, releaseRow(store, demoRepo).ToldHeld, c.reruns, c.told)
			}
		})
	}
}

func TestReleaseKeeperHold(t *testing.T) {
	ctx := context.Background()
	k, store, _ := newKeeper(t, &chainForgejo{})
	openPlanIn(store, "sf/other", "t-other", 5, keeperAt(9, 0))
	h, err := k.Hold(ctx, demoRepo)
	if err != nil || !h.Since.Equal(keeperAt(10, 0)) || len(h.Groups) != 1 || h.Groups[0].Plan != "t-plan" {
		t.Fatalf("Hold before a pass = %+v, %v; want plan t-plan since 10:00", h, err)
	}
	if _, err := store.PutReleaseHold(ctx, demoRepo, keeperAt(9, 30), time.Time{}, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if h, err := k.Hold(ctx, demoRepo); err != nil || !h.Since.Equal(keeperAt(9, 30)) {
		t.Errorf("Hold = %+v, %v; want the row's clock 09:30", h, err)
	}
	if h, err := k.Hold(ctx, "sf/other"); err != nil || !reflect.DeepEqual(h, Hold{}) {
		t.Errorf("Hold(sf/other) = %+v, %v; want zero: its CI never asked", h, err)
	}
	openPlanIn(store, codeRepo, "t-code", 7, keeperAt(9, 40))
	if h, err := k.Hold(ctx, codeRepo); err != nil || !h.Since.Equal(keeperAt(9, 40)) || !h.Held() {
		t.Errorf("Hold(own) = %+v, %v; want held since 09:40", h, err)
	}
}

func TestReplanChoice(t *testing.T) {
	for _, c := range []struct {
		value   string
		plan    string
		takeOff bool
		ok      bool
	}{
		{"replan_on_top:t-plan", "t-plan", false, true},
		{"take_off_main:t-plan", "t-plan", true, true},
		{"hold_wait", "", false, false},
		{"replan_on_top:", "", false, false},
		{"deploy_anyway:t-plan", "", false, false},
	} {
		plan, takeOff, ok := ReplanChoice(c.value)
		if plan != c.plan || takeOff != c.takeOff || ok != c.ok {
			t.Errorf("ReplanChoice(%q) = (%q, %v, %v), want (%q, %v, %v)", c.value, plan, takeOff, ok, c.plan, c.takeOff, c.ok)
		}
	}
}
