package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// logLines is go test output as Forgejo serves it: every line after a
// timestamp.
func logLines(lines ...string) string {
	var b strings.Builder
	for i, l := range lines {
		b.WriteString("2026-11-10T10:48:4" + string(rune('0'+i%10)) + ".1723690Z " + l + "\n")
	}
	return b.String()
}

func TestParseFailedTests(t *testing.T) {
	body := logLines(
		"ok  \texample.com/factory/internal/policy\t0.412s",
		"--- FAIL: TestRefusalNamesTheState (0.00s)",
		"    console_test.go:88: got \"refused\", want \"cannot\"",
		"    --- FAIL: TestRefusalNamesTheState/budget (0.00s)",
		"--- FAIL: TestErrorsShowNoInternals (0.01s)",
		"FAIL",
		"FAIL\texample.com/factory/internal/console\t8.213s",
		"--- FAIL: TestBudgetRefusal (0.00s)",
		"FAIL\texample.com/factory/internal/factoryd\t3.1s",
		"FAIL\texample.com/factory/internal/gateway [build failed]",
		"FAIL",
	)
	want := []FailedTest{
		{Test: "TestRefusalNamesTheState", Package: "example.com/factory/internal/console"},
		{Test: "TestErrorsShowNoInternals", Package: "example.com/factory/internal/console"},
		{Test: "TestBudgetRefusal", Package: "example.com/factory/internal/factoryd"},
		{Package: "example.com/factory/internal/gateway"},
	}
	if got := parseFailedTests([]byte(body)); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
}

// A log cut at its start has test lines without their package line
// before, and a prover-style timeout panic is no failing test.
func TestParseFailedTestsKeepsTheTimeoutApart(t *testing.T) {
	body := logLines(
		"--- FAIL: TestOne (0.00s)",
		"panic: test timed out after 10m0s",
		"running tests:",
		"\tTestSlow (9m59s)",
		"FAIL\texample.com/factory/internal/slow\t600.003s",
		"--- FAIL: TestCut (0.00s)",
	)
	want := []FailedTest{
		{Test: "TestOne", Package: "example.com/factory/internal/slow"},
		{Test: "TestCut"},
	}
	if got := parseFailedTests([]byte(body)); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
}

// The merge request of a red head names the failing tests of the red
// job's log, which a prover's timeout elsewhere does not touch.
func TestRequestApprovalNamesTheRedJobsFailingTests(t *testing.T) {
	r, fj, _ := killedRig(t, []string{killedFail}, []ActionJob{{ID: 12, Name: "test", Status: "failure", Attempt: 1}})
	fj.jobLogs["12/1"] = logLines(
		"--- FAIL: TestErrorsShowNoInternals (0.00s)",
		"    errors_test.go:31: body leaks the details",
		"FAIL\texample.com/factory/internal/console\t8.213s",
		"--- FAIL: TestBudgetRefusal (0.00s)",
		"FAIL\thttps://user:pass@host/factoryd\t3.1s",
		"--- FAIL: TestLeak/sk-abcdef0123456789abcdef (0.00s)",
		"    Authorization: Bearer abcdef0123456789abcdef0123456789",
		"FAIL\texample.com/factory/internal/leak\t1s",
	)
	in := ApprovalInput{PR: PullRequest{Number: 9}, Commit: "c1", Gates: GateResults{Policy: PolicyGate{Class: "high", PolicyVersion: 7}}, RedChecks: true}
	store := r.store.(*fakeStore)
	if _, err := execActivity(t, r, ActivityRequestApproval, "t1", in); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.ApprovalRequests(context.Background(), "pending")
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	var spec MergeSpec
	if err := json.Unmarshal(pending[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	want := []FailedTest{
		{Test: "TestErrorsShowNoInternals", Package: "example.com/factory/internal/console"},
		{Test: "TestBudgetRefusal", Package: "https://[redacted]@host/factoryd"},
		{Test: "[redacted]", Package: "example.com/factory/internal/leak"},
	}
	if !reflect.DeepEqual(spec.FailedTests, want) {
		t.Fatalf("failed tests %+v, want %+v", spec.FailedTests, want)
	}
	if raw := string(pending[0].Spec); strings.Contains(raw, "user:pass") || strings.Contains(raw, "abcdef0123456789") {
		t.Fatalf("spec leaks a secret: %s", raw)
	}
}

// A green head's request carries none, and a log factoryd cannot read
// leaves the request open with none.
func TestRequestApprovalWithoutRedChecksNamesNoTests(t *testing.T) {
	r, _, _ := killedRig(t, []string{killedOK})
	store := r.store.(*fakeStore)
	in := ApprovalInput{PR: PullRequest{Number: 9}, Commit: "c1", Gates: GateResults{Policy: PolicyGate{Class: "high"}}}
	if _, err := execActivity(t, r, ActivityRequestApproval, "t1", in); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.ApprovalRequests(context.Background(), "pending")
	var spec MergeSpec
	if len(pending) != 1 || json.Unmarshal(pending[0].Spec, &spec) != nil || len(spec.FailedTests) != 0 {
		t.Fatalf("pending = %+v, spec %+v", pending, spec)
	}
}

// The brief writer reads the typed list, under its own name.
func TestBriefFactsCarryTheFailedTests(t *testing.T) {
	spec, _ := json.Marshal(MergeSpec{RedChecks: true, FailedTests: []FailedTest{{Test: "TestA", Package: "p/q"}}})
	facts, _, err := briefFacts(ApprovalRequest{Kind: ApprovalMerge, Spec: spec}, Task{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := facts.(map[string]any)["failed_tests"]
	if !reflect.DeepEqual(got, []FailedTest{{Test: "TestA", Package: "p/q"}}) {
		t.Fatalf("failed_tests = %+v", got)
	}
}
