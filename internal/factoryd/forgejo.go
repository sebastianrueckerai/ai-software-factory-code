package factoryd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
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
	// and the writer lookups behind the trust gate (task 41, rulings 78,
	// 101, 101a): a write:repository token of the dedicated user
	// factory-ci scoped to the code repository. Forgejo's repository runner API demands the repository
	// owner (reqOwner), so factory-ci sits in the org's Owners team; main's
	// push and merge whitelist excludes it with apply_to_admins on, so the
	// token can neither push nor merge main. Never a registry credential,
	// never mounted into a runner pod.
	CIRegistrarToken string
	// CIOrgToken is factory-ci's second token, read:organization only and
	// no repository (ruling 108: Forgejo refuses read:organization on a
	// repository-limited token): the trust gate reads the org's teams,
	// their repositories and members with it.
	CIOrgToken string
	// MergeToken is the dedicated user factory-merge's token,
	// write:repository on the code repository only (ruling 109): main's
	// merge whitelist holds it, its push whitelist does not, and Forgejo
	// merges only after an approval by a whitelisted human. The Merge
	// activity uses it on the factory's own repositories; a managed
	// project has its own (Project.MergeToken, ruling 164).
	MergeToken string
	// ProjectsToken is the dedicated user factory-projects' token,
	// [write:repository, write:organization] on no fixed repository list
	// (Task 65): a member of org sf's team projects, which may create org
	// repositories and holds none, so it reaches exactly the repositories
	// it created or a human made it an admin collaborator on. It creates
	// a project's repository and keeps its webhook, collaborators and
	// branch protection; Forgejo keeps Actions secrets and runners to the
	// org owners, which it is not.
	ProjectsToken string
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
	return GetPages[TokenRef](ctx, f.call, fmt.Sprintf("/api/v1/users/%s/tokens?", f.BotUser))
}

// GetPages reads every page of a paged Forgejo list through call; path
// ends in ? or &.
// A page may hold fewer items than the limit asked for (the server caps
// it at its MAX_RESPONSE_ITEMS), so only an empty page ends the list.
func GetPages[T any](ctx context.Context, call func(context.Context, string, string, []byte, int) ([]byte, error), path string) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		body, err := call(ctx, http.MethodGet, fmt.Sprintf("%spage=%d&limit=50", path, page), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("forgejo GET %s: %w", path, err)
		}
		if len(items) == 0 {
			return all, nil
		}
		all = append(all, items...)
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
// orgCall authenticates with the org-read token.
func (f *Forgejo) orgCall(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.Header.Set("Authorization", "token "+f.CIOrgToken)
	})
}

func (f *Forgejo) ciCall(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.Header.Set("Authorization", "token "+f.CIRegistrarToken)
	})
}

// anySuccess as do's wantStatus admits every 2xx answer.
const anySuccess = 0

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
	if res.StatusCode != wantStatus && (wantStatus != anySuccess || res.StatusCode/100 != 2) {
		switch res.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%s %s: %w", method, path, errForgejoNotFound)
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		default:
			if res.StatusCode >= 400 && res.StatusCode < 500 {
				return nil, fmt.Errorf("forgejo %s %s: %w: status %d: %s", method, path, ErrRefused, res.StatusCode, body)
			}
		}
		return nil, fmt.Errorf("forgejo %s %s: status %d: %s", method, path, res.StatusCode, body)
	}
	return body, nil
}

