package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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

// ciPlatformReruns bounds the re-runs of a killed job whose pod the
// platform ended: each attempt up to it is re-run.
const ciPlatformReruns = 2

// runnerOpening matches the line forgejo-runner 13 opens a job's log
// with, which names the runner by its registration's UUID: "Runner <uuid>
// (version:<v>) received task <n> of job <job>, triggered by event: <e>".
var runnerOpening = regexp.MustCompile(`^(?:` + regexp.QuoteMeta(runnerLogPrefix) + ` )?Runner ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) \(version:[^)]*\) received task `)

// logHeadRead bounds the read of a long log's head for its opening line.
const logHeadRead = 4 << 10

// The checks gate waits up to ciRecordWait for the death record of a
// killed job's runner pod, which the CI spawner writes within a poll of
// the pod's end, asking every ciRecordPoll.
const (
	ciRecordWait = 30 * time.Second
	ciRecordPoll = 5 * time.Second
)

// runnerUUIDOfLog is the UUID of the runner a job log's first line opens
// it with, "" when that line is no runner's opening: a job's own output
// comes after it and names no runner.
func runnerUUIDOfLog(log []byte) string {
	first, _, _ := bytes.Cut(log, []byte("\n"))
	if m := runnerOpening.FindStringSubmatch(stripLogTimestamp(strings.TrimRight(string(first), "\r"))); m != nil {
		return m[1]
	}
	return ""
}

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
// for the same attempt twice while Forgejo still lists it.
type rerunNote struct {
	Job     string `json:"job"`
	Run     int64  `json:"run"`
	Attempt int64  `json:"attempt"`
	id      int64
}

