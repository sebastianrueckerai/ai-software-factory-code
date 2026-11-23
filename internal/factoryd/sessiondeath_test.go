package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// scriptedKernel answers each call, counted from 1: kills names its
// lines, fail its error, and block makes the call wait for its context's
// end. It keeps the pod UID each call named.
type scriptedKernel struct {
	mu    sync.Mutex
	calls int
	uids  []string
	kills func(call int) []KernelKill
	fail  func(call int) error
	block func(call int) bool
}

func (k *scriptedKernel) KernelLines(ctx context.Context, _, _ time.Time, podUID string) ([]KernelKill, error) {
	k.mu.Lock()
	k.calls++
	call := k.calls
	k.uids = append(k.uids, podUID)
	k.mu.Unlock()
	if k.block != nil && k.block(call) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k.fail != nil {
		if err := k.fail(call); err != nil {
			return nil, err
		}
	}
	if k.kills == nil {
		return nil, nil
	}
	return k.kills(call), nil
}

func (k *scriptedKernel) called() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

// deathRig is a runner whose session pods run on node sf-1 (boot b-1,
// Ready) with UID deathUID, beside a cluster store and a kernel source.
type deathRig struct {
	taskID  string
	r       *Runner
	kube    *fakeKube
	store   *fakeStore
	cluster *fakeClusterStore
	kernel  *scriptedKernel
	objects *fakeObjectStore
}

func newDeathRig(t *testing.T, taskID string, pod func(p *fakePod)) deathRig {
	t.Helper()
	kube := newFakeKube()
	kube.nodeStates = map[string]NodeState{"sf-1": {Name: "sf-1", BootID: "b-1", Ready: true}}
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.uid, p.node = deathUID, "sf-1"
		if pod != nil {
			pod(p)
		}
	}
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, Task{ID: taskID, Status: "running", Objective: "x", Repository: "sf/repo", Track: "direct"}, kube, objects)
	g := deathRig{taskID: taskID, r: r, kube: kube, store: store, cluster: newFakeClusterStore(), kernel: &scriptedKernel{}, objects: objects}
	r.cfg.Cluster, r.cfg.Kernel, r.deathWait = g.cluster, g.kernel, 200*time.Millisecond
	return g
}

func (g deathRig) run(t *testing.T, body string) SessionResult {
	t.Helper()
	res, err := runSession(t, nil, g.r, SessionInput{TaskID: g.taskID, Attempt: 1, Model: "m"}, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// stored is the death record the cluster store holds for the session.
func (g deathRig) stored(t *testing.T, taskID, session string) DeathRecord {
	t.Helper()
	rec, ok, err := g.cluster.SessionDeath(context.Background(), taskID, session)
	if err != nil || !ok {
		t.Fatalf("death of %s/%s: %v, %v; want it stored", taskID, session, ok, err)
	}
	return rec
}

func stoppedResultLine(summary string) string {
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: summary, Stopped: true}})
	return string(b)
}

var modelCall = envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n"

// placeSpy keeps the live row as each write of the pod's place left it,
// and the owners of the stops written to it.
type placeSpy struct {
	Store
	mu    sync.Mutex
	puts  []LiveSession
	rows  []LiveSession
	stops []string
}

func (s *placeSpy) SetLiveSessionStopping(ctx context.Context, taskID, owner string, at time.Time) error {
	s.mu.Lock()
	s.stops = append(s.stops, owner)
	s.mu.Unlock()
	return s.Store.SetLiveSessionStopping(ctx, taskID, owner, at)
}

func (s *placeSpy) PutLiveSession(ctx context.Context, l LiveSession) error {
	s.mu.Lock()
	s.puts = append(s.puts, l)
	s.mu.Unlock()
	return s.Store.PutLiveSession(ctx, l)
}

func (s *placeSpy) SetLiveSessionPod(ctx context.Context, taskID, owner, uid, node, boot string) error {
	err := s.Store.SetLiveSessionPod(ctx, taskID, owner, uid, node, boot)
	l, _, _ := s.Store.LiveSession(ctx, taskID)
	s.mu.Lock()
	s.rows = append(s.rows, l)
	s.mu.Unlock()
	return err
}

