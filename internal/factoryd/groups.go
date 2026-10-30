package factoryd

import (
	"context"
	"database/sql"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// GroupOf names card t's group: a plan's child belongs to its plan's
// group; every other card is a group of one, named by its own id.
func GroupOf(t Task) string {
	if t.Source == SourcePlan && t.Parent != "" {
		return t.Parent
	}
	return t.ID
}

// InPlan reports whether card t is a plan card or a plan's child, so its
// replan is its plan's.
func InPlan(t Task) bool {
	return t.Source == SourcePlan && t.Parent != "" || t.Track == policy.TrackPlan
}

// Group is an open plan with a card merged into main.
type Group struct {
	Plan       string
	Issue      int64
	Repository string
	// FirstMerge is when the group opened on main: its children's earliest
	// pull_request_merged event, else the earliest finish of a child with a
	// merge commit.
	FirstMerge time.Time
	// Children counts the plan's cards by status.
	Children map[string]int
}

func (s *PGStore) PlanClosed(ctx context.Context, plan string) (bool, error) {
	var closed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE task_id = $1 AND kind = 'plan_closed')`, plan).Scan(&closed)
	return closed, err
}

func (s *PGStore) OpenGroups(ctx context.Context) (map[string][]Group, error) {
	rows, err := s.db.QueryContext(ctx, `WITH merged AS (
			SELECT c.parent_task AS plan,
				COALESCE(min(e.at), min(c.finished_at) FILTER (WHERE c.merge_commit <> '')) AS first_merge
			FROM tasks c
			LEFT JOIN events e ON e.task_id = c.id AND e.kind = 'pull_request_merged'
			WHERE c.source = 'plan' AND c.plan_key IS NOT NULL AND (c.merge_commit <> '' OR e.id IS NOT NULL) AND c.reverted_at IS NULL
			GROUP BY c.parent_task
		)
		SELECT p.id, p.issue_number, p.repository, m.first_merge
		FROM merged m JOIN tasks p ON p.id = m.plan
		WHERE NOT EXISTS (SELECT 1 FROM events WHERE task_id = p.id AND kind = 'plan_closed')
		ORDER BY m.first_merge NULLS FIRST, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var open []Group
	for rows.Next() {
		var g Group
		var first sql.NullTime
		if err := rows.Scan(&g.Plan, &g.Issue, &g.Repository, &first); err != nil {
			return nil, err
		}
		g.FirstMerge, g.Children = first.Time, map[string]int{}
		open = append(open, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string][]Group{}
	if len(open) == 0 {
		return out, nil
	}
	plans := make([]string, len(open))
	byPlan := map[string]*Group{}
	for i := range open {
		plans[i], byPlan[open[i].Plan] = open[i].Plan, &open[i]
	}
	counts, err := s.db.QueryContext(ctx, `SELECT parent_task, status, count(*) FROM tasks
		WHERE parent_task = ANY($1) AND source = 'plan' AND plan_key IS NOT NULL GROUP BY 1, 2`, plans)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	for counts.Next() {
		var plan, status string
		var n int
		if err := counts.Scan(&plan, &status, &n); err != nil {
			return nil, err
		}
		byPlan[plan].Children[status] = n
	}
	if err := counts.Err(); err != nil {
		return nil, err
	}
	for _, g := range open {
		out[g.Repository] = append(out[g.Repository], g)
	}
	return out, nil
}
