package factoryd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// forgejoImage is the Forgejo the chart pins.
const forgejoImage = "codeberg.org/forgejo/forgejo:15.0.9"

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
	t.Skip("no container runtime answers (docker or podman): the Forgejo 15.0.9 container cannot start")
	return ""
}

// startForgejo runs a disposable Forgejo 15.0.9 container with a site
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
	out, err = exec.Command(runtime, "port", id, "3000").CombinedOutput()
	if err != nil {
		t.Fatalf("%s port: %v\n%s", runtime, err, out)
	}
	base := "http://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(base + "/api/v1/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Forgejo at %s did not answer within a minute: %v", base, err)
		}
		time.Sleep(time.Second)
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
