package factoryd

import (
	"context"
	"errors"
	"log"
	"net"
	"regexp"
	"strings"
	"time"
)

// What a red job's log says is red, one entry each. Kind is one of the
// failKind constants, or "timed out after <duration>" for a test go test's
// own timeout killed.
type FailedTest struct {
	Job     string `json:"job,omitempty"`
	Test    string `json:"test,omitempty"`
	Package string `json:"package,omitempty"`
	Kind    string `json:"kind"`
}

const (
	failKindFailed     = "failed"
	failKindNoTestRan  = "no test ran"
	failKindNoneNamed  = "no test named"
	failKindNoneInLog  = "no test failure in its log"
	failKindLogMissing = "log not readable"
	failKindCancelled  = "cancelled"
	failKindLogCut     = "its log is longer than 8 MiB; failures past that are not read"
	failKindLogSlow    = "its log could not be read in time"
)

// failedTestsBudget bounds every log read of one request together, well
// inside the activity's own time: a stalled read costs the list, never the
// request.
var failedTestsBudget = 45 * time.Second

// String is the entry as a human reads it.
func (f FailedTest) String() string {
	subject := "job " + f.Job
	switch {
	case f.Test != "" && f.Package != "":
		subject = f.Test + " (" + f.Package + ")"
	case f.Test != "":
		subject = f.Test
	case f.Package != "":
		subject = "package " + f.Package
	}
	s := subject + ": " + f.Kind
	if f.Job != "" && (f.Test != "" || f.Package != "") {
		s += " (job " + f.Job + ")"
	}
	return s
}

const (
	// maxFailedTests bounds the entries a request carries; the rest is
	// counted in MergeSpec.FailedTestsMore.
	maxFailedTests = 50
	// maxJobLog bounds what is read of one job's log.
	maxJobLog = 8 << 20
	// maxPendingTests bounds the tests waiting for their package line.
	maxPendingTests = 1000
)

// Names are checked against the grammar of Go identifiers and package
// paths, so no free text of a log reaches a reader through them.
var (
	testNameRe    = regexp.MustCompile(`^(?:Test|Fuzz|Example)[A-Za-z0-9_]*$`)
	packagePathRe = regexp.MustCompile(`^[A-Za-z0-9._~/-]+$`)
	durationRe    = regexp.MustCompile(`^[0-9.]+(?:ns|µs|ms|s|m|h)(?:[0-9.]+(?:ns|µs|ms|s|m|h))*$`)

	failedTestLine = regexp.MustCompile(`^--- FAIL: (\S+)`)
	failedPkgLine  = regexp.MustCompile(`^FAIL\s+(\S+)(.*)$`)
	timeoutLine    = regexp.MustCompile(`^panic: test timed out after (\S+)`)
	runningLine    = regexp.MustCompile(`^\s+(\S+) \(`)
)

// failScan reads go test's log line by line: each unindented "--- FAIL:
// Name" line is a failing test, and each test of the "running tests:" block
// of go test's own timeout one the timeout killed; the next "FAIL<TAB>
// package" line puts them in their package. Subtests, indented or after a
// slash, stay under their parent.
type failScan struct {
	out     []FailedTest
	more    int
	pending []FailedTest
	seen    map[FailedTest]bool
	timeout string // the duration of a timeout panic not yet closed by its package line
	running bool   // inside the "running tests:" block
}

func newFailScan() *failScan { return &failScan{seen: map[FailedTest]bool{}} }

func (s *failScan) add(f FailedTest) {
	if s.seen[f] {
		return
	}
	s.seen[f] = true
	if len(s.out) < maxFailedTests {
		s.out = append(s.out, f)
	} else {
		s.more++
	}
}

