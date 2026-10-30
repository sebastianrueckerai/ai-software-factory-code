package factoryd

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ReleaseAnswer is what GET /projects/{name}/release tells a promote job.
type ReleaseAnswer struct {
	Held   bool   `json:"held"`
	Reason string `json:"reason,omitempty"`
}

// ReleaseReader is the store ProjectReleaseHandler reads and records on.
type ReleaseReader interface {
	OpenGroups(ctx context.Context) (map[string][]Group, error)
	Release(ctx context.Context, repository string) (Release, error)
	RecordReleaseAsk(ctx context.Context, repository, commit string, held bool, at time.Time) error
}

var releaseCommitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ProjectReleaseHandler serves GET /projects/{name}/release?commit=: the
// bearer is the project's FACTORY_READ_TOKEN (401 otherwise, an unknown
// project too). Production is held while a plan's group is open on the
// project's repository, unless an approver released this commit with Deploy
// now. The ask is recorded; a failed record never fails the answer.
func ProjectReleaseHandler(projects ProjectReader, store ReleaseReader, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, commit := r.PathValue("name"), r.URL.Query().Get("commit")
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok, err := projects.Project(r.Context(), name)
		if err != nil {
			log.Printf("project %q release: %v", name, err) // #nosec G706 -- %q escapes every control character
			http.Error(w, "cannot read the project", http.StatusInternalServerError)
			return
		}
		if !ok || !p.ReadTokenMatches(token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !releaseCommitRe.MatchString(commit) {
			http.Error(w, "commit must be the run's commit sha", http.StatusBadRequest)
			return
		}
		all, err := store.OpenGroups(r.Context())
		var rel Release
		if err == nil && len(all[p.Repository]) > 0 {
			rel, err = store.Release(r.Context(), p.Repository)
		}
		if err != nil {
			log.Printf("project %q release: %v", name, err) // #nosec G706 -- %q escapes every control character
			http.Error(w, "cannot read the hold", http.StatusInternalServerError)
			return
		}
		groups := all[p.Repository]
		held := len(groups) > 0 && commit != rel.DeployNow
		answer := ReleaseAnswer{Held: held}
		if held {
			answer.Reason = HoldReason(groups)
		}
		body, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(body) // #nosec G104 -- best-effort response write
		if err := store.RecordReleaseAsk(context.WithoutCancel(r.Context()), p.Repository, commit, held, now()); err != nil {
			log.Printf("project %q release ask: %v", name, err) // #nosec G706 -- %q escapes every control character
		}
	})
}