// RepoWriters lists the users who may write the repository's code, the
// users the trust gate lets steer a task: members of the owner org's
// teams with write or more on the code unit of this repository, and direct
// collaborators with write or more. Forgejo shows no collaborator's
// permission to a scoped token (the per-user endpoint refuses every one
// that is not a site admin's, rulings 101 and 101a), so a collaborator
// counts by being on the assignee list (write or more, or a team's pull
// request read) and in no team without code write, which could be what
// put them there: such a collaborator fails closed. The org-read token
// reads the teams, their repositories and members (ruling 108); the
// registrar token reads the collaborators and assignees. An owner that is
// no organization has no team list, and the gate trusts no one.
func (f *Forgejo) RepoWriters(ctx context.Context, owner, repo string) ([]string, error) {
	type user struct {
		Login string `json:"login"`
	}
	type repository struct {
		FullName string `json:"full_name"`
	}
	fail := func(err error) ([]string, error) {
		return nil, fmt.Errorf("forgejo writers of %s/%s: %w", owner, repo, err)
	}
	logins := func(users []user) map[string]bool {
		set := map[string]bool{}
		for _, u := range users {
			set[u.Login] = true
		}
		return set
	}
	teams, err := GetPages[struct {
		ID       int64             `json:"id"`
		AllRepos bool              `json:"includes_all_repositories"`
		UnitsMap map[string]string `json:"units_map"`
	}](ctx, f.orgCall, fmt.Sprintf("/api/v1/orgs/%s/teams?", owner))
	if err != nil {
		return fail(err)
	}
	full := owner + "/" + repo
	writers, others := map[string]bool{}, map[string]bool{}
	for _, t := range teams {
		if !t.AllRepos {
			repos, err := GetPages[repository](ctx, f.orgCall, fmt.Sprintf("/api/v1/teams/%d/repos?", t.ID))
			if err != nil {
				return fail(err)
			}
			if !slices.Contains(repos, repository{full}) {
				continue
			}
		}
		members, err := GetPages[user](ctx, f.orgCall, fmt.Sprintf("/api/v1/teams/%d/members?", t.ID))
		if err != nil {
			return fail(err)
		}
		into := others
		// the unit, never the team's aggregate permission: Forgejo sets that
		// to the lowest unit mode, "write" for an issue-write team without
		// code; an admin or owner team lists every unit at its mode
		if codeWrite(t.UnitsMap["repo.code"]) {
			into = writers
		}
		maps.Copy(into, logins(members))
	}
	collaborators, err := assignedCollaborators(ctx, f.ciCall, full)
	if err != nil {
		return fail(err)
	}
	for _, login := range collaborators {
		if !others[login] {
			writers[login] = true
		}
	}
	return slices.Sorted(maps.Keys(writers)), nil
}

// ProjectWriters lists the writers of a managed project's repository
// (ruling 164) with the project's own factory-merge token, which reads
// the repository and nothing else: its collaborators with write or more
// (on the assignee list). The project's organisation holds only
// factory-projects as owner and its CI user in a team without code, so
// its teams add no writer.
func (f *Forgejo) ProjectWriters(ctx context.Context, repo Repo, token string) ([]string, error) {
	writers, err := assignedCollaborators(ctx, f.tokenCall(token), repo.String())
	if err != nil {
		return nil, fmt.Errorf("forgejo writers of %s: %w", repo, err)
	}
	return writers, nil
}

// assignedCollaborators are the collaborators of repository full who are
// also assignees: write or more, or a team's pull request read.
func assignedCollaborators(ctx context.Context, call func(context.Context, string, string, []byte, int) ([]byte, error), full string) ([]string, error) {
	type user struct {
		Login string `json:"login"`
	}
	body, err := call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/assignees", full), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var assignees []user
	if err := json.Unmarshal(body, &assignees); err != nil {
		return nil, err
	}
	collaborators, err := GetPages[user](ctx, call, fmt.Sprintf("/api/v1/repos/%s/collaborators?", full))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range collaborators {
		if slices.ContainsFunc(assignees, func(a user) bool { return a.Login == c.Login }) {
			out = append(out, c.Login)
		}
	}
	return out, nil
}

// codeWrite is a Forgejo access mode of write or more.
func codeWrite(mode string) bool {
	return mode == "write" || mode == "admin" || mode == "owner"
}

// CIJob is one Forgejo Actions job for a runner label (task 41: the
// `pr-untrusted` label routes pull-request jobs to the ephemeral gVisor
// runner instead of the privileged dind runner). TaskID is set once a
// runner picked the job up: the id of its actions/tasks entry.
type CIJob struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"`
	TaskID int64    `json:"task_id"`
}

// RunJobs lists owner/repo's Actions jobs carrying label, in every state
// the endpoint returns: it has no status filter. Only a job Forgejo marked
// "waiting" may get a runner; running or already-done jobs matching the
// label must not. The spawner names one namespace per job id (its
// idempotency key), but the runner it starts registers by label only and
// the server, not the caller, assigns it whichever waiting job it serves.
func (f *Forgejo) RunJobs(ctx context.Context, r CIRepo, label string) ([]CIJob, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/actions/runners/jobs?labels=%s", r.Repo, url.QueryEscape(label))
	body, err := f.tokenCall(r.Token)(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var all []CIJob
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("forgejo run jobs: %w", err)
	}
	return all, nil
}

