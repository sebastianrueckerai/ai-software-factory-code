package factoryd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var healthNow = time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC)

// healthAPI scripts the API server of tonight's case: factoryd's one live pod
// with no restarts, the Started events of two pods already gone, and
// objects the read must leave out.
func healthAPI(t *testing.T, pods string) *InClusterKube {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sel := r.URL.Query().Get("labelSelector"); (strings.HasSuffix(r.URL.Path, "/replicasets") || strings.HasSuffix(r.URL.Path, "/pods")) && sel != "app=factoryd" {
			t.Errorf("%s labelSelector = %q, want app=factoryd", r.URL.Path, sel)
		}
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/factory/deployments/factoryd":
			io.WriteString(w, `{"metadata":{"uid":"u1"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"factoryd"}}},
				"status":{"readyReplicas":1,"updatedReplicas":1,"availableReplicas":1}}`)
		case "/apis/apps/v1/namespaces/factory/replicasets":
			io.WriteString(w, `{"items":[
				{"metadata":{"name":"factoryd-5ffd8987d5","annotations":{"deployment.kubernetes.io/revision":"11"},"ownerReferences":[{"kind":"Deployment","uid":"u1"}]},"spec":{"replicas":0},"status":{}},
				{"metadata":{"name":"factoryd-5cf85f5cb8","annotations":{"deployment.kubernetes.io/revision":"12"},"ownerReferences":[{"kind":"Deployment","uid":"u1"}]},"spec":{"replicas":1},"status":{"readyReplicas":1}},
				{"metadata":{"name":"factoryd-foreign","annotations":{"deployment.kubernetes.io/revision":"3"},"ownerReferences":[{"kind":"Deployment","uid":"other"}]},"spec":{"replicas":1},"status":{}}]}`)
		case "/api/v1/namespaces/factory/pods":
			io.WriteString(w, pods)
		case "/api/v1/namespaces/factory/events":
			if r.URL.Query().Get("limit") == "" {
				t.Errorf("events read without a limit: %s", r.URL)
			}
			if r.URL.Query().Get("continue") == "p2" {
				io.WriteString(w, `{"items":[
				{"involvedObject":{"kind":"Pod","name":"factoryd-5cf85f5cb8-kqs8w"},"reason":"Unhealthy","message":"Readiness probe failed","eventTime":"2026-10-22T20:30:00.000000Z","series":{"count":5,"lastObservedTime":"2026-10-22T21:55:00.000000Z"}}]}`)
				return
			}
			io.WriteString(w, `{"metadata":{"continue":"p2"},"items":[
				{"involvedObject":{"kind":"Pod","name":"factoryd-5ffd8987d5-94hld"},"reason":"Started","message":"Container started","count":1,"lastTimestamp":"2026-10-22T21:30:01Z"},
				{"involvedObject":{"kind":"Pod","name":"factoryd-5ffd8987d5-abcde"},"reason":"Killing","message":"Stopping\n  container factoryd","lastTimestamp":"2026-10-22T21:40:00Z"},
				{"involvedObject":{"kind":"Pod","name":"factoryd-other-7d9f8-xyz12"},"reason":"Started","message":"other","count":1,"lastTimestamp":"2026-10-22T21:50:00Z"},
				{"involvedObject":{"kind":"Deployment","name":"factoryd-other"},"reason":"ScalingReplicaSet","message":"other","count":1,"lastTimestamp":"2026-10-22T21:50:00Z"},
				{"involvedObject":{"kind":"Pod","name":"factoryd-other-0"},"reason":"Started","message":"a StatefulSet's pod","count":1,"lastTimestamp":"2026-10-22T21:51:00Z"},
				{"involvedObject":{"kind":"ReplicaSet","name":"factoryd-foreign"},"reason":"SuccessfulCreate","message":"another owner's","count":1,"lastTimestamp":"2026-10-22T21:51:00Z"},
				{"involvedObject":{"kind":"Pod","name":"factoryd-5ffd8987d5-old00"},"reason":"Started","message":"too old","count":1,"lastTimestamp":"2026-10-22T20:59:59Z"},
				{"involvedObject":{"kind":"ReplicaSet","name":"factoryd-5cf85f5cb8"},"reason":"SuccessfulCreate","message":"Created pod: factoryd-5cf85f5cb8-kqs8w","count":1,"eventTime":"2026-10-22T21:48:04.000000Z"},
				{"involvedObject":{"kind":"Deployment","name":"factoryd"},"reason":"ScalingReplicaSet","message":"Scaled up replica set factoryd-5cf85f5cb8 to 1","count":2,"firstTimestamp":"2026-10-22T21:48:03Z"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := SaDirForTest(dir)
	t.Cleanup(func() { SaDirForTest(old) })
	return &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
}

const livePod = `{"items":[
	{"metadata":{"name":"factoryd-5cf85f5cb8-kqs8w","ownerReferences":[{"kind":"ReplicaSet","name":"factoryd-5cf85f5cb8"}]},
	 "status":{"phase":"Running","startTime":"2026-10-22T21:48:05Z","conditions":[{"type":"Ready","status":"True"}],
	  "containerStatuses":[{"name":"factoryd","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-10-22T21:48:05Z"}},"lastState":{}}]}},
	{"metadata":{"name":"factoryd-other-7d9f8-xyz12","ownerReferences":[{"kind":"ReplicaSet","name":"factoryd-other-7d9f8"}]},"status":{"phase":"Running"}}]}`

func TestDeploymentHealthReadsTonightsCase(t *testing.T) {
	h, err := healthAPI(t, livePod).DeploymentHealth(context.Background(), "factory", "factoryd", healthNow)
	if err != nil {
		t.Fatal(err)
	}
	want := `Deployment factoryd in namespace factory: 1 of 1 ready (1 updated, 1 available)
