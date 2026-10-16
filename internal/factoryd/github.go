package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// GitHub is GitHub as the exports reach it: API its REST API, Git the base
// of its repositories' git URLs, Forgejo the base of the projects' git URLs
// inside the cluster. Each call takes its token; none is kept.
type GitHub struct {
	API, Git, Forgejo string
	HTTP              *http.Client
}

// GitHubError is GitHub's refusal of an API call.
type GitHubError struct {
	Status  int
	Message string
}

func (e *GitHubError) Error() string {
	return fmt.Sprintf("GitHub answered %d %s", e.Status, e.Message)
}

func (g *GitHub) call(ctx context.Context, token, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(mustJSON(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, g.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := cmp.Or(g.HTTP, &http.Client{Timeout: 30 * time.Second})
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		e := &GitHubError{Status: res.StatusCode}
		json.Unmarshal(b, e) // #nosec G104 -- an answer without a message keeps its status
		return e
	}
	return json.Unmarshal(b, out)
}

// githubRepo is a GitHub repository as the exports read it: its default
// branch and its size in KiB.
type githubRepo struct {
	DefaultBranch string `json:"default_branch"`
	KiB           int64  `json:"size"`
}

// repo reads remote with token.
func (g *GitHub) repo(ctx context.Context, token, remote string) (githubRepo, error) {
	var repo githubRepo
	err := g.call(ctx, token, http.MethodGet, "/repos/"+remote, nil, &repo)
	return repo, err
}

// openPull opens a pull request of branch head into base of remote.
func (g *GitHub) openPull(ctx context.Context, token, remote, head, base, title, body string) (int64, string, error) {
	var pr struct {
		Number int64  `json:"number"`
		URL    string `json:"html_url"`
	}
	err := g.call(ctx, token, http.MethodPost, "/repos/"+remote+"/pulls",
		map[string]string{"title": title, "head": head, "base": base, "body": body}, &pr)
	return pr.Number, pr.URL, err
}

// pullState is remote's pull request n as a card shows it: open, merged
// or closed.
func (g *GitHub) pullState(ctx context.Context, token, remote string, n int64) (string, error) {
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := g.call(ctx, token, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", remote, n), nil, &pr); err != nil {
		return "", err
	}
	if pr.Merged {
		return "merged", nil
	}
	return pr.State, nil
}

// gitConfig bounds every git call: a transfer under 1000 bytes a second
// for a minute fails, and packing keeps to one thread and small windows,
// as factoryd's memory limit holds git too.
var gitConfig = []string{"http.lowSpeedLimit=1000", "http.lowSpeedTime=60", "pack.threads=1", "pack.windowMemory=32m",
	"core.packedGitLimit=64m", "core.deltaBaseCacheLimit=32m"}

// git runs git in dir with gitConfig and login, the basic login of the
// call's one remote, as an http.extraHeader, all from the environment, so
// no argument or error holds the token, and with none of factoryd's own
// environment. At its deadline, at most 2 minutes, its whole process group
// is killed: git's remote helper holds the output pipe.
func git(ctx context.Context, dir, login string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- no shell; the arguments are refs, SHAs and URLs factoryd builds from checked names
	cmd.Dir = dir
	config := gitConfig
	if login != "" {
		config = append(slices.Clip(config), "http.extraHeader=Authorization: Basic "+login)
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(config))}
	for i, c := range config {
		key, value, _ := strings.Cut(c, "=")
		cmd.Env = append(cmd.Env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, value))
	}
	ownProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, textutil.Truncate(strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", "; "), 400))
	}
	return strings.TrimSpace(string(out)), nil
}

func basicLogin(user, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
}

// bareRepo is an empty bare repository in a new temporary directory, which
// the caller removes.
func bareRepo(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "github-")
	if err != nil {
		return "", err
	}
	_, err = git(ctx, dir, "", "init", "-q", "--bare")
	return dir, err
}

const (
	// githubStates is how often at most the pass reads the open GitHub
	// pull requests' states.
	githubStates = 5 * time.Minute
	// githubPassTime bounds a pass, which runs in the registrar's loop.
	githubPassTime = 4 * time.Minute
	// githubSyncTime bounds a GitHubSync, which runs beside the loop.
	githubSyncTime = 5 * time.Minute
	// githubCapKiB is the largest project repository, in Forgejo's KiB,
	// that git fetches for GitHub.
	githubCapKiB = 200 << 10
)

