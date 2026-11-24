package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// deathReader reads how a pod ended: the pod, its node, its kills in the
// incident log and the nodes' kernel logs, and the watched services'
// restarts in its life. A nil cluster or kernel skips its reads.
type deathReader struct {
	kube    Kube
	cluster ClusterStore
	kernel  KernelSource
	wait    time.Duration
	// poll is how often the wait asks again; zero is killPoll
	poll time.Duration
	// margin is the time the reads leave before their context's deadline:
	// deathMargin for the rest of a session's or a gate's end
	margin time.Duration
	// scrub scrubs what the reads log
	scrub *redact.Scrubber
}

// deathInput names the pod and what factoryd knows of it: its UID, node and
// the node's boot when it started, empty when unknown, and its life. live
// is a read of a pod that may still run. It and a pod factoryd stopped are
// classed by the end's evidence alone: a node that went NotReady and
// answers again ended nothing. reported is an end the worker reported
// itself, which nothing killed (DeathFacts.Reported).
type deathInput struct {
	ns, pod                                        string
	uid, node, boot                                string
	started, ended                                 time.Time
	stoppedByFactory, afterRestart, live, reported bool
	lastCommand, lastPush                          string
}

const (
	// deathWait bounds the wait for a kill line of a pod that ended with
	// exit code 137: a memory guard's kill reaches SigNoz seconds after it.
	deathWait = 90 * time.Second
	killPoll  = 15 * time.Second
	// deathMargin is the time a read leaves before its context's deadline
	// for the rest of the end it is part of.
	deathMargin = 30 * time.Second
	// restartKill is how close to a restart a kill of its pod names its
	// cause.
	restartKill = time.Minute
	// notReadyWindow is how long before its end a node going NotReady
	// counts as what ended the session.
	notReadyWindow = 15 * time.Minute
)

