package factoryd

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func wrap(cases string) string {
	return `<testsuites><testsuite name="p">` + cases + `</testsuite></testsuites>`
}

func TestReadJUnit(t *testing.T) {
	for _, c := range []struct {
		name, report string
		want         proofOutcome
	}{
		{"TestA", wrap(`<testcase name="TestA"></testcase>`), proofOutcome{proofPassed, ""}},
		{"TestA", wrap(`<testcase name="TestA"><failure message="Failed">want 3, got 2</failure></testcase>`), proofOutcome{proofFailed, "Failed\nwant 3, got 2"}},
		{"TestA", wrap(`<testcase name="TestA"><error message="panic: boom"></error></testcase>`), proofOutcome{proofFailed, "panic: boom"}},
		{"TestA", wrap(`<testcase name="TestA"><skipped message="FACTORYD_TEST_DATABASE_URL not set"></skipped></testcase>`), proofOutcome{proofSkipped, "FACTORYD_TEST_DATABASE_URL not set"}},
		{"TestA", wrap(`<testcase name="TestA"><skipped message="s"/><failure message="F">x</failure></testcase>`), proofOutcome{proofFailed, "F\nx"}},
		{"TestA", wrap(`<testcase name="TestB"></testcase>`), proofOutcome{proofMissing, ""}},
		{"TestB", wrap(`<testcase classname="" name="TestMain"><failure message="Failed">FAIL&#x9;x/b [build failed]&#xA;</failure></testcase>`), proofOutcome{proofMissing, "Failed\nFAIL\tx/b [build failed]"}},
		{"TestA", wrap(`<testcase name="TestA/sub"><skipped message="s"></skipped></testcase><testcase name="TestA"></testcase>`), proofOutcome{proofPassed, ""}},
		{"", wrap(`<testcase name="TestA"></testcase><testcase name="TestB"><skipped message="no node"></skipped></testcase>`), proofOutcome{proofPassed, "1 passed, 1 skipped"}},
		{"", wrap(`<testcase name="TestA"><skipped message="no node"></skipped></testcase><testcase name="TestB"><skipped message="no db"></skipped></testcase>`), proofOutcome{proofSkipped, "no node"}},
		{"", wrap(`<testcase name="TestA"></testcase><testcase name="TestB"><failure message="Failed">boom</failure></testcase>`), proofOutcome{proofFailed, "Failed\nboom"}},
		{"", `<testsuites><testsuite name="p"></testsuite></testsuites>`, proofOutcome{proofMissing, ""}},
		{"TestA", `<testsuites><testsuite name="a"><testsuite name="b"><testcase name="TestA"></testcase></testsuite></testsuite></testsuites>`, proofOutcome{proofPassed, ""}},
		{"TestA", `<testsuite><testcase name="TestA"></testcase></testsuite>`, proofOutcome{proofPassed, ""}},
		{"TestA", wrap(`<testcase name="TestA"></testcase>`) + "\nwrote report.xml <<<", proofOutcome{proofPassed, ""}},
		{"TestA", "", proofOutcome{proofNoReport, "check test wrote an empty report"}},
		{"TestA", "  \n", proofOutcome{proofNoReport, "check test wrote an empty report"}},
	} {
		if got := readJUnit([]byte(c.report), c.name); got != c.want {
			t.Errorf("readJUnit(%q, %q) = %+v, want %+v", c.report, c.name, got, c.want)
		}
	}
	for _, report := range []string{`<testsuites><testcase name="TestA">`, "plain text, no XML"} {
		got := readJUnit([]byte(report), "TestA")
		if got.Outcome != proofNoReport || !strings.HasPrefix(got.Message, "the report does not parse: ") {
			t.Errorf("readJUnit(%q) = %+v, want no report, the report does not parse", report, got)
		}
	}
}

// gotestsumReport is gotestsum v1.13.0's report of two packages: a build
// failure, a failed, a skipped, and a passed test with a subtest.
const gotestsumReport = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="5" failures="2" errors="1" time="0.001704">
	<testsuite tests="0" failures="0" time="0.000000" name="example.com/x/b" timestamp="2026-11-03T23:02:48+01:00">
		<properties>
			<property name="go.version" value="go1.26.10 linux/amd64"></property>
		</properties>
		<testcase classname="" name="TestMain" time="0.000000">
			<failure message="Failed" type="">FAIL&#x9;example.com/x/b [build failed]&#xA;</failure>
		</testcase>
	</testsuite>
	<testsuite tests="4" failures="1" skipped="1" time="0.002000" name="example.com/x/s" timestamp="2026-11-03T23:02:48+01:00">
		<testcase classname="example.com/x/s" name="TestF" time="0.000000">
			<failure message="Failed" type="">=== RUN   TestF&#xA;    s_test.go:4: want 3, got 2&#xA;--- FAIL: TestF (0.00s)&#xA;</failure>
		</testcase>
		<testcase classname="example.com/x/s" name="TestS" time="0.000000">
			<skipped message="=== RUN   TestS&#xA;    s_test.go:3: no db&#xA;--- SKIP: TestS (0.00s)&#xA;"></skipped>
		</testcase>
		<testcase classname="example.com/x/s" name="TestP/sub" time="0.000000"></testcase>
		<testcase classname="example.com/x/s" name="TestP" time="0.000000"></testcase>
	</testsuite>
</testsuites>`

func TestReadJUnitReadsGotestsum(t *testing.T) {
	for name, want := range map[string]proofOutcome{
		"TestF": {proofFailed, "Failed\n=== RUN   TestF\n    s_test.go:4: want 3, got 2\n--- FAIL: TestF (0.00s)"},
		"TestS": {proofSkipped, "=== RUN   TestS\n    s_test.go:3: no db\n--- SKIP: TestS (0.00s)"},
		"TestP": {proofPassed, ""},
		"TestB": {proofMissing, "Failed\nFAIL\texample.com/x/b [build failed]"},
		"":      {proofFailed, "Failed\nFAIL\texample.com/x/b [build failed]"},
	} {
		if got := readJUnit([]byte(gotestsumReport), name); got != want {
			t.Errorf("readJUnit(gotestsumReport, %q) = %+v, want %+v", name, got, want)
		}
	}
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadReportUnzips(t *testing.T) {
	gz := gzipped(t, []byte(wrap(`<testcase name="TestA"></testcase>`)))
	if got, want := readReport(gz, "TestA"), (proofOutcome{proofPassed, ""}); got != want {
		t.Errorf("gzipped report = %+v, want %+v", got, want)
	}
	if got := readReport([]byte("not gzip"), "TestA"); got.Outcome != proofNoReport || !strings.HasPrefix(got.Message, "the report does not unzip: ") {
		t.Errorf("not gzip = %+v, want no report, the report does not unzip", got)
	}
	if got := readReport(gz[:len(gz)-4], "TestA"); got.Outcome != proofNoReport || !strings.HasPrefix(got.Message, "the report does not unzip: ") {
		t.Errorf("cut-off stream = %+v, want no report, the report does not unzip", got)
	}
	if got, want := readReport(gzipped(t, make([]byte, maxReportXML+1)), "TestA"), (proofOutcome{proofNoReport, "the report passes 32 MiB unzipped"}); got != want {
		t.Errorf("past the cap = %+v, want %+v", got, want)
	}
}
