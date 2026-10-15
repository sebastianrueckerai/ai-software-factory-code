package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestForgejoMintRepoScopedToken(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(201)
		io.WriteString(w, `{"id":77,"name":"task-t1","sha1":"40hexsecret","token_last_eight":"exsecret",
			"scopes":["write:repository"],"repositories":[{"id":1,"name":"ai-software-factory-code"}]}`)
	}))
	defer srv.Close()

	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-bot", BotPassword: "bot-pw"}
	tok, err := f.MintToken(context.Background(), "task-t1", "write:repository", []Repo{{Owner: "sf", Name: "ai-software-factory-code"}})
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if gotPath != "/api/v1/users/factory-bot/tokens" {
		t.Fatalf("mint path = %s", gotPath)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("factory-bot:bot-pw")); gotAuth != want {
		t.Fatalf("mint auth = %q", gotAuth)
	}
	if gotBody["name"] != "task-t1" {
		t.Fatalf("mint name = %v", gotBody["name"])
	}
	if scopes, _ := gotBody["scopes"].([]any); len(scopes) != 1 || scopes[0] != "write:repository" {
		t.Fatalf("mint scopes = %v", gotBody["scopes"])
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 {
		t.Fatalf("mint repositories = %v", gotBody["repositories"])
	}
	repo := repos[0].(map[string]any)
	if repo["owner"] != "sf" || repo["name"] != "ai-software-factory-code" {
		t.Fatalf("mint repo target = %v", repo)
	}
	if tok.ID != 77 || tok.Secret != "40hexsecret" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestForgejoMintRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"token name already exists"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-bot", BotPassword: "bot-pw"}
	_, err := f.MintToken(context.Background(), "task-t1", "write:repository", []Repo{{Owner: "sf", Name: "r"}})
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want the 422 body surfaced", err)
	}
}

func TestForgejoRevokeDeletesByID(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(204)
	}))
	defer srv.Close()

	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-bot", BotPassword: "bot-pw"}
	if err := f.RevokeToken(context.Background(), 77); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/users/factory-bot/tokens/77" {
		t.Fatalf("revoke = %s %s", gotMethod, gotPath)
	}
}

func TestForgejoRevokeRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-bot", BotPassword: "bot-pw"}
	err := f.RevokeToken(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the 403 surfaced", err)
	}
}

func TestForgejoRevokeAlreadyGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "bot", BotPassword: "bot-pw"}
	if err := f.RevokeToken(context.Background(), 77); err != nil {
		t.Fatalf("revoke of a gone token = %v, want nil (already clean)", err)
	}
}

