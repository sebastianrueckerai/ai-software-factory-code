package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The real client reads allocatable from the nodes and the requests of
// every live pod from the pods list: a pod's request is the larger of its
// containers' sum and its largest init container; a pod with no node is
// unplaced.
func TestNodeStorageReadsNodesAndPodRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"n1"},"status":{"allocatable":{"ephemeral-storage":"96000000000"}}},{"metadata":{"name":"n2"},"status":{"allocatable":{"ephemeral-storage":"80Gi"}}}]}`)
		case "/api/v1/pods":
			if got := r.URL.Query().Get("fieldSelector"); got != "status.phase!=Succeeded,status.phase!=Failed" {
				t.Errorf("fieldSelector = %q", got)
			}
			fmt.Fprint(w, `{"items":[
			 {"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"ephemeral-storage":"18Gi"}}},{"resources":{"requests":{"ephemeral-storage":"2Gi"}}}],"initContainers":[{"resources":{"requests":{"ephemeral-storage":"18Gi"}}}]}},
			 {"spec":{"nodeName":"n1","containers":[{"resources":{"requests":{"cpu":"1"}}}]}},
			 {"spec":{"nodeName":"n2","containers":[{"resources":{"requests":{"ephemeral-storage":"1Gi"}}}],"initContainers":[{"resources":{"requests":{"ephemeral-storage":"22Gi"}}}]}},
			 {"spec":{"containers":[{"resources":{"requests":{"ephemeral-storage":"22Gi"}}}]}}]}`)
		default:
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
	want := []NodeStorage{{"n1", 96000000000, 20 * gi}, {"n2", 80 * gi, 22 * gi}}
	if fmt.Sprint(nodes) != fmt.Sprint(want) || fmt.Sprint(unplaced) != fmt.Sprint([]int64{22 * gi}) {
		t.Fatalf("nodes %v, unplaced %v; want %v and [22 GiB]", nodes, unplaced, want)
	}
}
