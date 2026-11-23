package factoryd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// KernelFilter selects the kill lines of the nodes' kernel logs in SigNoz.
const KernelFilter = "resource.service.name = 'talos-kmsg' AND (body CONTAINS 'Sending SIGKILL to cgroup' OR body CONTAINS 'oom-kill:')"

// KernelWindow is how far back each pass reads the kernel logs: wide enough
// that a stall on the way to SigNoz loses no line, safe because every kill
// has a key.
const KernelWindow = 15 * time.Minute

type KernelSource interface {
	KernelLines(ctx context.Context, from, to time.Time) ([]KernelKill, error)
}

var _ KernelSource = (*SigNoz)(nil)

// WatchedService is a service whose containers' restarts the platform
// records.
type WatchedService struct{ Name, Namespace, Selector string }

var WatchedServices = []WatchedService{
	{ServiceFactoryd, "factory", "app=factoryd"},
	{ServiceGateway, "factory", "app=factory-gateway"},
	{ServiceLiteLLM, "platform", "app.kubernetes.io/name=litellm,app.kubernetes.io/instance=litellm"},
	{ServiceOpenBao, "platform", "app.kubernetes.io/name=openbao,app.kubernetes.io/instance=openbao"},
	{ServiceForgejo, "forgejo", "app.kubernetes.io/name=forgejo,app.kubernetes.io/instance=forgejo"},
	{ServiceAPIServer, "kube-system", "k8s-app=kube-apiserver"},
}

// IncidentSweep records what the platform did: the kernel's kills, the
// watched services' restarts and the nodes' boots and failures.
type IncidentSweep struct {
	Store  ClusterStore
	Kube   Kube
	Kernel KernelSource
	Now    func() time.Time
}

func (w *IncidentSweep) Run(ctx context.Context) error {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	var errs []error
	record := func(source string, rows []PlatformIncident, err error) {
		if err == nil {
			// a row already past the retention would be pruned and written again every pass
			rows = slices.DeleteFunc(rows, func(p PlatformIncident) bool {
				end := p.At
				if p.Until != nil {
					end = *p.Until
				}
				return end.Before(now.Add(-IncidentRetention))
			})
			_, err = w.Store.AddPlatformIncidents(ctx, rows)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source, err))
		}
	}
	if w.Kernel != nil {
		kills, err := w.Kernel.KernelLines(ctx, now.Add(-KernelWindow), now)
		var rows []PlatformIncident
		for _, k := range kills {
			p := PlatformIncident{Key: kmsgKey(k), At: k.At, Kind: IncidentGuardKill, Subject: k.PodUID, Node: k.Node, PodUID: k.PodUID}
			if k.Source == "kernel" {
				p.Kind, p.Detail = IncidentKernelOOM, map[string]any{"constraint": k.Constraint}
			}
			rows = append(rows, p)
		}
		record("kernel log", rows, err)
	}
	for _, svc := range WatchedServices {
		pods, err := w.Kube.ServicePods(ctx, svc.Namespace, svc.Selector)
		var rows []PlatformIncident
		for _, pod := range pods {
			if pod.Phase != "Running" || pod.Owner == "Job" {
				continue
			}
			for _, c := range pod.Containers {
				if c.LastFinished.IsZero() {
					continue
				}
				p := PlatformIncident{Key: fmt.Sprintf("restart/%s/%s/%s", pod.UID, c.Name, c.LastFinished.UTC().Format(time.RFC3339)),
					At: c.LastFinished, Kind: IncidentRestart, Subject: svc.Name, Node: pod.Node, Namespace: pod.Namespace, PodUID: pod.UID,
					Detail: map[string]any{"container": c.Name, "reason": c.LastReason, "exit_code": c.LastExitCode}}
				if !c.RunningSince.IsZero() {
					until := c.RunningSince
					p.Until = &until
				}
				rows = append(rows, p)
			}
		}
		record("service "+svc.Name, rows, err)
	}
	nodes, err := w.Kube.Nodes(ctx)
	var rows []PlatformIncident
	for _, n := range nodes {
		rows = append(rows, PlatformIncident{Key: "boot/" + n.Name + "/" + n.BootID, At: now, Kind: IncidentNodeBoot, Subject: n.Name, Node: n.Name})
		if !n.Ready {
			rows = append(rows, PlatformIncident{Key: "notready/" + n.Name + "/" + n.Since.UTC().Format(time.RFC3339), At: n.Since, Kind: IncidentNodeNotReady, Subject: n.Name, Node: n.Name})
		}
	}
	record("nodes", rows, err)
	if err := w.Store.PrunePlatformIncidents(ctx, IncidentRetention); err != nil {
		errs = append(errs, fmt.Errorf("prune: %w", err))
	}
	return errors.Join(errs...)
}
