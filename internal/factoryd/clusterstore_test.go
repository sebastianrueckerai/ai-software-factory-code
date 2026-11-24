package factoryd

import (
	"cmp"
	"reflect"
	"slices"
	"testing"
	"time"
)

func clusterStore(t *testing.T) *PGStore {
	t.Helper()
	s := chatStore(t)
	if _, err := s.db.ExecContext(t.Context(), `TRUNCATE platform_incidents, pod_deaths, live_sessions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func clk(h, m int) time.Time { return time.Date(2026, 11, 20, h, m, 0, 0, time.UTC) }

func timePtr(t time.Time) *time.Time { return &t }

func fullRecord(when time.Time) DeathRecord {
	return DeathRecord{
		Class: DeathPlatform, Rule: RuleGuardKill, At: when, Node: "n1", PodUID: "uid-1", ExitCode: 137,
		Reason: "Error", Container: "agent", Condition: "MemoryPressure", Resource: "memory", Limit: "2Gi",
		KillSource: "guard", LastCommand: "go test ./...",
		Restarts: []ServiceRestart{{Service: ServiceLiteLLM, From: when.Add(-time.Minute), Until: when, Cause: "guard"}},
		Events:   []PodEvent{{Reason: "Killing", Type: "Normal", At: when, Count: 2}},
		Lines:    []string{"a", "b"},
	}
}

func TestPlatformIncidentsAddOnce(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	in := []PlatformIncident{{Key: "k1", At: clk(10, 0), Kind: IncidentGuardKill, Subject: "pod/a"}}
	for i, want := range []int{1, 0} {
		n, err := s.AddPlatformIncidents(ctx, in)
		if err != nil || n != want {
			t.Fatalf("add %d: %d, %v; want %d", i, n, err, want)
		}
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM platform_incidents`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows %d, %v; want 1", rows, err)
	}
}

func TestPlatformIncidentsOverlapTheWindow(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	if _, err := s.AddPlatformIncidents(ctx, []PlatformIncident{
		{Key: "restart", At: clk(10, 0), Until: timePtr(clk(10, 4)), Kind: IncidentRestart, Subject: "litellm", Detail: map[string]any{"cause": "guard"}},
		{Key: "kill", At: clk(10, 6), Kind: IncidentGuardKill, Subject: "pod/a", PodUID: "uid-1"},
	}); err != nil {
		t.Fatal(err)
	}
	keys := func(from, to time.Time, limit int) []string {
		got, err := s.PlatformIncidents(ctx, from, to, limit)
		if err != nil {
			t.Fatal(err)
		}
		var k []string
		for _, p := range got {
			k = append(k, p.Key)
		}
		return k
	}
	if got := keys(clk(10, 3), clk(10, 10), 0); !reflect.DeepEqual(got, []string{"restart", "kill"}) {
		t.Fatalf("10:03-10:10: %v", got)
	}
	if got := keys(clk(10, 5), clk(10, 10), 0); !reflect.DeepEqual(got, []string{"kill"}) {
		t.Fatalf("10:05-10:10: %v", got)
	}
	if got := keys(clk(10, 3), clk(10, 10), 1); !reflect.DeepEqual(got, []string{"restart"}) {
		t.Fatalf("limit 1: %v", got)
	}
	got, err := s.PlatformIncidents(ctx, clk(10, 0), clk(10, 1), 0)
	if err != nil || len(got) != 1 || got[0].Detail["cause"] != "guard" || got[0].Until == nil || !got[0].Until.Equal(clk(10, 4)) {
		t.Fatalf("restart row %+v, %v", got, err)
	}
	pod, err := s.PodIncidents(ctx, "uid-1")
	if err != nil || len(pod) != 1 || pod[0].Key != "kill" || pod[0].Until != nil {
		t.Fatalf("pod incidents %+v, %v", pod, err)
	}
}