// SetIssueDependency reads the issue's dependencies first and leaves an
// issue already as asked alone; else it adds or deletes the dependency as
// the bot, the dependency named in the body, and takes Forgejo's 200 for
// the delete as its swagger says.
func TestForgejoSetIssueDependency(t *testing.T) {
	deps := []int64{21}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		got = append(got, strings.TrimSpace(fmt.Sprintf("%s %s %s %s", r.Method, r.URL.RequestURI(), user, b)))
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("page") == "1":
			list := []map[string]any{}
			for _, d := range deps {
				list = append(list, map[string]any{"number": d})
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet:
			w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete:
			deps = nil
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPost:
			deps = []int64{21}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-bot", BotPassword: "bot-pw"}
	const path = "/api/v1/repos/sf/r/issues/22/dependencies"
	const page1, page2 = "GET " + path + "?page=1&limit=50 factory-bot", "GET " + path + "?page=2&limit=50 factory-bot"
	const body = ` factory-bot {"index":21,"owner":"sf","repo":"r"}`
	for _, tc := range []struct {
		remove bool
		want   []string
	}{
		{false, []string{page1, page2}},
		{true, []string{page1, page2, "DELETE " + path + body}},
		{true, []string{page1}},
		{false, []string{page1, "POST " + path + body}},
	} {
		got = nil
		if err := f.SetIssueDependency(context.Background(), Repo{Owner: "sf", Name: "r"}, 22, 21, tc.remove); err != nil {
			t.Fatalf("SetIssueDependency(remove %t): %v", tc.remove, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("SetIssueDependency(remove %t) asked\n%q\nwant\n%q", tc.remove, got, tc.want)
		}
	}
}

func TestForgejoCreateIssue(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(201)
		io.WriteString(w, `{"number":42,"html_url":"https://git.example/sf/r/issues/42"}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ref, err := f.CreateIssue(context.Background(), "sf", "r", "Add the runner", "objective body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if gotPath != "/api/v1/repos/sf/r/issues" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotBody["title"] != "Add the runner" || gotBody["body"] != "objective body" {
		t.Fatalf("body = %v", gotBody)
	}
	if ref.Number != 42 || ref.URL != "https://git.example/sf/r/issues/42" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestForgejoCreateIssueRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	_, err := f.CreateIssue(context.Background(), "sf", "r", "t", "b")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the 403 surfaced", err)
	}
}

func TestForgejoEnsureLabelsCreatesOnlyMissing(t *testing.T) {
	var created []string
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":1,"name":"agent"},{"id":2,"name":"state/queued"}]`)
		case r.Method == http.MethodPost:
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			created = append(created, body["name"].(string))
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	wanted := map[string]string{"agent": "#ededed", "state/queued": "#ededed", "human": "#c2e0c6"}
	if _, err := f.EnsureLabels(context.Background(), "sf", "r", wanted); err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if len(created) != 1 || created[0] != "human" {
		t.Fatalf("created = %v, want exactly [human] (agent, state/queued already existed)", created)
	}
}

// Boot creates the priority and severity labels intake weighs, and reads
// every page of the existing ones so none is created twice.
func TestBootLabelsCoverPriorityAndSeverityAcrossPages(t *testing.T) {
	for name := range priorityLabelWeight {
		if _, ok := BootLabels[name]; !ok {
			t.Errorf("BootLabels lacks %s", name)
		}
	}
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("page") == "1":
			var page []RepoLabel
			for i := range 49 {
				page = append(page, RepoLabel{ID: int64(i + 10), Name: fmt.Sprintf("other/%d", i)})
			}
			json.NewEncoder(w).Encode(append(page, RepoLabel{ID: 1, Name: "agent"}))
		case r.Method == http.MethodGet && r.URL.Query().Get("page") == "2":
			io.WriteString(w, `[{"id":2,"name":"priority/p0"}]`)
		case r.Method == http.MethodGet && r.URL.Query().Get("page") == "3":
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet:
			t.Errorf("unexpected page %q", r.URL.RawQuery)
		case r.Method == http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			created = append(created, body["name"].(string))
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	if _, err := f.EnsureLabels(context.Background(), "sf", "r", BootLabels); err != nil {
		t.Fatal(err)
	}
	sort.Strings(created)
	want := []string{"bug", "human", "improvement", "noise", "priority/p1", "priority/p2", "priority/p3", "real", "routine", "severity/critical", "severity/high", "severity/low", "severity/medium",
		"state/backlog", "state/closed", "state/done", "state/failed", "state/in-review", "state/queued", "state/running", "state/waiting-deps", "state/waiting-human", "state/waiting-poster"}
	if !slices.Equal(created, want) {
		t.Fatalf("created %v, want %v", created, want)
	}
}

func TestForgejoEnsureLabelsJoinsCreateErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `[]`)
		case http.MethodPost:
			http.Error(w, "nope", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	_, err := f.EnsureLabels(context.Background(), "sf", "r", map[string]string{"agent": "#ededed", "human": "#c2e0c6"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the two 403s surfaced", err)
	}
}

func TestForgejoSetStateLabelSwapsExactlyOne(t *testing.T) {
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/issues/42/labels" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `[{"id":1,"name":"agent"},{"id":2,"name":"state/queued"}]`)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &putBody)
			w.WriteHeader(200)
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	if err := f.SetStateLabel(context.Background(), "sf", "r", 42, "running"); err != nil {
		t.Fatalf("SetStateLabel: %v", err)
	}
	labels, _ := putBody["labels"].([]any)
	got := make([]string, len(labels))
	for i, l := range labels {
		got[i] = l.(string)
	}
	if len(got) != 2 || got[0] != "agent" || got[1] != "state/running" {
		t.Fatalf("replaced labels = %v, want [agent state/running] (queued swapped for running)", got)
	}
}

func TestForgejoPullRequestStateReadsHeadRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/pulls/7" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"head":{"ref":"factory/t1","repo":{"full_name":"factory-agent/r"}}}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ref, err := f.PullRequestState(context.Background(), "sf", "r", 7)
	if err != nil || ref.Head != "factory/t1" || ref.HeadRepo != "factory-agent/r" {
		t.Fatalf("PullRequestState = (%+v, %v), want head factory/t1 on factory-agent/r", ref, err)
	}
}

// A read token on the base repository is minted with exactly that scope:
// the worker clones with it and can write nothing.
func TestForgejoMintReadScope(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(201)
		io.WriteString(w, `{"id":78,"sha1":"readsecret"}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-bot", BotPassword: "bot-pw"}
	if _, err := f.MintToken(context.Background(), "task-t1-base", "read:repository", []Repo{{Owner: "sf", Name: "r"}}); err != nil {
		t.Fatal(err)
	}
	if scopes, _ := gotBody["scopes"].([]any); len(scopes) != 1 || scopes[0] != "read:repository" {
		t.Fatalf("mint scopes = %v", gotBody["scopes"])
	}
}

// The bot's fork of a repository carries the same name under the bot user
// (ruling 97); EnsureFork creates it once and is a no-op once it exists.
func TestForgejoEnsureForkCreatesOnceAsTheBot(t *testing.T) {
	if got := (&Forgejo{BotUser: "factory-agent"}).Fork(Repo{Owner: "sf", Name: "r"}); got != (Repo{Owner: "factory-agent", Name: "r"}) {
		t.Fatalf("Fork = %+v", got)
	}
	var calls []string
	exists, actions := false, false
	repoJSON := func() string {
		return fmt.Sprintf(`{"full_name":"factory-agent/r","fork":true,"private":true,"has_actions":%t,"parent":{"full_name":"sf/r"}}`, actions)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/factory-agent/r":
			if !exists {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, repoJSON())
		// the fork comes with the Actions unit on: a leaked session token
		// could push the fork's main and run its ci.yml there
		case r.Method == "PATCH" && r.URL.Path == "/api/v1/repos/factory-agent/r":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["has_actions"] != false {
				t.Errorf("patch body = %v, want has_actions false", body)
			}
			actions = false
			io.WriteString(w, repoJSON())
		case r.Method == "POST" && r.URL.Path == "/api/v1/repos/sf/r/forks":
			if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("factory-agent:bot-pw")) {
				t.Errorf("fork auth = %q, want the bot's basic auth", r.Header.Get("Authorization"))
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "r" {
				t.Errorf("fork body = %v, want the same name", body)
			}
			exists, actions = true, true
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, repoJSON())
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	for range 2 {
		if err := f.EnsureFork(context.Background(), Repo{Owner: "sf", Name: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"GET /api/v1/repos/factory-agent/r", "POST /api/v1/repos/sf/r/forks", "PATCH /api/v1/repos/factory-agent/r", "GET /api/v1/repos/factory-agent/r"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// A repository under the bot's name that is not a fork of the upstream, that
// went public, or whose Actions unit stays on after the patch, is refused:
// the worker would push there.
func TestForgejoEnsureForkRefusesAStranger(t *testing.T) {
	for name, body := range map[string]string{
		"not a fork":   `{"full_name":"factory-agent/r","fork":false,"private":true}`,
		"other parent": `{"full_name":"factory-agent/r","fork":true,"private":true,"parent":{"full_name":"else/r"}}`,
		"public":       `{"full_name":"factory-agent/r","fork":true,"private":false,"parent":{"full_name":"sf/r"}}`,
		"actions stay": `{"full_name":"factory-agent/r","fork":true,"private":true,"has_actions":true,"parent":{"full_name":"sf/r"}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
		err := f.EnsureFork(context.Background(), Repo{Owner: "sf", Name: "r"})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "factory-agent/r") {
			t.Fatalf("%s: err = %v, want a refusal naming the repository", name, err)
		}
	}
}

// Finish deletes a finished task's branch on the fork; a branch already
// gone (a retried Finish) is done, a refusal is not.
// Forgejo 15 answers DELETE on a branch that does not exist with a 500, not
// a 404, so a branch is looked up before it is deleted: never pushed means
// nothing to do, and a retried Finish stays idempotent.
func TestForgejoDeleteBranchIsIdempotent(t *testing.T) {
	for name, tc := range map[string]struct {
		getCode, deleteCode int
		wantCalls           []string
		wantErr             bool
	}{
		"deleted":          {http.StatusOK, http.StatusNoContent, []string{"GET", "DELETE"}, false},
		"never pushed":     {http.StatusNotFound, http.StatusInternalServerError, []string{"GET"}, false},
		"vanished between": {http.StatusOK, http.StatusNotFound, []string{"GET", "DELETE"}, false},
		"refused":          {http.StatusOK, http.StatusInternalServerError, []string{"GET", "DELETE"}, true},
	} {
		var calls []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.EscapedPath() != "/api/v1/repos/factory-agent/r/branches/factory%2Ft1" {
				t.Errorf("%s: path = %s", name, r.URL.EscapedPath())
			}
			calls = append(calls, r.Method)
			if r.Method == http.MethodGet {
				w.WriteHeader(tc.getCode)
				return
			}
			w.WriteHeader(tc.deleteCode)
		}))
		f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
		err := f.DeleteBranch(context.Background(), "factory-agent", "r", "factory/t1")
		srv.Close()
		if (err != nil) != tc.wantErr || !slices.Equal(calls, tc.wantCalls) {
			t.Fatalf("%s: err = %v, calls = %v", name, err, calls)
		}
	}
}

func TestForgejoBranchHeadSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/branches/main" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"commit":{"id":"abc123"}}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	sha, err := f.BranchHeadSHA(context.Background(), "sf", "r", "main")
	if err != nil || sha != "abc123" {
		t.Fatalf("BranchHeadSHA = (%q, %v), want (abc123, nil)", sha, err)
	}
}

func TestForgejoCreatePullRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(201)
		io.WriteString(w, `{"number":9,"html_url":"https://git.example/sf/r/pulls/9","head":{"ref":"factory/t1"}}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ref, err := f.CreatePullRequest(context.Background(), "sf", "r", "factory/t1", "main", "Add the runner", "Closes #3")
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if gotPath != "/api/v1/repos/sf/r/pulls" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotBody["head"] != "factory/t1" || gotBody["base"] != "main" || gotBody["title"] != "Add the runner" || gotBody["body"] != "Closes #3" {
		t.Fatalf("body = %v", gotBody)
	}
	if ref.Number != 9 || ref.URL != "https://git.example/sf/r/pulls/9" || ref.Head != "factory/t1" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestForgejoFindOpenPullRequestForBranchFindsMatch(t *testing.T) {
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/pulls" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `[
			{"number":4,"html_url":"https://git.example/sf/r/pulls/4","head":{"ref":"factory/other","repo":{"full_name":"factory-agent/r"}}},
			{"number":8,"html_url":"https://git.example/sf/r/pulls/8","head":{"ref":"factory/t1","repo":{"full_name":"stranger/r"}}},
			{"number":9,"html_url":"https://git.example/sf/r/pulls/9","head":{"ref":"factory/t1","sha":"c9","repo":{"full_name":"factory-agent/r"}}}
		]`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ref, err := f.FindOpenPullRequestForBranch(context.Background(), "sf", "r", Repo{Owner: "factory-agent", Name: "r"}, "factory/t1")
	if err != nil {
		t.Fatalf("FindOpenPullRequestForBranch: %v", err)
	}
	if ref == nil || ref.Number != 9 || ref.HeadRepo != "factory-agent/r" || ref.HeadSHA != "c9" {
		t.Fatalf("ref = %+v, want #9 from the bot's fork, not #8 from a stranger's", ref)
	}
}

func TestForgejoFindOpenPullRequestForBranchNoMatch(t *testing.T) {
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"number":4,"html_url":"https://git.example/sf/r/pulls/4","head":{"ref":"factory/other","repo":{"full_name":"factory-agent/r"}}}]`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ref, err := f.FindOpenPullRequestForBranch(context.Background(), "sf", "r", Repo{Owner: "factory-agent", Name: "r"}, "factory/t1")
	if err != nil {
		t.Fatalf("FindOpenPullRequestForBranch: %v", err)
	}
	if ref != nil {
		t.Fatalf("ref = %+v, want nil", ref)
	}
}

func TestForgejoCombinedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/commits/abc123/status" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"state":"failure","statuses":[{"status":"failure","context":"ci/build","target_url":"https://ci.example/1","description":"tests failed"}]}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	status, err := f.CombinedStatus(context.Background(), "sf", "r", "abc123")
	if err != nil {
		t.Fatalf("CombinedStatus: %v", err)
	}
	if status.State != "failure" || len(status.Statuses) != 1 || status.Statuses[0].Context != "ci/build" || status.Statuses[0].State != "failure" {
		t.Fatalf("status = %+v", status)
	}
}

func TestForgejoCreateIssueComment(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(201)
		io.WriteString(w, `{"id":1}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	if _, err := f.CreateIssueComment(context.Background(), "sf", "r", 3, "attempts exhausted"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if gotPath != "/api/v1/repos/sf/r/issues/3/comments" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotBody["body"] != "attempts exhausted" {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestForgejoRepoDefaultBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"default_branch":"main"}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	branch, err := f.RepoDefaultBranch(context.Background(), "sf", "r")
	if err != nil || branch != "main" {
		t.Fatalf("RepoDefaultBranch = (%q, %v), want (main, nil)", branch, err)
	}
}

func TestForgejoPullRequestState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/pulls/9" || r.Method != http.MethodGet {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"number":9,"state":"closed","merged":true,"html_url":"https://git.example/sf/r/pulls/9","head":{"ref":"factory/t1","sha":"c7"}}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	got, err := f.PullRequestState(context.Background(), "sf", "r", 9)
	want := PullRequestRef{Number: 9, URL: "https://git.example/sf/r/pulls/9", Head: "factory/t1", HeadSHA: "c7", State: "closed", Merged: true}
	if err != nil || got != want {
		t.Fatalf("PullRequestState = (%+v, %v), want %+v", got, err, want)
	}
}

func TestForgejoIssueClosed(t *testing.T) {
	for state, want := range map[string]bool{"open": false, "closed": true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/sf/r/issues/4" {
				t.Fatalf("path %s", r.URL.Path)
			}
			fmt.Fprintf(w, `{"number":4,"state":%q}`, state)
		}))
		f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
		got, err := f.IssueClosed(context.Background(), "sf", "r", 4)
		srv.Close()
		if err != nil || got != want {
			t.Fatalf("%s: IssueClosed = (%v, %v)", state, got, err)
		}
	}
}

// The by-base-and-head endpoint finds the pull request in any state, also
// after its fork branch is gone; a missing one is nil, not an error.
func TestForgejoFindPullRequestByHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/sf/r/pulls/main/factory-agent/r:factory/t1":
			io.WriteString(w, `{"number":43,"html_url":"u43","state":"closed","merged":true,"head":{"ref":"refs/pull/43/head","sha":"b4d5","repo":{"full_name":"factory-agent/r"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	got, err := f.FindPullRequestByHead(context.Background(), "sf", "r", "main", Repo{Owner: "factory-agent", Name: "r"}, "factory/t1")
	if err != nil || got == nil || got.Number != 43 || !got.Merged || got.State != "closed" || got.HeadSHA != "b4d5" {
		t.Fatalf("FindPullRequestByHead = (%+v, %v), want the merged #43", got, err)
	}
	if got, err := f.FindPullRequestByHead(context.Background(), "sf", "r", "main", Repo{Owner: "factory-agent", Name: "r"}, "factory/t2"); err != nil || got != nil {
		t.Fatalf("unknown head = (%+v, %v), want nil, nil", got, err)
	}
}

// Forgejo flags a tree listing truncated whenever the whole tree exceeds
// one page, whatever the page: the flag says nothing, the pages do.
func TestForgejoWorkflowTreeEntriesPagesATruncatedListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/api/v1/repos/sf/r/git/trees/c1?page=1":
			io.WriteString(w, `{"truncated":true,"total_count":4,"tree":[{"path":".Forgejo","sha":"v"},{"path":".github","sha":"g"},{"path":"README.md","sha":"r"}]}`)
		case "/api/v1/repos/sf/r/git/trees/c1?page=2":
			io.WriteString(w, `{"truncated":true,"total_count":4,"tree":[{"path":".forgejo","sha":"f1"}]}`)
		case "/api/v1/repos/sf/r/git/trees/c2?page=1":
			io.WriteString(w, `{"truncated":false,"total_count":1,"tree":[{"path":"README.md","sha":"r"}]}`)
		case "/api/v1/repos/sf/r/git/trees/c3?page=1":
			w.WriteHeader(404)
		case "/api/v1/repos/sf/r/git/trees/c4?page=1":
			io.WriteString(w, `{"truncated":true,"total_count":3,"tree":[{"path":"README.md","sha":"r"}]}`)
		case "/api/v1/repos/sf/r/git/trees/c4?page=2":
			io.WriteString(w, `{"truncated":true,"total_count":3,"tree":[]}`)
		default:
			t.Errorf("unexpected %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ctx := context.Background()
	got, err := f.WorkflowTreeEntries(ctx, "sf", "r", "c1")
	if err != nil || !maps.Equal(got, map[string]string{".github": ".github g\n", ".forgejo": ".Forgejo v\n.forgejo f1\n"}) {
		t.Fatalf("c1 = (%q, %v), want .github from page 1 and .forgejo from both pages, its case variant included", got, err)
	}
	if got, err := f.WorkflowTreeEntries(ctx, "sf", "r", "c2"); err != nil || len(got) != 0 {
		t.Fatalf("c2 = (%v, %v), want none", got, err)
	}
	if _, err := f.WorkflowTreeEntries(ctx, "sf", "r", "c3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c3 = %v, want ErrNotFound: a commit Forgejo does not have", err)
	}
	// a listing that ends short of its total names no entry for sure
	if got, err := f.WorkflowTreeEntries(ctx, "sf", "r", "c4"); err == nil {
		t.Fatalf("c4 = %v, want an error: the pages ended short of total_count", got)
	}
}

// firstPage serves h for a paged list's first page and an empty page
// after it, for stubs that answer a list whatever the page asked.
func firstPage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Query().Get("page"); p != "" && p != "1" && !strings.Contains(r.URL.Path, "/git/trees/") && !strings.HasSuffix(r.URL.Path, "/actions/tasks") {
			io.WriteString(w, `[]`)
			return
		}
		h(w, r)
	}
}

// servePage answers a paged list the way a Forgejo with a smaller
// MAX_RESPONSE_ITEMS does: per items a page whatever the limit asked,
// then an empty page.
func servePage(w http.ResponseWriter, r *http.Request, per int, items ...string) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	lo := min(max(page-1, 0)*per, len(items))
	io.WriteString(w, "["+strings.Join(items[lo:min(lo+per, len(items))], ",")+"]")
}

