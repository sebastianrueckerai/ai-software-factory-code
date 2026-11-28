package factoryd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

const checkContext = "config-check / check (pull_request)"

// fakeAlertRows are the alert rows the judgment reads.
type fakeAlertRows struct {
	mu   sync.Mutex
	rows []Alert
}

func (f *fakeAlertRows) NewestAlertRows(context.Context, string, string) ([]Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Alert(nil), f.rows...), nil
}

// moveClock is the runner's clock of a judgment: a pause moves it, so the
// waits cost no time.
type moveClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *moveClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *moveClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

// moveRun is a follow run with its pull request opened by the pass, the
// gateway of the model call, the alerts and the probes of the judgment, and
// a clock of its own.
type moveRun struct {
	*followRun
	in      FollowMoveInput
	clock   *moveClock
	alerts  *fakeAlertRows
	probe   []string
	gateway int
}

// newMoveRun is a move run whose pass opened its pull request after the
// targets of the parts attempted had their attempts: attempted litellm moves
// the part cache, by a human, as pull request #41.
func newMoveRun(t *testing.T, attempted ...string) *moveRun {
	t.Helper()
	quickMergeWaits(t)
	m := &moveRun{followRun: newFollowRun(t, behindTree()), clock: &moveClock{now: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}, alerts: &fakeAlertRows{}, gateway: http.StatusOK}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(m.gateway)
		_, _ = w.Write([]byte(answerReply(`{"ok": true}`)))
	}))
	t.Cleanup(gw.Close)
	m.r.supervisor = &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}
	m.r.now, m.r.sleep = m.clock.Now, m.clock.sleep
	m.r.Follow.Alerts = m.alerts
	m.r.Follow.Probe = func(context.Context) []string { return m.probe }
	for _, part := range attempted {
		m.forge.attempt(followBranch(part, map[string]map[string][]byte{"litellm": litellmTarget(), "cache": cacheTarget()}[part], 1), "closed", false, nil)
	}
	m.run(t)
	if len(m.moves.started) != 1 {
		t.Fatalf("the pass started %v", m.moves.started)
	}
	m.in = m.moves.started[0]
	m.forge.statuses[m.head()] = []CommitStatus{{State: "success", Context: checkContext}}
	m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "success", Context: "flux/platform"}}
	return m
}

func (m *moveRun) head() string  { return "head" + strconv.FormatInt(m.in.PR, 10) }
func (m *moveRun) merge() string { return "merge" + strconv.FormatInt(m.in.PR, 10) }

