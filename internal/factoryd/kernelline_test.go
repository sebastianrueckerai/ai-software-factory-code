package factoryd

import (
	"testing"
	"time"
)

func TestParseKernelLineReadsTheGuardKill(t *testing.T) {
	at := time.Date(2026, 11, 23, 14, 2, 0, 0, time.UTC)
	for _, c := range []struct{ cgroup, uid string }{
		{"/sys/fs/cgroup/kubepods/burstable/pod91adbc7d-6406-4a24-ac91-c1979b028d36", "91adbc7d-6406-4a24-ac91-c1979b028d36"},
		{"/sys/fs/cgroup/kubepods/burstable/pod95b8e1d73a17b344fb5a65306746a377", "95b8e1d73a17b344fb5a65306746a377"},
	} {
		body := `[talos] Sending SIGKILL to cgroup {"component": "controller-runtime", "controller": "runtime.OOMController", "cgroup": "` + c.cgroup + `"}`
		k, ok := ParseKernelLine(body, "sf-2", 7, at)
		if !ok || k.Source != "guard" || k.PodUID != c.uid || k.Node != "sf-2" || k.Seq != 7 || !k.At.Equal(at) {
			t.Errorf("ParseKernelLine(%s) = %+v, %v; want guard kill of %s", c.cgroup, k, ok, c.uid)
		}
	}
}

func TestParseKernelLineReadsTheKernelKill(t *testing.T) {
	const uid = "0f1e2d3c-4b5a-6978-8091-a2b3c4d5e6f7"
	memcg := "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/kubepods/burstable/pod" + uid + ",task_memcg=/kubepods/burstable/pod" + uid + "/c1,task=runsc-sandbox,pid=4242,uid=0"
	k, ok := ParseKernelLine(memcg, "sf-2", 1, time.Time{})
	if !ok || k.Source != "kernel" || k.Constraint != "CONSTRAINT_MEMCG" || k.PodUID != uid {
		t.Errorf("memcg kill = %+v, %v", k, ok)
	}
	none := "oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/kubepods/burstable/pod" + uid + "/c1,task=runsc-sandbox,pid=4242,uid=0"
	k, ok = ParseKernelLine(none, "sf-2", 2, time.Time{})
	if !ok || k.Constraint != "CONSTRAINT_NONE" || k.PodUID != uid {
		t.Errorf("global kill = %+v, %v", k, ok)
	}
	if _, ok := ParseKernelLine("eth0: renamed from tmpff297", "sf-2", 3, time.Time{}); ok {
		t.Error("an unrelated line parsed as a kill")
	}
}
