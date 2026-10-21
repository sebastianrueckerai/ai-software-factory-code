package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The reproduction base of a signal card, roadmap 194 (the #518 ruling
// made standing): the task's base was stamped from main's head at session
// start (SetTaskSHA), never from the state that was live when the signal
// fired, so the reproduction gate judged a pair base..head whose base
// could no longer show the bug — a guard-class fix (main already holds
// the fixed condition) carried no natural pair at all and escalated until
// a human ruled "build the broken state inside the test". By
// construction:
//
//  1. a signal card's filing pins the commit that was live when the alert
//     fired as the task's reproduction base (repro_base, from the
//     deployed build's roll), and the reproduction runs against that tree
//     even though main has moved on;
//  2. a guard-class fix's filing names a reproduction that constructs the
//     pre-fix state within the test itself (a ```repro-in-test block),
//     and the gate accepts base = head with that in-test pair;
//  3. an ordinary card keeps today's pair: its base is restamped from
//     main's head at session start and the base run shows the bug.
//
// Reproduce in place:
//
//	cd /workspace/repo && go test ./internal/factoryd/ -run 'TestSignalCard|TestGuardClass|TestOrdinaryCard' -count=1 -v

// fakeDeployedCommits is the rolls table as the alert intake reads it:
// the deployed build's commit of a Deployment of the factory's own
// namespace.
type fakeDeployedCommits struct {
	mu    sync.Mutex
	m     map[string]string
	reads []string
}

func (f *fakeDeployedCommits) DeployedCommit(_ context.Context, deployment string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, deployment)
	sha, ok := f.m[deployment]
	return sha, ok, nil
}

// A signal card filed while the deployment lags main pins the deployed
// commit, not main's head: the task's reproduction base is the tree that
// was live when the alert fired, and the branch's sha keeps main's head
// as today. Without a roll to read — none recorded of the Deployment, the
// alert names no service, the alert is outside the factory's own
// namespace, or no reader is wired — the filing pins main's head, which
// SetTaskSHA's session-start stamping never overwrites for the
// reproduction either.
func TestSignalCardPinsTheDeployedCommitAtFiling(t *testing.T) {
	cases := []struct {
		name       string
		rolls      *fakeDeployedCommits
		namespace  string
		service    string
		alertname  string
		want       string
		wantReads  int
		wantStatus int
	}{
		{"the deployment lags main", &fakeDeployedCommits{m: map[string]string{"factoryd": "roll-7"}}, "factory", "factoryd", "roll not live: factoryd", "roll-7", 1, 200},
		{"no roll of the deployment recorded", &fakeDeployedCommits{m: map[string]string{}}, "factory", "factoryd", "roll not live: factoryd", "sha-demo-main", 1, 200},
		{"the alert names no service", &fakeDeployedCommits{m: map[string]string{"factoryd": "roll-7"}}, "factory", "", "roll not live: factoryd", "sha-demo-main", 0, 200},
		{"a project namespace pins its filing's main head", &fakeDeployedCommits{m: map[string]string{"factoryd": "roll-7"}}, "demo-staging", "factoryd", "5xx rate above 5 %", "sha-demo-main", 0, 200},
		{"no reader wired", nil, "factory", "factoryd", "roll not live: factoryd", "sha-demo-main", 0, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newAlertRig(t)
			r.in.Namespace = "factory"
			if c.rolls != nil {
				r.in.Rolls = c.rolls
			}
			// the factory namespace belongs to the mapped project too
			r.ns.labels["factory"] = "demo-app"
			projects := r.in.Projects.(*memProjects)
			p := projects.m["demo-app"]
			if c.namespace == "factory" && !slices.Contains(p.Namespaces, c.namespace) {
				p.Namespaces = append(p.Namespaces, c.namespace)
				projects.m["demo-app"] = p
			}
			alert := sigAlert("firing", "fp1", c.namespace, firedAt, neverEnds)
			alert["labels"].(map[string]string)["alertname"] = c.alertname
			if c.service != "" {
				alert["labels"].(map[string]string)["service.name"] = c.service
			}
			if rr := r.post(t, "signoz", "hook-pw", alert); rr.Code != c.wantStatus {
				t.Fatalf("status %d: %s", rr.Code, rr.Body)
			}
			if len(r.store.tasks) != 1 {
				t.Fatalf("tasks %d, want 1", len(r.store.tasks))
			}
			var task Task
			for _, task = range r.store.tasks {
			}
			if task.Source != SourceAlert || task.SHA != "sha-demo-main" {
				t.Fatalf("the branch base moved: task %+v, want sha sha-demo-main as filed", task)
			}
			if task.ReproBase != c.want {
				t.Fatalf("reproduction base %q, want %q", task.ReproBase, c.want)
			}
			if rows := r.store.alertRows(); len(rows) != 1 || rows[0].Detail["repro_base"] != c.want {
				t.Fatalf("alert row detail = %+v, want repro_base %q recorded", rows, c.want)
			}
			if c.rolls != nil {
				c.rolls.mu.Lock()
				reads := len(c.rolls.reads)
				c.rolls.mu.Unlock()
				if reads != c.wantReads {
					t.Fatalf("deployed-commit reads %d, want %d", reads, c.wantReads)
				}
			}
		})
	}
}

