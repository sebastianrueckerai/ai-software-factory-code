package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// Forgejo calls for a project's objects (Task 65): the projects token for
// the repository, webhook, collaborators and protection; a project's own
// factory-ci token (limited to its repository) for Actions secrets and
// runners; the site admin's basic auth, at registration only, to mint
// those tokens and to make factory-projects an admin of a repository a
// human created.

func (f *Forgejo) projectsCall(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
	return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
		r.Header.Set("Authorization", "token "+f.ProjectsToken)
	})
}

func (f *Forgejo) tokenCall(token string) func(context.Context, string, string, []byte, int) ([]byte, error) {
	return func(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
		return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) {
			r.Header.Set("Authorization", "token "+token)
		})
	}
}

// AdminAuth is the Forgejo site admin's login, given to register-project
// at run time and never stored.
type AdminAuth struct{ User, Password string }

func (f *Forgejo) adminCall(a AdminAuth) func(context.Context, string, string, []byte, int) ([]byte, error) {
	return func(ctx context.Context, method, path string, payload []byte, wantStatus int) ([]byte, error) {
		return f.do(ctx, method, path, payload, wantStatus, func(r *http.Request) { r.SetBasicAuth(a.User, a.Password) })
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only maps and structs of plain fields reach here
	}
	return b
}

