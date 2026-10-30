package factoryd

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"

	"gopkg.in/yaml.v3"
)

// The fakeStore's roll requests, as PGStore keeps them.

func (s *fakeStore) RollRequestSeen(ctx context.Context, sha string) (bool, error) {
	return slices.ContainsFunc(s.rolls, func(r RollRequest) bool { return r.SHA == sha }), nil
}

func (s *fakeStore) RollRuns(ctx context.Context, repository string) (first, carried int64, err error) {
	for _, r := range s.rolls {
		if r.Repository != repository {
			continue
		}
		if first == 0 || r.RunID < first {
			first = r.RunID
		}
		if r.State == RollOpened || r.State == RollMerged {
			carried = max(carried, r.RunID)
		}
	}
	return first, carried, nil
}

func (s *fakeStore) OpenRollTask(ctx context.Context, repository string) (Task, bool, error) {
	var open RollRequest
	for _, r := range s.rolls {
		if t, ok := s.tasks[r.TaskID]; ok && r.Repository == repository && r.State == RollOpened && !Final(t.Status) && r.RunID > open.RunID {
			open = r
		}
	}
	if open.TaskID == "" {
		return Task{}, false, nil
	}
	return s.tasks[open.TaskID], true, nil
}

func (s *fakeStore) InsertRollRequest(ctx context.Context, r RollRequest) error {
	if seen, _ := s.RollRequestSeen(ctx, r.SHA); seen {
		return fmt.Errorf("roll request %s exists", r.SHA)
	}
	s.rolls = append(s.rolls, r)
	return nil
}

func (s *fakeStore) DeleteRollRequest(ctx context.Context, sha string) error {
	s.rolls = slices.DeleteFunc(s.rolls, func(r RollRequest) bool { return r.SHA == sha })
	return nil
}

// SettleRollRequests settles the fake's open requests the way the store
// does: a done task's committed request merged, its never-committed ones
// forgotten, any other ended task's request failed, a request without its
// task forgotten. It returns what it settled.
func (s *fakeStore) SettleRollRequests(ctx context.Context) ([]RollRequest, error) {
	var settled []RollRequest
	var gone []string
	for _, r := range s.rolls {
		if r.State != RollOpened {
			continue
		}
		t, ok := s.tasks[r.TaskID]
		if !ok || (t.Status == "done" && !r.Committed) {
			gone = append(gone, r.SHA)
			continue
		}
		if !slices.Contains(FinalStatuses, t.Status) {
			continue
		}
		if t.Status == "done" {
			r.State = RollMerged
		} else {
			r.State = RollFailed
		}
		s.UpdateRollRequest(ctx, r)
		settled = append(settled, r)
	}
	for _, sha := range gone {
		s.DeleteRollRequest(ctx, sha)
	}
	return settled, nil
}

func (s *fakeStore) UpdateRollRequest(ctx context.Context, r RollRequest) {
	for i, old := range s.rolls {
		if old.SHA == r.SHA {
			s.rolls[i] = r
			return
		}
	}
}

func (s *fakeStore) LastMergedRoll(ctx context.Context, repository string) (string, bool, error) {
	var last RollRequest
	for _, r := range s.rolls {
		if r.Repository == repository && r.State == RollMerged && r.RunID > last.RunID {
			last = r
		}
	}
	return last.SHA, last.SHA != "", nil
}

// FailedRolls mirrors the store's read: the failures since repository's
// newest merged roll, oldest first.
func (s *fakeStore) FailedRolls(ctx context.Context, repository string) ([]RollRequest, error) {
	merged := int64(0)
	for _, r := range s.rolls {
		if r.Repository == repository && r.State == RollMerged && r.RunID > merged {
			merged = r.RunID
		}
	}
	var out []RollRequest
	for _, r := range s.rolls {
		if r.Repository == repository && r.State == RollFailed && r.RunID > merged {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b RollRequest) int { return cmp.Compare(a.RunID, b.RunID) })
	return out, nil
}