// killedChecks reads the runs of the failing checks of cs. A killed job is
// re-run by the cause its runner pod's death record names: one whose pod
// the platform ended at each attempt up to ciPlatformReruns, one its own
// pod's limit killed never, and one without that cause at its first
// attempt only (the attempt is the once-marker, whoever re-ran it); unsettled
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
	// the jobs of one pass share the wait for their records
	recordBy := time.Now().Add(r.ciDeathWait)
	for _, s := range cs.Statuses {
		n, ok := runNumberOf(s, repo)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		jobs, err := r.runJobs(ctx, token, repo, n)
		if err != nil {
			if marker := killMarkerOf(logOf, s.TargetURL); marker != "" {
				notes = append(notes, fmt.Sprintf("check %q was killed (%s) and no re-run was offered: CI run %d is not readable (%s)",
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
			done, note := r.killedJob(ctx, token, repo, n, j, recordBy, reran)
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
// was killed or its runner died (asked true), and answers the note the red
// verdict carries. The death of the pod that ran the attempt, waited for
// until recordBy, names the kill's cause: the platform's is re-run up to
// attempt ciPlatformReruns, the pod's own limit is not re-run.
func (r *Runner) killedJob(ctx context.Context, token string, repo Repo, run int64, j ActionJob, recordBy time.Time, reran *[]rerunNote) (asked bool, note string) {
	if slices.ContainsFunc(*reran, func(n rerunNote) bool { return n.id == j.ID && n.Attempt > j.Attempt }) {
		return true, "" // asked; Forgejo still lists the attempt
	}
	body, err := r.forgejo.ActionJobLog(ctx, token, repo.Owner, repo.Name, j.ID, j.Attempt, mainLogRead)
	if err != nil {
		log.Printf("checks gate: log of job %s of run %d: %v", j.Name, run, err)
		return false, ""
	}
	marker := killMarker(body)
	if marker == "" && runnerDied(body) {
		marker = runnerDiedMarker
	}
	if marker != "" {
		switch death := r.jobDeath(ctx, token, repo, j, body, recordBy); death.Class {
		case DeathPlatform:
			if j.Attempt > ciPlatformReruns {
				return false, fmt.Sprintf("job %q of CI run %d: the platform ended its pod again (%s) at attempt %d: no further re-run follows", j.Name, run, death.Short(), j.Attempt)
			}
			return r.rerunJob(ctx, token, repo, run, j, marker, fmt.Sprintf("job %q of CI run %d: the platform ended its pod (%s), so a re-run follows", j.Name, run, death.Short()), reran)
		case DeathOwnLimit:
			return false, fmt.Sprintf("job %q of CI run %d was killed (%s): %s, which is the job's own use, so no re-run follows", j.Name, run, marker, ownLimit(death))
		}
	}
	ours := slices.ContainsFunc(*reran, func(n rerunNote) bool { return n.id == j.ID })
	switch {
	case j.Attempt > 1 && ours && marker != "":
		return false, fmt.Sprintf("job %q of CI run %d was killed again (%s) at attempt %d, its one re-run: no second re-run follows", j.Name, run, marker, j.Attempt)
	case j.Attempt > 1 && ours:
		return false, fmt.Sprintf("job %q of CI run %d failed again at attempt %d, its re-run: a test failure, not a kill", j.Name, run, j.Attempt)
	case j.Attempt > 1 || marker == "":
		return false, ""
	}
	return r.rerunJob(ctx, token, repo, run, j, marker, "", reran)
}

// ownLimit is how an own_limit death reads in a job's note: Short's words
// with the limit's value when the record holds one.
func ownLimit(d DeathRecord) string {
	switch {
	case d.Limit != "" && d.Rule == RuleMemoryLimit:
		return "the pod reached its " + quantity(d.Limit) + " memory limit"
	case d.Limit != "" && d.Rule == RuleStorageLimit:
		return "the pod passed its " + quantity(d.Limit) + " storage limit"
	}
	return d.Short()
}

// rerunJob asks Forgejo to re-run killed job j and answers note with the
// ask.
func (r *Runner) rerunJob(ctx context.Context, token string, repo Repo, run int64, j ActionJob, marker, note string, reran *[]rerunNote) (asked bool, _ string) {
	err := r.forgejo.RerunActionJob(ctx, token, repo.Owner, repo.Name, j.ID)
	switch {
	case errors.Is(err, ErrNoRerun):
		return false, fmt.Sprintf("job %q of CI run %d was killed (%s) and no re-run was offered: forgejo has no rerun API, or the project's Actions token lacks the right", j.Name, run, marker)
	case err != nil:
		return false, fmt.Sprintf("job %q of CI run %d was killed (%s) and its re-run failed (%s)", j.Name, run, marker, oneLine(r.Scrub.String(err.Error())))
	}
	log.Printf("checks gate: %s", cmp.Or(note, fmt.Sprintf("job %s of run %d was killed (%s): re-run once", j.Name, run, marker)))
	*reran = append(*reran, rerunNote{Job: j.Name, Run: run, Attempt: j.Attempt + 1, id: j.ID})
	return true, note
}

// jobDeath is the death record of the runner pod that ran j's attempt, by
// the runner UUID its log's first line names: body is the log's tail, a
// whole log when shorter than the read, else its head is read for the
// line. A record not written yet is asked for every ciDeathPoll until
// recordBy. A zero record when there is none.
func (r *Runner) jobDeath(ctx context.Context, token string, repo Repo, j ActionJob, body []byte, recordBy time.Time) DeathRecord {
	if r.cfg.Cluster == nil {
		return DeathRecord{}
	}
	head := body
	if len(body) >= mainLogRead {
		var b bytes.Buffer
		err := r.forgejo.ScanActionJobLog(ctx, token, repo.Owner, repo.Name, j.ID, j.Attempt, logHeadRead, func(l string) { b.WriteString(l) })
		if err != nil && !errors.Is(err, ErrLogCut) {
			log.Printf("checks gate: head of the log of job %s: %v", j.Name, err)
		}
		head = b.Bytes()
	}
	runner := runnerUUIDOfLog(head)
	if runner == "" {
		return DeathRecord{}
	}
	for {
		death, ok, err := r.cfg.Cluster.CIDeath(ctx, runner)
		switch {
		case err != nil:
			log.Printf("checks gate: death of runner %s: %v", runner, err)
			return DeathRecord{}
		case ok || !time.Now().Before(recordBy):
			return death
		}
		select {
		case <-ctx.Done():
			return DeathRecord{}
		case <-time.After(min(r.ciDeathPoll, time.Until(recordBy))):
		}
	}
}