// workflow runs the move's workflow over the runner's activities.
// A human's act, when given, happens in the workflow's time at human.
func (m *moveRun) workflow(t *testing.T, human ...func(f *infraForge)) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	for _, act := range human {
		env.RegisterDelayedCallback(func() {
			m.forge.mu.Lock()
			defer m.forge.mu.Unlock()
			act(m.forge)
		}, 90*time.Second)
	}
	env.RegisterWorkflowWithOptions(FollowMoveWorkflow, workflow.RegisterOptions{Name: "FollowMove"})
	m.r.RegisterActivities(env)
	env.ExecuteWorkflow("FollowMove", m.in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("completed %v, err %v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
}

// judge runs the judgment of the merge commit the move made.
func (m *moveRun) judge(t *testing.T) FollowVerdict {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(m.r.FollowJudge, activity.RegisterOptions{Name: ActivityFollowJudge})
	val, err := env.ExecuteActivity(ActivityFollowJudge, m.in, FollowLanded{Commit: "merge40", At: m.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var v FollowVerdict
	if err := val.Get(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (m *moveRun) lastRun(t *testing.T) RoutineRun {
	t.Helper()
	runs, _ := m.store.RoutineRuns(context.Background(), infraName, "follow", 1)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	return runs[0]
}

func (m *moveRun) steps() []string {
	var out []string
	for _, e := range recordedEvents(m.r, EventFollowStep) {
		out = append(out, e.Detail["step"].(string))
	}
	return out
}

// A move of lane alone: the checks are green, the bot merges, Flux's statuses
// are success on the merge commit, the dependencies and the small class
// answer, nothing alerts for the settle time, and the next run starts at
// once. Each step is a comment on the pull request and an event.
func TestFollowMoveAloneGoodPath(t *testing.T) {
	m := newMoveRun(t)
	m.workflow(t)
	if pr := m.forge.pulls[40]; !pr.Merged {
		t.Fatalf("pull request #40 = %+v, want merged", pr)
	}
	if len(m.forge.merges) != 1 || m.forge.merges[0] != followBranch("litellm", litellmTarget(), 1)+" head40" {
		t.Fatalf("merges = %q, want the merge of %s to name head40, the commit the pass pushed", m.forge.merges, followBranch("litellm", litellmTarget(), 1))
	}
	want := []string{
		"the checks of pull request head head40 are green.",
		"merged as merge40.",
		"the chain is green on merge40 or a later commit of main that holds it: CI and Flux applied it.",
		"the dependencies answer and the small model class answers.",
		"no alert of the project's namespaces fired in the 10m0s after the merge: the move is settled.",
		"done: moved: ghcr.io/berriai/litellm: 1.102.4 to 1.108.17",
	}
	if got := m.steps(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps = %q, want %q", got, want)
	}
	for i, c := range m.forge.comments[40] {
		if c != "Follow routine: "+want[i] {
			t.Errorf("comment %d = %q", i, c)
		}
	}
	run := m.lastRun(t)
	if run.Outcome != "done" || run.Note != "part litellm: moved: ghcr.io/berriai/litellm: 1.102.4 to 1.108.17" {
		t.Fatalf("run = %+v", run)
	}
	if len(m.wf.routineRuns) != 1 || m.wf.routineRuns[0].Trigger != TriggerRunNow || m.wf.routineRuns[0].Routine != "follow" {
		t.Fatalf("next runs = %+v, want one run-now of the follow routine", m.wf.routineRuns)
	}
}

// A red check before the merge ends the move: nothing merges, the run says
// which check and its state.
func TestFollowMoveRedCheckBeforeTheMerge(t *testing.T) {
	m := newMoveRun(t)
	m.forge.statuses["head40"] = []CommitStatus{{State: "failure", Context: checkContext}}
	m.workflow(t)
	if m.forge.pulls[40].Merged {
		t.Fatal("a red pull request merged")
	}
	run := m.lastRun(t)
	if run.Outcome != "failed" || run.Note != "part litellm: the check “config-check / check (pull_request)” of its head head40 has the state failure" {
		t.Fatalf("run = %+v", run)
	}
	if len(m.wf.routineRuns) != 0 {
		t.Fatalf("a failed move started a run: %+v", m.wf.routineRuns)
	}
}

// Each way a judgment fails names what failed with its text.
func TestFollowJudgeFailures(t *testing.T) {
	at := func(m *moveRun, d time.Duration) string { return m.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	for name, tc := range map[string]struct {
		setup func(m *moveRun)
		want  string
	}{
		"a failed status": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "HelmRelease platform/litellm: upgrade retries exhausted"}, {State: "success", Context: "flux/platform"}}
		}, "the chain is red on the merge commit merge40: required context flux/flux-system: failure (HelmRelease platform/litellm: upgrade retries exhausted)"},
		"an errored status": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "error", Context: "flux/platform", Description: "health check failed"}}
		}, "the chain is red on the merge commit merge40: required context flux/platform: error (health check failed)"},
		"a status that never reports": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}}
		}, "the chain is not green on the merge commit merge40 within 30m0s: required context flux/platform: not reported"},
		"a status that stays pending": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "pending", Context: "flux/platform"}}
		}, "the chain is not green on the merge commit merge40 within 30m0s: required context flux/platform: pending"},
		"a dependency probe": {func(m *moveRun) { m.probe = []string{"openbao: sealed"} },
			"a dependency does not answer: openbao: sealed"},
		"the model call": {func(m *moveRun) { m.gateway = http.StatusServiceUnavailable },
			"the small model class does not answer through the gateway: "},
		"an alert in the settle time": {func(m *moveRun) {
			m.alerts.rows = []Alert{{Kind: AlertSigNoz, Source: "5xx rate above 5 %", Detail: map[string]any{"namespace": "platform", "starts_at": at(m, 5*time.Minute), "summary": "5xx at 7 %"}}}
		}, "the alert “5xx rate above 5 %” of namespace platform fired after the merge: 5xx at 7 %"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMoveRun(t)
			m.forge.main = "merge40"
			tc.setup(m)
			m.forge.statuses["merge40"] = m.forge.mergeStatus
			if v := m.judge(t); v.Ok || !strings.HasPrefix(v.Why, tc.want) {
				t.Fatalf("verdict = %+v, want %q", v, tc.want)
			}
		})
	}
	t.Run("the model call after the settle time", func(t *testing.T) {
		m := newMoveRun(t)
		m.forge.statuses["merge40"] = m.forge.mergeStatus
		m.r.sleep = func(ctx context.Context, d time.Duration) error {
			m.gateway = http.StatusServiceUnavailable
			return m.clock.sleep(ctx, d)
		}
		want := "after the 10m0s of settling, the small model class does not answer through the gateway: "
		if v := m.judge(t); v.Ok || !strings.HasPrefix(v.Why, want) {
			t.Fatalf("verdict = %+v, want %q", v, want)
		}
	})
	t.Run("alerts that do not count", func(t *testing.T) {
		m := newMoveRun(t)
		m.forge.statuses["merge40"] = m.forge.mergeStatus
		m.alerts.rows = []Alert{
			{Kind: AlertSigNoz, Source: "before the merge", Detail: map[string]any{"namespace": "platform", "starts_at": at(m, -time.Minute)}},
			{Kind: AlertSigNoz, Source: "another namespace", Detail: map[string]any{"namespace": "factory", "starts_at": at(m, time.Minute)}},
			{Kind: AlertSigNoz, Source: "noise", Verdict: VerdictNoise, Detail: map[string]any{"namespace": "platform", "starts_at": at(m, time.Minute)}},
		}
		if v := m.judge(t); !v.Ok {
			t.Fatalf("verdict = %+v, want a good move", v)
		}
	})
	t.Run("a held row counts", func(t *testing.T) {
		m := newMoveRun(t)
		m.forge.statuses["merge40"] = m.forge.mergeStatus
		m.alerts.rows = []Alert{{Kind: AlertSigNoz, Source: "restarts", Detail: map[string]any{"namespace": "forgejo", "starts_at": at(m, time.Minute), "roll_window": true, "summary": "pods restart"}}}
		if v := m.judge(t); v.Ok || !strings.Contains(v.Why, "“restarts” of namespace forgejo") {
			t.Fatalf("verdict = %+v, want the held alert named", v)
		}
	})
}

