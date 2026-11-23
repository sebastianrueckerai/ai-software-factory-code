package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	clusterFactsMax   = 12 << 10
	incidentWindowMax = 24 * time.Hour
	incidentRowMax    = 200
)

// FactsRefusal is a cluster_facts refusal: one sentence naming the state and
// the fitting action, with the HTTP status a caller shows it with.
type FactsRefusal struct {
	Status int
	Msg    string
}

func (e *FactsRefusal) Error() string { return e.Msg }

func factsRefuse(status int, f string, a ...any) error {
	return &FactsRefusal{Status: status, Msg: fmt.Sprintf(f, a...)}
}

func cutFacts(out string) string {
	if len(out) <= clusterFactsMax {
		return out
	}
	cut := strings.LastIndex(out[:clusterFactsMax], "\n")
	if cut < 0 {
		for cut = clusterFactsMax; cut > 0 && !utf8.RuneStart(out[cut]); cut-- {
		}
	}
	return fmt.Sprintf("%s\n[%d bytes cut]", out[:cut], len(out)-cut)
}

func factsStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// LiveSessionReader reads a card's running session.
type LiveSessionReader interface {
	LiveSession(ctx context.Context, taskID string) (LiveSession, bool, error)
}

// PodReader reads a pod and a node as the cluster reports them now.
type PodReader interface {
	PodDiagnosis(ctx context.Context, namespace, name string) (PodDiagnosis, error)
	NodeState(ctx context.Context, name string) (NodeState, error)
}

// ClusterFactsReader answers cluster_facts. Live and Kube may be nil: a
// running session is then unknown, and a node's state is left out.
type ClusterFactsReader struct {
	Store ClusterStore
	Live  LiveSessionReader
	Kube  PodReader
}

// recordedLifeFallback is how far back a record without its start looks
// for the incidents of its node.
const recordedLifeFallback = 2 * time.Hour

func incidentLine(p PlatformIncident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", factsStamp(p.At), p.Kind, p.Subject)
	if p.Node != "" {
		fmt.Fprintf(&b, " on %s", p.Node)
	}
	if p.Until != nil {
		fmt.Fprintf(&b, " to %s", factsStamp(*p.Until))
	}
	for _, k := range slices.Sorted(maps.Keys(p.Detail)) {
		fmt.Fprintf(&b, " %s=%v", k, p.Detail[k])
	}
	return b.String()
}

// nodeIncidents writes the incidents on node between from and to, each as
// a list line, under head; nothing when there are none.
func (r ClusterFactsReader) nodeIncidents(ctx context.Context, b *strings.Builder, head, node string, from, to time.Time) error {
	in, err := r.Store.PlatformIncidents(ctx, from, to, 0)
	if err != nil {
		return err
	}
	in = slices.DeleteFunc(in, func(p PlatformIncident) bool { return p.Node != node })
	if len(in) == 0 {
		return nil
	}
	b.WriteString(head + "\n")
	for _, p := range in[:min(len(in), incidentRowMax)] {
		b.WriteString("- " + incidentLine(p) + "\n")
	}
	if len(in) > incidentRowMax {
		fmt.Fprintf(b, "[more than %d incidents: ask a shorter window]\n", incidentRowMax)
	}
	return nil
}

func (r ClusterFactsReader) nodeFacts(ctx context.Context, b *strings.Builder, node string) {
	if r.Kube == nil || node == "" {
		return
	}
	n, err := r.Kube.NodeState(ctx, node)
	switch {
	case errors.Is(err, ErrNotFound):
		b.WriteString("node now: gone\n")
	case err != nil:
		b.WriteString("node now: not readable\n")
	default:
		fmt.Fprintf(b, "node now: ready %t, since %s, boot %s\n", n.Ready, factsStamp(n.Since), n.BootID)
	}
}