// ProjectRepoAccess reads whether the projects token sees repo and
// administers it; a repository it cannot see answers not found.
func (f *Forgejo) ProjectRepoAccess(ctx context.Context, repo Repo) (exists, admin bool, err error) {
	body, err := f.projectsCall(ctx, http.MethodGet, "/api/v1/repos/"+repo.String(), nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var res struct {
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return false, false, fmt.Errorf("forgejo repository %s: %w", repo, err)
	}
	return true, res.Permissions.Admin, nil
}

// CreateProjectRepo creates repo, private, with an initial commit on main;
// its creator factory-projects becomes its admin.
func (f *Forgejo) CreateProjectRepo(ctx context.Context, repo Repo) error {
	_, err := f.projectsCall(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(repo.Owner)+"/repos",
		mustJSON(map[string]any{"name": repo.Name, "private": true, "auto_init": true, "default_branch": "main"}), http.StatusCreated)
	return err
}

// Hook is a repository webhook as far as registration keeps it.
type Hook struct {
	ID     int64    `json:"id"`
	Type   string   `json:"type"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	} `json:"config"`
}

func (f *Forgejo) ProjectHooks(ctx context.Context, repo Repo) ([]Hook, error) {
	return GetPages[Hook](ctx, f.projectsCall, "/api/v1/repos/"+repo.String()+"/hooks?")
}

// SaveProjectHook creates the webhook (id 0) or edits hook id to it.
func (f *Forgejo) SaveProjectHook(ctx context.Context, repo Repo, id int64, hookURL, secret string, events []string) error {
	opt := map[string]any{"active": true, "branch_filter": "*", "events": events,
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret}}
	if id == 0 {
		opt["type"] = "forgejo"
		_, err := f.projectsCall(ctx, http.MethodPost, "/api/v1/repos/"+repo.String()+"/hooks", mustJSON(opt), http.StatusCreated)
		return err
	}
	_, err := f.projectsCall(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/hooks/%d", repo, id), mustJSON(opt), http.StatusOK)
	return err
}

// CollaboratorPermission is user's permission on repo as a collaborator,
// "" when it is none.
func (f *Forgejo) CollaboratorPermission(ctx context.Context, repo Repo, user string) (string, error) {
	return collaboratorPermission(ctx, f.projectsCall, repo, user)
}

// AdminCollaboratorPermission is CollaboratorPermission read with the site
// admin's login.
func (f *Forgejo) AdminCollaboratorPermission(ctx context.Context, a AdminAuth, repo Repo, user string) (string, error) {
	return collaboratorPermission(ctx, f.adminCall(a), repo, user)
}

func collaboratorPermission(ctx context.Context, call func(context.Context, string, string, []byte, int) ([]byte, error), repo Repo, user string) (string, error) {
	body, err := call(ctx, http.MethodGet, "/api/v1/repos/"+repo.String()+"/collaborators/"+url.PathEscape(user)+"/permission", nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var res struct {
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("forgejo permission of %s on %s: %w", user, repo, err)
	}
	return res.Permission, nil
}

func addCollaborator(ctx context.Context, call func(context.Context, string, string, []byte, int) ([]byte, error), repo Repo, user, permission string) error {
	_, err := call(ctx, http.MethodPut, "/api/v1/repos/"+repo.String()+"/collaborators/"+url.PathEscape(user),
		mustJSON(map[string]string{"permission": permission}), http.StatusNoContent)
	return err
}

func (f *Forgejo) AddProjectCollaborator(ctx context.Context, repo Repo, user, permission string) error {
	return addCollaborator(ctx, f.projectsCall, repo, user, permission)
}

// BranchProtection is the part of a branch protection registration sets.
type BranchProtection struct {
	EnablePush                  bool     `json:"enable_push"`
	EnablePushWhitelist         bool     `json:"enable_push_whitelist"`
	PushWhitelistUsernames      []string `json:"push_whitelist_usernames"`
	EnableMergeWhitelist        bool     `json:"enable_merge_whitelist"`
	MergeWhitelistUsernames     []string `json:"merge_whitelist_usernames"`
	EnableApprovalsWhitelist    bool     `json:"enable_approvals_whitelist"`
	ApprovalsWhitelistUsernames []string `json:"approvals_whitelist_username"`
	RequiredApprovals           int      `json:"required_approvals"`
	ApplyToAdmins               bool     `json:"apply_to_admins"`
	BlockOnRejectedReviews      bool     `json:"block_on_rejected_reviews"`
	DismissStaleApprovals       bool     `json:"dismiss_stale_approvals"`
	EnableStatusCheck           bool     `json:"enable_status_check"`
	ProtectedFilePatterns       string   `json:"protected_file_patterns"`
	UnprotectedFilePatterns     string   `json:"unprotected_file_patterns"`
}

func (b BranchProtection) equal(o BranchProtection) bool {
	norm := func(p BranchProtection) BranchProtection {
		for _, l := range []*[]string{&p.PushWhitelistUsernames, &p.MergeWhitelistUsernames, &p.ApprovalsWhitelistUsernames} {
			*l = slices.Sorted(slices.Values(nonNil(*l)))
		}
		return p
	}
	a, c := norm(b), norm(o)
	return slices.Equal(a.PushWhitelistUsernames, c.PushWhitelistUsernames) &&
		slices.Equal(a.MergeWhitelistUsernames, c.MergeWhitelistUsernames) &&
		slices.Equal(a.ApprovalsWhitelistUsernames, c.ApprovalsWhitelistUsernames) &&
		a.EnablePush == c.EnablePush && a.EnablePushWhitelist == c.EnablePushWhitelist &&
		a.EnableMergeWhitelist == c.EnableMergeWhitelist && a.EnableApprovalsWhitelist == c.EnableApprovalsWhitelist &&
		a.RequiredApprovals == c.RequiredApprovals && a.ApplyToAdmins == c.ApplyToAdmins &&
		a.BlockOnRejectedReviews == c.BlockOnRejectedReviews && a.DismissStaleApprovals == c.DismissStaleApprovals &&
		a.EnableStatusCheck == c.EnableStatusCheck && a.ProtectedFilePatterns == c.ProtectedFilePatterns &&
		a.UnprotectedFilePatterns == c.UnprotectedFilePatterns
}

// ProjectBranchProtection reads branch's protection; false when it has none.
func (f *Forgejo) ProjectBranchProtection(ctx context.Context, repo Repo, branch string) (BranchProtection, bool, error) {
	body, err := f.projectsCall(ctx, http.MethodGet, "/api/v1/repos/"+repo.String()+"/branch_protections/"+url.PathEscape(branch), nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return BranchProtection{}, false, nil
	}
	if err != nil {
		return BranchProtection{}, false, err
	}
	var b BranchProtection
	if err := json.Unmarshal(body, &b); err != nil {
		return BranchProtection{}, false, fmt.Errorf("forgejo protection of %s %s: %w", repo, branch, err)
	}
	return b, true, nil
}

// SaveProjectBranchProtection creates branch's protection or edits it to b.
func (f *Forgejo) SaveProjectBranchProtection(ctx context.Context, repo Repo, branch string, b BranchProtection, exists bool) error {
	if !exists {
		opt := map[string]any{}
		json.Unmarshal(mustJSON(b), &opt) // #nosec G104 -- round trip of a struct just marshalled
		opt["rule_name"] = branch
		_, err := f.projectsCall(ctx, http.MethodPost, "/api/v1/repos/"+repo.String()+"/branch_protections", mustJSON(opt), http.StatusCreated)
		return err
	}
	_, err := f.projectsCall(ctx, http.MethodPatch, "/api/v1/repos/"+repo.String()+"/branch_protections/"+url.PathEscape(branch), mustJSON(b), http.StatusOK)
	return err
}

// ActionSecretNames lists repo's Actions secrets by name, with the
// project's CI token.
func (f *Forgejo) ActionSecretNames(ctx context.Context, token string, repo Repo) ([]string, error) {
	secrets, err := GetPages[struct {
		Name string `json:"name"`
	}](ctx, f.tokenCall(token), "/api/v1/repos/"+repo.String()+"/actions/secrets?")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range secrets {
		names = append(names, s.Name)
	}
	return names, nil
}

// PutActionSecret creates or replaces repo's Actions secret name.
func (f *Forgejo) PutActionSecret(ctx context.Context, token string, repo Repo, name, value string) error {
	_, err := f.tokenCall(token)(ctx, http.MethodPut, "/api/v1/repos/"+repo.String()+"/actions/secrets/"+url.PathEscape(name),
		mustJSON(map[string]string{"data": value}), anySuccess)
	return err
}

// AdminAddCollaborator makes user a collaborator on repo with the site
// admin's login.
func (f *Forgejo) AdminAddCollaborator(ctx context.Context, a AdminAuth, repo Repo, user, permission string) error {
	return addCollaborator(ctx, f.adminCall(a), repo, user, permission)
}

// AdminRepoExists reads whether repo exists, with the site admin's login.
func (f *Forgejo) AdminRepoExists(ctx context.Context, a AdminAuth, repo Repo) (bool, error) {
	_, err := f.adminCall(a)(ctx, http.MethodGet, "/api/v1/repos/"+repo.String(), nil, http.StatusOK)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// AdminMintToken revokes user's tokens named name and mints a fresh one
// with scopes, limited to repos when given, as the site admin acting as
// user.
func (f *Forgejo) AdminMintToken(ctx context.Context, a AdminAuth, user, name string, scopes []string, repos []Repo) (string, error) {
	call := f.adminCall(a)
	tokens, err := GetPages[TokenRef](ctx, call, "/api/v1/users/"+url.PathEscape(user)+"/tokens?sudo="+url.QueryEscape(user)+"&")
	if err != nil {
		return "", err
	}
	for _, t := range tokens {
		if t.Name != name {
			continue
		}
		if _, err := call(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/users/%s/tokens/%d?sudo=%s", url.PathEscape(user), t.ID, url.QueryEscape(user)),
			nil, http.StatusNoContent); err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	opt := map[string]any{"name": name, "scopes": scopes}
	if len(repos) > 0 {
		opt["repositories"] = repos
	}
	body, err := call(ctx, http.MethodPost, "/api/v1/users/"+url.PathEscape(user)+"/tokens?sudo="+url.QueryEscape(user), mustJSON(opt), http.StatusCreated)
	if err != nil {
		return "", err
	}
	var res struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.SHA1 == "" {
		return "", fmt.Errorf("forgejo mint %s for %s: no token in the answer (%v)", name, user, err)
	}
	return res.SHA1, nil
}
