package factoryd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/bump"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	codeRepoAPI = "/api/v1/repos/sf/ai-software-factory-code"
	codeForkAPI = "/api/v1/repos/factory-agent/ai-software-factory-code"
)

// bumpForge is the code repository and the bot's fork as the bump pass
// uses them, its issues codeIssues'. Each call joins the shared log.
type bumpForge struct {
	mu      sync.Mutex
	log     *[]string
	main    map[string]string // main's files at m1
	commits []map[string]any  // POST /contents on the fork
	pulls   []map[string]any  // POST /pulls
	patches []map[string]any  // PATCH /pulls/2400
	deleted []string          // DELETE of a fork branch
}

func (f *bumpForge) handler(t *testing.T, issues http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		*f.log = append(*f.log, r.Method+" "+r.URL.Path)
		// the body stays readable for the issue API
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var in map[string]any
		json.Unmarshal(raw, &in) // #nosec G104 -- test stub
		answer := func(status int, v any) {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(v) // #nosec G104 -- test stub
		}
		switch p := r.URL.Path; {
		case p == codeRepoAPI+"/branches/main":
			answer(http.StatusOK, map[string]any{"name": "main", "commit": map[string]string{"id": "m1"}})
		case strings.HasPrefix(p, codeRepoAPI+"/raw/") && r.URL.Query().Get("ref") == "m1":
			body, ok := f.main[strings.TrimPrefix(p, codeRepoAPI+"/raw/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(body)) // #nosec G104 -- test stub
		case p == codeForkAPI && r.Method == http.MethodGet:
			answer(http.StatusOK, map[string]any{"fork": true, "private": true, "has_actions": false, "parent": map[string]string{"full_name": "sf/ai-software-factory-code"}})
		case p == codeForkAPI+"/sync_fork":
			answer(http.StatusOK, map[string]any{"allowed": false, "commits_behind": 0})
		case strings.HasPrefix(p, codeForkAPI+"/contents/") && r.URL.Query().Get("ref") == "main":
			file := strings.TrimPrefix(p, codeForkAPI+"/contents/")
			answer(http.StatusOK, map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(f.main[file])), "encoding": "base64", "sha": "blob-" + file})
		case p == codeForkAPI+"/contents" && r.Method == http.MethodPost:
			f.commits = append(f.commits, in)
			answer(http.StatusCreated, map[string]any{"commit": map[string]string{"sha": "b1"}})
		case p == codeRepoAPI+"/pulls" && r.Method == http.MethodPost:
			f.pulls = append(f.pulls, in)
			answer(http.StatusCreated, map[string]any{"number": 2400, "html_url": "https://git.example/sf/ai-software-factory-code/pulls/2400"})
		case p == codeRepoAPI+"/pulls/2400" && r.Method == http.MethodPatch:
			f.patches = append(f.patches, in)
			answer(http.StatusCreated, map[string]any{"number": 2400})
		case strings.HasPrefix(p, codeForkAPI+"/branches/"):
			if r.Method == http.MethodDelete {
				f.deleted = append(f.deleted, strings.TrimPrefix(p, codeForkAPI+"/branches/"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			answer(http.StatusOK, map[string]any{"name": "x", "commit": map[string]string{"id": "b1"}})
		default:
			f.mu.Unlock()
			issues(w, r)
			f.mu.Lock()
		}
	}
}

// bumpRun is the code project's runner with the weekly bump of pass bump,
// main's files at m1, the pod whose bump hands back res and whose copy
// records the files it got, and the shared log of the forge's calls and
// the pods' runs.
type bumpRun struct {
	r      *Runner
	store  *fakeStore
	issues *codeIssues
	wf     *fakeWorkflows
	forge  *bumpForge
	copied map[string]string
	log    []string
}

func newBumpRun(t *testing.T, res bump.Result) *bumpRun {
	t.Helper()
	b := &bumpRun{}
	b.r, b.store, b.issues, b.wf = routineRunner(t, routineTree(t))
	b.forge = &bumpForge{log: &b.log, main: map[string]string{}}
	for _, p := range slices.Concat(bump.Files, upstreamScripts) {
		b.forge.main[p] = "main's " + p + "\n"
	}
	srv := httptest.NewServer(b.forge.handler(t, b.issues.handler(t)))
	t.Cleanup(srv.Close)
	b.r.forgejo = &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	u := newUpstreamPod(t)
	podKube(u).run = func(secret map[string]string) (string, int32) {
		b.forge.mu.Lock()
		defer b.forge.mu.Unlock()
		if _, login := secret["password"]; login {
			b.log = append(b.log, "copy pod")
			b.copied = secret
			return "", 0
		}
		b.log = append(b.log, "bump pod")
		return bumpLog(t, res), 0
	}
	b.r.Upstream = u
	return b
}

var bumped = bump.Result{
	Files: map[string]string{
		"upstream-files.txt": "node v24.22.3 node-v24.22.3-linux-x64.tar.xz https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz 4f2b\n",
		"go.mod":             "module m\n\ntoolchain go1.26.11\n",
	},
	Report: bump.Report{Moved: []string{"node v24.22.2 → v24.22.3"},
		Failed: []string{"ghcr.io/cloudnative-pg/postgresql:18.7-system-trixie@sha256:6ebb: GET https://ghcr.io/v2/cloudnative-pg/postgresql/tags/list?n=1000: 429 Too Many Requests"}},
}

// The bump pass commits the pod's files on the bot's fork at the new
// card's branch, copies what they add into the store before the pull
// request opens, and files the routine's agent card with the pull request
// on its row: its run verifies the pull request first.
func TestRoutineRunBumpFilesTheCardOfItsPullRequest(t *testing.T) {
	b := newBumpRun(t, bumped)
	res, err := b.r.RoutineRun(context.Background(), RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	card := onlyTask(t, b.store)
	brief, _ := routineTree(t).Brief("routines/weekly-bump.md")
	// a link to a host the repository's text may not name is defanged
	text := strings.Replace(strings.TrimSpace(bumped.Report.Text()), "https://ghcr.io/", "hxxps://ghcr[.]io/", 1)
	if card.Kind != "agent" || card.Source != SourceRoutine || card.PRNumber != 2400 || card.IssueNumber != 71 || card.Track != policy.TrackDirect ||
		card.Objective != "Update the fixed versions of outside software every week\n\n"+strings.TrimSpace(brief)+"\n\n"+text {
		t.Fatalf("card %+v", card)
	}
	if want := (RoutineRunResult{Outcome: "filed", TaskID: card.ID, Issue: 71}); res != want {
		t.Errorf("result %+v, want %+v", res, want)
	}
	// the card's summary names the pin the lookup could not resolve
	if summary := "Summary: The factory's lookup moved 1 pin in this card's pull request. Its checks and review judge it, and an agent works on it only when a check fails. " +
		"Not resolved, kept as they were: ghcr.io/cloudnative-pg/postgresql:18.7-system-trixie.\n"; len(b.issues.issues) != 1 || !strings.Contains(b.issues.issues[0], summary) {
		t.Errorf("issues %q, want the summary %q", b.issues.issues, summary)
	}
	if !slices.Equal(b.wf.started, []string{card.ID}) {
		t.Errorf("started %v, want the card's run", b.wf.started)
	}
	branch := "factory/" + card.ID
	if len(b.forge.commits) != 1 || b.forge.commits[0]["branch"] != "main" || b.forge.commits[0]["new_branch"] != branch {
		t.Fatalf("commits %v, want one on the fork's main into %s", b.forge.commits, branch)
	}
	got := map[string]string{}
	for _, f := range b.forge.commits[0]["files"].([]any) {
		f := f.(map[string]any)
		content, _ := base64.StdEncoding.DecodeString(f["content"].(string))
		got[f["path"].(string)] = f["operation"].(string) + " " + f["sha"].(string) + " " + string(content)
	}
	if want := map[string]string{"upstream-files.txt": "update blob-upstream-files.txt " + bumped.Files["upstream-files.txt"], "go.mod": "update blob-go.mod " + bumped.Files["go.mod"]}; !maps.Equal(got, want) {
		t.Errorf("committed %v, want %v", got, want)
	}
	if len(b.forge.pulls) != 1 || b.forge.pulls[0]["head"] != "factory-agent:"+branch || b.forge.pulls[0]["base"] != "main" ||
		b.forge.pulls[0]["title"] != "Update the fixed versions of outside software every week" || b.forge.pulls[0]["body"] != text {
		t.Errorf("pulls %v", b.forge.pulls)
	}
	if len(b.forge.patches) != 1 || b.forge.patches[0]["body"] != text+"\n\nCloses #71\n\nOpened by the factory for card #71." {
		t.Errorf("the pull request's body names no card: %v", b.forge.patches)
	}
	// the copy gets the head's lists and main's scripts
	for key, want := range map[string]string{"upstream-files.txt": bumped.Files["upstream-files.txt"], "go.mod": bumped.Files["go.mod"],
		"go.sum": "main's go.sum\n", "upstream-copy": "main's scripts/upstream-copy\n", "upstream-files": "main's scripts/upstream-files\n"} {
		if b.copied[key] != want {
			t.Errorf("the copy's %s = %q, want %q", key, b.copied[key], want)
		}
	}
	at := func(entry string) int { return slices.Index(b.log, entry) }
	if !(at("bump pod") >= 0 && at("bump pod") < at("copy pod") && at("copy pod") < at("POST "+codeRepoAPI+"/pulls")) {
		t.Errorf("log %q: want the bump, the copy, then the pull request", b.log)
	}
	if run := b.store.routineRuns[codeProject+"/weekly-bump"][0]; run.Outcome != "filed" || run.TaskID != card.ID {
		t.Errorf("run %+v: want it filed with the card, so hold holds the next", run)
	}
}

// A bump that moves no pin files nothing and copies nothing.
func TestRoutineRunBumpWithNoMoveFilesNothing(t *testing.T) {
	b := newBumpRun(t, bump.Result{Files: map[string]string{}})
	res, err := b.r.RoutineRun(context.Background(), RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RoutineRunResult{Outcome: "no change", Note: "no pin moved"}); res != want {
		t.Errorf("result %+v, want %+v", res, want)
	}
	if len(b.store.tasks) != 0 || len(b.forge.pulls) != 0 || slices.Contains(b.log, "copy pod") {
		t.Errorf("tasks %v, pulls %v, log %q: want nothing filed or copied", b.store.tasks, b.forge.pulls, b.log)
	}
}

// A card whose filing fails takes its pull request and branch with it: no
// pull request stands without a card to answer its checks.
func TestRoutineRunBumpClosesThePullRequestOfACardItCouldNotFile(t *testing.T) {
	b := newBumpRun(t, bumped)
	b.store.createFails = 1
	_, err := b.r.RoutineRun(context.Background(), RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now()})
	if err == nil {
		t.Fatal("no error")
	}
	if len(b.forge.patches) != 1 || b.forge.patches[0]["state"] != "closed" {
		t.Errorf("patches %v, want the pull request closed", b.forge.patches)
	}
	if len(b.forge.deleted) != 1 || !strings.HasPrefix(b.forge.deleted[0], "factory%2Ft-") && !strings.HasPrefix(b.forge.deleted[0], "factory/t-") {
		t.Errorf("deleted %v, want the card's branch", b.forge.deleted)
	}
}

// A file the pod hands back that no bump writes is refused: the pod's
// answer is data, and only the bump's files reach the branch.
func TestRoutineRunBumpRefusesAFileNoBumpWrites(t *testing.T) {
	res := bump.Result{Files: maps.Clone(bumped.Files), Report: bumped.Report}
	res.Files[".forgejo/workflows/pr.yml"] = "on: push\n"
	b := newBumpRun(t, res)
	_, err := b.r.RoutineRun(context.Background(), RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "the bump pod handed back .forgejo/workflows/pr.yml, which is no file of a bump") || len(b.forge.commits) != 0 {
		t.Errorf("err %v, commits %v", err, b.forge.commits)
	}
}
