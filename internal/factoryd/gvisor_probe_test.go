package factoryd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The captures of three ends provoked on the live cluster under gVisor, in
// namespace cf-probe on node sf-2: busybox with a 512Mi memory limit and a
// 256Mi /tmp emptyDir.
func gvisorProbe(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "gvisor", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gvisorKernel is sf-2's kernel lines as SigNoz holds them, read through
// ParseKernelLine; a call naming a pod gets the lines that name it.
type gvisorKernel struct{ t *testing.T }

func (k gvisorKernel) KernelLines(_ context.Context, from, to time.Time, podUID string) ([]KernelKill, error) {
	var out []KernelKill
	sc := bufio.NewScanner(bytes.NewReader(gvisorProbe(k.t, "kernel-lines.jsonl")))
	for seq := int64(1); sc.Scan(); seq++ {
		var row struct{ T, Body string }
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, err
		}
		at, err := time.Parse("2006-01-02 15:04:05.999999999", row.T)
		if err != nil {
			return nil, err
		}
		if kill, ok := ParseKernelLine(row.Body, "sf-2", seq, at); ok && strings.Contains(row.Body, podUID) && !at.Before(from) && !at.After(to) {
			out = append(out, kill)
		}
	}
	return out, sc.Err()
}

// gvisorAPI serves the captured pods, the namespace's captured events by
// the pod they name, and node sf-2 Ready; a pod in gone answers 404.
func gvisorAPI(t *testing.T, gone string) *InClusterKube {
	var events struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(gvisorProbe(t, "events.json"), &events); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/cf-probe/pods/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/cf-probe/pods/")
			if name == gone {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(gvisorProbe(t, name+".json"))
		case r.URL.Path == "/api/v1/namespaces/cf-probe/events":
			pod := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "involvedObject.kind=Pod,involvedObject.name=")
			var named []map[string]any
			for _, e := range events.Items {
				if e["involvedObject"].(map[string]any)["name"] == pod {
					named = append(named, e)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": named})
		case r.URL.Path == "/api/v1/nodes":
			w.Write([]byte(`{"items":[{"metadata":{"name":"sf-2"},"status":{"nodeInfo":{"bootID":"b-sf-2"},` +
				`"conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-11-20T09:00:00Z"}]}}]}`))
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return kubeAt(t, srv)
}

// The three live ends read as the death reader reads them: gVisor ends a
// container its memory limit stopped OOMKilled with exit code 128, and the
// kernel's memcg line names its pod, so the session reached its own limit.
// The eviction of the emptyDir is the session's own storage. A pod deleted
// from outside, gone at the read, its events naming it, is the platform's.
func TestTheLiveGVisorEndsClassify(t *testing.T) {
	started, ended := time.Date(2026, 11, 24, 1, 35, 11, 0, time.UTC), time.Date(2026, 11, 24, 1, 35, 30, 0, time.UTC)
	for name, tc := range map[string]struct {
		pod, uid, gone string
		want           DeathRecord
		events         []string
	}{
		"memory": {pod: "probe-memory", want: DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, KillSource: "kernel", Limit: "512Mi",
			Container: "worker", Reason: "OOMKilled", ExitCode: 128, PodUID: "9bc456d0-ef25-4148-98c5-c1285aa258f2"}},
		"tmp": {pod: "probe-tmp", want: DeathRecord{Class: DeathOwnLimit, Rule: RuleStorageLimit, Resource: "tmp", Limit: "256Mi",
			Container: "worker", Reason: "ContainerStatusUnknown", ExitCode: 137, PodUID: "05b5903d-92fc-477b-a3ef-b4681fd8341a"}},
		"deleted": {pod: "probe-deleted", uid: "d0a5f95d-830e-4fb8-a8ab-bb24c309fd72", gone: "probe-deleted",
			want:   DeathRecord{Class: DeathPlatform, Rule: RuleDeleted, PodUID: "d0a5f95d-830e-4fb8-a8ab-bb24c309fd72"},
			events: []string{"Scheduled", "Pulling", "Pulled", "Created", "Started", "Killing"}},
	} {
		t.Run(name, func(t *testing.T) {
			in := deathInput{ns: "cf-probe", pod: tc.pod, uid: tc.uid, node: "sf-2", started: started, ended: ended}
			r := deathReader{kube: gvisorAPI(t, tc.gone), kernel: gvisorKernel{t}}.read(context.Background(), in)
			got := DeathRecord{Class: r.Class, Rule: r.Rule, KillSource: r.KillSource, Resource: r.Resource, Limit: r.Limit,
				Container: r.Container, Reason: r.Reason, ExitCode: r.ExitCode, PodUID: r.PodUID}
			if !reflect.DeepEqual(got, tc.want) || r.Node != "sf-2" {
				t.Fatalf("record = %+v on %q, want %+v on sf-2", got, r.Node, tc.want)
			}
			if tc.events != nil {
				var reasons []string
				for _, e := range r.Events {
					if e.Message != "" {
						t.Fatalf("event %+v keeps its message", e)
					}
					reasons = append(reasons, e.Reason)
				}
				if strings.Join(reasons, ",") != strings.Join(tc.events, ",") {
					t.Fatalf("events = %v, want %v", reasons, tc.events)
				}
			}
		})
	}
}

// gVisor reports 128 for a container whose sandbox it lost, as the live
// memory kill shows, where runc reports a kill 137. A pod that ended 128
// with no cause yet waits for a kill line as one that ended 137 does, so a
// memory guard's line that reaches SigNoz late still names the kill.
// containerd also ends a container it could not start 128, with reason
// StartError, in any runtime: nothing killed it, so it reads the kernel
// once and waits for no line.
func TestASandboxKilledUnderGVisorWaitsForItsKillLine(t *testing.T) {
	for name, tc := range map[string]struct {
		reason string
		class  DeathClass
		rule   DeathRule
		reads  int
	}{
		"a lost sandbox":    {reason: "Error", class: DeathPlatform, rule: RuleGuardKill, reads: 2},
		"a container start": {reason: "StartError", class: DeathUnknown, rule: RuleNone, reads: 1},
	} {
		t.Run(name, func(t *testing.T) {
			kube, in := deadPod()
			kube.pods["ns/worker-s1"].containers = []ContainerState{{Name: "worker", State: "terminated", Reason: tc.reason, ExitCode: 128}}
			kernel := &scriptedKernel{kills: func(call int) []KernelKill {
				if call < 2 {
					return nil
				}
				return []KernelKill{{Source: "guard", Node: "sf-1", PodUID: deathUID, At: in.ended.Add(-time.Second), Seq: 9}}
			}}
			r := deathReader{kube: kube, kernel: kernel, wait: time.Hour, poll: 100 * time.Millisecond, margin: deathMargin}.read(context.Background(), in)
			if r.Class != tc.class || r.Rule != tc.rule || r.ExitCode != 128 || kernel.called() != tc.reads {
				t.Fatalf("%s %s, exit %d after %d kernel reads; want %s %s after %d", r.Class, r.Rule, r.ExitCode, kernel.called(), tc.class, tc.rule, tc.reads)
			}
		})
	}
}
