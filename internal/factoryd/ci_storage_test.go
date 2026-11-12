package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// The runner pod is pinned to the node its fit check chose, with a required
// node affinity on the node's name, so the scheduler cannot place it on
// a node whose room the reserve took: the 22 GiB pod fits n2 (60 - 20 - 0 =
// 40 free in the model) and not n1 (30 - 20 = 10).
func TestCISpawnerPinsThePodToTheNodeItChose(t *testing.T) {
	kube := newFakeKube()
	kube.nodes = []NodeStorage{{Name: "n1", Allocatable: 30 * gi}, {Name: "n2", Allocatable: 60 * gi}}
	hub := &ciHub{jobs: []CIJob{{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.cfg.Repos = func(context.Context) ([]CIRepo, error) {
		return []CIRepo{{Repo: codeCIRepos[0].Repo, Token: "registrar-tok", Cache: "cache-ci-demo", AgentCache: "cache-agent-demo"}}, nil
	}
	spawner.poll(context.Background())
	pod := kube.pods["factory-agents-ci-1/"+ciPodName]
	if pod == nil {
		t.Fatalf("no pod was created: %v", kube.ops)
	}
	want := map[string]any{"nodeAffinity": map[string]any{
		"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{map[string]any{
			"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "In", "values": []any{"n2"}}}}}}}}
	if got := pod.spec["spec"].(map[string]any)["affinity"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("affinity = %v, want %v", got, want)
	}
}

// When the caches' caps leave no node room for a runner pod that the
// requests alone leave room for, the poll says so once, naming the caches
// and the figures, and counts it on the CI metrics, whatever the number of
// waiting jobs: the node has 41 GiB, the reserve is 2 x 10, the pod 22.
func TestCISpawnerSaysWhenTheReserveLeavesNoRoom(t *testing.T) {
	m, reader := testMetrics(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	kube := newFakeKube()
	kube.nodes = []NodeStorage{{Name: "n1", Allocatable: 41 * gi}}
	hub := &ciHub{jobs: []CIJob{
		{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
		{ID: 2, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
	}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.cfg.Queue = NewCIQueue(m)
	spawner.cfg.Repos = func(context.Context) ([]CIRepo, error) {
		return []CIRepo{{Repo: codeCIRepos[0].Repo, Token: "registrar-tok", Cache: "cache-ci-demo", AgentCache: "cache-agent-demo"}}, nil
	}
	spawner.poll(context.Background())
	want := "the caches' caps leave no node room for a pull request runner pod: 20 GiB reserved on every node for 2 caches (cache-ci-demo, cache-agent-demo), the roomiest node has 41 GiB free of requests and the pod requests 22 GiB; pull request jobs stay queued"
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("the log says it %d times, want once:\n%s", n, logs.String())
	}
	if got := sums(t, reader, "factory.ci.storage.starved"); !reflect.DeepEqual(got, map[string]float64{"reserve_gib=20": 1}) {
		t.Fatalf("starved = %v, want one poll at a 20 GiB reserve", got)
	}
}

// A node with room for the pod's storage but not for its memory (1 GiB, with
// 2 GiB allocatable and 1.5 GiB requested), or its CPU (1 CPU, with 1.5 CPU
// allocatable and 1 CPU requested), takes no pod; a node with a NoSchedule or
// NoExecute taint, which a runner pod does not tolerate, takes none either,
// however much it has free: the pod goes to the untainted n3, and a taint
// that only prefers (PreferNoSchedule) does not keep it off.
func TestCISpawnerFitsCPUMemoryAndTaintsBesideStorage(t *testing.T) {
	big := 200 * gi
	for name, tc := range map[string]struct {
		nodes []NodeStorage
		want  string // the node the pod is pinned to, "" for none
	}{
		"memory short": {[]NodeStorage{{Name: "n1", Allocatable: big, AllocatableCPU: 4000, AllocatableMemory: 2 * gi, RequestedMemory: 3 * gi / 2}}, ""},
		"cpu short":    {[]NodeStorage{{Name: "n1", Allocatable: big, AllocatableCPU: 1500, RequestedCPU: 1000, AllocatableMemory: 8 * gi}}, ""},
		"all room":     {[]NodeStorage{{Name: "n1", Allocatable: big, AllocatableCPU: 4000, AllocatableMemory: 8 * gi}}, "n1"},
		"tainted roomier": {[]NodeStorage{
			{Name: "n1", Allocatable: big, AllocatableCPU: 4000, AllocatableMemory: 8 * gi, Taints: []NodeTaint{{Key: "dedicated", Effect: "NoSchedule"}}},
			{Name: "n3", Allocatable: 60 * gi, AllocatableCPU: 4000, AllocatableMemory: 8 * gi}}, "n3"},
		"a NoExecute taint": {[]NodeStorage{{Name: "n1", Allocatable: big, AllocatableCPU: 4000, AllocatableMemory: 8 * gi, Taints: []NodeTaint{{Key: "k", Effect: "NoExecute"}}}}, ""},
		"a soft taint":      {[]NodeStorage{{Name: "n1", Allocatable: big, AllocatableCPU: 4000, AllocatableMemory: 8 * gi, Taints: []NodeTaint{{Key: "k", Effect: "PreferNoSchedule"}}}}, "n1"},
	} {
		t.Run(name, func(t *testing.T) {
			kube := newFakeKube()
			kube.nodes = tc.nodes
			hub := &ciHub{jobs: []CIJob{{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
			spawner := newTestCISpawner(t, kube, hub)
			spawner.poll(context.Background())
			pod := kube.pods["factory-agents-ci-1/"+ciPodName]
			if (pod != nil) != (tc.want != "") {
				t.Fatalf("pod created = %v, want node %q", pod != nil, tc.want)
			}
			if pod != nil {
				terms := pod.spec["spec"].(map[string]any)["affinity"].(map[string]any)["nodeAffinity"].(map[string]any)["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)["nodeSelectorTerms"].([]any)
				if got := terms[0].(map[string]any)["matchFields"].([]any)[0].(map[string]any)["values"]; !reflect.DeepEqual(got, []any{tc.want}) {
					t.Fatalf("pinned to %v, want %s", got, tc.want)
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
// gives no room) and every live pod of the cluster, in every namespace,
// page by page, as the scheduler counts them: a pod's request is the larger
// of its containers' sum and its largest init container, plus its
// overhead; a pod with no node is unplaced; a pod with a quantity it
// cannot read fills its node.
func TestNodeStorageReadsNodesAndPodRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[
			 {"metadata":{"name":"n1"},"spec":{"taints":[{"key":"dedicated","value":"ci","effect":"NoSchedule"}]},"status":{"allocatable":{"ephemeral-storage":"96000000000","cpu":"4","memory":"16Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
			 {"metadata":{"name":"n2"},"status":{"allocatable":{"ephemeral-storage":"80Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
			 {"metadata":{"name":"n3"},"spec":{"unschedulable":true},"status":{"allocatable":{"ephemeral-storage":"90Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
			 {"metadata":{"name":"n4"},"status":{"allocatable":{"ephemeral-storage":"90Gi"},"conditions":[{"type":"Ready","status":"False"}]}}]}`)
		case "/api/v1/pods":
			if got := r.URL.Query().Get("fieldSelector"); got != "status.phase!=Succeeded,status.phase!=Failed" {
				t.Errorf("fieldSelector = %q", got)
			}
			if r.URL.Query().Get("limit") != "200" {
				t.Errorf("limit = %q, want 200", r.URL.Query().Get("limit"))
			}
			switch r.URL.Query().Get("continue") {
			case "":
				fmt.Fprint(w, `{"metadata":{"continue":"page2"},"items":[
				 {"metadata":{"namespace":"factory-agents-1","name":"a"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"18Gi","cpu":"500m","memory":"1Gi"}}},{"resources":{"requests":{"ephemeral-storage":"2Gi","cpu":"1","memory":"2Gi"}}}],"initContainers":[{"resources":{"requests":{"ephemeral-storage":"18Gi","cpu":"2","memory":"1Gi"}}}]}},
				 {"metadata":{"namespace":"factory-agents-1","name":"b"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"cpu":"1"}}}]}}]}`)
			case "page2":
				fmt.Fprint(w, `{"metadata":{"continue":"page3"},"items":[
				 {"metadata":{"namespace":"factory-agents-1","name":"c"},"spec":{"nodeName":"n2","containers":[{"resources":{"requests":{"ephemeral-storage":"1Gi"}}}],"initContainers":[{"resources":{"requests":{"ephemeral-storage":"22Gi"}}}]}},
				 {"metadata":{"namespace":"factory-agents-1","name":"d"},"spec":{"containers":[{"resources":{"requests":{"ephemeral-storage":"22Gi"}}}]}},
				 {"metadata":{"namespace":"factory-agents-1","name":"cordoned"},"spec":{"nodeName":"n3","containers":[{"resources":{"requests":{"ephemeral-storage":"50Gi"}}}]}}]}`)
			case "page3":
				fmt.Fprint(w, `{"metadata":{},"items":[
				 {"metadata":{"namespace":"forgejo","name":"runner"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"50Gi"}}}]}},
				 {"metadata":{"namespace":"kube-system","name":"dns"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"cpu":"250m","memory":"512Mi"}}}],"overhead":{"cpu":"50m","memory":"64Mi"}}},
				 {"metadata":{"namespace":"forgejo","name":"odd"},"spec":{"nodeName":"n2","containers":[{"resources":{"requests":{"ephemeral-storage":"banana"}}}]}}]}`)
			default:
				t.Errorf("unexpected page %s", r.URL)
			}
		default:
			t.Errorf("unexpected read %s", r.URL)
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
	// then the unreadable pod fills it; n3 and n4 are no nodes. n1's CPU: pod a
	// max(500m + 1 CPU, 2 CPU init) = 2000m, pod b 1000m, kube-system's dns 250m
	// and its overhead 50m: 3300m; its memory: a max(1Gi + 2Gi, 1Gi) = 3Gi, dns
	// 512Mi and its overhead 64Mi: 3Gi + 576Mi
	want := []NodeStorage{{Name: "n1", Allocatable: 96000000000, Requested: 70 * gi, AllocatableCPU: 4000, RequestedCPU: 3300,
		AllocatableMemory: 16 * gi, RequestedMemory: 3*gi + 576<<20, Taints: []NodeTaint{{"dedicated", "ci", "NoSchedule"}}},
		{Name: "n2", Allocatable: 80 * gi, Requested: 80 * gi}}
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

// A failed read of the pods fails the whole read, so the job stays queued
// instead of counting without them: a node can hold forgejo's runner of
// 50 GiB.
func TestNodeStorageFailsWhenThePodsCannotBeRead(t *testing.T) {
	pods := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"n1"},"status":{"allocatable":{"ephemeral-storage":"80Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case "/api/v1/pods":
			if pods != http.StatusOK {
				http.Error(w, "forbidden", pods)
				return
			}
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"namespace":"forgejo","name":"runner"},"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"50Gi"}}}]}}]}`)
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
		t.Fatalf("err = %v, want ErrForbidden from the pods list", err)
	}
	pods = http.StatusOK
	nodes, _, err := k.NodeStorage(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].Requested != 50*gi {
		t.Fatalf("nodes %v, err %v; want n1 with the runner's 50 GiB", nodes, err)
	}
}

func TestParseMilliReadsCPUQuantities(t *testing.T) {
	for q, want := range map[string]int64{"": 0, "1": 1000, "0.5": 500, "500m": 500, "1500m": 1500, "2": 2000, "0.0005": 1} {
		if got, err := parseMilli(q); err != nil || got != want {
			t.Errorf("parseMilli(%q) = %d, %v; want %d", q, got, err, want)
		}
	}
	for _, q := range []string{"-1", "x", "1Gi"} {
		if got, err := parseMilli(q); err == nil {
			t.Errorf("parseMilli(%q) = %d, want an error", q, got)
		}
	}
}
