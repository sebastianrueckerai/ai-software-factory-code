package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const factorydPkg = "github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"

func scanOf(t *testing.T, log string) *failScan {
	t.Helper()
	s := newFailScan()
	for _, l := range strings.SplitAfter(log, "\n") {
		if l != "" {
			s.line(l)
		}
	}
	s.end()
	return s
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A section of the real log of a red test job (run 3840): the one failing
// test of a package, its subtest left under it, the timing column off the
// package, the bare FAIL lines no entry.
func TestFailScanReadsARealJobLog(t *testing.T) {
	s := scanOf(t, fixture(t, "joblog-run3840-test.log"))
	want := []FailedTest{{Test: "TestInterimPushRefusesTooLargeWorkBeforeReadingIt", Package: factorydPkg, Kind: "failed"}}
	if !reflect.DeepEqual(s.out, want) || s.more != 0 {
		t.Fatalf("entries %+v (more %d), want %+v", s.out, s.more, want)
	}
}

// Real go1.26.10 output of a package whose test failed, then whose slow
// test ran into go test -timeout 1s: both are named, the slow one with the
// duration, its subtest folded into it.
func TestFailScanNamesTheTestsATimeoutKilled(t *testing.T) {
	s := scanOf(t, fixture(t, "gotest-timeout.log"))
	want := []FailedTest{
		{Test: "TestFailsFirst", Package: "example.com/to", Kind: "failed"},
		{Test: "TestSlow", Package: "example.com/to", Kind: "timed out after 1s"},
	}
	if !reflect.DeepEqual(s.out, want) {
		t.Fatalf("entries %+v, want %+v", s.out, want)
	}
}

func TestFailScanPackageLines(t *testing.T) {
	s := scanOf(t, logLines(
		"--- FAIL: TestRefusalNamesTheState (0.00s)",
		"    --- FAIL: TestRefusalNamesTheState/budget (0.00s)",
		"--- FAIL: TestProjectMetricsAnswers403ForNonWriter (0.01s)",
		"FAIL\texample.com/factory/internal/console\t8.213s",
		"FAIL\texample.com/factory/internal/gateway [build failed]",
		"FAIL\texample.com/factory/internal/store [setup failed]",
		"FAIL\texample.com/factory/internal/exits\t0.1s",
		"FAIL",
	))
	want := []FailedTest{
		{Test: "TestRefusalNamesTheState", Package: "example.com/factory/internal/console", Kind: "failed"},
		{Test: "TestProjectMetricsAnswers403ForNonWriter", Package: "example.com/factory/internal/console", Kind: "failed"},
		{Package: "example.com/factory/internal/gateway", Kind: "no test ran"},
		{Package: "example.com/factory/internal/store", Kind: "no test ran"},
		{Package: "example.com/factory/internal/exits", Kind: "no test named"},
	}
	if !reflect.DeepEqual(s.out, want) {
		t.Fatalf("entries %+v, want %+v", s.out, want)
	}
}

// A name or package off the grammar is dropped, whatever text it carries;
// a log that ends inside its package leaves the package empty.
func TestFailScanKeepsOnlyGrammarNames(t *testing.T) {
	s := scanOf(t, logLines(
		"--- FAIL: TestLeak/sk-abcdef0123456789abcdef (0.00s)",
		"--- FAIL: Test-Dash (0.00s)",
		"--- FAIL: TestKept (0.00s)",
		"FAIL\thttps://user:pass@host/factoryd\t3.1s",
		"--- FAIL: TestCut (0.00s)",
	))
	want := []FailedTest{{Test: "TestKept", Kind: "failed"}, {Test: "TestCut", Kind: "failed"}}
	if !reflect.DeepEqual(s.out, want) {
		t.Fatalf("entries %+v, want %+v", s.out, want)
	}
	if raw, _ := json.Marshal(s.out); strings.Contains(string(raw), "user:pass") || strings.Contains(string(raw), "abcdef0123456789") {
		t.Fatalf("entries leak: %s", raw)
	}
}

func TestFailScanCapsAndCountsTheRest(t *testing.T) {
	var lines []string
	for i := range maxFailedTests + 3 {
		lines = append(lines, fmt.Sprintf("--- FAIL: TestN%d (0.00s)", i))
	}
	lines = append(lines, "FAIL\texample.com/p\t1s")
	s := scanOf(t, logLines(lines...))
	if len(s.out) != maxFailedTests || s.more != 3 {
		t.Fatalf("%d entries, more %d; want %d and 3", len(s.out), s.more, maxFailedTests)
	}
}

// Tests past the pending bound before their package line are counted, not
// lost silently.
func TestFailScanCountsTestsPastThePendingBound(t *testing.T) {
	s := newFailScan()
	for i := range maxPendingTests + 5 {
		s.line(fmt.Sprintf("--- FAIL: TestP%d (0.00s)\n", i))
	}
	s.line("FAIL\texample.com/p\t1s\n")
	s.end()
	if len(s.out) != maxFailedTests || s.more != maxPendingTests-maxFailedTests+5 {
		t.Fatalf("%d entries, more %d; want %d and %d", len(s.out), s.more, maxFailedTests, maxPendingTests-maxFailedTests+5)
	}
}

func TestFailedTestString(t *testing.T) {
	for _, c := range []struct {
		f    FailedTest
		want string
	}{
		{FailedTest{Job: "test", Test: "TestA", Package: "p/q", Kind: "failed"}, "TestA (p/q): failed (job test)"},
		{FailedTest{Test: "TestA", Kind: "failed"}, "TestA: failed"},
		{FailedTest{Job: "test", Package: "p/b", Kind: "no test ran"}, "package p/b: no test ran (job test)"},
		{FailedTest{Job: "vet", Kind: "no test failure in its log"}, "job vet: no test failure in its log"},
	} {
		if got := c.f.String(); got != c.want {
			t.Errorf("%+v reads %q, want %q", c.f, got, c.want)
		}
	}
}

// The streamed read passes every line, keeps the first jobLogLine bytes of
// a longer one, and stops at its byte limit.
func TestScanActionJobLogStreamsBoundedLines(t *testing.T) {
	long := strings.Repeat("x", 3*jobLogLine)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Error("a streamed read sends no Range")
		}
		fmt.Fprint(w, "a\nb\n"+long+"\nlast")
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL}
	var got []string
	if err := f.ScanActionJobLog(context.Background(), "tok", "o", "r", 1, 1, 1<<20, func(l string) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "a\n" || got[1] != "b\n" || got[2] != long[:jobLogLine] || got[3] != "last" {
		t.Fatalf("lines %d, first %q", len(got), got[0])
	}
	got = nil
	err := f.ScanActionJobLog(context.Background(), "tok", "o", "r", 1, 1, 3, func(l string) { got = append(got, l) })
	if !errors.Is(err, ErrLogCut) || !reflect.DeepEqual(got, []string{"a\n"}) {
		t.Fatalf("read past the limit = %q, %v; want the whole line before it and ErrLogCut", got, err)
	}
	got = nil
	if err := f.ScanActionJobLog(context.Background(), "tok", "o", "r", 1, 1, int64(4+len(long)+1+4), func(l string) { got = append(got, l) }); err != nil || len(got) != 4 {
		t.Fatalf("a log of exactly the limit = %d lines, %v; want all 4 and no cut", len(got), err)
	}
}

