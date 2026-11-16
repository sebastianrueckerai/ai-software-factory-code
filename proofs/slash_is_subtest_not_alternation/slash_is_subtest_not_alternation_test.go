package slash_is_subtest_not_alternation

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
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

// names is every testcase the report holds, its suite's classname with it.
func (r reportSuites) cases() []struct {
	Name      string
	Classname string
} {
	var out []struct {
		Name      string
		Classname string
	}
	for _, s := range r.Suites {
		for _, tc := range s.Testcases {
			out = append(out, struct {
				Name      string
				Classname string
			}{tc.Name, tc.Classname})
		}
	}
	return out
}

// runCheck runs bash check test on suite with the name the runner takes
// and returns its report's testcases, each with its suite's classname.
func runCheck(t *testing.T, suite, name string) []struct {
	Name      string
	Classname string
} {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "check")); err == nil {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				break
			}
		}
		if parent := filepath.Dir(root); parent == root {
			t.Fatalf("no repo root above %s", root)
		} else {
			root = parent
		}
	}
	report := filepath.Join(t.TempDir(), "report.xml")
	cmd := exec.Command("bash", "check", "test", suite, name)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CHECK_REPORT="+report)
	var run bytes.Buffer
	cmd.Stdout, cmd.Stderr = &run, &run
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash check test %s %s failed: %v\n%s", suite, name, err, run.String())
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the run wrote no report: %v", err)
	}
	var parsed reportSuites
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("the report is no JUnit: %v\n%s", err, body)
	}
	return parsed.cases()
}

// A name narrowed by a slash narrows within its one test — only that
// test's cases run, never another test's — and a piped name is no
// alternation: a proof entry whose name holds a pipe runs no test at
// all, so a pipe can never fan a run across many tests.
func TestSlashIsSubtestNotAlternation(t *testing.T) {
	for _, tc := range runCheck(t, "./internal/console", "TestRefusalNamesTheCount/one") {
		if tc.Name != "TestRefusalNamesTheCount" && !strings.HasPrefix(tc.Name, "TestRefusalNamesTheCount/") {
			t.Errorf("the slashed name ran another test: %+v", tc)
		}
	}
	if piped := runCheck(t, "./internal/console", "TestConfirmationLead|TestRefusalScrolls"); len(piped) != 0 {
		t.Errorf("the piped name ran %d tests, want none: %+v", len(piped), piped)
	}
}
