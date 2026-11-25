package factoryd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	infraName   = "ai-software-factory-infra"
	followAPI    = "/api/v1/repos/sf/" + infraName
	followForkAPI = "/api/v1/repos/factory-agent/" + infraName
	infraForkID = "factory-agent/" + infraName
)

func zeros(c string) string { return strings.Repeat(c, 64) }

// followLists are the two upstream lists of the fixture tree: litellm 1.108.17
// and registry 3 with their digests.
func followLists() map[string]string {
	return map[string]string{
		"upstream-images.txt": "ghcr.io/berriai/litellm:1.108.17@sha256:" + zeros("d") + " patch\n" +
			"docker.io/library/registry:3@sha256:" + zeros("8") + "\n",
		"upstream-files.txt": "node v24.22.3 node-v24.22.3-linux-x64.tar.xz https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz " + zeros("a") + "\n",
	}
}

const (
	litellmFile  = "k8s/platform/litellm/helmrelease.yaml"
	registryFile = "k8s/platform/build-cache/registry.yaml"
)

// litellmAt and registryAt are the files with the references at the given
// digest letter and version.
func litellmAt(tag, d string) string {
	return "# pin ghcr.io/berriai/litellm\n      tag: \"" + tag + "@sha256:" + zeros(d) + "\"\n"
}

func registryAt(d string) string {
	return "# pin docker.io/library/registry\nimage: reg/registry:3@sha256:" + zeros(d) + "\n"
}

// infraTarball is the archive Forgejo makes of the infra repository: every
// file under the repository's own directory.
func infraTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: infraName + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: infraName + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakePull is a pull request of the infra repository as the forge holds it.
type fakePull struct {
	Number            int64
	Head, State       string
	Merged            bool
	HeadSHA, MergeSHA string
}

// infraForge is the infra repository and the bot's fork as the follow pass
// and its moves use them. Each call joins log.
type infraForge struct {
	t        *testing.T
	mu       sync.Mutex
	log      []string
	tar      []byte
	main     string
	pulls    map[int64]*fakePull
	byBranch map[string]int64
	statuses map[string][]CommitStatus // by commit sha
	commits  []map[string]any          // POST /contents on the fork
	bodies   map[int64]string          // the text of each pull request opened
	titles   map[int64]string
	// mergeStatus is what the statuses of a merge commit become once the
	// pull request merges: merge<n> gets it
	mergeStatus []CommitStatus
	mergeFails  bool
	// notMergeable keeps Forgejo's mergeable flag false
	revert func(branch string)
}

func newInfraForge(t *testing.T, files map[string]string) *infraForge {
	t.Helper()
	return &infraForge{t: t, tar: infraTarball(t, files), main: "m1", pulls: map[int64]*fakePull{}, byBranch: map[string]int64{},
		statuses: map[string][]CommitStatus{}, bodies: map[int64]string{}, titles: map[int64]string{}}
}

// attempt is a pull request of a move's branch that exists already.
func (f *infraForge) attempt(branch, state string, merged bool) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int64(10 + len(f.pulls))
	f.pulls[n] = &fakePull{Number: n, Head: branch, State: state, Merged: merged, HeadSHA: "head" + strconv.FormatInt(n, 10)}
	f.byBranch[branch] = n
	return n
}

func (f *infraForge) prJSON(p *fakePull) map[string]any {
	return map[string]any{"number": p.Number, "state": p.State, "merged": p.Merged, "mergeable": true, "merge_commit_sha": p.MergeSHA,
		"html_url": fmt.Sprintf("https://git.example/sf/%s/pulls/%d", infraName, p.Number), "title": f.titles[p.Number], "body": f.bodies[p.Number],
		"base": map[string]string{"ref": "main"}, "merged_by": nil,
		"head": map[string]any{"ref": p.Head, "sha": p.HeadSHA, "repo": map[string]string{"full_name": infraForkID}}}
}

