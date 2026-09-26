package factoryd

import (
	"context"
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

	f := &Forgejo{BaseURL: srv.URL, AdminToken: "admin-tok"}
	tok, err := f.MintToken(context.Background(), "factory-bot", "task-t1", []Repo{{Owner: "sf", Name: "ai-software-factory-code"}})
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if gotPath != "/api/v1/admin/users/factory-bot/tokens" {
		t.Fatalf("mint path = %s", gotPath)
	}
	if gotAuth != "token admin-tok" {
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
	f := &Forgejo{BaseURL: srv.URL, AdminToken: "admin-tok"}
	_, err := f.MintToken(context.Background(), "bot", "task-t1", []Repo{{Owner: "sf", Name: "r"}})
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

	f := &Forgejo{BaseURL: srv.URL, AdminToken: "admin-tok"}
	if err := f.RevokeToken(context.Background(), "factory-bot", 77); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/admin/users/factory-bot/tokens/77" {
		t.Fatalf("revoke = %s %s", gotMethod, gotPath)
	}
}

func TestForgejoRevokeRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	f := &Forgejo{BaseURL: srv.URL, AdminToken: "admin-tok"}
	err := f.RevokeToken(context.Background(), "bot", 1)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the 403 surfaced", err)
	}
}
