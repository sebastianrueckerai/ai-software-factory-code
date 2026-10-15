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
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

	// CIOrgToken is factory-ci's second token, read:organization only and
	// no repository (ruling 108: Forgejo refuses read:organization on a
	// repository-limited token): the trust gate reads the org's teams,
	// their repositories and members with it.
	CIOrgToken string
	// ProjectsToken is the dedicated user factory-projects' token,
	// write:repository on no fixed repository list (ruling 164): the owner
	// of every project organisation and no member of sf, so it reaches the
	// project repositories and nothing of the factory's own. It keeps a
	// project's webhook, collaborators, branch protection, deploy key,
	// Actions secrets and runners; the organisation and the repository
	// come from registration with the site admin's login from OpenBao.
	ProjectsToken string
	// Seeded are the tokens the seed makes, by their secret's name
	// (policy.SeedSecrets), which a seed project's file names (ruling 179):
	// forgejo-ci-registrar, factory-ci's write:repository on the code
	// repository (factory-ci sits in org sf's Owners team, which Forgejo's
	// runner API demands; main's whitelists exclude it, rulings 78, 101,
	// 101a); forgejo-merge, factory-merge's write:repository on the code
	// repository (ruling 109: main's merge whitelist holds it, its push
	// whitelist does not); forgejo-config, factory-config's read:repository
	// on the infra repository (ruling 183).
	Seeded map[string]string
	// token, when set, is the one call authenticates with instead of the
	// bot's login (As).
	token string
}

// As is f acting as user with token wherever it acts as the bot: its
// branch, file, fork and pull request calls, and Fork, are user's.
func (f *Forgejo) As(user, token string) *Forgejo {
	g := *f
	g.BotUser, g.BotPassword, g.token = user, "", token
	return &g
}

// ProjectAccess is what reaches a project's repository for what.
type ProjectAccess struct {
	// Read reads the collaborators and assignees (the trust gate) and the
	// webhooks where its user administers the repository.
	Read string
	// Actions lists the Actions jobs and registers runners.
	Actions string
	// Merge merges an approved pull request.
	Merge string
}

// Access is the one mapping from a project to factoryd's tokens: a
// factory project's are registration's (ruling 164: its own factory-merge
// token reads and merges, factory-projects owns its Actions), a seed
// project's are the seeded secrets its file names, never factory-projects'.
// An empty field is a token the project has none of.
func (f *Forgejo) Access(p Project) ProjectAccess {
	if p.Objects == policy.ObjectsFactory {
		return ProjectAccess{Read: p.MergeToken, Actions: f.ProjectsToken, Merge: p.MergeToken}
	}
	return ProjectAccess{Read: f.Seeded[p.Tokens.Read], Actions: f.Seeded[p.Tokens.Actions], Merge: f.Seeded[p.Tokens.Merge]}
}

// Writers are the users the trust gate lets steer a task on p: a factory
// project's write collaborators (its organisation holds only
// factory-projects and its CI user in a team without code), a seed
// project's RepoWriters.
func (f *Forgejo) Writers(ctx context.Context, p Project) ([]string, error) {
	token := f.Access(p).Read
	if token == "" {
		return nil, fmt.Errorf("forgejo writers of %s: project %s has no read token", p.Repository, p.Name)
	}
	if p.Objects == policy.ObjectsFactory {
		writers, err := assignedCollaborators(ctx, f.tokenCall(token), p.Repository)
		if err != nil {
			return nil, fmt.Errorf("forgejo writers of %s: %w", p.Repository, err)
		}
		return writers, nil
	}
	return f.RepoWriters(ctx, p.Repo(), token)
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
	all, _, err := GetPagesUpTo[T](ctx, call, path, 0)
	return all, err
}