func (f *infraForge) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.log = append(f.log, r.Method+" "+r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		json.Unmarshal(raw, &in) // #nosec G104 -- test stub
		answer := func(status int, v any) {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(v) // #nosec G104 -- test stub
		}
		p := r.URL.Path
		switch {
		case p == followAPI+"/branches/main":
			answer(http.StatusOK, map[string]any{"name": "main", "commit": map[string]string{"id": f.main}})
		case strings.HasPrefix(p, followAPI+"/archive/"):
			w.Write(f.tar) // #nosec G104 -- test stub
		case p == followForkAPI && r.Method == http.MethodGet:
			answer(http.StatusOK, map[string]any{"fork": true, "private": true, "has_actions": false, "parent": map[string]string{"full_name": "sf/" + infraName}})
		case p == followForkAPI+"/sync_fork":
			answer(http.StatusOK, map[string]any{"allowed": false, "commits_behind": 0})
		case strings.HasPrefix(p, followForkAPI+"/contents/"):
			file := strings.TrimPrefix(p, followForkAPI+"/contents/")
			answer(http.StatusOK, map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("old " + file)), "encoding": "base64", "sha": "blob-" + file})
		case p == followForkAPI+"/contents" && r.Method == http.MethodPost:
			f.commits = append(f.commits, in)
			answer(http.StatusCreated, map[string]any{"commit": map[string]string{"sha": "b1"}})
		case strings.HasPrefix(p, followAPI+"/pulls/main/"+infraForkID+":"):
			n, ok := f.byBranch[strings.TrimPrefix(p, followAPI+"/pulls/main/"+infraForkID+":")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			answer(http.StatusOK, f.prJSON(f.pulls[n]))
		case p == followAPI+"/pulls" && r.Method == http.MethodPost:
			head, _ := in["head"].(string)
			branch := strings.TrimPrefix(head, "factory-agent:")
			n := int64(40 + len(f.pulls))
			f.pulls[n] = &fakePull{Number: n, Head: branch, State: "open", HeadSHA: "head" + strconv.FormatInt(n, 10)}
			f.byBranch[branch] = n
			f.bodies[n], _ = in["body"].(string)
			f.titles[n], _ = in["title"].(string)
			answer(http.StatusCreated, f.prJSON(f.pulls[n]))
		case strings.HasPrefix(p, followAPI+"/pulls/") && strings.HasSuffix(p, "/merge"):
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, followAPI+"/pulls/"), "/merge"), 10, 64)
			if f.mergeFails {
				http.Error(w, "no", http.StatusMethodNotAllowed)
				return
			}
			pr := f.pulls[n]
			pr.State, pr.Merged, pr.MergeSHA = "closed", true, "merge"+strconv.FormatInt(n, 10)
			f.main = pr.MergeSHA
			f.statuses[pr.MergeSHA] = append([]CommitStatus(nil), f.mergeStatus...)
			answer(http.StatusOK, map[string]any{})
		case strings.HasPrefix(p, followAPI+"/pulls/") && r.Method == http.MethodPatch:
			n, _ := strconv.ParseInt(strings.TrimPrefix(p, followAPI+"/pulls/"), 10, 64)
			f.pulls[n].State = "closed"
			answer(http.StatusCreated, f.prJSON(f.pulls[n]))
		case strings.HasPrefix(p, followAPI+"/pulls/") && r.Method == http.MethodGet:
			n, _ := strconv.ParseInt(strings.TrimPrefix(p, followAPI+"/pulls/"), 10, 64)
			pr, ok := f.pulls[n]
			if !ok {
				http.NotFound(w, r)
				return
			}
			answer(http.StatusOK, f.prJSON(pr))
		case strings.HasPrefix(p, followAPI+"/commits/") && strings.HasSuffix(p, "/status"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, followAPI+"/commits/"), "/status")
			answer(http.StatusOK, map[string]any{"state": "success", "statuses": statusJSON(f.statuses[sha])})
		default:
			f.t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}
}

func statusJSON(ss []CommitStatus) []map[string]any {
	out := []map[string]any{}
	for _, s := range ss {
		out = append(out, map[string]any{"status": s.State, "context": s.Context, "description": s.Description})
	}
	return out
}

// fakeMoves is the Temporal side of the follow routine in memory.
type fakeMoves struct {
	mu      sync.Mutex
	started []FollowMoveInput
	running *FollowMoveInput
}

func (m *fakeMoves) StartFollowMove(_ context.Context, in FollowMoveInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, in)
	return nil
}

func (m *fakeMoves) RunningFollowMove(context.Context, string) (FollowMoveInput, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		return FollowMoveInput{}, false, nil
	}
	return *m.running, true, nil
}

// followRun is the infra project's runner with its follow routine and the
// parts litellm (alone) and cache (human), both behind in the tree the
// forge archives.
type followRun struct {
	r     *Runner
	store *fakeStore
	forge *infraForge
	moves *fakeMoves
	wf    *fakeWorkflows
	srv   *httptest.Server
}