// A move of the human lane files the card that asks for a merge, with the
// versions, the notes, what a human does after the merge and the words that
// Reject leaves the part, and goes on when the human merges.
func TestFollowMoveHumanMerges(t *testing.T) {
	followPoll = time.Minute
	t.Cleanup(func() { followPoll, followHumanBound = 5*time.Minute, 14*24*time.Hour })
	m := newMoveRun(t, "litellm")
	m.workflow(t, func(f *infraForge) { f.humanMerges(41) })
	if m.in.Head != "head41" {
		t.Fatalf("head = %q", m.in.Head)
	}
	cards := m.store.tasks
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want the one that asks for the merge", cards)
	}
	var card Task
	for _, c := range cards {
		card = c
	}
	if card.SHA != "head41" {
		t.Fatalf("the card judged head %q, want head41: the console merges only that", card.SHA)
	}
	if card.Source != SourceFollow || card.PRNumber != 41 || card.Kind != "human" || card.Repository != "sf/"+infraName {
		t.Fatalf("card = %+v", card)
	}
	asked := recordedEvents(m.r, EventQuestionAsked)
	if len(asked) != 1 {
		t.Fatalf("asks = %+v", asked)
	}
	issue := asked[0].Detail["ask"].(string) + "\n" + asked[0].Detail["info"].(string)
	for _, want := range []string{
		"Merge the follow routine's move of part cache?",
		"part cache is of the human lane, and moves docker.io/library/registry: 3@000000000000 to 3@888888888888",
		"The maker's release notes: https://example.org/registry/releases",
		"Reject closes the pull request and leaves the part as it is until the lists carry its next version",
	} {
		if !strings.Contains(issue, want) {
			t.Errorf("the card lacks %q:\n%s", want, issue)
		}
	}
	if run := m.lastRun(t); run.Outcome != "done" {
		t.Fatalf("run = %+v, want the move judged and done", run)
	}
	if len(m.wf.routineRuns) != 1 {
		t.Fatalf("next runs = %+v", m.wf.routineRuns)
	}
	if got := m.forge.comments[41][1]; got != "Follow routine: asked a human: card “Merge the follow routine's move of part cache?”." {
		t.Fatalf("second step = %q", got)
	}
}

