package targeted_run_writes_one_report

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type reportSuites struct {
	Suites []struct {
		Name      string `xml:"name,attr"`
		Testcases []struct {
			Name      string `xml:"name,attr"`
			Classname string `xml:"classname,attr"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "check")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no repo root above %s", dir)
		}
		dir = parent
	}
}

// The targeted run runs the one named test beside a throwaway Postgres
// and writes the JUnit report the criteria gate's runner reads to
// $CHECK_REPORT: the run exits 0, the throwaway Postgres serves it from
// its own directory and the directory is gone after, and the report
// holds the named test's one testcase and no test of any other suite.
func TestTargetedRunWritesOneReport(t *testing.T) {
	root := repoRoot(t)
	report := filepath.Join(t.TempDir(), "nested.xml")
	cmd := exec.Command("bash", "check", "test", "./internal/console", "TestConfirmationLead")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CHECK_REPORT="+report)
	var run bytes.Buffer
	cmd.Stdout, cmd.Stderr = &run, &run
	if err := cmd.Run(); err != nil {
		t.Fatalf("the targeted run failed: %v\n%s", err, run.String())
	}
	served := regexp.MustCompile(`pg-test\.sh: postgres \(PostgreSQL\) .* on (\S+)`).FindStringSubmatch(run.String())
	if served == nil {
		t.Fatalf("the run names no throwaway Postgres:\n%s", run.String())
	}
	if _, err := os.Stat(served[1]); !os.IsNotExist(err) {
		t.Errorf("the throwaway Postgres home %s outlives the run: %v", served[1], err)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the run wrote no report to $CHECK_REPORT: %v", err)
	}
	var parsed reportSuites
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("the report is no JUnit the runner reads: %v\n%s", err, body)
	}
	var cases []string
	for _, s := range parsed.Suites {
		for _, tc := range s.Testcases {
			cases = append(cases, tc.Name+" in "+tc.Classname)
			if !strings.HasSuffix(tc.Classname, "/internal/console") {
				t.Errorf("the report holds a test of another suite: %+v", tc)
			}
		}
	}
	if len(cases) != 1 || !strings.HasPrefix(cases[0], "TestConfirmationLead in ") {
		t.Errorf("the report holds %d testcases, want the one TestConfirmationLead: %+v", len(cases), cases)
	}
}