// deathOf reads the death of a session's pod.
func (r *Runner) deathOf(ctx context.Context, in deathInput) DeathRecord {
	return deathReader{kube: r.kube, cluster: r.cfg.Cluster, kernel: r.cfg.Kernel, wait: r.deathWait, margin: deathMargin, scrub: r.Scrub}.read(ctx, in)
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

// logf logs a line of the reads through the scrubber: a store's or a
// source's error can quote a secret.
func (d deathReader) logf(format string, a ...any) {
	log.Print(d.scrub.String(fmt.Sprintf(format, a...)))
}

func (d deathReader) read(ctx context.Context, in deathInput) DeathRecord {
	// the wait for a kill line ends at waitEnd, and no kernel read outlasts
	// it; the first read may take one poll when the wait is shorter. Both
	// leave the margin before the context's deadline.
	start, poll := time.Now(), cmp.Or(d.poll, killPoll)
	waitEnd, firstEnd := start.Add(d.wait), start.Add(max(d.wait, poll))
	if dl, ok := ctx.Deadline(); ok {
		waitEnd, firstEnd = earliest(waitEnd, dl.Add(-d.margin)), earliest(firstEnd, dl.Add(-d.margin))
	}
	f := DeathFacts{StoppedByFactory: in.stoppedByFactory, AfterRestart: in.afterRestart, Reported: in.reported, BootAtStart: in.boot,
		Started: in.started, Ended: in.ended, LastCommand: in.lastCommand}
	pod, err := d.kube.PodDiagnosis(ctx, in.ns, in.pod)
	switch {
	case errors.Is(err, ErrNotFound):
		f.PodGone = true
	case err != nil:
		d.logf("death of pod %s/%s: read the pod: %v", in.ns, in.pod, err)
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
			d.logf("death of pod %s/%s: read node %s: %v", in.ns, in.pod, f.Pod.Node, err)
		}
	}
	var life []PlatformIncident
	if d.cluster != nil {
		if life, err = d.cluster.PlatformIncidents(ctx, in.started, in.ended, 0); err != nil {
			d.logf("death of pod %s/%s: read the incidents of its life: %v", in.ns, in.pod, err)
		}
	}
	// a flip of the node counts in the session's last minutes only: an
	// early one the session outlived does not outrank its own later end
	flips := in.ended.Add(-notReadyWindow)
	if flips.Before(in.started) {
		flips = in.started
	}
	f.NodeNotReadyDuring = !(in.live || in.stoppedByFactory) && f.Pod.Node != "" && slices.ContainsFunc(life, func(p PlatformIncident) bool {
		return p.Kind == IncidentNodeNotReady && (p.Subject == f.Pod.Node || p.Node == f.Pod.Node) && !p.At.Before(flips) && !p.At.After(in.ended)
	})
	seen := map[string]bool{}
	add := func(key string, k KernelKill) {
		if !seen[key] {
			seen[key] = true
			f.Kills = append(f.Kills, k)
		}
	}
	// readStored takes the pod's kills the incident pass recorded
	readStored := func() {
		if d.cluster == nil || f.Pod.UID == "" {
			return
		}
		incidents, err := d.cluster.PodIncidents(ctx, f.Pod.UID)
		if err != nil {
			d.logf("death of pod %s/%s: read its incidents: %v", in.ns, in.pod, err)
		}
		for _, p := range incidents {
			if k, ok := killOfIncident(p); ok {
				add(p.Key, k)
			}
		}
	}
	// readKernel asks the nodes' kernel logs for the pod's kills, at most
	// until end; false when there is nothing to ask or the source failed
	readKernel := func(end time.Time) bool {
		if d.kernel == nil || f.Pod.UID == "" {
			return false
		}
		kctx, cancel := context.WithDeadline(ctx, end)
		defer cancel()
		lines, err := d.kernel.KernelLines(kctx, in.started.Add(-time.Minute), time.Now(), f.Pod.UID)
		if err != nil {
			d.logf("death of pod %s/%s: read the kernel lines: %v", in.ns, in.pod, err)
			return false
		}
		for _, k := range lines {
			if k.PodUID == f.Pod.UID {
				add(kmsgKey(k), k)
			}
		}
		return true
	}
	readStored()
	asked := readKernel(firstEnd)
	r := Classify(f)
	// after factoryd's own stop a 137 is the kubelet's kill at the grace's end
	killed137 := slices.ContainsFunc(f.Pod.Containers, func(c ContainerState) bool { return c.State == "terminated" && c.ExitCode == 137 })
	if asked && !in.stoppedByFactory && !in.reported && r.Rule == RuleNone && killed137 {
	wait:
		for time.Now().Before(waitEnd) {
			select {
			case <-ctx.Done():
				break wait
			case <-time.After(min(poll, time.Until(waitEnd))):
			}
			readStored()
			// the wait's end has come: no kernel read has time left
			if !time.Now().Before(waitEnd) {
				r = Classify(f)
				break
			}
			ok := readKernel(waitEnd)
			if r = Classify(f); !ok || r.Rule != RuleNone {
				break
			}
		}
	}
	events, err := d.kube.PodEvents(ctx, in.ns, in.pod)
	if err != nil {
		d.logf("death of pod %s/%s: read its events: %v", in.ns, in.pod, err)
	}
	for _, e := range events {
		e.Message = ""
		r.Events = append(r.Events, e)
	}
	r.Started = in.started
	r.Restarts = d.restarts(ctx, in, life)
	r.Lines = GateLines(r, in.lastPush)
	return r
}

// restarts are the watched services' restarts among the incidents of the
// pod's life, each with the kill of its own pod within a minute of it as
// its cause.
func (d deathReader) restarts(ctx context.Context, in deathInput, life []PlatformIncident) []ServiceRestart {
	var out []ServiceRestart
	for _, p := range life {
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
				d.logf("death of pod %s/%s: read the incidents of %s's pod: %v", in.ns, in.pod, p.Subject, err)
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