func followTree(t *testing.T) policy.Tree {
	t.Helper()
	tree := routineTree(t)
	p := tree.Projects[infraName]
	p.Parts = []policy.Part{
		{Name: "litellm", Lane: policy.LaneAlone, Paths: []string{"k8s/platform/litellm"}, Notes: "https://example.org/litellm/releases", After: "The migration does not go back."},
		{Name: "cache", Lane: policy.LaneHuman, Paths: []string{registryFile}, Notes: "https://example.org/registry/releases"},
		{Name: "talos", Lane: policy.LaneHand},
	}
	p.Routines = []policy.Routine{{Name: "follow", Title: "Follow the lists", Files: policy.FilesPullRequest, Pass: policy.PassFollow, Schedule: []policy.Trigger{{Cron: "0 6 * * 2"}}}}
	p.Chain = policy.ChainConfig{Requires: []string{"flux/flux-system", "flux/platform"}, Checks: []string{"config-check"}}
	tree.Projects = map[string]policy.ProjectConfig{infraName: p}
	return tree
}

func newFollowRun(t *testing.T, files map[string]string) *followRun {
	t.Helper()
	tree := followTree(t)
	wf := &fakeWorkflows{}
	store := newFakeStore()
	store.projects = []Project{{Name: infraName, Repository: "sf/" + infraName, File: true, Objects: policy.ObjectsSeed, Agents: policy.AgentsNone, Namespaces: []string{"platform", "forgejo"}}}
	forge := newInfraForge(t, files)
	srv := httptest.NewServer(forge.handler())
	t.Cleanup(srv.Close)
	v := policy.Version{Tree: tree}
	moves := &fakeMoves{}
	r := &Runner{store: store, forgejo: &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"},
		timeline: &Timeline{Objects: &fakeObjectStore{}, Events: &fakeEventRecorder{}, Store: store},
		cfg:      RunnerConfig{Policy: func() policy.Version { return v }, Workflows: wf}, Routines: store,
		Follow: &FollowDeps{Moves: moves, Cards: store}}
	return &followRun{r: r, store: store, forge: forge, moves: moves, wf: wf, srv: srv}
}

// behindTree is the tree with litellm at 1.102.4 and the registry's digest
// behind the lists.
func behindTree() map[string]string {
	files := followLists()
	files[litellmFile] = litellmAt("1.102.4", "0")
	files[registryFile] = registryAt("0")
	return files
}

