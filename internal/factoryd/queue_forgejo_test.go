package factoryd

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// forgejo15 runs a disposable Forgejo 15.0.9 container (the chart's pin)
// with a site admin and returns its base URL and the admin's login; it
// skips when no container runtime answers.
func forgejo15(t *testing.T) (string, AdminAuth) {
	t.Helper()
	var runtime string
	for _, rt := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(rt); err == nil && exec.Command(rt, "info").Run() == nil {
			runtime = rt
			break
		}
	}
	if runtime == "" {
		t.Skip("no container runtime answers (docker or podman): the Forgejo 15.0.9 container cannot start")
	}
	out, err := exec.Command(runtime, "run", "-d", "--rm", "-p", "127.0.0.1::3000",
		"-e", "FORGEJO__security__INSTALL_LOCK=true", "-e", "FORGEJO__database__DB_TYPE=sqlite3", "codeberg.org/forgejo/forgejo:15.0.9").CombinedOutput()
	if err != nil {
		t.Fatalf("%s run: %v\n%s", runtime, err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command(runtime, "stop", id).Run() }) // #nosec G104 -- --rm removes it; a failed stop leaves a container, not a wrong result
	out, err = exec.Command(runtime, "port", id, "3000").CombinedOutput()
	if err != nil {
		t.Fatalf("%s port: %v\n%s", runtime, err, out)
	}
	base := "http://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	deadline := time.Now().Add(time.Minute)
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
	a := AdminAuth{User: "t102admin", Password: "t102-admin-" + fmt.Sprint(time.Now().UnixNano())}
	out, err = exec.Command(runtime, "exec", "-u", "git", id, "forgejo", "admin", "user", "create", "--admin",
		"--username", a.User, "--password", a.Password, "--email", "t102admin@example.org", "--must-change-password=false").CombinedOutput()
	if err != nil {
		t.Fatalf("create the site admin: %v\n%s", err, out)
	}
	return base, a
}

// The merge queue's sync (stage11-design.md 1.4, its one unverified
// assumption): the bot, a write collaborator of a private project
// repository whose main is protected, updates a pull request whose head is
// a branch of its own fork with style merge; the head then holds main's
// commit. An up-to-date branch's update moves nothing; a main that
// conflicts with the branch answers 409.
func TestUpdateBranchAsBotOnFork(t *testing.T) {
	base, a := forgejo15(t)
	ctx := context.Background()
	admin := (&Forgejo{BaseURL: base}).adminCall(a)
	must := func(_ []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	bot := &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: "t102-bot-" + fmt.Sprint(time.Now().UnixNano())}
	must(admin(ctx, http.MethodPost, "/api/v1/admin/users", mustJSON(map[string]any{"username": bot.BotUser, "password": bot.BotPassword,
		"email": "agent@example.org", "must_change_password": false}), http.StatusCreated))
	must(admin(ctx, http.MethodPost, "/api/v1/orgs", mustJSON(map[string]any{"username": "linkshort"}), http.StatusCreated))
	must(admin(ctx, http.MethodPost, "/api/v1/orgs/linkshort/repos", mustJSON(map[string]any{"name": "linkshort", "private": true,
		"auto_init": true, "default_branch": "main"}), http.StatusCreated))
	up := Repo{Owner: "linkshort", Name: "linkshort"}
	must(admin(ctx, http.MethodPut, "/api/v1/repos/linkshort/linkshort/collaborators/"+bot.BotUser,
		mustJSON(map[string]string{"permission": "write"}), http.StatusNoContent))
	must(admin(ctx, http.MethodPost, "/api/v1/repos/linkshort/linkshort/branch_protections", mustJSON(map[string]any{
		"rule_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{a.User},
		"required_approvals": 1, "dismiss_stale_approvals": true}), http.StatusCreated))
	create := func(call func(context.Context, string, string, []byte, int) ([]byte, error), repo Repo, branch, path, text string) {
		t.Helper()
		must(call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/contents", repo), mustJSON(map[string]any{"branch": branch,
			"message": "add " + path, "files": []map[string]string{{"operation": "create", "path": path,
				"content": base64.StdEncoding.EncodeToString([]byte(text))}}}), http.StatusCreated))
	}

	if err := bot.EnsureFork(ctx, up); err != nil {
		t.Fatal(err)
	}
	fork := bot.Fork(up)
	must(bot.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/branches", fork), mustJSON(map[string]string{"new_branch_name": "task-1",
		"old_branch_name": "main"}), http.StatusCreated))
	create(bot.call, fork, "task-1", "feature.go", "package feature\n")
	pr, err := bot.CreatePullRequest(ctx, up.Owner, up.Name, fork.Owner+":task-1", "main", "Task 1", "")
	if err != nil {
		t.Fatal(err)
	}
	create(admin, up, "main", "other.go", "package other\n")
	mainHead, err := bot.BranchHeadSHA(ctx, up.Owner, up.Name, "main")
	if err != nil {
		t.Fatal(err)
	}

	if err := bot.UpdatePullRequest(ctx, up.Owner, up.Name, pr.Number); err != nil {
		t.Fatalf("update as the bot: %v", err)
	}
	after, err := bot.PullRequestState(ctx, up.Owner, up.Name, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	branchHead, err := bot.BranchHeadSHA(ctx, fork.Owner, fork.Name, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadSHA == pr.HeadSHA || branchHead != after.HeadSHA {
		t.Fatalf("head %s -> %s, fork branch at %s: want a new head the branch holds", pr.HeadSHA, after.HeadSHA, branchHead)
	}
	parents, err := bot.CommitParents(ctx, fork.Owner, fork.Name, after.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(parents, []string{pr.HeadSHA, mainHead}) {
		t.Fatalf("the update's parents = %v, want [the old head %s, main %s]", parents, pr.HeadSHA, mainHead)
	}
	t.Logf("update as the bot: head %s -> %s (parents %v), main %s, merge base %q", pr.HeadSHA, after.HeadSHA, parents, mainHead, after.MergeBase)

	// the merge base Forgejo reports lags the update (it recomputes it in
	// the background), so an up-to-date branch may read as behind: its
	// update must move nothing
	if err := bot.UpdatePullRequest(ctx, up.Owner, up.Name, pr.Number); err != nil {
		t.Fatalf("update of an up-to-date branch: %v", err)
	}
	if same, err := bot.PullRequestState(ctx, up.Owner, up.Name, pr.Number); err != nil || same.HeadSHA != after.HeadSHA {
		t.Fatalf("update of an up-to-date branch moved the head %s -> %s (%v)", after.HeadSHA, same.HeadSHA, err)
	}
	create(admin, up, "main", "clash.txt", "main's line\n")
	create(bot.call, fork, "task-1", "clash.txt", "the branch's line\n")
	err = bot.UpdatePullRequest(ctx, up.Owner, up.Name, pr.Number)
	if !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("update against a conflicting main: %v, want ErrUpdateConflict", err)
	}
	t.Logf("conflicting main: %v", err)
}