func (s *failScan) line(raw string) {
	line := stripLogTimestamp(strings.TrimRight(raw, "\r\n"))
	if s.running {
		if m := runningLine.FindStringSubmatch(line); m != nil {
			name, _, _ := strings.Cut(m[1], "/")
			if testNameRe.MatchString(name) {
				s.hold(FailedTest{Test: name, Kind: "timed out after " + s.timeout})
			}
			return
		}
		s.running = false
	}
	switch trimmed := strings.TrimSpace(line); {
	case timeoutLine.MatchString(trimmed):
		if d := timeoutLine.FindStringSubmatch(trimmed)[1]; durationRe.MatchString(d) {
			s.timeout = d
		}
	case s.timeout != "" && trimmed == "running tests:":
		s.running = true
	case failedTestLine.MatchString(line):
		if name := failedTestLine.FindStringSubmatch(line)[1]; testNameRe.MatchString(name) {
			s.hold(FailedTest{Test: name, Kind: failKindFailed})
		}
	case failedPkgLine.MatchString(line):
		m := failedPkgLine.FindStringSubmatch(line)
		pkg := ""
		if packagePathRe.MatchString(m[1]) {
			pkg = m[1]
		}
		s.closePackage(pkg, m[2])
	}
}

// hold keeps f until its package line; past maxPendingTests it is counted.
func (s *failScan) hold(f FailedTest) {
	if len(s.pending) >= maxPendingTests {
		s.more++
		return
	}
	s.pending = append(s.pending, f)
}

// closePackage puts the pending tests in pkg; with none pending the
// package is the entry.
func (s *failScan) closePackage(pkg, rest string) {
	for _, f := range s.pending {
		f.Package = pkg
		s.add(f)
	}
	if len(s.pending) == 0 && pkg != "" {
		kind := failKindNoneNamed
		switch {
		case s.timeout != "":
			kind = "timed out after " + s.timeout
		case strings.Contains(rest, "[build failed]"), strings.Contains(rest, "[setup failed]"):
			kind = failKindNoTestRan
		}
		s.add(FailedTest{Package: pkg, Kind: kind})
	}
	s.pending, s.timeout, s.running = nil, "", false
}

// end closes the log: tests whose package line never came keep an empty
// package, which only a log that ends inside its package has.
func (s *failScan) end() {
	for _, f := range s.pending {
		s.add(f)
	}
	s.pending = nil
}

// failedTests are the entries of the red jobs of sha's failing checks, read
// from their whole logs through Forgejo's job log API: every red job
// appears, naming the tests its log shows failing, or saying why its log
// shows none: it has none, it is cancelled, it is too long, it could not be
// read (in time). more counts the entries past maxFailedTests. Any other
// failure logs and leaves the list out: a request opens without it rather
// than not at all. Job names and packages are the free text kept, and go
// through the scrubber.
func (r *Runner) failedTests(ctx context.Context, t Task, repo Repo, sha string) (out []FailedTest, more int) {
	ctx, cancel := context.WithTimeout(ctx, failedTestsBudget)
	defer cancel()
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil || !ok {
		log.Printf("task %s: failed tests: no project for %s (%v)", t.ID, t.Repository, err)
		return nil, 0
	}
	cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
	if err != nil {
		log.Printf("task %s: failed tests: status of %s: %v", t.ID, sha, err)
		return nil, 0
	}
	token := r.forgejo.Access(p).Actions
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
			if j.Status != "failure" && j.Status != "cancelled" {
				continue
			}
			name := r.Scrub.String(j.Name)
			scan := newFailScan()
			err := r.forgejo.ScanActionJobLog(ctx, token, repo.Owner, repo.Name, j.ID, j.Attempt, maxJobLog, scan.line)
			scan.end()
			for _, f := range scan.out {
				f.Job, f.Package = name, r.Scrub.String(f.Package)
				out, more = appendFailed(out, more, f)
			}
			more += scan.more
			kind := ""
			switch {
			case err == nil && len(scan.out) == 0 && scan.more == 0 && j.Status == "cancelled":
				kind = failKindCancelled
			case err == nil && len(scan.out) == 0 && scan.more == 0:
				kind = failKindNoneInLog
			case errors.Is(err, ErrLogCut):
				kind = failKindLogCut
			case err != nil && timedOut(err):
				kind = failKindLogSlow
			case err != nil:
				kind = failKindLogMissing
			}
			if err != nil {
				log.Printf("task %s: failed tests: log of job %s of run %d: %v", t.ID, j.Name, n, err)
			}
			if kind != "" {
				out, more = appendFailed(out, more, FailedTest{Job: name, Kind: kind})
			}
		}
	}
	return out, more
}

func timedOut(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

func appendFailed(out []FailedTest, more int, f FailedTest) ([]FailedTest, int) {
	if len(out) >= maxFailedTests {
		return out, more + 1
	}
	return append(out, f), more
}
