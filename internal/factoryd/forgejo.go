package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Repo names one repository a per-task Forgejo token is scoped to.
type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Forgejo mints and revokes the per-task git tokens (architecture 6.3,
// research Q3), authenticating as the bot user with basic auth — the live
// wire (Forgejo 15 / gitea 1.22): /users/{u}/tokens refuses token auth for
// minting ("auth method not allowed") and the /admin/users mint path does
// not exist. Each task gets a repo-scoped pair, deleted at task end:
// read:repository on the task's repository (the clone) and
// write:repository on the bot's fork (the branch push), so a leaked token
// can at most push the bot's own fork, for exactly one task.
type Forgejo struct {
	BaseURL     string // API calls: the in-cluster service
	PublicURL   string // links meant for people, e.g. https://git.aisoftwarefactory.world
	BotUser     string
	BotPassword string
	HTTP        *http.Client // nil = a mint-sized timeout

	// CIRegistrarToken authenticates the Actions runner-registration API
	// and the collaborator permission lookups (task 41, rulings 78, 101):
	// a repo-scoped token of the dedicated user factory-ci, an admin
	// collaborator on the code repository only (runner registration and
	// permission reads need repository admin), kept out of main's push and
	// merge whitelist with apply_to_admins on, so the token can neither
	// push nor merge main. Never a registry credential, never mounted into
	// a runner pod.
	CIRegistrarToken string
}

// ForgejoToken is a minted token. Secret is returned once by Forgejo and
// never persisted by the factory beyond the task secret.
type ForgejoToken struct {
	ID     int64
	Secret string
}

// MintToken mints a bot token limited to one scope ("read:repository" or
// "write:repository") on exactly the given repositories.
func (f *Forgejo) MintToken(ctx context.Context, name, scope string, repos []Repo) (ForgejoToken, error) {
	var tok ForgejoToken
	if len(repos) == 0 {
		return tok, fmt.Errorf("forgejo mint %s: at least one repository is required", name)
	}
	payload, err := json.Marshal(map[string]any{
		"name":         name,
		"scopes":       []string{scope},
		"repositories": repos,
	})
	if err != nil {
		return tok, err
	}
	body, err := f.call(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%s/tokens", f.BotUser), payload, http.StatusCreated)
	if err != nil {
		return tok, err
	}
	var res struct {
		ID   int64  `json:"id"`
		SHA1 string `json:"sha1"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return tok, fmt.Errorf("forgejo mint %s: %w", name, err)
	}
	if res.ID == 0 || res.SHA1 == "" {
		return tok, fmt.Errorf("forgejo mint %s: response carries no token secret", name)
	}
	return ForgejoToken{ID: res.ID, Secret: res.SHA1}, nil
}

// TokenRef is a listed token: enough to find a task's tokens by name and
// revoke them, without ever seeing the secret again.
type TokenRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ListTokens lists the bot user's tokens. The runner uses it to reconcile
// tokens by name: completion and the boot sweep must revoke a task's token
// even when the mint's ID was lost to a crash.
func (f *Forgejo) ListTokens(ctx context.Context) ([]TokenRef, error) {
	const limit = 50
	var refs []TokenRef
	for page := 1; ; page++ {
		body, err := f.call(ctx, http.MethodGet,
			fmt.Sprintf("/api/v1/users/%s/tokens?page=%d&limit=%d", f.BotUser, page, limit), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var res []TokenRef
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("forgejo list tokens: %w", err)
		}
		refs = append(refs, res...)
		if len(res) < limit {
			return refs, nil
		}
	}
}

// RevokeToken deletes a token. A 404 means it is already gone — a revoke
// racing another revoke (or a completion retry) has nothing left to do.
func (f *Forgejo) RevokeToken(ctx context.Context, id int64) error {
	_, err := f.call(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v1/users/%s/tokens/%d", f.BotUser, id), nil, http.StatusNoContent)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (f *Forgejo) call(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.SetBasicAuth(f.BotUser, f.BotPassword)
	})
}

// ciCall authenticates with the runner-registrar token instead of the bot
// user's basic auth (task 41): the CI Actions API takes a bearer-style
// `token <T>` header, not basic auth, and the registrar credential carries
// none of the bot user's repository-write scope.
func (f *Forgejo) ciCall(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.Header.Set("Authorization", "token "+f.CIRegistrarToken)
	})
}

func (f *Forgejo) do(ctx context.Context, method, path string, payload []byte, wantStatus int, auth func(*http.Request)) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	auth(req)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forgejo %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("forgejo %s %s: %w", method, path, err)
	}
	if res.StatusCode != wantStatus {
		if res.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("forgejo %s %s: %w", method, path, ErrNotFound)
		}
		return nil, fmt.Errorf("forgejo %s %s: status %d: %s", method, path, res.StatusCode, body)
	}
	return body, nil
}

// RepoPermission is user's effective permission on the repository ("owner",
// "admin", "write", "read"; "none" when Forgejo knows no access). Reading
// another user's permission needs repository admin, which the bot lacks:
// it runs on the registrar token (factory-ci, a repository admin that
// cannot push or merge main, ruling 101).
func (f *Forgejo) RepoPermission(ctx context.Context, owner, repo, user string) (string, error) {
	body, err := f.ciCall(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/collaborators/%s/permission", owner, repo, url.PathEscape(user)), nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		// Forgejo answers 404 for an unknown user and for an unknown (or
		// invisible) repository alike: only the first means "no access"
		if _, rerr := f.ciCall(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s", owner, repo), nil, http.StatusOK); rerr != nil {
			return "", fmt.Errorf("forgejo permission of %s: repository %s/%s: %w", user, owner, repo, rerr)
		}
		return "none", nil
	}
	if err != nil {
		return "", err
	}
	var res struct {
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("forgejo permission of %s on %s/%s: %w", user, owner, repo, err)
	}
	return res.Permission, nil
}

// CIJob is one Forgejo Actions job waiting for a runner matching its labels
// (task 41: the `pr-untrusted` label routes pull-request jobs to the
// ephemeral gVisor runner instead of the privileged dind runner).
type CIJob struct {
	ID     int64    `json:"id"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"`
}

// WaitingRunJobs lists owner/repo's queued Actions jobs carrying label. The
// search endpoint has no status filter, so this filters client-side to jobs
// Forgejo itself marked "waiting" — running or already-done jobs matching
// the label must not spawn a runner. The spawner names one namespace per
// job id (its idempotency key), but the runner it starts registers by
// label only and the server, not the caller, assigns it whichever waiting
// job it serves.
func (f *Forgejo) WaitingRunJobs(ctx context.Context, owner, repo, label string) ([]CIJob, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners/jobs?labels=%s", owner, repo, url.QueryEscape(label))
	body, err := f.ciCall(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var all []CIJob
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("forgejo waiting jobs: %w", err)
	}
	jobs := make([]CIJob, 0, len(all))
	for _, j := range all {
		if j.Status == "waiting" {
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}

// CIRunnerRegistration is a freshly registered ephemeral runner. Secret is
// returned once by Forgejo and never persisted beyond the pod's own runner
// registration secret.
type CIRunnerRegistration struct {
	ID     int64
	UUID   string
	Secret string
}

// RegisterCIRunner registers one ephemeral, single-job runner (task 41,
// ruling 78: "an ephemeral runner pod... that takes one job and exits").
// The registration API has no labels field — forgejo-runner declares its
// own labels when it connects with this token — so the caller configures
// the runner process itself with the label it must serve. Forgejo's
// scheduler, not this call, then decides which waiting job the runner
// instance actually serves.
func (f *Forgejo) RegisterCIRunner(ctx context.Context, owner, repo, name string) (CIRunnerRegistration, error) {
	var reg CIRunnerRegistration
	payload, err := json.Marshal(map[string]any{
		"name":      name,
		"ephemeral": true,
	})
	if err != nil {
		return reg, err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners", owner, repo)
	body, err := f.ciCall(ctx, http.MethodPost, path, payload, http.StatusCreated)
	if err != nil {
		return reg, err
	}
	var res struct {
		ID    int64  `json:"id"`
		UUID  string `json:"uuid"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return reg, fmt.Errorf("forgejo register ci runner %s: %w", name, err)
	}
	if res.ID == 0 || res.UUID == "" || res.Token == "" {
		return reg, fmt.Errorf("forgejo register ci runner %s: response carries no registration", name)
	}
	return CIRunnerRegistration{ID: res.ID, UUID: res.UUID, Secret: res.Token}, nil
}

// DeleteCIRunner removes a runner registration. A 404 means it is already
// gone — an ephemeral runner deregisters itself on exit, so cleanup racing
// that self-deregistration has nothing left to do.
func (f *Forgejo) DeleteCIRunner(ctx context.Context, owner, repo string, id int64) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners/%d", owner, repo, id)
	_, err := f.ciCall(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// CIRunnerRef is one currently registered runner, enough to recover the job
// id spawn named it for and to delete it.
type CIRunnerRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ListCIRunners lists owner/repo's currently registered runners. It is
// Forgejo's own durable record of a registration: the reconciler's last
// resort for a runner spawn registered but crashed before writing anything
// local to recover it from.
func (f *Forgejo) ListCIRunners(ctx context.Context, owner, repo string) ([]CIRunnerRef, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners", owner, repo)
	body, err := f.ciCall(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var refs []CIRunnerRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, fmt.Errorf("forgejo list ci runners: %w", err)
	}
	return refs, nil
}

// IssueRef is a freshly filed issue: enough to store the link and report it
// back to a POST /tasks caller.
type IssueRef struct {
	Number int64
	URL    string
}

// CreateIssue files a new Forgejo issue (task 42: every task gets one, so
// the factory's task list stays visible and actionable from the repo itself).
func (f *Forgejo) CreateIssue(ctx context.Context, owner, repo, title, body string) (IssueRef, error) {
	payload, err := json.Marshal(map[string]any{"title": title, "body": body})
	if err != nil {
		return IssueRef{}, err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues", owner, repo)
	resBody, err := f.call(ctx, http.MethodPost, path, payload, http.StatusCreated)
	if err != nil {
		return IssueRef{}, err
	}
	var res struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(resBody, &res); err != nil {
		return IssueRef{}, fmt.Errorf("forgejo create issue %s/%s: %w", owner, repo, err)
	}
	if res.Number == 0 {
		return IssueRef{}, fmt.Errorf("forgejo create issue %s/%s: response carries no issue number", owner, repo)
	}
	return IssueRef{Number: res.Number, URL: res.HTMLURL}, nil
}

// RepoLabel is one label defined on a repository.
type RepoLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ListLabels lists owner/repo's currently defined labels.
func (f *Forgejo) ListLabels(ctx context.Context, owner, repo string) ([]RepoLabel, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/labels", owner, repo)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var labels []RepoLabel
	if err := json.Unmarshal(body, &labels); err != nil {
		return nil, fmt.Errorf("forgejo list labels %s/%s: %w", owner, repo, err)
	}
	return labels, nil
}

// CreateLabel defines a new repository label. color is a hex RGB string
// without a leading '#' ambiguity issue: Forgejo accepts "#RRGGBB".
func (f *Forgejo) CreateLabel(ctx context.Context, owner, repo, name, color string) error {
	payload, err := json.Marshal(map[string]any{"name": name, "color": color})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/labels", owner, repo)
	_, err = f.call(ctx, http.MethodPost, path, payload, http.StatusCreated)
	return err
}

// EnsureLabels creates whichever of wanted (name -> color) does not already
// exist on owner/repo. It must run at boot, before anything files an issue
// under one of these names or mirrors state onto one: Forgejo's issue-label
// endpoints resolve labels by name and silently drop any name that doesn't
// exist yet, so a label first used mid-mirror would vanish instead of
// applying.
func (f *Forgejo) EnsureLabels(ctx context.Context, owner, repo string, wanted map[string]string) error {
	existing, err := f.ListLabels(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("forgejo ensure labels %s/%s: %w", owner, repo, err)
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[l.Name] = true
	}
	var errs []error
	for name, color := range wanted {
		if have[name] {
			continue
		}
		if err := f.CreateLabel(ctx, owner, repo, name, color); err != nil {
			errs = append(errs, fmt.Errorf("label %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// IssueLabels lists the names of the labels currently on issue number.
func (f *Forgejo) IssueLabels(ctx context.Context, owner, repo string, issueNumber int64) ([]string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/labels", owner, repo, issueNumber)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var labels []RepoLabel
	if err := json.Unmarshal(body, &labels); err != nil {
		return nil, fmt.Errorf("forgejo issue labels %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names, nil
}

// ReplaceIssueLabels sets issue number's full label set to exactly names.
func (f *Forgejo) ReplaceIssueLabels(ctx context.Context, owner, repo string, issueNumber int64, names []string) error {
	payload, err := json.Marshal(map[string]any{"labels": names})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/labels", owner, repo, issueNumber)
	_, err = f.call(ctx, http.MethodPut, path, payload, http.StatusOK)
	return err
}

// SetStateLabel swaps issue number's state onto exactly one state/* label,
// leaving every other label untouched. This is the one mirror primitive:
// task 42 calls it for agent-run tasks, and task 43's workflow can call it
// directly with any state string for the human lane.
func (f *Forgejo) SetStateLabel(ctx context.Context, owner, repo string, issueNumber int64, state string) error {
	current, err := f.IssueLabels(ctx, owner, repo, issueNumber)
	if err != nil {
		return fmt.Errorf("forgejo set state label %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	next := make([]string, 0, len(current)+1)
	for _, name := range current {
		if strings.HasPrefix(name, "state/") {
			continue
		}
		next = append(next, name)
	}
	next = append(next, "state/"+state)
	return f.ReplaceIssueLabels(ctx, owner, repo, issueNumber, next)
}

// mirrorTaskState swaps a task's linked Forgejo issue onto exactly one
// state/* label. A task with no linked issue (IssueNumber == 0) has nothing
// to mirror. Mirroring failures are logged, never returned: a Forgejo
// hiccup must never fail the caller's own transition. The one helper behind
// every mirror call site: the runner (state/running), completion
// (state/done, state/failed, ...), and the webhook intake (state/queued,
// state/waiting-human).
func mirrorTaskState(ctx context.Context, forgejo *Forgejo, t Task, state string) {
	if t.IssueNumber == 0 {
		return
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		log.Printf("task %s: mirror state %s: %v", t.ID, state, err)
		return
	}
	if err := forgejo.SetStateLabel(ctx, repo.Owner, repo.Name, t.IssueNumber, state); err != nil {
		log.Printf("task %s: mirror state %s: %v", t.ID, state, err)
	}
}

// Fork is the bot's private fork of upstream: same name under the bot user.
// Agent branches live there, so a pull request's job token is read-only
// (ruling 97).
func (f *Forgejo) Fork(upstream Repo) Repo {
	return Repo{Owner: f.BotUser, Name: upstream.Name}
}

// EnsureFork creates the bot's fork of upstream if it is missing, switches
// the fork's Actions unit off (a session's write token must not be able to
// run workflows there by pushing the fork's main), and refuses a repository
// under that name that is not a private fork of upstream with Actions off.
func (f *Forgejo) EnsureFork(ctx context.Context, upstream Repo) error {
	fork := f.Fork(upstream)
	path := fmt.Sprintf("/api/v1/repos/%s/%s", fork.Owner, fork.Name)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		payload, _ := json.Marshal(map[string]string{"name": fork.Name})
		body, err = f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/forks", upstream.Owner, upstream.Name), payload, http.StatusAccepted)
	}
	if err != nil {
		return err
	}
	var res struct {
		IsFork  bool `json:"fork"`
		Private bool `json:"private"`
		Actions bool `json:"has_actions"`
		Parent  struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("forgejo fork %s: %w", fork, err)
	}
	if res.Actions {
		payload, _ := json.Marshal(map[string]bool{"has_actions": false})
		if body, err = f.call(ctx, http.MethodPatch, path, payload, http.StatusOK); err != nil {
			return err
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return fmt.Errorf("forgejo fork %s: %w", fork, err)
		}
	}
	if !res.IsFork || res.Parent.FullName != upstream.String() || !res.Private || res.Actions {
		return fmt.Errorf("forgejo fork %s: not a private fork of %s with Actions off", fork, upstream)
	}
	return nil
}

// DeleteBranch deletes a branch. A 404 means it is already gone: a retried
// Finish has nothing left to do.
func (f *Forgejo) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(branch))
	_, err := f.call(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// BranchHeadSHA reads a branch's current head commit SHA: intake uses it to
// stamp a new task's base SHA to the default branch's head at intake time.
func (f *Forgejo) BranchHeadSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(branch))
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var res struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("forgejo branch head %s/%s/%s: %w", owner, repo, branch, err)
	}
	return res.Commit.ID, nil
}

// PullRequestRef is an opened or found pull request: enough for the
// workflow to record the number and URL, poll its head commit, and tell
// open, closed and merged apart.
type PullRequestRef struct {
	Number   int64
	URL      string
	Head     string
	HeadRepo string // "owner/name" of the repository holding Head
	HeadSHA  string
	State    string // "open" or "closed"
	Merged   bool
}

// wirePR is Forgejo's pull request JSON, as far as the factory reads it.
type wirePR struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (w wirePR) ref() PullRequestRef {
	return PullRequestRef{Number: w.Number, URL: w.HTMLURL, Head: w.Head.Ref, HeadRepo: w.Head.Repo.FullName, HeadSHA: w.Head.SHA, State: w.State, Merged: w.Merged}
}

// CreatePullRequest opens a pull request from head into base. The workflow
// calls FindOpenPullRequestForBranch first so a retried OpenPullRequest
// activity reuses the existing PR instead of opening a second one for the
// same branch.
func (f *Forgejo) CreatePullRequest(ctx context.Context, owner, repo, head, base, title, body string) (PullRequestRef, error) {
	payload, err := json.Marshal(map[string]any{
		"head":  head,
		"base":  base,
		"title": title,
		"body":  body,
	})
	if err != nil {
		return PullRequestRef{}, err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls", owner, repo)
	resBody, err := f.call(ctx, http.MethodPost, path, payload, http.StatusCreated)
	if err != nil {
		return PullRequestRef{}, err
	}
	var res wirePR
	if err := json.Unmarshal(resBody, &res); err != nil {
		return PullRequestRef{}, fmt.Errorf("forgejo create pull request %s/%s %s: %w", owner, repo, head, err)
	}
	if res.Number == 0 {
		return PullRequestRef{}, fmt.Errorf("forgejo create pull request %s/%s %s: response carries no pull number", owner, repo, head)
	}
	return res.ref(), nil
}

// FindOpenPullRequestForBranch looks for an already-open pull request whose
// head is branch in headRepo (the idempotency key OpenPullRequest reuses
// across a retried or replayed attempt). A nil ref with a nil error means no
// open PR exists yet.
func (f *Forgejo) FindOpenPullRequestForBranch(ctx context.Context, owner, repo string, headRepo Repo, branch string) (*PullRequestRef, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=open&limit=50", owner, repo)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var all []wirePR
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("forgejo find open pull request %s/%s %s: %w", owner, repo, branch, err)
	}
	for _, pr := range all {
		if pr.Head.Ref == branch && pr.Head.Repo.FullName == headRepo.String() {
			ref := pr.ref()
			return &ref, nil
		}
	}
	return nil, nil
}

// CommitStatus is one check reported against a commit (most often a
// Forgejo Actions job): State is one of "pending", "success", "error",
// "failure" or "warning".
type CommitStatus struct {
	State       string `json:"status"`
	Context     string `json:"context"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

// CombinedStatus is a commit's aggregate state across every reported check.
type CombinedStatus struct {
	State    string
	Statuses []CommitStatus
}

// CombinedStatus reads sha's combined commit status: WaitChecks polls this
// (architecture 7: Forgejo Actions is the first verification gate).
func (f *Forgejo) CombinedStatus(ctx context.Context, owner, repo, sha string) (CombinedStatus, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/commits/%s/status", owner, repo, sha)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return CombinedStatus{}, err
	}
	var res struct {
		State    string         `json:"state"`
		Statuses []CommitStatus `json:"statuses"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return CombinedStatus{}, fmt.Errorf("forgejo combined status %s/%s %s: %w", owner, repo, sha, err)
	}
	return CombinedStatus{State: res.State, Statuses: res.Statuses}, nil
}

// TreeEntries maps each of names to its object id as a top-level entry
// of commit's tree; an absent entry has no key. Two commits whose entry
// for a workflow directory differs changed something under it (rulings
// 90, 100), renames and deletions included, however large the diff. The
// listing is paged; a truncated one fails closed.
func (f *Forgejo) TreeEntries(ctx context.Context, owner, repo, commit string, names ...string) (map[string]string, error) {
	entries := map[string]string{}
	seen := 0
	for page := 1; ; page++ {
		path := fmt.Sprintf("/api/v1/repos/%s/%s/git/trees/%s?page=%d", owner, repo, url.PathEscape(commit), page)
		body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var res struct {
			Truncated  bool `json:"truncated"`
			TotalCount int  `json:"total_count"`
			Tree       []struct {
				Path string `json:"path"`
				SHA  string `json:"sha"`
			} `json:"tree"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("forgejo tree %s/%s %s: %w", owner, repo, commit, err)
		}
		if res.Truncated {
			return nil, fmt.Errorf("forgejo tree %s/%s %s: listing truncated", owner, repo, commit)
		}
		for _, e := range res.Tree {
			if slices.Contains(names, e.Path) {
				entries[e.Path] = e.SHA
			}
		}
		seen += len(res.Tree)
		if len(res.Tree) == 0 || seen >= res.TotalCount {
			return entries, nil
		}
	}
}

// ClosePullRequest closes pull request number without merging it.
func (f *Forgejo) ClosePullRequest(ctx context.Context, owner, repo string, number int64) error {
	payload, err := json.Marshal(map[string]any{"state": "closed"})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, repo, number), payload, http.StatusCreated)
	return err
}

// CreateIssueComment posts a comment on an issue: the workflow uses it to
// record why a task's attempts were exhausted or it otherwise failed, on
// the same issue task 42 filed at intake.
func (f *Forgejo) CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int64, body string) (int64, error) {
	payload, err := json.Marshal(map[string]any{"body": body})
	if err != nil {
		return 0, err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber)
	res, err := f.call(ctx, http.MethodPost, path, payload, http.StatusCreated)
	if err != nil {
		return 0, err
	}
	var c struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(res, &c); err != nil {
		return 0, fmt.Errorf("forgejo comment on %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	return c.ID, nil
}

// IssueComment is a comment on an issue or pull request as Forgejo lists
// it.
type IssueComment struct {
	ID        int64     `json:"id"`
	Author    string    `json:"-"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		UserName string `json:"login"`
	} `json:"user"`
}

// ListIssueComments lists every comment on issue or pull request number,
// oldest first, over as many pages as Forgejo serves.
func (f *Forgejo) ListIssueComments(ctx context.Context, owner, repo string, number int64) ([]IssueComment, error) {
	const limit = 50
	var out []IssueComment
	for page := 1; ; page++ {
		path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments?page=%d&limit=%d", owner, repo, number, page, limit)
		body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var res []IssueComment
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("forgejo comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, c := range res {
			c.Author = c.User.UserName
			out = append(out, c)
		}
		if len(res) < limit {
			return out, nil
		}
	}
}

// PullRequestState reads pull request number: open, closed or merged, and
// its head commit.
func (f *Forgejo) PullRequestState(ctx context.Context, owner, repo string, number int64) (PullRequestRef, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, repo, number), nil, http.StatusOK)
	if err != nil {
		return PullRequestRef{}, err
	}
	var res wirePR
	if err := json.Unmarshal(body, &res); err != nil {
		return PullRequestRef{}, fmt.Errorf("forgejo pull request %s/%s #%d: %w", owner, repo, number, err)
	}
	return res.ref(), nil
}

// IssueClosed reports whether issue number is closed.
func (f *Forgejo) IssueClosed(ctx context.Context, owner, repo string, number int64) (bool, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", owner, repo, number), nil, http.StatusOK)
	if err != nil {
		return false, err
	}
	var res struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return false, fmt.Errorf("forgejo issue %s/%s #%d: %w", owner, repo, number, err)
	}
	return res.State == "closed", nil
}

// RepoDefaultBranch reads a repository's default branch name. A defensive
// fallback for when a webhook payload's own repository.default_branch field
// is empty (the payload shape is reconstructed, not documented upstream).
func (f *Forgejo) RepoDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s", owner, repo)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var res struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("forgejo repo default branch %s/%s: %w", owner, repo, err)
	}
	return res.DefaultBranch, nil
}
