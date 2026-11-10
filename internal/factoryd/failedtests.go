package factoryd

import (
	"context"
	"log"
	"regexp"
	"slices"
	"strings"
)

// FailedTest is one failing test of a red job, as go test's log names it
// and the package whose FAIL line closed it. Test is empty for a package
// that failed without a test (a build failure); Package is empty for a
// test whose package line the log's cut took.
type FailedTest struct {
	Test    string `json:"test,omitempty"`
	Package string `json:"package,omitempty"`
}

// String is the test as a human reads it: "Test (package)".
func (f FailedTest) String() string {
	switch {
	case f.Test == "":
		return f.Package + " (no test ran)"
	case f.Package == "":
		return f.Test
	}
	return f.Test + " (" + f.Package + ")"
}

// maxFailedTests bounds what a request carries of one red head.
const maxFailedTests = 50

var (
	failedTestLine = regexp.MustCompile(`^--- FAIL: (\S+)`)
	failedPkgLine  = regexp.MustCompile(`^FAIL\s+(\S+)`)
)

// parseFailedTests reads go test's log: each unindented "--- FAIL: Name"
// line is a failing test, which the next "FAIL<TAB>package" line puts in
// its package; subtests, indented, stay under their parent.
func parseFailedTests(log []byte) []FailedTest {
	var out []FailedTest
	open := 0 // the tests still without their package line
	for _, line := range strings.Split(string(log), "\n") {
		line = stripLogTimestamp(strings.TrimRight(line, "\r"))
		if m := failedTestLine.FindStringSubmatch(line); m != nil {
			out = append(out, FailedTest{Test: m[1]})
			open++
		} else if m := failedPkgLine.FindStringSubmatch(line); m != nil {
			for i := len(out) - open; i < len(out); i++ {
				out[i].Package = m[1]
			}
			if open == 0 {
				out = append(out, FailedTest{Package: m[1]})
			}
			open = 0
		}
	}
	return out
}

// failedTests are the failing tests of the red jobs of sha's failing
// checks, read from their logs through Forgejo's job log API and scrubbed
// before they are parsed. Anything it cannot read logs and leaves out: a
// request opens without the list rather than not at all.
func (r *Runner) failedTests(ctx context.Context, t Task, repo Repo, sha string) []FailedTest {
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil || !ok {
		log.Printf("task %s: failed tests: no project for %s (%v)", t.ID, t.Repository, err)
		return nil
	}
	cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
	if err != nil {
		log.Printf("task %s: failed tests: status of %s: %v", t.ID, sha, err)
		return nil
	}
	token := r.forgejo.Access(p).Actions
	var out []FailedTest
	seen := map[int64]bool{}
	for _, s := range cs.Statuses {
		n, ok := runNumberOf(s, repo)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		jobs, err := r.runJobs(ctx, token, repo, n)
		if err != nil {
			log.Printf("task %s: failed tests: run %d: %v", t.ID, n, err)
			continue
		}
		for _, j := range jobs {
			if j.Status != "failure" {
				continue
			}
			body, err := r.forgejo.ActionJobLog(ctx, token, repo.Owner, repo.Name, j.ID, j.Attempt, mainLogRead)
			if err != nil {
				log.Printf("task %s: failed tests: log of job %s of run %d: %v", t.ID, j.Name, n, err)
				continue
			}
			for _, f := range parseFailedTests([]byte(r.Scrub.String(string(body)))) {
				if !slices.Contains(out, f) {
					out = append(out, f)
				}
			}
		}
	}
	return out[:min(len(out), maxFailedTests)]
}
