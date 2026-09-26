package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Repo names one repository a per-task Forgejo token is scoped to.
type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// Forgejo mints and revokes the per-task git tokens through Forgejo's admin
// API: repo-scoped (Forgejo v15+), write:repository, deleted at task end so
// a leaked token can push exactly one repository for exactly one task
// (architecture 6.3, research Q3).
type Forgejo struct {
	BaseURL    string // e.g. https://git.aisoftwarefactory.world
	AdminToken string
	HTTP       *http.Client // nil = a mint-sized timeout
}

// ForgejoToken is a minted token. Secret is returned once by Forgejo and
// never persisted by the factory beyond the task secret.
type ForgejoToken struct {
	ID     int64
	Secret string
}

func (f *Forgejo) MintToken(ctx context.Context, botUser, name string, repos []Repo) (ForgejoToken, error) {
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
		fmt.Sprintf("/api/v1/admin/users/%s/tokens", botUser), payload, http.StatusCreated)
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

// ListTokens lists the bot user's tokens (admin scope). The runner uses it
// to reconcile tokens by name: completion and the boot sweep must revoke a
// task's token even when the mint's ID was lost to a crash.
func (f *Forgejo) ListTokens(ctx context.Context, botUser string) ([]TokenRef, error) {
	body, err := f.call(ctx, http.MethodGet, "/api/v1/users/"+botUser+"/tokens", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var refs []TokenRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, fmt.Errorf("forgejo list tokens: %w", err)
	}
	return refs, nil
}

func (f *Forgejo) RevokeToken(ctx context.Context, botUser string, id int64) error {
	_, err := f.call(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v1/admin/users/%s/tokens/%d", botUser, id), nil, http.StatusNoContent)
	return err
}

func (f *Forgejo) call(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+f.AdminToken)
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
		return nil, fmt.Errorf("forgejo %s %s: status %d: %s", method, path, res.StatusCode, body)
	}
	return body, nil
}
