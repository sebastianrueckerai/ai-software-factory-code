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

func TestSessionFactsTextHoldsTheFields(t *testing.T) {
	s := newFakeClusterStore()
	at := time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)
	putSessionDeath(t, s, "t1", "s1", DeathRecord{Class: DeathPlatform, Rule: RuleGuardKill, At: at, Node: "n1", PodUID: "uid-1",
		ExitCode: 137, Reason: "Error", KillSource: "guard", LastCommand: "go test ./...",
		Restarts: []ServiceRestart{{Service: ServiceLiteLLM, From: at.Add(-time.Minute), Until: at, Cause: "guard"}},
		Events:   []PodEvent{{Reason: "Killing", Type: "Normal", Message: "stopped", At: at}, {Reason: "Evicted", Type: "Warning", Message: "low memory", At: at.Add(time.Second)}},
		Lines:    []string{"the platform ended this session", "no fault of yours"}})
	got, err := SessionFactsText(t.Context(), s, "t1", "s1")
	want := "class: platform\n" +
		"rule: guard_kill\n" +
		"at: 2026-11-20T10:05:00Z\n" +
		"node: n1\n" +
		"pod: uid-1\n" +
		"exit code: 137 Error\n" +
		"kill: memory guard\n" +
		"last command: go test ./...\n" +
		"service restarts in the session's life:\n" +
		"- litellm 2026-11-20T10:04:00Z to 2026-11-20T10:05:00Z UTC, guard\n" +
		"events of the pod:\n" +
		"- 2026-11-20T10:05:00Z Normal Killing: stopped\n" +
		"- 2026-11-20T10:05:01Z Warning Evicted: low memory\n" +
		"lines given to the next session:\n" +
		"the platform ended this session\n" +
		"no fault of yours"
	if err != nil || got != want {
		t.Fatalf("got (%v)\n%s\nwant\n%s", err, got, want)
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
	got, err := SessionFactsText(t.Context(), s, "t1", "s1")
	head := "class: unknown\nrule: none\nat: 2026-11-20T10:00:00Z\nlines given to the next session:\n"
	total := len(head) + 200*100 - 1
	fit := (clusterFactsMax - len(head)) / 100
	kept := len(head) + fit*100 - 1
	want := head + strings.TrimSuffix(strings.Repeat(line+"\n", fit), "\n") + "\n[" + strconv.Itoa(total-kept) + " bytes cut]"
	if err != nil || got != want {
		t.Fatalf("got (%v) %d bytes, tail %q; want %d bytes", err, len(got), got[max(0, len(got)-40):], len(want))
	}
}

func TestIncidentsTextRefusals(t *testing.T) {
	s := newFakeClusterStore()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	h := func(d time.Duration) time.Time { return now.Add(d) }
	for _, c := range []struct {
		name         string
		since, until time.Time
		want         string
	}{
		{"until before since", h(-time.Hour), h(-2 * time.Hour), "until comes before since: ask with since before until."},
		{"too wide", h(-26 * time.Hour), h(-time.Hour), "a window holds at most 24 hours: ask with since and until at most 24 hours apart."},
		{"too old", h(-31 * 24 * time.Hour), h(-31*24*time.Hour + time.Hour), "the incident log keeps 30 days: ask for a window after 2026-10-24."},
		{"one missing", h(-time.Hour), time.Time{}, "ask with both since and until, or with a session."},
	} {
		_, err := IncidentsText(t.Context(), s, c.since, c.until, now)
		var no *FactsRefusal
		if !errors.As(err, &no) || no.Msg != c.want {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	_, err := SessionFactsText(t.Context(), s, "t1", "s9")
	if err == nil || err.Error() != "session s9 has no death record: it ended with a push or before the factory recorded deaths. Read card_step for its end." {
		t.Errorf("no record: %v", err)
	}
	_, err = ClusterFactsText(t.Context(), s, "t1", "s1", h(-time.Hour), h(0), now)
	if err == nil || err.Error() != "ask for a session or for a window, not both." {
		t.Errorf("both: %v", err)
	}
}

func TestIncidentsTextListsTheWindowOldestFirst(t *testing.T) {
	s := newFakeClusterStore()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	at := now.Add(-2 * time.Hour)
	s.AddPlatformIncidents(t.Context(), []PlatformIncident{
		{Key: "b", At: at.Add(time.Minute), Kind: IncidentRestart, Subject: "litellm", Detail: map[string]any{"cause": "guard", "after": "1m"}},
		{Key: "a", At: at, Kind: IncidentNodeBoot, Subject: "n1", Node: "n1"},
	})
	got, err := IncidentsText(t.Context(), s, now.Add(-3*time.Hour), now, now)
	want := "2026-11-23T10:00:00Z node_boot n1 on n1\n2026-11-23T10:01:00Z restart litellm after=1m cause=guard"
	if err != nil || got != want {
		t.Fatalf("got (%v)\n%s\nwant\n%s", err, got, want)
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
