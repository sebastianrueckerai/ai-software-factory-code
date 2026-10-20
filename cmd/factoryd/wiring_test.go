package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
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

// The config alarm as main builds it, before the runner exists, files a
// refused commit's card and records its events whole: card_created on the
// card's timeline object and its needs-you row, which the Poker carries.
// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestWiringBuildsTheConfigAlarmAsMainDoes(t *testing.T) {
	ctx := context.Background()
	store, err := factoryd.OpenStore(ctx, schematest.DSN(t, "factoryd_wiring_alarm"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/factory-config/issues":
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/sf/factory-config/issues":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":5}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/repos/sf/factory-config/issues/5/labels":
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	objects := fakeObjects{}
	alarm := newConfigAlarm(store, &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"},
		factoryd.Repo{Owner: "sf", Name: "factory-config"}, telemetry.Noop(), objects)
	if err := alarm.Raise(ctx, "c9c9c9c9c9c9", errors.New("policy.yaml: budget: month_eur is negative"), policy.Version{}, false); err != nil {
		t.Fatalf("raise: %v", err)
	}
	row, ok, err := store.AlertByFingerprint(ctx, factoryd.AlertConfigRejected, "c9c9c9c9c9c9")
	if err != nil || !ok || row.TaskID == "" {
		t.Fatalf("alert %+v, %v, %v; want the rejection linked to its card", row, ok, err)
	}
	if line := objects["sessions/"+row.TaskID+"/timeline.jsonl"]; !bytes.Contains(line, []byte(`"event":"card_created"`)) {
		t.Fatalf("the card's timeline %q, want its card_created", line)
	}
	events, err := store.ProjectEvents(ctx, "sf/factory-config", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e factoryd.Event) bool { return e.Kind == factoryd.EventQuestionAsked && e.TaskID == row.TaskID }) {
		t.Fatalf("events %+v, want the card's needs-you row", events)
	}
}