// githubPass takes each card of an export of mode pull_requests that
// merged since the export was put to GitHub, oldest first; at most every
// githubStates it reads the state of each open GitHub pull request, one
// call each, and drops each export's finished sync branches. The export's state counts the open ones. A card's refusal is
// its state, which no pass retries; an error is logged and the next pass
// tries again.
func (r *Registrar) githubPass(ctx context.Context) {
	if r.GitHub == nil || r.Mirrors == nil {
		return
	}
	refresh := time.Since(r.githubAt) >= githubStates
	if refresh {
		r.githubAt = time.Now()
	}
	ctx, cancel := context.WithTimeout(ctx, githubPassTime)
	defer cancel()
	ps, err := r.Store.Projects(ctx)
	if err != nil {
		log.Printf("projects: github pull requests: %v", err)
		return
	}
	for _, p := range ps {
		m, ok, err := r.Mirrors.MirrorToken(ctx, p.Name, MirrorExport)
		if err == nil && !ok {
			continue
		}
		if err == nil && refresh {
			err = r.dropSyncBranches(ctx, p)
		}
		if err == nil && m.Mode == ModePullRequests {
			err = r.githubPulls(ctx, p, m, refresh)
		}
		if err != nil {
			log.Printf("project %s: github pull requests: %v", p.Name, err)
		}
	}
}

// dropSyncBranches deletes the github/sync-* branches of p's fork whose
// pull request is no longer open, unless a git operation runs: a sync's
// push and pull request come under the same lock.
func (r *Registrar) dropSyncBranches(ctx context.Context, p Project) error {
	if !r.gitMu.TryLock() {
		return nil
	}
	defer r.gitMu.Unlock()
	repo := p.Repo()
	fork := r.Forgejo.Fork(repo)
	branches, err := GetPages[struct{ Name string }](ctx, r.Forgejo.call, "/api/v1/repos/"+fork.String()+"/branches?")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	var open []wirePR
	if err == nil {
		open, err = GetPages[wirePR](ctx, r.Forgejo.call, "/api/v1/repos/"+repo.String()+"/pulls?state=open&")
	}
	for _, b := range branches {
		if err == nil && strings.HasPrefix(b.Name, "github/sync-") && !slices.ContainsFunc(open, func(pr wirePR) bool {
			return pr.Head.Ref == b.Name && pr.Head.Repo.FullName == fork.String()
		}) {
			err = r.Forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, b.Name)
		}
	}
	return err
}

func (r *Registrar) githubPulls(ctx context.Context, p Project, m Mirror, refresh bool) error {
	cards, err := r.Mirrors.GitHubCards(ctx, p.Repository, m.Since)
	if err != nil {
		return err
	}
	open := 0
	for _, t := range cards {
		n, url, state := t.GitHubPR, t.GitHubPRURL, t.GitHubPRState
		var cerr error
		switch {
		case state == "":
			n, url, state, cerr = r.exportCard(ctx, p, m, t)
		case refresh:
			state, cerr = r.GitHub.pullState(ctx, m.Token, m.Remote, n)
		}
		switch {
		case cerr != nil:
			err, state = errors.Join(err, fmt.Errorf("card %s: %w", t.ID, cerr)), t.GitHubPRState
		case state != t.GitHubPRState:
			err = errors.Join(err, r.Mirrors.SetGitHubPR(ctx, t.ID, n, url, state))
		}
		if state == "open" {
			open++
		}
	}
	words := fmt.Sprintf("%d open pull requests", open)
	if open == 1 {
		words = "1 open pull request"
	}
	err = errors.Join(err, r.Mirrors.SetMirrorState(ctx, p.Name, MirrorExport, words, "", time.Now()))
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), m.Token, "[token]"))
	}
	return nil
}