// A human's Reject closes the pull request: the move ends closed, nothing is
// judged and no run starts.
func TestFollowMoveHumanRejects(t *testing.T) {
	followPoll = time.Minute
	t.Cleanup(func() { followPoll = 5 * time.Minute })
	m := newMoveRun(t, "litellm")
	m.workflow(t, func(f *infraForge) { f.pulls[41].State = "closed" })
	run := m.lastRun(t)
	if run.Outcome != "closed" || run.Note != "part cache: the pull request was closed without a merge: the part stays as it is until the lists carry its next version" {
		t.Fatalf("run = %+v", run)
	}
	if len(m.wf.routineRuns) != 0 || len(m.forge.statuses["merge41"]) != 0 {
		t.Fatalf("a rejected move went on: %+v", m.wf.routineRuns)
	}
}

// A human who never answers: after the bound the factory closes the pull
// request and the move ends.
func TestFollowMoveHumanExpires(t *testing.T) {
	followPoll, followHumanBound = 5*time.Minute, 20*time.Minute
	t.Cleanup(func() { followPoll, followHumanBound = 5*time.Minute, 14*24*time.Hour })
	m := newMoveRun(t, "litellm")
	m.workflow(t)
	if m.forge.pulls[41].State != "closed" || m.forge.pulls[41].Merged {
		t.Fatalf("pull request #41 = %+v, want closed by the factory", m.forge.pulls[41])
	}
	run := m.lastRun(t)
	want := "part cache: no human decided in 20m0s, so the factory closed the pull request: the part stays as it is until the lists carry its next version"
	if run.Outcome != "closed" || run.Note != want {
		t.Fatalf("run = %+v, want %q", run, want)
	}
}

// stubRevert substitutes the helper that reverts on a clone: it records the
// commits and the branch it was asked for and answers conflict.
func stubRevert(t *testing.T, conflict string) *[]string {
	t.Helper()
	prev := groupRevertBranch
	t.Cleanup(func() { groupRevertBranch = prev })
	var calls []string
	groupRevertBranch = func(_ context.Context, _ *Forgejo, repo Repo, shas []string, salvage string, salvageExists bool, branch string) (string, error) {
		calls = append(calls, repo.String()+" "+strings.Join(shas, ",")+" "+branch+" salvage="+salvage)
		return conflict, nil
	}
	return &calls
}

// failureCard is the project's one failure card and its text.
func (m *moveRun) failureCard(t *testing.T) (Task, string) {
	t.Helper()
	var cards []Task
	for _, c := range m.store.tasks {
		if c.Source == SourceFollowFailed {
			cards = append(cards, c)
		}
	}
	if len(cards) != 1 || cards[0].Kind != "human" {
		t.Fatalf("failure cards = %+v, want one human card", cards)
	}
	for _, e := range recordedEvents(m.r, EventQuestionAsked) {
		if e.TaskID == cards[0].ID {
			return cards[0], e.Detail["ask"].(string) + "\n" + e.Detail["info"].(string)
		}
	}
	t.Fatal("the card has no ask")
	return Task{}, ""
}

func wantText(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("the card lacks %q:\n%s", w, got)
		}
	}
}