// GetPagesUpTo reads the pages of the list as GetPages does, at most max
// of them (0 every one); more is true when it stopped at max with items
// on the last page it read.
func GetPagesUpTo[T any](ctx context.Context, call func(context.Context, string, string, []byte, int) ([]byte, error), path string, max int) (all []T, more bool, err error) {
	for page := 1; max == 0 || page <= max; page++ {
		body, err := call(ctx, http.MethodGet, fmt.Sprintf("%spage=%d&limit=50", path, page), nil, http.StatusOK)
		if err != nil {
			return nil, false, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, false, fmt.Errorf("forgejo GET %s: %w", path, err)
		}
		if len(items) == 0 {
			return all, false, nil
		}
		all = append(all, items...)
	}
	return all, true, nil
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
	if f.token != "" {
		return f.tokenCall(f.token)(ctx, method, path, payload, wantStatus)
	}
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.SetBasicAuth(f.BotUser, f.BotPassword)
	})
}

// orgCall authenticates with the org-read token.
func (f *Forgejo) orgCall(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.Header.Set("Authorization", "token "+f.CIOrgToken)
	})
}

// ErrAnswerTooLong is an answer past maxForgejoAnswer: an error for every
// caller but PullRequestDiff, which takes the cut body.
var ErrAnswerTooLong = errors.New("the answer is longer than 1 MiB")

// maxForgejoAnswer bounds a Forgejo answer factoryd reads.
const maxForgejoAnswer = 1 << 20

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
	body, err := io.ReadAll(io.LimitReader(res.Body, maxForgejoAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("forgejo %s %s: %w", method, path, err)
	}
	var tooLong error
	if len(body) > maxForgejoAnswer {
		body, tooLong = body[:maxForgejoAnswer], fmt.Errorf("forgejo %s %s: %w", method, path, ErrAnswerTooLong)
	}
	if res.StatusCode != wantStatus && (wantStatus != anySuccess || res.StatusCode/100 != 2) {
		switch res.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%s %s: %w", method, path, errForgejoNotFound)
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		default:
			if res.StatusCode >= 400 && res.StatusCode < 500 {
				refused := ErrRefused
				if res.StatusCode == http.StatusForbidden {
					refused = errForgejoForbidden
				}
				return nil, fmt.Errorf("forgejo %s %s: %w: status %d: %s", method, path, refused, res.StatusCode, body)
			}
		}
		return nil, fmt.Errorf("forgejo %s %s: status %d: %s", method, path, res.StatusCode, body)
	}
	return body, tooLong
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
// reads the teams, their repositories and members (ruling 108); token,
// the project's read token, reads the collaborators and assignees. An
// owner that is no organization has no team list, and the gate trusts no
// one.
func (f *Forgejo) RepoWriters(ctx context.Context, r Repo, token string) ([]string, error) {
	owner, repo := r.Owner, r.Name
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
	collaborators, err := assignedCollaborators(ctx, f.tokenCall(token), full)
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
// runner picked the job up: the id of its actions/tasks entry. Handle
// names the job's current attempt, which a runner may ask for alone.
type CIJob struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"`
	TaskID int64    `json:"task_id"`
	Handle string   `json:"handle"`
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
	ID        int64  `json:"id"`
	Index     int64  `json:"index_in_repo"` // the run number
	CommitSHA string `json:"commit_sha"`
	PrettyRef string `json:"prettyref"` // the branch of a push run
	Event     string `json:"event"`
	// TriggerEvent is what started the run: a scheduled run reads push
	// in Event, schedule here
	TriggerEvent string    `json:"trigger_event"`
	Status       string    `json:"status"`
	Created      time.Time `json:"created"`
	Started      time.Time `json:"started"`
	Stopped      time.Time `json:"stopped"`
	// EventPayload is the webhook payload that created the run, as JSON
	EventPayload string `json:"event_payload"`
}

func (r ActionRun) Finished() bool { return doneStatus(r.Status) }

// PushedRef is a push run's full ref from its payload, "" when it names
// none: unlike PrettyRef, which reads "main" for a tag named main too, it
// tells a branch from a tag.
func (r ActionRun) PushedRef() string {
	var p struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(r.EventPayload), &p); err != nil {
		return ""
	}
	return p.Ref
}

