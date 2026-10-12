package factoryd

import (
	"cmp"
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

const (
	// MirrorExport is a project's push mirror to GitHub, kept by Forgejo.
	MirrorExport = "export"
	// MirrorImport is a new project's GitHub repository, which
	// registration copies into its Forgejo repository once; its token,
	// empty for a public one, is forgotten after the copy. Its state is
	// pending, copying since the copy's start, then imported or failed;
	// its last update the copy's start, then an import's end.
	MirrorImport = "import"
	// ModeMirror is an export Forgejo keeps as a push mirror of every
	// branch and tag; ModePullRequests one that takes each card merged
	// since to GitHub as a pull request (githubPass).
	ModeMirror       = "mirror"
	ModePullRequests = "pull_requests"
)

// Mirror is a project's project_mirrors row: Remote is the GitHub
// repository, owner/name; State the registrar's reading of its last sync.
// Token is set only by MirrorToken, the registrar's and the chat export's
// read. Mode is an export's; Since when the row was put with its remote
// and mode; Synced the GitHub default branch's head the last GitHubSync
// brought back, SyncResult what the last one answered.
type Mirror struct {
	Project, Direction, Remote, Username, Token, State, LastError, Mode, Synced, SyncResult string
	LastUpdate, Since                                                                       time.Time
}

// PutMirror stores m as its project's mirror of its direction, a new
// token and mode (default ModeMirror) with no sync yet; a row of the same
// remote and mode keeps its since and its last GitHubSync.
func (s *PGStore) PutMirror(ctx context.Context, m Mirror) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO project_mirrors (project, direction, remote, username, token, state, mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project, direction) DO UPDATE SET remote = $3, username = $4, token = $5, state = $6, mode = $7,
			last_error = '', last_update = NULL, updated_at = now(),
			since = CASE WHEN project_mirrors.remote = $3 AND project_mirrors.mode = $7 THEN project_mirrors.since ELSE now() END,
			synced = CASE WHEN project_mirrors.remote = $3 AND project_mirrors.mode = $7 THEN project_mirrors.synced ELSE '' END,
			sync_result = CASE WHEN project_mirrors.remote = $3 AND project_mirrors.mode = $7 THEN project_mirrors.sync_result ELSE '' END`,
		m.Project, m.Direction, m.Remote, m.Username, m.Token, m.State, cmp.Or(m.Mode, ModeMirror))
	return err
}

// Mirror is project's mirror of direction without its token.
func (s *PGStore) Mirror(ctx context.Context, project, direction string) (Mirror, bool, error) {
	return s.mirror(ctx, project, direction, `''`)
}

// MirrorToken is project's mirror of direction with its token: the
// registrar's read and the chat export's, which pushes with the token the
// projects page's form stored; never a page's or a tool answer's.
func (s *PGStore) MirrorToken(ctx context.Context, project, direction string) (Mirror, bool, error) {
	return s.mirror(ctx, project, direction, `token`)
}

func (s *PGStore) mirror(ctx context.Context, project, direction, token string) (Mirror, bool, error) {
	m := Mirror{Project: project, Direction: direction}
	var last sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT remote, username, `+token+`, state, last_error, last_update, mode, synced, sync_result, since
		FROM project_mirrors WHERE project = $1 AND direction = $2`, project, direction).
		Scan(&m.Remote, &m.Username, &m.Token, &m.State, &m.LastError, &last, &m.Mode, &m.Synced, &m.SyncResult, &m.Since)
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

// SetMirrorToken replaces the token of project's mirror of direction.
func (s *PGStore) SetMirrorToken(ctx context.Context, project, direction, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE project_mirrors SET token = $3, updated_at = now() WHERE project = $1 AND direction = $2`,
		project, direction, token)
	return err
}

// SetMirrorSynced records sha as the GitHub head project's export last
// brought back and result as what the last GitHubSync answered.
func (s *PGStore) SetMirrorSynced(ctx context.Context, project, sha, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE project_mirrors SET synced = $2, sync_result = $3, updated_at = now()
		WHERE project = $1 AND direction = 'export'`, project, sha, result)
	return err
}

// GitHubCards are repository's done cards with a pull request that
// finished since since and have no GitHub pull request yet or an open
// one, oldest first.
func (s *PGStore) GitHubCards(ctx context.Context, repository string, since time.Time) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE repository = $1 AND status = 'done' AND pr_number > 0
		AND finished_at >= $2 AND github_pr_state IN ('', 'open') ORDER BY finished_at, id`, repository, since)
}

// SetGitHubPR records task id's GitHub pull request and its state.
func (s *PGStore) SetGitHubPR(ctx context.Context, id string, number int64, url, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET github_pr = $2, github_pr_url = $3, github_pr_state = $4 WHERE id = $1`,
		id, number, url, state)
	return oneRow(res, err, "task "+id)
}

// ClearGitHubFailures empties the failed state of repository's cards
// without a GitHub pull request: each export put gives them one more try.
func (s *PGStore) ClearGitHubFailures(ctx context.Context, repository string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET github_pr_state = '' WHERE repository = $1 AND github_pr = 0 AND github_pr_state LIKE 'failed:%'`,
		repository)
	return err
}

// DeleteMirror forgets project's mirror of direction.
func (s *PGStore) DeleteMirror(ctx context.Context, project, direction string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_mirrors WHERE project = $1 AND direction = $2`, project, direction)
	return err
}

// DeleteStaleImports deletes the import rows still pending, copying or
// failed, last written before before, of projects not in files, and
// answers them without their tokens, their state its first word.
func (s *PGStore) DeleteStaleImports(ctx context.Context, files []string, before time.Time) ([]Mirror, error) {
	rows, err := s.db.QueryContext(ctx, `DELETE FROM project_mirrors WHERE direction = 'import' 
		AND (state = 'pending' OR state LIKE 'copying %' OR state LIKE 'failed:%') AND updated_at < $1 AND project <> ALL($2::text[])
		RETURNING project, remote, rtrim(split_part(state, ' ', 1), ':')`, before, files)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gone []Mirror
	for rows.Next() {
		m := Mirror{Direction: MirrorImport}
		if err := rows.Scan(&m.Project, &m.Remote, &m.State); err != nil {
			return nil, err
		}
		gone = append(gone, m)
	}
	return gone, rows.Err()
}

// MirrorStore is what the registrar reads and writes of the mirror rows
// (PGStore).
type MirrorStore interface {
	MirrorToken(ctx context.Context, project, direction string) (Mirror, bool, error)
	SetMirrorState(ctx context.Context, project, direction, state, lastError string, lastUpdate time.Time) error
	SetMirrorToken(ctx context.Context, project, direction, token string) error
	DeleteStaleImports(ctx context.Context, files []string, before time.Time) ([]Mirror, error)
	SetMirrorSynced(ctx context.Context, project, sha, result string) error
	GitHubCards(ctx context.Context, repository string, since time.Time) ([]Task, error)
	SetGitHubPR(ctx context.Context, id string, number int64, url, state string) error
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
		ok = ok && m.Mode != ModePullRequests
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