// A merged move whose judgment fails is returned: the revert of its merge
// commit goes to a pull request that merges as the move did, Flux's statuses
// on it are success, and one failure card tells the part, both versions,
// what failed with its text, that the return held, and the part's words.
func TestFollowMoveJudgmentFailureReturnsAndFilesOneCard(t *testing.T) {
	calls := stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.mergeStatus = []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "HelmRelease platform/litellm: upgrade retries exhausted"}, {State: "success", Context: "flux/platform"}}
	m.forge.revertStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "success", Context: "flux/platform"}}
	m.forge.statuses["head41"] = []CommitStatus{{State: "success", Context: checkContext}}
	m.workflow(t)
	if want := "sf/ai-software-factory-infra merge40 factory/follow-return-merge40 salvage="; len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("reverts = %q, want %q", *calls, want)
	}
	if pr := m.forge.pulls[41]; pr.Head != "factory/follow-return-merge40" || !pr.Merged {
		t.Fatalf("the return = %+v, want its pull request merged", pr)
	}
	if got := m.forge.titles[41]; got != "Return the follow routine's move of part litellm" {
		t.Fatalf("title = %q", got)
	}
	_, text := m.failureCard(t)
	wantText(t, text,
		"The follow move of part litellm failed. Try it again?",
		"part litellm (lane alone) in pull request #40: ghcr.io/berriai/litellm: 1.102.4 to 1.108.17",
		"The move failed: the chain is red on the merge commit merge40: required context flux/flux-system: failure (HelmRelease platform/litellm: upgrade retries exhausted)",
		"The factory returned the move: pull request #41 reverted the merge merge40, and its checks and Flux's statuses are success.",
		"About this part: The migration does not go back.",
		"The maker's release notes: https://example.org/litellm/releases",
		"Try the move again opens a new pull request for the same versions")
	run := m.lastRun(t)
	if run.Outcome != "failed" || !strings.HasPrefix(run.Note, "part litellm: the chain is red on the merge commit merge40") {
		t.Fatalf("run = %+v", run)
	}
	if len(m.wf.routineRuns) != 0 {
		t.Fatalf("a failed move started a run: %+v", m.wf.routineRuns)
	}
}

// A return that does not hold says why on the card: the revert's own status
// failed, so the cluster may run the new versions.
func TestFollowMoveReturnThatDoesNotHold(t *testing.T) {
	stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.mergeStatus = []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "not ready"}, {State: "success", Context: "flux/platform"}}
	m.forge.revertStatus = []CommitStatus{{State: "error", Context: "flux/flux-system", Description: "still not ready"}, {State: "success", Context: "flux/platform"}}
	m.forge.statuses["head41"] = []CommitStatus{{State: "success", Context: checkContext}}
	m.workflow(t)
	_, text := m.failureCard(t)
	wantText(t, text,
		"The factory could not return the move: the revert merged as merge41, and the chain is red on the merge commit merge41: required context flux/flux-system: error (still not ready). Pull request #41, the revert, stands. The cluster may still run the new versions: look at it first.")
}

// A revert that conflicts with main is no return: the card says so.
func TestFollowMoveReturnConflicts(t *testing.T) {
	stubRevert(t, "merge40")
	m := newMoveRun(t)
	m.forge.mergeStatus = []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "not ready"}, {State: "success", Context: "flux/platform"}}
	m.workflow(t)
	_, text := m.failureCard(t)
	wantText(t, text, "The factory could not return the move: the revert of merge40 conflicts with main, which moved on since the merge. The cluster may still run the new versions: look at it first.")
	if len(m.forge.byBranch) != 1 {
		t.Fatalf("pull requests = %v, a conflicting revert opens none", m.forge.byBranch)
	}
}

// A red check before the merge files the same card with no return, and the
// pull request is closed.
func TestFollowMoveRedCheckFilesTheCardWithoutAReturn(t *testing.T) {
	calls := stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.statuses["head40"] = []CommitStatus{{State: "failure", Context: checkContext}}
	m.workflow(t)
	if len(*calls) != 0 || m.forge.pulls[40].State != "closed" || m.forge.pulls[40].Merged {
		t.Fatalf("reverts %v, pull request %+v: want no return and a closed pull request", *calls, m.forge.pulls[40])
	}
	_, text := m.failureCard(t)
	wantText(t, text,
		"The move failed: the check “config-check / check (pull_request)” of its head head40 has the state failure",
		"The pull request did not merge, so nothing was returned.")
}