// SessionText is what the cluster did to session sessionID of card taskID:
// the record kept at its end, or, for the card's running session, the pod
// and its node as they are now.
func (r ClusterFactsReader) SessionText(ctx context.Context, taskID, sessionID string, now time.Time) (string, error) {
	if r.Store == nil {
		return "", factsRefuse(http.StatusConflict, "the factory keeps no cluster facts here")
	}
	d, ok, err := r.Store.SessionDeath(ctx, taskID, sessionID)
	if err != nil {
		return "", err
	}
	if !ok {
		return r.liveText(ctx, taskID, sessionID, now)
	}
	var b strings.Builder
	line := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, value)
		}
	}
	line("class", string(d.Class))
	line("rule", string(d.Rule))
	line("at", factsStamp(d.At))
	line("node", d.Node)
	line("pod", d.PodUID)
	if d.ExitCode != 0 || d.Reason != "" {
		line("exit code", strings.TrimSpace(fmt.Sprintf("%d %s", d.ExitCode, d.Reason)))
	}
	line("container", d.Container)
	line("condition", d.Condition)
	line("resource", d.Resource)
	line("limit", d.Limit)
	switch d.KillSource {
	case "guard":
		line("kill", "memory guard")
	case "kernel":
		line("kill", "kernel")
	default:
		line("kill", d.KillSource)
	}
	line("last command", d.LastCommand)
	r.nodeFacts(ctx, &b, d.Node)
	if len(d.Restarts) > 0 {
		b.WriteString("service restarts in the session's life:\n")
		for _, s := range d.Restarts {
			fmt.Fprintf(&b, "- %s %s", s.Service, factsStamp(s.From))
			if !s.Until.IsZero() {
				fmt.Fprintf(&b, " to %s", factsStamp(s.Until))
			}
			b.WriteString(" UTC")
			if s.Cause != "" {
				fmt.Fprintf(&b, ", %s", s.Cause)
			}
			b.WriteByte('\n')
		}
	}
	if len(d.Events) > 0 {
		b.WriteString("events of the pod:\n")
		for _, e := range d.Events {
			fmt.Fprintf(&b, "- %s %s %s (x%d)\n", factsStamp(e.At), e.Type, e.Reason, max(e.Count, 1))
		}
	}
	if d.Node != "" {
		start := d.Started
		if start.IsZero() {
			start = d.At.Add(-recordedLifeFallback)
		}
		if err := r.nodeIncidents(ctx, &b, "incidents on its node:", d.Node, start, d.At); err != nil {
			return "", err
		}
	}
	if len(d.Lines) > 0 {
		b.WriteString("lines given to the next session:\n")
		for _, l := range d.Lines {
			b.WriteString(l + "\n")
		}
	}
	return cutFacts(strings.TrimRight(b.String(), "\n")), nil
}