func (f *followRun) run(t *testing.T) RoutineRunResult {
	t.Helper()
	res, err := f.r.RoutineRun(context.Background(), RoutineRunInput{Project: infraName, Routine: "follow", Trigger: "cron", Due: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// litellmTarget is the changes the move of litellm makes to the fixture tree.
func litellmTarget() map[string][]byte {
	return map[string][]byte{litellmFile: []byte(litellmAt("1.108.17", "d"))}
}

func cacheTarget() map[string][]byte {
	return map[string][]byte{registryFile: []byte(registryAt("8"))}
}

// A run moves the first part that is behind, in the project file's order:
// it commits the part's changes on the bot's fork at a branch the target
// names, opens the pull request with the versions and the notes link, and
// starts the move. Its report line names part, versions and pull request.
func TestFollowPassOpensTheFirstBehindPart(t *testing.T) {
	f := newFollowRun(t, behindTree())
	res := f.run(t)
	if res.Outcome != "reported" || res.Note != "part litellm (alone): ghcr.io/berriai/litellm: 1.102.4 to 1.108.17, pull request #40" {
		t.Fatalf("result = %+v", res)
	}
	branch := followBranch("litellm", litellmTarget(), 1)
	if len(f.forge.commits) != 1 || f.forge.commits[0]["branch"] != "main" || f.forge.commits[0]["new_branch"] != branch {
		t.Fatalf("commits = %v, want one on %s", f.forge.commits, branch)
	}
	files := f.forge.commits[0]["files"].([]any)[0].(map[string]any)
	if got, _ := base64.StdEncoding.DecodeString(files["content"].(string)); files["path"] != litellmFile || string(got) != litellmAt("1.108.17", "d") {
		t.Fatalf("file = %v", files)
	}
	body := f.forge.bodies[40]
	for _, want := range []string{"part litellm, lane alone", "- ghcr.io/berriai/litellm: 1.102.4 to 1.108.17", "Release notes: https://example.org/litellm/releases", "merges it once its checks are green"} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, want %q", body, want)
		}
	}
	want := FollowMoveInput{Project: infraName, Routine: "follow", Run: 1, Part: "litellm", Lane: policy.LaneAlone, PR: 40,
		Moves: []string{"ghcr.io/berriai/litellm: 1.102.4 to 1.108.17"}, Notes: "https://example.org/litellm/releases", After: "The migration does not go back."}
	if len(f.moves.started) != 1 || fmt.Sprint(f.moves.started[0]) != fmt.Sprint(want) {
		t.Fatalf("started = %+v, want %+v", f.moves.started, want)
	}
}

// A target that has its attempt, in any state, is skipped: the next part
// that is behind moves. Once every target has one the run says so.
func TestFollowPassSkipsAnAttemptedTarget(t *testing.T) {
	f := newFollowRun(t, behindTree())
	f.forge.attempt(followBranch("litellm", litellmTarget(), 1), "closed", false)
	res := f.run(t)
	if res.Outcome != "reported" || !strings.HasPrefix(res.Note, "part cache (human): docker.io/library/registry: 3@000000000000 to 3@888888888888, pull request #41") {
		t.Fatalf("result = %+v", res)
	}
	f2 := newFollowRun(t, behindTree())
	f2.forge.attempt(followBranch("litellm", litellmTarget(), 1), "closed", true)
	f2.forge.attempt(followBranch("cache", cacheTarget(), 1), "closed", false)
	res = f2.run(t)
	if res.Outcome != "no change" || res.Note != "2 parts behind, and each target has its attempt already: litellm, cache. A newer version of the lists makes a new target" {
		t.Fatalf("result = %+v", res)
	}
	if len(f2.moves.started) != 0 || len(f2.forge.commits) != 0 {
		t.Fatalf("started %v, committed %v: want none", f2.moves.started, f2.forge.commits)
	}
}

// A new version of the lists is a new target: the branch changes with it.
func TestFollowBranchNamesTheTarget(t *testing.T) {
	a := followBranch("litellm", litellmTarget(), 1)
	b := followBranch("litellm", map[string][]byte{litellmFile: []byte(litellmAt("1.108.18", "e"))}, 1)
	if a == b || a != followBranch("litellm", litellmTarget(), 1) {
		t.Fatalf("branches %s and %s: a target is one branch, another target another", a, b)
	}
	if got := followBranch("litellm", litellmTarget(), 3); got != a+"-3" {
		t.Fatalf("third attempt = %s, want %s-3", got, a)
	}
	if !strings.HasPrefix(a, "factory/follow-litellm-") || len(a) != len("factory/follow-litellm-")+10 {
		t.Fatalf("branch = %s", a)
	}
}

// An attempt whose pull request stands open with no move running stopped
// before its workflow started: the run starts the workflow for it and opens
// no second pull request.
func TestFollowPassAdoptsAnOpenAttempt(t *testing.T) {
	f := newFollowRun(t, behindTree())
	n := f.forge.attempt(followBranch("litellm", litellmTarget(), 1), "open", false)
	res := f.run(t)
	if len(f.moves.started) != 1 || f.moves.started[0].PR != n || len(f.forge.commits) != 0 {
		t.Fatalf("started %+v, commits %v, want the workflow for #%d and no commit (%+v)", f.moves.started, f.forge.commits, n, res)
	}
}

// A move that runs holds the run, and the report line names it; so does an
// open failure card.
func TestFollowPassNamesTheMoveAndTheCardThatHold(t *testing.T) {
	f := newFollowRun(t, behindTree())
	f.moves.running = &FollowMoveInput{Part: "litellm", PR: 40}
	res := f.run(t)
	if res.Outcome != "held" || res.Note != "held: the move of part litellm runs, pull request #40" {
		t.Fatalf("running: %+v", res)
	}
	f = newFollowRun(t, behindTree())
	f.store.tasks = map[string]Task{"t1": {ID: "t1", Kind: "human", Status: "waiting-human", Repository: "sf/" + infraName, IssueNumber: 77,
		Objective: followMarker + "litellm failed: the probe of OpenBao"}}
	res = f.run(t)
	if res.Outcome != "held" || res.Note != "held: the failure card #77 is open: answer it, and the next run goes on" {
		t.Fatalf("card: %+v", res)
	}
	if len(f.forge.commits) != 0 || len(f.moves.started) != 0 {
		t.Fatalf("a held run moved: %v %v", f.forge.commits, f.moves.started)
	}
}

// Nothing behind outside the hand lane ends the run with a line that says
// how many parts move by hand.
func TestFollowPassNothingBehind(t *testing.T) {
	files := followLists()
	files[litellmFile] = litellmAt("1.108.17", "d")
	files[registryFile] = registryAt("8")
	f := newFollowRun(t, files)
	res := f.run(t)
	if res.Outcome != "no change" || res.Note != "nothing is behind; parts of the hand lane: 1" {
		t.Fatalf("result = %+v", res)
	}
}
