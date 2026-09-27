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
	"slices"
	"strconv"
	"strings"
	"testing"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if err := f.EnsureLabels(context.Background(), "sf", "r", wanted); err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if len(created) != 1 || created[0] != "human" {
		t.Fatalf("created = %v, want exactly [human] (agent, state/queued already existed)", created)
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
	err := f.EnsureLabels(context.Background(), "sf", "r", map[string]string{"agent": "#ededed", "human": "#c2e0c6"})
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
func TestForgejoTreeEntriesPagesATruncatedListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/api/v1/repos/sf/r/git/trees/c1?page=1":
			io.WriteString(w, `{"truncated":true,"total_count":3,"tree":[{"path":".github","sha":"g"},{"path":"README.md","sha":"r"}]}`)
		case "/api/v1/repos/sf/r/git/trees/c1?page=2":
			io.WriteString(w, `{"truncated":true,"total_count":3,"tree":[{"path":".forgejo","sha":"f1"}]}`)
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
	got, err := f.TreeEntries(ctx, "sf", "r", "c1", WorkflowDirs...)
	if err != nil || !maps.Equal(got, map[string]string{".github": "g", ".forgejo": "f1"}) {
		t.Fatalf("c1 = (%v, %v), want .github from page 1 and .forgejo from page 2", got, err)
	}
	if got, err := f.TreeEntries(ctx, "sf", "r", "c2", WorkflowDirs...); err != nil || len(got) != 0 {
		t.Fatalf("c2 = (%v, %v), want none", got, err)
	}
	if _, err := f.TreeEntries(ctx, "sf", "r", "c3", WorkflowDirs...); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c3 = %v, want ErrNotFound: a commit Forgejo does not have", err)
	}
	// a listing that ends short of its total names no entry for sure
	if got, err := f.TreeEntries(ctx, "sf", "r", "c4", WorkflowDirs...); err == nil {
		t.Fatalf("c4 = %v, want an error: the pages ended short of total_count", got)
	}
}

// firstPage serves h for a paged list's first page and an empty page
// after it, for stubs that answer a list whatever the page asked.
func firstPage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Query().Get("page"); p != "" && p != "1" && !strings.Contains(r.URL.Path, "/git/trees/") {
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
// token (I3), and the open pull request may sit on any page.
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

// A 4xx no retry can fix is a refusal (I4): the activity interceptor ends
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

// The trust gate trusts code writers only (I-2): members of the
// repository's teams with code write, and direct collaborators with write.
// A collaborator's own permission is unreadable with a scoped token, so a
// collaborator counts by being on the assignee list (write or more on some
// unit), unless a team without code write could be what put them there
// (Forgejo lists a read-only team's members as assignees through its pull
// request read): then they fail closed. Every call carries the registrar
// token.
func TestForgejoRepoWritersTrustsCodeWritersOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			t.Errorf("%s read without the registrar token: %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/repos/sf/r/teams":
			io.WriteString(w, `[
				{"id":1,"name":"Owners","permission":"owner","units_map":{"repo.code":"owner","repo.pulls":"owner"}},
				{"id":2,"name":"reviewers","permission":"read","units_map":{"repo.code":"read","repo.pulls":"read"}},
				{"id":3,"name":"devs","permission":"read","units_map":{"repo.code":"write","repo.issues":"read"}}]`)
		case "/api/v1/teams/1/members":
			servePage(w, r, 1, `{"login":"sebastian"}`, `{"login":"factory-ci"}`)
		case "/api/v1/teams/2/members":
			servePage(w, r, 1, `{"login":"rita"}`, `{"login":"carol"}`)
		case "/api/v1/teams/3/members":
			servePage(w, r, 1, `{"login":"dave"}`)
		case "/api/v1/repos/sf/r/collaborators":
			servePage(w, r, 2, `{"login":"factory-agent"}`, `{"login":"carol"}`, `{"login":"erin"}`)
		case "/api/v1/repos/sf/r/assignees":
			io.WriteString(w, `[{"login":"carol"},{"login":"dave"},{"login":"factory-agent"},{"login":"factory-ci"},{"login":"rita"},{"login":"sebastian"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, CIRegistrarToken: "tok"}
	got, err := f.RepoWriters(context.Background(), "sf", "r")
	// rita: a read-only team's member; carol: a collaborator who is one too;
	// erin: a read collaborator, no assignee
	if want := []string{"dave", "factory-agent", "factory-ci", "sebastian"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("writers = %q, %v; want %q", got, err, want)
	}
	if _, err := f.RepoWriters(context.Background(), "sf", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repository: %v, want ErrNotFound", err)
	}
}
