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

func (s *PGStore) PlanClosed(ctx context.Context, plan string) (time.Time, bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT min(at) FROM events WHERE task_id = $1 AND kind = 'plan_closed'`, plan).Scan(&at)
	return at.Time, at.Valid, err
}

// QueuedPlans are the issues of repository's plans with a card in the
// merge queue, in review and holding an entry or waiting in gate
// merge-queue: its merge would open the plan's group on main. A card
// merged by hand keeps its gate but has left the queue.
func (s *PGStore) QueuedPlans(ctx context.Context, repository string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT p.issue_number FROM tasks c JOIN tasks p ON p.id = c.parent_task
		WHERE c.repository = $1 AND c.source = 'plan' AND c.plan_key IS NOT NULL AND c.status = 'in-review'
			AND (c.gate = $2 OR EXISTS (SELECT 1 FROM merge_queue q WHERE q.task_id = c.id))
		ORDER BY 1`, repository, GateMergeQueue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var issue int64
		if err := rows.Scan(&issue); err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	return out, rows.Err()
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

func (s *PGStore) PlansBetweenRounds(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.task_id FROM plan_revisions r JOIN tasks t ON t.id = r.task_id
		WHERE t.status IN ('done', 'closed')
			AND r.revision = (SELECT max(revision) FROM plan_revisions WHERE task_id = r.task_id)
			AND jsonb_typeof(r.plan->'outline') = 'array' AND r.plan->'outline' <> '[]'::jsonb
			AND NOT EXISTS (SELECT 1 FROM events WHERE task_id = r.task_id AND kind = 'plan_closed')
		ORDER BY r.task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
