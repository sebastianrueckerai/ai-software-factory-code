package factoryd

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

type DeathClass string

const (
	DeathPlatform DeathClass = "platform"
	DeathOwnLimit DeathClass = "own_limit"
	DeathUnknown  DeathClass = "unknown"
)

type DeathRule string

const (
	RuleGuardKill        DeathRule = "guard_kill"
	RuleNodeOOM          DeathRule = "node_oom"
	RuleNodePressure     DeathRule = "node_pressure"
	RuleNodeRebooted     DeathRule = "node_rebooted"
	RuleNodeNotReady     DeathRule = "node_not_ready"
	RuleDisruption       DeathRule = "disruption"
	RuleGoneAfterRestart DeathRule = "gone_after_restart"
	RuleDeleted          DeathRule = "deleted"
	RuleMemoryLimit      DeathRule = "memory_limit"
	RuleStorageLimit     DeathRule = "storage_limit"
	RuleNone             DeathRule = "none"
)

const (
	ServiceFactoryd  = "factoryd"
	ServiceGateway   = "factory-gateway"
	ServiceLiteLLM   = "litellm"
	ServiceOpenBao   = "openbao"
	ServiceForgejo   = "forgejo"
	ServiceAPIServer = "kube-apiserver"
)

// NodeState is a node as read at a session's end. Since is the Ready
// condition's last transition.
type NodeState struct {
	Name, BootID string
	Ready        bool
	Since        time.Time
}

type PodEvent struct {
	Reason  string    `json:"reason,omitempty"`
	Type    string    `json:"type,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at,omitzero"`
	Count   int       `json:"count,omitempty"`
}

// ServiceRestart is a restart of a service the session depends on. Cause
// is "guard", "kernel" or "".
type ServiceRestart struct {
	Service string    `json:"service"`
	From    time.Time `json:"from"`
	Until   time.Time `json:"until,omitzero"`
	Cause   string    `json:"cause,omitempty"`
}

// DeathFacts is everything Classify reads. Node nil means the node was
// not read.
type DeathFacts struct {
	Pod                                     PodDiagnosis
	PodGone, StoppedByFactory, AfterRestart bool
	BootAtStart                             string
	Node                                    *NodeState
	NodeGone                                bool
	// NodeNotReadyDuring: the incident log holds the node going NotReady
	// inside the session's life.
	NodeNotReadyDuring bool
	Started, Ended     time.Time
	Kills              []KernelKill
	LastCommand        string
}

type DeathRecord struct {
	Class       DeathClass       `json:"class"`
	Rule        DeathRule        `json:"rule"`
	At          time.Time        `json:"at"`
	Started     time.Time        `json:"started,omitzero"`
	Node        string           `json:"node,omitempty"`
	PodUID      string           `json:"pod_uid,omitempty"`
	ExitCode    int32            `json:"exit_code,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	Container   string           `json:"container,omitempty"`
	Condition   string           `json:"condition,omitempty"`
	Resource    string           `json:"resource,omitempty"`
	Limit       string           `json:"limit,omitempty"`
	KillSource  string           `json:"kill_source,omitempty"`
	LastCommand string           `json:"last_command,omitempty"`
	Restarts    []ServiceRestart `json:"restarts,omitempty"`
	Events      []PodEvent       `json:"events,omitempty"`
	Lines       []string         `json:"lines,omitempty"`
}

var (
	pressureResource = regexp.MustCompile(`resource: ([^.]*)\.`)
	quotedValue      = regexp.MustCompile(`"([^"]*)"`)
)