// IssueClosedBy pages the closed issue's timeline for the last close
// event's user: a reopened and re-closed issue names the last closer,
// however few events a page holds. An open issue reads no timeline; a
// closed one without a close event is an error, not a closer factoryd
// would trust.
func TestForgejoIssueClosedBy(t *testing.T) {
	timeline := map[string][]string{
		"7": {`{"type":"close","user":{"login":"mallory"}}`, `{"type":"reopen","user":{"login":"sebastian"}}`, `{"type":"close","user":{"login":"sebastian"}}`},
		"8": {`{"type":"comment","user":{"login":"sebastian"}}`},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/repos/sf/r/issues/6":
			io.WriteString(w, `{"state":"open"}`)
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			servePage(w, r, 2, timeline[strings.Split(r.URL.Path, "/")[7]]...)
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/r/issues/"):
			io.WriteString(w, `{"state":"closed"}`)
		default:
			t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ctx := context.Background()
	if closer, err := f.IssueClosedBy(ctx, "sf", "r", 6); err != nil || closer != "" {
		t.Fatalf("open issue = (%q, %v), want no closer", closer, err)
	}
	if closer, err := f.IssueClosedBy(ctx, "sf", "r", 7); err != nil || closer != "sebastian" {
		t.Fatalf("reclosed issue = (%q, %v), want the last closer, sebastian", closer, err)
	}
	if _, err := f.IssueClosedBy(ctx, "sf", "r", 8); !errors.Is(err, errNoCloser) {
		t.Fatalf("closed issue without a close event = %v, want errNoCloser", err)
	}
}

// Every paged list reads on until an empty page: Forgejo may serve fewer
// items a page than the limit asked for. The boot sweep must see every
// token, every label, and the open pull request may sit on any page.
func TestForgejoPagedListsReadEveryPage(t *testing.T) {
	pr := func(n int, ref string) string {
		return fmt.Sprintf(`{"number":%d,"state":"open","head":{"ref":%q,"repo":{"full_name":"factory-agent/r"}}}`, n, ref)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/factory-agent/tokens":
			servePage(w, r, 2, `{"id":1}`, `{"id":2}`, `{"id":3}`, `{"id":4}`, `{"id":5}`)
		case "/api/v1/repos/sf/r/pulls":
			servePage(w, r, 2, pr(1, "a"), pr(2, "b"), pr(3, "c"), pr(4, "factory/t1"))
		case "/api/v1/repos/sf/r/labels":
			servePage(w, r, 2, `{"id":1,"name":"agent"}`, `{"id":2,"name":"human"}`, `{"id":3,"name":"state/done"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ctx := context.Background()
	if tokens, err := f.ListTokens(ctx); err != nil || len(tokens) != 5 {
		t.Fatalf("tokens = (%v, %v), want all 5", tokens, err)
	}
	if got, err := f.FindOpenPullRequestForBranch(ctx, "sf", "r", Repo{Owner: "factory-agent", Name: "r"}, "factory/t1"); err != nil || got == nil || got.Number != 4 {
		t.Fatalf("open pull request = (%+v, %v), want #4 from the second page", got, err)
	}
	if labels, err := f.ListLabels(ctx, "sf", "r"); err != nil || len(labels) != 3 || labels[2].Name != "state/done" {
		t.Fatalf("labels = (%v, %v), want all 3", labels, err)
	}
}

// Forgejo's issue comment list is not paged: it answers every comment
// whatever the page asked, so it is read in one request.
func TestForgejoListIssueCommentsIsOneRequest(t *testing.T) {
	var comments []string
	for i := range 60 {
		comments = append(comments, fmt.Sprintf(`{"id":%d,"user":{"login":"sebastian"},"body":"c"}`, i+1))
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests++; requests > 1 {
			t.Errorf("request %d: %s?%s", requests, r.URL.Path, r.URL.RawQuery)
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, "["+strings.Join(comments, ",")+"]")
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	got, err := f.ListIssueComments(context.Background(), "sf", "r", 3)
	if err != nil || len(got) != 60 || got[59].Author != "sebastian" {
		t.Fatalf("comments = %d, %v, want all 60 with their authors", len(got), err)
	}
}

// A 4xx no retry can fix is a refusal: the activity interceptor ends
// the retries on it. 404 is Forgejo's own not-found (also ErrNotFound), and 408, 409 and 429 are as
// transient as a 5xx.
func TestForgejoClientErrorsAreRefusals(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"no"}`, status)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	for code, want := range map[int]error{
		400: ErrRefused, 401: ErrRefused, 403: ErrRefused, 405: ErrRefused, 410: ErrRefused, 422: ErrRefused,
		404: ErrNotFound, 408: nil, 409: nil, 429: nil, 500: nil, 502: nil,
	} {
		status = code
		_, err := f.RepoDefaultBranch(context.Background(), "sf", "r")
		if err == nil {
			t.Fatalf("%d: err = nil", code)
		}
		if got := errors.Is(err, ErrRefused); got != (want == ErrRefused) {
			t.Fatalf("%d: refused = %v: %v", code, got, err)
		}
		if got := errors.Is(err, ErrNotFound); got != (want == ErrNotFound) {
			t.Fatalf("%d: not found = %v: %v", code, got, err)
		}
		if got := errors.Is(err, errForgejoNotFound); got != (want == ErrNotFound) {
			t.Fatalf("%d: forgejo not found = %v: %v", code, got, err)
		}
		if got := errors.Is(err, errForgejoForbidden); got != (code == 403) {
			t.Fatalf("%d: forbidden = %v: %v", code, got, err)
		}
		if want == ErrRefused && !strings.Contains(err.Error(), "no") {
			t.Fatalf("%d: body not surfaced: %v", code, err)
		}
	}
}

func TestForgejoClosePullRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/repos/sf/r/pulls/9" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":9,"state":"closed"}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	if err := f.ClosePullRequest(context.Background(), "sf", "r", 9); err != nil || got["state"] != "closed" {
		t.Fatalf("ClosePullRequest = %v, body %v", err, got)
	}
}