func (s *fakeStore) StandingQuestion(ctx context.Context, repository, marker string) (Task, bool, error) {
	for _, t := range s.tasks {
		if t.Repository == repository && t.Kind == "human" && t.Status == "waiting-human" && strings.Contains(t.Objective, marker) {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

// TaskRolls are task's roll requests, oldest first: an update writes them
// all again, on main's head (roadmap 279).
func (s *fakeStore) TaskRolls(ctx context.Context, taskID string) ([]RollRequest, error) {
	var out []RollRequest
	for _, r := range s.rolls {
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b RollRequest) int { return int(a.RunID - b.RunID) })
	return out, nil
}

func (s *fakeStore) UncommittedRolls(ctx context.Context, taskID string) ([]RollRequest, error) {
	reqs, err := s.TaskRolls(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(reqs, func(r RollRequest) bool { return r.Committed }), nil
}

func (s *fakeStore) MarkRollsCommitted(ctx context.Context, taskID string, shas []string, head string) error {
	for i, r := range s.rolls {
		if r.TaskID == taskID && slices.Contains(shas, r.SHA) {
			s.rolls[i].Committed, s.rolls[i].Head = true, head
		}
	}
	return nil
}

func (s *fakeStore) RollHead(ctx context.Context, taskID string) (string, error) {
	var last RollRequest
	for _, r := range s.rolls {
		if r.TaskID == taskID && r.Committed && r.RunID > last.RunID {
			last = r
		}
	}
	return last.Head, nil
}

func (s *fakeStore) RebaseRolls(ctx context.Context, taskID, head string) (bool, error) {
	recorded := slices.ContainsFunc(s.rolls, func(r RollRequest) bool {
		return r.TaskID == taskID && r.Committed && r.Head == head
	})
	if recorded {
		for i, r := range s.rolls {
			if r.TaskID == taskID && r.Committed && r.Head != head {
				s.rolls[i].Committed, s.rolls[i].Head = false, ""
			}
		}
	}
	return recorded, nil
}

func (s *fakeStore) RollTaskPRs(ctx context.Context, repository string) ([]int64, error) {
	var out []int64
	for _, t := range s.tasks {
		if t.Source == SourceRoll && t.Repository == repository && t.PRNumber != 0 {
			out = append(out, t.PRNumber)
		}
	}
	return out, nil
}

func (s *fakeStore) SetTaskSHA(ctx context.Context, id, sha string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.SHA = sha
	s.tasks[id] = t
	return nil
}

// imageIndex is an index as main CI pushes it: the image manifest
// platform and an attestation, whose build timestamps differ per build.
func imageIndex(platform, attestation string) string {
	return strings.NewReplacer("IMG", platform, "sha256:ATT", attestation).Replace(testIndexTemplate)
}

// Two builds of each image: old (pinned on main) and new, and a rebuild of
// each with no input change (the same image manifest, a new attestation).
var (
	gwOld, gwNew         = sha256Of(`{"img":"gw-old"}`), sha256Of(`{"img":"gw-new"}`)
	workerOld, workerNew = sha256Of(`{"img":"worker-old"}`), sha256Of(`{"img":"worker-new"}`)
	runnerImg            = sha256Of(`{"img":"runner"}`)

	gwOldIdx       = imageIndex(gwOld, "sha256:a1")
	gwNewIdx       = imageIndex(gwNew, "sha256:a2")
	gwNewRebuilt   = imageIndex(gwNew, "sha256:a3")
	workerOldIdx   = imageIndex(workerOld, "sha256:a4")
	workerRebuilt  = imageIndex(workerOld, "sha256:a5")
	workerNewIdx   = imageIndex(workerNew, "sha256:a6")
	runnerIdx      = imageIndex(runnerImg, "sha256:a7")
	runnerRebuilt  = imageIndex(runnerImg, "sha256:a8")
	runnerRebuilt2 = imageIndex(runnerImg, "sha256:a9")
	baseIdx        = imageIndex(sha256Of(`{"img":"base"}`), "sha256:b1")
	baseNewIdx     = imageIndex(sha256Of(`{"img":"base-new"}`), "sha256:b2")
)

const (
	codeRepo  = "sf/ai-software-factory-code"
	forkRepo  = "factory-agent/ai-software-factory-code"
	commitC1  = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	commitC2  = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	commitHR  = "4040404040404040404040404040404040404040" // the last hand roll
	commitC0  = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0" // the last roll task's
	mergePR12 = "1212121212121212121212121212121212121212"
	mergePR14 = "1414141414141414141414141414141414141414"
	agentA1   = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	rollR0    = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
)

// sinceHandRoll is main's history from the last hand roll to a commit,
// newest first: a direct push, the merge of agent pull request #12 and
// its branch commit, and the merge of roll #14 and its roll commit.
var sinceHandRoll = []map[string]any{
	{"sha": commitC1, "commit": map[string]string{"message": "Tighten the gateway's upstream timeout\n\nbody"}, "parents": []any{map[string]any{}}},
	{"sha": mergePR12, "commit": map[string]string{"message": "Merge pull request 'Keys are no longer blocked' (#12) from factory-agent/x:factory/t-agent1 into main"},
		"parents": []any{map[string]any{}, map[string]any{}}},
	{"sha": agentA1, "commit": map[string]string{"message": "Stop blocking keys at every session, fixes #3 for @admin, see https://evil.test/x"}, "parents": []any{map[string]any{}}},
	{"sha": mergePR14, "commit": map[string]string{"message": "Merge pull request 'roll 0a0a0a0: worker' (#14) from factory-agent/x:factory/t-roll0 into main"},
		"parents": []any{map[string]any{}, map[string]any{}}},
	{"sha": rollR0, "commit": map[string]string{"message": "roll 0a0a0a0: worker\n\nMain CI's push run 50 published"}, "parents": []any{map[string]any{}}},
}

// rollForgejo plays Forgejo for the roller and RollCommit: the code
// repository and the bot's fork, their files by ref, the push runs, the
// registry, and records every write. It answers the git data API's
// writes (POST git/blobs, git/trees, git/commits, PATCH git/refs) the
// way the factory's Forgejo does — by refusing them — so a test catches
// a call Forgejo lacks.
type rollForgejo struct {
	mu       sync.Mutex
	runs     []map[string]any
	files    map[string]map[string]string // "owner/name@ref" -> path -> content
	blobs    map[string]string            // "owner/name@ref path" -> blob sha
	registry map[string]string            // "sf/<image>/manifests/<reference>" -> body
	branches map[string]string            // fork branch -> head
	behind   int                          // the fork's main behind upstream
	mainHead string                       // the code repository's main head, m1 when empty

	issues      []string // "title\n\nbody"
	comments    []string // "#n body"
	commits     []map[string]any
	synced      int
	calls       []string
	touched     map[string][]string            // commit -> the files it changes against its first parent (the no-roll-merged fallback); unlisted: main.go
	rolled      map[string][]string            // commit -> the files it changes against the commit the last merged roll rolled; unlisted: touched, else main.go
	carries     map[string][]map[string]any    // compare base...head -> the commits it lists, the chain recorder's carry
	forkParents map[string][]string            // fork commit -> its parents (a merge commit has several)
	forkDiffs   map[string][]string            // fork "base...head" -> the files it changes
	statuses    map[string][]map[string]string // commit -> its statuses (status, context, target_url, created_at)
	reported    []string                       // "sha state context" of every status the roller reported
	pulls       []string                       // "head title" of every pull request opened
	deps        []string                       // "issue on dependency" of every issue dependency added
	depsDown    bool                           // Forgejo answers 500 on the issue dependencies
	logs        map[string]string              // a job's target_url -> its raw log, served to the bot's session
	unreadable  map[string]bool                // a job's target_url whose log Forgejo cannot open
	closed      []string                       // the number of every issue closed
	jobs        map[int64][]map[string]any     // run id -> its jobs (actionJob), as actions/runs/{id}/jobs lists them
	jobsDown    bool                           // actions/runs/{id}/jobs answers 500
	jobLogs     map[string]string              // "job id/attempt" -> that attempt's log, as actions/jobs/{id}/logs serves it; unlisted: 404
	rerun       int                            // the status a job's re-run answers; 0 is 204
	rerunOf     map[int64]int                  // job id -> the status its re-run answers, over rerun
	reruns      []string                       // "token job" of every job re-run asked

	// the roll branch's writes: what each contents commit changed on it,
	// the pull request updates (its rebases), and the pull requests the
	// fake knows by number (its head branches)
	branchPaths    map[string][]string // fork branch -> the paths its contents commits changed
	blobSeq        int                 // the fake's blob shas of contents writes
	updates        []string            // "#n rebase <branch> onto <main's head>" of every pull request update
	updateConflict bool                // every pull request update answers a conflict
	prs            map[int64]string    // pull request number -> its head branch
	closedPRs      []int64             // the numbers of the pull requests closed
	titles         map[int64]string    // pull request number -> its title
	titleFails     map[int64]bool      // pull requests whose read answers 500
	deleted        []string            // the fork branches deleted
}

// job tells a job page the fake serves: one with a log, though empty, or
// one whose log is unreadable.
func (f *rollForgejo) job(p string) bool {
	_, ok := f.logs[p]
	return ok || f.unreadable[p]
}

func (f *rollForgejo) srv(t *testing.T) (*Forgejo, *Registry) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		q := r.URL.Query()
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		p, repo := r.URL.Path, ""
		for _, name := range []string{codeRepo, forkRepo} {
			if rest, ok := strings.CutPrefix(p, "/api/v1/repos/"+name); ok {
				p, repo = rest, name
			}
		}
		switch {
		case p == "/v2/token":
			reply(map[string]string{"token": "bearer-tok"})
		case strings.HasPrefix(p, "/v2/"):
			body, ok := f.registry[strings.TrimPrefix(p, "/v2/")]
			if !ok {
				http.Error(w, "manifest unknown", http.StatusNotFound)
				return
			}
			io.WriteString(w, body)
		case repo == codeRepo && r.Method == "POST" && p == "/pulls":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.pulls = append(f.pulls, in["head"]+" "+in["title"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 70 + len(f.pulls), "html_url": "https://git.example/pr", "head": map[string]any{"sha": "rev1"}, "base": map[string]any{"sha": "m1"}, "state": "open"})
		case repo == codeRepo && strings.HasPrefix(p, "/commits/") && strings.HasSuffix(p, "/status"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, "/commits/"), "/status")
			state := "success"
			for _, s := range f.statuses[sha] {
				if s["status"] == "failure" {
					state = "failure"
				}
			}
			reply(map[string]any{"state": state, "statuses": f.statuses[sha]})
		case repo == codeRepo && r.Method == "POST" && strings.HasPrefix(p, "/statuses/"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			sha := strings.TrimPrefix(p, "/statuses/")
			if f.statuses == nil {
				f.statuses = map[string][]map[string]string{}
			}
			f.statuses[sha] = append(f.statuses[sha], in)
			f.reported = append(f.reported, sha+" "+in["state"]+" "+in["context"])
			w.WriteHeader(http.StatusCreated)
		case repo == codeRepo && r.Method == "POST" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/dependencies"):
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.deps = append(f.deps, strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/dependencies")+" on "+fmt.Sprint(in["index"]))
			w.WriteHeader(http.StatusCreated)
		case repo == codeRepo && f.depsDown && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/dependencies"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case repo == codeRepo && r.Method == "GET" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/dependencies"):
			issue := strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/dependencies")
			out := []map[string]int64{}
			for _, d := range f.deps {
				var n int64
				if on, ok := strings.CutPrefix(d, issue+" on "); ok && q.Get("page") == "1" {
					fmt.Sscan(on, &n)
					out = append(out, map[string]int64{"number": n})
				}
			}
			reply(out)
		case repo == codeRepo && r.Method == "DELETE" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/dependencies"):
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			dep := strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/dependencies") + " on " + fmt.Sprint(in["index"])
			f.deps = slices.DeleteFunc(f.deps, func(d string) bool { return d == dep })
		case repo == codeRepo && f.jobsDown && strings.HasPrefix(p, "/actions/runs/") && strings.HasSuffix(p, "/jobs"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case repo == codeRepo && r.Method == "GET" && strings.HasPrefix(p, "/actions/runs/") && strings.HasSuffix(p, "/jobs"):
			id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/actions/runs/"), "/jobs"), 10, 64)
			jobs, ok := f.jobs[id]
			if !ok {
				t.Errorf("jobs of run %d, which the fake does not know", id)
				http.NotFound(w, r)
				return
			}
			reply(jobs)
		case repo == codeRepo && r.Method == "GET" && strings.HasPrefix(p, "/actions/jobs/") && strings.HasSuffix(p, "/logs"):
			id, attempt := strings.TrimSuffix(strings.TrimPrefix(p, "/actions/jobs/"), "/logs"), q.Get("attempt")
			if attempt == "" {
				// Forgejo serves the job's newest attempt
				attempt = "1"
				for _, jobs := range f.jobs {
					for _, j := range jobs {
						if fmt.Sprint(j["id"]) == id {
							attempt = fmt.Sprint(j["attempt"])
						}
					}
				}
			}
			log, ok := f.jobLogs[id+"/"+attempt]
			if !ok {
				// Forgejo's ErrJobNotExecuted, ErrLogsExpired
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(log))
		case repo == codeRepo && r.Method == "POST" && strings.HasPrefix(p, "/actions/jobs/") && strings.HasSuffix(p, "/rerun"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/actions/jobs/"), "/rerun")
			f.reruns = append(f.reruns, strings.TrimPrefix(r.Header.Get("Authorization"), "token ")+" "+id)
			n, _ := strconv.ParseInt(id, 10, 64)
			if status := cmp.Or(f.rerunOf[n], f.rerun, http.StatusNoContent); status != http.StatusNoContent {
				w.WriteHeader(status)
				return
			}
			// as Forgejo 17's PrepareNextAttempt: the job's next attempt waits
			for _, jobs := range f.jobs {
				for _, j := range jobs {
					if fmt.Sprint(j["id"]) == id {
						j["attempt"], j["status"] = j["attempt"].(int)+1, "waiting"
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case repo == codeRepo && p == "/actions/runs":
			if q.Get("event") != "push" {
				t.Errorf("runs query %v", q)
			}
			reply(map[string]any{"workflow_runs": f.runs})
		case r.Method == "GET" && strings.HasPrefix(p, "/raw/"):
			content, ok := f.files[repo+"@"+q.Get("ref")][strings.TrimPrefix(p, "/raw/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, content)
		case r.Method == "GET" && strings.HasPrefix(p, "/contents/"):
			path := strings.TrimPrefix(p, "/contents/")
			content, ok := f.files[repo+"@"+q.Get("ref")][path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64",
				"sha": f.blobs[repo+"@"+q.Get("ref")+" "+path]})
		case repo == codeRepo && p == "/commits" && q.Get("path") == "k8s":
			reply([]map[string]any{{"sha": commitHR, "parents": []map[string]string{{"sha": "p"}}}})
		case repo == forkRepo && r.Method != "GET" && (p == "/git/blobs" || p == "/git/trees" || p == "/git/commits" || strings.HasPrefix(p, "/git/refs/")):
			// the factory's Forgejo offers GET only on the git data API
			t.Errorf("the git data API is not the factory Forgejo's: %s %s", r.Method, p)
			http.Error(w, "no such endpoint", http.StatusNotFound)
		case repo == forkRepo && strings.HasPrefix(p, "/git/commits/"):
			sha := strings.TrimPrefix(p, "/git/commits/")
			var parents []map[string]string
			for _, parent := range f.forkParents[sha] {
				parents = append(parents, map[string]string{"sha": parent})
			}
			reply(map[string]any{"parents": parents})
		case repo == codeRepo && r.Method == "POST" && strings.HasSuffix(p, "/update"):
			if style := q.Get("style"); style != "rebase" {
				t.Errorf("pull request update style %q, want rebase", style)
			}
			n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/pulls/"), "/update"), 10, 64)
			if err != nil {
				t.Errorf("pull request update path %s: %v", p, err)
			}
			branch, ok := f.prs[n]
			if !ok {
				t.Errorf("update of pull request %d, which the fake does not know", n)
				http.NotFound(w, r)
				return
			}
			if f.updateConflict {
				http.Error(w, "the branch conflicts with its base", http.StatusConflict)
				return
			}
			base := f.branches[prBase]
			if base == "" {
				t.Errorf("update of pull request %d: the fork's main has no head", n)
			}
			// the branch's own commits replayed on main's head: main's
			// files with the branch's changes where its commits made them
			replayed := map[string]string{}
			maps.Copy(replayed, f.files[forkRepo+"@"+prBase])
			for _, path := range f.branchPaths[branch] {
				if content, ok := f.files[forkRepo+"@"+branch][path]; ok {
					replayed[path] = content
				}
			}
			if f.files == nil {
				f.files = map[string]map[string]string{}
			}
			f.files[forkRepo+"@"+branch] = replayed
			for _, path := range slices.Sorted(maps.Keys(replayed)) {
				if _, ok := f.blobs[forkRepo+"@"+branch+" "+path]; ok {
					continue
				}
				if b, ok := f.blobs[forkRepo+"@"+prBase+" "+path]; ok && b != "" {
					if f.blobs == nil {
						f.blobs = map[string]string{}
					}
					f.blobs[forkRepo+"@"+branch+" "+path] = b
					continue
				}
				f.blobSeq++
				if f.blobs == nil {
					f.blobs = map[string]string{}
				}
				f.blobs[forkRepo+"@"+branch+" "+path] = fmt.Sprintf("blob-%d", f.blobSeq)
			}
			sha := fmt.Sprintf("rebase-%d", len(f.updates)+1)
			f.updates = append(f.updates, fmt.Sprintf("#%d rebase %s onto %s", n, branch, base))
			if f.forkParents == nil {
				f.forkParents = map[string][]string{}
			}
			f.forkParents[sha] = []string{base}
			if f.branches == nil {
				f.branches = map[string]string{}
			}
			f.branches[branch] = sha
			reply(map[string]any{"message": "rebased"})
		case repo == codeRepo && r.Method == "GET" && p == "/pulls":
			out := []map[string]any{}
			for i, pr := range f.pulls {
				n := int64(71 + i)
				if q.Get("page") != "1" || slices.Contains(f.closedPRs, n) {
					continue
				}
				head, _, _ := strings.Cut(pr, " ")
				owner, ref, _ := strings.Cut(head, ":")
				out = append(out, map[string]any{"number": n, "state": "open", "head": map[string]any{"ref": ref, "repo": map[string]any{"full_name": owner + "/ai-software-factory-code"}}})
			}
			reply(out)
		case repo == codeRepo && r.Method == "GET" && strings.HasPrefix(p, "/pulls/"):
			n, err := strconv.ParseInt(strings.TrimPrefix(p, "/pulls/"), 10, 64)
			if err != nil {
				t.Errorf("pull request path %s: %v", p, err)
			}
			state := "open"
			if slices.Contains(f.closedPRs, n) {
				state = "closed"
			}
			if f.titleFails[n] {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			reply(map[string]any{"number": n, "state": state, "title": f.titles[n]})
		case repo == codeRepo && r.Method == "PATCH" && strings.HasPrefix(p, "/pulls/"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			n, _ := strconv.ParseInt(strings.TrimPrefix(p, "/pulls/"), 10, 64)
			if in["state"] == "closed" {
				f.closedPRs = append(f.closedPRs, n)
			}
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{})
		case repo == forkRepo && strings.HasPrefix(p, "/compare/"):
			var out []map[string]string
			for _, name := range f.forkDiffs[strings.TrimPrefix(p, "/compare/")] {
				out = append(out, map[string]string{"filename": name})
			}
			reply(map[string]any{"files": out})
		case repo == codeRepo && strings.HasPrefix(p, "/git/commits/"):
			reply(map[string]any{"parents": []map[string]string{{"sha": "parent-of-" + strings.TrimPrefix(p, "/git/commits/")}}})
		case repo == codeRepo && strings.HasPrefix(p, "/compare/"):
			base, head, _ := strings.Cut(strings.TrimPrefix(p, "/compare/"), "...")
			if commits := f.carries[base+"..."+head]; commits != nil {
				reply(map[string]any{"commits": commits})
				return
			}
			filesOf := func(names []string) []map[string]string {
				if names == nil {
					names = []string{"main.go"}
				}
				var out []map[string]string
				for _, name := range names {
					out = append(out, map[string]string{"filename": name})
				}
				return out
			}
			switch {
			case base == "parent-of-"+head:
				reply(map[string]any{"files": filesOf(f.touched[head])})
			case base == commitHR || base == commitC0:
				files := f.rolled[head]
				if files == nil {
					files = f.touched[head]
				}
				reply(map[string]any{"commits": sinceHandRoll, "files": filesOf(files)})
			default:
				t.Errorf("compare %s", p)
			}
		case r.Method == "POST" && repo == codeRepo && p == "/issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.issues = append(f.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 30 + len(f.issues), "html_url": "https://git.example/sf/code/issues/31"})
		case (r.Method == "PUT" || r.Method == "GET") && strings.HasSuffix(p, "/labels"):
			reply([]any{})
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments"):
			n := strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")
			out := []map[string]string{}
			for _, c := range f.comments {
				if body, ok := strings.CutPrefix(c, n+" "); ok {
					out = append(out, map[string]string{"body": body})
				}
			}
			reply(out)
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")+" "+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"id": 900})
		case repo == forkRepo && p == "":
			reply(map[string]any{"fork": true, "private": true, "has_actions": false, "parent": map[string]string{"full_name": codeRepo}})
		case repo == codeRepo && r.Method == "GET" && p == "/branches/main":
			reply(map[string]any{"commit": map[string]string{"id": cmp.Or(f.mainHead, "m1")}})
		case repo == forkRepo && p == "/sync_fork" && r.Method == "GET":
			reply(map[string]any{"allowed": f.behind > 0, "commits_behind": f.behind})
		case repo == forkRepo && p == "/sync_fork" && r.Method == "POST":
			f.synced++
			w.WriteHeader(http.StatusNoContent)
		case repo == forkRepo && r.Method == "DELETE" && strings.HasPrefix(p, "/branches/"):
			branch := strings.TrimPrefix(p, "/branches/")
			delete(f.branches, branch)
			f.deleted = append(f.deleted, branch)
			w.WriteHeader(http.StatusNoContent)
		case repo == forkRepo && r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
			head, ok := f.branches[strings.TrimPrefix(p, "/branches/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(map[string]any{"commit": map[string]string{"id": head}})
		case repo == forkRepo && p == "/contents" && r.Method == "POST":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.commits = append(f.commits, in)
			base, _ := in["branch"].(string)
			branch := base
			if nb, _ := in["new_branch"].(string); nb != "" {
				branch = nb
			}
			// the commit the fake holds: its branch's files with the
			// posted ones, its head and first parent
			if f.files == nil {
				f.files = map[string]map[string]string{}
			}
			files := map[string]string{}
			maps.Copy(files, f.files[forkRepo+"@"+base])
			for _, e := range in["files"].([]any) {
				op, _ := e.(map[string]any)
				path, _ := op["path"].(string)
				content, err := base64.StdEncoding.DecodeString(fmt.Sprint(op["content"]))
				if err != nil {
					t.Errorf("content of %s: %v", path, err)
				}
				files[path] = string(content)
				f.blobSeq++
				if f.blobs == nil {
					f.blobs = map[string]string{}
				}
				f.blobs[forkRepo+"@"+branch+" "+path] = fmt.Sprintf("blob-%d", f.blobSeq)
				if !slices.Contains(f.branchPaths[branch], path) {
					if f.branchPaths == nil {
						f.branchPaths = map[string][]string{}
					}
					f.branchPaths[branch] = append(f.branchPaths[branch], path)
				}
			}
			f.files[forkRepo+"@"+branch] = files
			sha := fmt.Sprintf("roll-commit-%d", len(f.commits))
			if f.forkParents == nil {
				f.forkParents = map[string][]string{}
			}
			f.forkParents[sha] = []string{f.branches[base]}
			f.branches[branch] = sha
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"commit": map[string]string{"sha": sha}})
		case r.Method == "POST" && p == "/user/login":
			http.Redirect(w, r, "/", http.StatusSeeOther)
		case r.Method == "GET" && (p == "/" || f.job(p)):
		case r.Method == "GET" && f.unreadable[strings.TrimSuffix(p, "/logs")]:
			// actions.Logs on a log file no runner ever wrote
			http.Error(w, `dbfs open "actions_log/sf/ai-software-factory-code/00/0.log": file does not exist`, http.StatusInternalServerError)
		case r.Method == "GET" && f.job(strings.TrimSuffix(p, "/logs")):
			// as Forgejo's actions.Logs serves it
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(f.logs[strings.TrimSuffix(p, "/logs")]))
		case repo == codeRepo && r.Method == "PATCH" && strings.HasPrefix(p, "/issues/"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["state"] == "closed" {
				f.closed = append(f.closed, strings.TrimPrefix(p, "/issues/"))
			}
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{})
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"},
		&Registry{BaseURL: srv.URL, User: "puller", Password: "pw"}
}

// actionJob is a job of a run as Forgejo 17's actions/runs/{id}/jobs
// lists it.
func actionJob(id int64, name, status string, attempt int) map[string]any {
	return map[string]any{"id": id, "name": name, "status": status, "attempt": attempt}
}

func pushRun(id int64, sha, status string) map[string]any {
	return map[string]any{"id": id, "commit_sha": sha, "event": "push", "status": status, "prettyref": "main", "created": "2026-09-29T10:00:00Z"}
}

// imageGreen is the shas' combined status as main CI reports it once its
// three image jobs published their images: the roller's trigger (roadmap
// 296), whatever the run's own state then is. The image jobs need the test
// job, so its status is green too.
func imageGreen(shas ...string) map[string][]map[string]string {
	out := map[string][]map[string]string{}
	for _, sha := range shas {
		for _, ctx := range append([]string{testContext}, imageContexts...) {
			out[sha] = append(out[sha], map[string]string{"status": "success", "context": ctx, "target_url": "https://git.example"})
		}
	}
	return out
}

// registryOf serves each index under its digest and under the tags given,
// each signed as main CI signs what it pushes. The sandbox base is
// baseIdx, the one k8sManifests pins, at every tag the others are given,
// unless the tags name its own.
func registryOf(tags map[string]string, indexes ...string) map[string]string {
	out := map[string]string{"sf/sandbox-base/manifests/" + sha256Of(baseIdx): baseIdx}
	maps.Copy(out, cosignSigned(testCosignKey, "sf/sandbox-base", sha256Of(baseIdx)))
	for _, image := range []string{"factory-gateway", "opencode-worker", "ci-runner"} {
		for _, idx := range indexes {
			out["sf/"+image+"/manifests/"+sha256Of(idx)] = idx
			maps.Copy(out, cosignSigned(testCosignKey, "sf/"+image, sha256Of(idx)))
		}
	}
	for key, idx := range tags {
		out[key] = idx
		repository, tag, _ := strings.Cut(key, "/manifests/")
		maps.Copy(out, cosignSigned(testCosignKey, repository, sha256Of(idx)))
		if base := "sf/sandbox-base/manifests/" + tag; tags[base] == "" {
			out[base] = baseIdx
		}
	}
	return out
}

func newTestRoller(t *testing.T, f *rollForgejo) (*Roller, *fakeStore, *fakeWorkflows) {
	t.Helper()
	fj, reg := f.srv(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	return &Roller{Forgejo: fj, Registry: reg, SigningKey: &testCosignKey.PublicKey, Store: store, Workflows: wf,
		Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}, Token: "registrar-tok", Budget: testBudget}, store, wf
}

// mainPins is the code repository's main before the roll: every image at
// its old build.
func mainPins() map[string]string {
	return k8sManifests(sha256Of(gwOldIdx), sha256Of(gwOldIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
}

// A carried pull request whose title cannot be read still lists, by its
// number alone; the other keeps its title, and the roll is filed.
func TestRollerListsACarriedPullRequestWhoseTitleFailsToRead(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(57, commitC1, "success"), pushRun(56, "c0c0c0c0", "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
		titles:     map[int64]string{13: "Second change"},
		titleFails: map[int64]bool{12: true},
	}
	roller, store, _ := newTestRoller(t, f)
	store.tasks["t-12"] = Task{ID: "t-12", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12, MergeCommit: mergePR12}
	store.tasks["t-13"] = Task{ID: "t-13", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 13, MergeCommit: agentA1}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 1 || !strings.HasSuffix(f.issues[0], "Merged pull requests it carries:\n\n- #12\n- #13 Second change") {
		t.Fatalf("issues %q, want one ending in #12 without a title and #13 with its own", f.issues)
	}
	if len(store.rolls) != 1 || !slices.Equal(store.rolls[0].PRs, []int64{12, 13}) {
		t.Fatalf("roll requests %+v, want one carrying #12 and #13", store.rolls)
	}
}

// The roll task's budget is the default, at most the project's card maximum.
func TestRollerFilesUnderTheProjectsCardMaximum(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(57, commitC1, "success"), pushRun(56, "c0c0c0c0", "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, _ := newTestRoller(t, f)
	project := seedProject(codeRepo)
	project.Budget = &policy.ProjectBudget{MonthEUR: 50, TaskMaxEUR: 1}
	store.projects = []Project{project}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 {
		t.Fatalf("roll requests %+v, want one", store.rolls)
	}
	if got := store.tasks[store.rolls[0].TaskID].BudgetEUR; got != 1 {
		t.Fatalf("roll task budget %v, want the project's card maximum 1 under the default %v", got, testBudget().TaskDefaultEUR)
	}
}

// The newest green push run of main whose images changed files one roll
// task: the issue names the changed images, their new references and the
// merged pull requests since the last roll (the roll tasks' own left
// out); an image rebuilt with no input change is no change. Main's head is
// a newer commit whose tests passed and whose images still build. A second
// poll files nothing: each commit once.
func TestRollerFilesOneRollPerCommit(t *testing.T) {
	logs := captureLog(t)
	statuses := imageGreen(commitC1)
	statuses[commitC2] = []map[string]string{{"status": "success", "context": testContext, "target_url": "https://git.example"}}
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "running"), pushRun(57, commitC1, "success"), pushRun(56, "c0c0c0c0", "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: statuses,
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	f.titles = map[int64]string{12: "Keys are no longer blocked at every session (closes #4, @bob)"}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14, MergeCommit: mergePR14}
	store.tasks["t-12"] = Task{ID: "t-12", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12, MergeCommit: mergePR12}
	store.tasks["t-11"] = Task{ID: "t-11", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 11, MergeCommit: "0b0b"}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	newGw := registryHost + "factory-gateway@" + sha256Of(gwNewIdx)
	want := "roll c1c1c1c: factoryd, gateway\n\n" +
		"Main CI's push run 57 published the images of " + commitC1 + ". The digests under k8s/ that change:\n\n" +
		"- factoryd: " + newGw + "\n- gateway: " + newGw + "\n\n" +
		"Commits it carries since the last roll:\n\n- a1a1a1a Stop blocking keys at every session, (fixes) #3 for ＠admin, see hxxps://evil[.]test/x\n- c1c1c1c Tighten the gateway's upstream timeout\n\n" +
		"Merged pull requests it carries:\n\n- #12 Keys are no longer blocked at every session ((closes) #4, ＠bob)"
	if len(f.issues) != 1 || f.issues[0] != want {
		t.Fatalf("issues %q, want one:\n%s", f.issues, want)
	}
	if len(store.rolls) != 1 {
		t.Fatalf("roll requests %+v, want one", store.rolls)
	}
	req := store.rolls[0]
	task := store.tasks[req.TaskID]
	if req.SHA != commitC1 || req.RunID != 57 || req.State != RollOpened || req.Summary != want ||
		len(req.Changes) != 2 || req.Changes["factoryd"] != sha256Of(gwNewIdx) || req.Changes["gateway"] != sha256Of(gwNewIdx) {
		t.Fatalf("roll request %+v", req)
	}
	if !slices.Equal(req.PRs, []int64{12}) {
		t.Fatalf("roll request carries pull requests %v, want #12's: the card merged among its commits", req.PRs)
	}
	if task.Source != SourceRoll || task.Objective != want || task.SHA != commitC1 || task.Status != "queued" || task.Kind != "agent" ||
		task.IssueNumber != 31 || task.AgentSummary != want || task.BudgetEUR != testBudget().TaskDefaultEUR {
		t.Fatalf("roll task %+v", task)
	}
	if !slices.Equal(wf.started, []string{task.ID}) {
		t.Fatalf("started %v, want the roll task's workflow", wf.started)
	}
	if !strings.Contains(logs.String(), "roll task "+task.ID+", issue #31: roll c1c1c1c: factoryd, gateway") {
		t.Fatalf("log %q", logs.String())
	}
	if n := slices.Index(f.calls, "GET /v2/token"); n < 0 || slices.Contains(f.calls[n+1:], "GET /v2/token") {
		t.Fatalf("registry tokens read %v, want one per poll", f.calls)
	}

	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 1 || len(store.rolls) != 1 || len(wf.started) != 1 {
		t.Fatalf("the second poll filed again: issues %d, rolls %d, starts %d", len(f.issues), len(store.rolls), len(wf.started))
	}
}

// The roller judges a run whose three image jobs are green even while the
// run itself still runs (roadmap 296): the run's own state says nothing of its
// images. A run with an image job not yet green waits.
func TestRollerRollsARunWhoseImagesAreGreenWhileItRuns(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(64, commitC1, "running")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || store.rolls[0].SHA != commitC1 || store.rolls[0].State != RollOpened || len(wf.started) != 1 {
		t.Fatalf("rolls %+v, starts %v; the running run was not judged", store.rolls, wf.started)
	}

	// the sandbox image job has not reported yet: no judgement
	f.runs = []map[string]any{pushRun(65, commitC2, "running")}
	f.statuses = map[string][]map[string]string{commitC2: {
		{"status": "success", "context": "ci / image (push)", "target_url": "https://git.example"},
		{"status": "success", "context": "ci / image-runner (push)", "target_url": "https://git.example"},
	}}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || len(wf.started) != 1 {
		t.Fatalf("rolls %+v, starts %v; a run with a pending image job was judged", store.rolls, wf.started)
	}
}

// A roll's branch is cut from main's head (RollCommit), and its pull
// request's checks run main CI's test job again on the head's code (roll
// #1219: cut on a newer commit whose tests still ran, it failed on that
// commit's gosec finding). While main's head is a newer commit whose test
// job runs or failed, the green commit's roll waits, saying so once; it
// files once the head's tests are green.
func TestRollerWaitsForMainsHeadToPassItsTests(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "running"), pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, _ := newTestRoller(t, f)
	ctx := context.Background()
	rolls := func() int {
		n := 0
		for _, issue := range f.issues {
			if strings.HasPrefix(issue, "roll ") {
				n++
			}
		}
		return n
	}
	for range 2 {
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.rolls) != 0 || rolls() != 0 {
		t.Fatalf("rolls %+v, issues %q: a roll was cut while main's head ran its tests", store.rolls, f.issues)
	}
	if n := strings.Count(logs.String(), "main's head "+commitC2+" has no green ci / test (push) yet: the roll waits"); n != 1 {
		t.Fatalf("log %q, want the wait said once", logs.String())
	}

	f.mu.Lock()
	f.runs[0]["status"] = "failure"
	f.statuses[commitC2] = []map[string]string{{"status": "failure", "context": "ci / test (push)", "target_url": "https://git.example"}}
	f.jobs = map[int64][]map[string]any{58: {actionJob(5801, "test", "failure", 1)}}
	f.mu.Unlock()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 0 || rolls() != 0 {
		t.Fatalf("rolls %+v, issues %q: a roll was cut on main's red head", store.rolls, f.issues)
	}

	f.mu.Lock()
	f.runs[0]["status"] = "running"
	f.statuses[commitC2] = []map[string]string{{"status": "success", "context": "ci / test (push)", "target_url": "https://git.example"}}
	f.mu.Unlock()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || store.rolls[0].SHA != commitC1 || store.rolls[0].State != RollOpened || rolls() != 1 {
		t.Fatalf("rolls %+v, issues %q, want commit C1's roll once main's head passed its tests", store.rolls, f.issues)
	}
}

// A green commit whose changed images are not all signed with the
// factory's key (roadmap 46) rolls none of them: the gateway's signature
// is by another key and the worker carries none, so no roll task is
// filed, the commit's request is refused, and a human card names each
// image and why. A second poll files nothing: each commit once.
func TestRollerRefusesImagesWithoutTheFactorysSignature(t *testing.T) {
	logs := captureLog(t)
	run := pushRun(57, commitC1, "success")
	run["index_in_repo"] = 412
	f := &rollForgejo{
		runs:     []map[string]any{run},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerNewIdx,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(f.registry, cosignSigned(stranger, "sf/factory-gateway", sha256Of(gwNewIdx)))
	delete(f.registry, "sf/opencode-worker/manifests/sha256-"+strings.TrimPrefix(sha256Of(workerNewIdx), "sha256:"))
	roller, store, wf := newTestRoller(t, f)
	roller.Forgejo.PublicURL = "https://git.example"
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	gw, worker := "sf/factory-gateway@"+sha256Of(gwNewIdx), "sf/opencode-worker@"+sha256Of(workerNewIdx)
	ask := "roll c1c1c1c refused: factoryd, gateway, worker not signed with the factory's key. Fix the signing, then close."
	want := ask + "\n\n```\n" + ask + "\n```\n\n" +
		"Why: Main CI's run is green, and factoryd rolls none of its images: an image it rolls must carry a cosign signature by the factory's key (k8s/cosign.pub), and these do not.\n\n" +
		"Info: The run: https://git.example/sf/ai-software-factory-code/actions/runs/412\n\n" +
		"- factoryd: " + gw + " carries no signature by this key over this digest (1 by another key or over another digest)\n" +
		"- gateway: " + gw + " carries no signature by this key over this digest (1 by another key or over another digest)\n" +
		"- worker: " + worker + " carries no signature\n\n" +
		"Main CI's image jobs sign each image they push. No signature means a job did not sign it or the tag was pushed outside main CI; " +
		"a signature by another key means the code repository's Actions secret COSIGN_PRIVATE_KEY is not the key k8s/cosign.pub holds. " +
		"Find which, fix it, and close this issue. The next green push run of main is judged again; an empty commit on main starts one.\n\n" +
		"Choices:\n- Fixed, close\n\n" +
		"factoryd asks (factoryd). Answer on the card in the console or in the chat's inbox, or close this issue once decided.\n\nfiled by factoryd"
	if len(f.issues) != 1 || f.issues[0] != want {
		t.Fatalf("issues %q, want one:\n%s", f.issues, want)
	}
	if len(store.rolls) != 1 {
		t.Fatalf("roll requests %+v, want one", store.rolls)
	}
	req := store.rolls[0]
	card := store.tasks[req.TaskID]
	if req.SHA != commitC1 || req.RunID != 57 || req.State != RollRefused || req.Summary != ask || !reflect.DeepEqual(req.Changes,
		map[string]string{"factoryd": sha256Of(gwNewIdx), "gateway": sha256Of(gwNewIdx), "worker": sha256Of(workerNewIdx)}) {
		t.Fatalf("roll request %+v", req)
	}
	if card.Kind != "human" || card.Source != SourceRollRefused || card.Status != "waiting-human" || card.SHA != commitC1 ||
		card.IssueNumber != 31 || card.Objective != want || len(store.tasks) != 1 {
		t.Fatalf("card %+v of tasks %v, want the one human card", card, store.tasks)
	}
	if len(wf.started) != 0 || len(f.pulls) != 0 {
		t.Fatalf("a roll ran: starts %v, pull requests %v", wf.started, f.pulls)
	}
	if !strings.Contains(logs.String(), "refused, human card "+card.ID+", issue #31: "+ask) {
		t.Fatalf("log %q", logs.String())
	}
	if n := slices.Index(f.calls, "GET /v2/token"); n < 0 || slices.Contains(f.calls[n+1:], "GET /v2/token") {
		t.Fatalf("registry tokens read %v, want one per poll", f.calls)
	}
	if !slices.Equal(f.reported, []string{commitC1 + " failure factory/roll"}) {
		t.Fatalf("reported %v, want the refusal named", f.reported)
	}

	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 1 || len(store.rolls) != 1 || len(store.tasks) != 1 {
		t.Fatalf("the second poll filed again: issues %d, rolls %d, tasks %d", len(f.issues), len(store.rolls), len(store.tasks))
	}
}

// A green commit whose images are rebuilds of the pinned ones (the same
// image manifest under a new attestation) records a skip and files
// nothing; a run older than a judged one is never judged.
func TestRollerSkipsACommitThatChangesNoImage(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(60, commitC2, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC2),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c2c2c2c": imageIndex(gwOld, "sha256:b1"),
			"sf/opencode-worker/manifests/c2c2c2c": workerRebuilt,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt2,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-12"] = Task{ID: "t-12", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12, MergeCommit: mergePR12}
	store.tasks["t-14"] = Task{ID: "t-14", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 14, MergeCommit: mergePR14}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || !reflect.DeepEqual(store.rolls[0], RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 60, State: RollSkipped, PRs: []int64{12, 14}}) {
		t.Fatalf("roll requests %+v, want one skip", store.rolls)
	}
	if len(f.issues) != 0 || len(wf.started) != 0 || !strings.Contains(logs.String(), commitC2+" (run 60) changed no image") {
		t.Fatalf("issues %v, starts %v, log %q", f.issues, wf.started, logs.String())
	}
	if !slices.Equal(f.reported, []string{commitC2 + " success factory/roll"}) {
		t.Fatalf("reported %v, want the skip live", f.reported)
	}

	f.runs = []map[string]any{pushRun(59, commitC1, "success")}
	calls := len(f.calls)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || len(f.calls) != calls+1 {
		t.Fatalf("an older run was judged: rolls %+v, calls %v", store.rolls, f.calls[calls:])
	}
}

// While a roll task is open, a newer green commit whose images differ
// from the roll branch's is folded into it: a request on the same task, a
// comment on its pull request and the change request that has its
// workflow commit it. No second task, and none before its pull request
// is open.
func TestRollerFoldsNewImagesIntoTheOpenRoll(t *testing.T) {
	branchPins := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(61, commitC2, "success"), pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins(), forkRepo + "@factory/t-open": branchPins},
		statuses: imageGreen(commitC2),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c2c2c2c": gwNewRebuilt,
			"sf/opencode-worker/manifests/c2c2c2c": workerNewIdx,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt,
		}, gwOldIdx, gwNewIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-open"] = Task{ID: "t-open", Source: SourceRoll, Status: "in-review", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-open", Committed: true,
		Changes: map[string]string{"factoryd": sha256Of(gwNewIdx), "gateway": sha256Of(gwNewIdx)}}}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	// the runs read and the trigger's status read; the chain's bootstrap
	// reads belong to the chain poller now, not the roller
	if len(store.rolls) != 1 || len(f.calls) != 2 {
		t.Fatalf("folded before the pull request was open: rolls %+v, calls %v", store.rolls, f.calls)
	}

	open := store.tasks["t-open"]
	open.PRNumber = 21
	store.tasks["t-open"] = open
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 {
		t.Fatalf("roll requests %+v, want the fold", store.rolls)
	}
	fold := store.rolls[1]
	if fold.SHA != commitC2 || fold.TaskID != "t-open" || fold.State != RollOpened || fold.Committed ||
		len(fold.Changes) != 1 || fold.Changes["worker"] != sha256Of(workerNewIdx) {
		t.Fatalf("fold %+v, want the worker only (the gateway's rebuild is no change against the branch)", fold)
	}
	if !strings.HasPrefix(fold.Summary, "roll c2c2c2c: worker\n\n") || !strings.Contains(fold.Summary, "- worker: "+registryHost+"opencode-worker@"+sha256Of(workerNewIdx)) {
		t.Fatalf("fold summary %q", fold.Summary)
	}
	if len(f.issues) != 0 || len(wf.started) != 0 {
		t.Fatalf("a second task: issues %v, starts %v", f.issues, wf.started)
	}
	if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], "21 factoryd: folded into this roll:\n\nroll c2c2c2c: worker") {
		t.Fatalf("comments %q", f.comments)
	}
	if !slices.Equal(wf.signals, []string{"t-open roll_folded factory-agent #21: roll c2c2c2c: worker folded in"}) {
		t.Fatalf("signals %q", wf.signals)
	}
}

// bumpDigest is the roll's exact edit: the one pinned digest, nothing else.
func TestBumpDigestEditsOnlyThePinnedDigest(t *testing.T) {
	before := k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner)
	got, err := bumpDigest([]byte(before["k8s/factoryd.yaml"]), "opencode-worker", digestWorkerNew)
	if err != nil {
		t.Fatal(err)
	}
	if want := k8sManifests(digestOld, digestOld, digestWorkerNew, digestRunner)["k8s/factoryd.yaml"]; string(got) != want {
		t.Fatalf("bumped:\n%s\nwant:\n%s", got, want)
	}
	if _, err := bumpDigest([]byte(before["k8s/deployment.yaml"]), "opencode-worker", digestWorkerNew); err == nil {
		t.Fatal("a manifest without the image was bumped")
	}
}

func newRollRunner(t *testing.T, f *rollForgejo, rolls ...RollRequest) (*Runner, *fakeStore) {
	t.Helper()
	runner, _, store := newTestRunner(t, Task{ID: "t-roll", Source: SourceRoll, Status: "running", Repository: codeRepo, Kind: "agent", Track: "direct"},
		newFakeKube(), &fakeObjectStore{})
	runner.forgejo, _ = f.srv(t)
	store.rolls = rolls
	return runner, store
}

// RollCommit writes the roll's first commit from main's head on a new
// branch of the synced fork, an update as its pull request's branch
// rebased onto main's head and the pins written on it as one fresh
// commit of every request, and nothing when the branch carries every
// request. The commit's files are the exact edits and its message the
// requests' summaries.
func TestRollCommitWritesTheBranch(t *testing.T) {
	before := mainPins()
	f := &rollForgejo{behind: 3, branches: map[string]string{"main": "fork-main-head"},
		files: map[string]map[string]string{forkRepo + "@main": before},
		blobs: map[string]string{forkRepo + "@main k8s/factoryd.yaml": "blob-f", forkRepo + "@main k8s/deployment.yaml": "blob-d"},
		prs:   map[int64]string{847: "factory/t-roll"}}
	newGw := sha256Of(gwNewIdx)
	runner, store := newRollRunner(t, f, RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll",
		Changes: map[string]string{"factoryd": newGw, "gateway": newGw}, Summary: "roll c1c1c1c: factoryd, gateway\n\nbody one"})
	task := store.tasks["t-roll"]
	task.PRNumber = 847
	store.tasks["t-roll"] = task

	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "roll-commit-1" {
		t.Fatalf("first commit %q %v", head, err)
	}
	after := k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx))
	wantFirst := map[string]any{"branch": "main", "new_branch": "factory/t-roll", "message": "roll c1c1c1c: factoryd, gateway\n\nbody one\n\nFactory-Role: roll\nFactory-Task: t-roll\n",
		"files": []any{
			map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f", "content": base64.StdEncoding.EncodeToString([]byte(after["k8s/factoryd.yaml"]))},
			map[string]any{"operation": "update", "path": "k8s/deployment.yaml", "sha": "blob-d", "content": base64.StdEncoding.EncodeToString([]byte(after["k8s/deployment.yaml"]))},
		}}
	if len(f.commits) != 1 || fmt.Sprint(f.commits[0]) != fmt.Sprint(wantFirst) {
		t.Fatalf("commit %v\nwant %v", f.commits, wantFirst)
	}
	if f.synced != 1 || !store.rolls[0].Committed || store.rolls[0].Head != "roll-commit-1" {
		t.Fatalf("synced %d, committed %v at %q", f.synced, store.rolls[0].Committed, store.rolls[0].Head)
	}
	// the base the workflow-directory guard compares with is the head
	// the branch was cut from
	if base := store.tasks["t-roll"].SHA; base != "fork-main-head" {
		t.Fatalf("task base %q, want the synced main head", base)
	}

	// main moved; the fold is written as the pull request's branch
	// rebased onto main's head and one fresh commit of both requests'
	// pins on it
	f.branches["factory/t-roll"] = "roll-commit-1"
	f.branches["main"] = "main-B"
	store.rolls = append(store.rolls, RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
		Changes: map[string]string{"worker": sha256Of(workerNewIdx)}, Summary: "roll c2c2c2c: worker\n\nbody two"})
	if out, err = execActivity(t, runner, ActivityRollCommit, "t-roll"); err != nil || out.Get(&head) != nil || head != "roll-commit-2" {
		t.Fatalf("update commit %q %v", head, err)
	}
	folded := k8sManifests(newGw, newGw, sha256Of(workerNewIdx), sha256Of(runnerIdx))
	if !slices.Equal(f.updates, []string{"#847 rebase factory/t-roll onto main-B"}) {
		t.Fatalf("pull request updates %v, want one rebase onto main's head", f.updates)
	}
	// the branch is based on main's head again: its replayed commit's
	// parent is main's head, the fresh commit's the replayed branch
	if parents := f.forkParents["rebase-1"]; !slices.Equal(parents, []string{"main-B"}) {
		t.Fatalf("the rebased branch's parents %v, want main's head", parents)
	}
	if parents := f.forkParents["roll-commit-2"]; !slices.Equal(parents, []string{"rebase-1"}) {
		t.Fatalf("the fresh commit's parents %v, want the rebased branch", parents)
	}
	// the fresh commit writes only what the rebased branch still lacks:
	// the worker's pin, its file at the blob the branch holds
	wantUpdate := map[string]any{"branch": "factory/t-roll", "message": "roll c1c1c1c: factoryd, gateway\n\nbody one\n\nroll c2c2c2c: worker\n\nbody two\n\nFactory-Role: roll\nFactory-Task: t-roll\n",
		"files": []any{
			map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-1", "content": base64.StdEncoding.EncodeToString([]byte(folded["k8s/factoryd.yaml"]))},
		}}
	if len(f.commits) != 2 || fmt.Sprint(f.commits[1]) != fmt.Sprint(wantUpdate) {
		t.Fatalf("update commit %v\nwant %v", f.commits[1:], wantUpdate)
	}
	// the branch's diff against main is only the pinned files
	got := map[string]string{}
	for path, content := range f.files[forkRepo+"@factory/t-roll"] {
		if content != f.files[forkRepo+"@main"][path] {
			got[path] = content
		}
	}
	if !reflect.DeepEqual(got, map[string]string{"k8s/factoryd.yaml": folded["k8s/factoryd.yaml"], "k8s/deployment.yaml": folded["k8s/deployment.yaml"]}) {
		t.Fatalf("the branch's diff against main %q, want only the pinned files", got)
	}
	if f.branches["factory/t-roll"] != "roll-commit-2" {
		t.Fatalf("branch %v", f.branches)
	}
	if f.synced != 2 {
		t.Fatalf("synced %d", f.synced)
	}
	for i, r := range store.rolls {
		if !r.Committed || r.Head != "roll-commit-2" {
			t.Fatalf("roll %d %+v, want committed at roll-commit-2", i, r)
		}
	}
	if base := store.tasks["t-roll"].SHA; base != "main-B" {
		t.Fatalf("task base %q, want the synced main head", base)
	}

	if out, err = execActivity(t, runner, ActivityRollCommit, "t-roll"); err != nil || out.Get(&head) != nil || head != "roll-commit-2" {
		t.Fatalf("nothing to commit: %q %v", head, err)
	}
	if len(f.commits) != 2 || len(f.updates) != 1 {
		t.Fatalf("a commit with nothing to write: %v %v", f.commits, f.updates)
	}
}