// Classify names who ended a session, by the first rule that holds.
func Classify(f DeathFacts) DeathRecord {
	r := DeathRecord{Class: DeathUnknown, Rule: RuleNone, At: f.Ended, Node: f.Pod.Node, PodUID: f.Pod.UID, LastCommand: f.LastCommand}
	failed := slices.IndexFunc(f.Pod.Containers, func(c ContainerState) bool {
		return c.State == "terminated" && (c.ExitCode != 0 || c.Reason != "Completed")
	})
	if failed >= 0 {
		c := f.Pod.Containers[failed]
		r.Container, r.Reason, r.ExitCode = c.Name, c.Reason, c.ExitCode
	}
	set := func(class DeathClass, rule DeathRule) DeathRecord {
		r.Class, r.Rule = class, rule
		return r
	}
	kills := f.sessionKills()
	killed := func(match func(KernelKill) bool) bool {
		i := slices.IndexFunc(kills, match)
		if i >= 0 {
			r.KillSource, r.Node = kills[i].Source, cmp.Or(r.Node, kills[i].Node)
		}
		return i >= 0
	}

	if killed(func(k KernelKill) bool { return k.Source == "guard" }) {
		return set(DeathPlatform, RuleGuardKill)
	}
	if killed(func(k KernelKill) bool { return k.Constraint == "CONSTRAINT_NONE" }) {
		return set(DeathPlatform, RuleNodeOOM)
	}
	evicted := f.Pod.Reason == "Evicted"
	if evicted && strings.HasPrefix(f.Pod.Message, "The node was low on resource") {
		if m := pressureResource.FindStringSubmatch(f.Pod.Message); m != nil {
			r.Resource = m[1]
		}
		return set(DeathPlatform, RuleNodePressure)
	}
	if f.BootAtStart != "" && f.Node != nil && f.Node.BootID != f.BootAtStart {
		return set(DeathPlatform, RuleNodeRebooted)
	}
	if f.NodeGone || (f.Node != nil && !f.Node.Ready) || f.NodeNotReadyDuring {
		return set(DeathPlatform, RuleNodeNotReady)
	}
	for _, c := range f.Pod.Conditions {
		if c.Type == "DisruptionTarget" && c.Status == "True" {
			r.Condition = c.Reason
			return set(DeathPlatform, RuleDisruption)
		}
	}
	if f.AfterRestart && !f.StoppedByFactory {
		return set(DeathPlatform, RuleGoneAfterRestart)
	}
	if (f.PodGone || f.Pod.Deleting) && !f.StoppedByFactory {
		return set(DeathPlatform, RuleDeleted)
	}
	oom := slices.IndexFunc(f.Pod.Containers, func(c ContainerState) bool { return c.State == "terminated" && c.Reason == "OOMKilled" })
	if memcg := killed(func(k KernelKill) bool { return k.Constraint == "CONSTRAINT_MEMCG" }); oom >= 0 || memcg {
		// A kernel kill names no container: the limit is the ended one's.
		if oom < 0 {
			oom = failed
		}
		if oom >= 0 {
			r.Limit = f.Pod.Containers[oom].MemoryLimit
		}
		return set(DeathOwnLimit, RuleMemoryLimit)
	}
	if evicted {
		if resource, limit, ok := storageEviction(f.Pod.Message); ok {
			r.Resource, r.Limit = resource, limit
			return set(DeathOwnLimit, RuleStorageLimit)
		}
	}
	return r
}

// sessionKills are the kills of this pod inside [Started, Ended+1m].
func (f DeathFacts) sessionKills() []KernelKill {
	var out []KernelKill
	for _, k := range f.Kills {
		if k.PodUID != "" && k.PodUID == f.Pod.UID && !k.At.Before(f.Started) && !k.At.After(f.Ended.Add(time.Minute)) {
			out = append(out, k)
		}
	}
	return out
}

func storageEviction(msg string) (resource, limit string, ok bool) {
	q := quotedValue.FindAllStringSubmatch(msg, -1)
	switch {
	case strings.HasPrefix(msg, "Usage of EmptyDir volume"):
		if len(q) < 2 {
			return "", "", false
		}
		return q[0][1], q[1][1], true
	case strings.HasPrefix(msg, "Pod ephemeral local storage usage exceeds"):
		words := strings.Fields(strings.TrimSpace(msg))
		return "ephemeral-storage", strings.TrimSuffix(words[len(words)-1], "."), true
	case strings.HasPrefix(msg, "Container ") && strings.Contains(msg, "exceeded its local ephemeral storage limit"):
		if len(q) > 0 {
			limit = q[0][1]
		} else {
			words := strings.Fields(strings.TrimSpace(msg))
			limit = strings.TrimSuffix(words[len(words)-1], ".")
		}
		return "ephemeral-storage", limit, true
	}
	return "", "", false
}

// Short is the phrase for the card's timeline.
func (r DeathRecord) Short() string {
	switch r.Rule {
	case RuleGuardKill:
		return "the node's memory guard killed the pod"
	case RuleNodeOOM:
		return "the node ran out of memory"
	case RuleNodePressure:
		return "the node ran low on " + r.lowOn()
	case RuleNodeRebooted:
		return "the node restarted"
	case RuleNodeNotReady:
		return "the node stopped answering"
	case RuleDisruption:
		return "Kubernetes stopped the pod"
	case RuleGoneAfterRestart:
		return "the pod was gone after a factory restart"
	case RuleDeleted:
		return "the pod was deleted outside the factory"
	case RuleMemoryLimit:
		return "the pod reached its memory limit"
	case RuleStorageLimit:
		return "the pod passed its storage limit"
	}
	return "no cluster cause found"
}

const (
	gateMaxLines = 3
	gateMaxBytes = 600
	commandCut   = 120
)

