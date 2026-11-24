package factoryd

import (
	"strings"
	"testing"
	"time"
)

var (
	deathStart = time.Date(2026, 11, 23, 13, 0, 0, 0, time.UTC)
	deathEnd   = time.Date(2026, 11, 23, 14, 2, 0, 0, time.UTC)
)

const deathUID = "91adbc7d-6406-4a24-ac91-c1979b028d36"

func deathFacts() DeathFacts {
	return DeathFacts{
		Pod:     PodDiagnosis{UID: deathUID, Node: "sf-2", Phase: "Failed"},
		Started: deathStart,
		Ended:   deathEnd,
	}
}

func killAt(source, constraint string, at time.Time) KernelKill {
	return KernelKill{Source: source, Node: "sf-2", PodUID: deathUID, Constraint: constraint, At: at}
}

func TestClassifyFollowsTheRules(t *testing.T) {
	inWindow := deathEnd.Add(-time.Minute)
	for _, c := range []struct {
		name     string
		mutate   func(*DeathFacts)
		class    DeathClass
		rule     DeathRule
		resource string
		limit    string
	}{
		{"guard", func(f *DeathFacts) { f.Kills = []KernelKill{killAt("guard", "", inWindow)} }, DeathPlatform, RuleGuardKill, "", ""},
		{"node oom", func(f *DeathFacts) { f.Kills = []KernelKill{killAt("kernel", "CONSTRAINT_NONE", inWindow)} }, DeathPlatform, RuleNodeOOM, "", ""},
		{"node pressure", func(f *DeathFacts) {
			f.Pod.Reason = "Evicted"
			f.Pod.Message = "The node was low on resource: ephemeral-storage. Threshold quantity: 10Gi, available: 9Gi."
		}, DeathPlatform, RuleNodePressure, "ephemeral-storage", ""},
		{"node rebooted", func(f *DeathFacts) {
			f.BootAtStart = "boot-1"
			f.Node = &NodeState{Name: "sf-2", BootID: "boot-2", Ready: true}
		}, DeathPlatform, RuleNodeRebooted, "", ""},
		{"same boot", func(f *DeathFacts) {
			f.BootAtStart = "boot-1"
			f.Node = &NodeState{Name: "sf-2", BootID: "boot-1", Ready: true}
		}, DeathUnknown, RuleNone, "", ""},
		{"node gone", func(f *DeathFacts) { f.NodeGone = true }, DeathPlatform, RuleNodeNotReady, "", ""},
		{"node not ready", func(f *DeathFacts) { f.Node = &NodeState{Name: "sf-2", Ready: false} }, DeathPlatform, RuleNodeNotReady, "", ""},
		{"node not ready during the session", func(f *DeathFacts) {
			f.Node = &NodeState{Name: "sf-2", Ready: true}
			f.NodeNotReadyDuring = true
		}, DeathPlatform, RuleNodeNotReady, "", ""},
		{"disruption", func(f *DeathFacts) {
			f.Pod.Conditions = []PodCondition{{Type: "Ready", Status: "False"}, {Type: "DisruptionTarget", Status: "True", Reason: "TerminationByKubelet"}}
		}, DeathPlatform, RuleDisruption, "", ""},
		{"disruption false", func(f *DeathFacts) {
			f.Pod.Conditions = []PodCondition{{Type: "DisruptionTarget", Status: "False"}}
		}, DeathUnknown, RuleNone, "", ""},
		{"after restart", func(f *DeathFacts) { f.AfterRestart = true }, DeathPlatform, RuleGoneAfterRestart, "", ""},
		{"after restart, in factoryd's stop", func(f *DeathFacts) { f.AfterRestart, f.PodGone, f.StoppedByFactory = true, true, true }, DeathUnknown, RuleNone, "", ""},
		{"pod gone", func(f *DeathFacts) { f.PodGone = true }, DeathPlatform, RuleDeleted, "", ""},
		{"pod deleting", func(f *DeathFacts) { f.Pod.Deleting = true }, DeathPlatform, RuleDeleted, "", ""},
		{"memcg kill", func(f *DeathFacts) {
			f.Pod.Containers = []ContainerState{
				{Name: "clone", State: "terminated", Reason: "Completed", MemoryLimit: "1Gi"},
				{Name: "agent", State: "terminated", Reason: "Error", ExitCode: 137, MemoryLimit: "2Gi"},
			}
			f.Kills = []KernelKill{killAt("kernel", "CONSTRAINT_MEMCG", inWindow)}
		}, DeathOwnLimit, RuleMemoryLimit, "", "2Gi"},
		{"oom killed", func(f *DeathFacts) {
			f.Pod.Containers = []ContainerState{{Name: "agent", State: "terminated", Reason: "OOMKilled", ExitCode: 137, MemoryLimit: "4Gi"}}
		}, DeathOwnLimit, RuleMemoryLimit, "", "4Gi"},
		{"emptydir", func(f *DeathFacts) {
			f.Pod.Reason = "Evicted"
			f.Pod.Message = `Usage of EmptyDir volume "tmp" exceeds the limit "4Gi". `
		}, DeathOwnLimit, RuleStorageLimit, "tmp", "4Gi"},
		{"pod storage", func(f *DeathFacts) {
			f.Pod.Reason = "Evicted"
			f.Pod.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 10Gi."
		}, DeathOwnLimit, RuleStorageLimit, "ephemeral-storage", "10Gi"},
		{"container storage", func(f *DeathFacts) {
			f.Pod.Reason = "Evicted"
			f.Pod.Message = `Container agent exceeded its local ephemeral storage limit "1Gi". `
		}, DeathOwnLimit, RuleStorageLimit, "ephemeral-storage", "1Gi"},
		{"nothing", func(f *DeathFacts) {}, DeathUnknown, RuleNone, "", ""},
	} {
		f := deathFacts()
		c.mutate(&f)
		r := Classify(f)
		if r.Class != c.class || r.Rule != c.rule || r.Resource != c.resource || r.Limit != c.limit {
			t.Errorf("%s: got class %s rule %s resource %q limit %q; want %s %s %q %q", c.name, r.Class, r.Rule, r.Resource, r.Limit, c.class, c.rule, c.resource, c.limit)
		}
		if !r.At.Equal(deathEnd) || r.Node != "sf-2" || r.PodUID != deathUID {
			t.Errorf("%s: record base = at %v node %q uid %q", c.name, r.At, r.Node, r.PodUID)
		}
		if c.rule == RuleDisruption && r.Condition != "TerminationByKubelet" {
			t.Errorf("%s: condition = %q", c.name, r.Condition)
		}
		if want := map[string]string{"guard": "guard", "node oom": "kernel", "memcg kill": "kernel"}[c.name]; r.KillSource != want {
			t.Errorf("%s: kill source = %q; want %q", c.name, r.KillSource, want)
		}
	}
}