// ActionRuns reads owner/repo's actions/runs, newest first, filtered by
// query (page, limit, event, head_sha, ...). Forgejo 15 honours limit only
// with a page, so page 1 is asked when query names none. Forgejo files a
// scheduled run under event push (main's nightly, run 1241), so a query
// for an event drops the runs another trigger started: a push run is one
// a push made.
func (f *Forgejo) ActionRuns(ctx context.Context, token, owner, repo string, query url.Values) ([]ActionRun, error) {
	if query.Get("page") == "" {
		query = maps.Clone(query)
		query.Set("page", "1")
	}
	body, err := f.tokenCall(token)(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/actions/runs?%s", owner, repo, query.Encode()), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var res struct {
		Entries []ActionRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("forgejo actions runs: %w", err)
	}
	if event := query.Get("event"); event != "" {
		res.Entries = slices.DeleteFunc(res.Entries, func(r ActionRun) bool { return r.TriggerEvent != "" && r.TriggerEvent != event })
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
// exist on owner/repo: Unchanged when none was missing. It must run at
// boot, before anything files an issue under one of these names or
// mirrors state onto one: Forgejo's issue-label endpoints resolve labels
// by name and silently drop any name that doesn't exist yet, so a label
// first used mid-mirror would vanish instead of applying.
func (f *Forgejo) EnsureLabels(ctx context.Context, owner, repo string, wanted map[string]string) (Change, error) {
	existing, err := f.ListLabels(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("forgejo ensure labels %s/%s: %w", owner, repo, err)
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[l.Name] = true
	}
	var missing []string
	for name := range wanted {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return Unchanged, nil
	}
	slices.Sort(missing)
	var errs []error
	for _, name := range missing {
		if err := f.CreateLabel(ctx, owner, repo, name, wanted[name]); err != nil {
			errs = append(errs, fmt.Errorf("label %s: %w", name, err))
		}
	}
	return Change(fmt.Sprintf("created %s", strings.Join(missing, ", "))), errors.Join(errs...)
}

// EnsureProjectLabels is a project's labels step: state/suggested renamed
// state/backlog first (renameLabel), then BootLabels and the lane labels
// of lanes, the project's process, created where missing.
func (f *Forgejo) EnsureProjectLabels(ctx context.Context, owner, repo string, lanes []policy.Lane) (Change, error) {
	renamed, err := f.renameLabel(ctx, owner, repo, "state/suggested", "state/backlog")
	if err != nil {
		return "", err
	}
	wanted := maps.Clone(BootLabels)
	maps.Copy(wanted, laneLabels(lanes))
	ensured, err := f.EnsureLabels(ctx, owner, repo, wanted)
	var changes []string
	for _, c := range []Change{renamed, ensured} {
		if c != "" && c != Unchanged {
			changes = append(changes, string(c))
		}
	}
	if len(changes) == 0 {
		return Unchanged, err
	}
	return Change(strings.Join(changes, "; ")), err
}

// renameLabel renames label from of owner/repo to to in place, so every
// issue carrying it follows. With both present (a half-done earlier run)
// it moves from's open issues to to and deletes from; with from absent it
// does nothing.
func (f *Forgejo) renameLabel(ctx context.Context, owner, repo, from, to string) (Change, error) {
	existing, err := f.ListLabels(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("forgejo labels %s/%s: %w", owner, repo, err)
	}
	old := slices.IndexFunc(existing, func(l RepoLabel) bool { return l.Name == from })
	if old < 0 {
		return Unchanged, nil
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/labels/%d", owner, repo, existing[old].ID)
	if !slices.ContainsFunc(existing, func(l RepoLabel) bool { return l.Name == to }) {
		payload, err := json.Marshal(map[string]string{"name": to})
		if err != nil {
			return "", err
		}
		if _, err := f.call(ctx, http.MethodPatch, path, payload, http.StatusOK); err != nil {
			return "", fmt.Errorf("forgejo rename label %s of %s/%s: %w", from, owner, repo, err)
		}
		return Change(fmt.Sprintf("renamed %s to %s", from, to)), nil
	}
	issues, err := f.OpenIssues(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	moved := 0
	for _, is := range issues {
		names := labelNames(is.Labels)
		if !slices.Contains(names, from) {
			continue
		}
		names = slices.DeleteFunc(names, func(n string) bool { return n == from || n == to })
		if err := f.ReplaceIssueLabels(ctx, owner, repo, is.Index, append(names, to)); err != nil {
			return "", fmt.Errorf("forgejo move issue #%d of %s/%s to %s: %w", is.Index, owner, repo, to, err)
		}
		moved++
	}
	if _, err := f.call(ctx, http.MethodDelete, path, nil, http.StatusNoContent); err != nil {
		return "", fmt.Errorf("forgejo delete label %s of %s/%s: %w", from, owner, repo, err)
	}
	return Change(fmt.Sprintf("moved %d issues from %s to %s and deleted %s", moved, from, to, from)), nil
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

// CopyBranch creates branch to on repo at branch from's head; nothing when
// from does not exist (Forgejo answers 404), and a branch to that exists
// is kept (Forgejo would answer 409).
func (f *Forgejo) CopyBranch(ctx context.Context, repo Repo, from, to string) error {
	if _, err := f.BranchHeadSHA(ctx, repo.Owner, repo.Name, to); !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/branches", repo.Owner, repo.Name),
		mustJSON(map[string]string{"new_branch_name": to, "old_branch_name": from}), http.StatusCreated)
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
	// MergeBase is the base commit Forgejo last merged the head against.
	MergeBase string
	Body      string
}

// wirePR is Forgejo's pull request JSON, as far as the factory reads it.
type wirePR struct {
	Number    int64  `json:"number"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Mergeable bool   `json:"mergeable"`
	MergeSHA  string `json:"merge_commit_sha"`
	MergeBase string `json:"merge_base"`
	Body      string `json:"body"`
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
		State: w.State, Merged: w.Merged, Mergeable: w.Mergeable, Base: w.Base.Ref, MergeCommit: w.MergeSHA, MergeBase: w.MergeBase, Body: w.Body}
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
	open, err := f.OpenPullRequests(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	for _, pr := range open {
		if pr.Head == branch && pr.HeadRepo == headRepo.String() {
			return &pr, nil
		}
	}
	return nil, nil
}

// OpenPullRequests lists owner/repo's open pull requests.
func (f *Forgejo) OpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequestRef, error) {
	all, err := GetPages[wirePR](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=open&", owner, repo))
	var out []PullRequestRef
	for _, pr := range all {
		out = append(out, pr.ref())
	}
	return out, err
}

// LastClosedPullRequestFrom is the pull request from branch on headRepo
// into owner/repo that closed last among poster's 50 closed last, or nil.
// (Forgejo 15's base and head lookup answers the oldest.)
func (f *Forgejo) LastClosedPullRequestFrom(ctx context.Context, owner, repo, poster string, headRepo Repo, branch string) (*PullRequestRef, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=closed&sort=recentclose&poster=%s&page=1&limit=50", owner, repo, url.QueryEscape(poster))
	body, err := f.call(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var prs []wirePR
	if err := json.Unmarshal(body, &prs); err != nil {
		return nil, fmt.Errorf("forgejo closed pull requests of %s/%s: %w", owner, repo, err)
	}
	for _, pr := range prs {
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
// "failure" or "warning". Created is when Forgejo recorded it; a finished
// job's, when Forgejo learned the job ended.
type CommitStatus struct {
	State       string    `json:"status"`
	Context     string    `json:"context"`
	TargetURL   string    `json:"target_url"`
	Description string    `json:"description"`
	Created     time.Time `json:"created_at,omitzero"`
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

// JobLog fetches the tail of a finished job's raw log, its last
// maxForgejoAnswer bytes, where a failed step's error and the runner's
// own lines stand; a log cut there starts mid-line. Forgejo 15.0.9 has
// no /api/v1 route for it (its own swagger lists none, and the design's
// guessed GET /api/v1/repos/{owner}/{repo}/actions/jobs/{job_id}/logs
// 404s); the web route that serves it (routers/web/web.go: GET
// .../actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs, actions.Logs)
// only recognizes a signed-in session — basic auth and a scoped token both
// 404 on it, though the same credentials 200 the equivalent API calls
// (verified live against a Forgejo 15.0.9 container: task-115-report.md).
// factoryd logs in as the bot user, already a write collaborator on every
// project repository (register.go's collaborator step), to reach it.
// targetURL is a commit status's target_url, e.g.
// "/sf/repo/actions/runs/7/jobs/0"; Forgejo redirects it to the run's
// latest attempt, which this follows before asking for that attempt's log.
// actions.Logs serves the log with http.ServeContent over a seekable
// reader (zstd-compressed in storage too), so a suffix Range asks for the
// tail alone.
//
// httpsJar wraps a cookiejar so every lookup and store believes the URL is
// https, regardless of the scheme actually dialed.
type httpsJar struct{ http.CookieJar }

func (j httpsJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	v := *u
	v.Scheme = "https"
	j.CookieJar.SetCookies(&v, cookies)
}

func (j httpsJar) Cookies(u *url.URL) []*http.Cookie {
	v := *u
	v.Scheme = "https"
	return j.CookieJar.Cookies(&v)
}

func (f *Forgejo) JobLog(ctx context.Context, targetURL string) ([]byte, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("forgejo job log %s: %w", targetURL, err)
	}
	base, err := url.Parse(f.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("forgejo job log %s: %w", targetURL, err)
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		// Production's ROOT_URL is https, so Forgejo marks the session
		// cookie Secure; cookiejar withholds a Secure cookie from a plain
		// http request, and BaseURL is always http (the in-cluster
		// address). httpsJar makes every jar lookup believe it is https,
		// so the cookie set at login is the one sent back.
		Jar: httpsJar{jar},
		// Forgejo's job page redirects to its attempt with an absolute URL
		// built from its own ROOT_URL, which is PublicURL's host, not
		// BaseURL's (the Forgejo struct's own split): left alone, the
		// client would leave the in-cluster address for the human-facing
		// one. Every redirect is rewritten back onto BaseURL.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			req.URL.Scheme, req.URL.Host = base.Scheme, base.Host
			return nil
		},
	}

	login, err := http.NewRequestWithContext(ctx, http.MethodPost, f.BaseURL+"/user/login",
		strings.NewReader(url.Values{"user_name": {f.BotUser}, "password": {f.BotPassword}}.Encode()))
	if err != nil {
		return nil, err
	}
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(login)
	if err != nil {
		return nil, fmt.Errorf("forgejo job log %s: login: %w", targetURL, err)
	}
	_ = res.Body.Close()
	if res.Request.URL.Path == "/user/login" {
		return nil, fmt.Errorf("forgejo job log %s: login refused", targetURL)
	}

	viewReq, err := http.NewRequestWithContext(ctx, http.MethodGet, f.BaseURL+targetURL, nil)
	if err != nil {
		return nil, err
	}
	view, err := client.Do(viewReq)
	if err != nil {
		return nil, fmt.Errorf("forgejo job log %s: %w", targetURL, err)
	}
	_ = view.Body.Close()

	logsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, view.Request.URL.String()+"/logs", nil)
	if err != nil {
		return nil, err
	}
	logsReq.Header.Set("Range", fmt.Sprintf("bytes=-%d", maxForgejoAnswer))
	res, err = client.Do(logsReq)
	if err != nil {
		return nil, fmt.Errorf("forgejo job log %s: %w", targetURL, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxForgejoAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("forgejo job log %s: %w", targetURL, err)
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("forgejo job log %s: status %d", targetURL, res.StatusCode)
	}
	if len(body) > maxForgejoAnswer {
		// the Range went unheeded: the body is the log's head
		return nil, fmt.Errorf("forgejo job log %s: %w", targetURL, ErrAnswerTooLong)
	}
	return body, nil
}

// WorkflowTreeEntries maps each workflow directory among commit's
// top-level tree entries, in any case, to the path and object id of every
// entry naming it; an absent directory has no key. Two commits whose
// value for a workflow directory differs changed something under it
// (rulings 90, 100), renames, deletions and case variants included,
// however large the diff. The listing is paged by total_count; Forgejo's
// truncated flag only says the tree exceeds one page, whichever page it
// is on.
func (f *Forgejo) WorkflowTreeEntries(ctx context.Context, owner, repo, commit string) (map[string]string, error) {
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
			if dir, ok := WorkflowDir(e.Path); ok {
				entries[dir] += e.Path + " " + e.SHA + "\n"
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

// ReopenPullRequest reopens closed pull request number.
func (f *Forgejo) ReopenPullRequest(ctx context.Context, owner, repo string, number int64) error {
	_, err := f.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, repo, number), mustJSON(map[string]string{"state": "open"}), http.StatusCreated)
	return err
}

// EditPullRequestBody sets pull request number's description.
func (f *Forgejo) EditPullRequestBody(ctx context.Context, owner, repo string, number int64, body string) error {
	_, err := f.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, repo, number), mustJSON(map[string]string{"body": body}), http.StatusCreated)
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

// LabelAdder is the user of the latest entry on issue number's timeline
// that added one of labels, skip's aside (the bot, whose state/* mirror is
// most of them); "" when none did. Forgejo's entry of type label carries
// body "1" for an addition and "" for a removal.
func (f *Forgejo) LabelAdder(ctx context.Context, owner, repo string, number int64, labels []string, skip string) (string, error) {
	events, err := GetPages[struct {
		Type string `json:"type"`
		User struct {
			UserName string `json:"login"`
		} `json:"user"`
		Label *struct {
			Name string `json:"name"`
		} `json:"label"`
		Body string `json:"body"`
	}](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/timeline?", owner, repo, number))
	if err != nil {
		return "", err
	}
	adder := ""
	for _, e := range events {
		if e.Type == "label" && e.Body == "1" && e.Label != nil && slices.Contains(labels, e.Label.Name) && e.User.UserName != skip {
			adder = e.User.UserName
		}
	}
	return adder, nil
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

// PRFile is a file a pull request changes: its name, its old name on a
// rename or copy, and its lines added and deleted.
type PRFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
}

// PullRequestFiles lists the files pull request number changes, every
// page. (The compare API's file list carries no previous name.)
func (f *Forgejo) PullRequestFiles(ctx context.Context, owner, repo string, number int64) ([]PRFile, error) {
	return GetPages[PRFile](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/files?", owner, repo, number))
}

// diffCut opens the note PullRequestDiff appends to a diff it cut.
const diffCut = "\n… (diff cut at "

// PullRequestDiff is pull request number's unified diff, at most max bytes
// and at most the 1 MiB read (the rest cut, and said so).
func (f *Forgejo) PullRequestDiff(ctx context.Context, owner, repo string, number int64, max int) (string, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d.diff", owner, repo, number), nil, http.StatusOK)
	if errors.Is(err, ErrAnswerTooLong) {
		n := min(max, len(body))
		return string(body[:n]) + diffCut + fmt.Sprintf("%d bytes of more than 1 MiB)", n), nil
	}
	if err != nil {
		return "", err
	}
	if len(body) > max {
		return string(body[:max]) + diffCut + fmt.Sprintf("%d of %d bytes)", max, len(body)), nil
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

// ErrUpdateConflict is an update of a pull request's branch whose base
// conflicts with it.
var ErrUpdateConflict = errors.New("the branch conflicts with its base")

// UpdatePullRequest merges the base into pull request number's head
// branch as the bot, which owns the fork the branch is on (Forgejo's
// repoUpdatePullRequest, style merge); a conflict answers 409, which do
// takes for transient.
func (f *Forgejo) UpdatePullRequest(ctx context.Context, owner, repo string, number int64) error {
	_, err := f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/update?style=merge", owner, repo, number), nil, http.StatusOK)
	if err != nil && strings.Contains(err.Error(), ": status 409: ") {
		return fmt.Errorf("%w: %v", ErrUpdateConflict, err)
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

// FileChange replaces one file's content; Blob is the sha it replaces,
// "" for a file it creates. Delete deletes the file instead.
type FileChange struct {
	Path    string
	Content []byte
	Blob    string
	Delete  bool
}

// CommitFiles writes changes as one commit on branch, or on newBranch
// created from branch when newBranch is set, as the bot, and returns the
// commit's sha.
func (f *Forgejo) CommitFiles(ctx context.Context, owner, repo, branch, newBranch, message string, changes []FileChange) (string, error) {
	files := make([]map[string]string, 0, len(changes))
	for _, c := range changes {
		op := "update"
		switch {
		case c.Delete:
			op = "delete"
		case c.Blob == "":
			op = "create"
		}
		files = append(files, map[string]string{"operation": op, "path": c.Path, "sha": c.Blob,
			"content": base64.StdEncoding.EncodeToString(c.Content)})
	}
	req := map[string]any{"branch": branch, "message": message, "files": files}
	if newBranch != "" {
		req["new_branch"] = newBranch
	}
	return postContents(ctx, f.call, Repo{Owner: owner, Name: repo}, req)
}

// CreateFiles commits files, keyed by path, as one commit on branch with
// token, every file created: into an empty repository too, whose branch
// it starts. It returns the commit's sha.
func (f *Forgejo) CreateFiles(ctx context.Context, token string, repo Repo, branch, message string, files map[string][]byte) (string, error) {
	ops := make([]map[string]string, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		ops = append(ops, map[string]string{"operation": "create", "path": path, "content": base64.StdEncoding.EncodeToString(files[path])})
	}
	return postContents(ctx, f.tokenCall(token), repo, map[string]any{"branch": branch, "message": message, "files": ops})
}

// postContents is repoChangeFiles: one commit of req's files.
func postContents(ctx context.Context, call func(context.Context, string, string, []byte, int) ([]byte, error), r Repo, req map[string]any) (string, error) {
	owner, repo := r.Owner, r.Name
	body, err := call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/contents", owner, repo), mustJSON(req), http.StatusCreated)
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

// MergedPullRequest is a merged pull request as the roll's body and the
// revert pass read it.
type MergedPullRequest struct {
	Number      int64
	Title       string
	MergeCommit string
	HeadSHA     string
	Base        string
	MergedAt    time.Time
}

// wireClosedPR is a closed pull request of Forgejo's list.
type wireClosedPR struct {
	Number      int64      `json:"number"`
	Title       string     `json:"title"`
	Merged      bool       `json:"merged"`
	MergedAt    *time.Time `json:"merged_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	MergeCommit string     `json:"merge_commit_sha"`
	Base        struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// closedPullRequests is page of repository's closed pull requests, the
// last updated first.
func (f *Forgejo) closedPullRequests(ctx context.Context, owner, repo string, page int) ([]wireClosedPR, error) {
	body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=closed&sort=recentupdate&limit=50&page=%d", owner, repo, page), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var prs []wireClosedPR
	if err := json.Unmarshal(body, &prs); err != nil {
		return nil, fmt.Errorf("forgejo pulls of %s/%s: %w", owner, repo, err)
	}
	return prs, nil
}

func (p wireClosedPR) merged() MergedPullRequest {
	m := MergedPullRequest{Number: p.Number, Title: p.Title, MergeCommit: p.MergeCommit, HeadSHA: p.Head.SHA, Base: p.Base.Ref}
	if p.MergedAt != nil {
		m.MergedAt = *p.MergedAt
	}
	return m
}

// RecentMergedPullRequests lists the merged ones of the closed pull
// requests updated last.
func (f *Forgejo) RecentMergedPullRequests(ctx context.Context, owner, repo string) ([]MergedPullRequest, error) {
	prs, err := f.closedPullRequests(ctx, owner, repo, 1)
	if err != nil {
		return nil, err
	}
	var out []MergedPullRequest
	for _, p := range prs {
		if p.Merged {
			out = append(out, p.merged())
		}
	}
	return out, nil
}

// MergedPullRequestsSince lists the pull requests merged since since,
// reading the closed ones page by page until one was last updated before
// it.
func (f *Forgejo) MergedPullRequestsSince(ctx context.Context, owner, repo string, since time.Time) ([]MergedPullRequest, error) {
	var out []MergedPullRequest
	for page := 1; ; page++ {
		prs, err := f.closedPullRequests(ctx, owner, repo, page)
		if err != nil || len(prs) == 0 {
			return out, err
		}
		for _, p := range prs {
			if p.UpdatedAt.Before(since) {
				return out, nil
			}
			if p.Merged && p.MergedAt != nil && !p.MergedAt.Before(since) {
				out = append(out, p.merged())
			}
		}
	}
}