// exportCard pushes card t's head as merged, Forgejo's refs/pull/<n>/head,
// to m's GitHub repository as the card's branch and opens a pull request
// of it there, unless another git operation runs. A definite refusal (a
// GitHub 4xx, from the first read of the repository on, a project
// repository over githubCapKiB, a push GitHub refuses)
// answers the card's failed state; any other failure is an error the next
// pass retries.
func (r *Registrar) exportCard(ctx context.Context, p Project, m Mirror, t Task) (int64, string, string, error) {
	if !r.gitMu.TryLock() {
		return 0, "", "", errors.New("another git operation runs")
	}
	defer r.gitMu.Unlock()
	gh, err := r.GitHub.repo(ctx, m.Token, m.Remote)
	if refused := (*GitHubError)(nil); errors.As(err, &refused) && refused.Status < 500 {
		return 0, "", fmt.Sprintf("failed: GitHub refused the export's repository %s: %v", m.Remote, err), nil
	}
	if err != nil {
		return 0, "", "", err
	}
	if over, err := r.overCap(ctx, p, "to take a card to GitHub"); over != "" || err != nil {
		return 0, "", over, err
	}
	dir, err := bareRepo(ctx)
	defer os.RemoveAll(dir) // #nosec G104 -- a temporary directory
	branch := taskBranch(t)
	if err == nil {
		_, err = git(ctx, dir, basicLogin(r.ProjectsUser, r.Forgejo.ProjectsToken), "fetch", "-q",
			r.GitHub.Forgejo+"/"+p.Repository+".git", fmt.Sprintf("refs/pull/%d/head:refs/heads/%s", t.PRNumber, branch))
	}
	if err == nil {
		_, err = git(ctx, dir, basicLogin(m.Username, m.Token), "push", "-q", r.GitHub.Git+"/"+m.Remote+".git", "refs/heads/"+branch)
		if msg := fmt.Sprint(err); err != nil && (strings.Contains(msg, "returned error: 4") || strings.Contains(msg, "rejected")) {
			return 0, "", "failed: the push of " + branch + " to GitHub was refused: " + strings.ReplaceAll(msg, m.Token, "[token]"), nil
		}
	}
	if err != nil {
		return 0, "", "", err
	}
	body := fmt.Sprintf("%s\n\nMerged in the AI software factory as %s/%s/pulls/%d (task %s).",
		t.Objective, r.Forgejo.PublicURL, p.Repository, t.PRNumber, t.ID)
	n, url, err := r.GitHub.openPull(ctx, m.Token, m.Remote, branch, gh.DefaultBranch, FirstLine(t.Objective), body)
	var refused *GitHubError
	switch {
	case errors.As(err, &refused) && refused.Status == http.StatusForbidden && strings.HasPrefix(refused.Message, "Resource not accessible"):
		return 0, "", "failed: " + branch + ` is on GitHub, but the export's token may not open pull requests: it needs the permission "Pull requests: read and write"`, nil
	case errors.As(err, &refused) && refused.Status < 500:
		return 0, "", "failed: GitHub refused the pull request: " + refused.Error(), nil
	case err != nil:
		return 0, "", "", err
	}
	return n, url, "open", nil
}

// overCap answers, for project p's repository over githubCapKiB, the
// plain line refusing to fetch it for why.
func (r *Registrar) overCap(ctx context.Context, p Project, why string) (string, error) {
	kib, err := r.Forgejo.ProjectRepoSize(ctx, p.Repo())
	if err != nil || kib <= githubCapKiB {
		return "", err
	}
	return fmt.Sprintf("failed: %s is %d MiB, over the %d MiB the factory fetches %s", p.Repository, kib>>10, githubCapKiB>>10, why), nil
}

// ErrNoExport is a project without a GitHub export.
var ErrNoExport = errors.New("the project has no GitHub export")

// GitHubSync brings project name's GitHub default branch back when it
// moved since the last sync and main lacks it: pushed to the bot's fork as
// github/sync-<short sha> with a pull request to main as the bot, so its
// CI runs as a fork's, without the repository's secrets, and an approver
// merges it like any other. A head that changes .forgejo/, .gitea/ or
// .github/ is refused, as a card's is (rulings 90, 100), and so is one
// main shares no history with. It answers what it did, which the export
// row keeps as its last sync, or why it failed, which the row keeps too.
func (r *Registrar) GitHubSync(ctx context.Context, name, by string) (string, error) {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	run, cancel := context.WithTimeout(ctx, githubSyncTime)
	defer cancel()
	m, ok, err := r.Mirrors.MirrorToken(run, name, MirrorExport)
	if err != nil || !ok {
		return "", cmp.Or(err, ErrNoExport)
	}
	sha, note, err := r.githubSync(run, name, m, by)
	result := note
	if err != nil {
		err = errors.New(strings.ReplaceAll(err.Error(), m.Token, "[token]"))
		sha, result = m.Synced, "failed: "+err.Error()
	}
	write, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	return note, errors.Join(err, r.Mirrors.SetMirrorSynced(write, name, sha, result))
}