func TestClassifyTakesTheNodeFromTheKill(t *testing.T) {
	for _, k := range []KernelKill{killAt("guard", "", deathEnd), killAt("kernel", "CONSTRAINT_NONE", deathEnd), killAt("kernel", "CONSTRAINT_MEMCG", deathEnd)} {
		k.Node = "sf-3"
		f := deathFacts()
		f.Pod.Node = ""
		f.Kills = []KernelKill{k}
		if r := Classify(f); r.Node != "sf-3" {
			t.Errorf("%s %s kill on sf-3 with no pod node: node %q", k.Source, k.Constraint, r.Node)
		}
	}
}

func TestClassifyIgnoresOtherPodsKills(t *testing.T) {
	f := deathFacts()
	other := killAt("guard", "", deathEnd)
	other.PodUID = "0f1e2d3c-4b5a-6978-8091-a2b3c4d5e6f7"
	f.Kills = []KernelKill{other}
	if r := Classify(f); r.Class != DeathUnknown {
		t.Errorf("another pod's kill = %s %s", r.Class, r.Rule)
	}
	f = deathFacts()
	f.Pod.UID = ""
	unnamed := killAt("guard", "", deathEnd)
	unnamed.PodUID = ""
	f.Kills = []KernelKill{unnamed}
	if r := Classify(f); r.Class != DeathUnknown {
		t.Errorf("a kill naming no pod, for a pod of no UID = %s %s", r.Class, r.Rule)
	}
}

