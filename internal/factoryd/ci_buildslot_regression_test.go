package factoryd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
)

// The CI queue p95 alert (factory.ci.job.queue): a build job whose runner
// pod finished is held off a second VM for ciReclaimWindow, and the cap
// handed its slot back only once the teardown had deleted every row of that
// VM. A deleting server row counted as live — "its IP is not yet released" —
// although its delete had been accepted: what still existed was the
// firewall, which the provider answers resource_in_use for until the
// server's attachment is gone, so the release gave up on it with its rows
// left open. One teardown therefore pinned a slot through the reclaim window
// plus the collector passes that finish an interrupted release, and builds
// queued behind the two-VM cap waited minutes past it. A deleting row holds
// no slot: its delete is already asked for, and the collector owns its rest.
func TestCISpawnerReusesABuildSlotWhileItsFirewallStillDeleting(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 70, Name: "image", RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	m, reader := testMetrics(t)
	spawner.cfg.Queue = NewCIQueue(m)
	hz, reg := ciBuildFakes(spawner)
	// Hetzner keeps answering the firewall of a server it just deleted with
	// resource_in_use, so factoryd's own release cannot finish it
	hz.fwInUseTimes = 1 << 20
	now := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	spawner.now = func() time.Time { return now }
	spawner.builds.now = func() time.Time { return now }
	spawner.cfg.Queue.now = func() time.Time { return now }
	ctx := context.Background()

	waiting := func(id int64) CIJob {
		return CIJob{ID: id, Name: "image", RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}
	}
	taken := func(id, task int64) CIJob {
		j := waiting(id)
		j.Status, j.TaskID = "running", task
		return j
	}
	setJobs := func(js ...CIJob) {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		hub.jobs = js
	}
	poll := func(step time.Duration) {
		now = now.Add(step)
		spawner.poll(ctx)
	}

	poll(0) // job 70 gets its VM
	if len(hub.registered) != 1 {
		t.Fatalf("registered = %v", hub.registered)
	}
	serverRow, fwRow := ciBuildRows(t, reg, "70")
	kube.pods["factory-agents-ci-70/"+ciPodName].phase = "Succeeded"

	poll(spawner.cfg.PollInterval) // tears 70 down; Forgejo still lists it waiting

	if got := regState(t, reg, serverRow); got != cloudres.StateDeleted {
		t.Fatalf("job 70's server row = %s, want deleted: its delete was accepted", got)
	}
	if got := regState(t, reg, fwRow); got != cloudres.StateDeleting {
		t.Fatalf("job 70's firewall row = %s, want left deleting for the collector", got)
	}
	for key := range kube.secrets {
		if strings.HasPrefix(key, "factory-agents-ci-70/") {
			delete(kube.secrets, key) // the namespace's secrets went with it
		}
	}

	// Three build jobs queue behind the one that just finished, and the poll
	// that serves two of them also tears job 70 down: the reclaim guard then
	// keeps job 70 itself out of the race. Its refused firewall delete left
	// its row open, and before this fix an open row counted as live whatever
	// its state — so all three waited on a slot that was already free, past
	// the reclaim window and through the collector passes that finish the
	// delete.
	spawner.builds.cfg.Max = 2
	setJobs(waiting(70), waiting(71), waiting(72), waiting(73))
	poll(spawner.cfg.PollInterval)
	if len(hub.registered) != 3 || hub.registered[1] != "ci-job-71" || hub.registered[2] != "ci-job-72" {
		t.Fatalf("registered = %v, want jobs 71 and 72 served on the slot job 70's teardown left", hub.registered)
	}
	rows, _ := reg.OpenByOwner(ctx, "ci", "70")
	if len(rows) != 1 || rows[0].Kind != cloudres.KindFirewall {
		t.Fatalf("job 70's open rows = %+v, want its firewall alone still deleting", rows)
	}

	// Both slots are live now, and job 73 waits on the cap alone.
	for range 3 {
		poll(spawner.cfg.PollInterval)
	}
	if len(hub.registered) != 3 {
		t.Fatalf("registered = %v, want no fourth runner while both slots are live", hub.registered)
	}
	if got := histograms(t, reader, "factory.ci.job.queue"); len(got) != 0 {
		t.Fatalf("queue emitted before a runner took a capped job: %v", got)
	}

	// Job 71's runner pod finishes while Forgejo still lists its job waiting.
	// Tearing it down must hand the slot back in that same poll — firewall
	// left deleting at the provider, namespace gone — rather than when the
	// collector finally gets the delete past resource_in_use. The reclaim
	// guard keeps job 70 itself out of the race; the oldest free waiter, job
	// 73, is what the freed slot serves.
	kube.pods["factory-agents-ci-71/"+ciPodName].phase = "Succeeded"
	_, fwRow = ciBuildRows(t, reg, "71")
	hz.calls = nil

	setJobs(waiting(70), waiting(71), waiting(72), waiting(73))
	poll(spawner.cfg.PollInterval)
	if strings.Join(hz.calls, ",") != "delete server/104,in use firewall/103,create firewall/107,create server/108" {
		t.Fatalf("hetzner calls = %v, want the teardown's refused firewall then job 73's VM in one poll", hz.calls)
	}
	if got := regState(t, reg, fwRow); got != cloudres.StateDeleting {
		t.Fatalf("job 71's firewall row = %s, want left deleting for the collector", got)
	}
	if _, ok := kube.namespaces["factory-agents-ci-71"]; ok {
		t.Fatal("job 71's namespace survived its teardown")
	}
	if len(hub.registered) != 4 || hub.registered[3] != "ci-job-73" {
		t.Fatalf("registered = %v, want job 73 served at once: a deleting row holds no slot", hub.registered)
	}

	// It waited on the cap alone — not on the reclaim window of the job
	// whose runner finished, and not on the release of a finished VM: the
	// freed slot served it in the very poll that tore job 71 down. Forgejo's
	// scheduler then handed it to that runner; the poll that sees it gone
	// from the waiting list is where its queue measures.
	setJobs(waiting(70), taken(71, 71), waiting(72), taken(73, 73))
	spawner.poll(ctx)
	sample, ok := spawner.cfg.Queue.Seconds(73)
	if !ok {
		t.Fatal("job 73 taken without a measured queue")
	}
	want := ciReclaimWindow.Seconds() + 8*spawner.cfg.PollInterval.Seconds()
	if sample > want {
		t.Fatalf("job 73 queued %vs, want at most the %s reclaim window plus the polls it waited capped (%vs)",
			sample, ciReclaimWindow, want)
	}
	got := histograms(t, reader, "factory.ci.job.queue")
	if p, ok := got["job=image,label=pr-untrusted-build"]; !ok || p.Count < 2 || p.Sum > float64(p.Count)*want {
		t.Fatalf("queue histogram = %v, want every served job back within the reclaim window plus its capped polls (%vs)", got, want)
	}
}

