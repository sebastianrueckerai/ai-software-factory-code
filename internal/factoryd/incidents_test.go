package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signozKernelServer(t *testing.T, logins *atomic.Int32, query func(w http.ResponseWriter, r *http.Request, n int32)) *SigNoz {
	t.Helper()
	var queries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/sessions/context":
			io.WriteString(w, `{"status":"success","data":{"orgs":[{"id":"org-1"}]}}`)
		case "POST /api/v2/sessions/email_password":
			n := logins.Add(1)
			io.WriteString(w, `{"status":"success","data":{"accessToken":"tok-`+string(rune('0'+n))+`"}}`)
		case "POST /api/v5/query_range":
			query(w, r, queries.Add(1))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return &SigNoz{BaseURL: srv.URL, Email: "viewer@example.com", Password: "pw", HTTP: srv.Client()}
}

const kernelAnswer = `{"status":"success","data":{"type":"raw","data":{"results":[{"queryName":"A","nextCursor":"","rows":[
 {"timestamp":"2026-11-23T14:02:00.5Z","data":{"body":"[talos] Sending SIGKILL to cgroup {\"component\": \"controller-runtime\", \"cgroup\": \"/sys/fs/cgroup/kubepods/burstable/pod91adbc7d-6406-4a24-ac91-c1979b028d36\"}\n",
  "attributes_number":{"seq":711,"clock":6252819000},"resources_string":{"k8s.node.name":"sf-2"}}},
 {"timestamp":"2026-11-23T14:03:00Z","data":{"body":"[talos] eth0: renamed from tmpff297\n","attributes_number":{"seq":712},"resources_string":{"k8s.node.name":"sf-2"}}}
]}]}}}`

