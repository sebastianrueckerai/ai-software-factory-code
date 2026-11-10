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

// killMarkers are what a tool killed by a signal leaves in a job's log, as
// opposed to a test that failed.
var killMarkers = regexp.MustCompile(`(?i)signal: killed|exit (?:status|code) 137\b|OOMKilled`)

// killMarker is the kill marker log shows, "" for none.
func killMarker(log []byte) string { return string(killMarkers.Find(log)) }

func killedBySignal(log []byte) bool { return killMarker(log) != "" }

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
// answers true while a run still has jobs that wait or run, or a re-run
// was just asked for. notes name, for the red verdict, each kill and the
// re-run or the missing one, and a job that failed again at its re-run.
// Anything it cannot read leaves the check red as it stands.
func (r *Runner) killedChecks(ctx context.Context, t Task, repo Repo, cs CombinedStatus, logOf func(string) ([]byte, error), reran *[]rerunNote) (unsettled bool, notes []string) {
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil || !ok {
		log.Printf("task %s: killed checks: no project for %s (%v)", t.ID, t.Repository, err)
		return false, nil
	}
	token := r.forgejo.Access(p).Actions
	prefix := "/" + repo.Owner + "/" + repo.Name + "/actions/runs/"
	seen := map[int64]bool{}
	for _, s := range cs.Statuses {
		if s.State != "failure" && s.State != "error" || !strings.HasPrefix(s.TargetURL, prefix) {
			continue
		}
		number, _, _ := strings.Cut(strings.TrimPrefix(s.TargetURL, prefix), "/")
		n, err := strconv.ParseInt(number, 10, 64)
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		jobs, run, err := r.runJobs(ctx, token, repo, n)
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
			case !doneStatus(j.Status) && (run.Status != "blocked" || j.Status != "blocked"):
				wait = true
			case j.Status == "failure" || j.Status == "cancelled":
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
func (r *Runner) runJobs(ctx context.Context, token string, repo Repo, n int64) ([]ActionJob, ActionRun, error) {
	runs, err := r.forgejo.ActionRuns(ctx, token, repo.Owner, repo.Name, url.Values{"run_number": {strconv.FormatInt(n, 10)}})
	if err != nil {
		return nil, ActionRun{}, err
	}
	i := slices.IndexFunc(runs, func(a ActionRun) bool { return a.Index == n })
	if i < 0 {
		return nil, ActionRun{}, fmt.Errorf("forgejo lists no run %d", n)
	}
	jobs, err := r.forgejo.ActionRunJobs(ctx, token, repo.Owner, repo.Name, runs[i].ID)
	return jobs, runs[i], err
}

// killedJob judges one red job: it re-runs the job when its first attempt
// was killed (asked true), and answers the note the red verdict carries.
func (r *Runner) killedJob(ctx context.Context, token string, repo Repo, run int64, j ActionJob, reran *[]rerunNote) (asked bool, note string) {
	body, err := r.forgejo.ActionJobLog(ctx, token, repo.Owner, repo.Name, j.ID, j.Attempt, mainLogRead)
	if err != nil {
		log.Printf("checks gate: log of job %s of run %d: %v", j.Name, run, err)
		return false, ""
	}
	marker := killMarker(body)
	switch {
	case j.Attempt > 1 && marker != "":
		return false, fmt.Sprintf("job %q of run %d was killed again (%s) at attempt %d, its one re-run: no second re-run follows", j.Name, run, marker, j.Attempt)
	case j.Attempt > 1:
		return false, fmt.Sprintf("job %q of run %d failed again at attempt %d, its re-run: a test failure, not a kill", j.Name, run, j.Attempt)
	case marker == "":
		return false, ""
	case slices.ContainsFunc(*reran, func(n rerunNote) bool { return n.id == j.ID }):
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
