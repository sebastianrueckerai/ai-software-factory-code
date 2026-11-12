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

// A roll's branch is cut at a commit main has moved past, and its pull
// request merges from behind main: Forgejo 17 cuts a branch at a commit
// sha, and main's branch protection, which does not block outdated
// branches, lets the merge land with both main's newer commit and the pin.
func TestRollBranchCutBehindMainMergesOnForgejo(t *testing.T) {
	base, a := startForgejo(t)
	ctx := context.Background()
	admin := (&Forgejo{BaseURL: base}).adminCall(a)
	must := func(_ []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	bot := &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: "t4g5-bot-" + fmt.Sprint(time.Now().UnixNano())}
	must(admin(ctx, http.MethodPost, "/api/v1/admin/users", mustJSON(map[string]any{"username": bot.BotUser, "password": bot.BotPassword,
		"email": "agent@example.org", "must_change_password": false}), http.StatusCreated))
	must(admin(ctx, http.MethodPost, "/api/v1/orgs", mustJSON(map[string]any{"username": "linkshort"}), http.StatusCreated))
	must(admin(ctx, http.MethodPost, "/api/v1/orgs/linkshort/repos", mustJSON(map[string]any{"name": "linkshort", "private": true,
		"auto_init": true, "default_branch": "main"}), http.StatusCreated))
	up := Repo{Owner: "linkshort", Name: "linkshort"}
	must(admin(ctx, http.MethodPut, "/api/v1/repos/linkshort/linkshort/collaborators/"+bot.BotUser,
		mustJSON(map[string]string{"permission": "write"}), http.StatusNoContent))
	must(admin(ctx, http.MethodPost, "/api/v1/repos/linkshort/linkshort/branch_protections", mustJSON(map[string]any{
		"rule_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{a.User}}), http.StatusCreated))
	create := func(path, text string) {
		t.Helper()
		must(admin(ctx, http.MethodPost, "/api/v1/repos/linkshort/linkshort/contents", mustJSON(map[string]any{"branch": "main",
			"message": "add " + path, "files": []map[string]string{{"operation": "create", "path": path,
				"content": base64.StdEncoding.EncodeToString([]byte(text))}}}), http.StatusCreated))
	}

	create("pins.txt", "old\n")
	rolled, err := bot.BranchHeadSHA(ctx, up.Owner, up.Name, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.EnsureFork(ctx, up); err != nil {
		t.Fatal(err)
	}
	fork := bot.Fork(up)
	create("other.txt", "a card merged after the rolled commit\n")
	if err := bot.SyncFork(ctx, fork); err != nil {
		t.Fatal(err)
	}
	mainHead, err := bot.BranchHeadSHA(ctx, up.Owner, up.Name, "main")
	if err != nil || mainHead == rolled {
		t.Fatalf("main head %s, rolled %s: main must have moved past the rolled commit (%v)", mainHead, rolled, err)
	}

	if err := bot.CreateBranchAt(ctx, fork, "factory/t-roll", rolled); err != nil {
		t.Fatalf("cut at a commit sha: %v", err)
	}
	_, blob, err := bot.FileContent(ctx, fork.Owner, fork.Name, rolled, "pins.txt")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := bot.CommitFiles(ctx, fork.Owner, fork.Name, "factory/t-roll", "", Attribution{Role: "roll", Task: "t-roll"}, "roll",
		[]FileChange{{Path: "pins.txt", Content: []byte("new\n"), Blob: blob}})
	if err != nil {
		t.Fatal(err)
	}
	if parents, err := bot.CommitParents(ctx, fork.Owner, fork.Name, commit); err != nil || !slices.Equal(parents, []string{rolled}) {
		t.Fatalf("the roll commit's parents %v (%v), want the rolled commit %s", parents, err, rolled)
	}
	pr, err := bot.CreatePullRequest(ctx, up.Owner, up.Name, fork.Owner+":factory/t-roll", "main", "roll", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = bot.MergePullRequestAsBot(ctx, up.Owner, up.Name, pr.Number, commit, Attribution{Role: "merge", Task: "t-roll"}, "roll")
		if err == nil {
			break
		}
		if time.Now().After(deadline) || errors.Is(err, ErrRefused) {
			t.Fatalf("merge of a roll whose branch is behind main: %v", err)
		}
		time.Sleep(time.Second)
	}
	for path, want := range map[string]string{"pins.txt": "new\n", "other.txt": "a card merged after the rolled commit\n"} {
		got, _, err := bot.FileContent(ctx, up.Owner, up.Name, "main", path)
		if err != nil || string(got) != want {
			t.Fatalf("main's %s = %q (%v), want %q", path, got, err, want)
		}
	}
}
