package policy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A document that sets the gate alone keeps every other default, and a 0
// reads as the default. A document without the block reads the default
// whole.
func TestWaitsDefaultsFieldByField(t *testing.T) {
	d, err := Parse([]byte(valid + "waits: {gate: 3h, queued: 0s}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Waits{Queued: 2 * time.Hour, Dependency: 48 * time.Hour, Gate: 3 * time.Hour, MergeRetry: time.Hour,
		ProviderBackoff: 30 * time.Minute, RunQuiet: 30 * time.Minute, RungAnswer: 35 * time.Minute, Checks: 50 * time.Minute, NoPushSessions: 3}
	if got := (Tree{Document: d}).WaitsOf("demo-app/demo-app"); got != want {
		t.Errorf("waits = %+v, want %+v", got, want)
	}
	d, err = Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	want.Gate = 6 * time.Hour
	if got := (Tree{Document: d}).WaitsOf("demo-app/demo-app"); got != want {
		t.Errorf("waits without the block = %+v, want %+v", got, want)
	}
}

// A project file may shorten a bound below policy.yaml's and never
// lengthen one. Where the fixture's policy.yaml sets none, the bound is the
// default's: queued 2h, no_push_sessions 3. Where it shortens one, the
// project's bound is held to policy.yaml's, not to the default.
func TestProjectWaitsMayOnlyShorten(t *testing.T) {
	with := func(policy, project string) map[string][]byte {
		files := fixtureTree(t)
		// the fixture's policy.yaml carries the waits row {checks: 40m};
		// each case's policy fragment replaces it
		files["policy.yaml"] = bytes.Replace(files["policy.yaml"], []byte("waits: {checks: 40m}\n"), []byte(policy), 1)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], project...)
		return files
	}
	for name, tc := range map[string]struct{ policy, block, want string }{
		"queued": {"", "waits: {queued: 3h}\n", "project demo-app: waits.queued 3h0m0s is looser than policy.yaml's waits.queued 2h0m0s"},
		"no push sessions": {"", "waits: {no_push_sessions: 4}\n",
			"project demo-app: waits.no_push_sessions 4 is looser than policy.yaml's waits.no_push_sessions 3"},
		"under the default, over a shortened policy": {"waits: {gate: 1h}\n", "waits: {gate: 90m}\n",
			"project demo-app: waits.gate 1h30m0s is looser than policy.yaml's waits.gate 1h0m0s"},
		"checks": {"waits: {checks: 1h}\n", "waits: {checks: 90m}\n",
			"project demo-app: waits.checks 1h30m0s is looser than policy.yaml's waits.checks 1h0m0s"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTree(with(tc.policy, tc.block)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	equal, err := ParseTree(with("waits: {gate: 1h}\n", "waits: {gate: 1h}\n"))
	if err != nil {
		t.Fatalf("ParseTree with the project's gate equal to policy.yaml's = %v", err)
	}
	if got := equal.WaitsOf("demo-app/demo-app").Gate; got != time.Hour {
		t.Errorf("demo-app's gate equal to policy.yaml's = %s, want 1h0m0s", got)
	}
	tree, err := ParseTree(with("waits: {queued: 4h, checks: 40m}\n", "waits: {queued: 3h, gate: 1h, checks: 30m, no_push_sessions: 2}\n"))
	if err != nil {
		t.Fatal(err)
	}
	demo := tree.WaitsOf("demo-app/demo-app")
	if demo.Queued != 3*time.Hour || demo.Gate != time.Hour || demo.NoPushSessions != 2 || demo.RunQuiet != 30*time.Minute {
		t.Errorf("demo-app's waits = %+v, want queued 3h, gate 1h, no_push_sessions 2, run_quiet the default 30m", demo)
	}
	if demo.Checks != 30*time.Minute {
		t.Errorf("demo-app's checks = %s, want the project's 30m over policy.yaml's 40m", demo.Checks)
	}
	link := tree.WaitsOf("linkshort/linkshort")
	if link.Queued != 4*time.Hour || link.Gate != 6*time.Hour || link.NoPushSessions != 3 || link.Checks != 40*time.Minute {
		t.Errorf("linkshort's waits = %+v, want policy.yaml's queued 4h and checks 40m, the default gate 6h, no_push_sessions 3", link)
	}
	got := Document{}.waitsOf(ProjectConfig{Waits: &Waits{Queued: 5 * time.Hour, RunQuiet: 10 * time.Minute, NoPushSessions: 9}})
	if got.Queued != 2*time.Hour || got.RunQuiet != 10*time.Minute || got.NoPushSessions != 3 {
		t.Errorf("a longer project bound = %+v, want queued 2h and no_push_sessions 3 kept, run_quiet 10m taken", got)
	}
}

// Bound names each timed wait by its key and answers 0 for any other.
func TestWaitsBoundByName(t *testing.T) {
	for kind, want := range map[string]time.Duration{
		"queued": 2 * time.Hour, "dependency": 48 * time.Hour, "gate": 6 * time.Hour, "merge_retry": time.Hour,
		"provider_backoff": 30 * time.Minute, "run_quiet": 30 * time.Minute, "rung_answer": 35 * time.Minute,
		"checks": 50 * time.Minute, "no_push_sessions": 0, "lunch": 0, "": 0,
	} {
		if got := DefaultWaits.Bound(kind); got != want {
			t.Errorf("Bound(%q) = %s, want %s", kind, got, want)
		}
	}
}

// Every bound lies between 1m and 7 days, no_push_sessions is not
// negative, in policy.yaml and in a project file, and no_push_sessions 0
// reads as the default 3.
func TestWaitsAreBounded(t *testing.T) {
	if _, err := Parse([]byte(valid + "waits: {queued: 1m, dependency: 168h, no_push_sessions: 1}\n")); err != nil {
		t.Fatalf("Parse at the edges = %v", err)
	}
	for name, tc := range map[string]struct{ block, want string }{
		"short":    {"waits: {gate: 30s}\n", "policy: waits.gate must be 0 for the default or lie between 1m and 7 days, is 30s"},
		"long":     {"waits: {dependency: 169h}\n", "policy: waits.dependency must be 0 for the default or lie between 1m and 7 days, is 169h0m0s"},
		"negative": {"waits: {no_push_sessions: -1}\n", "policy: waits.no_push_sessions must not be negative, is -1"},
		"checks":   {"waits: {checks: 30s}\n", "policy: waits.checks must be 0 for the default or lie between 1m and 7 days, is 30s"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(valid + tc.block)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	with := func(block string) map[string][]byte {
		files := fixtureTree(t)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], block...)
		return files
	}
	want := "project demo-app: waits.rung_answer must be 0 for the default or lie between 1m and 7 days, is 59s"
	if _, err := ParseTree(with("waits: {rung_answer: 59s}\n")); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("ParseTree = %v, want it to contain %q", err, want)
	}
	zero, err := ParseTree(with("waits: {no_push_sessions: 0}\n"))
	if err != nil {
		t.Fatalf("ParseTree with no_push_sessions 0 = %v", err)
	}
	if got := zero.WaitsOf("demo-app/demo-app").NoPushSessions; got != 3 {
		t.Errorf("no_push_sessions 0 reads as %d, want the default 3", got)
	}
}

// The bounds are strings in JSON, as in YAML and as Limits' durations: the
// stored tree and GET /policy show 2h0m0s.
func TestWaitsJSON(t *testing.T) {
	w := Waits{Queued: time.Hour, Dependency: 2 * time.Hour, Gate: 3 * time.Hour, MergeRetry: 4 * time.Hour,
		ProviderBackoff: 5 * time.Minute, RunQuiet: 6 * time.Minute, RungAnswer: 7 * time.Minute, Checks: 12 * time.Minute, NoPushSessions: 8}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"queued":"1h0m0s","dependency":"2h0m0s","gate":"3h0m0s","merge_retry":"4h0m0s",` +
		`"provider_backoff":"5m0s","run_quiet":"6m0s","rung_answer":"7m0s","checks":"12m0s","no_push_sessions":8}`; string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
	var back Waits
	if err := json.Unmarshal(b, &back); err != nil || back != w {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"gate":"soon"}`), &Waits{}); err == nil {
		t.Fatal("a bad duration decoded")
	}
}
