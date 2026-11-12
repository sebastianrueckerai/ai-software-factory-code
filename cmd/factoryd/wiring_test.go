package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The console's Config literal in main.go must wire every function field it
// declares, or its routes panic on a nil func at the first click (drill 2 of
// round 2: RetryPlanCard, a panic at lanes.go's retryCard).
var fieldRe = regexp.MustCompile(`^\t([A-Z][A-Za-z0-9]*)\s+func`)

func TestConsoleConfigWiresEveryFunctionField(t *testing.T) {
	mainGo, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	consoleGo, err := os.ReadFile("../../internal/console/console.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(consoleGo), "\n") {
		m := fieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if !strings.Contains(string(mainGo), m[1]+":") {
			t.Errorf("console.Config declares %s but main.go's literal wires no %s:", m[1], m[1])
		}
	}
}

// The chain poller as main builds it, over the real store and the
// work-bounded workflows, runs a tick: every capability its post-merge
// path needs is a method of the types it takes, so the compiler pins the
// rest. The fixture is empty: ending a card through a real runner needs
// the whole runner, which the factoryd package's tests build. Requires a
// disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestWiringBuildsThePollerAsMainDoes(t *testing.T) {
	ctx := context.Background()
	store, err := factoryd.OpenStore(ctx, schematest.DSN(t, "factoryd_wiring"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	var runner *factoryd.Runner // no card to end: never called
	p := newChainPoller(&factoryd.Forgejo{}, store, func() policy.Version { return policy.Version{} }, factoryd.BoundedWorkflows{},
		nil, factoryd.Repo{}, runner.EndCovered)
	if err := p.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

// The alert intake in main.go gets the in-cluster client as its Jobs reader
// and runs the Job failed close every minute: without either, a Job failed
// card never closes on its CronJob's next run, and nothing else notices.
func TestMainWiresTheJobFailedClose(t *testing.T) {
	mainGo, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for name, re := range map[string]string{
		"the Jobs reader": `(?s)&factoryd\.AlertIntake\{.*?\bJobs:\s+kube\b.*?\}\n`,
		"the pass":        `passes\.Run\(ctx, "completed_job_alerts", time\.Minute, [^,]+, alertIntake\.CloseCompletedJobs\)`,
	} {
		if !regexp.MustCompile(re).Match(mainGo) {
			t.Errorf("main.go does not wire %s of the Job failed close", name)
		}
	}
}