// A roll filed for commit A, whose first write cut its branch from A, is
// updated for B once main moves to B: the update rebases the branch onto
// B, main's head, and writes the pins as one fresh commit on it — so the
// pull request's base is B and its diff against B only the pinned files
// (roadmap 279).
func TestRollCommitCutsAnUpdateFromMainsHeadAgain(t *testing.T) {
	newGw, newWorker := sha256Of(gwNewIdx), sha256Of(workerNewIdx)
	// main moved to B; the branch still holds A's roll: one commit with
	// the gateway's new digest, cut from A
	branchPins := k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx))
	f := &rollForgejo{behind: 1, branches: map[string]string{"main": "main-B", "factory/t-roll": "roll-cut-1"},
		files: map[string]map[string]string{forkRepo + "@main": mainPins(), forkRepo + "@factory/t-roll": branchPins},
		blobs: map[string]string{forkRepo + "@main k8s/factoryd.yaml": "blob-f", forkRepo + "@main k8s/deployment.yaml": "blob-d",
			forkRepo + "@factory/t-roll k8s/factoryd.yaml": "blob-f", forkRepo + "@factory/t-roll k8s/deployment.yaml": "blob-d"},
		branchPaths: map[string][]string{"factory/t-roll": {"k8s/factoryd.yaml", "k8s/deployment.yaml"}},
		forkParents: map[string][]string{"roll-cut-1": {"main-A"}},
		prs:         map[int64]string{847: "factory/t-roll"}}
	runner, store := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-cut-1",
			Changes: map[string]string{"factoryd": newGw, "gateway": newGw}, Summary: "roll c1c1c1c: factoryd, gateway\n\nbody one"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"worker": newWorker}, Summary: "roll c2c2c2c: worker\n\nbody two"})
	task := store.tasks["t-roll"]
	task.SHA, task.PRNumber = "main-A", 847
	store.tasks["t-roll"] = task

	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "roll-commit-1" {
		t.Fatalf("update commit %q %v", head, err)
	}
	// the branch is based on B, main's head: the replayed roll commit's
	// parent is B, and the fresh pins commit builds on the replayed branch
	if parents := f.forkParents["rebase-1"]; !slices.Equal(parents, []string{"main-B"}) {
		t.Fatalf("the rebased branch's parents %v, want main's head main-B", parents)
	}
	if parents := f.forkParents["roll-commit-1"]; !slices.Equal(parents, []string{"rebase-1"}) {
		t.Fatalf("the fresh commit's parents %v, want the rebased branch", parents)
	}
	if !slices.Equal(f.updates, []string{"#847 rebase factory/t-roll onto main-B"}) {
		t.Fatalf("pull request updates %v, want the one rebase", f.updates)
	}
	// the branch's diff against B is only the pinned files, each holding
	// exactly what the task's requests make of them
	want := k8sManifests(newGw, newGw, newWorker, sha256Of(runnerIdx))
	got := map[string]string{}
	for path, content := range f.files[forkRepo+"@factory/t-roll"] {
		if content != f.files[forkRepo+"@main"][path] {
			got[path] = content
		}
	}
	if !reflect.DeepEqual(got, map[string]string{"k8s/factoryd.yaml": want["k8s/factoryd.yaml"], "k8s/deployment.yaml": want["k8s/deployment.yaml"]}) {
		t.Fatalf("the branch's diff against main %q, want only the pinned files", got)
	}
	// the update changes no digest: the branch pins exactly what each
	// request recorded, each in the manifest its target pins, the rebase
	// and the fresh write between them
	for _, r := range store.rolls {
		for name, digest := range r.Changes {
			target, _ := rollTarget(name)
			ref := registryHost + target.Image + "@" + digest
			if content := f.files[forkRepo+"@factory/t-roll"][target.File]; !strings.Contains(content, ref) {
				t.Fatalf("the branch does not pin %s at %s, the digest run %d rolled", name, digest, r.RunID)
			}
		}
	}
	// the fresh commit writes the worker's pin on the rebased branch, its
	// file at the blob the branch holds
	wantUpdate := map[string]any{"branch": "factory/t-roll", "message": "roll c1c1c1c: factoryd, gateway\n\nbody one\n\nroll c2c2c2c: worker\n\nbody two\n\nFactory-Role: roll\nFactory-Task: t-roll\n",
		"files": []any{
			map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f", "content": base64.StdEncoding.EncodeToString([]byte(want["k8s/factoryd.yaml"]))},
		}}
	if len(f.commits) != 1 || fmt.Sprint(f.commits[0]) != fmt.Sprint(wantUpdate) {
		t.Fatalf("the fresh commit %v\nwant %v", f.commits, wantUpdate)
	}
	if f.branches["factory/t-roll"] != "roll-commit-1" {
		t.Fatalf("branch %v", f.branches)
	}
	if base := store.tasks["t-roll"].SHA; base != "main-B" {
		t.Fatalf("task base %q, want main's head", base)
	}
	for i, r := range store.rolls {
		if !r.Committed || r.Head != "roll-commit-1" {
			t.Fatalf("roll %d %+v, want committed at roll-commit-1", i, r)
		}
	}
	if f.synced != 1 {
		t.Fatalf("synced %d", f.synced)
	}
}

