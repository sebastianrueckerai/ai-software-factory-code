package factoryd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type releaseStore struct {
	groups    map[string][]Group
	groupsErr error
	rows      map[string]Release
	asked     []Release
	askErr    error
}

func (s *releaseStore) OpenGroups(context.Context) (map[string][]Group, error) {
	return s.groups, s.groupsErr
}

func (s *releaseStore) Release(_ context.Context, repository string) (Release, error) {
	r := s.rows[repository]
	r.Repository = repository
	return r, nil
}

func (s *releaseStore) RecordReleaseAsk(_ context.Context, repository, commit string, held bool, at time.Time) error {
	r := s.rows[repository]
	r.Repository, r.AsksAt = repository, at
	if held {
		r.ToldHeld = commit
	}
	if s.rows == nil {
		s.rows = map[string]Release{}
	}
	s.rows[repository] = r
	s.asked = append(s.asked, r)
	return s.askErr
}

type failingProjects struct{ ProjectReader }

func (failingProjects) Project(context.Context, string) (Project, bool, error) {
	return Project{}, false, errors.New("db down")
}

func routed(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /projects/{name}/release", h)
	return mux
}

func releaseRig(t *testing.T) (*releaseStore, http.Handler, string, time.Time) {
	t.Helper()
	projects := &memProjects{m: map[string]Project{}}
	token, hash := NewReadToken()
	p := demoProject()
	p.ReadTokenHash = hash
	projects.m[p.Name] = p
	projects.m["other"] = Project{Name: "other", ReadTokenHash: HashToken("other-token")}
	store := &releaseStore{rows: map[string]Release{}}
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	return store, routed(ProjectReleaseHandler(projects, store, func() time.Time { return at })), token, at
}

func TestProjectReleaseAuth(t *testing.T) {
	store, h, token, _ := releaseRig(t)
	wrong := token[:63] + "0"
	if wrong == token {
		wrong = token[:63] + "1"
	}
	for name, tc := range map[string]struct {
		path, token string
		code        int
	}{
		"no bearer":         {"/projects/demo-app/release?commit=abc1234", "", http.StatusUnauthorized},
		"wrong token":       {"/projects/demo-app/release?commit=abc1234", wrong, http.StatusUnauthorized},
		"another project's": {"/projects/demo-app/release?commit=abc1234", "other-token", http.StatusUnauthorized},
		"unknown project":   {"/projects/nope/release?commit=abc1234", token, http.StatusUnauthorized},
		"no commit":         {"/projects/demo-app/release", token, http.StatusBadRequest},
		"bad commit":        {"/projects/demo-app/release?commit=xyz", token, http.StatusBadRequest},
		"upper-case commit": {"/projects/demo-app/release?commit=ABC1234", token, http.StatusBadRequest},
		"own token":         {"/projects/demo-app/release?commit=abc1234", token, http.StatusOK},
	} {
		if rec := getAlerts(h, tc.path, tc.token); rec.Code != tc.code {
			t.Errorf("%s: %d, want %d: %s", name, rec.Code, tc.code, rec.Body)
		}
	}
	if len(store.asked) != 1 {
		t.Errorf("%d asks recorded, want only the one 200's", len(store.asked))
	}

	failing := routed(ProjectReleaseHandler(failingProjects{}, store, time.Now))
	if rec := getAlerts(failing, "/projects/demo-app/release?commit=abc1234", token); rec.Code != 500 || !strings.Contains(rec.Body.String(), "cannot read the project") {
		t.Errorf("failing project read: %d %q", rec.Code, rec.Body)
	}
	store.groupsErr = errors.New("db down")
	if rec := getAlerts(h, "/projects/demo-app/release?commit=abc1234", token); rec.Code != 500 || !strings.Contains(rec.Body.String(), "cannot read the hold") {
		t.Errorf("failing hold read: %d %q", rec.Code, rec.Body)
	}
}

func TestProjectReleaseAnswers(t *testing.T) {
	const repo = "demo-app/demo-app"
	store, h, token, at := releaseRig(t)
	ask := func(commit string) string {
		t.Helper()
		rec := getAlerts(h, "/projects/demo-app/release?commit="+commit, token)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%d %v", rec.Code, rec.Header())
		}
		return strings.TrimSpace(rec.Body.String())
	}

	if got := ask("abc1234"); got != `{"held":false}` {
		t.Errorf("no plan open: %s", got)
	}
	if r := store.rows[repo]; !r.AsksAt.Equal(at) || r.ToldHeld != "" {
		t.Errorf("row after a free ask: %+v", r)
	}

	store.groups = map[string][]Group{repo: {{Plan: "p1", Issue: 12, Repository: repo}}, "sf/other": {{Plan: "p9", Issue: 99}}}
	if got := ask("abc1234"); got != `{"held":true,"reason":"plan #12 is open on main"}` {
		t.Errorf("one plan: %s", got)
	}
	if r := store.rows[repo]; r.ToldHeld != "abc1234" {
		t.Errorf("ToldHeld %q, want abc1234", r.ToldHeld)
	}

	store.groups[repo] = append(store.groups[repo], Group{Plan: "p2", Issue: 15, Repository: repo})
	if got := ask("abc1234"); got != `{"held":true,"reason":"plans #12 and #15 are open on main"}` {
		t.Errorf("two plans: %s", got)
	}

	r := store.rows[repo]
	r.DeployNow = "abc1234"
	store.rows[repo] = r
	if got := ask("abc1234"); got != `{"held":false}` {
		t.Errorf("deploy-now commit: %s", got)
	}
	if got := store.rows[repo].ToldHeld; got != "abc1234" {
		t.Errorf("ToldHeld %q after a released ask, want it left", got)
	}
	if got := ask("def5678"); got != `{"held":true,"reason":"plans #12 and #15 are open on main"}` {
		t.Errorf("other commit: %s", got)
	}
	if got := store.rows[repo].ToldHeld; got != "def5678" {
		t.Errorf("ToldHeld %q, want def5678", got)
	}

	store.groups = map[string][]Group{"sf/other": {{Plan: "p9", Issue: 99}}}
	if got := ask("abc1234"); got != `{"held":false}` {
		t.Errorf("plan in another repository: %s", got)
	}

	store.askErr = errors.New("db down")
	if got := ask("abc1234"); got != `{"held":false}` {
		t.Errorf("failed record: %s", got)
	}
}
