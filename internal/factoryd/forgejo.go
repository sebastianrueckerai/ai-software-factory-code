package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Repo names one repository a per-task Forgejo token is scoped to.
type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// Forgejo mints and revokes the per-task git tokens (architecture 6.3,
// research Q3), authenticating as the bot user with basic auth — the live
// wire (Forgejo 15 / gitea 1.22): /users/{u}/tokens refuses token auth for
// minting ("auth method not allowed") and the /admin/users mint path does
// not exist. Tokens are repo-scoped, write:repository, and deleted at task
// end, so a leaked token can push exactly one repository for exactly one
// task.
type Forgejo struct {
	BaseURL     string // e.g. https://git.aisoftwarefactory.world
	BotUser     string
	BotPassword string
	HTTP        *http.Client // nil = a mint-sized timeout

	// CIRegistrarToken authenticates the Actions runner-registration API
	// (task 41, ruling 78): a token scoped to registering and deleting
	// ephemeral runners and reading the waiting-job queue, never repository
	// or registry credentials.
	CIRegistrarToken string
}

// ForgejoToken is a minted token. Secret is returned once by Forgejo and
// never persisted by the factory beyond the task secret.
type ForgejoToken struct {
	ID     int64
	Secret string
}

func (f *Forgejo) MintToken(ctx context.Context, name string, repos []Repo) (ForgejoToken, error) {
	var tok ForgejoToken
	if len(repos) == 0 {
		return tok, fmt.Errorf("forgejo mint %s: at least one repository is required", name)
	}
	payload, err := json.Marshal(map[string]any{
		"name":         name,
		"scopes":       []string{"write:repository"},
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
	body, err := f.call(ctx, http.MethodGet,
		fmt.Sprintf("/api/v1/users/%s/tokens", f.BotUser), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var refs []TokenRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, fmt.Errorf("forgejo list tokens: %w", err)
	}
	return refs, nil
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
// the label must not inflate the spawn count. The spawner uses the count,
// never the identity of a specific job: forgejo-runner registers by label
// only and the server itself — not the caller — assigns whichever waiting
// job a given runner instance ends up serving.
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