// The pod's place is known once it runs: the row written at the pod's
// creation names none, and once the pod left Pending the row names its
// UID, its node and the node's boot, the rest of it as it was.
func TestPodStartRecordsItsPlace(t *testing.T) {
	g := newDeathRig(t, "t5p", nil)
	spy := &placeSpy{Store: g.store}
	g.r.store = spy
	g.run(t, resultLine(true, "no changes to push"))
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.puts) != 1 || spy.puts[0].PodUID != "" || spy.puts[0].Node != "" || spy.puts[0].BootID != "" {
		t.Fatalf("rows put %+v, want the creation's alone, with no place yet", spy.puts)
	}
	want := spy.puts[0]
	want.PodUID, want.Node, want.BootID = deathUID, "sf-1", "b-1"
	if len(spy.rows) != 1 || !reflect.DeepEqual(spy.rows[0], want) {
		t.Fatalf("the row after the start = %+v, want %+v", spy.rows, want)
	}
}

// A pod the node's memory guard killed ends Error 137 with no result: the
// kernel line names its UID, so the session is the platform's death, its
// record is stored, and the timeline's end and its fact row name the cause.
func TestAGuardKillIsAPlatformDeath(t *testing.T) {
	g := newDeathRig(t, "t5g", func(p *fakePod) {
		p.containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, {Name: "worker", State: "terminated", Reason: "Error", ExitCode: 137}}
	})
	at := time.Now().Add(time.Second)
	g.kernel.kills = func(int) []KernelKill {
		return []KernelKill{{Source: "guard", Node: "sf-1", PodUID: deathUID, At: at, Seq: 711},
			{Source: "guard", Node: "sf-1", PodUID: "another-pod", At: at, Seq: 712}}
	}
	res := g.run(t, modelCall)
	if res.Outcome != OutcomePlatformKilled || res.Death == nil || res.Death.Class != DeathPlatform || res.Death.Rule != RuleGuardKill {
		t.Fatalf("result = %+v, death %+v; want platform_killed by guard_kill", res, res.Death)
	}
	want := "Your previous session did not fail on its own. The node's memory guard killed its pod on sf-1 at "
	if len(res.Death.Lines) != 1 || !strings.HasPrefix(res.Death.Lines[0], want) {
		t.Fatalf("lines = %q, want the class line", res.Death.Lines)
	}
	if rec := g.stored(t, "t5g", res.SessionID); rec.Rule != RuleGuardKill || rec.PodUID != deathUID || rec.Node != "sf-1" || rec.KillSource != "guard" || rec.ExitCode != 137 {
		t.Fatalf("stored = %+v, want the guard kill of %s on sf-1", rec, deathUID)
	}
	end := timelineStep(t, g.objects, "t5g", "session_end")
	if end["outcome"] != string(OutcomePlatformKilled) || end["cause"] != "the node's memory guard killed the pod" {
		t.Fatalf("session_end = %v, want platform_killed with its cause", end)
	}
	var fact *Event
	for i, e := range g.store.events {
		if e.Kind == EventSessionEnd {
			fact = &g.store.events[i]
		}
	}
	if fact == nil || fact.Detail["cause"] != "the node's memory guard killed the pod" {
		t.Fatalf("session_end fact = %+v, want the cause", fact)
	}
}

// A pod that reached its own memory limit is the session's own use: it
// stays an attempt, and its line names the limit and the command it was
// killed in. opencode emits a bash call as a running tool call and an
// EventCommand only when it completes; a call the sandbox killed the worker
// in never completes. The line names the open call's command, else the last
// completed one.
func TestOwnMemoryLimitStaysAnAttempt(t *testing.T) {
	completed := func(id, cmd string) string {
		return envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(),
			Detail: map[string]any{"call_id": id, "command": cmd, "status": "completed", "exit": 0}}) + "\n"
	}
	running := func(id, cmd string) string {
		return envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(),
			Detail: map[string]any{"call_id": id, "tool": "bash", "status": "running", "args": map[string]any{"command": cmd}}}) + "\n"
	}
	errored := func(id, cmd string) string {
		return envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(),
			Detail: map[string]any{"call_id": id, "tool": "bash", "status": "error", "args": map[string]any{"command": cmd}}}) + "\n"
	}
	for name, tc := range map[string]struct{ logs, command, line string }{
		"killed in a running call": {completed("c1", "gofmt -l .") + running("c2", "go test -race ./..."), "go test -race ./...",
			"Your previous session's pod reached its 4 GiB memory limit while it ran `go test -race ./...`, and the kernel killed it."},
		"the running call completed first": {running("c1", "gofmt -l .") + completed("c1", "gofmt -l ."), "gofmt -l .",
			"Your previous session's pod reached its 4 GiB memory limit while it ran `gofmt -l .`, and the kernel killed it."},
		"a call that ended in error": {running("c1", "sleep 9999") + errored("c1", "sleep 9999") + running("c2", "go vet ./...") + completed("c2", "go vet ./..."), "go vet ./...",
			"Your previous session's pod reached its 4 GiB memory limit while it ran `go vet ./...`, and the kernel killed it."},
		"no command at all": {"", "", "Your previous session's pod reached its 4 GiB memory limit, and the kernel killed it."},
	} {
		t.Run(name, func(t *testing.T) {
			g := newDeathRig(t, "t6k", func(p *fakePod) {
				p.containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"},
					{Name: "worker", State: "terminated", Reason: "OOMKilled", ExitCode: 137, MemoryLimit: "4Gi"}}
			})
			res := g.run(t, tc.logs)
			d := res.Death
			if res.Outcome != OutcomeNoPush || d == nil || d.Class != DeathOwnLimit || d.Rule != RuleMemoryLimit {
				t.Fatalf("result = %+v, death %+v; want no_push with an own memory limit", res, d)
			}
			if d.Reason != "OOMKilled" || d.ExitCode != 137 || d.Container != "worker" || d.Limit != "4Gi" || d.LastCommand != tc.command {
				t.Fatalf("death = %+v, want the worker's OOMKilled 137 at 4Gi in %q", d, tc.command)
			}
			if len(d.Lines) != 1 || d.Lines[0] != tc.line {
				t.Fatalf("lines = %q, want %q", d.Lines, tc.line)
			}
			if end := timelineStep(t, g.objects, "t6k", "session_end"); end["cause"] != "the pod reached its memory limit" {
				t.Fatalf("session_end = %v, want the memory limit as its cause", end)
			}
		})
	}
}

