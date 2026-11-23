package factoryd

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func putSessionDeath(t *testing.T, s ClusterStore, task, session string, d DeathRecord) {
	t.Helper()
	if err := s.PutPodDeath(context.Background(), PodDeath{Kind: "session", TaskID: task, SessionID: session, Record: d}); err != nil {
		t.Fatal(err)
	}
}

type factsLive map[string]LiveSession

func (f factsLive) LiveSession(_ context.Context, taskID string) (LiveSession, bool, error) {
	l, ok := f[taskID]
	return l, ok, nil
}

type factsKube struct {
	pod     PodDiagnosis
	podErr  error
	node    NodeState
	nodeErr error
}

func (k factsKube) PodDiagnosis(context.Context, string, string) (PodDiagnosis, error) {
	return k.pod, k.podErr
}

func (k factsKube) NodeState(context.Context, string) (NodeState, error) { return k.node, k.nodeErr }

var cfNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func TestSessionFactsTextHoldsTheFields(t *testing.T) {
	s := newFakeClusterStore()
	at := time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)
	started := at.Add(-30 * time.Minute)
	until := at
	s.AddPlatformIncidents(t.Context(), []PlatformIncident{
		{Key: "k1", At: at.Add(-5 * time.Minute), Kind: IncidentGuardKill, Subject: "pod/a", Node: "n1", Detail: map[string]any{"constraint": "CONSTRAINT_NONE"}},
		{Key: "k2", At: at.Add(-time.Minute), Until: &until, Kind: IncidentRestart, Subject: "litellm", Node: "n1"},
		{Key: "k3", At: at.Add(-time.Minute), Kind: IncidentGuardKill, Subject: "pod/b", Node: "n2"},
		{Key: "k4", At: started.Add(-time.Hour), Kind: IncidentNodeBoot, Subject: "n1", Node: "n1"},
	})
	putSessionDeath(t, s, "t1", "s1", DeathRecord{Class: DeathPlatform, Rule: RuleGuardKill, At: at, Started: started, Node: "n1", PodUID: "uid-1",
		ExitCode: 137, Reason: "Error", Container: "agent", Condition: "DisruptionTarget", Resource: "memory", Limit: "2Gi",
		KillSource: "guard", LastCommand: "go test ./...",
		Restarts: []ServiceRestart{{Service: ServiceLiteLLM, From: at.Add(-time.Minute), Until: at, Cause: "guard"}},
		Events: []PodEvent{{Reason: "Killing", Type: "Normal", Message: "secret kubelet text", At: at, Count: 2},
			{Reason: "Evicted", Type: "Warning", Message: "low memory", At: at.Add(time.Second)}},
		Lines: []string{"the platform ended this session", "no fault of yours"}})
	r := ClusterFactsReader{Store: s, Kube: factsKube{node: NodeState{Name: "n1", BootID: "boot-9", Ready: true, Since: at.Add(-48 * time.Hour)}}}
	got, err := r.SessionText(t.Context(), "t1", "s1")
	want := "class: platform\n" +
		"rule: guard_kill\n" +
		"at: 2026-11-20T10:05:00Z\n" +
		"node: n1\n" +
		"pod: uid-1\n" +
		"exit code: 137 Error\n" +
		"container: agent\n" +
		"condition: DisruptionTarget\n" +
		"resource: memory\n" +
		"limit: 2Gi\n" +
		"kill: memory guard\n" +
		"last command: go test ./...\n" +
		"node ready: true\n" +
		"node ready since: 2026-11-18T10:05:00Z\n" +
		"node boot id: boot-9\n" +
		"service restarts in the session's life:\n" +
		"- litellm 2026-11-20T10:04:00Z to 2026-11-20T10:05:00Z UTC, guard\n" +
		"events of the pod:\n" +
		"- 2026-11-20T10:05:00Z Normal Killing (x2)\n" +
		"- 2026-11-20T10:05:01Z Warning Evicted (x1)\n" +
		"incidents on its node:\n" +
		"- 2026-11-20T10:00:00Z guard_kill pod/a on n1 constraint=CONSTRAINT_NONE\n" +
		"- 2026-11-20T10:04:00Z restart litellm on n1 to 2026-11-20T10:05:00Z\n" +
		"lines given to the next session:\n" +
		"the platform ended this session\n" +
		"no fault of yours"
	if err != nil || got != want {
		t.Fatalf("got (%v)\n%s\nwant\n%s", err, got, want)
	}
}

