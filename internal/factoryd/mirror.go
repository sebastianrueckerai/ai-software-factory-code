package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// MirrorExport is a project's push mirror to GitHub, kept by Forgejo.
const MirrorExport = "export"

// Mirror is a project's project_mirrors row: Remote is the GitHub
// repository, owner/name; State the registrar's reading of its last sync.
// Token is set only by MirrorToken, the registrar's read.
type Mirror struct {
	Project, Direction, Remote, Username, Token, State, LastError string
	LastUpdate                                                    time.Time
}

// PutMirror stores m as its project's mirror of its direction, a new
// remote and token with no sync yet.
func (s *PGStore) PutMirror(ctx context.Context, m Mirror) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO project_mirrors (project, direction, remote, username, token, state)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project, direction) DO UPDATE SET remote = $3, username = $4, token = $5, state = $6,
			last_error = '', last_update = NULL, updated_at = now()`,
		m.Project, m.Direction, m.Remote, m.Username, m.Token, m.State)
	return err
}

// Mirror is project's mirror of direction without its token.
func (s *PGStore) Mirror(ctx context.Context, project, direction string) (Mirror, bool, error) {
	return s.mirror(ctx, project, direction, `''`)
}

// MirrorToken is project's mirror of direction with its token: the
// registrar's read, never a handler's or a tool's.
func (s *PGStore) MirrorToken(ctx context.Context, project, direction string) (Mirror, bool, error) {
	return s.mirror(ctx, project, direction, `token`)
}

func (s *PGStore) mirror(ctx context.Context, project, direction, token string) (Mirror, bool, error) {
	m := Mirror{Project: project, Direction: direction}
	var last sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT remote, username, `+token+`, state, last_error, last_update
		FROM project_mirrors WHERE project = $1 AND direction = $2`, project, direction).
		Scan(&m.Remote, &m.Username, &m.Token, &m.State, &m.LastError, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return Mirror{}, false, nil
	}
	m.LastUpdate = last.Time
	return m, err == nil, err
}

// SetMirrorState records the registrar's reading of project's mirror of
// direction.
func (s *PGStore) SetMirrorState(ctx context.Context, project, direction, state, lastError string, lastUpdate time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE project_mirrors SET state = $3, last_error = $4, last_update = $5, updated_at = now()
		WHERE project = $1 AND direction = $2`, project, direction, state, lastError, sql.NullTime{Time: lastUpdate, Valid: !lastUpdate.IsZero()})
	return err
}

// DeleteMirror forgets project's mirror of direction.
func (s *PGStore) DeleteMirror(ctx context.Context, project, direction string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_mirrors WHERE project = $1 AND direction = $2`, project, direction)
	return err
}

// MirrorStore is what the registrar reads and writes of the mirror rows
// (PGStore).
type MirrorStore interface {
	MirrorToken(ctx context.Context, project, direction string) (Mirror, bool, error)
	SetMirrorState(ctx context.Context, project, direction, state, lastError string, lastUpdate time.Time) error
}

// GitHubAddress is the push mirror address of GitHub repository remote,
// owner/name.
func GitHubAddress(remote string) string { return "https://github.com/" + remote + ".git" }

// mirrorStates records the state of every project's export: "synced
// <ago>" after a clean sync, "failed: <the error's first line>" after a
// failed one or with no push mirror on Forgejo, the row's own before the
// first sync. The token is taken out of Forgejo's error.
func (r *Registrar) mirrorStates(ctx context.Context) {
	if r.Mirrors == nil {
		return
	}
	ps, err := r.Store.Projects(ctx)
	if err != nil {
		log.Printf("projects: mirrors: %v", err)
		return
	}
	for _, p := range ps {
		m, ok, err := r.Mirrors.MirrorToken(ctx, p.Name, MirrorExport)
		var pms []PushMirror
		if ok && err == nil {
			pms, err = r.Forgejo.PushMirrors(ctx, p.Repo())
		}
		if !ok || err != nil {
			if err != nil {
				log.Printf("project %s: github export: %v", p.Name, err)
			}
			continue
		}
		state, lastErr, last := "failed: Forgejo holds no push mirror to "+m.Remote, "", time.Time{}
		if i := slices.IndexFunc(pms, func(pm PushMirror) bool { return strings.EqualFold(pm.RemoteAddress, GitHubAddress(m.Remote)) }); i >= 0 {
			lastErr, last, state = strings.ReplaceAll(pms[i].LastError, m.Token, "[token]"), pms[i].LastUpdate, m.State
			first, _, _ := strings.Cut(lastErr, "\n")
			switch {
			case lastErr != "":
				state = "failed: " + textutil.Truncate(first, 120)
			case !last.IsZero():
				state = "synced " + ago(time.Since(last))
			}
		}
		if err := r.Mirrors.SetMirrorState(ctx, p.Name, MirrorExport, state, lastErr, last); err != nil {
			log.Printf("project %s: github export: %v", p.Name, err)
		}
	}
}

// ago is d as the status line says it.
func ago(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h ago", int(d.Hours()))
}
