package factoryd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gi = int64(1) << 30

// A runner pod without a project cache requests its three emptyDirs,
// 8 + 2 + 4 + 8 (the cache volume) = 22 GiB. A node with 22 GiB free takes one pod, and the poll
// spends what it read: the second job stays queued. With the node's
// storage taken by other pods or by a pod no node holds yet, or unreadable,
// no job is spawned; with room for both, both are.
func TestCISpawnerSpawnsOnlyWhatFitsTheNodesStorage(t *testing.T) {
	for name, tc := range map[string]struct {
		nodes    []NodeStorage
		unplaced []int64
		err      error
		want     []bool
	}{
		"one pod's room":       {nodes: []NodeStorage{{Name: "n1", Allocatable: 40 * gi, Requested: 18 * gi}}, want: []bool{true, false}},
		"room for both":        {nodes: []NodeStorage{{Name: "n1", Allocatable: 70 * gi, Requested: 18 * gi}}, want: []bool{true, true}},
		"two nodes, one each":  {nodes: []NodeStorage{{Name: "n1", Allocatable: 22 * gi}, {Name: "n2", Allocatable: 22 * gi}}, want: []bool{true, true}},
		"a pending pod's room": {nodes: []NodeStorage{{Name: "n1", Allocatable: 40 * gi, Requested: 18 * gi}}, unplaced: []int64{gi}, want: []bool{false, false}},
		"no node has room":     {nodes: []NodeStorage{{Name: "n1", Allocatable: 40 * gi, Requested: 19 * gi}}, want: []bool{false, false}},
		"nodes unreadable":     {nodes: []NodeStorage{{Name: "n1", Allocatable: 1 << 50}}, err: fmt.Errorf("apiserver says no"), want: []bool{false, false}},
	} {
		t.Run(name, func(t *testing.T) {
			kube := newFakeKube()
			kube.nodes, kube.unplaced, kube.nodesErr = tc.nodes, tc.unplaced, tc.err
			hub := &ciHub{jobs: []CIJob{
				{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
				{ID: 2, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
			}}
			spawner := newTestCISpawner(t, kube, hub)
			if got := spawner.podStorage(ciJobKind{label: "pr-untrusted"}, ""); got != 22*gi {
				t.Fatalf("pod storage = %d GiB, want 22", got/gi)
			}
			spawner.poll(context.Background())
			for i, want := range tc.want {
				ns := fmt.Sprintf("factory-agents-ci-%d", i+1)
				if _, ok := kube.namespaces[ns]; ok != want {
					t.Errorf("namespace %s present = %v, want %v", ns, ok, want)
				}
			}
		})
	}
}

// The node-fit check reserves the 10 GiB cap of every cache the factory
// runs (a project's CI cache and its agents' cache) on every node, once per
// cache, whether or not a pod mounts it: the directory outlives its pods. A
// pod of 22 GiB with one project's two caches needs 22 + 2 x 10 = 42 GiB.
func TestCISpawnerReservesEveryCachesCapOnEveryNode(t *testing.T) {
	demo := CIRepo{Repo: codeCIRepos[0].Repo, Token: "registrar-tok", Cache: "cache-ci-demo", AgentCache: "cache-agent-demo"}
	for name, tc := range map[string]struct {
		repos []CIRepo
		node  int64 // allocatable, GiB
		want  []bool
	}{
		"no room for the caps": {[]CIRepo{demo}, 41, []bool{false, false}},
		"one pod, two caps":    {[]CIRepo{demo}, 63, []bool{true, false}},
		"both pods, caps once": {[]CIRepo{demo}, 64, []bool{true, true}},
	} {
		t.Run(name, func(t *testing.T) {
			kube := newFakeKube()
			kube.nodes = []NodeStorage{{Name: "n1", Allocatable: tc.node * gi}}
			hub := &ciHub{jobs: []CIJob{
				{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
				{ID: 2, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
			}}
			spawner := newTestCISpawner(t, kube, hub)
			spawner.cfg.Repos = func(context.Context) ([]CIRepo, error) { return tc.repos, nil }
			spawner.poll(context.Background())
			for i, want := range tc.want {
				ns := fmt.Sprintf("factory-agents-ci-%d", i+1)
				if _, ok := kube.namespaces[ns]; ok != want {
					t.Errorf("namespace %s present = %v, want %v", ns, ok, want)
				}
			}
		})
	}
}

// Each cache counts once, whatever the number of repositories that name it.
func TestCacheReserveCountsEachCacheOnce(t *testing.T) {
	repos := []CIRepo{
		{Cache: "cache-ci-a", AgentCache: "cache-agent-a"},
		{Cache: "cache-ci-a", AgentCache: "cache-agent-a"},
		{Cache: "cache-ci-b", AgentCache: "cache-agent-b"},
		{},
	}
	if got := cacheReserve(repos); got != 4*cacheCapBytes {
		t.Fatalf("reserve = %d GiB, want 4 caches x 10 GiB", got/gi)
	}
}

// The real client reads the schedulable nodes (a cordoned or NotReady one
// gives no room) and, in each task namespace and in forgejo, every live
// pod, page by page: a pod's request is the larger of its containers' sum
// and its largest init container; a pod with no node is unplaced; a pod
// with a quantity it cannot read fills its node.
func TestNodeStorageReadsNodesAndPodRequests(t *testing.T) {
	pods := func(w http.ResponseWriter, ns string, r *http.Request) {
		if got := r.URL.Query().Get("fieldSelector"); got != "status.phase!=Succeeded,status.phase!=Failed" {
			t.Errorf("fieldSelector = %q", got)
		}
		if r.URL.Query().Get("limit") != "200" {
			t.Errorf("limit = %q, want 200", r.URL.Query().Get("limit"))
		}
		switch ns + "/" + r.URL.Query().Get("continue") {
		case "factory-agents-1/":
			fmt.Fprint(w, `{"metadata":{"continue":"page2"},"items":[
			 {"metadata":{"name":"a"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"18Gi"}}},{"resources":{"requests":{"ephemeral-storage":"2Gi"}}}],"initContainers":[{"resources":{"requests":{"ephemeral-storage":"18Gi"}}}]}},
			 {"metadata":{"name":"b"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"cpu":"1"}}}]}}]}`)
		case "factory-agents-1/page2":
			fmt.Fprint(w, `{"metadata":{},"items":[
			 {"metadata":{"name":"c"},"spec":{"nodeName":"n2","containers":[{"resources":{"requests":{"ephemeral-storage":"1Gi"}}}],"initContainers":[{"resources":{"requests":{"ephemeral-storage":"22Gi"}}}]}},
			 {"metadata":{"name":"d"},"spec":{"containers":[{"resources":{"requests":{"ephemeral-storage":"22Gi"}}}]}},
			 {"metadata":{"name":"cordoned"},"spec":{"nodeName":"n3","containers":[{"resources":{"requests":{"ephemeral-storage":"50Gi"}}}]}}]}`)
		case "forgejo/":
			fmt.Fprint(w, `{"metadata":{},"items":[
			 {"metadata":{"name":"runner"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"50Gi"}}}]}},
			 {"metadata":{"name":"odd"},"spec":{"nodeName":"n2","containers":[{"resources":{"requests":{"ephemeral-storage":"banana"}}}]}}]}`)
		default:
			t.Errorf("unexpected pods list %s", r.URL)
			http.NotFound(w, r)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[
			 {"metadata":{"name":"n1"},"status":{"allocatable":{"ephemeral-storage":"96000000000"},"conditions":[{"type":"Ready","status":"True"}]}},
			 {"metadata":{"name":"n2"},"status":{"allocatable":{"ephemeral-storage":"80Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
			 {"metadata":{"name":"n3"},"spec":{"unschedulable":true},"status":{"allocatable":{"ephemeral-storage":"90Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
			 {"metadata":{"name":"n4"},"status":{"allocatable":{"ephemeral-storage":"90Gi"},"conditions":[{"type":"Ready","status":"False"}]}}]}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"factory-agents-1"}}]}`)
		default:
			if ns, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/pods"); ok {
				pods(w, ns, r)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := SaDirForTest(dir)
	t.Cleanup(func() { SaDirForTest(old) })
	nodes, unplaced, err := (&InClusterKube{Host: srv.URL, HTTP: srv.Client()}).NodeStorage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// n1: 20 GiB (a: max(18+2, 18)) + 50 GiB (runner); n2: 22 GiB (c: max(1, 22)),
	// then the unreadable pod fills it; n3 and n4 are no nodes
	want := []NodeStorage{{"n1", 96000000000, 70 * gi}, {"n2", 80 * gi, 80 * gi}}
	if fmt.Sprint(nodes) != fmt.Sprint(want) || fmt.Sprint(unplaced) != fmt.Sprint([]int64{22 * gi}) {
		t.Fatalf("nodes %v, unplaced %v; want %v and [22 GiB]", nodes, unplaced, want)
	}
}

// Every form of a Kubernetes quantity reads as bytes, rounded up.
func TestParseBytesReadsEveryQuantityForm(t *testing.T) {
	for q, want := range map[string]int64{
		"": 0, "0": 0, "123": 123, "96000000000": 96000000000, "18Gi": 18 << 30, "1536Mi": 1536 << 20, "1.5Gi": 3 << 29,
		"1k": 1000, "2M": 2e6, "3G": 3e9, "4T": 4e12, "1P": 1e15, "1Pi": 1 << 50, "1e9": 1e9, "129e6": 129e6, "12E3": 12000,
		"1E": 1e18, "500m": 1, "1500m": 2, "1u": 1, "1Ki": 1024,
	} {
		if got, err := parseBytes(q); err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", q, got, err, want)
		}
	}
	for _, q := range []string{"banana", "-1Gi", "1Xi", "Gi", "1e30"} {
		if got, err := parseBytes(q); err == nil {
			t.Errorf("parseBytes(%q) = %d, want an error", q, got)
		}
	}
}

// With no job waiting, a poll reads no node.
func TestCISpawnerReadsNodesOnlyForAWaitingJob(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background())
	if kube.nodeReads != 0 {
		t.Fatalf("node reads = %d with no job waiting, want 0", kube.nodeReads)
	}
	hub.jobs = []CIJob{{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}
	spawner.poll(context.Background())
	if kube.nodeReads != 1 {
		t.Fatalf("node reads = %d with a job waiting, want 1", kube.nodeReads)
	}
}

// A smoke runner requests 8 + 2 + 4 = 14 GiB (no cache volume). The node
// keeps 10 GiB for the project's cache (the smoke test's repository names
// one): with 24 GiB it takes the first waiting smoke job, and the second
// stays queued.
func TestCISpawnerFitsSmokeRunnersToo(t *testing.T) {
	kube := newFakeKube()
	kube.nodes = []NodeStorage{{Name: "n1", Allocatable: 24 * gi}}
	hub := &smokeHub{jobs: []CIJob{smokeJob(11, "waiting", "h-11", 0), smokeJob(12, "waiting", "h-12", 0)},
		tasks: []ActionTask{{ID: 501, RunNumber: 7}}, runs: demoRuns}
	spawner, _ := hub.spawner(t, kube)
	spawner.poll(context.Background())
	for id, want := range map[string]bool{"factory-agents-ci-11": true, "factory-agents-ci-12": false} {
		if _, ok := kube.namespaces[id]; ok != want {
			t.Errorf("namespace %s present = %v, want %v", id, ok, want)
		}
	}
}

// A task namespace gone since the listing (404) or going (Terminating) is
// left out; a failed read of forgejo, whose runner holds 50 GiB, fails the
// whole read, so the job stays queued instead of counting without it.
func TestNodeStorageSkipsGoneTaskNamespacesButNotForgejo(t *testing.T) {
	forgejo := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"n1"},"status":{"allocatable":{"ephemeral-storage":"80Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"factory-agents-gone"}},{"metadata":{"name":"factory-agents-ending","deletionTimestamp":"2026-11-10T12:00:00Z"}}]}`)
		case "/api/v1/namespaces/factory-agents-gone/pods":
			http.NotFound(w, r)
		case "/api/v1/namespaces/forgejo/pods":
			if forgejo != http.StatusOK {
				http.Error(w, "forbidden", forgejo)
				return
			}
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"name":"runner"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"50Gi"}}}]}}]}`)
		default:
			t.Errorf("unexpected call %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := SaDirForTest(dir)
	t.Cleanup(func() { SaDirForTest(old) })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	if _, _, err := k.NodeStorage(context.Background()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden from the forgejo namespace", err)
	}
	forgejo = http.StatusOK
	nodes, _, err := k.NodeStorage(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].Requested != 50*gi {
		t.Fatalf("nodes %v, err %v; want n1 with the runner's 50 GiB", nodes, err)
	}
}
