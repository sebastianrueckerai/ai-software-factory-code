package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
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
