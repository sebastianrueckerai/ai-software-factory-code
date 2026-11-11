package factoryd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// rollRetryRig is a quiet main whose one commit's roll fails: clock is the
// roller's and the store's, advanced by the test.
type rollRetryRig struct {
	t      *testing.T
	f      *rollForgejo
	roller *Roller
	store  *fakeStore
	wf     *fakeWorkflows
	clock  time.Time
}

func newRollRetryRig(t *testing.T, f *rollForgejo) *rollRetryRig {
	t.Helper()
	roller, store, wf := newTestRoller(t, f)
	g := &rollRetryRig{t: t, f: f, roller: roller, store: store, wf: wf, clock: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	store.rollClock = func() time.Time { return g.clock }
	return g
}

func (g *rollRetryRig) poll() {
	g.t.Helper()
	if err := g.roller.poll(context.Background()); err != nil {
		g.t.Fatal(err)
	}
}

// fail ends the newest open roll task failed and polls, so the request settles.
func (g *rollRetryRig) fail() {
	g.t.Helper()
	for _, r := range g.store.rolls {
		if r.State == RollOpened {
			task := g.store.tasks[r.TaskID]
			task.Status = "failed"
			g.store.tasks[r.TaskID] = task
		}
	}
	g.poll()
}

func (g *rollRetryRig) advance(d time.Duration) { g.clock = g.clock.Add(d) }

func retryForgejo() *rollForgejo {
	statuses := imageGreen(commitC1, commitC2)
	return &rollForgejo{
		runs:     []map[string]any{pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: statuses,
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
			"sf/factory-gateway/manifests/c2c2c2c": gwNewIdx,
			"sf/opencode-worker/manifests/c2c2c2c": workerRebuilt,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
}

func (g *rollRetryRig) states() string {
	var out []string
	for _, r := range g.store.rolls {
		out = append(out, r.SHA[:2]+":"+r.State+":"+string(rune('0'+len(r.FailedTasks))))
	}
	return strings.Join(out, " ")
}

// One failed roll on a quiet main: no retry before the backoff, one roll of
// the same commit after it, and a poll after that files nothing more.
func TestAFailedRollOnAQuietMainIsRetriedAfterTheBackoff(t *testing.T) {
	g := newRollRetryRig(t, retryForgejo())
	g.poll()
	g.fail()
	g.advance(rollRetryBackoff - time.Second)
	g.poll()
	if len(g.wf.started) != 1 {
		t.Fatalf("started %v before the backoff ended, want the first roll only", g.wf.started)
	}
	g.advance(time.Second)
	g.poll()
	g.poll()
	if len(g.wf.started) != 2 || g.states() != "c1:opened:1" {
		t.Fatalf("started %v, requests %s, want one retry of c1 counting one failure", g.wf.started, g.states())
	}
	if g.store.rolls[0].TaskID != g.wf.started[1] {
		t.Fatalf("the request's task %s, want the retry's %s", g.store.rolls[0].TaskID, g.wf.started[1])
	}
}

// Three failures climb the ladder: attempt 1, retries at +10 min and +20
// min, then no fourth, and the one question stands.
func TestRollRetriesStopAtTheAttemptBound(t *testing.T) {
	g := newRollRetryRig(t, retryForgejo())
	g.poll()
	for range rollAttemptBound - 1 {
		g.fail()
		g.advance(rollRetryBackoff)
		g.poll()
	}
	if len(g.wf.started) != rollAttemptBound || g.states() != "c1:opened:2" {
		t.Fatalf("started %d, requests %s, want three attempts and two counted failures", len(g.wf.started), g.states())
	}
	g.fail()
	for range 3 {
		g.advance(24 * time.Hour)
		g.poll()
	}
	asks := 0
	for _, task := range g.store.tasks {
		if task.Kind == "human" && strings.Contains(task.Objective, rollAskMarker) {
			asks++
		}
	}
	if len(g.wf.started) != 3 || g.states() != "c1:failed:2" || asks != 1 {
		t.Fatalf("started %d, requests %s, %d questions, want no fourth attempt and one question", len(g.wf.started), g.states(), asks)
	}
}

// A new green head during the backoff rolls on its own; the old head's
// retry never runs, and once the new head's roll fails the retry is the new
// head's.
func TestANewGreenHeadTakesThePlaceOfTheRetry(t *testing.T) {
	f := retryForgejo()
	g := newRollRetryRig(t, f)
	g.poll()
	g.fail()
	g.advance(5 * time.Minute)
	f.mu.Lock()
	f.runs = []map[string]any{pushRun(58, commitC2, "success"), pushRun(57, commitC1, "success")}
	f.mu.Unlock()
	g.poll()
	if g.states() != "c1:failed:0 c2:opened:0" {
		t.Fatalf("requests %s, want c2 rolled and c1 left failed", g.states())
	}
	g.advance(time.Hour)
	g.poll()
	if len(g.wf.started) != 2 {
		t.Fatalf("started %v, want no retry of c1 beside c2's open roll", g.wf.started)
	}
	g.fail()
	g.advance(rollRetryBackoff)
	g.poll()
	if g.states() != "c1:failed:0 c2:opened:1" {
		t.Fatalf("requests %s, want c2 retried and c1 never", g.states())
	}
}

// A restart during the backoff: a new Roller over the same store waits out
// the remaining backoff, retries once when it ends, and a second new Roller
// after that retries nothing.
func TestARestartDuringTheBackoffRetriesOnce(t *testing.T) {
	g := newRollRetryRig(t, retryForgejo())
	g.poll()
	g.fail()
	g.advance(4 * time.Minute)
	g.roller = restarted(g.roller)
	g.poll()
	if len(g.wf.started) != 1 {
		t.Fatalf("started %v after the restart, want the backoff kept", g.wf.started)
	}
	g.advance(6 * time.Minute)
	g.poll()
	g.roller = restarted(g.roller)
	g.poll()
	if len(g.wf.started) != 2 || g.states() != "c1:opened:1" {
		t.Fatalf("started %v, requests %s, want exactly one retry", g.wf.started, g.states())
	}
}

func restarted(r *Roller) *Roller {
	return &Roller{Forgejo: r.Forgejo, Registry: r.Registry, SigningKey: r.SigningKey, Store: r.Store, Workflows: r.Workflows,
		Repo: r.Repo, Token: r.Token, Budget: r.Budget, Scrub: redact.New(), Flux: &fakeFluxSwitch{}}
}

func (g *rollRetryRig) asks() []Task {
	var out []Task
	for _, task := range g.store.tasks {
		if task.Kind == "human" && strings.Contains(task.Objective, rollAskMarker) {
			out = append(out, task)
		}
	}
	return out
}

// A filing error on the third attempt does not forget the failures before
// it: the request goes back to the failed attempt it overwrote, waits out
// its backoff, and the third roll card is the last before the human is
// asked.
func TestAFilingErrorOnARetryKeepsTheLadder(t *testing.T) {
	g := newRollRetryRig(t, retryForgejo())
	g.poll()
	g.fail()
	g.advance(rollRetryBackoff)
	g.poll()
	g.fail()
	g.advance(rollRetryBackoff)
	g.store.createFails = 1
	if err := g.roller.poll(context.Background()); err == nil {
		t.Fatal("the filing error was swallowed")
	}
	if g.states() != "c1:failed:1" || len(g.wf.started) != 2 {
		t.Fatalf("requests %s, started %d after the filing error, want the second failure restored", g.states(), len(g.wf.started))
	}
	g.poll()
	if len(g.wf.started) != 2 {
		t.Fatalf("started %d right after the error, want the backoff again", len(g.wf.started))
	}
	g.advance(rollRetryBackoff)
	g.poll()
	g.fail()
	for range 3 {
		g.advance(24 * time.Hour)
		g.poll()
	}
	if len(g.wf.started) != rollAttemptBound || len(g.asks()) != 1 {
		t.Fatalf("started %d roll cards, %d questions, want %d and one", len(g.wf.started), len(g.asks()), rollAttemptBound)
	}
}

// A first attempt that fails to file leaves nothing behind.
func TestAFilingErrorOnAFirstAttemptLeavesNoRequest(t *testing.T) {
	g := newRollRetryRig(t, retryForgejo())
	g.store.createFails = 1
	if err := g.roller.poll(context.Background()); err == nil {
		t.Fatal("the filing error was swallowed")
	}
	if len(g.store.rolls) != 0 {
		t.Fatalf("requests %s, want none", g.states())
	}
}

// The question at the bound names every attempt with its own card.
func TestTheBoundQuestionNamesEachAttempt(t *testing.T) {
	g := newRollRetryRig(t, retryForgejo())
	g.poll()
	for range rollAttemptBound - 1 {
		g.fail()
		g.advance(rollRetryBackoff)
		g.poll()
	}
	g.fail()
	asks := g.asks()
	if len(asks) != 1 {
		t.Fatalf("%d questions, want one", len(asks))
	}
	var cards []string
	for _, line := range strings.Split(asks[0].Objective, "\n") {
		if strings.HasPrefix(line, "- run 57: ") {
			_, card, _ := strings.Cut(line, "(card #")
			cards = append(cards, card)
		}
	}
	if len(cards) != 3 || cards[0] == cards[1] || cards[1] == cards[2] || cards[0] == cards[2] || cards[0] == "" {
		t.Fatalf("attempt lines %q, want three with distinct cards", cards)
	}
}

// A newer head that is tested but whose images are not green yet is not
// preceded by the old head's retry.
func TestARetryWaitsForANewerHeadWhoseImagesBuild(t *testing.T) {
	f := retryForgejo()
	f.statuses[commitC2] = []map[string]string{{"status": "success", "context": testContext, "target_url": "https://git.example"}}
	g := newRollRetryRig(t, f)
	g.poll()
	g.fail()
	f.mu.Lock()
	f.runs = []map[string]any{pushRun(58, commitC2, "running"), pushRun(57, commitC1, "success")}
	f.mu.Unlock()
	g.advance(time.Hour)
	g.poll()
	if len(g.wf.started) != 1 || g.states() != "c1:failed:0" {
		t.Fatalf("started %d, requests %s, want no retry beside a newer head", len(g.wf.started), g.states())
	}
}

// A retry the roller refuses, whose human card fails to file, goes back to
// the failed attempt it overwrote.
func TestARefusedRetryWhoseCardFailsKeepsItsFailure(t *testing.T) {
	f := retryForgejo()
	g := newRollRetryRig(t, f)
	g.poll()
	first := g.store.rolls[0].TaskID
	g.fail()
	f.mu.Lock()
	delete(f.registry, "sf/factory-gateway/manifests/sha256-"+strings.TrimPrefix(sha256Of(gwNewIdx), "sha256:"))
	f.mu.Unlock()
	g.advance(rollRetryBackoff)
	g.store.createFails = 1
	if err := g.roller.poll(context.Background()); err == nil {
		t.Fatal("the card's filing error was swallowed")
	}
	if g.states() != "c1:failed:0" || g.store.rolls[0].TaskID != first || g.store.rolls[0].State != RollFailed {
		t.Fatalf("requests %s task %s, want the failed attempt under %s back", g.states(), g.store.rolls[0].TaskID, first)
	}
}