// RollCommit refuses a task that is no roll.
func TestRollCommitRefusesAnAgentTask(t *testing.T) {
	runner, _, _ := newTestRunner(t, Task{ID: "t1", Source: "webhook", Status: "running", Repository: codeRepo, Track: "direct"}, newFakeKube(), &fakeObjectStore{})
	if _, err := execActivity(t, runner, ActivityRollCommit, "t1"); err == nil || !strings.Contains(err.Error(), "no roll task") {
		t.Fatalf("err %v", err)
	}
}

// A roll's model review makes no model call (the test runner's supervisor
// endpoint fails the test on any call): the card reads the note.
func TestModelReviewOfARollCallsNoModel(t *testing.T) {
	runner, _, _ := newTestRunner(t, Task{ID: "t-roll", Source: SourceRoll, Status: "in-review", Repository: codeRepo, Kind: "agent",
		RequiredCapabilities: []string{"coder"}, Track: "direct"}, newFakeKube(), &fakeObjectStore{})
	var rv Review
	out, err := execActivity(t, runner, ActivityModelReview, "t-roll", PullRequest{Number: 9}, "r1", GateResults{})
	if err != nil || out.Get(&rv) != nil {
		t.Fatal(err)
	}
	if rv.Verdict != "" || rv.Note != "no model review: a roll task only bumps image digests main CI built, and makes no model call (ruling 178)" {
		t.Fatalf("review %+v", rv)
	}
}