// The kubelet's eviction for a volume's own limit is the session's own use.
func TestTmpEvictionIsOwnLimit(t *testing.T) {
	const msg = `Usage of EmptyDir volume "tmp" exceeds the limit "4Gi". `
	g := newDeathRig(t, "t5t", func(p *fakePod) { p.reason, p.message = "Evicted", msg })
	res := g.run(t, modelCall)
	if res.Outcome != OutcomeNoPush || res.Summary != "evicted: "+msg || res.Death == nil ||
		res.Death.Class != DeathOwnLimit || res.Death.Rule != RuleStorageLimit || res.Death.Resource != "tmp" || res.Death.Limit != "4Gi" {
		t.Fatalf("result = %+v, death %+v; want no_push on the tmp volume's 4Gi", res, res.Death)
	}
}

// The kubelet's eviction for the node's pressure is the platform's.
func TestNodePressureEvictionIsPlatform(t *testing.T) {
	const msg = "The node was low on resource: ephemeral-storage. Threshold quantity: 10Gi, available: 9Gi."
	g := newDeathRig(t, "t5n", func(p *fakePod) { p.reason, p.message = "Evicted", msg })
	res := g.run(t, modelCall)
	if res.Outcome != OutcomePlatformKilled || res.Summary != "evicted: "+msg || res.Death == nil ||
		res.Death.Rule != RuleNodePressure || res.Death.Resource != "ephemeral-storage" {
		t.Fatalf("result = %+v, death %+v; want platform_killed on the node's ephemeral storage", res, res.Death)
	}
}

// A worker that reports a stop factoryd did not send got its SIGTERM from
// the cluster: a pod a preemption took is the platform's death. One with no
// cluster cause keeps its outcome, its record unknown.
func TestAClusterStopIsAPlatformDeath(t *testing.T) {
	preempted := newDeathRig(t, "t5c", func(p *fakePod) {
		p.conditions = []PodCondition{{Type: "DisruptionTarget", Status: "True", Reason: "PreemptionByScheduler"}}
	})
	res := preempted.run(t, modelCall+stoppedResultLine("stopped; no work in progress to push"))
	if res.Outcome != OutcomePlatformKilled || res.Death == nil || res.Death.Rule != RuleDisruption || res.Death.Condition != "PreemptionByScheduler" {
		t.Fatalf("result = %+v, death %+v; want platform_killed by the preemption", res, res.Death)
	}
	preempted.stored(t, "t5c", res.SessionID)

	plain := newDeathRig(t, "t5d", nil)
	res = plain.run(t, modelCall+stoppedResultLine("stopped; no work in progress to push"))
	if res.Outcome != OutcomeNoPush || res.Death == nil || res.Death.Class != DeathUnknown {
		t.Fatalf("result = %+v, death %+v; want no_push with no cluster cause", res, res.Death)
	}
}

