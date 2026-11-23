package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	kube.servicePods = map[string][]ServicePod{"factory/app=factoryd": {{
		Namespace: "factory", Name: "factoryd-1", UID: "uid-f", Node: "sf-1",
		Containers: []ServiceContainer{
			{Name: "factoryd", LastReason: "OOMKilled", LastExitCode: 137, LastFinished: now.Add(-time.Hour), RunningSince: now.Add(-59 * time.Minute)},
			{Name: "sidecar"},
		}}}}
	kube.nodeStates = map[string]NodeState{
		"sf-1": {Name: "sf-1", BootID: "b1", Ready: true},
		"sf-2": {Name: "sf-2", BootID: "b2", Ready: false, Since: now.Add(-10 * time.Minute)},
	}
	kernel := &fakeKernel{kills: []KernelKill{{Source: "guard", Node: "sf-2", PodUID: "uid-k", Seq: 711, At: now.Add(-2 * time.Minute)}}}
	store := newFakeClusterStore()
	return &IncidentSweep{Store: store, Kube: kube, Kernel: kernel, Now: func() time.Time { return now }}, store, kernel
}

func TestIncidentSweepRecordsEachSourceOnce(t *testing.T) {
	now := time.Date(2026, 11, 23, 14, 5, 0, 0, time.UTC)
	w, store, kernel := sweepFixture(now)
	for range 2 {
		if err := w.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !kernel.to.Equal(now) || !kernel.from.Equal(now.Add(-KernelWindow)) {
		t.Errorf("kernel window %v..%v", kernel.from, kernel.to)
	}
	rows, _ := store.PlatformIncidents(context.Background(), now.Add(-2*time.Hour), now.Add(time.Hour), 0)
	byKey := map[string]PlatformIncident{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if len(rows) != 5 {
		t.Fatalf("%d rows, want 5: %+v", len(rows), rows)
	}
	kill := byKey["kmsg/sf-2/711/"+now.Add(-2*time.Minute).Format(time.RFC3339Nano)]
	if kill.Kind != IncidentGuardKill || kill.Subject != "uid-k" || kill.PodUID != "uid-k" || kill.Node != "sf-2" {
		t.Errorf("kill row %+v", kill)
	}
	fin := now.Add(-time.Hour)
	restart := byKey["restart/uid-f/factoryd/"+fin.Format(time.RFC3339)]
	if restart.Kind != IncidentRestart || restart.Subject != "factoryd" || !restart.At.Equal(fin) || restart.Until == nil || !restart.Until.Equal(now.Add(-59*time.Minute)) ||
		restart.Detail["container"] != "factoryd" || restart.Detail["reason"] != "OOMKilled" || restart.Detail["exit_code"] != int32(137) {
		t.Errorf("restart row %+v", restart)
	}
	if boot := byKey["boot/sf-1/b1"]; boot.Kind != IncidentNodeBoot || !boot.At.Equal(now) {
		t.Errorf("boot row %+v", boot)
	}
	if nr := byKey["notready/sf-2/"+now.Add(-10*time.Minute).Format(time.RFC3339)]; nr.Kind != IncidentNodeNotReady || !nr.At.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("not ready row %+v", nr)
	}
	if _, ok := byKey["boot/sf-2/b2"]; !ok {
		t.Error("no boot row for sf-2")
	}
}

func TestIncidentSweepKeepsGoingWhenOneSourceFails(t *testing.T) {
	now := time.Date(2026, 11, 23, 14, 5, 0, 0, time.UTC)
	w, store, kernel := sweepFixture(now)
	kernel.err = errors.New("signoz is down")
	err := w.Run(context.Background())
	if err == nil || !errors.Is(err, kernel.err) {
		t.Fatalf("Run = %v, want the kernel error", err)
	}
	rows, _ := store.PlatformIncidents(context.Background(), now.Add(-2*time.Hour), now.Add(time.Hour), 0)
	if len(rows) != 4 {
		t.Errorf("%d rows, want the restart and the three node rows: %+v", len(rows), rows)
	}
}

func TestWatchedServicesSelectors(t *testing.T) {
	want := []WatchedService{
		{"factoryd", "factory", "app=factoryd"},
		{"factory-gateway", "factory", "app=factory-gateway"},
		{"litellm", "platform", "app.kubernetes.io/name=litellm-helm,app.kubernetes.io/instance=litellm"},
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