// An interrupted release leaves its rows open: the next pass must not lose
// them, and the collector finishes what factoryd's own release could not
// get rid of.
func TestBuildVMReleaseLeavesARetryableFailureToTheCollector(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 80, Name: "image", RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	hz, reg := ciBuildFakes(spawner)
	ctx := context.Background()

	spawner.poll(ctx)
	serverRow, fwRow := ciBuildRows(t, reg, "80")
	// the provider keeps refusing the firewall: the release gives up on it
	hz.fwInUseTimes = 1 << 20
	kube.pods["factory-agents-ci-80/"+ciPodName].phase = "Succeeded"
	spawner.reconcile(ctx, codeCIRepos)

	if got := regState(t, reg, serverRow); got != cloudres.StateDeleted {
		t.Fatalf("server row = %s, want deleted", got)
	}
	if got := regState(t, reg, fwRow); got != cloudres.StateDeleting {
		t.Fatalf("firewall row = %s, want deleting", got)
	}
	if _, ok := kube.namespaces["factory-agents-ci-80"]; ok {
		t.Fatal("the namespace survived its teardown")
	}

	// the server's delete is accepted at once, so the slot is free again...
	free, err := spawner.builds.FreeSlot(ctx)
	if err != nil || !free {
		t.Fatalf("free slot = %v %v: a deleting row holds the cap", free, err)
	}
	// ...and the collector, seeing the namespace gone, finishes the firewall
	hz.fwInUseTimes = 0
	if err := spawner.builds.collector.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if open, err := reg.Open(ctx, "ci"); err != nil || len(open) != 0 {
		t.Fatalf("open rows after the collector = %+v, %v", open, err)
	}
	if len(hz.firewalls) != 0 {
		t.Fatalf("provider firewalls = %v", hz.firewalls)
	}
}

// ciBuildRows returns job owner's live server and firewall rows, before any
// release touched them.
func ciBuildRows(t *testing.T, reg *cloudres.Memory, owner string) (serverRow, fwRow int64) {
	t.Helper()
	rows, err := reg.OpenByOwner(context.Background(), "ci", owner)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows of job %s = %+v, %v", owner, rows, err)
	}
	for _, r := range rows {
		switch r.Kind {
		case cloudres.KindServer:
			serverRow = r.ID
		case cloudres.KindFirewall:
			fwRow = r.ID
		}
	}
	if serverRow == 0 || fwRow == 0 {
		t.Fatalf("rows of job %s = %+v", owner, rows)
	}
	return serverRow, fwRow
}

func regRows(t *testing.T, reg *cloudres.Memory) []cloudres.Resource {
	t.Helper()
	rows, err := reg.Open(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func regState(t *testing.T, reg *cloudres.Memory, id int64) string {
	t.Helper()
	r, ok := reg.Get(id)
	if !ok {
		t.Fatalf("row %d missing", id)
	}
	return r.State
}