// logLines is go test output as Forgejo serves it: every line after a
// timestamp.
func logLines(lines ...string) string {
	var b strings.Builder
	for i, l := range lines {
		b.WriteString("2026-11-10T10:48:4" + string(rune('0'+i%10)) + ".1723690Z " + l + "\n")
	}
	return b.String()
}

// redJobsRig is a red head whose run 7 holds jobs, each with its log.
func redJobsRig(t *testing.T, jobs []ActionJob, logs map[string]string) (*Runner, *prForgejo) {
	t.Helper()
	r, fj, _ := killedRig(t, []string{killedFail}, jobs)
	fj.wholeLogs = true
	for k, v := range logs {
		fj.jobLogs[k] = v
	}
	return r, fj
}

func requestRed(t *testing.T, r *Runner, red bool) MergeSpec {
	t.Helper()
	in := ApprovalInput{PR: PullRequest{Number: 9}, Commit: "c1", Gates: GateResults{Policy: PolicyGate{Class: "high", PolicyVersion: 7}}, RedChecks: red}
	if _, err := execActivity(t, r, ActivityRequestApproval, "t1", in); err != nil {
		t.Fatal(err)
	}
	pending, _ := r.store.(*fakeStore).ApprovalRequests(context.Background(), "pending")
	var spec MergeSpec
	if len(pending) != 1 || json.Unmarshal(pending[0].Spec, &spec) != nil {
		t.Fatalf("pending = %+v", pending)
	}
	return spec
}