// While the failure card is open a run does nothing and its line names the
// card, whoever starts it; a second failure files no second card.
func TestFollowFailureCardHoldsTheRuns(t *testing.T) {
	stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.statuses["head40"] = []CommitStatus{{State: "failure", Context: checkContext}}
	m.workflow(t)
	card, _ := m.failureCard(t)
	for _, trigger := range []string{"cron", TriggerRunNow} {
		res, err := m.r.RoutineRun(context.Background(), RoutineRunInput{Project: infraName, Routine: "follow", Trigger: trigger, Due: m.clock.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		want := "held: the failure card " + ShownName(card) + " is open: answer it, and the next run goes on"
		if res.Outcome != "held" || res.Note != want {
			t.Fatalf("%s run = %+v, want %q", trigger, res, want)
		}
	}
	if err := m.r.FollowFail(context.Background(), m.in, FollowFailure{Why: "again"}); err != nil {
		t.Fatal(err)
	}
	m.failureCard(t)
}

// A failure card whose move did not merge closes by itself once the lists
// carry another version of its part: the target's first branch has no pull
// request then, so the run withdraws the card's ask, closes the card and
// goes on to the move of the new target. A card whose merged move the
// factory could not return stays, and holds the runs.
func TestFollowFailureCardClosesOnceItsTargetIsGone(t *testing.T) {
	for name, tc := range map[string]struct {
		held bool
		want string
	}{
		"the old versions hold":          {true, "reported"},
		"the move could not be returned": {false, "held"},
	} {
		t.Run(name, func(t *testing.T) {
			stubRevert(t, "")
			m := newMoveRun(t)
			m.forge.statuses["head40"] = []CommitStatus{{State: "failure", Context: checkContext}}
			m.workflow(t)
			card, _ := m.failureCard(t)
			var asked Event
			for _, e := range recordedEvents(m.r, EventQuestionAsked) {
				if e.TaskID == card.ID {
					asked = e
				}
			}
			if asked.Detail[factFollowPart] != "litellm" || asked.Detail[factFollowHeld] != true {
				t.Fatalf("the ask's facts = %v, want the part litellm and the old versions held", asked.Detail)
			}
			asked.Detail[factFollowHeld] = tc.held
			m.store.cases = []Case{{ID: 1, TaskID: card.ID, Status: CaseAtHuman, Kind: CaseHumanCard, Facts: asked.Detail}}
			delete(m.forge.byBranch, followBranch("litellm", litellmTarget(), 1))
			res, err := m.r.RoutineRun(context.Background(), RoutineRunInput{Project: infraName, Routine: "follow", Trigger: "cron", Due: m.clock.Now().Add(time.Hour)})
			if err != nil || res.Outcome != tc.want {
				t.Fatalf("run = %+v, %v; want outcome %q", res, err, tc.want)
			}
			events, _ := m.store.IntakeEvents(context.Background(), card.ID)
			if !tc.held {
				if len(events) != 0 {
					t.Fatalf("the card's close = %+v, want it open", events)
				}
				return
			}
			const why = "The lists carry another version of part litellm, which the routine's next run moves, so there is nothing left to try again. Closing this issue."
			if len(events) != 1 || events[0].Kind != IntakeFactoryClosed || events[0].Body != why {
				t.Fatalf("the card's close = %+v, want %q", events, why)
			}
		})
	}
}

// Try again opens a new attempt for the same versions, by the counter on
// the branch, and closes the card.
func TestFollowRetryOpensAnotherAttempt(t *testing.T) {
	stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.statuses["head40"] = []CommitStatus{{State: "failure", Context: checkContext}}
	m.workflow(t)
	card, _ := m.failureCard(t)
	var asked Event
	for _, e := range recordedEvents(m.r, EventQuestionAsked) {
		if e.TaskID == card.ID {
			asked = e
		}
	}
	m.store.cases = []Case{{ID: 1, TaskID: card.ID, Status: CaseOpen, Kind: CaseHumanCard, Facts: asked.Detail}}
	m.moves.started = nil
	if err := m.r.RetryFollow(context.Background(), card); err != nil {
		t.Fatal(err)
	}
	branch := followBranch("litellm", litellmTarget(), 2)
	if n, ok := m.forge.byBranch[branch]; !ok || n != 41 {
		t.Fatalf("branches = %v, want %s as pull request #41", m.forge.byBranch, branch)
	}
	if len(m.moves.started) != 1 || m.moves.started[0].PR != 41 || m.moves.started[0].Run != 0 || m.moves.started[0].Part != "litellm" {
		t.Fatalf("started = %+v", m.moves.started)
	}
	events, _ := m.store.IntakeEvents(context.Background(), card.ID)
	if len(events) != 1 || events[0].Kind != IntakeFactoryClosed || !strings.Contains(events[0].Body, "attempt 2 of part litellm") {
		t.Fatalf("the card's close = %+v", events)
	}
	// a third attempt takes the next counter
	if err := m.r.RetryFollow(context.Background(), card); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.forge.byBranch[followBranch("litellm", litellmTarget(), 3)]; !ok {
		t.Fatalf("branches = %v, want attempt 3", m.forge.byBranch)
	}
}

// A push to the branch after the pass wrote it is not checked and not
// merged: the move fails on the head it did not push, the pull request
// closes, and nothing merges.
func TestFollowMoveRefusesAPushedHead(t *testing.T) {
	m := newMoveRun(t)
	m.forge.pulls[40].HeadSHA = "pushed1"
	m.forge.statuses["pushed1"] = []CommitStatus{{State: "success", Context: checkContext}}
	m.workflow(t)
	run := m.lastRun(t)
	want := "part litellm: the head of pull request #40 is pushed1, and the pass pushed head40: a push since then is not checked"
	if run.Outcome != "failed" || run.Note != want || len(m.forge.merges) != 0 || m.forge.pulls[40].State != "closed" {
		t.Fatalf("run = %+v, merges %v, pull request %+v; want %q and nothing merged", run, m.forge.merges, m.forge.pulls[40], want)
	}
}

// A push between the checks and the merge cannot ride in: the merge is
// vetted against the pushed head and names it, and Forgejo's own head check
// refuses any other.
func TestFollowLandVetsTheHead(t *testing.T) {
	m := newMoveRun(t)
	m.forge.pulls[40].HeadSHA = "pushed1"
	m.forge.statuses["pushed1"] = []CommitStatus{{State: "success", Context: checkContext}}
	landed, err := m.r.FollowLand(context.Background(), m.in)
	if err != nil || landed.Why != "the head of pull request #40 is pushed1, and the pass pushed head40: a push since then is not checked" || len(m.forge.merges) != 0 {
		t.Fatalf("landed = %+v, %v, merges %v", landed, err, m.forge.merges)
	}
}

// The return's revert is checked and merged at the head read right after
// its push, and its merge names it.
func TestFollowReturnMergesThePushedHead(t *testing.T) {
	stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.mergeStatus = []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "x"}, {State: "success", Context: "flux/platform"}}
	m.forge.revertStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "success", Context: "flux/platform"}}
	m.forge.statuses["head41"] = []CommitStatus{{State: "success", Context: checkContext}}
	m.workflow(t)
	if got := m.forge.merges; len(got) != 2 || got[1] != "factory/follow-return-merge40 head41" {
		t.Fatalf("merges = %q, want the return's to name head41", got)
	}
}