// The trust gate trusts code writers only: members of the org's
// teams with code write on the repository, and direct collaborators with
// write. A collaborator's own permission is unreadable with a scoped
// token, so a collaborator counts by being on the assignee list (write or
// more on some unit), unless a team without code write could be what put
// them there (Forgejo lists a read-only team's members as assignees
// through its pull request read): then they fail closed. Team reads carry
// the org-read token (ruling 108: a repository-limited token cannot hold
// read:organization), repository reads the project's read token.
func TestForgejoRepoWritersTrustsCodeWritersOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "token registrar"
		if strings.HasPrefix(r.URL.Path, "/api/v1/orgs/") || strings.HasPrefix(r.URL.Path, "/api/v1/teams/") {
			want = "token org-read"
		}
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("%s read with %q, want %q", r.URL.Path, got, want)
		}
		switch r.URL.Path {
		case "/api/v1/orgs/sf/teams":
			servePage(w, r, 2,
				`{"id":1,"name":"Owners","permission":"owner","includes_all_repositories":true,"units_map":{"repo.code":"owner","repo.pulls":"owner"}}`,
				`{"id":2,"name":"reviewers","permission":"read","units_map":{"repo.code":"read","repo.pulls":"read"}}`,
				`{"id":3,"name":"devs","permission":"read","units_map":{"repo.code":"write","repo.issues":"read"}}`,
				`{"id":4,"name":"infra","permission":"write","units_map":{"repo.code":"write"}}`,
				`{"id":5,"name":"triage","permission":"write","includes_all_repositories":true,"units_map":{"repo.issues":"write"}}`)
		case "/api/v1/teams/2/repos", "/api/v1/teams/3/repos":
			servePage(w, r, 1, `{"full_name":"sf/other"}`, `{"full_name":"sf/r"}`)
		case "/api/v1/teams/4/repos":
			servePage(w, r, 1, `{"full_name":"sf/other"}`)
		case "/api/v1/teams/1/members":
			servePage(w, r, 1, `{"login":"sebastian"}`, `{"login":"factory-ci"}`)
		case "/api/v1/teams/2/members":
			servePage(w, r, 1, `{"login":"rita"}`, `{"login":"carol"}`)
		case "/api/v1/teams/3/members":
			servePage(w, r, 1, `{"login":"dave"}`)
		case "/api/v1/teams/4/members":
			servePage(w, r, 1, `{"login":"ivan"}`)
		case "/api/v1/teams/5/members":
			servePage(w, r, 1, `{"login":"judy"}`, `{"login":"kim"}`)
		case "/api/v1/repos/sf/r/collaborators":
			servePage(w, r, 2, `{"login":"factory-agent"}`, `{"login":"carol"}`, `{"login":"erin"}`, `{"login":"kim"}`)
		case "/api/v1/repos/sf/r/assignees":
			io.WriteString(w, `[{"login":"carol"},{"login":"dave"},{"login":"factory-agent"},{"login":"factory-ci"},{"login":"judy"},{"login":"kim"},{"login":"rita"},{"login":"sebastian"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, CIOrgToken: "org-read"}
	got, err := f.RepoWriters(context.Background(), Repo{"sf", "r"}, "registrar")
	// rita: a read-only team's member; carol: a collaborator who is one too;
	// erin: a read collaborator, no assignee; ivan: a code writer elsewhere;
	// judy: in a team with issue write and no code, whose aggregate
	// permission Forgejo reports as "write"; kim: a collaborator in it
	if want := []string{"dave", "factory-agent", "factory-ci", "sebastian"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("writers = %q, %v; want %q", got, err, want)
	}
	if _, err := f.RepoWriters(context.Background(), Repo{"nobody", "r"}, "registrar"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner that is no org: %v, want ErrNotFound", err)
	}
}

// Each file comes with its old name on a rename and its lines added and
// deleted; the list is read page by page.
func TestForgejoPullRequestFilesAnswersNamesAndLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/repo/pulls/7/files" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		servePage(w, r, 2,
			`{"filename":"README.md","status":"modified","additions":40,"deletions":3}`,
			`{"filename":"ci.yml","previous_filename":".forgejo/workflows/ci.yml","status":"renamed","additions":5,"deletions":5}`,
			`{"filename":"k8s/x.yaml","previous_filename":"","status":"added"}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"}
	got, err := f.PullRequestFiles(context.Background(), "sf", "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []PRFile{{Filename: "README.md", Additions: 40, Deletions: 3}, {Filename: "ci.yml", PreviousFilename: ".forgejo/workflows/ci.yml", Additions: 5, Deletions: 5},
		{Filename: "k8s/x.yaml"}}
	if !slices.Equal(got, want) {
		t.Fatalf("files = %+v, want %+v", got, want)
	}
}

func TestForgejoPullRequestDiffIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/repo/pulls/7.diff" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, "0123456789")
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"}
	if got, _ := f.PullRequestDiff(context.Background(), "sf", "repo", 7, 20); got != "0123456789" {
		t.Fatalf("uncut diff = %q", got)
	}
	if got, _ := f.PullRequestDiff(context.Background(), "sf", "repo", 7, 4); got != "0123\n… (diff cut at 4 of 10 bytes)" {
		t.Fatalf("cut diff = %q", got)
	}
}

