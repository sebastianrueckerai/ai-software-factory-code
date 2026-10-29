package factoryd

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// The merge queue's sync (stage11-design.md 1.4, its one unverified
// assumption): the bot, a write collaborator of a private project
// repository whose main is protected, updates a pull request whose head is
// a branch of its own fork with style merge; the head then holds main's
// commit. An up-to-date branch's update moves nothing; a main that
// conflicts with the branch answers 409. SyncWithMain on the same pull
// request moves it, moves nothing when up to date, answers a retried
// move, and reports the conflict.
func TestUpdateBranchAsBotOnFork(t *testing.T) {
	base, a := startForgejo(t)
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

	// the activity on the same pull request: main moves again
	store := newFakeStore()
	store.CreateTask(ctx, Task{ID: "q1", Status: "in-review", Repository: up.String(), PRNumber: pr.Number, Track: "direct"})
	r, err := NewRunner(testRunnerConfig(), store, newFakeKube(), bot, &Completion{Store: store, Forgejo: bot}, &fakeObjectStore{}, testSupervisorModel("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	create(admin, up, "main", "third.go", "package third\n")
	main2, err := bot.BranchHeadSHA(ctx, up.Owner, up.Name, "main")
	if err != nil {
		t.Fatal(err)
	}
	synced, err := r.SyncWithMain(ctx, "q1", pr.Number, after.HeadSHA)
	if err != nil || synced.From != after.HeadSHA || synced.Main != main2 || synced.To == synced.From || synced.Conflict != "" {
		t.Fatalf("SyncWithMain = %+v, %v: want a move from %s with main %s", synced, err, after.HeadSHA, main2)
	}
	if parents, err := bot.CommitParents(ctx, fork.Owner, fork.Name, synced.To); err != nil || !slices.Equal(parents, []string{after.HeadSHA, main2}) {
		t.Fatalf("the synced head's parents = %v %v", parents, err)
	}
	if again, err := r.SyncWithMain(ctx, "q1", pr.Number, synced.To); err != nil || again.To != synced.To {
		t.Fatalf("SyncWithMain of the synced head = %+v, %v: want no move", again, err)
	}
	// a retry whose first attempt's answer was lost still answers the move
	if retry, err := r.SyncWithMain(ctx, "q1", pr.Number, after.HeadSHA); err != nil || retry.To != synced.To {
		t.Fatalf("SyncWithMain retried = %+v, %v: want the move to %s", retry, err, synced.To)
	}
	var moves []string
	for _, l := range timelineLines(t, r, "q1") {
		if l["step"] == "queue_synced" {
			moves = append(moves, fmt.Sprintf("%v %v %v", l["from"], l["to"], l["main"]))
		}
	}
	if move := after.HeadSHA + " " + synced.To + " " + main2; !slices.Equal(moves, []string{move, move}) {
		t.Fatalf("queue_synced steps = %q, want the move twice (the call and its retry)", moves)
	}

	create(admin, up, "main", "clash.txt", "main's line\n")
	create(bot.call, fork, "task-1", "clash.txt", "the branch's line\n")
	err = bot.UpdatePullRequest(ctx, up.Owner, up.Name, pr.Number)
	if !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("update against a conflicting main: %v, want ErrUpdateConflict", err)
	}
	t.Logf("conflicting main: %v", err)
	head, err := bot.BranchHeadSHA(ctx, fork.Owner, fork.Name, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.SyncWithMain(ctx, "q1", pr.Number, head); err != nil || res.Conflict == "" {
		t.Fatalf("SyncWithMain against a conflicting main = %+v, %v: want the conflict", res, err)
	}
}

// The retry's branch copy (item 119) on Forgejo 15.0.9: the bot creates
// factory/t-new on its fork at factory/t-old's head; a branch already
// there is kept; a branch that does not exist copies nothing.
func TestCopyBranchOnFork(t *testing.T) {
	base, a := startForgejo(t)
	ctx := context.Background()
	admin := (&Forgejo{BaseURL: base}).adminCall(a)
	must := func(_ []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	bot := &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: "t135-bot-" + fmt.Sprint(time.Now().UnixNano())}
	must(admin(ctx, http.MethodPost, "/api/v1/admin/users", mustJSON(map[string]any{"username": bot.BotUser, "password": bot.BotPassword,
		"email": "agent@example.org", "must_change_password": false}), http.StatusCreated))
	must(admin(ctx, http.MethodPost, "/api/v1/orgs", mustJSON(map[string]any{"username": "linkshort"}), http.StatusCreated))
	must(admin(ctx, http.MethodPost, "/api/v1/orgs/linkshort/repos", mustJSON(map[string]any{"name": "linkshort", "private": true,
		"auto_init": true, "default_branch": "main"}), http.StatusCreated))
	up := Repo{Owner: "linkshort", Name: "linkshort"}
	must(admin(ctx, http.MethodPut, "/api/v1/repos/linkshort/linkshort/collaborators/"+bot.BotUser,
		mustJSON(map[string]string{"permission": "write"}), http.StatusNoContent))
	if err := bot.EnsureFork(ctx, up); err != nil {
		t.Fatal(err)
	}
	fork := bot.Fork(up)
	must(bot.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/branches", fork), mustJSON(map[string]string{"new_branch_name": "factory/t-old",
		"old_branch_name": "main"}), http.StatusCreated))
	must(bot.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/contents", fork), mustJSON(map[string]any{"branch": "factory/t-old",
		"message": "wip", "files": []map[string]string{{"operation": "create", "path": "links.go",
			"content": base64.StdEncoding.EncodeToString([]byte("package links\n"))}}}), http.StatusCreated))
	head := func(branch string) string {
		t.Helper()
		sha, err := bot.BranchHeadSHA(ctx, fork.Owner, fork.Name, branch)
		if err != nil {
			t.Fatalf("%s: %v", branch, err)
		}
		return sha
	}
	old, main := head("factory/t-old"), head("main")
	if old == main {
		t.Fatal("the old branch holds no commit of its own")
	}
	if err := bot.CopyBranch(ctx, fork, "factory/t-old", "factory/t-new"); err != nil {
		t.Fatal(err)
	}
	if got := head("factory/t-new"); got != old {
		t.Fatalf("factory/t-new at %s, want factory/t-old's head %s", got, old)
	}
	must(bot.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/branches", fork), mustJSON(map[string]string{"new_branch_name": "factory/t-own",
		"old_branch_name": "main"}), http.StatusCreated))
	if err := bot.CopyBranch(ctx, fork, "factory/t-old", "factory/t-own"); err != nil {
		t.Fatalf("copy onto an existing branch: %v, want it kept", err)
	}
	if got := head("factory/t-own"); got != main {
		t.Fatalf("factory/t-own at %s, want its own head %s kept", got, main)
	}
	if err := bot.CopyBranch(ctx, fork, "factory/t-gone", "factory/t-x"); err != nil {
		t.Fatalf("copy of a missing branch: %v, want nothing done", err)
	}
	if _, err := bot.BranchHeadSHA(ctx, fork.Owner, fork.Name, "factory/t-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("factory/t-x after copying a missing branch: %v, want not found", err)
	}
}