// A green commit that changes nothing outside k8s/ against its first
// parent (a roll's own merge, a hand roll) is skipped before any image is
// read: a cold build cache cannot make it look like a change.
func TestRollerSkipsACommitThatChangesOnlyK8s(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(62, commitC2, "success")},
		statuses: imageGreen(commitC2),
		touched:  map[string][]string{commitC2: {"k8s/factoryd.yaml", "k8s/deployment.yaml", "k8s/factoryd.yaml"}}}
	roller, store, wf := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || store.rolls[0].State != RollSkipped || store.rolls[0].RunID != 62 || len(wf.started) != 0 {
		t.Fatalf("rolls %+v, starts %v", store.rolls, wf.started)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "GET /v2/") || strings.Contains(c, "/raw/") {
			t.Fatalf("read images or manifests for a k8s-only commit: %v", f.calls)
		}
	}
	if !strings.Contains(logs.String(), commitC2+" (run 62) changes nothing outside k8s/") {
		t.Fatalf("log %q", logs.String())
	}
}

// A code merge C1 is followed by a roll merge R1 of C1, whose push cancels
// C1's own main CI run (item 126): comparing R1 with the commit the last
// merged roll rolled (C0), not with R1's first parent (C1, the cancelled
// run's commit, whose code R1's tree already carries), finds C1's code
// change, so R1's images, which hold it, are rolled.
func TestRollerRollsACodeMergeWhoseRunTheRollMergeCancelled(t *testing.T) {
	const commitR1 = "9191919191919191919191919191919191919191"
	f := &rollForgejo{
		runs: []map[string]any{pushRun(102, commitR1, "success"), pushRun(101, commitC1, "cancelled")},
		// Against its first parent C1 (the cancelled run's own commit), R1
		// looks k8s-only: C1's code is already in that tree. Against C0,
		// the last merged roll, R1 also carries C1's code change.
		touched:  map[string][]string{commitR1: {"k8s/factoryd.yaml"}},
		rolled:   map[string][]string{commitR1: {"internal/factoryd/x.go", "k8s/factoryd.yaml"}},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitR1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/9191919": gwNewIdx,
			"sf/opencode-worker/manifests/9191919": workerRebuilt,
			"sf/ci-runner/manifests/9191919":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC0, Repository: codeRepo, RunID: 90, State: RollMerged, TaskID: "t-prev", Committed: true}}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitR1 || store.rolls[1].RunID != 102 || store.rolls[1].State != RollOpened || len(wf.started) != 1 {
		t.Fatalf("rolls %+v, starts %v", store.rolls, wf.started)
	}
	if changes := store.rolls[1].Changes; len(changes) != 2 || changes["factoryd"] != sha256Of(gwNewIdx) || changes["gateway"] != sha256Of(gwNewIdx) {
		t.Fatalf("changes %+v, want the gateway image only (worker and runner are rebuilds)", changes)
	}
}