// ActionTask is one entry of actions/tasks: a job a runner picked up.
// Forgejo sets RunStartedAt equal to CreatedAt, so the entry cannot tell
// queue time from run time; UpdatedAt of a finished task is its finish.
type ActionTask struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	HeadSHA      string    `json:"head_sha"`
	RunNumber    int64     `json:"run_number"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	RunStartedAt time.Time `json:"run_started_at"`
}

func (t ActionTask) Finished() bool { return doneStatus(t.Status) }

// doneStatus is a final Actions status.
func doneStatus(status string) bool {
	switch status {
	case "success", "failure", "cancelled", "skipped":
		return true
	}
	return false
}

// RunSeconds is a finished task's run time.
func (t ActionTask) RunSeconds() float64 { return t.UpdatedAt.Sub(t.RunStartedAt).Seconds() }

// ActionTasks reads one page (from 1) of owner/repo's actions/tasks,
// newest first.
func (f *Forgejo) ActionTasks(ctx context.Context, token, owner, repo string, page int) ([]ActionTask, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/tasks?page=%d&limit=%d", owner, repo, page, actionTasksPage)
	body, err := f.tokenCall(token)(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var res struct {
		Entries []ActionTask `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("forgejo actions tasks: %w", err)
	}
	return res.Entries, nil
}

const actionTasksPage = 50

// ActionRun is one entry of actions/runs. Status aggregates every job of
// the run, those still blocked or waiting included; Created is when the
// event (the push) created the run, Stopped when its last job finished.
type ActionRun struct {
	ID        int64     `json:"id"`
	CommitSHA string    `json:"commit_sha"`
	PrettyRef string    `json:"prettyref"` // the branch of a push run
	Event     string    `json:"event"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started"`
	Stopped   time.Time `json:"stopped"`
}

func (r ActionRun) Finished() bool { return doneStatus(r.Status) }

// ActionRuns reads owner/repo's actions/runs, newest first, filtered by
// query (page, limit, event, head_sha, ...).
func (f *Forgejo) ActionRuns(ctx context.Context, owner, repo string, query url.Values) ([]ActionRun, error) {
	body, err := f.ciCall(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/actions/runs?%s", owner, repo, query.Encode()), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var res struct {
		Entries []ActionRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("forgejo actions runs: %w", err)
	}
	return res.Entries, nil
}

// LatestCommit is the newest commit on branch that touched path, and its
// first parent ("" for a root commit).
func (f *Forgejo) LatestCommit(ctx context.Context, owner, repo, branch, path string) (sha, parent string, err error) {
	q := url.Values{"sha": {branch}, "path": {path}, "limit": {"1"}, "stat": {"false"}, "verification": {"false"}, "files": {"false"}}
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/commits?%s", owner, repo, q.Encode()), nil, http.StatusOK)
	if err != nil {
		return "", "", err
	}
	var commits []struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := json.Unmarshal(body, &commits); err != nil {
		return "", "", fmt.Errorf("forgejo commits of %s: %w", path, err)
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", "", fmt.Errorf("forgejo: no commit on %s touched %s", branch, path)
	}
	if len(commits[0].Parents) > 0 {
		parent = commits[0].Parents[0].SHA
	}
	return commits[0].SHA, parent, nil
}