func TestClassifyTakesTheFirstFailedContainer(t *testing.T) {
	f := deathFacts()
	f.LastCommand = "make test"
	f.Pod.Containers = []ContainerState{
		{Name: "init", State: "terminated", Reason: "Completed"},
		{Name: "agent", State: "terminated", Reason: "Error", ExitCode: 2},
		{Name: "side", State: "terminated", Reason: "Error", ExitCode: 3},
	}
	r := Classify(f)
	if r.Container != "agent" || r.ExitCode != 2 || r.Reason != "Error" || r.LastCommand != "make test" {
		t.Errorf("record = %+v", r)
	}
}

func TestClassifyOrder(t *testing.T) {
	f := deathFacts()
	f.Pod.Containers = []ContainerState{{Name: "agent", State: "terminated", Reason: "OOMKilled", ExitCode: 137}}
	f.Kills = []KernelKill{killAt("guard", "", deathEnd)}
	if r := Classify(f); r.Rule != RuleGuardKill || r.KillSource != "guard" {
		t.Errorf("guard with OOMKilled = %s %q", r.Rule, r.KillSource)
	}
	f = deathFacts()
	f.StoppedByFactory = true
	f.Pod.Deleting = true
	if r := Classify(f); r.Class != DeathUnknown {
		t.Errorf("a factory stop of a deleting pod = %s %s", r.Class, r.Rule)
	}

	// Every rule's fact at once, then each winner's fact peeled off: each
	// rule must beat every rule after it.
	in := deathEnd.Add(-time.Minute)
	f = deathFacts()
	f.Kills = []KernelKill{killAt("guard", "", in), killAt("kernel", "CONSTRAINT_NONE", in), killAt("kernel", "CONSTRAINT_MEMCG", in)}
	f.Pod.Reason, f.Pod.Message = "Evicted", "The node was low on resource: memory. "
	f.BootAtStart, f.Node = "boot-1", &NodeState{Name: "sf-2", BootID: "boot-2"}
	f.Pod.Conditions = []PodCondition{{Type: "DisruptionTarget", Status: "True", Reason: "PreemptionByScheduler"}}
	f.AfterRestart, f.PodGone = true, true
	f.Pod.Containers = []ContainerState{{Name: "agent", State: "terminated", Reason: "OOMKilled", ExitCode: 137}}
	// a worker that reported its own end beats every rule
	f.Reported = true
	if r := Classify(f); r.Class != DeathUnknown || r.Rule != RuleNone || r.Container != "agent" {
		t.Fatalf("a reported end with every rule's fact = %s %s %q; want unknown with its container", r.Class, r.Rule, r.Container)
	}
	f.Reported = false
	for _, step := range []struct {
		rule   DeathRule
		source string
		peel   func()
	}{
		{RuleGuardKill, "guard", func() { f.Kills = f.Kills[1:] }},
		{RuleNodeOOM, "kernel", func() { f.Kills = f.Kills[1:] }},
		{RuleNodePressure, "", func() { f.Pod.Message = `Usage of EmptyDir volume "tmp" exceeds the limit "4Gi". ` }},
		{RuleNodeRebooted, "", func() { f.Node.BootID = "boot-1" }},
		{RuleNodeNotReady, "", func() { f.Node.Ready = true }},
		{RuleDisruption, "", func() { f.Pod.Conditions = nil }},
		{RuleGoneAfterRestart, "", func() { f.AfterRestart = false }},
		{RuleDeleted, "", func() { f.PodGone = false }},
		{RuleMemoryLimit, "kernel", func() { f.Kills, f.Pod.Containers = nil, nil }},
		{RuleStorageLimit, "", func() { f.Pod.Reason = "" }},
		{RuleNone, "", func() {}},
	} {
		if r := Classify(f); r.Rule != step.rule || r.KillSource != step.source {
			t.Fatalf("got %s %q; want %s %q", r.Rule, r.KillSource, step.rule, step.source)
		}
		step.peel()
	}
}