// Every red job of the head is named: the test job by its failing tests
// from its whole log, a job with no go test output as such, a job whose
// log cannot be read as such; a job name goes through the scrubber, and a
// green job is not named.
func TestRequestApprovalNamesEveryRedJob(t *testing.T) {
	const deploy = "deploy Authorization: Bearer abcdef0123456789abcdef0123456789"
	r, fj := redJobsRig(t, []ActionJob{
		{ID: 12, Name: "test", Status: "failure", Attempt: 1},
		{ID: 21, Name: "vet-and-format", Status: "failure", Attempt: 1},
		{ID: 22, Name: "security", Status: "failure", Attempt: 1},
		{ID: 23, Name: deploy, Status: "failure", Attempt: 1},
		{ID: 24, Name: "lint", Status: "success", Attempt: 1},
	}, map[string]string{
		"12/1": fixture(t, "joblog-run3840-test.log"),
		"21/1": logLines("# example.com/p", "vet: p.go:3:1: unreachable code"),
		"23/1": logLines("--- FAIL: TestDeploy (0.00s)", "FAIL\texample.com/deploy\t1s"),
	})
	fj.logStatus = map[string]int{"22/1": http.StatusInternalServerError}
	spec := requestRed(t, r, true)
	want := []FailedTest{
		{Job: "test", Test: "TestInterimPushRefusesTooLargeWorkBeforeReadingIt", Package: factorydPkg, Kind: "failed"},
		{Job: "vet-and-format", Kind: "no test failure in its log"},
		{Job: "security", Kind: "log not readable"},
		{Job: "deploy Authorization: Bearer [redacted]", Test: "TestDeploy", Package: "example.com/deploy", Kind: "failed"},
	}
	if !reflect.DeepEqual(spec.FailedTests, want) || spec.FailedTestsMore != 0 {
		t.Fatalf("entries %+v (more %d), want %+v", spec.FailedTests, spec.FailedTestsMore, want)
	}
}

// Past the cap the request says how many more are red, a red job with no
// test failure counting as one.
func TestRequestApprovalCountsTheEntriesPastTheCap(t *testing.T) {
	var lines []string
	for i := range maxFailedTests + 2 {
		lines = append(lines, fmt.Sprintf("--- FAIL: TestN%d (0.00s)", i))
	}
	lines = append(lines, "FAIL\texample.com/p\t1s")
	r, _ := redJobsRig(t, []ActionJob{
		{ID: 12, Name: "test", Status: "failure", Attempt: 1},
		{ID: 21, Name: "vet", Status: "failure", Attempt: 1},
	}, map[string]string{"12/1": logLines(lines...), "21/1": logLines("vet: x")})
	spec := requestRed(t, r, true)
	if len(spec.FailedTests) != maxFailedTests || spec.FailedTestsMore != 3 {
		t.Fatalf("%d entries, more %d; want %d and 3 (two tests and the vet job)", len(spec.FailedTests), spec.FailedTestsMore, maxFailedTests)
	}
}

// A green head's request names none: the jobs' logs are not read.
func TestRequestApprovalWithoutRedChecksNamesNoTests(t *testing.T) {
	r, fj := redJobsRig(t, []ActionJob{{ID: 12, Name: "test", Status: "failure", Attempt: 1}}, nil)
	spec := requestRed(t, r, false)
	if len(spec.FailedTests) != 0 || len(fj.logGets) != 0 {
		t.Fatalf("entries %+v, logs read %v; want none", spec.FailedTests, fj.logGets)
	}
}