// The reproduction of a signal card runs against the tree its filing
// pinned, not main's head the branch was cut from: the base pod checks
// out the deployed commit and the fault shows there.
func TestSignalCardReproducesAtThePinnedBase(t *testing.T) {
	task := gateTask
	task.Source = SourceAlert
	task.ReproBase = "roll-7"
	task.ReproCommand = "go test ./internal/x -run TestY"
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, _ := newTestRunner(t, task, kube, objects)
	hub.mainHead = "main-9" // main moved past the deployed build
	var res ReproResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		res = execRepro(t, r)
	}()
	scriptReproPods(kube, reproLine(2, "panic: runtime error: invalid memory address"), reproLine(0, "ok"))
	<-done
	if res.BaseExit != 2 || res.Exit != 0 || res.InTest || !res.Reproduced() {
		t.Fatalf("result = %+v, want the bug shown at the pinned base and the fix at the head", res)
	}
	if spec, _ := json.Marshal(kube.podAt(0).spec); !strings.Contains(string(spec), `{"name":"CHECKOUT_SHA","value":"roll-7"}`) {
		t.Fatalf("the base pod does not run at the pinned tree: %s", spec)
	}
	if got := hub.comments[len(hub.comments)-1]; !strings.Contains(got, "reproduced at base (exit 2 at roll-7)") {
		t.Fatalf("comment = %s", got)
	}
}