// factoryd's own stop deletes the pod: the deletion is no platform death.
func TestTheFactorysStopIsNoPlatformDeath(t *testing.T) {
	g := newDeathRig(t, "t5s", func(p *fakePod) {
		p.containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, {Name: "worker", State: "terminated", Reason: "Error", ExitCode: 143}}
	})
	g.kube.keepDeleted = true
	g.r.stopWait = 5 * time.Second
	spy := &placeSpy{Store: g.store}
	g.r.store = spy
	setDetectors(g.r, map[string]int{policy.DetectorNoProgress: 3})
	setLimits(g.r, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	res := g.run(t, modelCall+sigtermLogs)
	if res.Outcome != OutcomeWallClock || res.Death == nil || res.Death.Class == DeathPlatform {
		t.Fatalf("result = %+v, death %+v; want wall_clock and no platform death", res, res.Death)
	}
	if rec := g.stored(t, "t5s", res.SessionID); rec.Class == DeathPlatform {
		t.Fatalf("stored = %+v, want no platform death", rec)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.stops) != 1 || spy.stops[0] != "own" {
		t.Fatalf("stops written %q, want the stop on the session's row, for its owner", spy.stops)
	}
}

// deadPod is a pod that ended Error 137 on sf-1, which is Ready.
func deadPod() (*fakeKube, deathInput) {
	kube := newFakeKube()
	kube.nodeStates = map[string]NodeState{"sf-1": {Name: "sf-1", BootID: "b-1", Ready: true}}
	kube.pods["ns/worker-s1"] = &fakePod{ns: "ns", name: "worker-s1", phase: "Failed", uid: deathUID, node: "sf-1",
		containers: []ContainerState{{Name: "worker", State: "terminated", Reason: "Error", ExitCode: 137}}}
	now := time.Now()
	return kube, deathInput{ns: "ns", pod: "worker-s1", boot: "b-1", started: now.Add(-10 * time.Minute), ended: now}
}

// A pod that ended 137 with no cause yet waits for a kill line up to the
// bound, asking at each poll, and is unknown after it; a kill line that
// arrives in the wait names the kill, and so does one the incident pass
// records meanwhile. The wait stops at the kernel source's first error,
// at once or partway, ends at the bound however slow a read is, and does
// not start for a stop factoryd sent or a context whose deadline leaves no
// room. A reader that does not wait still reads once. Every read names the
// pod's UID.
func TestTheKillWaitEnds(t *testing.T) {
	kube, in := deadPod()
	failed := errors.New("signoz: login refused")
	guard := []KernelKill{{Source: "guard", Node: "sf-1", PodUID: deathUID, At: in.ended.Add(-time.Second), Seq: 9}}
	after := func(n int) func(int) bool { return func(call int) bool { return call >= n } }
	for name, tc := range map[string]struct {
		kernel            *scriptedKernel
		wait              time.Duration
		ctx               func() (context.Context, context.CancelFunc)
		stopped, recorded bool
		rule              DeathRule
		calls             int // 0: three or more
		atLeast, atMost   time.Duration
	}{
		"quiet": {kernel: &scriptedKernel{}, wait: 300 * time.Millisecond, rule: RuleNone, atLeast: 300 * time.Millisecond, atMost: 2 * time.Second},
		"a line in the wait": {kernel: &scriptedKernel{kills: func(call int) []KernelKill {
			if call < 2 {
				return nil
			}
			return guard
		}}, wait: time.Hour, rule: RuleGuardKill, calls: 2, atMost: 5 * time.Second},
		"recorded meanwhile": {kernel: &scriptedKernel{}, wait: time.Hour, recorded: true, rule: RuleGuardKill, calls: 2, atMost: 5 * time.Second},
		"failing at once":    {kernel: &scriptedKernel{fail: func(int) error { return failed }}, wait: time.Hour, rule: RuleNone, calls: 1, atMost: time.Second},
		"failing partway": {kernel: &scriptedKernel{fail: func(call int) error {
			if call == 2 {
				return failed
			}
			return nil
		}, kills: func(call int) []KernelKill {
			if call < 3 {
				return nil
			}
			return guard
		}}, wait: time.Hour, rule: RuleNone, calls: 2, atMost: 5 * time.Second},
		"slow partway": {kernel: &scriptedKernel{block: after(2)}, wait: 300 * time.Millisecond, rule: RuleNone, calls: 2,
			atLeast: 300 * time.Millisecond, atMost: 800 * time.Millisecond},
		"slow at once": {kernel: &scriptedKernel{block: after(1)}, wait: 300 * time.Millisecond, rule: RuleNone, calls: 1,
			atLeast: 300 * time.Millisecond, atMost: 800 * time.Millisecond},
		"factoryd's stop": {kernel: &scriptedKernel{}, wait: time.Hour, stopped: true, rule: RuleNone, calls: 1, atMost: time.Second},
		"near the deadline": {kernel: &scriptedKernel{}, wait: time.Hour, rule: RuleNone, calls: 1, atMost: time.Second,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Second)
			}},
		"no wait": {kernel: &scriptedKernel{kills: func(int) []KernelKill { return guard }}, wait: 0, rule: RuleGuardKill, calls: 1, atMost: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			d := deathReader{kube: kube, kernel: tc.kernel, wait: tc.wait, poll: 100 * time.Millisecond}
			if tc.recorded {
				cluster := newFakeClusterStore()
				d.cluster = cluster
				tc.kernel.kills = func(call int) []KernelKill {
					if call == 1 {
						cluster.AddPlatformIncidents(context.Background(), []PlatformIncident{{Key: kmsgKey(guard[0]), At: guard[0].At, Kind: IncidentGuardKill,
							Subject: deathUID, Node: "sf-1", PodUID: deathUID}})
					}
					return nil
				}
			}
			in := in
			in.stoppedByFactory = tc.stopped
			began := time.Now()
			r := d.read(ctx, in)
			took, calls := time.Since(began), tc.kernel.called()
			if r.Rule != tc.rule || tc.calls == 0 && calls < 3 || tc.calls != 0 && calls != tc.calls || took < tc.atLeast || took > tc.atMost {
				t.Fatalf("%s after %s and %d calls; want %s in [%s, %s] after %d calls (0: three or more)", r.Rule, took, calls, tc.rule, tc.atLeast, tc.atMost, tc.calls)
			}
			if r.Rule == RuleNone && r.Class != DeathUnknown {
				t.Fatalf("class %s, want unknown", r.Class)
			}
			for _, uid := range tc.kernel.uids {
				if uid != deathUID {
					t.Fatalf("a kernel read named %q, want the pod's UID %s", uid, deathUID)
				}
			}
		})
	}
}

