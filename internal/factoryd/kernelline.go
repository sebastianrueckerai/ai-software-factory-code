package factoryd

import (
	"regexp"
	"strings"
	"time"
)

// KernelKill is one kill of a pod's processes read from a node's kernel
// log: the Talos memory guard's SIGKILL ("guard") or the kernel's own OOM
// kill ("kernel"). Constraint is set for kernel kills only.
type KernelKill struct {
	Source     string
	Node       string
	PodUID     string
	Constraint string
	At         time.Time
	Seq        int64
}

var (
	podUIDPattern     = regexp.MustCompile(`pod([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{32})`)
	guardCgroup       = regexp.MustCompile(`"cgroup":\s*"([^"]*)"`)
	killConstraint    = regexp.MustCompile(`constraint=([A-Z_]+)`)
	killTaskMemcgPath = regexp.MustCompile(`task_memcg=([^,\s]*)`)
)

// ParseKernelLine reads one kernel-log body; false for a line that is not
// a kill.
func ParseKernelLine(body, node string, seq int64, at time.Time) (KernelKill, bool) {
	k := KernelKill{Node: node, Seq: seq, At: at}
	switch {
	case strings.Contains(body, "Sending SIGKILL to cgroup"):
		k.Source = "guard"
		if m := guardCgroup.FindStringSubmatch(body); m != nil {
			k.PodUID = podUID(m[1])
		}
	case strings.Contains(body, "oom-kill:"):
		k.Source = "kernel"
		if m := killConstraint.FindStringSubmatch(body); m != nil {
			k.Constraint = m[1]
		}
		if m := killTaskMemcgPath.FindStringSubmatch(body); m != nil {
			k.PodUID = podUID(m[1])
		}
	default:
		return KernelKill{}, false
	}
	return k, true
}

func podUID(path string) string {
	if m := podUIDPattern.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}