func TestSigNozKernelLinesQueriesTheWindow(t *testing.T) {
	var logins atomic.Int32
	to := time.Date(2026, 11, 23, 14, 5, 0, 0, time.UTC)
	from := to.Add(-KernelWindow)
	s := signozKernelServer(t, &logins, func(w http.ResponseWriter, r *http.Request, _ int32) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		var body struct {
			SchemaVersion  string `json:"schemaVersion"`
			Start, End     int64
			RequestType    string `json:"requestType"`
			CompositeQuery struct {
				Queries []struct {
					Type string `json:"type"`
					Spec struct {
						Name, Signal string
						Filter       struct{ Expression string }
						Order        []struct {
							Key       struct{ Name string }
							Direction string
						}
						Limit int
					}
				}
			} `json:"compositeQuery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		q := body.CompositeQuery.Queries
		const filter = "resource.service.name = 'talos-kmsg' AND (body CONTAINS 'Sending SIGKILL to cgroup' OR body CONTAINS 'oom-kill:')"
		if body.SchemaVersion != "v1" || body.RequestType != "raw" || len(q) != 1 || q[0].Type != "builder_query" ||
			q[0].Spec.Name != "A" || q[0].Spec.Signal != "logs" || q[0].Spec.Filter.Expression != filter || q[0].Spec.Limit != 1000 ||
			len(q[0].Spec.Order) != 2 || q[0].Spec.Order[0].Key.Name != "timestamp" || q[0].Spec.Order[1].Key.Name != "id" ||
			q[0].Spec.Order[0].Direction != "asc" || q[0].Spec.Order[1].Direction != "asc" {
			t.Errorf("query = %+v", body)
		}
		if body.Start != from.UnixMilli() || body.End != to.UnixMilli() {
			t.Errorf("window = %d..%d, want %d..%d", body.Start, body.End, from.UnixMilli(), to.UnixMilli())
		}
		io.WriteString(w, kernelAnswer)
	})
	got, err := s.KernelLines(context.Background(), from, to)
	want := KernelKill{Source: "guard", Node: "sf-2", PodUID: "91adbc7d-6406-4a24-ac91-c1979b028d36", Seq: 711, At: time.Date(2026, 11, 23, 14, 2, 0, 500000000, time.UTC)}
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("KernelLines = %+v, %v; want [%+v]", got, err, want)
	}
}

func TestSigNozKernelLinesLogsInAgainAfter401(t *testing.T) {
	var logins atomic.Int32
	s := signozKernelServer(t, &logins, func(w http.ResponseWriter, r *http.Request, n int32) {
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			t.Errorf("second query sent %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, kernelAnswer)
	})
	now := time.Now()
	got, err := s.KernelLines(context.Background(), now.Add(-time.Minute), now)
	if err != nil || len(got) != 1 || logins.Load() != 2 {
		t.Fatalf("got %+v, %v after %d logins; want one kill after 2", got, err, logins.Load())
	}
}

type fakeKernel struct {
	kills []KernelKill
	err   error
	from  time.Time
	to    time.Time
}

func (f *fakeKernel) KernelLines(_ context.Context, from, to time.Time) ([]KernelKill, error) {
	f.from, f.to = from, to
	return f.kills, f.err
}

func sweepFixture(now time.Time) (*IncidentSweep, *fakeClusterStore, *fakeKernel) {
	kube := newFakeKube()
	kube.servicePods = map[string][]ServicePod{"factory/app=factoryd": {
		{Namespace: "factory", Name: "factoryd-1", UID: "uid-f", Node: "sf-1", Phase: "Running",
			Containers: []ServiceContainer{
				{Name: "factoryd", LastReason: "OOMKilled", LastExitCode: 137, LastFinished: now.Add(-time.Hour), RunningSince: now.Add(-59 * time.Minute)},
				{Name: "sidecar"},
				{Name: "ancient", LastFinished: now.Add(-40 * 24 * time.Hour)},
			}},
		{Namespace: "factory", Name: "migrate-1", UID: "uid-m", Node: "sf-1", Phase: "Failed",
			Containers: []ServiceContainer{{Name: "migrate", LastReason: "Error", LastExitCode: 1, LastFinished: now.Add(-30 * time.Minute)}}},
	}}
	kube.nodeStates = map[string]NodeState{
		"sf-1": {Name: "sf-1", BootID: "b1", Ready: true},
		"sf-2": {Name: "sf-2", BootID: "b2", Ready: false, Since: now.Add(-10 * time.Minute)},
		"sf-3": {Name: "sf-3", BootID: "b3", Ready: false},
	}
	kernel := &fakeKernel{kills: []KernelKill{
		{Source: "guard", Node: "sf-2", PodUID: "uid-k", Seq: 711, At: now.Add(-2 * time.Minute)},
		{Source: "kernel", Node: "sf-2", PodUID: "uid-n", Seq: 712, Constraint: "CONSTRAINT_NONE", At: now.Add(-90*time.Second + 250*time.Millisecond)},
		{Source: "kernel", Node: "sf-2", PodUID: "uid-m", Seq: 713, Constraint: "CONSTRAINT_MEMCG", At: now.Add(-80 * time.Second)},
	}}
	store := newFakeClusterStore()
	return &IncidentSweep{Store: store, Kube: kube, Kernel: kernel, Now: func() time.Time { return now }}, store, kernel
}

func incidentsByKey(t *testing.T, store ClusterStore, now time.Time) (map[string]PlatformIncident, int) {
	t.Helper()
	rows, err := store.PlatformIncidents(context.Background(), now.Add(-2*time.Hour), now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]PlatformIncident{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	return byKey, len(rows)
}

func TestIncidentSweepRecordsEachSourceOnce(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	w, store, kernel := sweepFixture(now)
	if err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !kernel.to.Equal(now) || !kernel.from.Equal(now.Add(-KernelWindow)) {
		t.Errorf("kernel window %v..%v", kernel.from, kernel.to)
	}
	// a pass a minute later reads an overlapping window and finds the same incidents
	later := now.Add(time.Minute)
	w2 := &IncidentSweep{Store: store, Kube: w.Kube, Kernel: kernel, Now: func() time.Time { return later }}
	if err := w2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !kernel.to.Equal(later) || !kernel.from.Equal(later.Add(-KernelWindow)) {
		t.Errorf("second kernel window %v..%v", kernel.from, kernel.to)
	}
	byKey, n := incidentsByKey(t, store, later)
	if n != 8 {
		t.Fatalf("%d rows, want 8 (3 kills, 1 restart, 3 boots, 1 not ready): %v", n, byKey)
	}
	kill := byKey["kmsg/sf-2/711/"+now.Add(-2*time.Minute).Format("2006-01-02T15:04:05Z")]
	if kill.Kind != IncidentGuardKill || kill.Subject != "uid-k" || kill.PodUID != "uid-k" || kill.Node != "sf-2" || len(kill.Detail) != 0 {
		t.Errorf("guard row %+v", kill)
	}
	oomAt := now.Add(-90*time.Second + 250*time.Millisecond)
	oom := byKey["kmsg/sf-2/712/"+oomAt.Format("2006-01-02T15:04:05")+".25Z"]
	if oom.Kind != IncidentKernelOOM || oom.Subject != "uid-n" || oom.PodUID != "uid-n" || oom.Detail["constraint"] != "CONSTRAINT_NONE" || !oom.At.Equal(oomAt) {
		t.Errorf("node oom row %+v", oom)
	}
	memcg := byKey["kmsg/sf-2/713/"+now.Add(-80*time.Second).Format("2006-01-02T15:04:05Z")]
	if memcg.Kind != IncidentKernelOOM || memcg.Subject != "uid-m" || memcg.Detail["constraint"] != "CONSTRAINT_MEMCG" {
		t.Errorf("memcg row %+v", memcg)
	}
	fin := now.Add(-time.Hour)
	restart := byKey["restart/uid-f/factoryd/"+fin.Format("2006-01-02T15:04:05Z")]
	if restart.Kind != IncidentRestart || restart.Subject != "factoryd" || !restart.At.Equal(fin) || restart.Until == nil || !restart.Until.Equal(now.Add(-59*time.Minute)) ||
		restart.Detail["container"] != "factoryd" || restart.Detail["reason"] != "OOMKilled" || restart.Detail["exit_code"] != int32(137) {
		t.Errorf("restart row %+v", restart)
	}
	if boot := byKey["boot/sf-1/b1"]; boot.Kind != IncidentNodeBoot || !boot.At.Equal(now) {
		t.Errorf("boot row %+v: At is the first pass's time", boot)
	}
	if nr := byKey["notready/sf-2/"+now.Add(-10*time.Minute).Format("2006-01-02T15:04:05Z")]; nr.Kind != IncidentNodeNotReady || !nr.At.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("not ready row %+v", nr)
	}
	for _, k := range []string{"boot/sf-2/b2", "boot/sf-3/b3"} {
		if _, ok := byKey[k]; !ok {
			t.Errorf("no row %s", k)
		}
	}
}

func TestIncidentSweepKeepsGoingWhenOneSourceFails(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	w, store, kernel := sweepFixture(now)
	kernel.err = errors.New("signoz is down")
	err := w.Run(context.Background())
	if err == nil || !errors.Is(err, kernel.err) {
		t.Fatalf("Run = %v, want the kernel error", err)
	}
	if _, n := incidentsByKey(t, store, now); n != 5 {
		t.Errorf("%d rows, want the restart and the four node rows", n)
	}
}

func TestWatchedServicesSelectors(t *testing.T) {
	want := []WatchedService{
		{"factoryd", "factory", "app=factoryd"},
		{"factory-gateway", "factory", "app=factory-gateway"},
		{"litellm", "platform", "app.kubernetes.io/name=litellm,app.kubernetes.io/instance=litellm"},
		{"openbao", "platform", "app.kubernetes.io/name=openbao,app.kubernetes.io/instance=openbao"},
		{"forgejo", "forgejo", "app.kubernetes.io/name=forgejo,app.kubernetes.io/instance=forgejo"},
		{"kube-apiserver", "kube-system", "k8s-app=kube-apiserver"},
	}
	if len(WatchedServices) != len(want) {
		t.Fatalf("%d services", len(WatchedServices))
	}
	for i, w := range want {
		if WatchedServices[i] != w {
			t.Errorf("service %d = %+v, want %+v", i, WatchedServices[i], w)
		}
	}
}

func TestSigNozWaitGivesUpWhenItsContextEnds(t *testing.T) {
	var logins atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	s := signozKernelServer(t, &logins, func(w http.ResponseWriter, r *http.Request, _ int32) {
		close(started)
		<-release
		io.WriteString(w, kernelAnswer)
	})
	done := make(chan error, 1)
	go func() {
		_, err := s.KernelLines(context.Background(), time.Now().Add(-time.Minute), time.Now())
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begun := time.Now()
	_, err := s.FiringAlerts(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(begun) > 2*time.Second {
		t.Errorf("FiringAlerts = %v after %v; want its deadline, promptly", err, time.Since(begun))
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("the holder: %v", err)
	}
}

type addedKeys struct {
	*fakeClusterStore
	keys []string
}

func (a *addedKeys) AddPlatformIncidents(ctx context.Context, in []PlatformIncident) (int, error) {
	for _, p := range in {
		a.keys = append(a.keys, p.Key)
	}
	return a.fakeClusterStore.AddPlatformIncidents(ctx, in)
}

func TestIncidentSweepSkipsWhatTheRetentionWouldPrune(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	w, store, _ := sweepFixture(now)
	w.Store = &addedKeys{fakeClusterStore: store}
	if err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, k := range w.Store.(*addedKeys).keys {
		if strings.Contains(k, "ancient") || strings.HasPrefix(k, "notready/sf-3/") {
			t.Errorf("recorded %s, which the retention prunes at once", k)
		}
	}
}