func TestClassifyIgnoresKillsOutsideTheSession(t *testing.T) {
	f := deathFacts()
	f.Kills = []KernelKill{killAt("guard", "", deathStart.Add(-24*time.Hour))}
	if r := Classify(f); r.Class != DeathUnknown {
		t.Errorf("a kill a day early = %s %s", r.Class, r.Rule)
	}
	f.Kills = []KernelKill{killAt("guard", "", deathEnd.Add(time.Minute))}
	if r := Classify(f); r.Rule != RuleGuardKill {
		t.Errorf("a kill at Ended+1m = %s", r.Rule)
	}
	f.Kills = []KernelKill{killAt("guard", "", deathEnd.Add(time.Minute+time.Second))}
	if r := Classify(f); r.Class != DeathUnknown {
		t.Errorf("a kill after Ended+1m = %s", r.Rule)
	}
}

func TestGateLinesExact(t *testing.T) {
	guard := DeathRecord{Class: DeathPlatform, Rule: RuleGuardKill, At: deathEnd, Node: "sf-2"}
	want := "Your previous session did not fail on its own. The node's memory guard killed its pod on sf-2 at 14:02 UTC. Work after the last push, commit abc1234, is lost. That session does not count against the card."
	if got := strings.Join(GateLines(guard, "abc1234def"), " "); got != want {
		t.Errorf("guard lines = %q", got)
	}
	if got := GateLines(guard, "")[0]; !strings.Contains(got, "Work it did not push is lost.") {
		t.Errorf("no push = %q", got)
	}

	mem := DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, Limit: "4Gi", LastCommand: "go test -race ./..."}
	want = "Your previous session's pod reached its 4 GiB memory limit while it ran `go test -race ./...`, and the kernel killed it."
	if got := GateLines(mem, ""); len(got) != 1 || got[0] != want {
		t.Errorf("memory lines = %q", got)
	}
	mem.LastCommand = "echo `date`\n  go test ./..."
	want = "Your previous session's pod reached its 4 GiB memory limit while it ran ``echo `date` go test ./...``, and the kernel killed it."
	if got := GateLines(mem, ""); len(got) != 1 || got[0] != want {
		t.Errorf("two-line command lines = %q", got)
	}
	mem.Limit, mem.LastCommand = "", " \n "
	want = "Your previous session's pod reached its memory limit, and the kernel killed it."
	if got := GateLines(mem, ""); len(got) != 1 || got[0] != want {
		t.Errorf("bare memory lines = %q", got)
	}

	vol := DeathRecord{Class: DeathOwnLimit, Rule: RuleStorageLimit, Resource: "tmp", Limit: "4Gi", LastCommand: "npm ci"}
	want = "Your previous session's pod filled its `tmp` volume past its 4 GiB limit while it ran `npm ci`, and Kubernetes evicted it."
	if got := GateLines(vol, ""); len(got) != 1 || got[0] != want {
		t.Errorf("volume lines = %q", got)
	}
	eph := DeathRecord{Class: DeathOwnLimit, Rule: RuleStorageLimit, Resource: "ephemeral-storage", Limit: "512Mi", LastCommand: "npm ci"}
	want = "Your previous session's pod used more than its 512 MiB of local storage while it ran `npm ci`, and Kubernetes evicted it."
	if got := GateLines(eph, ""); len(got) != 1 || got[0] != want {
		t.Errorf("ephemeral lines = %q", got)
	}

	at := func(h, m int) time.Time { return time.Date(2026, 11, 23, h, m, 0, 0, time.UTC) }
	fg := DeathRecord{Class: DeathOwnLimit, Rule: RuleMemoryLimit, Restarts: []ServiceRestart{{Service: ServiceForgejo, From: at(14, 2), Until: at(14, 4), Cause: "guard"}}}
	want = "Forgejo restarted from 14:02 to 14:04 UTC during your previous session. The node's memory guard killed it. Git errors in that window came from the restart."
	if got := GateLines(fg, ""); len(got) != 2 || got[1] != want {
		t.Errorf("forgejo lines = %q", got)
	}
	open := DeathRecord{Class: DeathUnknown, Rule: RuleNone, Restarts: []ServiceRestart{{Service: ServiceLiteLLM, From: at(14, 2), Cause: "kernel"}}}
	want = "LiteLLM restarted at 14:02 UTC during your previous session. The kernel killed it for its memory. Model call errors in that window came from the restart."
	if got := GateLines(open, ""); len(got) != 1 || got[0] != want {
		t.Errorf("open restart lines = %q", got)
	}
	gw := DeathRecord{Class: DeathUnknown, Rule: RuleNone, Restarts: []ServiceRestart{{Service: ServiceGateway, From: at(14, 2), Until: at(14, 3)}}}
	want = "The model gateway restarted from 14:02 to 14:03 UTC during your previous session. Model call errors in that window came from the restart."
	if got := GateLines(gw, ""); len(got) != 1 || got[0] != want {
		t.Errorf("gateway lines = %q", got)
	}
}