// FileAt reads path at ref.
func (f *Forgejo) FileAt(ctx context.Context, owner, repo, ref, path string) ([]byte, error) {
	return f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/raw/%s?ref=%s", owner, repo, path, url.QueryEscape(ref)), nil, http.StatusOK)
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
func (f *Forgejo) RegisterCIRunner(ctx context.Context, r CIRepo, name string) (CIRunnerRegistration, error) {
	var reg CIRunnerRegistration
	payload, err := json.Marshal(map[string]any{
		"name":      name,
		"ephemeral": true,
	})
	if err != nil {
		return reg, err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/actions/runners", r.Repo)
	body, err := f.tokenCall(r.Token)(ctx, http.MethodPost, path, payload, http.StatusCreated)
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
func (f *Forgejo) DeleteCIRunner(ctx context.Context, r CIRepo, id int64) error {
	path := fmt.Sprintf("/api/v1/repos/%s/actions/runners/%d", r.Repo, id)
	_, err := f.tokenCall(r.Token)(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
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
func (f *Forgejo) ListCIRunners(ctx context.Context, r CIRepo) ([]CIRunnerRef, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/actions/runners", r.Repo)
	body, err := f.tokenCall(r.Token)(ctx, http.MethodGet, path, nil, http.StatusOK)
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

// ListLabels lists owner/repo's currently defined labels, all pages.
func (f *Forgejo) ListLabels(ctx context.Context, owner, repo string) ([]RepoLabel, error) {
	return GetPages[RepoLabel](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/labels?", owner, repo))
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

// DeleteBranch deletes a branch. Absent means nothing to do (never pushed, or
// a retried Finish). Forgejo 15 answers DELETE on a missing branch with 500
// rather than 404, so the branch is looked up first.
func (f *Forgejo) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(branch))
	if _, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
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
	Base     string // the branch it merges into
	MergedBy string // who merged it, when Merged
	// MergeCommit is the commit its merge made on the base, when Merged.
	MergeCommit string
	// Mergeable is Forgejo's verdict that the head merges into the base
	// without conflict.
	Mergeable bool
}

// wirePR is Forgejo's pull request JSON, as far as the factory reads it.
type wirePR struct {
	Number    int64  `json:"number"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Mergeable bool   `json:"mergeable"`
	MergeSHA  string `json:"merge_commit_sha"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	MergedBy *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (w wirePR) ref() PullRequestRef {
	ref := PullRequestRef{Number: w.Number, URL: w.HTMLURL, Head: w.Head.Ref, HeadRepo: w.Head.Repo.FullName, HeadSHA: w.Head.SHA,
		State: w.State, Merged: w.Merged, Mergeable: w.Mergeable, Base: w.Base.Ref, MergeCommit: w.MergeSHA}
	if w.MergedBy != nil {
		ref.MergedBy = w.MergedBy.Login
	}
	return ref
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
	all, err := GetPages[wirePR](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=open&", owner, repo))
	if err != nil {
		return nil, err
	}
	for _, pr := range all {
		if pr.Head.Ref == branch && pr.Head.Repo.FullName == headRepo.String() {
			ref := pr.ref()
			return &ref, nil
		}
	}
	return nil, nil
}

// FindPullRequestByHead is owner/repo's pull request from branch on headRepo
// into base, whatever its state, or nil. Reconcile uses it while no number
// is recorded: a crash between opening the pull request and recording it
// leaves one the open list stops showing once the reviewer merges or closes
// it.
func (f *Forgejo) FindPullRequestByHead(ctx context.Context, owner, repo, base string, headRepo Repo, branch string) (*PullRequestRef, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%s/%s:%s", owner, repo, url.PathEscape(base), headRepo.String(), branch)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pr wirePR
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("forgejo find pull request %s/%s %s: %w", owner, repo, branch, err)
	}
	ref := pr.ref()
	return &ref, nil
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
// listing is paged by total_count; Forgejo's truncated flag only says the
// tree exceeds one page, whichever page it is on.
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
			TotalCount int `json:"total_count"`
			Tree       []struct {
				Path string `json:"path"`
				SHA  string `json:"sha"`
			} `json:"tree"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("forgejo tree %s/%s %s: %w", owner, repo, commit, err)
		}
		for _, e := range res.Tree {
			if slices.Contains(names, e.Path) {
				entries[e.Path] = e.SHA
			}
		}
		seen += len(res.Tree)
		if seen >= res.TotalCount {
			return entries, nil
		}
		if len(res.Tree) == 0 {
			return nil, fmt.Errorf("forgejo tree %s/%s %s: pages ended at %d of %d entries", owner, repo, commit, seen, res.TotalCount)
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

// CloseIssue closes issue number as the bot.
func (f *Forgejo) CloseIssue(ctx context.Context, owner, repo string, number int64) error {
	payload, err := json.Marshal(map[string]any{"state": "closed"})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", owner, repo, number), payload, http.StatusCreated)
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
// oldest first. Forgejo does not page this list: one request holds all.
func (f *Forgejo) ListIssueComments(ctx context.Context, owner, repo string, number int64) ([]IssueComment, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", owner, repo, number), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []IssueComment
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("forgejo comments on %s/%s#%d: %w", owner, repo, number, err)
	}
	for i := range out {
		out[i].Author = out[i].User.UserName
	}
	return out, nil
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

// errNoCloser is a closed issue whose timeline names no one who closed it.
var errNoCloser = errors.New("closed with no close event on its timeline")

// IssueClosedBy is the user who closed issue number, "" while it is open:
// the last close event on its timeline, who a webhook's sender would have
// been. A closed issue with no close event is errNoCloser rather than a
// closer to trust.
func (f *Forgejo) IssueClosedBy(ctx context.Context, owner, repo string, number int64) (string, error) {
	closed, err := f.IssueClosed(ctx, owner, repo, number)
	if err != nil || !closed {
		return "", err
	}
	events, err := GetPages[struct {
		Type string `json:"type"`
		User struct {
			UserName string `json:"login"`
		} `json:"user"`
	}](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/timeline?", owner, repo, number))
	if err != nil {
		return "", err
	}
	closer := ""
	for _, e := range events {
		if e.Type == "close" {
			closer = e.User.UserName
		}
	}
	if closer == "" {
		return "", fmt.Errorf("forgejo issue %s/%s#%d: %w", owner, repo, number, errNoCloser)
	}
	return closer, nil
}

// OpenIssues lists owner/repo's open issues, not pull requests.
func (f *Forgejo) OpenIssues(ctx context.Context, owner, repo string) ([]webhookIssue, error) {
	return GetPages[webhookIssue](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/issues?state=open&type=issues&", owner, repo))
}

// DefaultBranchHeadSHA is the head of owner/repo's default branch, the base
// every agent task is stamped with at intake; branch is the default branch
// when the caller already knows it (a webhook payload), else it is read.
func (f *Forgejo) DefaultBranchHeadSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	if branch == "" {
		var err error
		if branch, err = f.RepoDefaultBranch(ctx, owner, repo); err != nil {
			return "", err
		}
	}
	return f.BranchHeadSHA(ctx, owner, repo, branch)
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

// PullRequestFiles lists the paths pull request number changes, every
// page; a rename or copy lists its old name too, so a file moved out of a
// guarded directory still counts against it. (The compare API's file list
// carries no previous name.)
func (f *Forgejo) PullRequestFiles(ctx context.Context, owner, repo string, number int64) ([]string, error) {
	files, err := GetPages[struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	}](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/files?", owner, repo, number))
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, c := range files {
		paths = append(paths, c.Filename)
		if c.PreviousFilename != "" && c.PreviousFilename != c.Filename {
			paths = append(paths, c.PreviousFilename)
		}
	}
	return paths, nil
}

// PullRequestDiff is pull request number's unified diff, at most max bytes
// (the rest cut, and said so).
func (f *Forgejo) PullRequestDiff(ctx context.Context, owner, repo string, number int64, max int) (string, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d.diff", owner, repo, number), nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	if len(body) > max {
		return string(body[:max]) + fmt.Sprintf("\n… (diff cut at %d of %d bytes)", max, len(body)), nil
	}
	return string(body), nil
}

// MergePullRequest merges pull request number as factory-merge with token,
// only while its head is still head: a head that moved after the gates is
// refused. Forgejo refuses the merge (ErrRefused, 405) without the
// approval of a human on main's approvals whitelist, where main requires
// one (every repository on the merge rule human).
func (f *Forgejo) MergePullRequest(ctx context.Context, token, owner, repo string, number int64, head, message string) error {
	payload, err := json.Marshal(map[string]any{"Do": "merge", "head_commit_id": head, "MergeMessageField": message})
	if err != nil {
		return err
	}
	_, err = f.tokenCall(token)(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/merge", owner, repo, number), payload, http.StatusOK)
	// do retries a 409 as transient; on merge it is a conflict or a head
	// that moved, which no retry fixes
	if err != nil && !errors.Is(err, ErrRefused) && strings.Contains(err.Error(), ": status 409: ") {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	return err
}

// SyncFork fast-forwards fork's default branch to its upstream's head.
// Forgejo 15 reports an up-to-date fork as not allowed and answers its
// sync 400, so the state is read first.
func (f *Forgejo) SyncFork(ctx context.Context, fork Repo) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/sync_fork", fork.Owner, fork.Name)
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var st struct {
		Allowed bool `json:"allowed"`
		Behind  int  `json:"commits_behind"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return fmt.Errorf("forgejo sync_fork %s: %w", fork, err)
	}
	if st.Behind == 0 {
		return nil
	}
	if !st.Allowed {
		return fmt.Errorf("forgejo: %s is %d commits behind its upstream and cannot be synced", fork, st.Behind)
	}
	_, err = f.call(ctx, http.MethodPost, path, nil, http.StatusNoContent)
	return err
}

// FileContent reads path at ref with its blob sha, which a change of the
// file must name.
func (f *Forgejo) FileContent(ctx context.Context, owner, repo, ref, path string) (content []byte, blob string, err error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, url.QueryEscape(ref)), nil, http.StatusOK)
	if err != nil {
		return nil, "", err
	}
	var res struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, "", fmt.Errorf("forgejo contents %s: %w", path, err)
	}
	if res.Encoding != "base64" {
		return nil, "", fmt.Errorf("forgejo contents %s: encoding %q", path, res.Encoding)
	}
	content, err = base64.StdEncoding.DecodeString(res.Content)
	return content, res.SHA, err
}

// FileChange replaces one file's content; Blob is the sha it replaces.
type FileChange struct {
	Path    string
	Content []byte
	Blob    string
}

// CommitFiles writes changes as one commit on branch, or on newBranch
// created from branch when newBranch is set, as the bot, and returns the
// commit's sha.
func (f *Forgejo) CommitFiles(ctx context.Context, owner, repo, branch, newBranch, message string, changes []FileChange) (string, error) {
	files := make([]map[string]string, 0, len(changes))
	for _, c := range changes {
		files = append(files, map[string]string{"operation": "update", "path": c.Path, "sha": c.Blob,
			"content": base64.StdEncoding.EncodeToString(c.Content)})
	}
	req := map[string]any{"branch": branch, "message": message, "files": files}
	if newBranch != "" {
		req["new_branch"] = newBranch
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	body, err := f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/contents", owner, repo), payload, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var res struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Commit.SHA == "" {
		return "", fmt.Errorf("forgejo contents commit on %s/%s: no commit in the answer", owner, repo)
	}
	return res.Commit.SHA, nil
}

// Comparison is what head has that base lacks: its commits, newest first,
// and the files they touch.
type Comparison struct {
	Commits []ComparedCommit
	Files   []string
}

// ComparedCommit is one commit of a comparison: its sha, its message's
// first line and how many parents it has (a merge has two).
type ComparedCommit struct {
	SHA     string
	Subject string
	Parents int
}

// Compare reads base...head.
func (f *Forgejo) Compare(ctx context.Context, owner, repo, base, head string) (Comparison, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/compare/%s...%s", owner, repo, url.PathEscape(base), url.PathEscape(head)), nil, http.StatusOK)
	if err != nil {
		return Comparison{}, err
	}
	var res struct {
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
			Parents []struct{} `json:"parents"`
		} `json:"commits"`
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return Comparison{}, fmt.Errorf("forgejo compare %s...%s: %w", base, head, err)
	}
	var out Comparison
	for _, c := range res.Commits {
		out.Commits = append(out.Commits, ComparedCommit{SHA: c.SHA, Subject: FirstLine(c.Commit.Message), Parents: len(c.Parents)})
	}
	for _, f := range res.Files {
		out.Files = append(out.Files, f.Filename)
	}
	return out, nil
}

// CommitParents are a commit's parents, the first parent first.
func (f *Forgejo) CommitParents(ctx context.Context, owner, repo, sha string) ([]string, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/git/commits/%s?stat=false&verification=false&files=false", owner, repo, url.PathEscape(sha)), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var res struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("forgejo commit %s: %w", sha, err)
	}
	var out []string
	for _, p := range res.Parents {
		out = append(out, p.SHA)
	}
	return out, nil
}

// MergedPullRequest is a merged pull request as the roll's body names it.
type MergedPullRequest struct {
	Number      int64
	Title       string
	MergeCommit string
	HeadSHA     string
}

// RecentMergedPullRequests lists the merged ones of the closed pull
// requests updated last.
func (f *Forgejo) RecentMergedPullRequests(ctx context.Context, owner, repo string) ([]MergedPullRequest, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=closed&sort=recentupdate&limit=50", owner, repo), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number      int64  `json:"number"`
		Title       string `json:"title"`
		Merged      bool   `json:"merged"`
		MergeCommit string `json:"merge_commit_sha"`
		Head        struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &prs); err != nil {
		return nil, fmt.Errorf("forgejo pulls of %s/%s: %w", owner, repo, err)
	}
	var out []MergedPullRequest
	for _, p := range prs {
		if p.Merged {
			out = append(out, MergedPullRequest{Number: p.Number, Title: p.Title, MergeCommit: p.MergeCommit, HeadSHA: p.Head.SHA})
		}
	}
	return out, nil
}