// GateLines are the lines the next session's handover carries: the class
// line first, then the gated services' restarts while the count and the
// size bounds hold.
func GateLines(r DeathRecord, lastPush string) []string {
	var lines []string
	total := 0
	if l := classLine(r, lastPush); l != "" {
		lines, total = append(lines, l), len(l)
	}
	restarts := slices.Clone(r.Restarts)
	slices.SortStableFunc(restarts, func(a, b ServiceRestart) int { return a.From.Compare(b.From) })
	for _, s := range restarts {
		l := restartLine(s)
		if l == "" {
			continue
		}
		if len(lines)+1 > gateMaxLines || total+len(l) > gateMaxBytes {
			break
		}
		lines = append(lines, l)
		total += len(l)
	}
	return lines
}

func classLine(r DeathRecord, lastPush string) string {
	at := clock(r.At)
	switch r.Class {
	case DeathPlatform:
		node, on := "Its node", ""
		if r.Node != "" {
			node, on = "The node "+r.Node, " on "+r.Node
		}
		var sentence string
		switch r.Rule {
		case RuleGuardKill:
			sentence = "The node's memory guard killed its pod" + on + " at " + at + " UTC."
		case RuleNodeOOM:
			sentence = node + " ran out of memory at " + at + " UTC, and the kernel killed its pod."
		case RuleNodePressure:
			sentence = node + " ran low on " + r.lowOn() + " at " + at + " UTC, and Kubernetes evicted its pod."
		case RuleNodeRebooted:
			sentence = node + " restarted at about " + at + " UTC."
		case RuleNodeNotReady:
			sentence = node + " stopped answering at about " + at + " UTC."
		case RuleDisruption:
			sentence = "Kubernetes stopped its pod at " + at + " UTC"
			if why, ok := disruptionWhy[r.Condition]; ok {
				sentence += ": " + why
			}
			sentence += "."
		case RuleGoneAfterRestart:
			sentence = "Its pod was gone when the factory restarted at " + at + " UTC."
		case RuleDeleted:
			sentence = "Something outside the factory deleted its pod at " + at + " UTC."
		default:
			return ""
		}
		lost := "Work it did not push is lost."
		if lastPush != "" {
			lost = "Work after the last push, commit " + lastPush[:min(7, len(lastPush))] + ", is lost."
		}
		return "Your previous session did not fail on its own. " + sentence + " " + lost + " That session does not count against the card."
	case DeathOwnLimit:
		ran := ""
		if c := oneLine(r.LastCommand); c != "" {
			ran = " while it ran " + codeSpan(textutil.Truncate(c, commandCut))
		}
		switch r.Rule {
		case RuleMemoryLimit:
			limit := "its memory limit"
			if r.Limit != "" {
				limit = "its " + quantity(r.Limit) + " memory limit"
			}
			return "Your previous session's pod reached " + limit + ran + ", and the kernel killed it."
		case RuleStorageLimit:
			limit := quantity(r.Limit)
			if r.Resource == "ephemeral-storage" {
				return "Your previous session's pod used more than its " + limit + " of local storage" + ran + ", and Kubernetes evicted it."
			}
			return "Your previous session's pod filled its `" + r.Resource + "` volume past its " + limit + " limit" + ran + ", and Kubernetes evicted it."
		}
	}
	return ""
}

func restartLine(s ServiceRestart) string {
	var name, errs string
	switch s.Service {
	case ServiceForgejo:
		name, errs = "Forgejo", "Git errors"
	case ServiceGateway:
		name, errs = "The model gateway", "Model call errors"
	case ServiceLiteLLM:
		name, errs = "LiteLLM", "Model call errors"
	default:
		return ""
	}
	l := name + " restarted at " + clock(s.From) + " UTC during your previous session."
	if !s.Until.IsZero() {
		l = name + " restarted from " + clock(s.From) + " to " + clock(s.Until) + " UTC during your previous session."
	}
	switch s.Cause {
	case "guard":
		l += " The node's memory guard killed it."
	case "kernel":
		l += " The kernel killed it for its memory."
	}
	return l + " " + errs + " in that window came from the restart."
}

func clock(t time.Time) string { return t.UTC().Format("15:04") }

// disruptionWhy says why Kubernetes stopped a pod, by its DisruptionTarget
// condition's reason.
var disruptionWhy = map[string]string{
	"PreemptionByScheduler":  "a pod of higher priority took its place",
	"DeletionByTaintManager": "its node was marked for eviction",
	"EvictionByEvictionAPI":  "an eviction request",
	"DeletionByPodGC":        "the cleanup of pods on a lost node",
	"TerminationByKubelet":   "the node's kubelet ended it",
}

func (r DeathRecord) lowOn() string { return cmp.Or(r.Resource, "resources") }

func quantity(q string) string {
	for _, u := range []string{"Ki", "Mi", "Gi", "Ti"} {
		if n, ok := strings.CutSuffix(q, u); ok && n != "" {
			return n + " " + u + "B"
		}
	}
	return q
}