func TestGateLinesRuleSentences(t *testing.T) {
	for _, c := range []struct {
		r    DeathRecord
		want string
	}{
		{DeathRecord{Rule: RuleNodeOOM, Node: "sf-2"}, "The node sf-2 ran out of memory at 14:02 UTC, and the kernel killed its pod."},
		{DeathRecord{Rule: RuleNodePressure, Node: "sf-2", Resource: "memory"}, "The node sf-2 ran low on memory at 14:02 UTC, and Kubernetes evicted its pod."},
		{DeathRecord{Rule: RuleNodeRebooted, Node: "sf-2"}, "The node sf-2 restarted at about 14:02 UTC."},
		{DeathRecord{Rule: RuleNodeNotReady, Node: "sf-2"}, "The node sf-2 stopped answering at about 14:02 UTC."},
		{DeathRecord{Rule: RuleNodePressure, Node: "sf-2"}, "The node sf-2 ran low on resources at 14:02 UTC, and Kubernetes evicted its pod."},
		{DeathRecord{Rule: RuleGuardKill}, "The node's memory guard killed its pod at 14:02 UTC."},
		{DeathRecord{Rule: RuleNodeOOM}, "Its node ran out of memory at 14:02 UTC, and the kernel killed its pod."},
		{DeathRecord{Rule: RuleNodePressure, Resource: "memory"}, "Its node ran low on memory at 14:02 UTC, and Kubernetes evicted its pod."},
		{DeathRecord{Rule: RuleNodeRebooted}, "Its node restarted at about 14:02 UTC."},
		{DeathRecord{Rule: RuleNodeNotReady}, "Its node stopped answering at about 14:02 UTC."},
		{DeathRecord{Rule: RuleDisruption, Condition: "TerminationByKubelet"}, "Kubernetes stopped its pod at 14:02 UTC: the node's kubelet ended it."},
		{DeathRecord{Rule: RuleDisruption, Condition: "PreemptionByScheduler"}, "Kubernetes stopped its pod at 14:02 UTC: a pod of higher priority took its place."},
		{DeathRecord{Rule: RuleDisruption, Condition: "DeletionByTaintManager"}, "Kubernetes stopped its pod at 14:02 UTC: its node was marked for eviction."},
		{DeathRecord{Rule: RuleDisruption, Condition: "EvictionByEvictionAPI"}, "Kubernetes stopped its pod at 14:02 UTC: an eviction request."},
		{DeathRecord{Rule: RuleDisruption, Condition: "DeletionByPodGC"}, "Kubernetes stopped its pod at 14:02 UTC: the cleanup of pods on a lost node."},
		{DeathRecord{Rule: RuleDisruption, Condition: "SomeNewReason"}, "Kubernetes stopped its pod at 14:02 UTC."},
		{DeathRecord{Rule: RuleDisruption}, "Kubernetes stopped its pod at 14:02 UTC."},
		{DeathRecord{Rule: RuleGoneAfterRestart}, "Its pod was gone when the factory restarted at 14:02 UTC."},
		{DeathRecord{Rule: RuleDeleted}, "Something outside the factory deleted its pod at 14:02 UTC."},
	} {
		c.r.Class, c.r.At = DeathPlatform, deathEnd
		got := GateLines(c.r, "")
		if len(got) != 1 || !strings.Contains(got[0], " "+c.want+" Work it did not push is lost.") {
			t.Errorf("%s = %q", c.r.Rule, got)
		}
	}
}