// The merge goes out as the given factory-merge token with the gated head; a 405
// (no whitelisted approval) and a 409 (conflict, head moved) are refusals.
func TestForgejoMergePullRequest(t *testing.T) {
	status := http.StatusOK
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/repos/sf/repo/pulls/7/merge" || r.Header.Get("Authorization") != "token merge-tok" {
			t.Errorf("unexpected %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"}
	if err := f.MergePullRequest(context.Background(), "merge-tok", "sf", "repo", 7, "c1", Attribution{Role: "merge", Task: "t1"}, "msg"); err != nil {
		t.Fatal(err)
	}
	if body["Do"] != "merge" || body["head_commit_id"] != "c1" || body["MergeMessageField"] != "msg\n\nFactory-Role: merge\nFactory-Task: t1\n" {
		t.Fatalf("merge body = %v", body)
	}
	for _, s := range []int{http.StatusMethodNotAllowed, http.StatusConflict} {
		status = s
		if err := f.MergePullRequest(context.Background(), "merge-tok", "sf", "repo", 7, "c1", Attribution{Role: "merge", Task: "t1"}, "msg"); !errors.Is(err, ErrRefused) {
			t.Fatalf("merge answered %d = %v, want a refusal", s, err)
		}
	}
	status = http.StatusBadGateway
	if err := f.MergePullRequest(context.Background(), "merge-tok", "sf", "repo", 7, "c1", Attribution{Role: "merge", Task: "t1"}, "msg"); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("merge answered 502 = %v, want a transient error", err)
	}
}