// A node object that is gone at the end is the platform's: the node was
// replaced or deleted under the pod.
func TestAGoneNodeIsAPlatformDeath(t *testing.T) {
	kube, in := deadPod()
	kube.pods["ns/worker-s1"].containers[0].ExitCode = 1
	delete(kube.nodeStates, "sf-1")
	if r := (deathReader{kube: kube}).read(context.Background(), in); r.Class != DeathPlatform || r.Rule != RuleNodeNotReady || r.Node != "sf-1" {
		t.Fatalf("death = %+v, want node_not_ready on sf-1", r)
	}
}

// The incident log names the pod's kills, which a kernel source without
// them does not, once each; the restarts of the pod's life, a kill of the
// service's own pod within a minute naming the cause; and the pod's events,
// each message cut.
func TestTheIncidentLogNamesKillsAndRestarts(t *testing.T) {
	ctx := context.Background()
	kube, in := deadPod()
	long := strings.Repeat("é", 150)
	kube.podEvents = map[string][]PodEvent{"ns/worker-s1": {{Reason: "Killing", Type: "Normal", Message: long, At: in.ended}}}
	kill := KernelKill{Source: "guard", Node: "sf-1", PodUID: deathUID, At: in.started.Add(5 * time.Minute), Seq: 40}
	forgejoAt, until := in.started.Add(3*time.Minute), in.started.Add(4*time.Minute)
	cluster := newFakeClusterStore()
	if _, err := cluster.AddPlatformIncidents(ctx, []PlatformIncident{
		{Key: kmsgKey(kill), At: kill.At, Kind: IncidentGuardKill, Subject: deathUID, Node: "sf-1", PodUID: deathUID},
		{Key: "kmsg/sf-2/7/x", At: forgejoAt.Add(-20 * time.Second), Kind: IncidentGuardKill, Subject: "forgejo-pod", Node: "sf-2", PodUID: "forgejo-pod"},
		{Key: "restart/forgejo-pod/forgejo/x", At: forgejoAt, Until: &until, Kind: IncidentRestart, Subject: ServiceForgejo, Node: "sf-2", Namespace: "forgejo", PodUID: "forgejo-pod"},
		{Key: "restart/litellm-pod/litellm/x", At: in.started.Add(-time.Hour), Until: &forgejoAt, Kind: IncidentRestart, Subject: ServiceLiteLLM, PodUID: "litellm-pod"},
		{Key: "restart/old/forgejo/x", At: in.started.Add(-3 * time.Hour), Until: ptr(in.started.Add(-2 * time.Hour)), Kind: IncidentRestart, Subject: ServiceForgejo, PodUID: "old"},
	}); err != nil {
		t.Fatal(err)
	}
	kernel := &scriptedKernel{kills: func(int) []KernelKill { return []KernelKill{kill} }}
	r := deathReader{kube: kube, cluster: cluster, kernel: kernel}.read(ctx, in)
	if r.Rule != RuleGuardKill {
		t.Fatalf("rule = %s, want guard_kill", r.Rule)
	}
	want := []ServiceRestart{{Service: ServiceLiteLLM, From: in.started.Add(-time.Hour), Until: forgejoAt},
		{Service: ServiceForgejo, From: forgejoAt, Until: until, Cause: "guard"}}
	if !reflect.DeepEqual(r.Restarts, want) {
		t.Fatalf("restarts = %+v, want %+v", r.Restarts, want)
	}
	if len(r.Lines) != 3 || !strings.Contains(r.Lines[2], "Forgejo restarted from") || !strings.Contains(r.Lines[2], "The node's memory guard killed it.") {
		t.Fatalf("lines = %q, want the class line and both restarts", r.Lines)
	}
	if len(r.Events) != 1 || r.Events[0].Message != textutil.Truncate(long, eventMessageB) || len(r.Events[0].Message) > eventMessageB+len("…") || !utf8.ValidString(r.Events[0].Message) {
		t.Fatalf("events = %+v, want the message cut to %d bytes on a character", r.Events, eventMessageB)
	}

	stored := deathReader{kube: kube, cluster: cluster}.read(ctx, in)
	if stored.Rule != RuleGuardKill {
		t.Fatalf("with the incident log alone: %s, want guard_kill", stored.Rule)
	}
}

