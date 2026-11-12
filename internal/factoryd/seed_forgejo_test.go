package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// forgejoImage is the Forgejo image the chart pins, by the same digest.
const forgejoImage = "code.forgejo.org/forgejo/forgejo:17.0.1-rootless@sha256:b76f32add7267b2a791ea04014a441cf9d086050e7c0250feec9fb7066e9828f"

// containerRuntime is docker or podman, whichever answers; it skips the
// test when neither does (main CI's dind cannot pull and port-map images).
func containerRuntime(t *testing.T) string {
	t.Helper()
	if os.Getenv("FACTORYD_FORGEJO_TEST") == "" {
		t.Skip("set FACTORYD_FORGEJO_TEST=1 to run the Forgejo container tests (main CI's dind cannot pull and port-map the image)")
	}
	for _, rt := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(rt); err == nil && exec.Command(rt, "info").Run() == nil {
			return rt
		}
	}
	t.Skip("no container runtime answers (docker or podman): the Forgejo 17.0.1 container cannot start")
	return ""
}

// startForgejo runs a disposable Forgejo 17.0.1 container with a site
// admin and returns its base URL and the admin's login; it skips when no
// container runtime answers.
func startForgejo(t *testing.T) (string, AdminAuth) {
	t.Helper()
	runtime := containerRuntime(t)
	out, err := exec.Command(runtime, "run", "-d", "--rm", "-p", "127.0.0.1::3000",
		"-e", "FORGEJO__security__INSTALL_LOCK=true", "-e", "FORGEJO__database__DB_TYPE=sqlite3",
		"-e", "FORGEJO__actions__ENABLED=true",
		// Production's ROOT_URL is https (k8s/platform/forgejo/helmrelease.yaml),
		// which makes Forgejo mark its session cookie Secure by default; an http
		// ROOT_URL here would hide that and let JobLog's httpsJar go untested.
		"-e", "FORGEJO__server__ROOT_URL=https://git.example.org/", forgejoImage).CombinedOutput()
	if err != nil {
		t.Fatalf("%s run %s: %v\n%s", runtime, forgejoImage, err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command(runtime, "stop", id).Run() }) // #nosec G104 -- --rm removes it; a failed stop leaves a container, not a wrong result
	deadline := time.Now().Add(60 * time.Second)
	var base, version string
	for {
		var err error
		if base == "" {
			var out []byte
			if out, err = exec.Command(runtime, "port", id, "3000").CombinedOutput(); err == nil {
				base = "http://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
			}
		}
		if base != "" {
			var resp *http.Response
			if resp, err = http.Get(base + "/api/v1/version"); err == nil {
				var v struct{ Version string }
				err = json.NewDecoder(resp.Body).Decode(&v)
				resp.Body.Close()
				if err == nil && resp.StatusCode == http.StatusOK {
					version = v.Version
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Forgejo (%s) did not answer within a minute: %v", base, err)
		}
		time.Sleep(time.Second)
	}
	if want := "17.0.1"; version != want && !strings.HasPrefix(version, want+"+") {
		t.Fatalf("the harness Forgejo reports version %q, want %s", version, want)
	}
	a := AdminAuth{User: "t99admin", Password: "t99-admin-" + fmt.Sprint(time.Now().UnixNano())}
	out, err = exec.Command(runtime, "exec", "-u", "git", id, "forgejo", "admin", "user", "create", "--admin",
		"--username", a.User, "--password", a.Password, "--email", "t99admin@example.org", "--must-change-password=false").CombinedOutput()
	if err != nil {
		t.Fatalf("create the site admin: %v\n%s", err, out)
	}
	return base, a
}

// The contents API commits several created files as one commit into an
// empty repository, whose main it makes (stage11-design.md section 6: the
// seed step's one unverified assumption).
func TestContentsAPIOnEmptyRepository(t *testing.T) {
	base, a := startForgejo(t)
	ctx := context.Background()
	f := &Forgejo{BaseURL: base}
	admin := f.adminCall(a)
	token, err := f.AdminMintToken(ctx, a, a.User, "seed", []string{"write:repository"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := Repo{Owner: a.User, Name: "empty"}
	if _, err := admin(ctx, http.MethodPost, "/api/v1/user/repos",
		mustJSON(map[string]any{"name": repo.Name, "private": true, "default_branch": "main"}), http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	if ok, err := branchExists(ctx, f, token, repo, "main"); err != nil || ok {
		t.Fatalf("main before the seed: %v, %v", ok, err)
	}
	files := map[string][]byte{".forgejo/workflows/pr.yml": []byte("name: pr\n"), "deploy/base/kustomization.yaml": []byte("resources: []\n")}
	sha, err := f.CreateFiles(ctx, token, repo, "main", Attribution{Role: "registration"}, "Seed from template ts-go (registration)", files)
	if err != nil {
		t.Fatalf("CreateFiles on an empty repository: %v", err)
	}
	head, err := f.tokenCall(token)(ctx, http.MethodGet, "/api/v1/repos/"+repo.String()+"/branches/main", nil, http.StatusOK)
	if err != nil {
		t.Fatalf("main after the seed: %v", err)
	}
	if !strings.Contains(string(head), `"id":"`+sha+`"`) {
		t.Fatalf("main's head is not the seed commit %s: %s", sha, head)
	}
	for path, want := range files {
		got, err := f.tokenCall(token)(ctx, http.MethodGet, "/api/v1/repos/"+repo.String()+"/raw/"+path+"?ref=main", nil, http.StatusOK)
		if err != nil || string(got) != string(want) {
			t.Errorf("%s on main = %q, %v; want %q", path, got, err, want)
		}
	}
}

func branchExists(ctx context.Context, f *Forgejo, token string, repo Repo, branch string) (bool, error) {
	_, err := f.tokenCall(token)(ctx, http.MethodGet, "/api/v1/repos/"+repo.String()+"/branches/"+branch, nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func forgejoIssueRepo(t *testing.T) (func(context.Context, string, string, []byte, int) ([]byte, error), string) {
	t.Helper()
	base, a := startForgejo(t)
	admin := (&Forgejo{BaseURL: base}).adminCall(a)
	ctx := context.Background()
	if _, err := admin(ctx, http.MethodPost, "/api/v1/user/repos", mustJSON(map[string]any{"name": "iss", "auto_init": true}), http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	p := "/api/v1/repos/" + a.User + "/iss"
	if _, err := admin(ctx, http.MethodPost, p+"/issues", mustJSON(map[string]any{"title": "first", "body": "original"}), http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	return admin, p
}

// Forgejo refuses an issue comment with an empty body and accepts a blank
// one.
func TestIssueCommentEmptyBodyOnForgejo(t *testing.T) {
	admin, p := forgejoIssueRepo(t)
	ctx := context.Background()
	_, err := admin(ctx, http.MethodPost, p+"/issues/1/comments", mustJSON(map[string]any{"body": ""}), http.StatusCreated)
	if err == nil || !strings.Contains(err.Error(), `status 422: {"message":"[Body]: Required"`) {
		t.Fatalf("empty body = %v, want status 422 with [Body]: Required", err)
	}
	for _, blank := range []string{" ", "\n"} {
		if _, err := admin(ctx, http.MethodPost, p+"/issues/1/comments", mustJSON(map[string]any{"body": blank}), http.StatusCreated); err != nil {
			t.Errorf("blank body %q = %v, want 201", blank, err)
		}
	}
	got, err := admin(ctx, http.MethodGet, p+"/issues/1/comments", nil, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	var comments []struct{ Body string }
	if err := json.Unmarshal(got, &comments); err != nil || len(comments) != 2 {
		t.Fatalf("comments after one refused and two blank posts = %d, %v; want 2", len(comments), err)
	}
}

// A PATCH on a closed issue is not refused: it answers 201 like the one on
// an open issue and applies body and title; a state of closed on a closed
// issue answers 201 and leaves it closed; an edit of a comment on it
// answers 200.
func TestPatchClosedIssueOnForgejo(t *testing.T) {
	admin, p := forgejoIssueRepo(t)
	ctx := context.Background()
	comment, err := admin(ctx, http.MethodPost, p+"/issues/1/comments", mustJSON(map[string]any{"body": "c"}), http.StatusCreated)
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ ID int64 }
	if err := json.Unmarshal(comment, &c); err != nil {
		t.Fatal(err)
	}
	if _, err := admin(ctx, http.MethodPatch, p+"/issues/1", mustJSON(map[string]any{"state": "closed"}), http.StatusCreated); err != nil {
		t.Fatalf("close: %v", err)
	}
	read := func() (issue struct{ State, Title, Body string }) {
		got, err := admin(ctx, http.MethodGet, p+"/issues/1", nil, http.StatusOK)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &issue); err != nil {
			t.Fatal(err)
		}
		return issue
	}
	if _, err := admin(ctx, http.MethodPatch, p+"/issues/1", mustJSON(map[string]any{"body": "edited", "title": "second"}), http.StatusCreated); err != nil {
		t.Fatalf("edit body and title of a closed issue: %v", err)
	}
	if got := read(); got.State != "closed" || got.Body != "edited" || got.Title != "second" {
		t.Errorf("after the edit: %+v, want closed, edited, second", got)
	}
	if _, err := admin(ctx, http.MethodPatch, p+"/issues/1", mustJSON(map[string]any{"state": "closed"}), http.StatusCreated); err != nil {
		t.Fatalf("close a closed issue: %v", err)
	}
	if got := read(); got.State != "closed" {
		t.Errorf("after a second close: %+v, want closed", got)
	}
	if _, err := admin(ctx, http.MethodPatch, p+fmt.Sprintf("/issues/comments/%d", c.ID), mustJSON(map[string]any{"body": "c2"}), http.StatusOK); err != nil {
		t.Fatalf("edit a comment of a closed issue: %v", err)
	}
}
