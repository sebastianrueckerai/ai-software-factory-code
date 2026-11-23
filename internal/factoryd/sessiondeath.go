package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// deathReader reads how a pod ended: the pod, its node, its kills in the
// incident log and the nodes' kernel logs, and the watched services'
// restarts in its life. A nil cluster or kernel skips its reads.
type deathReader struct {
	kube    Kube
	cluster ClusterStore
	kernel  KernelSource
	wait    time.Duration
}

// deathInput names the pod and what factoryd knows of it: its UID, node and
// the node's boot when it started, empty when unknown, and its life.
type deathInput struct {
	ns, pod                        string
	uid, node, boot                string
	started, ended                 time.Time
	stoppedByFactory, afterRestart bool
	lastCommand, lastPush          string
}

const (
	// deathWait bounds the wait for a kill line of a pod that ended with
	// exit code 137: a memory guard's kill reaches SigNoz seconds after it.
	deathWait = 90 * time.Second
	killPoll  = 15 * time.Second
	// deathMargin is the time a read leaves before its context's deadline
	// for the session's end.
	deathMargin   = 30 * time.Second
	eventMessageB = 200
	// restartKill is how close to a restart a kill of its pod names its
	// cause.
	restartKill = time.Minute
)

// deathOf reads the death of a session's pod.
func (r *Runner) deathOf(ctx context.Context, in deathInput) DeathRecord {
	return deathReader{kube: r.kube, cluster: r.cfg.Cluster, kernel: r.cfg.Kernel, wait: r.deathWait}.read(ctx, in)
}

// putDeath stores a session's death record; a failed write is logged, the
// session's end goes on.
func (r *Runner) putDeath(ctx context.Context, taskID, sessionID string, rec DeathRecord) {
	if r.cfg.Cluster == nil {
		return
	}
	if err := r.cfg.Cluster.PutPodDeath(ctx, PodDeath{Kind: "session", TaskID: taskID, SessionID: sessionID, Record: rec}); err != nil {
		log.Printf("task %s: store the death of session %s: %s", taskID, sessionID, r.Scrub.String(err.Error()))
	}
}

func (d deathReader) read(ctx context.Context, in deathInput) DeathRecord {
	f := DeathFacts{StoppedByFactory: in.stoppedByFactory, AfterRestart: in.afterRestart, BootAtStart: in.boot,
		Started: in.started, Ended: in.ended, LastCommand: in.lastCommand}
	pod, err := d.kube.PodDiagnosis(ctx, in.ns, in.pod)
	switch {
	case errors.Is(err, ErrNotFound):
		f.PodGone = true
	case err != nil:
		log.Printf("death of pod %s/%s: read the pod: %v", in.ns, in.pod, err)
	}
	f.Pod = pod
	if in.uid != "" {
		f.Pod.UID = in.uid
	}
	if in.node != "" {
		f.Pod.Node = in.node
	}
	if f.Pod.Node != "" {
		n, err := d.kube.NodeState(ctx, f.Pod.Node)
		switch {
		case err == nil:
			f.Node = &n
		case errors.Is(err, ErrNotFound):
			f.NodeGone = true
		default:
			log.Printf("death of pod %s/%s: read node %s: %v", in.ns, in.pod, f.Pod.Node, err)
		}
	}
	seen := map[string]bool{}
	add := func(key string, k KernelKill) {
		if !seen[key] {
			seen[key] = true
			f.Kills = append(f.Kills, k)
		}
	}
	if d.cluster != nil && f.Pod.UID != "" {
		incidents, err := d.cluster.PodIncidents(ctx, f.Pod.UID)
		if err != nil {
			log.Printf("death of pod %s/%s: read its incidents: %v", in.ns, in.pod, err)
		}
		for _, p := range incidents {
			if k, ok := killOfIncident(p); ok {
				add(p.Key, k)
			}
		}
	}
	// readKernel asks the nodes' kernel logs for the pod's kills; false
	// when there is nothing to ask or the source failed
	readKernel := func() bool {
		if d.kernel == nil || f.Pod.UID == "" {
			return false
		}
		lines, err := d.kernel.KernelLines(ctx, in.started.Add(-time.Minute), time.Now())
		if err != nil {
			log.Printf("death of pod %s/%s: read the kernel lines: %v", in.ns, in.pod, err)
			return false
		}
		for _, k := range lines {
			if k.PodUID == f.Pod.UID {
				add(kmsgKey(k), k)
			}
		}
		return true
	}
	asked := readKernel()
	r := Classify(f)
	killed137 := slices.ContainsFunc(f.Pod.Containers, func(c ContainerState) bool { return c.State == "terminated" && c.ExitCode == 137 })
	if asked && r.Rule == RuleNone && killed137 {
		until := time.Now().Add(d.wait)
		if dl, ok := ctx.Deadline(); ok {
			until = earliest(until, dl.Add(-deathMargin))
		}
	wait:
		for time.Now().Before(until) {
			select {
			case <-ctx.Done():
				break wait
			case <-time.After(min(killPoll, time.Until(until))):
			}
			if !readKernel() {
				break
			}
			if r = Classify(f); r.Rule != RuleNone {
				break
			}
		}
	}
	events, err := d.kube.PodEvents(ctx, in.ns, in.pod)
	if err != nil {
		log.Printf("death of pod %s/%s: read its events: %v", in.ns, in.pod, err)
	}
	for _, e := range events {
		e.Message = textutil.Truncate(e.Message, eventMessageB)
		r.Events = append(r.Events, e)
	}
	r.Restarts = d.restarts(ctx, in)
	r.Lines = GateLines(r, in.lastPush)
	return r
}

// restarts are the watched services' restarts in the pod's life, each with
// the kill of its own pod within a minute of it as its cause.
func (d deathReader) restarts(ctx context.Context, in deathInput) []ServiceRestart {
	if d.cluster == nil {
		return nil
	}
	incidents, err := d.cluster.PlatformIncidents(ctx, in.started, in.ended, 0)
	if err != nil {
		log.Printf("death of pod %s/%s: read the incidents of its life: %v", in.ns, in.pod, err)
		return nil
	}
	var out []ServiceRestart
	for _, p := range incidents {
		if p.Kind != IncidentRestart {
			continue
		}
		s := ServiceRestart{Service: p.Subject, From: p.At}
		if p.Until != nil {
			s.Until = *p.Until
		}
		if p.PodUID != "" {
			own, err := d.cluster.PodIncidents(ctx, p.PodUID)
			if err != nil {
				log.Printf("death of pod %s/%s: read the incidents of %s's pod: %v", in.ns, in.pod, p.Subject, err)
			}
			for _, q := range own {
				if k, ok := killOfIncident(q); ok && k.At.Sub(p.At).Abs() <= restartKill {
					s.Cause = k.Source
					break
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// killOfIncident is the kill an incident of the log records.
func killOfIncident(p PlatformIncident) (KernelKill, bool) {
	k := KernelKill{Node: p.Node, PodUID: p.PodUID, At: p.At}
	switch p.Kind {
	case IncidentGuardKill:
		k.Source = "guard"
	case IncidentKernelOOM:
		k.Source = "kernel"
		k.Constraint, _ = p.Detail["constraint"].(string)
	default:
		return KernelKill{}, false
	}
	return k, true
}

// kmsgKey is a kernel line's key: its node, sequence number and time make
// it unique.
func kmsgKey(k KernelKill) string {
	return fmt.Sprintf("kmsg/%s/%d/%s", k.Node, k.Seq, k.At.UTC().Format(time.RFC3339Nano))
}
