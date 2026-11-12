package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// killMarkers match a line a tool killed by a signal ends on: the marker
// alone or after unindented prefixes such as the package and the tool
// ("pkg: /usr/local/go/pkg/tool/linux_amd64/vet: signal: killed"). A
// test's own output is indented, so it is not one. The runner's own
// step-exit line counts too: a killed top process leaves no other.
var killMarkers = regexp.MustCompile(`(?i)^(?:` + regexp.QuoteMeta(runnerLogPrefix) + ` RUN )?(?:\S+: )*(signal: killed|exit (?:status|code) 137|OOMKilled)\s*$`)

// killMarker is the kill marker a line of log shows, "" for none.
func killMarker(log []byte) string {
	for _, line := range strings.Split(string(log), "\n") {
		if m := killMarkers.FindStringSubmatch(stripLogTimestamp(strings.TrimRight(line, "\r"))); m != nil {
			return m[1]
		}
	}
	return ""
}

func killedBySignal(log []byte) bool { return killMarker(log) != "" }

// runnerDiedMarker is what a red job's note names when its log ends cut.
const runnerDiedMarker = "the runner died: its log holds no closing line and no FAIL line"

var (
	closingLine = regexp.MustCompile(`^[^\w\s]*\s*Job (?:succeeded|failed)\s*$`)
	failLine    = regexp.MustCompile(`^(?:--- )?FAIL\b`)
)

// runnerDied reports whether a red job's log ends cut: a runner that ran
// to its end writes a closing line, "Job failed" or "Job succeeded", and a
// failing go test writes a FAIL line; a pod killed, evicted or lost mid-job
// leaves neither.
func runnerDied(log []byte) bool {
	for _, line := range strings.Split(string(log), "\n") {
		line = stripLogTimestamp(strings.TrimRight(line, "\r"))
		if closingLine.MatchString(line) || failLine.MatchString(line) {
			return false
		}
	}
	return true
}

// rerunNote is one re-run the checks gate asked of Forgejo: the job, its
// run's number and the attempt the re-run is. id keeps the gate from asking
// for the same job twice while Forgejo still lists its first attempt.
type rerunNote struct {
	Job     string `json:"job"`
	Run     int64  `json:"run"`
	Attempt int64  `json:"attempt"`
	id      int64
}

// killedChecks reads the runs of the failing checks of cs. A job killed at
// its first attempt is re-run once (the attempt is the once-marker: a job
// at attempt 2 or up is never re-run, whoever re-ran it first); unsettled
// answers true while a re-run is queued or running, or one was just asked
// for; a sibling job that merely runs holds nothing back. notes name, for the red verdict, each kill and the
// re-run or the missing one, and a job that failed again at its re-run.
// Anything it cannot read leaves the check red as it stands.
func (r *Runner) killedChecks(ctx context.Context, t Task, repo Repo, cs CombinedStatus, logOf func(string) ([]byte, error), reran *[]rerunNote) (unsettled bool, notes []string) {
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil || !ok {
		log.Printf("task %s: killed checks: no project for %s (%v)", t.ID, t.Repository, err)
		return false, nil
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
			if marker := killMarkerOf(logOf, s.TargetURL); marker != "" {
				notes = append(notes, fmt.Sprintf("check %q was killed (%s) and no re-run was offered: run %d is not readable (%s)",
					s.Context, marker, n, r.Scrub.String(err.Error())))
			}
			continue
		}
		wait := false
		var red []ActionJob
		for _, j := range jobs {
			switch {
			case !doneStatus(j.Status):
				wait = wait || rerunning(j, *reran)
			case j.Status == "failure":
				red = append(red, j)
			}
		}
		if wait {
			unsettled = true
			continue
		}
		for _, j := range red {
			done, note := r.killedJob(ctx, token, repo, n, j, reran)
			unsettled = unsettled || done
			if note != "" {
				notes = append(notes, note)
			}
		}
	}
	return unsettled, notes
}