// Flux posts its status on the revision it applied, which is main's head: a
// move whose merge commit Flux skipped is judged on the later commit of main
// that contains it. A later commit that does not contain it counts for
// nothing.
func TestFollowJudgeReadsLaterCommitsOfMain(t *testing.T) {
	good := []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "success", Context: "flux/platform"}}
	for name, tc := range map[string]struct {
		holds  bool
		status []CommitStatus
		want   string // "" is a good move
	}{
		"success on a later commit": {true, good, ""},
		"failure on a later commit": {true, []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "not ready"}, {State: "success", Context: "flux/platform"}},
			"the chain is red on commit laterc2 after the merge commit merge40: required context flux/flux-system: failure (not ready)"},
		"a later commit that lacks the merge": {false, good, "the chain is not green on the merge commit merge40 within 30m0s: required context flux/flux-system: not reported; required context flux/platform: not reported"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMoveRun(t)
			m.forge.main, m.forge.later, m.forge.mainHolds = "laterc2", []string{"laterc2", "laterc1"}, tc.holds
			m.forge.statuses["laterc2"] = tc.status
			v := m.judge(t)
			if (tc.want == "") != v.Ok || !strings.HasPrefix(v.Why, tc.want) {
				t.Fatalf("verdict = %+v, want %q", v, tc.want)
			}
		})
	}
}