// A guard-class card's in-test reproduction passes the gate (the #518
// ruling): the agent's ```repro-in-test block constructs the pre-fix
// state within the test itself, the gate runs that one command at the
// head — the base is the head, no base pod — and its passing proves the
// fix, for the auto merge too.
func TestGuardClassCardReproducesInTest(t *testing.T) {
	cmd := "cat > unwired_test.go <<'EOF'\npackage factoryd\nEOF\ngo test ./internal/factoryd -run TestReproUnwiredIntake"
	task := gateTask
	task.Source = SourceAlert
	task.ReproBase = "roll-7" // pinned, but no base run happens
	task.AgentSummary = "Guarded the unwired intake.\n\n```repro-in-test\n" + cmd + "\n```\nMerge it."
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, _ := newTestRunner(t, task, kube, objects)
	var res ReproResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		res = execRepro(t, r)
	}()
	scriptReproPods(kube, reproLine(0, "ok: the unwired delivery answers 500 with a body"))
	<-done
	if res != (ReproResult{Ran: true, Command: cmd, Exit: 0, Output: "ok: the unwired delivery answers 500 with a body", ByAgent: true, InTest: true}) {
		t.Fatalf("result = %+v", res)
	}
	if !res.Reproduced() || !res.Passed() {
		t.Fatalf("the in-test pair must prove the fix: %+v", res)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods created %d, want the head run only (base = head)", len(kube.podOrder))
	}
	spec, _ := json.Marshal(kube.podAt(0).spec)
	if !strings.Contains(string(spec), `{"name":"REPO_SHA","value":"c1"}`) || strings.Contains(string(spec), "CHECKOUT_SHA") {
		t.Fatalf("the one pod must run at the head: %s", spec)
	}
	got := hub.comments[len(hub.comments)-1]
	for _, want := range []string{"**reproduced in test**", "the base is the head", "the command is the agent's own reproduction"} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Output tail at the base") {
		t.Fatalf("an in-test run has no base tail: %s", got)
	}
	if step := timelineStep(t, objects, "t1", "gate_reproduction"); step["in_test"] != true || step["base"] != "c1" {
		t.Fatalf("timeline step = %v, want in_test at base c1 (the head)", step)
	}
	// the auto merge accepts the in-test pair as the proven fix
	yes := true
	ok, _, unmet := AutoMerge(GateResults{Policy: PolicyGate{Class: policy.ClassNormal, Outcome: policy.OutcomeAuto, MergeRule: policy.MergeAuto},
		ChecksPassed: true, Reproduction: res}, Review{Verdict: "approve", Summary: "a narrow guard", StrictImprovement: &yes}, "api")
	if !ok {
		t.Fatalf("auto merge unmet: %v", unmet)
	}
}

// An ordinary card keeps today's pair (roadmap 194 changes nothing
// there): its base is the task's stamp — the fork point the branch was
// cut from, read once at session start, never main's moved head — and the
// gate's base run at that tree shows the bug.
func TestOrdinaryCardShowsTheBugAtBaseAsToday(t *testing.T) {
	task := gateTask // source api, stamped at sha base before the fix landed on main
	task.ReproCommand = "go test ./internal/x -run TestY"
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, task, kube, objects)
	hub.mainHead = "main-4" // the fix's commit landed after the filing
	ctx := context.Background()
	t0, repo, err := r.task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.startRef(ctx, &t0, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "base" {
		t.Fatalf("startRef = %q, want the stamped base", ref)
	}
	if got := store.tasks["t1"].SHA; got != "base" {
		t.Fatalf("an ordinary card's base moved to %q, want the stamp base kept", got)
	}
	if t0.ReproBase != "" {
		t.Fatalf("an ordinary card pins no reproduction base: %+v", t0)
	}
	var res ReproResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		res = execRepro(t, r)
	}()
	scriptReproPods(kube, reproLine(2, "--- FAIL: TestY at the base"), reproLine(0, "ok"))
	<-done
	if res.BaseExit != 2 || res.Exit != 0 || res.InTest || res.ByAgent || !res.Reproduced() {
		t.Fatalf("result = %+v, want the bug at the stamped base, the fix at the head", res)
	}
	if spec, _ := json.Marshal(kube.podAt(0).spec); !strings.Contains(string(spec), `{"name":"CHECKOUT_SHA","value":"base"}`) {
		t.Fatalf("the base pod does not run at the stamped base: %s", spec)
	}
	if got := hub.comments[len(hub.comments)-1]; !strings.Contains(got, "reproduced at base (exit 2 at base)") {
		t.Fatalf("comment = %s", got)
	}
}

// execRepro runs GateReproduction for task t1's pull request 9 at head
// c1 and returns its verdict.
func execRepro(t *testing.T, r *Runner) ReproResult {
	t.Helper()
	val, err := execActivity(t, r, ActivityGateReproduction, "t1", int64(9), "c1")
	if err != nil {
		t.Fatal(err)
	}
	return getInto[ReproResult](t, val, err)
}