// runNumberOf is the number of the actions run of repo that failing status
// s points at.
func runNumberOf(s CommitStatus, repo Repo) (int64, bool) {
	prefix := "/" + repo.Owner + "/" + repo.Name + "/actions/runs/"
	if s.State != "failure" && s.State != "error" || !strings.HasPrefix(s.TargetURL, prefix) {
		return 0, false
	}
	number, _, _ := strings.Cut(strings.TrimPrefix(s.TargetURL, prefix), "/")
	n, err := strconv.ParseInt(number, 10, 64)
	return n, err == nil
}

// rerunning is whether j, not done, is a re-run in progress: past its first
// attempt, or one this gate asked for.
func rerunning(j ActionJob, reran []rerunNote) bool {
	return j.Attempt > 1 || slices.ContainsFunc(reran, func(n rerunNote) bool { return n.id == j.ID })
}

func killMarkerOf(logOf func(string) ([]byte, error), target string) string {
	if logOf == nil {
		return ""
	}
	body, err := logOf(target)
	if err != nil {
		return ""
	}
	return killMarker(body)
}

// runJobs lists the jobs of run number n.
func (r *Runner) runJobs(ctx context.Context, token string, repo Repo, n int64) ([]ActionJob, error) {
	runs, err := r.forgejo.ActionRuns(ctx, token, repo.Owner, repo.Name, url.Values{"run_number": {strconv.FormatInt(n, 10)}})
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(runs, func(a ActionRun) bool { return a.Index == n })
	if i < 0 {
		return nil, fmt.Errorf("forgejo lists no run %d", n)
	}
	return r.forgejo.ActionRunJobs(ctx, token, repo.Owner, repo.Name, runs[i].ID)
}

// killedJob judges one red job: it re-runs the job when its first attempt
// was killed or its runner died (asked true), and answers the note the red verdict carries.
func (r *Runner) killedJob(ctx context.Context, token string, repo Repo, run int64, j ActionJob, reran *[]rerunNote) (asked bool, note string) {
	body, err := r.forgejo.ActionJobLog(ctx, token, repo.Owner, repo.Name, j.ID, j.Attempt, mainLogRead)
	if err != nil {
		log.Printf("checks gate: log of job %s of run %d: %v", j.Name, run, err)
		return false, ""
	}
	marker := killMarker(body)
	if marker == "" && runnerDied(body) {
		marker = runnerDiedMarker
	}
	ours := slices.ContainsFunc(*reran, func(n rerunNote) bool { return n.id == j.ID })
	switch {
	case j.Attempt > 1 && ours && marker != "":
		return false, fmt.Sprintf("job %q of run %d was killed again (%s) at attempt %d, its one re-run: no second re-run follows", j.Name, run, marker, j.Attempt)
	case j.Attempt > 1 && ours:
		return false, fmt.Sprintf("job %q of run %d failed again at attempt %d, its re-run: a test failure, not a kill", j.Name, run, j.Attempt)
	case j.Attempt > 1 || marker == "":
		return false, ""
	case ours:
		return true, ""
	}
	err = r.forgejo.RerunActionJob(ctx, token, repo.Owner, repo.Name, j.ID)
	switch {
	case errors.Is(err, ErrNoRerun):
		return false, fmt.Sprintf("job %q of run %d was killed (%s) and no re-run was offered: forgejo has no rerun API, or the project's Actions token lacks the right", j.Name, run, marker)
	case err != nil:
		return false, fmt.Sprintf("job %q of run %d was killed (%s) and its re-run failed (%s)", j.Name, run, marker, oneLine(r.Scrub.String(err.Error())))
	}
	log.Printf("checks gate: job %s of run %d was killed (%s): re-run once", j.Name, run, marker)
	*reran = append(*reran, rerunNote{Job: j.Name, Run: run, Attempt: j.Attempt + 1, id: j.ID})
	return true, ""
}