const pushCheck = "config-check / config-check (push)"

// A move whose merge commit passes Flux but fails the repository's own push
// check is no good move: the chain is judged as the chain poller judges a
// head, so the red CI run fails the judgment at once, the revert goes out,
// and the failure card names the run with its link and the check with its text.
func TestFollowMoveRedPushCheckFailsAndReturns(t *testing.T) {
	stubRevert(t, "")
	m := newMoveRun(t)
	m.forge.runs["merge40"] = []ActionRun{{Index: 12, Status: "failure"}}
	m.forge.mergeStatus = append(m.forge.mergeStatus, CommitStatus{State: "failure", Context: pushCheck, Description: "the store's copy of a line or a bootstrap line by its digest"})
	m.forge.runs["merge41"] = []ActionRun{{Index: 13, Status: "success"}}
	m.forge.revertStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "success", Context: "flux/platform"}, {State: "success", Context: pushCheck}}
	m.forge.statuses["head41"] = []CommitStatus{{State: "success", Context: checkContext}}
	m.workflow(t)
	if !m.forge.pulls[41].Merged {
		t.Fatalf("the return = %+v, want its pull request merged", m.forge.pulls[41])
	}
	_, text := m.failureCard(t)
	wantText(t, text,
		"The move failed: the chain is red on the merge commit merge40: CI run 12 failure (https://git.example/sf/ai-software-factory-infra/actions/runs/12); check “config-check / config-check (push)” failure (the store's copy of a line or a bootstrap line by its digest)",
		"The factory returned the move: pull request #41 reverted the merge merge40, and its checks and Flux's statuses are success.")
}

// A CI run that is still running is waited for within the cap, and the move
// is good once it succeeds.
func TestFollowJudgeWaitsForARunningPushRun(t *testing.T) {
	m := newMoveRun(t)
	m.forge.statuses["merge40"] = m.forge.mergeStatus
	m.forge.runs["merge40"] = []ActionRun{{Index: 12, Status: "running"}}
	pauses := 0
	m.r.sleep = func(ctx context.Context, d time.Duration) error {
		pauses++
		if pauses == 3 {
			m.forge.mu.Lock()
			m.forge.runs["merge40"] = []ActionRun{{Index: 12, Status: "success"}}
			m.forge.mu.Unlock()
		}
		return m.clock.sleep(ctx, d)
	}
	if v := m.judge(t); !v.Ok || pauses < 3 {
		t.Fatalf("verdict = %+v after %d pauses, want a good move once the run succeeds", v, pauses)
	}
	// a run that never ends fails the move at the cap, naming the run
	m = newMoveRun(t)
	m.forge.statuses["merge40"] = m.forge.mergeStatus
	m.forge.runs["merge40"] = []ActionRun{{Index: 12, Status: "running"}}
	want := "the chain is not green on the merge commit merge40 within 30m0s: CI run 12 running (https://git.example/sf/ai-software-factory-infra/actions/runs/12)"
	if v := m.judge(t); v.Ok || v.Why != want {
		t.Fatalf("verdict = %+v, want %q", v, want)
	}
}

// Green on a later commit of main that holds the merge makes the move good,
// whatever the merge commit's own run said.
func TestFollowJudgeLaterGreenCommitIsGood(t *testing.T) {
	m := newMoveRun(t)
	m.forge.main, m.forge.later, m.forge.mainHolds = "laterc2", []string{"laterc2"}, true
	m.forge.runs["merge40"] = []ActionRun{{Index: 12, Status: "failure"}}
	m.forge.runs["laterc2"] = []ActionRun{{Index: 14, Status: "success"}}
	m.forge.statuses["laterc2"] = m.forge.mergeStatus
	if v := m.judge(t); !v.Ok {
		t.Fatalf("verdict = %+v, want a good move", v)
	}
}