// A pod gone when factoryd came back is the platform's death: its record is
// stored under the dead session's id, and the fresh session that starts over
// reads its line. A pod factoryd itself was stopping when it went is no
// platform death: its record names no cause, and no line rides on.
func TestReattachFindsThePodGone(t *testing.T) {
	for name, stopping := range map[string]bool{"gone": false, "gone in factoryd's stop": true} {
		t.Run(name, func(t *testing.T) {
			g := newDeathRig(t, "t2", nil)
			first := g.detach(t, hangLogs)
			g.kube.mu.Lock()
			delete(g.kube.pods, first.Namespace+"/"+first.Pod)
			g.kube.mu.Unlock()
			if stopping {
				if err := g.store.SetLiveSessionStopping(context.Background(), "t2", first.Owner, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			res, err := runSessionIn(t, g.r, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{pushedLine("c2") + "\n" + resultLine(true, "pushed")}, func(env *testsuite.TestActivityEnvironment) {
				env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
			})
			if err != nil || res.Outcome != OutcomePushed || res.SessionID == first.SessionID {
				t.Fatalf("result = (%+v, %v), want a fresh session's push", res, err)
			}
			rec := g.stored(t, "t2", first.SessionID)
			findings, _ := timelineStep(t, g.objects, "t2", "session_start")["findings"].([]any)
			if stopping {
				if rec.Class != DeathUnknown || len(rec.Lines) != 0 || slices.ContainsFunc(findings, func(f any) bool { return strings.HasPrefix(f.(string), "Your previous session") }) {
					t.Fatalf("stored = %+v, findings %q; want no cause and no line", rec, findings)
				}
				return
			}
			if rec.Class != DeathPlatform || rec.Rule != RuleGoneAfterRestart || rec.PodUID != deathUID || len(rec.Lines) != 1 {
				t.Fatalf("stored = %+v, want gone_after_restart of the old pod", rec)
			}
			if !slices.Contains(findings, any(rec.Lines[0])) {
				t.Fatalf("the fresh session's findings %q, want %q", findings, rec.Lines[0])
			}
		})
	}
}

// detach runs a session whose pod logs body until factoryd stops, which
// detaches from the pod, and answers the live row it left: the place of
// the pod and its owner.
func (g deathRig) detach(t *testing.T, body string) LiveSession {
	t.Helper()
	stop := make(chan struct{})
	create := g.kube.onCreatePod
	g.kube.onCreatePod = func() {
		create()
		if len(g.kube.podOrder) == 1 {
			go func() { time.Sleep(20 * time.Millisecond); close(stop) }()
		}
	}
	if _, err := runSessionIn(t, g.r, SessionInput{TaskID: g.taskID, Attempt: 1, Model: "m"}, []string{body}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := g.store.liveSessions[g.taskID]
	if first.PodUID != deathUID || first.Node != "sf-1" || first.BootID != "b-1" {
		t.Fatalf("live row = %+v, want the pod's place", first)
	}
	return first
}

// A factoryd that died inside its own stop leaves the pod deleting: the
// next one reattaches, reads the worker's stopped result, and the stop the
// row records is factoryd's own, so the deletion is no platform death.
func TestReattachToAPodFactorydWasStopping(t *testing.T) {
	g := newDeathRig(t, "t2", nil)
	g.kube.keepDeleted = true
	first := g.detach(t, modelCall+cutLogs+stoppedResultLine("stopped; no work in progress to push")+"\n")
	if err := g.store.SetLiveSessionStopping(context.Background(), "t2", first.Owner, time.Now()); err != nil {
		t.Fatal(err)
	}
	g.kube.mu.Lock()
	g.kube.pods[first.Namespace+"/"+first.Pod].deleted = true
	g.kube.mu.Unlock()
	res, err := runSessionIn(t, g.r, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if err != nil || res.SessionID != first.SessionID || res.Outcome != OutcomeNoPush || res.Death == nil || res.Death.Class != DeathUnknown {
		t.Fatalf("result = (%+v, %v), death %+v; want the reattached session's no_push with no cluster cause", res, err, res.Death)
	}
}

// A node that went NotReady during the session is the platform's, though
// it answers again at the end: the incident log holds the flip. A flip of
// another node, or one before the session, is not.
func TestANodeNotReadyDuringTheSessionIsPlatform(t *testing.T) {
	ctx := context.Background()
	kube, in := deadPod()
	kube.pods["ns/worker-s1"].containers[0].ExitCode = 1
	for name, tc := range map[string]struct {
		node string
		at   time.Time
		want DeathRule
	}{
		"its node in its life": {"sf-1", in.started.Add(5 * time.Minute), RuleNodeNotReady},
		"another node":         {"sf-2", in.started.Add(5 * time.Minute), RuleNone},
		"its node before":      {"sf-1", in.started.Add(-time.Hour), RuleNone},
	} {
		cluster := newFakeClusterStore()
		if _, err := cluster.AddPlatformIncidents(ctx, []PlatformIncident{{Key: "notready/" + tc.node, At: tc.at, Kind: IncidentNodeNotReady, Subject: tc.node, Node: tc.node}}); err != nil {
			t.Fatal(err)
		}
		if r := (deathReader{kube: kube, cluster: cluster}).read(ctx, in); r.Rule != tc.want {
			t.Errorf("%s: %s, want %s", name, r.Rule, tc.want)
		}
	}
}

// The pod's events stay on the stored record: the result the workflow
// takes, which rides the trip into the case facts, carries the class, the
// rule and the lines, and no kubelet text.
func TestTheEventsStayOnTheStoredRecord(t *testing.T) {
	const msg = "The node was low on resource: memory. Threshold quantity: 100Mi, available: 50Mi."
	var kube *fakeKube
	g := newDeathRig(t, "t5e", func(p *fakePod) {
		p.reason, p.message = "Evicted", msg
		kube.podEvents = map[string][]PodEvent{p.ns + "/" + p.name: {{Reason: "Evicted", Type: "Warning", Message: msg}}}
	})
	kube = g.kube
	res := g.run(t, modelCall)
	stored := g.stored(t, "t5e", res.SessionID)
	if len(stored.Events) != 1 || stored.Events[0].Message != msg {
		t.Fatalf("stored events = %+v, want the eviction", stored.Events)
	}
	if d := res.Death; d == nil || d.Events != nil || d.Class != DeathPlatform || d.Rule != RuleNodePressure || len(d.Lines) != 1 || !slices.Equal(d.Lines, stored.Lines) {
		t.Fatalf("the result's death = %+v, want the stored one's class, rule and lines without its events", d)
	}
}

// A death with no cluster cause is stored, and the card's timeline stays as
// it was: no cause on the end's line or its fact row.
func TestAnUnknownDeathKeepsTheTimelinePlain(t *testing.T) {
	g := newDeathRig(t, "t5u", func(p *fakePod) {
		p.containers = []ContainerState{{Name: "worker", State: "terminated", Reason: "Error", ExitCode: 1}}
	})
	res := g.run(t, modelCall)
	if res.Outcome != OutcomeNoPush || res.Death == nil || res.Death.Class != DeathUnknown {
		t.Fatalf("result = %+v, death %+v; want no_push with no cluster cause", res, res.Death)
	}
	if rec := g.stored(t, "t5u", res.SessionID); rec.Class != DeathUnknown {
		t.Fatalf("stored = %+v, want the unknown record", rec)
	}
	if end := timelineStep(t, g.objects, "t5u", "session_end"); end["cause"] != nil {
		t.Fatalf("session_end = %v, want no cause", end)
	}
	for _, e := range g.store.events {
		if e.Kind == EventSessionEnd && e.Detail["cause"] != nil {
			t.Fatalf("session_end fact = %+v, want no cause", e)
		}
	}
}

// A result recorded before the death record holds the memory kill under
// its old key: it decodes, with no death.
func TestAnOldResultWithAKillDecodes(t *testing.T) {
	var res SessionResult
	if err := json.Unmarshal([]byte(`{"Outcome":"no_push","killed":{"reason":"OOMKilled","exit_code":137}}`), &res); err != nil || res.Death != nil || res.Outcome != OutcomeNoPush {
		t.Fatalf("decoded %+v, %v; want no_push with no death", res, err)
	}
}

// The death of a session rides the stop that follows it into the trip's
// input; a run from before the change id carries none, and a later
// session with no death clears it.
func TestTripFactsCarryTheDeath(t *testing.T) {
	death := &DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, Reason: "OOMKilled", ExitCode: 137, Container: "worker", LastCommand: "go test -race ./..."}
	stopped := func() SessionResult {
		return checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: RungHandUp, Reason: "the session died twice",
			Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}})
	}
	carried := stopped()
	carried.Death = death
	for name, tc := range map[string]struct {
		pinned  bool
		stopped SessionResult
		want    *DeathRecord
	}{
		"gated":             {false, carried, death},
		"before the change": {true, carried, nil},
		"cleared":           {false, stopped(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			if tc.pinned {
				h.env.OnGetVersion("session-kill", workflow.DefaultVersion, versionSessionKill).Return(workflow.DefaultVersion)
			}
			deadSessions(h, SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped", Death: death}, tc.stopped, pushed("s3", "c3"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" || len(h.trips) == 0 {
				t.Fatalf("final = %q, trips %d", final, len(h.trips))
			}
			if got := h.trips[0].Death; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the stop's death = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A session's end with a cause shows it after the outcome's words.
func TestDigestShowsTheCause(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 5, "session_start", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1})
	g.line(t, 10, "session_end", map[string]any{"session": "s-1", "model": "factory/small", "attempt": 1, "outcome": "platform_killed",
		"cause": "the node's memory guard killed the pod", "started": rfc3339(digestAt(5))})
	g.session(t, 15, 20, 2, "no_push")
	got := g.digest(t).Render()
	if !strings.Contains(got, "→ platform killed: the node's memory guard killed the pod\n") || !strings.Contains(got, "→ no push\n") {
		t.Fatalf("Render =\n%s\nwant the cause after the platform death's words and none after the plain end", got)
	}
}

// Rung 1 reads the last session's death: a memory limit as the sandbox's
// kill with its reason, exit code and command, another cause by its phrase,
// with the command when one is known, and no cluster cause not at all.
func TestRungOneNamesTheDeath(t *testing.T) {
	r := new(Runner)
	in := ActInput{Rung: 1, Res: Resolution{Action: ActContinue, Note: "go"}}
	for name, tc := range map[string]struct {
		death           *DeathRecord
		statement, note string
	}{
		"memory": {&DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, Reason: "OOMKilled", ExitCode: 137, LastCommand: "go test -race ./..."},
			"The sandbox killed the last session for its memory (OOMKilled, exit code 137) while it ran: go test -race ./...",
			"The sandbox killed the last session for its memory while it ran: go test -race ./..."},
		"guard": {&DeathRecord{Class: DeathPlatform, Rule: RuleGuardKill, LastCommand: "make build"},
			"The last session ended: the node's memory guard killed the pod while it ran: make build",
			"The last session ended: the node's memory guard killed the pod while it ran: make build"},
		"storage, no command": {&DeathRecord{Class: DeathOwnLimit, Rule: RuleStorageLimit},
			"The last session ended: the pod passed its storage limit.", "The last session ended: the pod passed its storage limit."},
		"unknown": {&DeathRecord{Class: DeathUnknown, Rule: RuleNone, LastCommand: "make build"}, "", ""},
	} {
		f := CaseFacts{Death: tc.death}
		statement := r.rungStatement(Case{Kind: CaseSessionCap, Problem: "PROBLEM"}, f, "DIGEST", nil, 1)
		_, after := r.sessionParts(f, in)
		switch {
		case tc.statement == "" && (strings.Contains(statement, "The last session ended") || strings.Contains(statement, "The sandbox killed")):
			t.Errorf("%s: statement names the death:\n%s", name, statement)
		case tc.statement != "" && !strings.Contains(statement, "\n"+tc.statement+"\n"):
			t.Errorf("%s: statement\n%s\nwant the line %q", name, statement, tc.statement)
		case tc.note == "" && len(after) != 0, tc.note != "" && (len(after) != 1 || after[0] != tc.note):
			t.Errorf("%s: note parts %q, want %q", name, after, tc.note)
		}
	}
}