func (r *Registrar) githubSync(ctx context.Context, name string, m Mirror, by string) (sha, note string, err error) {
	p, ok, err := r.Store.Project(ctx, name)
	if err != nil || !ok {
		return "", "", cmp.Or(err, ErrNoExport)
	}
	if over, err := r.overCap(ctx, p, "to sync from GitHub"); over != "" || err != nil {
		return "", "", cmp.Or(err, errors.New(strings.TrimPrefix(over, "failed: ")))
	}
	gh, err := r.GitHub.repo(ctx, m.Token, m.Remote)
	switch {
	case err != nil:
		return "", "", err
	case gh.KiB > githubCapKiB:
		return "", "", fmt.Errorf("%s is %d MiB on GitHub, over the %d MiB the factory fetches to sync from GitHub", m.Remote, gh.KiB>>10, githubCapKiB>>10)
	}
	dir, err := bareRepo(ctx)
	defer os.RemoveAll(dir) // #nosec G104 -- a temporary directory
	if err == nil {
		_, err = git(ctx, dir, basicLogin(m.Username, m.Token), "fetch", "-q", r.GitHub.Git+"/"+m.Remote+".git", "HEAD:refs/heads/github")
	}
	if err == nil {
		sha, err = git(ctx, dir, "", "rev-parse", "refs/heads/github")
	}
	if err == nil && sha != m.Synced {
		_, err = git(ctx, dir, basicLogin(r.ProjectsUser, r.Forgejo.ProjectsToken), "fetch", "-q", r.GitHub.Forgejo+"/"+p.Repository+".git",
			prBase+":refs/heads/"+prBase)
	}
	if err != nil {
		return "", "", err
	}
	short, of := sha[:7], "GitHub's default branch of "+m.Remote
	if sha == m.Synced {
		return sha, of + " is still at " + short + ", which the last sync brought back: nothing to sync.", nil
	}
	base, err := git(ctx, dir, "", "merge-base", "refs/heads/"+prBase, "refs/heads/github")
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return "", "", fmt.Errorf("%s at %s shares no history with main: nothing synced", of, short)
	case err != nil:
		return "", "", err
	case base == sha:
		return sha, "main holds " + of + " at " + short + ": nothing to sync.", nil
	}
	if _, err := git(ctx, dir, "", "diff", "--quiet", base, "refs/heads/github", "--", ".forgejo", ".gitea", ".github"); err != nil {
		return "", "", fmt.Errorf("%s at %s changes .forgejo/, .gitea/ or .github/, which no pull request of the factory's may change (rulings 90, 100): bring it back by hand", of, short)
	}
	repo, branch := p.Repo(), "github/sync-"+short
	fork := r.Forgejo.Fork(repo)
	if err := r.Forgejo.EnsureFork(ctx, repo); err != nil {
		return "", "", err
	}
	if _, err := git(ctx, dir, basicLogin(r.Forgejo.BotUser, r.Forgejo.BotPassword), "push", "-q", r.GitHub.Forgejo+"/"+fork.String()+".git",
		"refs/heads/github:refs/heads/"+branch); err != nil {
		return "", "", err
	}
	pr, err := r.Forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, "Sync from GitHub "+short,
		fmt.Sprintf("%s at %s, brought back for %s.", of, sha, by))
	if err != nil {
		return "", "", err
	}
	// The pull request stands on the bot's fork: its card is filed there,
	// where the workflow reads and merges it (roadmap 187).
	r.fileSyncCard(ctx, p, fork, pr.Number, short, sha, by)
	return sha, fmt.Sprintf("Pull request #%d brings %s at %s to main: %s", pr.Number, of, short, pr.URL), nil
}

// fileSyncCard files the card of the sync pull request pr (roadmap 187):
// an issue of its own through FileFactoryPRTask, authored by requester by,
// whose merge merges that pull request, so the chat, the board and the
// merge rule cover it as they cover every card. A failure is logged, not
// returned: the pull request stands open either way, and the next sync of
// a newer SHA opens another card. Idempotent by repository and pull request,
// so a sync whose pull request already has a card files none.
func (r *Registrar) fileSyncCard(ctx context.Context, p Project, fork Repo, pr int64, short, sha, by string) {
	if r.Tasks == nil || pr == 0 {
		return
	}
	if _, ok, err := r.Tasks.TaskByPR(ctx, p.Repo().String(), pr); err != nil {
		log.Printf("project %s: the card of the sync pull request %d: %v", p.Name, pr, err)
		return
	} else if ok {
		return // this pull request's card exists: nothing to file
	}
	sum := sha256.Sum256([]byte("sync " + p.Repository + " " + strconv.FormatInt(pr, 10)))
	id := "t-" + hex.EncodeToString(sum[:6])
	if r.Workflows == nil {
		return
	}
	title := "Sync from GitHub " + short
	body := fmt.Sprintf("%s's default branch at %s, brought back for %s as pull request #%d; merging this card merges that pull request.",
		p.Repository, sha, by, pr)
	if _, ref, err := FileFactoryPRTask(ctx, r.Tasks, r.Forgejo, r.Workflows, r.Timeline, r.Policy().Document.Budget, p, title, body, nil,
		Task{ID: id, Kind: "agent", Objective: title + "\n\n" + body, Author: by, Source: SourceGitHubSync}, pr); err != nil {
		log.Printf("project %s: the card of the sync pull request %d: %v", p.Name, pr, err)
	} else {
		log.Printf("project %s: sync pull request %d filed as issue #%d", p.Name, pr, ref.Number)
	}
}
