package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	tok, err := f.MintToken(context.Background(), "task-t1", []Repo{{Owner: "sf", Name: "ai-software-factory-code"}})
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
	_, err := f.MintToken(context.Background(), "task-t1", []Repo{{Owner: "sf", Name: "r"}})
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

func TestForgejoPullRequestHeadBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/r/pulls/7" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"head":{"ref":"factory/t1"}}`)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ref, err := f.PullRequestHeadBranch(context.Background(), "sf", "r", 7)
	if err != nil || ref != "factory/t1" {
		t.Fatalf("PullRequestHeadBranch = (%q, %v), want (factory/t1, nil)", ref, err)
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