func TestSessionFactsTextOfAnOldRecordStartsTwoHoursBack(t *testing.T) {
	s := newFakeClusterStore()
	at := time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)
	s.AddPlatformIncidents(t.Context(), []PlatformIncident{
		{Key: "in", At: at.Add(-119 * time.Minute), Kind: IncidentNodeBoot, Subject: "n1", Node: "n1"},
		{Key: "out", At: at.Add(-121 * time.Minute), Kind: IncidentNodeBoot, Subject: "n1b", Node: "n1"},
	})
	putSessionDeath(t, s, "t1", "s1", DeathRecord{Class: DeathUnknown, Rule: RuleNone, At: at, Node: "n1"})
	got, err := ClusterFactsReader{Store: s}.SessionText(t.Context(), "t1", "s1")
	want := "class: unknown\nrule: none\nat: 2026-11-20T10:05:00Z\nnode: n1\nincidents on its node:\n- 2026-11-20T08:06:00Z node_boot n1 on n1"
	if err != nil || got != want {
		t.Fatalf("got (%v)\n%s\nwant\n%s", err, got, want)
	}
}

func TestSessionFactsTextNamesAKernelKill(t *testing.T) {
	s := newFakeClusterStore()
	putSessionDeath(t, s, "t1", "s1", DeathRecord{Class: DeathPlatform, Rule: RuleNodeOOM, At: cfNow, KillSource: "kernel"})
	got, err := ClusterFactsReader{Store: s}.SessionText(t.Context(), "t1", "s1")
	if err != nil || !strings.Contains(got, "\nkill: kernel") {
		t.Fatalf("got (%v)\n%s", err, got)
	}
}

func TestSessionFactsTextCutsAtTwelveKiB(t *testing.T) {
	s := newFakeClusterStore()
	line := strings.Repeat("x", 99)
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = line
	}
	putSessionDeath(t, s, "t1", "s1", DeathRecord{Class: DeathUnknown, Rule: RuleNone, At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), Lines: lines})
	got, err := ClusterFactsReader{Store: s}.SessionText(t.Context(), "t1", "s1")
	head := "class: unknown\nrule: none\nat: 2026-11-20T10:00:00Z\nlines given to the next session:\n"
	total := len(head) + 200*100 - 1
	fit := (clusterFactsMax - len(head)) / 100
	kept := len(head) + fit*100 - 1
	want := head + strings.TrimSuffix(strings.Repeat(line+"\n", fit), "\n") + "\n[" + strconv.Itoa(total-kept) + " bytes cut]"
	if err != nil || got != want {
		t.Fatalf("got (%v) %d bytes, tail %q; want %d bytes", err, len(got), got[max(0, len(got)-40):], len(want))
	}
}

func TestCutFactsWithoutANewlineCutsOnARuneStart(t *testing.T) {
	in := strings.Repeat("a", clusterFactsMax-1) + "\u00e4" + "tail"
	got := cutFacts(in)
	want := strings.Repeat("a", clusterFactsMax-1) + "\n[" + strconv.Itoa(len(in)-(clusterFactsMax-1)) + " bytes cut]"
	if got != want {
		t.Fatalf("got %d bytes, tail %q; want %d bytes", len(got), got[max(0, len(got)-30):], len(want))
	}
}

