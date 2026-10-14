package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// forgejoRunnerImage is the forgejo-runner the chart pins for CI pods
// (task 41), run the same way ci.go's runnerPodSpec runs it: one-job,
// host-mode, authenticated with the uuid and token RegisterCIRunner
// assigns (no interactive register step, no per-job docker image).
const forgejoRunnerImage = "code.forgejo.org/forgejo/runner:13.2.0"

// runActionsJob pushes a one-job workflow that prints 100000 numbered
// lines, a log past the 1 MiB JobLog reads, then echoes marker, to sha's
// commit on repo's main and waits for Forgejo to run it for real, using a
// disposable forgejo-runner container registered against repo. It returns
// the finished job's commit status, whatever its outcome — the caller
// decides what a "finished job" needs from there.
func runActionsJob(t *testing.T, bot *Forgejo, token string, repo Repo, marker string) CommitStatus {
	t.Helper()
	ctx := context.Background()
	workflow := fmt.Sprintf("on: [push]\njobs:\n  log:\n    runs-on: ubuntu-latest\n    steps:\n      - run: seq 1 100000 && echo %q\n", marker)
	sha, err := bot.CreateFiles(ctx, token, repo, "main", "Add workflow", map[string][]byte{".forgejo/workflows/log.yml": []byte(workflow)})
	if err != nil {
		t.Fatalf("push the workflow: %v", err)
	}

	reg, err := bot.RegisterCIRunner(ctx, CIRepo{Repo: repo, Token: token}, "t115joblog")
	if err != nil {
		t.Fatalf("register a runner: %v", err)
	}
	secretDir := t.TempDir()
	if err := os.WriteFile(secretDir+"/token", []byte(reg.Secret), 0o600); err != nil {
		t.Fatalf("write the runner's token file: %v", err)
	}
	runtime := containerRuntime(t)
	out, err := exec.Command(runtime, "run", "-d", "--rm", "--network", "host", "-v", secretDir+":/secrets:ro",
		forgejoRunnerImage, "forgejo-runner", "one-job", "--url", bot.BaseURL, "--uuid", reg.UUID,
		"--token-url", "file:/secrets/token", "--label", "ubuntu-latest:host", "--wait").CombinedOutput()
	if err != nil {
		t.Fatalf("%s run %s: %v\n%s", runtime, forgejoRunnerImage, err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command(runtime, "stop", id).Run() }) // #nosec G104 -- --rm removes it; a failed stop leaves a container, not a wrong result

	// bot authenticates with its own basic-auth credentials here (CombinedStatus
	// uses the receiver's BotUser/BotPassword); a bare &Forgejo{BaseURL: base}
	// sends none and Forgejo answers 401 for the whole deadline.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		cs, err := bot.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
		if err == nil && len(cs.Statuses) > 0 && cs.Statuses[0].State != "pending" {
			return cs.Statuses[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job did not finish within 3 minutes: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// Forgejo 15.0.9 serves a finished job's raw log only on the web route
// behind a signed-in session (routers/web/web.go: GET
// .../actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs, actions.Logs);
// its own swagger.json lists no /api/v1 log route, and the design's guess
// at one (GET /api/v1/repos/{owner}/{repo}/actions/jobs/{job_id}/logs) 404s,
// as does the web route without the session (basic auth and a scoped
// write:repository token both reach the equivalent /api/v1 calls but 404 on
// this one). factoryd's bot user, a write collaborator on every project
// repository, logs in for the session instead (Forgejo.JobLog).
func TestJobLogOnForgejo15(t *testing.T) {
	base, a := startForgejo(t)
	ctx := context.Background()
	root := &Forgejo{BaseURL: base}
	admin := root.adminCall(a)

	bot := &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: "t115-bot-" + fmt.Sprint(time.Now().UnixNano())}
	if _, err := admin(ctx, http.MethodPost, "/api/v1/admin/users", mustJSON(map[string]any{"username": bot.BotUser,
		"password": bot.BotPassword, "email": "agent@example.org", "must_change_password": false}), http.StatusCreated); err != nil {
		t.Fatalf("create the bot user: %v", err)
	}

	repo := Repo{Owner: a.User, Name: "joblog"}
	if _, err := admin(ctx, http.MethodPost, "/api/v1/user/repos",
		mustJSON(map[string]any{"name": repo.Name, "private": true, "default_branch": "main"}), http.StatusCreated); err != nil {
		t.Fatalf("create the repository: %v", err)
	}
	if _, err := admin(ctx, http.MethodPut, "/api/v1/repos/"+repo.String()+"/collaborators/"+bot.BotUser,
		mustJSON(map[string]string{"permission": "write"}), http.StatusNoContent); err != nil {
		t.Fatalf("add the bot as a collaborator: %v", err)
	}
	token, err := root.AdminMintToken(ctx, a, a.User, "seed", []string{"write:repository"}, nil)
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	const marker = "t115-joblog-sentinel"
	status := runActionsJob(t, bot, token, repo, marker)

	// The design's two guessed routes: neither exists.
	apiRoute := fmt.Sprintf("/api/v1/repos/%s/actions/jobs/1/logs", repo)
	if body, err := bot.call(ctx, http.MethodGet, apiRoute, nil, http.StatusOK); err == nil {
		t.Fatalf("stage12-design.md's guessed API route now exists, contrary to its own note: %s answered %q", apiRoute, body)
	}
	if resp, err := http.Get(base + status.TargetURL + "/logs"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("stage12-design.md's guessed web route (target_url + /logs, no attempt segment) now answers 200: %s", status.TargetURL+"/logs")
		}
	}

	log, err := bot.JobLog(ctx, status.TargetURL)
	if err != nil {
		t.Fatalf("JobLog: %v", err)
	}
	if !strings.Contains(string(log), marker) {
		t.Fatalf("job log does not contain %q: %s", marker, log)
	}
	// the log's tail, cut at 1 MiB: the runner's first line is gone
	if len(log) != maxForgejoAnswer || strings.Contains(string(log), "received task") || !strings.Contains(string(log), " 100000\n") {
		t.Fatalf("job log of %d bytes starting %q, want its last 1048576", len(log), log[:min(200, len(log))])
	}
}
