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
	roller.Now = func() time.Time { return g.clock }
	store.rollClock = roller.Now
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
			g.store.tasks[r.TaskID] = Task{ID: r.TaskID, Source: SourceRoll, Status: "failed", Repository: codeRepo}
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
		out = append(out, r.SHA[:2]+":"+r.State+":"+string(rune('0'+r.Retries)))
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
		Repo: r.Repo, Token: r.Token, Budget: r.Budget, Scrub: redact.New(), Flux: &fakeFluxSwitch{}, Now: r.Now}
}