// A roll's own merge R2 of the commit the last merged roll rolled (C0)
// changes nothing outside k8s/ against C0 either: the old skip of a roll's
// own rebuild stays, now measured against the right base.
func TestRollerSkipsARollMergeOfTheLastRolledCommit(t *testing.T) {
	const commitR2 = "9292929292929292929292929292929292929292"
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(103, commitR2, "success")},
		statuses: imageGreen(commitR2),
		touched:  map[string][]string{commitR2: {"k8s/factoryd.yaml"}}}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC0, Repository: codeRepo, RunID: 90, State: RollMerged, TaskID: "t-prev", Committed: true}}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitR2 || store.rolls[1].RunID != 103 || store.rolls[1].State != RollSkipped || len(wf.started) != 0 {
		t.Fatalf("rolls %+v, starts %v", store.rolls, wf.started)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "GET /v2/") || strings.Contains(c, "/raw/") {
			t.Fatalf("read images or manifests for a k8s-only commit: %v", f.calls)
		}
	}
	if !strings.Contains(logs.String(), commitR2+" (run 103) changes nothing outside k8s/") {
		t.Fatalf("log %q", logs.String())
	}
}

// Main CI signs every image the roller rolls (roadmap 46): the build that
// pushes it writes the pushed digest to a metadata file, and the step after
// its job's push step signs exactly that digest, for each image the step
// pushed in its order, with the key from the Actions secrets, run here
// with docker, jq and the cosign it exports standing in. No other step of
// any workflow reads the key: pull request runs never see it.
func TestMainCISignsEveryImageTheRollerRolls(t *testing.T) {
	type step struct {
		Name string            `yaml:"name"`
		Env  map[string]string `yaml:"env"`
		Run  string            `yaml:"run"`
	}
	type workflow struct {
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	read := func(name string) (string, workflow) {
		t.Helper()
		b, err := os.ReadFile("../../.forgejo/workflows/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var w workflow
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		return string(b), w
	}
	raw, ci := read("ci.yml")
	keyEnv := map[string]string{"COSIGN_PRIVATE_KEY": "${{ secrets.COSIGN_PRIVATE_KEY }}", "COSIGN_PASSWORD": "${{ secrets.COSIGN_PASSWORD }}"}
	const digest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	// a push step's images in its order, each by the metadata file its
	// build command writes
	type push struct{ image, metadata string }
	type signing struct {
		job    string
		pushes []push
		sign   step
	}
	pushed := regexp.MustCompile(`name=git\.aisoftwarefactory\.world/sf/([a-z-]+):\$SHA\b.*push=true`)
	metadata := regexp.MustCompile(`--metadata-file (\S+)`)
	var signings []*signing
	for name, j := range ci.Jobs {
		for i, st := range j.Steps {
			var pushes []push
			for _, cmd := range strings.Split(strings.ReplaceAll(st.Run, "\\\n", " "), "\n") {
				if m := pushed.FindStringSubmatch(cmd); m != nil {
					file := metadata.FindStringSubmatch(cmd)
					if file == nil {
						t.Errorf("job %s pushes %s without a --metadata-file", name, m[1])
						continue
					}
					pushes = append(pushes, push{m[1], file[1]})
				}
			}
			if len(pushes) == 0 {
				continue
			}
			if i+1 == len(j.Steps) {
				t.Errorf("job %s: no step follows its push", name)
				continue
			}
			signings = append(signings, &signing{name, pushes, j.Steps[i+1]})
		}
	}
	for _, target := range RollTargets {
		if !slices.ContainsFunc(signings, func(s *signing) bool {
			return slices.ContainsFunc(s.pushes, func(p push) bool { return p.image == target.Image })
		}) {
			t.Errorf("%s: no job of ci.yml pushes git.aisoftwarefactory.world/sf/%s:$SHA", target.Name, target.Image)
		}
	}
	for _, s := range signings {
		if !maps.Equal(s.sign.Env, keyEnv) {
			t.Errorf("job %s's step %q has env %v, want the key's two Actions secrets", s.job, s.sign.Name, s.sign.Env)
		}
		dir := t.TempDir()
		stub := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		stub("docker", `echo "docker $*" >> "$LOG"
[ "$6" = -o ] && mkdir -p "$7" && printf '#!/bin/sh\necho "cosign $* key=$COSIGN_PRIVATE_KEY password=$COSIGN_PASSWORD" >> "$LOG"\n' > "$7/cosign" && chmod +x "$7/cosign"
`)
		stub("jq", `echo "jq $*" >> "$LOG"; echo `+digest+"\n")
		cmd := exec.Command("sh", "-e", "-c", strings.ReplaceAll(s.sign.Run, "/tmp/", dir+"/"))
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":/usr/bin:/bin", "LOG="+filepath.Join(dir, "log"),
			"COSIGN_PRIVATE_KEY=the-key", "COSIGN_PASSWORD=its-password")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("job %s's sign step: %v: %s", s.job, err, out)
		}
		log, err := os.ReadFile(filepath.Join(dir, "log"))
		if err != nil {
			t.Fatal(err)
		}
		want := "docker build -f Dockerfile.runner --target cosign -o " + dir + "/cosign .\n"
		for _, p := range s.pushes {
			want += "jq -r .\"containerimage.digest\" " + strings.ReplaceAll(p.metadata, "/tmp/", dir+"/") + "\n" +
				"cosign sign --yes --key env://COSIGN_PRIVATE_KEY --signing-config " + dir + "/signing-config.json git.aisoftwarefactory.world/sf/" +
				p.image + "@" + digest + " key=the-key password=its-password\n"
		}
		if string(log) != want {
			t.Errorf("job %s's sign step ran\n%s\nwant\n%s", s.job, log, want)
		}
		if config, err := os.ReadFile(filepath.Join(dir, "signing-config.json")); err != nil ||
			string(config) != `{"mediaType":"application/vnd.dev.sigstore.signingconfig.v0.2+json"}` {
			t.Errorf("job %s: signing config %q %v, want one of no services", s.job, config, err)
		}
	}
	if n := strings.Count(raw, "secrets.COSIGN_PRIVATE_KEY"); n != 3 {
		t.Errorf("ci.yml reads the key in %d steps, want the three image jobs' sign steps", n)
	}
	for _, name := range []string{"pr.yml", "nightly.yml"} {
		if other, _ := read(name); strings.Contains(other, "COSIGN_") {
			t.Errorf("%s reads the signing key", name)
		}
	}
}

// Every roll target is pinned exactly once in the repository's own
// manifests: an edit there that breaks the roller fails here.
func TestRollTargetsMatchTheManifests(t *testing.T) {
	for _, target := range RollTargets {
		content, err := os.ReadFile("../../" + target.File)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := pinnedRef(content, target.Image); err != nil {
			t.Errorf("%s in %s: %v", target.Name, target.File, err)
		}
	}
}

// The sandbox base rolls as the factory's other images do: a commit that
// changed only the base files a roll of the base alone, signed by the
// factory's key, and its bump moves SANDBOX_BASE_IMAGE and nothing else.
func TestRollerRollsTheSandboxBase(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
			"sf/sandbox-base/manifests/c1c1c1c":    baseNewIdx,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, _ := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	newBase := sha256Of(baseNewIdx)
	if len(store.rolls) != 1 || !maps.Equal(store.rolls[0].Changes, map[string]string{"sandbox-base": newBase}) ||
		!strings.HasPrefix(store.rolls[0].Summary, "roll c1c1c1c: sandbox-base\n\n") ||
		!strings.Contains(store.rolls[0].Summary, "\n- sandbox-base: "+registryHost+"sandbox-base@"+newBase+"\n") {
		t.Fatalf("roll requests %+v, want one rolling the sandbox base alone", store.rolls)
	}
	pins := mainPins()["k8s/factoryd.yaml"]
	bumped, err := bumpDigest([]byte(pins), "sandbox-base", newBase)
	if want := strings.Replace(pins, "        - name: SANDBOX_BASE_IMAGE\n          value: "+registryHost+"sandbox-base@"+sha256Of(baseIdx)+"\n",
		"        - name: SANDBOX_BASE_IMAGE\n          value: "+registryHost+"sandbox-base@"+newBase+"\n", 1); err != nil || want == pins || string(bumped) != want {
		t.Fatalf("bumped manifest (%v):\n%s\nwant:\n%s", err, bumped, want)
	}
}

// A pin the registry does not hold (the sandbox base's seed, the worker's
// digest under the base's name; or a digest the store collected) is no
// reason to roll nothing: its target counts as changed, the others are
// judged as ever, and the new digest still needs the factory's signature.
func TestRollerRollsAPinTheRegistryDoesNotHold(t *testing.T) {
	seed := sha256Of(workerOldIdx)
	onMain := mainPins()
	onMain["k8s/factoryd.yaml"] = strings.Replace(onMain["k8s/factoryd.yaml"], "sandbox-base@"+sha256Of(baseIdx), "sandbox-base@"+seed, 1)
	for name, c := range map[string]struct {
		signed bool
		state  string
	}{
		"signed":   {true, RollOpened},
		"unsigned": {false, RollRefused},
	} {
		f := &rollForgejo{
			runs:     []map[string]any{pushRun(57, commitC1, "success")},
			files:    map[string]map[string]string{codeRepo + "@main": onMain},
			statuses: imageGreen(commitC1),
			registry: registryOf(map[string]string{
				"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx,
				"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
				"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
				"sf/sandbox-base/manifests/c1c1c1c":    baseIdx,
			}, gwOldIdx, workerOldIdx, runnerIdx),
		}
		if _, held := f.registry["sf/sandbox-base/manifests/"+seed]; held {
			t.Fatal("the registry fixture holds the seed")
		}
		if !c.signed {
			delete(f.registry, "sf/sandbox-base/manifests/sha256-"+strings.TrimPrefix(sha256Of(baseIdx), "sha256:"))
		}
		roller, store, _ := newTestRoller(t, f)
		if err := roller.poll(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(store.rolls) != 1 || store.rolls[0].State != c.state ||
			!maps.Equal(store.rolls[0].Changes, map[string]string{"sandbox-base": sha256Of(baseIdx)}) {
			t.Fatalf("%s: roll requests %+v, want one %s rolling the sandbox base alone", name, store.rolls, c.state)
		}
	}
}

// Every agent task's token writes the bot's fork, a roll's branch included:
// a head that is not the commit RollCommit wrote is refused, for a fold and
// for a resumed run, so no agent commit rides under a roll's card.
func TestRollCommitRefusesABranchItDidNotWrite(t *testing.T) {
	branch := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
	for name, pending := range map[string]bool{"fold": true, "resumed": false} {
		f := &rollForgejo{branches: map[string]string{"factory/t-roll": "pushed-by-an-agent"},
			files: map[string]map[string]string{forkRepo + "@factory/t-roll": branch}}
		rolls := []RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1",
			Changes: map[string]string{"gateway": sha256Of(gwNewIdx)}}}
		if pending {
			rolls = append(rolls, RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
				Changes: map[string]string{"worker": sha256Of(workerNewIdx)}})
		}
		runner, _ := newRollRunner(t, f, rolls...)
		_, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
		if err == nil || !strings.Contains(err.Error(), "factory/t-roll is at pushed-by-an-agent, not at roll-commit-1, the commit factoryd wrote") ||
			!strings.Contains(err.Error(), errTerminal) {
			t.Fatalf("%s: err %v", name, err)
		}
		if len(f.commits) != 0 {
			t.Fatalf("%s: committed on a foreign head: %v", name, f.commits)
		}
	}
}

// An empty commit (the recovery after a failed roll) changes nothing
// under k8s/ either: its images are compared, not skipped unread.
func TestRollerComparesTheImagesOfAnEmptyCommit(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(63, commitC2, "success")}, touched: map[string][]string{commitC2: {}},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()}, statuses: imageGreen(commitC2),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c2c2c2c": gwNewIdx,
			"sf/opencode-worker/manifests/c2c2c2c": workerRebuilt,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx)}
	roller, store, wf := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || store.rolls[0].State != RollOpened || len(wf.started) != 1 || strings.Contains(logs.String(), "outside k8s/") {
		t.Fatalf("rolls %+v, starts %v, log %q", store.rolls, wf.started, logs.String())
	}
}

