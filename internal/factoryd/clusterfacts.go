package factoryd

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	clusterFactsMax   = 12 << 10
	incidentWindowMax = 24 * time.Hour
	incidentRowMax    = 200
)

// FactsRefusal is a cluster_facts refusal: one sentence naming the state and
// the fitting action.
type FactsRefusal struct{ Msg string }

func (e *FactsRefusal) Error() string { return e.Msg }

func cutFacts(out string) string {
	if len(out) <= clusterFactsMax {
		return out
	}
	cut := strings.LastIndex(out[:clusterFactsMax], "\n")
	return fmt.Sprintf("%s\n[%d bytes cut]", out[:cut], len(out)-cut)
}

func factsStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// SessionFactsText is what the cluster did to session sessionID of card
// taskID, from the record kept at its end.
func SessionFactsText(ctx context.Context, s ClusterStore, taskID, sessionID string) (string, error) {
	d, ok, err := s.SessionDeath(ctx, taskID, sessionID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &FactsRefusal{fmt.Sprintf("session %s has no death record: it ended with a push or before the factory recorded deaths. Read card_step for its end.", sessionID)}
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
	if len(d.Restarts) > 0 {
		b.WriteString("service restarts in the session's life:\n")
		for _, r := range d.Restarts {
			fmt.Fprintf(&b, "- %s %s", r.Service, factsStamp(r.From))
			if !r.Until.IsZero() {
				fmt.Fprintf(&b, " to %s", factsStamp(r.Until))
			}
			b.WriteString(" UTC")
			if r.Cause != "" {
				fmt.Fprintf(&b, ", %s", r.Cause)
			}
			b.WriteByte('\n')
		}
	}
	if len(d.Events) > 0 {
		b.WriteString("events of the pod:\n")
		for _, e := range d.Events {
			fmt.Fprintf(&b, "- %s %s %s: %s\n", factsStamp(e.At), e.Type, e.Reason, e.Message)
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

// IncidentsText is the platform's incidents between since and until, oldest
// first.
func IncidentsText(ctx context.Context, s ClusterStore, since, until, now time.Time) (string, error) {
	refuse := func(f string, a ...any) (string, error) { return "", &FactsRefusal{fmt.Sprintf(f, a...)} }
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
	in, err := s.PlatformIncidents(ctx, since, until, incidentRowMax+1)
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
		fmt.Fprintf(&b, "%s %s %s", factsStamp(p.At), p.Kind, p.Subject)
		if p.Node != "" {
			fmt.Fprintf(&b, " on %s", p.Node)
		}
		for _, k := range slices.Sorted(maps.Keys(p.Detail)) {
			fmt.Fprintf(&b, " %s=%v", k, p.Detail[k])
		}
		b.WriteByte('\n')
	}
	if more {
		fmt.Fprintf(&b, "[more than %d incidents: ask a shorter window]\n", incidentRowMax)
	}
	return cutFacts(strings.TrimRight(b.String(), "\n")), nil
}

// ClusterFactsText answers a cluster_facts call: a session of card taskID
// when sessionID is set, else the window.
func ClusterFactsText(ctx context.Context, s ClusterStore, taskID, sessionID string, since, until, now time.Time) (string, error) {
	if sessionID != "" {
		if !since.IsZero() || !until.IsZero() {
			return "", &FactsRefusal{"ask for a session or for a window, not both."}
		}
		return SessionFactsText(ctx, s, taskID, sessionID)
	}
	return IncidentsText(ctx, s, since, until, now)
}

// ParseFactsTimes reads a call's since and until, RFC 3339; empty is zero.
func ParseFactsTimes(since, until string) (time.Time, time.Time, error) {
	parse := func(name, v string) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return t, &FactsRefusal{fmt.Sprintf("%s must be RFC 3339, like 2026-11-20T14:00:00Z: ask again with that form.", name)}
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
func cardClusterFacts(ctx context.Context, s ClusterStore, taskID, defSession string, args map[string]any, now time.Time) (string, error) {
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
	return ClusterFactsText(ctx, s, taskID, session, from, to, now)
}
