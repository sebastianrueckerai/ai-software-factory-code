package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

// scriptedKernel answers the kernel lines of each call, counted from 1.
type scriptedKernel struct {
	mu    sync.Mutex
	calls int
	kills func(call int) []KernelKill
	err   error
}

func (k *scriptedKernel) KernelLines(context.Context, time.Time, time.Time) ([]KernelKill, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	if k.err != nil || k.kills == nil {
		return nil, k.err
	}
	return k.kills(k.calls), nil
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

// placeSpy keeps the live row as each write of the pod's place left it.
type placeSpy struct {
	Store
	mu   sync.Mutex
	puts []LiveSession
	rows []LiveSession
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
	setDetectors(g.r, map[string]int{policy.DetectorNoProgress: 3})
	setLimits(g.r, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	res := g.run(t, modelCall+sigtermLogs)
	if res.Outcome != OutcomeWallClock || res.Death == nil || res.Death.Class == DeathPlatform {
		t.Fatalf("result = %+v, death %+v; want wall_clock and no platform death", res, res.Death)
	}
	if rec := g.stored(t, "t5s", res.SessionID); rec.Class == DeathPlatform {
		t.Fatalf("stored = %+v, want no platform death", rec)
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
// bound and is unknown after it; a kill line that arrives in the wait names
// the kill. A kernel source that fails ends the wait at once, and so does a
// context whose deadline leaves no room for it.
func TestTheKillWaitEnds(t *testing.T) {
	ctx := context.Background()
	kube, in := deadPod()
	quiet := &scriptedKernel{}
	began := time.Now()
	r := deathReader{kube: kube, kernel: quiet, wait: 300 * time.Millisecond}.read(ctx, in)
	if took := time.Since(began); r.Class != DeathUnknown || r.Rule != RuleNone || took < 300*time.Millisecond || took > 10*time.Second || quiet.called() != 2 {
		t.Fatalf("quiet kernel: %s %s after %s and %d calls; want unknown at the 300ms bound, asked twice", r.Class, r.Rule, took, quiet.called())
	}

	late := &scriptedKernel{kills: func(call int) []KernelKill {
		if call < 2 {
			return nil
		}
		return []KernelKill{{Source: "guard", Node: "sf-1", PodUID: deathUID, At: in.ended.Add(-time.Second), Seq: 9}}
	}}
	if r := (deathReader{kube: kube, kernel: late, wait: 300 * time.Millisecond}).read(ctx, in); r.Rule != RuleGuardKill {
		t.Fatalf("a kill line in the wait: %s, want guard_kill", r.Rule)
	}

	failing := &scriptedKernel{err: errors.New("signoz: login refused")}
	began = time.Now()
	r = deathReader{kube: kube, kernel: failing, wait: time.Hour}.read(ctx, in)
	if took := time.Since(began); r.Class != DeathUnknown || took > 10*time.Second || failing.called() != 1 {
		t.Fatalf("failing kernel: %s after %s and %d calls; want unknown at once", r.Class, took, failing.called())
	}

	near, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	quiet = &scriptedKernel{}
	began = time.Now()
	r = deathReader{kube: kube, kernel: quiet, wait: time.Hour}.read(near, in)
	if took := time.Since(began); r.Class != DeathUnknown || took > 10*time.Second || quiet.called() != 1 {
		t.Fatalf("near deadline: %s after %s and %d calls; want unknown at once", r.Class, took, quiet.called())
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
// reads its line.
func TestReattachFindsThePodGone(t *testing.T) {
	g := newDeathRig(t, "t2", nil)
	stop := make(chan struct{})
	create := g.kube.onCreatePod
	g.kube.onCreatePod = func() {
		create()
		if len(g.kube.podOrder) == 1 {
			go func() { time.Sleep(20 * time.Millisecond); close(stop) }()
		}
	}
	if _, err := runSessionIn(t, g.r, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{hangLogs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := g.store.liveSessions["t2"]
	if first.PodUID != deathUID || first.Node != "sf-1" || first.BootID != "b-1" {
		t.Fatalf("live row = %+v, want the pod's place", first)
	}
	g.kube.mu.Lock()
	delete(g.kube.pods, first.Namespace+"/"+first.Pod)
	g.kube.mu.Unlock()

	res, err := runSessionIn(t, g.r, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{pushedLine("c2") + "\n" + resultLine(true, "pushed")}, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if err != nil || res.Outcome != OutcomePushed || res.SessionID == first.SessionID {
		t.Fatalf("result = (%+v, %v), want a fresh session's push", res, err)
	}
	rec := g.stored(t, "t2", first.SessionID)
	if rec.Class != DeathPlatform || rec.Rule != RuleGoneAfterRestart || rec.PodUID != deathUID || len(rec.Lines) != 1 {
		t.Fatalf("stored = %+v, want gone_after_restart of the old pod", rec)
	}
	findings, _ := timelineStep(t, g.objects, "t2", "session_start")["findings"].([]any)
	found := false
	for _, f := range findings {
		found = found || f == rec.Lines[0]
	}
	if !found {
		t.Fatalf("the fresh session's findings %q, want %q", findings, rec.Lines[0])
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