// A code commit A whose run builds slowly, then B, a roll's own merge (a
// hand roll) that changes nothing outside k8s/ against the commit the last
// merged roll rolled: B goes green first and is skipped; A's run,
// finishing later with the lower id, is still judged and opens the roll of
// A's code.
func TestRollerRollsACodeCommitWhoseRunFinishesAfterAK8sOnlyOne(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(101, commitC2, "success"), pushRun(100, commitC1, "running")},
		touched:  map[string][]string{commitC2: {"k8s/factoryd.yaml"}},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC2),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC0, Repository: codeRepo, RunID: 90, State: RollMerged, TaskID: "t-prev", Committed: true}}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitC2 || store.rolls[1].State != RollSkipped || len(wf.started) != 0 {
		t.Fatalf("after B: rolls %+v, starts %v", store.rolls, wf.started)
	}
	f.runs = []map[string]any{pushRun(101, commitC2, "success"), pushRun(100, commitC1, "success")}
	f.statuses = imageGreen(commitC2, commitC1) // the slow run's images landed
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 3 || store.rolls[2].SHA != commitC1 || store.rolls[2].State != RollOpened || len(wf.started) != 1 ||
		len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "roll c1c1c1c: factoryd, gateway\n\n") {
		t.Fatalf("after A: rolls %+v, starts %v, issues %q", store.rolls, wf.started, f.issues)
	}
}

// A green run older than a commit a roll carries is never judged: its
// images would roll main back.
func TestRollerNeverJudgesARunOlderThanARolledOne(t *testing.T) {
	f := &rollForgejo{runs: []map[string]any{pushRun(104, commitC1, "success")}}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: "b0b0", Repository: codeRepo, RunID: 90, State: RollSkipped},
		{SHA: commitC0, Repository: codeRepo, RunID: 105, State: RollMerged, TaskID: "t-prev", Committed: true}}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the runs read alone; nothing judged, and the chain's bootstrap
	// reads belong to the chain poller now
	if len(store.rolls) != 2 || len(wf.started) != 0 || len(f.calls) != 1 {
		t.Fatalf("rolls %+v, starts %v, calls %v", store.rolls, wf.started, f.calls)
	}
}

// A RollCommit that died between its write and its record finds its own
// commit on the retry: the head's parent is the recorded head (or the
// base the branch was cut from), it changes the roll's files only, and
// they hold exactly what the pending requests make. It adopts it. A head
// that changes anything more is refused.
func TestRollCommitAdoptsItsOwnUnrecordedCommit(t *testing.T) {
	newGw := sha256Of(gwNewIdx)
	first := k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx))
	folded := k8sManifests(newGw, newGw, sha256Of(workerNewIdx), sha256Of(runnerIdx))
	fold := RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
		Changes: map[string]string{"worker": sha256Of(workerNewIdx)}, Summary: "roll c2c2c2c: worker"}
	for name, tc := range map[string]struct {
		rolls  []RollRequest
		parent string
		base   map[string]string
		diff   []string
		adopt  bool
	}{
		"fold":              {[]RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"}, fold}, "roll-commit-1", first, []string{"k8s/factoryd.yaml"}, true},
		"first commit":      {[]RollRequest{fold}, "fork-main-head", k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx)), []string{"k8s/factoryd.yaml"}, true},
		"with agent code":   {[]RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"}, fold}, "roll-commit-1", first, []string{"k8s/factoryd.yaml", "internal/factoryd/gates.go"}, false},
		"on another parent": {[]RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"}, fold}, "agent-commit", first, []string{"k8s/factoryd.yaml"}, false},
	} {
		f := &rollForgejo{branches: map[string]string{"factory/t-roll": "crashed-write"},
			forkParents: map[string][]string{"crashed-write": {tc.parent}},
			forkDiffs:   map[string][]string{tc.parent + "...crashed-write": tc.diff},
			files:       map[string]map[string]string{forkRepo + "@" + tc.parent: tc.base, forkRepo + "@crashed-write": folded}}
		runner, store := newRollRunner(t, f, tc.rolls...)
		task := store.tasks["t-roll"]
		task.SHA = "fork-main-head"
		store.tasks["t-roll"] = task
		out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
		if !tc.adopt {
			if err == nil || !strings.Contains(err.Error(), "the commit factoryd wrote") {
				t.Fatalf("%s: err %v", name, err)
			}
			continue
		}
		var head string
		if err != nil || out.Get(&head) != nil || head != "crashed-write" || len(f.commits) != 0 {
			t.Fatalf("%s: head %q err %v commits %v", name, head, err, f.commits)
		}
		if got, _ := store.RollHead(context.Background(), "t-roll"); got != "crashed-write" {
			t.Fatalf("%s: recorded head %q", name, got)
		}
	}
}

// The queue's staging merge advanced main into the roll's branch after
// factoryd wrote its commit: the tip is a two-parent merge carrying the
// recorded commit, not that commit itself. The guard walks the ancestry,
// finds factoryd's commit and adopts the tip; no commit is written.
func TestRollCommitAcceptsAQueueStagedMerge(t *testing.T) {
	newGw := sha256Of(gwNewIdx)
	folded := k8sManifests(newGw, newGw, sha256Of(workerNewIdx), sha256Of(runnerIdx))
	f := &rollForgejo{branches: map[string]string{"factory/t-roll": "staged-merge"},
		forkParents: map[string][]string{"staged-merge": {"roll-commit-1", "fork-main-head"}},
		files:       map[string]map[string]string{forkRepo + "@staged-merge": folded}}
	runner, store := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"worker": sha256Of(workerNewIdx)}})
	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "staged-merge" {
		t.Fatalf("head %q err %v", head, err)
	}
	if len(f.commits) != 0 {
		t.Fatalf("committed on a staged branch: %v", f.commits)
	}
	if got, _ := store.RollHead(context.Background(), "t-roll"); got != "staged-merge" {
		t.Fatalf("recorded head %q, want the merged tip", got)
	}
	for _, r := range store.rolls {
		if !r.Committed {
			t.Fatalf("roll %+v left uncommitted", r)
		}
	}
}

// Two RollCommit runs overlapped (the later attempt read the branch
// before the earlier one's write landed and the store after its record):
// the branch sits at the roll's FIRST commit while the store's newest
// marking is past it. That head is factoryd's own, so the guard does not
// fail the task: the marking past the branch goes back to pending and the
// update is written as the branch rebased onto main's head with one
// fresh commit of every pin on it, recorded at the head actually written.
func TestRollCommitWritesAFoldOnARecordedOwnHead(t *testing.T) {
	folded := k8sManifests(sha256Of(gwOldIdx), sha256Of(gwNewIdx), sha256Of(workerNewIdx), sha256Of(runnerRebuilt))
	branchPins := k8sManifests(sha256Of(gwOldIdx), sha256Of(gwNewIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
	f := &rollForgejo{behind: 1, branches: map[string]string{"main": "fork-main-head", "factory/t-roll": "roll-commit-0"},
		files: map[string]map[string]string{forkRepo + "@main": mainPins(), forkRepo + "@factory/t-roll": branchPins},
		blobs: map[string]string{forkRepo + "@main k8s/factoryd.yaml": "blob-f", forkRepo + "@main k8s/deployment.yaml": "blob-d",
			forkRepo + "@factory/t-roll k8s/deployment.yaml": "blob-d"},
		branchPaths: map[string][]string{"factory/t-roll": {"k8s/deployment.yaml"}},
		forkParents: map[string][]string{"roll-commit-0": {"fork-main-head"}},
		prs:         map[int64]string{847: "factory/t-roll"}}
	runner, store := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-0",
			Changes: map[string]string{"gateway": sha256Of(gwNewIdx)}, Summary: "roll c1c1c1c: gateway"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-2",
			Changes: map[string]string{"worker": sha256Of(workerNewIdx)}, Summary: "roll c2c2c2c: worker"},
		RollRequest{SHA: commitHR, Repository: codeRepo, RunID: 63, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"ci-runner": sha256Of(runnerRebuilt)}, Summary: "roll c3c3c3c: runner"})
	task := store.tasks["t-roll"]
	task.PRNumber = 847
	store.tasks["t-roll"] = task
	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "roll-commit-1" {
		t.Fatalf("head %q err %v", head, err)
	}
	if !slices.Equal(f.updates, []string{"#847 rebase factory/t-roll onto fork-main-head"}) {
		t.Fatalf("pull request updates %v, want one rebase onto main's head", f.updates)
	}
	if parents := f.forkParents["rebase-1"]; !slices.Equal(parents, []string{"fork-main-head"}) {
		t.Fatalf("the rebased branch's parents %v, want main's head", parents)
	}
	if parents := f.forkParents["roll-commit-1"]; !slices.Equal(parents, []string{"rebase-1"}) {
		t.Fatalf("the fresh commit's parents %v, want the rebased branch", parents)
	}
	// the branch's diff against main is only the pinned files
	got := map[string]string{}
	for path, content := range f.files[forkRepo+"@factory/t-roll"] {
		if content != f.files[forkRepo+"@main"][path] {
			got[path] = content
		}
	}
	if !reflect.DeepEqual(got, map[string]string{"k8s/factoryd.yaml": folded["k8s/factoryd.yaml"], "k8s/deployment.yaml": folded["k8s/deployment.yaml"]}) {
		t.Fatalf("the branch's diff against main %q, want only the pinned files", got)
	}
	want := map[string]any{"branch": "factory/t-roll", "message": "roll c1c1c1c: gateway\n\nroll c2c2c2c: worker\n\nroll c3c3c3c: runner\n\nFactory-Role: roll\nFactory-Task: t-roll\n",
		"files": []any{
			map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f", "content": base64.StdEncoding.EncodeToString([]byte(folded["k8s/factoryd.yaml"]))},
		}}
	if len(f.commits) != 1 || fmt.Sprint(f.commits[0]) != fmt.Sprint(want) {
		t.Fatalf("the fresh commit %v\nwant %v", f.commits, want)
	}
	for _, r := range store.rolls {
		if !r.Committed || r.Head != "roll-commit-1" {
			t.Fatalf("roll %+v, want committed at roll-commit-1", r)
		}
	}
	if got, _ := store.RollHead(context.Background(), "t-roll"); got != "roll-commit-1" {
		t.Fatalf("recorded head %q", got)
	}
}

// The rebase of a roll's branch onto main's head can conflict (a hand
// roll pinned the same lines on main): the roller closes the superseded
// pull request with a note, cuts the branch from main's head again —
// deleted and written as its first commit, every pin of the task on
// main's head — files a fresh pull request for it and names it on the
// roll card (roadmap 279).
func TestRollCommitRefilesTheRollWhenTheRebaseConflicts(t *testing.T) {
	newGw, newWorker := sha256Of(gwNewIdx), sha256Of(workerNewIdx)
	branchPins := k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx))
	f := &rollForgejo{behind: 1, updateConflict: true,
		branches:    map[string]string{"main": "main-B", "factory/t-roll": "roll-cut-1"},
		files:       map[string]map[string]string{forkRepo + "@main": mainPins(), forkRepo + "@factory/t-roll": branchPins},
		blobs:       map[string]string{forkRepo + "@main k8s/factoryd.yaml": "blob-f", forkRepo + "@main k8s/deployment.yaml": "blob-d"},
		forkParents: map[string][]string{"roll-cut-1": {"main-A"}},
		prs:         map[int64]string{847: "factory/t-roll"}}
	runner, store := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-cut-1",
			Changes: map[string]string{"factoryd": newGw, "gateway": newGw}, Summary: "roll c1c1c1c: factoryd, gateway\n\nbody one"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"worker": newWorker}, Summary: "roll c2c2c2c: worker\n\nbody two"})
	task := store.tasks["t-roll"]
	task.SHA, task.PRNumber, task.IssueNumber = "main-A", 847, 31
	task.Objective = "roll c1c1c1c: factoryd, gateway\n\nbody one"
	store.tasks["t-roll"] = task

	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "roll-commit-1" {
		t.Fatalf("fresh commit %q %v", head, err)
	}
	// the superseded pull request closed with a note
	if len(f.comments) != 2 || !strings.HasPrefix(f.comments[0], "847 factoryd: closing this roll: its branch cannot be rebased onto main's head") {
		t.Fatalf("comments %q, want the note on the superseded pull request", f.comments)
	}
	if !slices.Equal(f.closedPRs, []int64{847}) {
		t.Fatalf("closed pull requests %v", f.closedPRs)
	}
	if !slices.Equal(f.deleted, []string{"factory/t-roll"}) {
		t.Fatalf("deleted branches %v", f.deleted)
	}
	// the branch cut from main's head again: its first commit written on
	// main, every pin of the task in it
	folded := k8sManifests(newGw, newGw, newWorker, sha256Of(runnerIdx))
	want := map[string]any{"branch": "main", "new_branch": "factory/t-roll", "message": "roll c1c1c1c: factoryd, gateway\n\nbody one\n\nroll c2c2c2c: worker\n\nbody two\n\nFactory-Role: roll\nFactory-Task: t-roll\n",
		"files": []any{
			map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f", "content": base64.StdEncoding.EncodeToString([]byte(folded["k8s/factoryd.yaml"]))},
			map[string]any{"operation": "update", "path": "k8s/deployment.yaml", "sha": "blob-d", "content": base64.StdEncoding.EncodeToString([]byte(folded["k8s/deployment.yaml"]))},
		}}
	if len(f.commits) != 1 || fmt.Sprint(f.commits[0]) != fmt.Sprint(want) {
		t.Fatalf("the fresh commit %v\nwant %v", f.commits, want)
	}
	if parents := f.forkParents["roll-commit-1"]; !slices.Equal(parents, []string{"main-B"}) {
		t.Fatalf("the fresh branch's parents %v, want main's head main-B", parents)
	}
	if f.branches["factory/t-roll"] != "roll-commit-1" {
		t.Fatalf("branch %v", f.branches)
	}
	// a fresh pull request was filed for the branch, and the roll card
	// names it
	if len(f.pulls) != 1 || f.pulls[0] != "factory-agent:factory/t-roll roll c1c1c1c: factoryd, gateway" {
		t.Fatalf("pull requests %q", f.pulls)
	}
	if pr := store.tasks["t-roll"].PRNumber; pr != 71 {
		t.Fatalf("task pull request %d, want the fresh 71", pr)
	}
	if !strings.Contains(f.comments[1], "31 factoryd: main moved and this roll's branch could not be rebased onto its head; pull request #847 is superseded by #71") {
		t.Fatalf("the card's comment %q, want it to name the fresh pull request", f.comments[1])
	}
	for i, r := range store.rolls {
		if !r.Committed || r.Head != "roll-commit-1" {
			t.Fatalf("roll %d %+v, want committed at roll-commit-1", i, r)
		}
	}
	if base := store.tasks["t-roll"].SHA; base != "main-B" {
		t.Fatalf("task base %q, want main's head", base)
	}
}