// The brief writer reads the typed list and the count past its cap.
func TestBriefFactsCarryTheFailedTests(t *testing.T) {
	tests := []FailedTest{{Job: "test", Test: "TestA", Package: "p/q", Kind: "failed"}}
	spec, _ := json.Marshal(MergeSpec{RedChecks: true, FailedTests: tests, FailedTestsMore: 4})
	facts, _, err := briefFacts(ApprovalRequest{Kind: ApprovalMerge, Spec: spec}, Task{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	m := facts.(map[string]any)
	if !reflect.DeepEqual(m["failed_tests"], tests) || m["failed_tests_more"] != 4 {
		t.Fatalf("facts = %+v", m)
	}
}

// A cancelled job is red like a failed one; a log longer than the read's
// bound says so, with the tests before the cut kept and no test named from
// the line the cut falls in; a package that is a token is masked.
func TestRequestApprovalNamesCancelledCutAndTokenJobs(t *testing.T) {
	big := logLines("--- FAIL: TestBeforeTheCut (0.00s)", "FAIL\texample.com/big\t1s") +
		strings.Repeat("2026-11-10T10:48:40.1723690Z "+strings.Repeat("x", 500)+"\n", maxJobLog/530+1) +
		logLines("--- FAIL: TestPastTheCut (0.00s)", "FAIL\texample.com/big\t1s")
	r, _ := redJobsRig(t, []ActionJob{
		{ID: 31, Name: "build", Status: "cancelled", Attempt: 1},
		{ID: 32, Name: "huge", Status: "failure", Attempt: 1},
		{ID: 33, Name: "tok", Status: "failure", Attempt: 1},
	}, map[string]string{
		"31/1": logLines("Job cancelled"),
		"32/1": big,
		"33/1": logLines("--- FAIL: TestTok (0.00s)", "FAIL\tsk-abcdef0123456789abcdef0123456789"),
	})
	spec := requestRed(t, r, true)
	want := []FailedTest{
		{Job: "build", Kind: "cancelled"},
		{Job: "huge", Test: "TestBeforeTheCut", Package: "example.com/big", Kind: "failed"},
		{Job: "huge", Kind: "its log is longer than 8 MiB; failures past that are not read"},
		{Job: "tok", Test: "TestTok", Package: "[redacted]", Kind: "failed"},
	}
	if !reflect.DeepEqual(spec.FailedTests, want) {
		t.Fatalf("entries %+v, want %+v", spec.FailedTests, want)
	}
}

// The red runs share one budget: two stalled logs end together at the
// budget, long before one read's own timeout, and both jobs say their log
// could not be read in time, the lines before the stall kept.
func TestRequestApprovalOpensWhenLogsStall(t *testing.T) {
	saved := failedTestsBudget
	failedTestsBudget = 300 * time.Millisecond
	t.Cleanup(func() { failedTestsBudget = saved })
	r, fj := redJobsRig(t, []ActionJob{
		{ID: 41, Name: "test", Status: "failure", Attempt: 1},
		{ID: 42, Name: "security", Status: "failure", Attempt: 1},
	}, nil)
	fj.logStall = map[string]string{"41/1": logLines("--- FAIL: TestEarly (0.00s)", "FAIL\texample.com/p\t1s"), "42/1": ""}
	start := time.Now()
	spec := requestRed(t, r, true)
	if took := time.Since(start); took > logScanTimeout/4 {
		t.Fatalf("the request took %v with two stalled logs; the shared budget is %v", took, failedTestsBudget)
	}
	want := []FailedTest{
		{Job: "test", Test: "TestEarly", Package: "example.com/p", Kind: "failed"},
		{Job: "test", Kind: "its log could not be read in time"},
		{Job: "security", Kind: "its log could not be read in time"},
	}
	if !reflect.DeepEqual(spec.FailedTests, want) {
		t.Fatalf("entries %+v, want %+v", spec.FailedTests, want)
	}
}

// twoRunsRig is a head whose failing checks sit in runs 7 and 8, each with
// one red job; run 8 is listed unless unlisted.
func twoRunsRig(t *testing.T, unlisted bool) (*Runner, *prForgejo) {
	t.Helper()
	two := statusBody("failure",
		[4]string{"failure", "pr / test (pull_request)", "/sf/repo/actions/runs/7/jobs/0", "x"},
		[4]string{"failure", "pr / smoke (pull_request)", "/sf/repo/actions/runs/8/jobs/0", "x"})
	r, fj, _ := killedRig(t, []string{two},
		[]ActionJob{{ID: 12, Name: "test", Status: "failure", Attempt: 1}},
		[]ActionJob{{ID: 13, Name: "smoke", Status: "failure", Attempt: 1}})
	fj.wholeLogs = true
	fj.jobLogs["12/1"] = logLines("--- FAIL: TestEarly (0.00s)", "FAIL\texample.com/p\t1s")
	fj.jobLogs["13/1"] = logLines("--- FAIL: TestSmoke (0.00s)", "FAIL\texample.com/s\t1s")
	if !unlisted {
		fj.runs = append(fj.runs, ActionRun{ID: 80, Index: 8, Status: "failure"})
	}
	return r, fj
}

// A run whose jobs cannot be listed is named by its check, not dropped:
// "log not readable" for a listing that fails, "could not be read in
// time" when the budget a stalled log spent is gone.
func TestRequestApprovalNamesARunItCannotList(t *testing.T) {
	r, _ := twoRunsRig(t, true)
	spec := requestRed(t, r, true)
	want := []FailedTest{
		{Job: "test", Test: "TestEarly", Package: "example.com/p", Kind: "failed"},
		{Job: "pr / smoke (pull_request)", Kind: "log not readable"},
	}
	if !reflect.DeepEqual(spec.FailedTests, want) {
		t.Fatalf("entries %+v, want %+v", spec.FailedTests, want)
	}

	saved := failedTestsBudget
	failedTestsBudget = 300 * time.Millisecond
	t.Cleanup(func() { failedTestsBudget = saved })
	r, fj := twoRunsRig(t, false)
	fj.logStall = map[string]string{"12/1": logLines("--- FAIL: TestEarly (0.00s)", "FAIL\texample.com/p\t1s")}
	spec = requestRed(t, r, true)
	want = []FailedTest{
		{Job: "test", Test: "TestEarly", Package: "example.com/p", Kind: "failed"},
		{Job: "test", Kind: "its log could not be read in time"},
		{Job: "pr / smoke (pull_request)", Kind: "its log could not be read in time"},
	}
	if !reflect.DeepEqual(spec.FailedTests, want) {
		t.Fatalf("after a stall: entries %+v, want %+v", spec.FailedTests, want)
	}
}

// A cancelled job whose log is missing is still "cancelled".
func TestRequestApprovalNamesACancelledJobWithoutALog(t *testing.T) {
	r, fj := redJobsRig(t, []ActionJob{{ID: 51, Name: "build", Status: "cancelled", Attempt: 1}}, nil)
	fj.logStatus = map[string]int{"51/1": http.StatusNotFound}
	spec := requestRed(t, r, true)
	if want := []FailedTest{{Job: "build", Kind: "cancelled"}}; !reflect.DeepEqual(spec.FailedTests, want) {
		t.Fatalf("entries %+v, want %+v", spec.FailedTests, want)
	}
}

// A go test timeout a failing test's message echoes, indented, is no
// timeout of this job: the tests it names did not run here.
func TestFailScanIgnoresAnEchoedTimeout(t *testing.T) {
	s := scanOf(t, logLines(
		"--- FAIL: TestEchoesASandboxTimeout (0.00s)",
		"    c_test.go:9: the sandbox said:",
		"        panic: test timed out after 10m0s",
		"        \trunning tests:",
		"        \t\tTestSandboxOnly (10m0s)",
		"FAIL\texample.com/c\t0.01s",
	))
	want := []FailedTest{{Test: "TestEchoesASandboxTimeout", Package: "example.com/c", Kind: "failed"}}
	if !reflect.DeepEqual(s.out, want) {
		t.Fatalf("entries %+v, want %+v", s.out, want)
	}
}