// liveText reads the card's running session: a session that is neither
// recorded nor running has no facts to give.
func (r ClusterFactsReader) liveText(ctx context.Context, taskID, sessionID string, now time.Time) (string, error) {
	var live LiveSession
	var ok bool
	var err error
	if r.Live != nil {
		live, ok, err = r.Live.LiveSession(ctx, taskID)
	}
	if err != nil {
		return "", err
	}
	if !ok || live.SessionID != sessionID {
		return "", factsRefuse(http.StatusNotFound, "session %s has no death record: it ended with a push or before the factory recorded deaths. Read card_step for its end.", sessionID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "session %s is running\n", sessionID)
	node := live.Node
	var pod *PodDiagnosis
	if r.Kube != nil {
		d, err := r.Kube.PodDiagnosis(ctx, live.Namespace, live.Pod)
		switch {
		case errors.Is(err, ErrNotFound):
			b.WriteString("pod phase: the pod is gone\n")
		case err != nil:
			b.WriteString("pod phase: not readable now\n")
		default:
			pod = &d
			fmt.Fprintf(&b, "pod phase: %s\n", d.Phase)
			node = cmp.Or(d.Node, node)
			if d.Reason != "" {
				fmt.Fprintf(&b, "pod reason: %s\n", d.Reason)
			}
		}
	}
	if node != "" {
		fmt.Fprintf(&b, "node: %s\n", node)
	}
	if pod != nil {
		for _, c := range pod.Containers {
			fmt.Fprintf(&b, "container %s: %s, restarts %d\n", c.Name, strings.TrimSpace(c.State+" "+c.Reason), c.Restarts)
		}
	}
	r.nodeFacts(ctx, &b, node)
	if node != "" {
		if err := r.nodeIncidents(ctx, &b, "incidents on its node since "+factsStamp(live.StartedAt)+":", node, live.StartedAt, now); err != nil {
			return "", err
		}
	}
	return cutFacts(strings.TrimRight(b.String(), "\n")), nil
}

// IncidentsText is the platform's incidents between since and until, oldest
// first.
func (r ClusterFactsReader) IncidentsText(ctx context.Context, since, until, now time.Time) (string, error) {
	if r.Store == nil {
		return "", factsRefuse(http.StatusConflict, "the factory keeps no cluster facts here")
	}
	refuse := func(f string, a ...any) (string, error) { return "", factsRefuse(http.StatusBadRequest, f, a...) }
	oldest := now.Add(-IncidentRetention)
	switch {
	case since.IsZero() || until.IsZero():
		return refuse("ask with both since and until, or with a session.")
	case until.Before(since):
		return refuse("until comes before since: ask with since before until.")
	case until.Sub(since) > incidentWindowMax:
		return refuse("a window holds at most 24 hours: ask with since and until at most 24 hours apart.")
	case since.Before(oldest):
		return refuse("the incident log keeps 30 days: ask for a window after %s.", oldest.UTC().Format("2006-01-02"))
	}
	in, err := r.Store.PlatformIncidents(ctx, since, until, incidentRowMax+1)
	if err != nil {
		return "", err
	}
	if len(in) == 0 {
		return "no incident between " + factsStamp(since) + " and " + factsStamp(until), nil
	}
	more := len(in) > incidentRowMax
	in = in[:min(len(in), incidentRowMax)]
	var b strings.Builder
	for _, p := range in {
		b.WriteString(incidentLine(p) + "\n")
	}
	if more {
		fmt.Fprintf(&b, "[more than %d incidents: ask a shorter window]\n", incidentRowMax)
	}
	return cutFacts(strings.TrimRight(b.String(), "\n")), nil
}

// Text answers a cluster_facts call: a session of card taskID
// when sessionID is set, else the window.
func (r ClusterFactsReader) Text(ctx context.Context, taskID, sessionID string, since, until, now time.Time) (string, error) {
	if sessionID != "" {
		if !since.IsZero() || !until.IsZero() {
			return "", factsRefuse(http.StatusBadRequest, "ask for a session or for a window, not both.")
		}
		return r.SessionText(ctx, taskID, sessionID, now)
	}
	return r.IncidentsText(ctx, since, until, now)
}

// ParseFactsTimes reads a call's since and until, RFC 3339; empty is zero.
func ParseFactsTimes(since, until string) (time.Time, time.Time, error) {
	parse := func(name, v string) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return t, factsRefuse(http.StatusBadRequest, "%s must be RFC 3339, like 2026-11-20T14:00:00Z: ask again with that form.", name)
		}
		return t, nil
	}
	s, err := parse("since", since)
	if err != nil {
		return s, s, err
	}
	u, err := parse("until", until)
	return s, u, err
}

// cardClusterFacts answers the check-in's and rung 1's call about card
// taskID: with no window the session is defSession.
func cardClusterFacts(ctx context.Context, r ClusterFactsReader, taskID, defSession string, args map[string]any, now time.Time) (string, error) {
	session, _ := args["session"].(string)
	since, _ := args["since"].(string)
	until, _ := args["until"].(string)
	from, to, err := ParseFactsTimes(since, until)
	if err != nil {
		return "", err
	}
	if session == "" && from.IsZero() && to.IsZero() {
		session = defSession
	}
	return r.Text(ctx, taskID, session, from, to, now)
}