func TestIncidentsTextRefusals(t *testing.T) {
	r := ClusterFactsReader{Store: newFakeClusterStore()}
	now := cfNow
	h := func(d time.Duration) time.Time { return now.Add(d) }
	for _, c := range []struct {
		name         string
		since, until time.Time
		want         string
		status       int
	}{
		{"until before since", h(-time.Hour), h(-2 * time.Hour), "until comes before since: ask with since before until.", 400},
		{"too wide", h(-26 * time.Hour), h(-time.Hour), "a window holds at most 24 hours: ask with since and until at most 24 hours apart.", 400},
		{"just over 24 hours", h(-24*time.Hour - time.Second), now, "a window holds at most 24 hours: ask with since and until at most 24 hours apart.", 400},
		{"too old", h(-31 * 24 * time.Hour), h(-31*24*time.Hour + time.Hour), "the incident log keeps 30 days: ask for a window after 2026-10-24.", 400},
		{"just too old", h(-IncidentRetention - time.Second), h(-IncidentRetention + time.Hour), "the incident log keeps 30 days: ask for a window after 2026-10-24.", 400},
		{"one missing", h(-time.Hour), time.Time{}, "ask with both since and until, or with a session.", 400},
	} {
		_, err := r.IncidentsText(t.Context(), c.since, c.until, now)
		var no *FactsRefusal
		if !errors.As(err, &no) || no.Msg != c.want || no.Status != c.status {
			t.Errorf("%s: err = %v, want %d %q", c.name, err, c.status, c.want)
		}
	}
	for name, w := range map[string][2]time.Time{
		"exactly 24 hours":   {h(-24 * time.Hour), now},
		"exactly 30 days":    {h(-IncidentRetention), h(-IncidentRetention + time.Hour)},
		"until equals since": {h(-time.Hour), h(-time.Hour)},
	} {
		if _, err := r.IncidentsText(t.Context(), w[0], w[1], now); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	_, err := r.SessionText(t.Context(), "t1", "s9")
	var no *FactsRefusal
	if !errors.As(err, &no) || no.Status != 404 || no.Msg != "session s9 has no death record: it ended with a push or before the factory recorded deaths. Read card_step for its end." {
		t.Errorf("no record: %v", err)
	}
	_, err = r.Text(t.Context(), "t1", "s1", h(-time.Hour), h(0), now)
	if !errors.As(err, &no) || no.Status != 400 || no.Msg != "ask for a session or for a window, not both." {
		t.Errorf("both: %v", err)
	}
}

func TestClusterFactsWithoutAStoreRefuses(t *testing.T) {
	for name, call := range map[string]func(ClusterFactsReader) error{
		"session": func(r ClusterFactsReader) error { _, err := r.SessionText(t.Context(), "t1", "s1"); return err },
		"window": func(r ClusterFactsReader) error {
			_, err := r.IncidentsText(t.Context(), cfNow.Add(-time.Hour), cfNow, cfNow)
			return err
		},
	} {
		var no *FactsRefusal
		if err := call(ClusterFactsReader{}); !errors.As(err, &no) || no.Msg != "the factory keeps no cluster facts here" || no.Status != 409 {
			t.Errorf("%s: err = %v, want the 409 refusal", name, err)
		}
	}
}

func TestIncidentsTextCutsAValueWithoutANewline(t *testing.T) {
	s := newFakeClusterStore()
	s.AddPlatformIncidents(t.Context(), []PlatformIncident{{Key: "a", At: cfNow.Add(-time.Hour), Kind: IncidentRestart, Subject: "x",
		Detail: map[string]any{"v": strings.Repeat("\u00e4", 7000)}}})
	got, err := ClusterFactsReader{Store: s}.IncidentsText(t.Context(), cfNow.Add(-2*time.Hour), cfNow, cfNow)
	if err != nil || len(got) > clusterFactsMax+40 || !strings.HasSuffix(got, " bytes cut]") {
		t.Fatalf("got (%v) %d bytes, tail %q", err, len(got), got[max(0, len(got)-30):])
	}
}

func TestIncidentsTextListsTheWindowOldestFirst(t *testing.T) {
	s := newFakeClusterStore()
	at := cfNow.Add(-2 * time.Hour)
	until := at.Add(3 * time.Minute)
	s.AddPlatformIncidents(t.Context(), []PlatformIncident{
		{Key: "b", At: at.Add(time.Minute), Until: &until, Kind: IncidentRestart, Subject: "litellm", Detail: map[string]any{"cause": "guard", "after": "1m"}},
		{Key: "a", At: at, Kind: IncidentNodeBoot, Subject: "n1", Node: "n1"},
	})
	got, err := ClusterFactsReader{Store: s}.IncidentsText(t.Context(), cfNow.Add(-3*time.Hour), cfNow, cfNow)
	want := "2026-11-23T10:00:00Z node_boot n1 on n1\n2026-11-23T10:01:00Z restart litellm to 2026-11-23T10:03:00Z after=1m cause=guard"
	if err != nil || got != want {
		t.Fatalf("got (%v)\n%s\nwant\n%s", err, got, want)
	}
}

func TestSessionFactsTextOfARunningSessionReadsThePod(t *testing.T) {
	s := newFakeClusterStore()
	started := cfNow.Add(-20 * time.Minute)
	s.AddPlatformIncidents(t.Context(), []PlatformIncident{
		{Key: "on", At: started.Add(5 * time.Minute), Kind: IncidentGuardKill, Subject: "pod/x", Node: "n1", Detail: map[string]any{"source": "guard"}},
		{Key: "other", At: started.Add(5 * time.Minute), Kind: IncidentGuardKill, Subject: "pod/y", Node: "n2"},
		{Key: "before", At: started.Add(-time.Minute), Kind: IncidentGuardKill, Subject: "pod/z", Node: "n1"},
	})
	r := ClusterFactsReader{Store: s,
		Live: factsLive{"t1": {TaskID: "t1", SessionID: "s1", Namespace: "ns", Pod: "p", StartedAt: started}},
		Kube: factsKube{pod: PodDiagnosis{Phase: "Running", Node: "n1", Containers: []ContainerState{
			{Name: "agent", State: "running", Restarts: 0}, {Name: "sidecar", State: "waiting", Reason: "CrashLoopBackOff", Restarts: 3}}},
			node: NodeState{Name: "n1", BootID: "b1", Ready: true, Since: started.Add(-time.Hour)}}}
	got, err := r.SessionText(t.Context(), "t1", "s1")
	want := "session s1 is running\n" +
		"pod phase: Running\n" +
		"node: n1\n" +
		"container agent: running, restarts 0\n" +
		"container sidecar: waiting CrashLoopBackOff, restarts 3\n" +
		"node ready: true\n" +
		"node ready since: 2026-11-23T10:40:00Z\n" +
		"node boot id: b1\n" +
		"incidents on its node since 2026-11-23T11:40:00Z:\n" +
		"- 2026-11-23T11:45:00Z guard_kill pod/x on n1 source=guard"
	if err != nil || got != want {
		t.Fatalf("got (%v)\n%s\nwant\n%s", err, got, want)
	}
	if _, err := r.SessionText(t.Context(), "t1", "s2"); err == nil || !strings.HasPrefix(err.Error(), "session s2 has no death record") {
		t.Fatalf("a session neither recorded nor live: %v", err)
	}
}

func TestToolLinesNameEveryTool(t *testing.T) {
	for _, tool := range checkinTools {
		if !strings.Contains(checkinToolsLine, tool.Name+" (") {
			t.Errorf("checkinToolsLine lacks %s", tool.Name)
		}
	}
	for _, tool := range CaseEvidence() {
		if !strings.Contains(rungToolsLine, tool.Name+" (") {
			t.Errorf("rungToolsLine lacks %s", tool.Name)
		}
	}
}

func TestCheckinOffersClusterFacts(t *testing.T) {
	g := skeletonRig(t)
	cs := newFakeClusterStore()
	g.r.cfg.Cluster = cs
	putSessionDeath(t, cs, "t1410", "s-1-41", DeathRecord{Class: DeathPlatform, Rule: RuleNodeRebooted, At: digestAt(55), Node: "n2"})
	putSessionDeath(t, cs, "t1999", "s-other", DeathRecord{Class: DeathPlatform, Rule: RuleNodeRebooted, At: digestAt(55)})
	tools := &runnerTools{r: g.r, taskID: "t1410", ask: &CheckInAsk{ParentSession: "s-1-41"}}
	out, cut, err := tools.Tool(t.Context(), "cluster_facts", nil)
	if want := "class: platform\nrule: node_rebooted\nat: 2026-11-01T00:55:00Z\nnode: n2"; out != want || cut || err != nil {
		t.Fatalf("cluster_facts = (%q, %v, %v), want %q", out, cut, err, want)
	}
	_, _, err = tools.Tool(t.Context(), "cluster_facts", map[string]any{"session": "s-other"})
	if err == nil || !strings.HasPrefix(err.Error(), "session s-other has no death record") {
		t.Fatalf("another card's session = %v, want the no-record refusal", err)
	}
}

func TestCheckinReadsItsRunningParentLive(t *testing.T) {
	g := skeletonRig(t)
	g.r.cfg.Cluster = newFakeClusterStore()
	g.store.liveSessions = map[string]LiveSession{"t1410": {TaskID: "t1410", SessionID: "s-live", Namespace: "ns", Pod: "p", StartedAt: digestAt(10)}}
	g.r.kube = &fakeKube{}
	tools := &runnerTools{r: g.r, taskID: "t1410", ask: &CheckInAsk{ParentSession: "s-live"}}
	out, _, err := tools.Tool(t.Context(), "cluster_facts", nil)
	if err != nil || !strings.HasPrefix(out, "session s-live is running\n") {
		t.Fatalf("cluster_facts = (%q, %v), want the live read", out, err)
	}
}

func TestRungOneReadsClusterFacts(t *testing.T) {
	g := skeletonRig(t)
	cs := newFakeClusterStore()
	g.r.cfg.Cluster = cs
	g.at(700)
	for id, at := range map[string]int{"s-1-41": 41, "s-5-600": 600} {
		g.store.sessions[id] = Session{ID: id, TaskID: "t1410", CreatedAt: digestAt(at)}
	}
	g.r.objects.Put(t.Context(), "sessions/t1410/s-5-600.jsonl", []byte("{}\n"))
	putSessionDeath(t, cs, "t1410", "s-5-600", DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, At: digestAt(610)})
	putSessionDeath(t, cs, "t1999", "s-other", DeathRecord{Class: DeathPlatform, Rule: RuleGuardKill, At: digestAt(610)})
	tools := &CaseTools{r: g.r, t: Task{ID: "t1410"}}
	out, _, err := tools.Tool(t.Context(), "cluster_facts", nil)
	if want := "class: own_limit\nrule: memory_limit\nat: 2026-11-01T10:10:00Z"; out != want || err != nil {
		t.Fatalf("newest session = (%q, %v), want %q", out, err, want)
	}
	if _, _, err := tools.Tool(t.Context(), "cluster_facts", map[string]any{"session": "s-other"}); err == nil || !strings.HasPrefix(err.Error(), "session s-other has no death record") {
		t.Fatalf("another card's session = %v, want the no-record refusal", err)
	}
}

type failingGet struct{ ObjectStore }

func (failingGet) Get(context.Context, string) ([]byte, error) { return nil, errors.New("s3 down") }

func TestRungOneKeepsTheNewestSessionWhenItsRecordFailsToRead(t *testing.T) {
	g := skeletonRig(t)
	cs := newFakeClusterStore()
	g.r.cfg.Cluster = cs
	g.at(700)
	g.store.sessions["s-5-600"] = Session{ID: "s-5-600", TaskID: "t1410", CreatedAt: digestAt(600)}
	g.r.objects = failingGet{g.r.objects}
	putSessionDeath(t, cs, "t1410", "s-5-600", DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, At: digestAt(610)})
	out, _, err := (&CaseTools{r: g.r, t: Task{ID: "t1410"}}).Tool(t.Context(), "cluster_facts", nil)
	if !strings.HasPrefix(out, "class: own_limit") || err != nil {
		t.Fatalf("got (%q, %v), want the newest session's facts", out, err)
	}
}