func TestPruneClusterFacts(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	now := time.Now().UTC()
	if _, err := s.AddPlatformIncidents(ctx, []PlatformIncident{
		{Key: "old", At: now.Add(-31 * 24 * time.Hour), Kind: IncidentNodeBoot, Subject: "n1"},
		{Key: "new", At: now.Add(-29 * 24 * time.Hour), Kind: IncidentNodeBoot, Subject: "n1"},
		{Key: "quiet-old", At: now.Add(-40 * 24 * time.Hour), Kind: IncidentNodeBoot, Subject: "n2"},
		{Key: "quiet-older", At: now.Add(-50 * 24 * time.Hour), Kind: IncidentNodeBoot, Subject: "n2"},
		{Key: "quiet-restart", At: now.Add(-40 * 24 * time.Hour), Kind: IncidentRestart, Subject: "n2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneClusterFacts(ctx, IncidentRetention); err != nil {
		t.Fatal(err)
	}
	got, err := s.PlatformIncidents(ctx, now.Add(-60*24*time.Hour), now, 0)
	var keys []string
	for _, g := range got {
		keys = append(keys, g.Key)
	}
	slices.Sort(keys)
	if want := []string{"new", "quiet-old"}; err != nil || !slices.Equal(keys, want) {
		t.Fatalf("after prune %v, %v; want %v: a node's newest boot stays, any other old row goes", keys, err, want)
	}
}

func TestPodDeathRoundTrip(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	task, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, clk(9, 0))
	if err != nil {
		t.Fatal(err)
	}
	rec := fullRecord(clk(10, 0))
	if err := s.PutPodDeath(ctx, PodDeath{Kind: "session", TaskID: task.ID, SessionID: "s1", Record: rec}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.SessionDeath(ctx, task.ID, "s1")
	if err != nil || !ok || !reflect.DeepEqual(got, rec) {
		t.Fatalf("session death %+v, %v, %v; want %+v", got, ok, err, rec)
	}
	if _, ok, err := s.SessionDeath(ctx, task.ID, "s2"); err != nil || ok {
		t.Fatalf("other session: %v, %v", ok, err)
	}
	ci := fullRecord(clk(11, 0))
	ci.Rule = RuleNodeOOM
	if err := s.PutPodDeath(ctx, PodDeath{Kind: "ci", Runner: "runner-7", Record: ci}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.CIDeath(ctx, "runner-7"); err != nil || !ok || !reflect.DeepEqual(got, ci) {
		t.Fatalf("ci death %+v, %v, %v; want %+v", got, ok, err, ci)
	}
	rec.Class, rec.Rule = DeathOwnLimit, RuleMemoryLimit
	if err := s.PutPodDeath(ctx, PodDeath{Kind: "session", TaskID: task.ID, SessionID: "s1", Record: rec}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.SessionDeath(ctx, task.ID, "s1"); got.Rule != RuleMemoryLimit || got.Class != DeathOwnLimit {
		t.Fatalf("second put left %+v; want it replaced", got)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pod_deaths WHERE kind = 'session'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("session rows %d, %v; want 1", rows, err)
	}
	if err := s.PutPodDeath(ctx, PodDeath{Kind: "gate", Record: DeathRecord{Class: DeathPlatform, At: clk(10, 0)}}); err == nil {
		t.Fatal("a gate death without a pod uid was stored")
	}
	if err := s.PutPodDeath(ctx, PodDeath{Kind: "other", Runner: "x", Record: rec}); err == nil {
		t.Fatal("an unknown kind was stored")
	}
}

func TestPlatformDeathsSince(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	task, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, clk(9, 0))
	if err != nil {
		t.Fatal(err)
	}
	rec := func(class DeathClass, when time.Time, uid string) DeathRecord {
		r := fullRecord(when)
		r.Class, r.PodUID = class, uid
		return r
	}
	for _, d := range []PodDeath{
		{Kind: "session", TaskID: task.ID, SessionID: "s1", Record: rec(DeathPlatform, clk(10, 0), "u1")},
		{Kind: "session", TaskID: task.ID, SessionID: "s2", Record: rec(DeathOwnLimit, clk(10, 30), "u2")},
		{Kind: "session", TaskID: task.ID, SessionID: "s3", Record: rec(DeathPlatform, clk(8, 0), "u3")},
		{Kind: "ci", Runner: "r1", Record: rec(DeathPlatform, clk(11, 0), "u4")},
		{Kind: "gate", Record: rec(DeathPlatform, clk(12, 0), "u5")},
	} {
		if err := s.PutPodDeath(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.PlatformDeaths(ctx, clk(9, 0))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, d := range got {
		kinds = append(kinds, d.Kind+"/"+d.Record.PodUID)
	}
	if want := []string{"gate/u5", "ci/u4", "session/u1"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("deaths %v; want %v", kinds, want)
	}
	if got[1].TaskID != "" || got[1].Runner != "r1" || got[2].TaskID != task.ID || got[2].SessionID != "s1" {
		t.Fatalf("parts lost: %+v", got)
	}
}

func TestLiveSessionKeepsItsPod(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	task, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, clk(9, 0))
	if err != nil {
		t.Fatal(err)
	}
	l := LiveSession{TaskID: task.ID, SessionID: "s1", Namespace: "ns", Pod: "p", MessageToken: "tok", StartedAt: clk(10, 0), Deadline: clk(11, 0), Owner: "a", PodUID: "uid-1", Node: "n1", BootID: "boot-1"}
	if err := s.PutLiveSession(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LiveSession(ctx, task.ID)
	if err != nil || !ok || got.PodUID != "uid-1" || got.Node != "n1" || got.BootID != "boot-1" {
		t.Fatalf("live session %+v, %v, %v", got, ok, err)
	}
}

// The pod's place is written for the owner holding the row alone, and
// leaves the rest of the row, a raised deadline among it, as it was.
func TestLiveSessionPodForItsOwner(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	task, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, clk(9, 0))
	if err != nil {
		t.Fatal(err)
	}
	l := LiveSession{TaskID: task.ID, SessionID: "s1", Namespace: "ns", Pod: "p", MessageToken: "tok", StartedAt: clk(10, 0), Deadline: clk(11, 0), Owner: "a"}
	if err := s.PutLiveSession(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLiveSessionDeadline(ctx, task.ID, "a", clk(11, 30), 30*time.Minute, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLiveSessionPod(ctx, task.ID, "b", "uid-b", "n-b", "boot-b"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LiveSession(ctx, task.ID); got.PodUID != "" || got.Node != "" || got.BootID != "" {
		t.Fatalf("after another owner's write: %+v, want no place", got)
	}
	if err := s.SetLiveSessionPod(ctx, task.ID, "a", "uid-1", "n1", "boot-1"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LiveSession(ctx, task.ID)
	if err != nil || !ok || got.PodUID != "uid-1" || got.Node != "n1" || got.BootID != "boot-1" || !got.Deadline.Equal(clk(11, 30)) || got.Owner != "a" {
		t.Fatalf("live session %+v, %v, %v; want the place beside the raised deadline", got, ok, err)
	}
}

// The stop factoryd sent is written for the owner holding the row alone,
// reads back, and a new session's row holds none.
func TestLiveSessionStoppingForItsOwner(t *testing.T) {
	s, ctx := clusterStore(t), t.Context()
	task, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, clk(9, 0))
	if err != nil {
		t.Fatal(err)
	}
	l := LiveSession{TaskID: task.ID, SessionID: "s1", Namespace: "ns", Pod: "p", MessageToken: "tok", StartedAt: clk(10, 0), Deadline: clk(11, 0), Owner: "a"}
	if err := s.PutLiveSession(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLiveSessionStopping(ctx, task.ID, "b", clk(10, 30)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LiveSession(ctx, task.ID); got.StoppingAt != nil {
		t.Fatalf("after another owner's stop: %v, want none", got.StoppingAt)
	}
	if err := s.SetLiveSessionStopping(ctx, task.ID, "a", clk(10, 30)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.LiveSession(ctx, task.ID); err != nil || !ok || got.StoppingAt == nil || !got.StoppingAt.Equal(clk(10, 30)) {
		t.Fatalf("live session %+v, %v, %v; want the stop at 10:30", got, ok, err)
	}
	l.SessionID = "s2"
	if err := s.PutLiveSession(ctx, l); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LiveSession(ctx, task.ID); got.StoppingAt != nil {
		t.Fatalf("the next session's row stops at %v, want none", got.StoppingAt)
	}
}

// The fake and Postgres agree: equal times come back in key order, and a
// nil Detail reads back as an empty map.
func TestClusterStoreFakeMatchesPostgres(t *testing.T) {
	run := func(t *testing.T, cs ClusterStore) {
		ctx := t.Context()
		when := clk(10, 0)
		for _, d := range []PodDeath{
			{Kind: "gate", Record: DeathRecord{Class: DeathPlatform, At: when, PodUID: "x"}},
			{Kind: "ci", Runner: "b", Record: DeathRecord{Class: DeathPlatform, At: when}},
			{Kind: "ci", Runner: "a", Record: DeathRecord{Class: DeathPlatform, At: when}},
			{Kind: "ci", Runner: "ab", Record: DeathRecord{Class: DeathPlatform, At: when}},
			{Kind: "ci", Runner: "a-c", Record: DeathRecord{Class: DeathPlatform, At: when}},
		} {
			if err := cs.PutPodDeath(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		got, err := cs.PlatformDeaths(ctx, clk(9, 0))
		var order []string
		for _, d := range got {
			order = append(order, cmp.Or(d.Runner, d.Kind))
		}
		if want := []string{"a", "a-c", "ab", "b", "gate"}; err != nil || !slices.Equal(order, want) {
			t.Fatalf("deaths %v, %v; want byte order of the keys %v", order, err, want)
		}
		bootAt := time.Now().UTC().Truncate(time.Second)
		if _, err := cs.AddPlatformIncidents(ctx, []PlatformIncident{{Key: "k", At: bootAt, Kind: IncidentNodeBoot, Subject: "n1"}}); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-31 * 24 * time.Hour)
		if _, err := cs.AddPlatformIncidents(ctx, []PlatformIncident{
			{Key: "b1", At: old, Kind: IncidentNodeBoot, Subject: "n9"},
			{Key: "b2", At: old.Add(time.Hour), Kind: IncidentNodeBoot, Subject: "n9"},
			{Key: "r", At: old, Kind: IncidentRestart, Subject: "n9"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := cs.PruneClusterFacts(ctx, IncidentRetention); err != nil {
			t.Fatal(err)
		}
		left, err := cs.PlatformIncidents(ctx, old.Add(-time.Hour), old.Add(2*time.Hour), 0)
		if err != nil || len(left) != 1 || left[0].Key != "b2" {
			t.Fatalf("after prune %+v, %v; want only the newest boot b2", left, err)
		}
		inc, err := cs.PlatformIncidents(ctx, bootAt, bootAt, 0)
		if err != nil || len(inc) != 1 || inc[0].Detail == nil || len(inc[0].Detail) != 0 {
			t.Fatalf("incidents %+v, %v; want an empty, non-nil Detail", inc, err)
		}
	}
	t.Run("fake", func(t *testing.T) { run(t, newFakeClusterStore()) })
	t.Run("postgres", func(t *testing.T) { run(t, clusterStore(t)) })
}

// The prune drops the CI and gate deaths past the retention, which only the
// checks gate and the human's card read, and keeps every session's death,
// which cluster_facts answers, and the newer ones.
func TestPruneKeepsTheSessionsDeaths(t *testing.T) {
	run := func(t *testing.T, cs ClusterStore, task string) {
		ctx := t.Context()
		old, recent := time.Now().Add(-31*24*time.Hour), time.Now().Add(-time.Hour)
		rec := func(at time.Time, uid string) DeathRecord {
			return DeathRecord{Class: DeathPlatform, At: at, PodUID: uid}
		}
		for _, d := range []PodDeath{
			{Kind: "session", TaskID: task, SessionID: "s-old", Record: rec(old, "")},
			{Kind: "session", TaskID: task, SessionID: "s-new", Record: rec(recent, "")},
			{Kind: "ci", Runner: "r-old", Record: rec(old, "")},
			{Kind: "ci", Runner: "r-new", Record: rec(recent, "")},
			{Kind: "gate", Record: rec(old, "g-old")},
			{Kind: "gate", Record: rec(recent, "g-new")},
		} {
			if err := cs.PutPodDeath(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		if err := cs.PruneClusterFacts(ctx, IncidentRetention); err != nil {
			t.Fatal(err)
		}
		got, err := cs.PlatformDeaths(ctx, old.Add(-time.Hour))
		var left []string
		for _, d := range got {
			left = append(left, d.Kind+"/"+d.SessionID+d.Runner+d.Record.PodUID)
		}
		slices.Sort(left)
		if want := []string{"ci/r-new", "gate/g-new", "session/s-new", "session/s-old"}; err != nil || !slices.Equal(left, want) {
			t.Fatalf("deaths after the prune %v, %v; want %v", left, err, want)
		}
	}
	t.Run("fake", func(t *testing.T) { run(t, newFakeClusterStore(), "t1") })
	t.Run("postgres", func(t *testing.T) {
		s := clusterStore(t)
		task, err := s.EnsureChatTask(t.Context(), "linkshort", "linkshort/linkshort", 5, clk(9, 0))
		if err != nil {
			t.Fatal(err)
		}
		run(t, s, task.ID)
	})
}

// A restart first seen while its container waits has no end: a later pass
// that sees it running writes its end, and an end once written stays.
func TestARestartGetsItsEndOnALaterPass(t *testing.T) {
	run := func(t *testing.T, cs ClusterStore) {
		ctx := t.Context()
		restart := PlatformIncident{Key: "restart/p/forgejo/x", At: clk(14, 2), Kind: IncidentRestart, Subject: ServiceForgejo, PodUID: "p"}
		ended, later := restart, restart
		ended.Until, later.Until = timePtr(clk(14, 4)), timePtr(clk(14, 9))
		for _, p := range []PlatformIncident{restart, ended, later, restart} {
			if _, err := cs.AddPlatformIncidents(ctx, []PlatformIncident{p}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := cs.PlatformIncidents(ctx, clk(14, 3), clk(14, 3), 0)
		if err != nil || len(got) != 1 || got[0].Until == nil || !got[0].Until.Equal(clk(14, 4)) {
			t.Fatalf("incidents at 14:03 %+v, %v; want the restart to 14:04", got, err)
		}
	}
	t.Run("fake", func(t *testing.T) { run(t, newFakeClusterStore()) })
	t.Run("postgres", func(t *testing.T) { run(t, clusterStore(t)) })
}
