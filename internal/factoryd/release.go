package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Release is the stored state of a project's production hold, its row of
// releases.
type Release struct {
	Repository string
	// AsksAt is when the project's CI last asked /release (zero: never);
	// HeldSince the hold's clock (zero: not held); AskedAt the hold's last
	// ask.
	AsksAt, HeldSince, AskedAt time.Time
	// ToldHeld is the newest commit a promote job was told held; DeployNow
	// the commit an approver's Deploy now released.
	ToldHeld, DeployNow string
}

// Hold is what holds a project's production now: the groups open on main,
// since the clock's start.
type Hold struct {
	Since  time.Time
	Groups []Group
}

func (h Hold) Held() bool { return len(h.Groups) > 0 }

// Merged is how many of the group's children reached main, of total: every
// child but a closed one. A reverted child still counts as merged.
func (g Group) Merged() (merged, total int) {
	for status, n := range g.Children {
		if status != "closed" {
			total += n
		}
	}
	return g.Children["merged"] + g.Children["done"], total
}

// HoldReason names the plans that hold production.
func HoldReason(groups []Group) string {
	plans := make([]string, len(groups))
	for i, g := range groups {
		plans[i] = fmt.Sprintf("#%d", g.Issue)
	}
	if len(plans) == 1 {
		return "plan " + plans[0] + " is open on main"
	}
	return "plans " + strings.Join(plans[:len(plans)-1], ", ") + " and " + plans[len(plans)-1] + " are open on main"
}

// fixSources are the sources of the cards that fix production: one merged
// while held asks a person at once.
var fixSources = []string{SourceAlert, SourceMainFix, SourceChainRepair}

const releaseColumns = `repository, asks_at, held_since, asked_at, told_held, deploy_now`

func scanRelease(scan func(dest ...any) error) (Release, error) {
	var r Release
	var asks, held, asked sql.NullTime
	err := scan(&r.Repository, &asks, &held, &asked, &r.ToldHeld, &r.DeployNow)
	r.AsksAt, r.HeldSince, r.AskedAt = asks.Time, held.Time, asked.Time
	return r, err
}

func (s *PGStore) Release(ctx context.Context, repository string) (Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseColumns+` FROM releases WHERE repository = $1`, repository).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Release{Repository: repository}, nil
	}
	return r, err
}

func (s *PGStore) Releases(ctx context.Context) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PGStore) RecordReleaseAsk(ctx context.Context, repository, commit string, held bool, at time.Time) error {
	told := ""
	if held {
		told = commit
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases (repository, asks_at, told_held) VALUES ($1, $2, $3)
		ON CONFLICT (repository) DO UPDATE SET asks_at = EXCLUDED.asks_at,
			told_held = CASE WHEN $4 THEN EXCLUDED.told_held ELSE releases.told_held END`, repository, at, told, held)
	return err
}

func (s *PGStore) PutReleaseHold(ctx context.Context, repository string, since, asked, was time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases (repository, held_since, asked_at) VALUES ($1, $2, $3)
		ON CONFLICT (repository) DO UPDATE SET held_since = COALESCE(releases.held_since, EXCLUDED.held_since),
			asked_at = CASE WHEN releases.asked_at IS NOT DISTINCT FROM $4 THEN EXCLUDED.asked_at ELSE releases.asked_at END`,
		repository, nullTime(since), nullTime(asked), nullTime(was))
	return err
}

func (s *PGStore) SetDeployNow(ctx context.Context, repository, commit string, since time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases (repository, held_since, deploy_now) VALUES ($1, $2, $3)
		ON CONFLICT (repository) DO UPDATE SET deploy_now = EXCLUDED.deploy_now, held_since = EXCLUDED.held_since, asked_at = NULL`,
		repository, since, commit)
	return err
}

func (s *PGStore) ClearDeployNow(ctx context.Context, repository, commit string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE releases SET deploy_now = '' WHERE repository = $1 AND deploy_now = $2`, repository, commit)
	return err
}

func (s *PGStore) EndReleaseHold(ctx context.Context, repository string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE releases SET held_since = NULL, asked_at = NULL, deploy_now = '' WHERE repository = $1`, repository)
	return err
}

func (s *PGStore) ClearToldHeld(ctx context.Context, repository, commit string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE releases SET told_held = '' WHERE repository = $1 AND told_held = $2`, repository, commit)
	return err
}

func (s *PGStore) FixesMergedSince(ctx context.Context, repository string, since time.Time) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks
		CROSS JOIN LATERAL (SELECT COALESCE(min(e.at), CASE WHEN tasks.merge_commit <> '' THEN tasks.finished_at END) AS merged_at
			FROM events e WHERE e.task_id = tasks.id AND e.kind = 'pull_request_merged') m
		WHERE repository = $1 AND source = ANY($2) AND m.merged_at > $3
		ORDER BY m.merged_at, id`, repository, fixSources, since)
}
