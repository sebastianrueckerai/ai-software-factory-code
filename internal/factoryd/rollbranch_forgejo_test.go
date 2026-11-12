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

// rollBranchCase is a roll's branch cut at the commit of a roll that main
// has moved past: main's own change after the cut runs through edit, and
// the branch's one commit pins pins.txt's first line. It returns the bot,
// the upstream, the roll's pull request and its head commit.
func rollBranchCase(t *testing.T, edit func(admin func(path, text string) error)) (bot *Forgejo, up Repo, pr PullRequestRef, commit string) {
	t.Helper()
	base, a := startForgejo(t)
	ctx := context.Background()
	adminCall := (&Forgejo{BaseURL: base}).adminCall(a)
	must := func(_ []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	bot = &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: "t4g5-bot-" + fmt.Sprint(time.Now().UnixNano())}
	must(adminCall(ctx, http.MethodPost, "/api/v1/admin/users", mustJSON(map[string]any{"username": bot.BotUser, "password": bot.BotPassword,
		"email": "agent@example.org", "must_change_password": false}), http.StatusCreated))
	must(adminCall(ctx, http.MethodPost, "/api/v1/orgs", mustJSON(map[string]any{"username": "linkshort"}), http.StatusCreated))
	must(adminCall(ctx, http.MethodPost, "/api/v1/orgs/linkshort/repos", mustJSON(map[string]any{"name": "linkshort", "private": true,
		"auto_init": true, "default_branch": "main"}), http.StatusCreated))
	up = Repo{Owner: "linkshort", Name: "linkshort"}
	must(adminCall(ctx, http.MethodPut, "/api/v1/repos/linkshort/linkshort/collaborators/"+bot.BotUser,
		mustJSON(map[string]string{"permission": "write"}), http.StatusNoContent))
	// main's protection as production has it: no block on outdated branches
	must(adminCall(ctx, http.MethodPost, "/api/v1/repos/linkshort/linkshort/branch_protections", mustJSON(map[string]any{
		"rule_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{a.User}}), http.StatusCreated))
	write := func(path, text string) error {
		op, blob := "create", ""
		if _, b, err := bot.FileContent(ctx, up.Owner, up.Name, "main", path); err == nil {
			op, blob = "update", b
		}
		_, err := adminCall(ctx, http.MethodPost, "/api/v1/repos/linkshort/linkshort/contents", mustJSON(map[string]any{"branch": "main",
			"message": op + " " + path, "files": []map[string]string{{"operation": op, "path": path, "sha": blob,
				"content": base64.StdEncoding.EncodeToString([]byte(text))}}}), http.StatusCreated)
		return err
	}

	if err := write("pins.txt", "old\nb\nc\nd\ne\nf\n"); err != nil {
		t.Fatal(err)
	}
	rolled, err := bot.BranchHeadSHA(ctx, up.Owner, up.Name, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.EnsureFork(ctx, up); err != nil {
		t.Fatal(err)
	}
	fork := bot.Fork(up)
	edit(write)
	if err := bot.SyncFork(ctx, fork); err != nil {
		t.Fatal(err)
	}
	if head, err := bot.BranchHeadSHA(ctx, up.Owner, up.Name, "main"); err != nil || head == rolled {
		t.Fatalf("main head %s, rolled %s: main must have moved past the rolled commit (%v)", head, rolled, err)
	}

	if err := bot.CreateBranchAt(ctx, fork, "factory/t-roll", rolled); err != nil {
		t.Fatalf("cut at a commit sha: %v", err)
	}
	_, blob, err := bot.FileContent(ctx, fork.Owner, fork.Name, rolled, "pins.txt")
	if err != nil {
		t.Fatal(err)
	}
	commit, err = bot.CommitFiles(ctx, fork.Owner, fork.Name, "factory/t-roll", "", Attribution{Role: "roll", Task: "t-roll"}, "roll",
		[]FileChange{{Path: "pins.txt", Content: []byte("new\nb\nc\nd\ne\nf\n"), Blob: blob}})
	if err != nil {
		t.Fatal(err)
	}
	if parents, err := bot.CommitParents(ctx, fork.Owner, fork.Name, commit); err != nil || !slices.Equal(parents, []string{rolled}) {
		t.Fatalf("the roll commit's parents %v (%v), want the rolled commit %s", parents, err, rolled)
	}
	if pr, err = bot.CreatePullRequest(ctx, up.Owner, up.Name, fork.Owner+":factory/t-roll", "main", "roll", ""); err != nil {
		t.Fatal(err)
	}
	return bot, up, pr, commit
}

// A roll's branch is cut at a commit main has moved past, and its pull
// request merges from behind main: Forgejo 17 cuts a branch at a commit
// sha, and main's branch protection, which does not block outdated
// branches, lets the merge land with both main's newer commit and the pin,
// as long as main's change stays away from the roll's lines.
func TestRollBranchCutBehindMainMergesOnForgejo(t *testing.T) {
	bot, up, pr, commit := rollBranchCase(t, func(write func(path, text string) error) {
		if err := write("other.txt", "a card merged after the rolled commit\n"); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := bot.MergePullRequestAsBot(ctx, up.Owner, up.Name, pr.Number, commit, Attribution{Role: "merge", Task: "t-roll"}, "roll")
		if err == nil {
			break
		}
		if time.Now().After(deadline) || errors.Is(err, ErrRefused) {
			t.Fatalf("merge of a roll whose branch is behind main: %v", err)
		}
		time.Sleep(time.Second)
	}
	for path, want := range map[string]string{"pins.txt": "new\nb\nc\nd\ne\nf\n", "other.txt": "a card merged after the rolled commit\n"} {
		got, _, err := bot.FileContent(ctx, up.Owner, up.Name, "main", path)
		if err != nil || string(got) != want {
			t.Fatalf("main's %s = %q (%v), want %q", path, got, err, want)
		}
	}
}

// A roll cut at a commit that predates a pin change on main (a roll or a
// hand roll merged since) cannot merge: Forgejo reads the pull request as
// not mergeable and refuses the merge. This is why the roller files a run
// only when its commit holds main's pins.
func TestRollBranchCutBehindMainsPinChangeIsNotMergeableOnForgejo(t *testing.T) {
	bot, up, pr, commit := rollBranchCase(t, func(write func(path, text string) error) {
		if err := write("pins.txt", "hand-rolled\nb\nc\nd\ne\nf\n"); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	time.Sleep(15 * time.Second)
	st, err := bot.PullRequestState(ctx, up.Owner, up.Name, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mergeable {
		t.Fatalf("pull request %d reads mergeable after main changed the roll's pin line", pr.Number)
	}
	if err := bot.MergePullRequestAsBot(ctx, up.Owner, up.Name, pr.Number, commit, Attribution{Role: "merge", Task: "t-roll"}, "roll"); err == nil {
		t.Fatal("the merge of a roll behind main's pin change landed")
	}
}