// A tip that does not carry factoryd's commit (a foreign commit on the
// base) refuses with the existing message shape.
func TestRollCommitRefusesATipWithoutFactorydsCommit(t *testing.T) {
	folded := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerNewIdx), sha256Of(runnerIdx))
	f := &rollForgejo{branches: map[string]string{"factory/t-roll": "rebased-by-an-agent"},
		forkParents: map[string][]string{"rebased-by-an-agent": {"agent-base-commit"}},
		files:       map[string]map[string]string{forkRepo + "@rebased-by-an-agent": folded}}
	runner, _ := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"worker": sha256Of(workerNewIdx)}})
	_, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err == nil || !strings.Contains(err.Error(), "factory/t-roll is at rebased-by-an-agent, not at roll-commit-1, the commit factoryd wrote") ||
		!strings.Contains(err.Error(), errTerminal) {
		t.Fatalf("err %v", err)
	}
	if len(f.commits) != 0 {
		t.Fatalf("committed on a foreign tip: %v", f.commits)
	}
}

// Several green runs waiting and no roll open: the newest files the roll,
// which carries the older commits' code; no fold follows, and the older
// run falls below the roll.
func TestRollerFilesTheNewestOfSeveralGreenRuns(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(102, commitC2, "success"), pushRun(101, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC2, commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx, "sf/opencode-worker/manifests/c1c1c1c": workerRebuilt, "sf/ci-runner/manifests/c1c1c1c": runnerRebuilt,
			"sf/factory-gateway/manifests/c2c2c2c": gwNewRebuilt, "sf/opencode-worker/manifests/c2c2c2c": workerNewIdx, "sf/ci-runner/manifests/c2c2c2c": runnerRebuilt2,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.rolls = []RollRequest{{SHA: "b0b0", Repository: codeRepo, RunID: 90, State: RollSkipped}}
	for range 2 {
		if err := roller.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitC2 || store.rolls[1].State != RollOpened || len(wf.started) != 1 || len(wf.signals) != 0 ||
		len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "roll c2c2c2c: factoryd, gateway, worker\n\n") {
		t.Fatalf("rolls %+v, starts %v, signals %v, issues %q", store.rolls, wf.started, wf.signals, f.issues)
	}
}

// The roller reports each verdict as the factory/roll commit status
// (roadmap 297): pending while the roll task it filed carries the commit,
// then success once the task settles merged — the status the factory's
// chain requires.
func TestRollerReportsItsVerdictAsACommitStatus(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, _ := newTestRoller(t, f)
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{commitC1 + " pending factory/roll"}
	if !slices.Equal(f.reported, want) {
		t.Fatalf("reported %v, want %v", f.reported, want)
	}
	// the roll task ends done with its request committed: the next poll
	// settles it and reports the commit live
	id := store.rolls[0].TaskID
	store.tasks[id] = Task{ID: id, Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls[0].Committed = true
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	want = append(want, commitC1+" success factory/roll")
	if !slices.Equal(f.reported, want) {
		t.Fatalf("reported %v, want %v", f.reported, want)
	}
}

// A roll task that ends failed (its rollout bound passed or its pull
// request closed) settles its request failed, and the settle posts the
// failure on the commit: the one writer of that outcome.
func TestARollTaskThatFailsEndsItsCommitsStatusFailed(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, _ := newTestRoller(t, f)
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	id := store.rolls[0].TaskID
	store.tasks[id] = Task{ID: id, Source: SourceRoll, Status: "failed", Repository: codeRepo}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{commitC1 + " pending factory/roll", commitC1 + " failure factory/roll"}
	if !slices.Equal(f.reported, want) || store.rolls[0].State != RollFailed {
		t.Fatalf("reported %v, request %s, want %v and failed", f.reported, store.rolls[0].State, want)
	}
}

// The roll ladder's end: when a roll request settles failed and three
// have failed since the last merged roll, the roller asks a person — the
// five-part question, Retry the roll / Park / Close as its buttons —
// once: while the question stands no second card files.
func TestRollEscalatesAfterAttempts(t *testing.T) {
	f := &rollForgejo{files: map[string]map[string]string{codeRepo + "@main": mainPins()}}
	roller, store, _ := newTestRoller(t, f)
	rows := &fakeEventRecorder{}
	roller.Timeline = &Timeline{Store: store, Events: rows}
	ctx := context.Background()
	seed := func(id, sha string, run int64, request string) {
		store.tasks[id] = Task{ID: id, Source: SourceRoll, Status: "failed", Repository: codeRepo, IssueNumber: run - 20}
		store.rolls = append(store.rolls, RollRequest{SHA: sha, Repository: codeRepo, RunID: run,
			State: request, TaskID: id, Summary: fmt.Sprintf("roll %s: worker\n\nMain CI's push run %d published the images.", sha[:7], run)})
	}
	seed("t-r1", "1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f", 51, RollFailed)
	seed("t-r2", "2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f", 52, RollFailed)
	seed("t-r3", "3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f", 53, RollOpened) // ends this poll
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	var asks []Task
	for _, t := range store.tasks {
		if t.Kind == "human" && strings.Contains(t.Objective, "rolls failed since the last one merged") {
			asks = append(asks, t)
		}
	}
	if len(asks) != 1 || asks[0].Status != "waiting-human" {
		t.Fatalf("needs-you cards %+v, want one waiting", asks)
	}
	// factoryd's ask, never the chat agent's: its row wakes the project
	// agent, which attaches its analysis
	if asks[0].Source != SourceRollFailed || strings.Contains(asks[0].Objective, "agent asked") ||
		!strings.Contains(asks[0].Objective, "factoryd asks (the roller)") {
		t.Fatalf("the card %+v, want factoryd's roll-failed ask", asks[0])
	}
	var asked []Event
	for _, e := range rows.rows {
		if e.Kind == EventQuestionAsked {
			asked = append(asked, e)
		}
	}
	if len(asked) != 1 || asked[0].Via != ViaFactoryd || asked[0].Detail["source"] != SourceRollFailed {
		t.Fatalf("question_asked rows %+v, want factoryd's", asked)
	}
	for _, want := range []string{"Choices:", "- Retry the roll", "- Park", "- Close", "card #31"} {
		if !strings.Contains(asks[0].Objective, want) {
			t.Fatalf("the question misses %q:\n%s", want, asks[0].Objective)
		}
	}
	var q *HumanQuestion
	for _, e := range store.intakeEvents {
		if e.Kind == IntakeEscalation {
			q = e.Question
		}
	}
	if q == nil || q.ChoiceValue("Retry the roll") != "retry" {
		t.Fatalf("question %+v, want its buttons' values", q)
	}
	// a fourth failure settles while the question stands: no second card
	seed("t-r4", "4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f", 54, RollOpened)
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.issues); n != 1 {
		t.Fatalf("%d issues filed, want the one question", n)
	}
}

// The question's Retry the roll answer acts: the failed attempts'
// requests go — their commits unjudged again — the question's card closes
// as factoryd's own, and the same poll's judge files one fresh roll of
// the newest green images. A third poll consumes the answer once.
func TestRollRetryAnswerStartsAFreshRoll(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(52, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	ctx := context.Background()
	seed := func(id, sha string, run int64, request string) {
		store.tasks[id] = Task{ID: id, Source: SourceRoll, Status: "failed", Repository: codeRepo, IssueNumber: run - 20}
		store.rolls = append(store.rolls, RollRequest{SHA: sha, Repository: codeRepo, RunID: run, State: request, TaskID: id})
	}
	seed("t-r1", "1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f", 50, RollFailed)
	seed("t-r2", "2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f", 51, RollFailed)
	seed("t-r3", commitC1, 52, RollOpened) // ends this poll
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	var ask Task
	for _, t := range store.tasks {
		if t.Kind == "human" && strings.Contains(t.Objective, rollAskMarker) {
			ask = t
		}
	}
	if ask.ID == "" || ask.Status != "waiting-human" || ask.IssueNumber != 31 {
		t.Fatalf("the question's card %+v, want the waiting ask on issue #31", ask)
	}
	// the member's click: the label posted as their trusted comment
	store.intakeEvents = append(store.intakeEvents,
		IntakeEvent{TaskID: ask.ID, Kind: "issue_comment", Author: "sebastian", Body: "Retry the roll", Trusted: true})
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.closed, []string{"31"}) {
		t.Fatalf("closed %v, want the question's issue", f.closed)
	}
	if len(store.rolls) != 1 || store.rolls[0].SHA != commitC1 || store.rolls[0].State != RollOpened {
		t.Fatalf("roll requests %+v, want one fresh open roll of %s", store.rolls, commitC1)
	}
	task := store.tasks[store.rolls[0].TaskID]
	if task.Source != SourceRoll || task.Status != "queued" || !slices.Equal(wf.started, []string{task.ID}) {
		t.Fatalf("the fresh roll task %+v, started %v", task, wf.started)
	}
	// the answer consumes once: a third poll closes, comments and files nothing again
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.closed, []string{"31"}) || len(f.comments) != 1 || len(f.issues) != 2 || len(wf.started) != 1 {
		t.Fatalf("third poll: closed %v, %d comments, %d issues, started %v", f.closed, len(f.comments), len(f.issues), wf.started)
	}
}

// A roll's rollout wait reads the factory's Deployments through the Kube
// the runner takes: the merge moved factoryd's gateway image, and the
// factoryd Deployment carrying it, available after Flux applied it,
// confirms the rollout.
func TestAwaitRolloutReadsTheDeploymentThroughTheKube(t *testing.T) {
	kube := newFakeKube()
	r, _, _ := newTestRunner(t, Task{ID: "t1", Status: "running", Kind: "agent", Repository: "sf/repo", PRNumber: 9, Source: SourceRoll, Track: "direct"}, kube, &fakeObjectStore{})
	pinned := func(image, digest string) string { return registryHost + image + "@" + digest }
	factorydAt := func(gateway string) string {
		return "image: " + pinned("factory-gateway", gateway) + "\nworker: " + pinned("opencode-worker", digestOld) + "\nbase: " + pinned("sandbox-base", digestOld) +
			"\nci: " + pinned("ci-runner", digestOld) + "\n"
	}
	files := map[string]string{
		"k8s/factoryd.yaml m9":   factorydAt(digestNew),
		"k8s/factoryd.yaml p9":   factorydAt(digestOld),
		"k8s/deployment.yaml m9": "image: " + pinned("factory-gateway", digestOld) + "\n",
		"k8s/deployment.yaml p9": "image: " + pinned("factory-gateway", digestOld) + "\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch p := req.URL.Path; {
		case p == "/api/v1/repos/sf/repo/pulls/9":
			io.WriteString(w, mergedAt(9, "m9"))
		case p == "/api/v1/repos/sf/repo/git/commits/m9":
			io.WriteString(w, `{"parents":[{"sha":"p9"}]}`)
		case strings.HasPrefix(p, "/api/v1/repos/sf/repo/raw/"):
			body, ok := files[strings.TrimPrefix(p, "/api/v1/repos/sf/repo/raw/")+" "+req.URL.Query().Get("ref")]
			if !ok {
				http.NotFound(w, req)
				return
			}
			io.WriteString(w, body)
		default:
			t.Errorf("unexpected call %s %s", req.Method, req.URL)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	r.forgejo = &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	kube.deployments = map[string]string{
		"factory/factoryd": deploymentJSON("2026-10-20T10:00:00Z", "2026-10-20T10:01:00Z", pinned("factory-gateway", digestNew)),
	}
	val, err := execActivity(t, r, ActivityAwaitRollout, "t1", int64(9), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var res RolloutResult
	if err := val.Get(&res); err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed || !slices.Equal(res.Live, []string{"factoryd"}) {
		t.Fatalf("rollout = %+v, want confirmed with factoryd live", res)
	}
}