ReplicaSets: factoryd-5cf85f5cb8 (revision 12, 1 of 1 ready), 1 older at 0 replicas
Pods:
- factoryd-5cf85f5cb8-kqs8w (ReplicaSet factoryd-5cf85f5cb8): Running, ready, started 2026-10-22T21:48:05Z
  container factoryd: 0 restarts, running since 2026-10-22T21:48:05Z, no earlier termination
Events of the last hour, oldest first (Kubernetes' own counts):
- 2026-10-22T21:30:01Z Pod factoryd-5ffd8987d5-94hld Started x1: Container started
- 2026-10-22T21:40:00Z Pod factoryd-5ffd8987d5-abcde Killing x1: Stopping container factoryd
- 2026-10-22T21:48:03Z Deployment factoryd ScalingReplicaSet x2: Scaled up replica set factoryd-5cf85f5cb8 to 1
- 2026-10-22T21:48:04Z ReplicaSet factoryd-5cf85f5cb8 SuccessfulCreate x1: Created pod: factoryd-5cf85f5cb8-kqs8w
- 2026-10-22T21:55:00Z Pod factoryd-5cf85f5cb8-kqs8w Unhealthy x5: Readiness probe failed`
	if got := h.Text(); got != want {
		t.Fatalf("Text\n%s\nwant\n%s", got, want)
	}
}

// A Deployment that selects its pods by expressions, not labels, reads by
// its ReplicaSets' and pods' owners alone.
func TestDeploymentHealthReadsADeploymentSelectingByExpressions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("labelSelector") {
			t.Errorf("%s read with a label selector", r.URL)
		}
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/factory/deployments/web":
			io.WriteString(w, `{"metadata":{"uid":"u9"},"spec":{"selector":{"matchExpressions":[{"key":"app","operator":"In","values":["web"]}]}},"status":{"readyReplicas":1}}`)
		case "/apis/apps/v1/namespaces/factory/replicasets":
			io.WriteString(w, `{"items":[{"metadata":{"name":"web-6d4b7","ownerReferences":[{"kind":"Deployment","uid":"u9"}]},"spec":{"replicas":1},"status":{"readyReplicas":1}},
				{"metadata":{"name":"api-7c9f2","ownerReferences":[{"kind":"Deployment","uid":"u8"}]},"spec":{"replicas":1}}]}`)
		case "/api/v1/namespaces/factory/pods":
			io.WriteString(w, `{"items":[{"metadata":{"name":"web-6d4b7-q2x9z","ownerReferences":[{"kind":"ReplicaSet","name":"web-6d4b7"}]},"status":{"phase":"Running"}},
				{"metadata":{"name":"api-7c9f2-a1b2c","ownerReferences":[{"kind":"ReplicaSet","name":"api-7c9f2"}]},"status":{"phase":"Running"}}]}`)
		case "/api/v1/namespaces/factory/events":
			io.WriteString(w, `{"items":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := SaDirForTest(dir)
	t.Cleanup(func() { SaDirForTest(old) })
	h, err := (&InClusterKube{Host: srv.URL, HTTP: srv.Client()}).DeploymentHealth(context.Background(), "factory", "web", healthNow)
	if err != nil {
		t.Fatal(err)
	}
	if h.Desired != 1 || len(h.ReplicaSets) != 1 || h.ReplicaSets[0].Name != "web-6d4b7" || len(h.Pods) != 1 || h.Pods[0].Name != "web-6d4b7-q2x9z" {
		t.Fatalf("read %+v, want web's one ReplicaSet and pod", h)
	}
}

func TestDeploymentHealthNamesACrashedContainersLastTermination(t *testing.T) {
	pods := `{"items":[{"metadata":{"name":"factoryd-5cf85f5cb8-kqs8w","ownerReferences":[{"kind":"ReplicaSet","name":"factoryd-5cf85f5cb8"}]},
		"status":{"phase":"Running","startTime":"2026-10-22T21:48:05Z","conditions":[{"type":"Ready","status":"False"}],
		"containerStatuses":[{"name":"factoryd","restartCount":3,"state":{"waiting":{"reason":"CrashLoopBackOff"}},
		 "lastState":{"terminated":{"reason":"Error","exitCode":1,"finishedAt":"2026-10-18T13:47:20Z"}}}]}}]}`
	h, err := healthAPI(t, pods).DeploymentHealth(context.Background(), "factory", "factoryd", healthNow)
	if err != nil {
		t.Fatal(err)
	}
	c := h.Pods[0].Containers[0]
	if c.Restarts != 3 || c.State != "waiting" || c.Reason != "CrashLoopBackOff" || c.Last == nil || c.Last.ExitCode != 1 {
		t.Fatalf("container = %+v, last %+v", c, c.Last)
	}
	line := "  container factoryd: 3 restarts, waiting: CrashLoopBackOff, last terminated 2026-10-18T13:47:20Z: Error, exit code 1"
	if !strings.Contains(h.Text(), "Running, not ready, started 2026-10-22T21:48:05Z\n"+line+"\n") {
		t.Fatalf("Text lacks %q:\n%s", line, h.Text())
	}
}

func TestDeploymentHealthMapsTheAPIsAnswers(t *testing.T) {
	k := healthAPI(t, livePod)
	if _, err := k.DeploymentHealth(context.Background(), "factory", "nope", healthNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	if _, err := (&InClusterKube{Host: srv.URL}).DeploymentHealth(context.Background(), "factory", "factoryd", healthNow); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestDeploymentHealthTextIsCapped(t *testing.T) {
	h := DeploymentHealth{Namespace: "factory", Name: "factoryd", Desired: 300}
	for i := range 300 {
		h.Pods = append(h.Pods, HealthPod{Name: fmt.Sprintf("factoryd-5cf85f5cb8-p%03d", i), ReplicaSet: "factoryd-5cf85f5cb8", Phase: "Running"})
	}
	got := h.Text()
	i := strings.LastIndex(got, "\n")
	kept, tail := got[:i], got[i+1:]
	if len(kept) > healthTextMax || !strings.HasPrefix(tail, "[") || !strings.HasSuffix(tail, " bytes cut]") {
		t.Fatalf("kept %d bytes, tail %q", len(kept), tail)
	}
	if !strings.HasSuffix(kept, ": Running, not ready") {
		t.Fatalf("cut inside a line: ...%q", kept[len(kept)-40:])
	}
}

type fakeHealth struct {
	byNS  map[string]error
	calls []string
}

func (f *fakeHealth) DeploymentHealth(_ context.Context, ns, name string, _ time.Time) (DeploymentHealth, error) {
	f.calls = append(f.calls, ns+"/"+name)
	if err := f.byNS[ns]; err != nil {
		return DeploymentHealth{}, err
	}
	return DeploymentHealth{Namespace: ns, Name: name, Desired: 1, Ready: 1, Updated: 1, Available: 1}, nil
}

func TestDeploymentHealthTextScopesToTheProject(t *testing.T) {
	p := Project{Name: "infra", Namespaces: []string{"platform", "forgejo"}}
	const reads = "Deployment forgejo in namespace %s: 1 of 1 ready (1 updated, 1 available)\nReplicaSets: none\nPods: none\nEvents of the last hour: none"
	for _, c := range []struct {
		name, deployment, namespace string
		byNS                        map[string]error
		text, err                   string
		status                      int
		calls                       []string
	}{
		{name: "another project's namespace", deployment: "forgejo", namespace: "factory", status: 403,
			err: "namespace factory is not one of project infra's namespaces (platform, forgejo): name one of them or leave namespace out"},
		{name: "the second namespace", deployment: "forgejo", byNS: map[string]error{"platform": ErrNotFound},
			text: fmt.Sprintf(reads, "forgejo"), calls: []string{"platform/forgejo", "forgejo/forgejo"}},
		{name: "a given namespace", deployment: "forgejo", namespace: "forgejo", text: fmt.Sprintf(reads, "forgejo"), calls: []string{"forgejo/forgejo"}},
		{name: "found nowhere", deployment: "forgejo", byNS: map[string]error{"platform": ErrNotFound, "forgejo": ErrNotFound}, status: 404,
			err: "no Deployment forgejo in namespace platform or forgejo of project infra: check the name", calls: []string{"platform/forgejo", "forgejo/forgejo"}},
		{name: "forbidden", deployment: "forgejo", namespace: "platform", byNS: map[string]error{"platform": fmt.Errorf("%w: GET", ErrForbidden)}, status: 403,
			err: "factoryd may not read pods and events in namespace platform: bind ClusterRole factoryd-health to ServiceAccount factory/factoryd there", calls: []string{"platform/forgejo"}},
		{name: "forbidden in the first, found in the second", deployment: "forgejo", byNS: map[string]error{"platform": fmt.Errorf("%w: GET", ErrForbidden)},
			text: fmt.Sprintf(reads, "forgejo"), calls: []string{"platform/forgejo", "forgejo/forgejo"}},
		{name: "forbidden in the first, found nowhere", deployment: "forgejo", byNS: map[string]error{"platform": fmt.Errorf("%w: GET", ErrForbidden), "forgejo": ErrNotFound}, status: 403,
			err: "factoryd may not read pods and events in namespace platform: bind ClusterRole factoryd-health to ServiceAccount factory/factoryd there", calls: []string{"platform/forgejo", "forgejo/forgejo"}},
		{name: "a bad deployment", deployment: "Forgejo/../x", status: 400,
			err: "deployment must be a Deployment's name (lower-case letters, digits, '-' and '.', at most 253 characters): give the name as the cluster lists it"},
		{name: "a bad namespace", deployment: "forgejo", namespace: "a/b", status: 400,
			err: "namespace must be a namespace's name (lower-case letters, digits and '-', at most 63 characters): give one or leave it out"},
		{name: "no deployment", status: 400,
			err: "deployment must be a Deployment's name (lower-case letters, digits, '-' and '.', at most 253 characters): give the name as the cluster lists it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeHealth{byNS: c.byNS}
			text, err := DeploymentHealthText(context.Background(), f, p, c.deployment, c.namespace, healthNow)
			if c.err == "" {
				if err != nil || text != c.text {
					t.Fatalf("text %q, err %v; want %q", text, err, c.text)
				}
			} else {
				var no *HealthRefusal
				if !errors.As(err, &no) || no.Status != c.status || no.Msg != c.err {
					t.Fatalf("err = %v, want %d %q", err, c.status, c.err)
				}
			}
			if strings.Join(f.calls, " ") != strings.Join(c.calls, " ") {
				t.Fatalf("reads %v, want %v", f.calls, c.calls)
			}
		})
	}
}

type healthStore struct{ tasks map[string]Task }

func (s healthStore) Task(_ context.Context, id string) (Task, bool, error) {
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s healthStore) ProjectByRepository(_ context.Context, repo string) (Project, bool, error) {
	switch repo {
	case "sf/infra":
		return Project{Name: "infra", Namespaces: []string{"platform"}, File: true, Objects: policy.ObjectsSeed}, true, nil
	case "sf/old":
		return Project{Name: "old", Namespaces: []string{"old"}}, true, nil
	}
	return Project{}, false, nil
}

func TestDeploymentHealthEndpoint(t *testing.T) {
	store := healthStore{tasks: map[string]Task{
		"t1":   {ID: "t1", Status: "running", Repository: "sf/infra"},
		"done": {ID: "done", Status: "done", Repository: "sf/infra"},
		"orph": {ID: "orph", Status: "running", Repository: "sf/none"},
		"old1": {ID: "old1", Status: "running", Repository: "sf/old"},
	}}
	f := &fakeHealth{byNS: map[string]error{}}
	h := DeploymentHealthHandler(store, f, func() time.Time { return healthNow })
	for _, c := range []struct {
		name, body string
		code       int
		out        string
	}{
		{"success", `{"task_id":"t1","deployment":"forgejo"}`, 200,
			`{"text":"Deployment forgejo in namespace platform: 1 of 1 ready (1 updated, 1 available)\nReplicaSets: none\nPods: none\nEvents of the last hour: none"}` + "\n"},
		{"another namespace", `{"task_id":"t1","deployment":"forgejo","namespace":"factory"}`, 403,
			"namespace factory is not one of project infra's namespaces (platform): name one of them or leave namespace out\n"},
		{"bad name", `{"task_id":"t1","deployment":"X"}`, 400,
			"deployment must be a Deployment's name (lower-case letters, digits, '-' and '.', at most 253 characters): give the name as the cluster lists it\n"},
		{"unknown task", `{"task_id":"zz","deployment":"forgejo"}`, 404, "no task zz\n"},
		{"final task", `{"task_id":"done","deployment":"forgejo"}`, 409, "task done is done\n"},
		{"no project", `{"task_id":"orph","deployment":"forgejo"}`, 409, "no project owns the repository sf/none of task orph\n"},
		{"no task id", `{"deployment":"forgejo"}`, 400, "task_id is required\n"},
		{"inactive project", `{"task_id":"old1","deployment":"web"}`, 409, "project old of task old1 is not active: its Deployments are not read\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h(rr, httptest.NewRequest(http.MethodPost, "/internal/deployment-health", strings.NewReader(c.body)))
			if rr.Code != c.code || rr.Body.String() != c.out {
				t.Fatalf("%d %q, want %d %q", rr.Code, rr.Body, c.code, c.out)
			}
		})
	}
	f.byNS["platform"] = ErrNotFound
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"task_id":"t1","deployment":"forgejo"}`)))
	if rr.Code != 404 || rr.Body.String() != "no Deployment forgejo in namespace platform of project infra: check the name\n" {
		t.Fatalf("%d %q", rr.Code, rr.Body)
	}
	f.byNS["platform"] = errors.New("boom")
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"task_id":"t1","deployment":"forgejo"}`)))
	if rr.Code != 502 {
		t.Fatalf("%d %q, want 502", rr.Code, rr.Body)
	}
}