func TestGateLinesCaps(t *testing.T) {
	at := time.Date(2026, 11, 23, 14, 2, 0, 0, time.UTC)
	r := DeathRecord{Class: DeathPlatform, Rule: RuleDeleted, At: deathEnd}
	for _, s := range []string{ServiceForgejo, ServiceGateway, ServiceLiteLLM, ServiceForgejo} {
		r.Restarts = append(r.Restarts, ServiceRestart{Service: s, From: at, Until: at.Add(time.Minute)})
	}
	if got := GateLines(r, ""); len(got) != 3 {
		t.Errorf("four restarts gave %d lines", len(got))
	}
	r.Restarts = []ServiceRestart{
		{Service: ServiceLiteLLM, From: at.Add(8 * time.Minute), Until: at.Add(9 * time.Minute)},
		{Service: ServiceGateway, From: at.Add(3 * time.Minute), Until: at.Add(4 * time.Minute)},
		{Service: ServiceForgejo, From: at, Until: at.Add(time.Minute)},
	}
	oldest := []string{
		"Forgejo restarted from 14:02 to 14:03 UTC during your previous session. Git errors in that window came from the restart.",
		"The model gateway restarted from 14:05 to 14:06 UTC during your previous session. Model call errors in that window came from the restart.",
	}
	if got := GateLines(r, ""); len(got) != 3 || got[1] != oldest[0] || got[2] != oldest[1] {
		t.Errorf("restarts given newest first = %q; want the two oldest, oldest first", got)
	}

	vol := strings.Repeat("v", 63)
	long := DeathRecord{Class: DeathOwnLimit, Rule: RuleStorageLimit, Resource: vol, Limit: "4Gi", LastCommand: strings.Repeat("x", 2000)}
	for _, from := range []time.Time{at, at.Add(3 * time.Minute)} {
		long.Restarts = append(long.Restarts, ServiceRestart{Service: ServiceGateway, From: from, Until: from.Add(2 * time.Minute), Cause: "kernel"})
	}
	got := GateLines(long, "")
	total := 0
	for _, l := range got {
		total += len(l)
	}
	// 299 bytes for the class line and 174 for each restart: a third line would make 647.
	want := "Your previous session's pod filled its `" + vol + "` volume past its 4 GiB limit while it ran `" + strings.Repeat("x", 120) + "…`, and Kubernetes evicted it."
	if len(got) != 2 || got[0] != want || total > 600 {
		t.Errorf("long command: %d lines, %d bytes: %q", len(got), total, got)
	}

	skip := DeathRecord{Class: DeathUnknown, Rule: RuleNone, Restarts: []ServiceRestart{{Service: ServiceFactoryd, From: at}, {Service: ServiceAPIServer, From: at}, {Service: ServiceOpenBao, From: at}}}
	if got := GateLines(skip, ""); len(got) != 0 {
		t.Errorf("factoryd, apiserver and openbao restarts gave %q", got)
	}
	if got := GateLines(DeathRecord{Class: DeathUnknown, Rule: RuleNone}, ""); len(got) != 0 {
		t.Errorf("unknown gave %q", got)
	}
}

func TestDeathRecordShort(t *testing.T) {
	for rule, want := range map[DeathRule]string{
		RuleGuardKill:        "the node's memory guard killed the pod",
		RuleNodeOOM:          "the node ran out of memory",
		RuleNodePressure:     "the node ran low on ephemeral-storage",
		RuleNodeRebooted:     "the node restarted",
		RuleNodeNotReady:     "the node stopped answering",
		RuleDisruption:       "Kubernetes stopped the pod",
		RuleGoneAfterRestart: "the pod was gone after a factory restart",
		RuleDeleted:          "the pod was deleted outside the factory",
		RuleMemoryLimit:      "the pod reached its memory limit",
		RuleStorageLimit:     "the pod passed its storage limit",
		RuleNone:             "no cluster cause found",
	} {
		if got := (DeathRecord{Rule: rule, Resource: "ephemeral-storage"}).Short(); got != want {
			t.Errorf("Short(%s) = %q; want %q", rule, got, want)
		}
	}
	if got := (DeathRecord{Rule: RuleNodePressure}).Short(); got != "the node ran low on resources" {
		t.Errorf("Short of a pressure with no resource = %q", got)
	}
}
