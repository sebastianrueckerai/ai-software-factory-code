package factoryd

import (
	"context"
	"database/sql"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// GroupOf names card t's group: a plan's card (Task.PlanCard) belongs to
// its plan's group; every other card is a group of one, named by its own
// id.
func GroupOf(t Task) string {
	if t.PlanCard() {
		return t.Parent
	}
	return t.ID
}

// PlanCard reports whether card t is its key's card in its parent plan: a
// card the plan filed, or the successor of one, which carries its key. Its
// replan is its plan's, and it never turns plan-track.
func (t Task) PlanCard() bool {
	return t.Parent != "" && (t.Source == SourcePlan || t.Source == SourceSuccessor && t.PlanKey != "")
}

// InPlan reports whether card t is a plan or a plan's card
// (Task.PlanCard), so its replan is its plan's.
func InPlan(t Task) bool {
	return t.PlanCard() || t.Track == policy.TrackPlan
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
	// Children counts the plan's cards by status, each key once, as its
	// newest card.
	Children map[string]int
}

func (s *PGStore) PlanClosed(ctx context.Context, plan string) (time.Time, bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT min(at) FROM events WHERE task_id = $1 AND kind = 'plan_closed'`, plan).Scan(&at)
	return at.Time, at.Valid, err
}

// QueuedPlans are the issues of repository's plans with a card
// (Task.PlanCard) in the merge queue, in review and holding an entry,
// waiting in gate merge-queue, or waiting for its approval, which takes a
// turn again before the merge (versionQueueWaits): its merge would open
// the plan's group on main. A card merged by hand keeps its gate but has
// left the queue.
func (s *PGStore) QueuedPlans(ctx context.Context, repository string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT p.issue_number FROM tasks c JOIN tasks p ON p.id = c.parent_task
		WHERE c.repository = $1 AND `+planCardRow+` AND c.plan_key IS NOT NULL AND c.status = 'in-review'
			AND (c.gate IN ($2, $3) OR EXISTS (SELECT 1 FROM merge_queue q WHERE q.task_id = c.id))
		ORDER BY 1`, repository, GateMergeQueue, GateApproval)
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

// planCardRow is the SQL of Task.PlanCard for a row c: a card its plan
// filed, or the successor that carries a key.
const planCardRow = `(c.source = 'plan' OR c.source = 'successor' AND c.plan_key IS NOT NULL)`

// groupOpenings is each plan's opening, its first actual child merge, a
// successor's that carries a key among them: per child its earliest
// pull_request_merged row, else its finish when it holds its own merge
// commit, reverted children out, and the plan's earliest of those.
// OpenGroups and the group-open panel read it.
const groupOpenings = `SELECT plan, repository, min(merged) AS first_merge FROM (
		SELECT c.parent_task AS plan, c.repository,
			COALESCE(min(e.at), min(c.finished_at) FILTER (WHERE c.merge_commit <> '')) AS merged
		FROM tasks c
		LEFT JOIN events e ON e.task_id = c.id AND e.kind = 'pull_request_merged'
		WHERE ` + planCardRow + ` AND c.plan_key IS NOT NULL AND (c.merge_commit <> '' OR e.id IS NOT NULL) AND c.reverted_at IS NULL
		GROUP BY c.parent_task, c.repository, c.id
	) child GROUP BY plan, repository`

func (s *PGStore) OpenGroups(ctx context.Context) (map[string][]Group, error) {
	rows, err := s.db.QueryContext(ctx, `WITH merged AS (`+groupOpenings+`)
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
	// each key counts once, as its newest card: the end of its chain of
	// successors
	counts, err := s.db.QueryContext(ctx, `SELECT parent_task, status, count(*) FROM (
			SELECT DISTINCT ON (c.parent_task, c.plan_key) c.parent_task, c.status FROM tasks c
			WHERE c.parent_task = ANY($1) AND `+planCardRow+` AND c.plan_key IS NOT NULL ORDER BY c.parent_task, c.plan_key, c.created_at DESC, c.id DESC
		) k GROUP BY 1, 2`, plans)
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

func (s *PGStore) PlanClosesToFinish(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, COALESCE(e.detail->>'words', '') FROM tasks t JOIN events e ON e.task_id = t.id AND e.kind = 'plan_closed'
		WHERE t.track = $1 AND e.at < now() - interval '1 minute'
			AND (t.status = 'failed' OR t.status = 'done' AND COALESCE(e.detail->>'words', '') <> '')`, policy.TrackPlan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, words string
		if err := rows.Scan(&id, &words); err != nil {
			return nil, err
		}
		out[id] = words
	}
	return out, rows.Err()
}

// PlanMerged says whether a card of plan holds an unreverted merge: the
// plan has an opening in groupOpenings, as OpenGroups reads it.
func (s *PGStore) PlanMerged(ctx context.Context, plan string) (bool, error) {
	var merged bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM (`+groupOpenings+`) g WHERE g.plan = $1)`, plan).Scan(&merged)
	return merged, err
}

// PlansAwaitingCriteria are the plan cards done or failed, the plans the
// criteria gate can still run for, or closed after a run of the gate on
// main's head, which may have withdrawn their rows, without a plan_closed
// event, whose newest revision names criteria and no later round, which
// filed the integration card of that revision, and whose cards, each
// successor that carries a key among them, are all final or merged, by
// id.
func (s *PGStore) PlansAwaitingCriteria(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.task_id FROM plan_revisions r JOIN tasks t ON t.id = r.task_id
		WHERE (t.status IN ('done', 'failed') OR t.status = 'closed' AND EXISTS (SELECT 1 FROM events
				WHERE task_id = r.task_id AND kind = 'criteria_gate_run' AND detail->>'pr' = '0'))
			AND r.revision = (SELECT max(revision) FROM plan_revisions WHERE task_id = r.task_id)
			AND jsonb_typeof(r.plan->'criteria') = 'array' AND r.plan->'criteria' <> '[]'::jsonb
			AND COALESCE(r.plan->'outline', '[]'::jsonb) IN ('[]'::jsonb, 'null'::jsonb)
			AND NOT EXISTS (SELECT 1 FROM events WHERE task_id = r.task_id AND kind = 'plan_closed')
			AND EXISTS (SELECT 1 FROM tasks i WHERE i.parent_task = r.task_id AND i.source = 'plan'
				AND i.plan_key IN (SELECT x->>'key' FROM jsonb_array_elements(r.plan->'cards') x WHERE x->>'role' = 'integration'))
			AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_task = r.task_id AND `+planCardRow+` AND c.plan_key IS NOT NULL
				AND c.status NOT IN ('done', 'failed', 'closed', 'merged'))
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