// Forgejo 15 ignores actions/runs' limit without a page and answers every
// run with its event payload (live: 247 runs, 2.7 MB, which cut at the
// 1 MiB read broke the roller's JSON): ActionRuns always asks for a page.
func TestActionRunsAlwaysAsksForAPage(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Encode())
		io.WriteString(w, `{"workflow_runs":[]}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL}
	for _, q := range []url.Values{{"event": {"push"}, "limit": {"20"}}, {"limit": {"50"}, "page": {"3"}}} {
		if _, err := f.ActionRuns(context.Background(), "tok", "sf", "r", q); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"event=push&limit=20&page=1", "limit=50&page=3"}; !slices.Equal(got, want) {
		t.Fatalf("queries %q, want %q", got, want)
	}
}

// Forgejo files a scheduled run under event push, its trigger_event
// schedule (main's nightly, run 1241): a query for push runs drops it, and
// a query naming no event keeps every run.
func TestActionRunsForAnEventDropsRunsAnotherTriggerStarted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"workflow_runs":[{"id":3,"event":"push","trigger_event":"schedule"},{"id":2,"event":"push","trigger_event":"push"},{"id":1,"event":"push"}]}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL}
	ids := func(q url.Values) []int64 {
		runs, err := f.ActionRuns(context.Background(), "tok", "sf", "r", q)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range runs {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(url.Values{"event": {"push"}}); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("push runs = %v, want 2 and 1: the scheduled run 3 is no push run", got)
	}
	if got := ids(url.Values{}); !slices.Equal(got, []int64{3, 2, 1}) {
		t.Fatalf("runs = %v, want all three", got)
	}
}

// An answer longer than the read limit is an error that says so, not a
// cut body a decoder fails on.
func TestForgejoRefusesAnAnswerPastItsReadLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"workflow_runs":[`+strings.Repeat(" ", 1<<20)+`]}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL}
	if _, err := f.ActionRuns(context.Background(), "tok", "sf", "r", url.Values{"page": {"1"}}); err == nil || !strings.Contains(err.Error(), "longer than 1 MiB") {
		t.Fatalf("err %v", err)
	}
}

// A diff past the 1 MiB read is still the model review's input, cut to its
// bound or at the read and saying so; every other long answer stays an
// error.
func TestPullRequestDiffPastTheReadLimitIsCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("d", 3<<19))
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "b", BotPassword: "p"}
	got, err := f.PullRequestDiff(context.Background(), "sf", "repo", 7, 48<<10)
	if want := strings.Repeat("d", 48<<10) + "\n… (diff cut at 49152 bytes of more than 1 MiB)"; err != nil || got != want {
		t.Fatalf("diff of %d bytes ending %q, err %v", len(got), got[max(0, len(got)-60):], err)
	}
	got, err = f.PullRequestDiff(context.Background(), "sf", "repo", 7, 1<<20)
	if want := strings.Repeat("d", 1<<20) + "\n… (diff cut at 1048576 bytes of more than 1 MiB)"; err != nil || got != want {
		t.Fatalf("diff read at 1 MiB: %d bytes ending %q, err %v", len(got), got[max(0, len(got)-60):], err)
	}
	if _, err := f.ActionRuns(context.Background(), "tok", "sf", "repo", url.Values{}); !errors.Is(err, ErrAnswerTooLong) {
		t.Fatalf("another long answer: %v", err)
	}
}

// Ruling 179's one mapping from a project to factoryd's tokens: a factory
// project's registration tokens, a seed project's seeded secrets as its
// file names them; ruling 164's bound: no seed project ever gets
// factory-projects' token, whatever its file says.
func TestAccess(t *testing.T) {
	f := &Forgejo{ProjectsToken: "projects-tok", Seeded: testSeeded}
	for name, tc := range map[string]struct {
		p    Project
		want ProjectAccess
	}{
		"factory":            {Project{Objects: "factory", MergeToken: "own-merge"}, ProjectAccess{Read: "own-merge", Actions: "projects-tok", Merge: "own-merge"}},
		"code":               {seedProject("sf/code"), ProjectAccess{Read: "registrar-tok", Actions: "registrar-tok", Merge: "merge-tok"}},
		"infra":              {Project{Objects: "seed", Tokens: policy.SeedTokens{Read: policy.SecretConfig}}, ProjectAccess{Read: "config-tok"}},
		"names what is none": {Project{Objects: "seed", Tokens: policy.SeedTokens{Read: "forgejo-projects", Actions: "projects-tok", Merge: "ProjectsToken"}}, ProjectAccess{}},
	} {
		if got := f.Access(tc.p); got != tc.want {
			t.Errorf("%s: Access = %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestEnsureLabelsReportsWhatItCreated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			servePage(w, r, 50, `{"id":1,"name":"agent"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	if c, err := f.EnsureLabels(context.Background(), "sf", "r", map[string]string{"agent": "#1", "human": "#2", "bug": "#3"}); c != "created bug, human" || err != nil {
		t.Fatalf("= %q, %v", c, err)
	}
	if c, err := f.EnsureLabels(context.Background(), "sf", "r", map[string]string{"agent": "#1"}); c != Unchanged || err != nil {
		t.Fatalf("all present = %q, %v", c, err)
	}
}

// JobLog asks for the log's last 1 MiB with a suffix Range, which
// Forgejo's actions.Logs honours (http.ServeContent): a log past it
// answers its tail, where the failed step's error stands, not its head.
// A server that ignores the Range answers the head, which JobLog refuses.
func TestJobLogReadsTheTail(t *testing.T) {
	// 40000 lines of 30 bytes, then the runner's line
	head := strings.Repeat("2026-10-14T20:28:00Z filler 1\n", 40000)
	last := "2026-10-14T20:29:01Z \u2699\ufe0f [runner]: context canceled\n"
	full := head + last
	honour := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/user/login":
			http.Redirect(w, r, "/", http.StatusSeeOther)
		case r.URL.Path == "/sf/code/actions/runs/7/jobs/0/logs" && honour:
			if got := r.Header.Get("Range"); got != "bytes=-1048576" {
				t.Errorf("Range = %q, want the last 1 MiB", got)
			}
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(full))
		case r.URL.Path == "/sf/code/actions/runs/7/jobs/0/logs":
			io.WriteString(w, full)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"}

	body, err := f.JobLog(context.Background(), "/sf/code/actions/runs/7/jobs/0")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 1<<20 || !strings.HasSuffix(string(body), last) || string(body) != full[len(full)-1<<20:] {
		t.Fatalf("JobLog answered %d bytes ending %q, want the log's last 1048576 of %d", len(body), body[max(0, len(body)-80):], len(full))
	}

	honour = false
	if _, err := f.JobLog(context.Background(), "/sf/code/actions/runs/7/jobs/0"); !errors.Is(err, ErrAnswerTooLong) {
		t.Fatalf("JobLog with the Range ignored: %v, want ErrAnswerTooLong", err)
	}
}
