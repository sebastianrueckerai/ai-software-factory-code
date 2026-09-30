package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const lanesMigration = "factoryd/20260930095046_lanes.sql"

// The lanes migration on a database in the state before it: suggested
// becomes backlog, every open agent row takes its lane from its status,
// and rolls, report tasks, the human lane and finished rows keep none.
func TestLanesMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_lanes"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`DROP INDEX tasks_plan_key`,
		`DROP INDEX tasks_parent_source_key`,
		`CREATE UNIQUE INDEX tasks_parent_source_key ON tasks (parent_task, source) WHERE parent_task IS NOT NULL`,
		`ALTER TABLE tasks DROP COLUMN lane, DROP COLUMN process, DROP COLUMN plan_key`,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task) VALUES
			('t-sug', 1, 0, 'suggested', 'sf/r', 'agent', 'webhook', NULL),
			('t-q', 1, 0, 'queued', 'sf/r', 'agent', 'api', NULL),
			('t-run', 1, 0, 'running', 'sf/r', 'agent', 'alert', NULL),
			('t-rev', 1, 0, 'in-review', 'sf/r', 'agent', 'webhook', NULL),
			('t-wh', 1, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', NULL),
			('t-wp', 1, 0, 'waiting-poster', 'sf/r', 'agent', 'webhook', NULL),
			('t-cl', 1, 0, 'closing', 'sf/r', 'agent', 'webhook', NULL),
			('t-done', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL),
			('t-closed', 1, 0, 'closed', 'sf/r', 'agent', 'webhook', NULL),
			('t-roll', 1, 0, 'queued', 'sf/r', 'agent', 'roll', NULL),
			('t-human', 1, 0, 'waiting-human', 'sf/r', 'human', 'config', NULL),
			('t-pre', 1, 0, 'queued', 'sf/r', 'agent', 'presort', 't-sug'),
			('t-scr', 1, 0, 'running', 'sf/r', 'agent', 'screening', 't-q')`,
		`DELETE FROM schema_migrations WHERE name = '` + lanesMigration + `'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := schema.Migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id || ' ' || status || ' ' || COALESCE(lane, '-') || ' ' || COALESCE(process, '-') FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var r string
		rows.Scan(&r)
		got = append(got, r)
	}
	rows.Close()
	want := []string{
		"t-cl closing ready -",
		"t-closed closed - -",
		"t-done done - -",
		"t-human waiting-human - -",
		"t-pre queued - -",
		"t-q queued ready -",
		"t-rev in-review ready -",
		"t-roll queued - -",
		"t-run running ready -",
		"t-scr running - -",
		"t-sug backlog backlog -",
		"t-wh waiting-human ready -",
		"t-wp waiting-poster ready -",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows after the lanes migration:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET lane = 'review' WHERE id = 't-q'`); err == nil {
		t.Fatal("lane review was accepted")
	}
}

// The narrowed index takes a plan's many children of one parent, one per
// key, and OpenReportTask's conflict clause still names it: a report
// opens once per parent and source.
func TestOpenReportTaskAfterNarrowedIndex(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	parent := Task{ID: "t-plan", RiskClass: "standard", BudgetEUR: 2, Status: "running", Objective: "o", Repository: "sf/r", IssueNumber: 1, Source: "webhook",
		Lane: "plan", Process: "spec"}
	if err := s.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{SourcePresort, SourceScreening} {
		first := Task{ID: "t-" + source, RiskClass: "standard", BudgetEUR: 2, Status: "queued", Objective: "o", Repository: "sf/r", Source: source,
			Parent: "t-plan", RequiredCapabilities: []string{"analyzer"}}
		if got, err := s.OpenReportTask(ctx, first); err != nil || got.ID != first.ID {
			t.Fatalf("open %s: %+v %v", source, got, err)
		}
		again := first
		again.ID = "t-" + source + "-again"
		if got, err := s.OpenReportTask(ctx, again); err != nil || got.ID != first.ID {
			t.Fatalf("open %s again: %+v %v, want the first row", source, got, err)
		}
	}
	child := func(id, key string) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task, plan_key, lane, process)
			VALUES ($1, 1, 0, 'backlog', 'sf/r', 'agent', 'plan', 't-plan', $2, 'backlog', 'spec')`, id, key)
		return err
	}
	if err := child("t-c1", "foundation"); err != nil {
		t.Fatal(err)
	}
	if err := child("t-c2", "links-api"); err != nil {
		t.Fatalf("a plan's second child: %v", err)
	}
	if err := child("t-c3", "links-api"); err == nil {
		t.Fatal("a plan key twice was accepted")
	}
	got, ok, err := s.Task(ctx, "t-c2")
	if err != nil || !ok || got.Lane != "backlog" || got.Process != "spec" || got.Parent != "t-plan" {
		t.Fatalf("child: %+v %v %v", got, ok, err)
	}
	if got, _, _ := s.Task(ctx, "t-plan"); got.Lane != "plan" || got.Process != "spec" {
		t.Fatalf("plan task: lane %q process %q", got.Lane, got.Process)
	}
}

// The loader's Loaded hook gives every open card with a lane and no
// process its project's process from the loaded version: finished rows,
// rolls, cards that have one and other repositories keep theirs.
func TestProcessBackfilledOnLoad(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, lane, process) VALUES
		('t-ready', 1, 0, 'running', 'sf/r', 'agent', 'webhook', 'ready', NULL),
		('t-backlog', 1, 0, 'backlog', 'sf/r', 'agent', 'webhook', 'backlog', NULL),
		('t-has', 1, 0, 'queued', 'sf/r', 'agent', 'webhook', 'ready', 'spec'),
		('t-done', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'ready', NULL),
		('t-roll', 1, 0, 'queued', 'sf/r', 'agent', 'roll', NULL, NULL),
		('t-infra', 1, 0, 'queued', 'sf/infra', 'agent', 'webhook', 'ready', NULL)`); err != nil {
		t.Fatal(err)
	}
	v := policy.Version{ID: 3, Commit: "c3", Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
		"r": {Repository: "sf/r", Process: "maintenance"}, "infra": {Repository: "sf/infra"}}}}
	alarm := &ConfigAlarm{Store: s, Repo: Repo{Owner: "sf", Name: "infra"}, Dir: "factory"}
	for range 2 {
		if err := alarm.Resolve(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id || ' ' || COALESCE(process, '-') FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var r string
		rows.Scan(&r)
		got = append(got, r)
	}
	rows.Close()
	if want := []string{"t-backlog maintenance", "t-done -", "t-has spec", "t-infra -", "t-ready maintenance", "t-roll -"}; !slices.Equal(got, want) {
		t.Fatalf("processes = %v, want %v", got, want)
	}
}

// OpenCardsByLane reads the open cards with a lane, each with the project
// whose file in force names its repository and the process it follows (a
// row with none yet takes the project's in force): finished cards, rolls
// and repositories of no project are left out.
func TestOpenCardsByLane(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordPolicy(ctx, "open-cards", fixtureVersion(t).Tree); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, lane, process, issue_number) VALUES
		('t-plan', 1, 0, 'queued', 'linkshort/linkshort', 'agent', 'webhook', 'plan', 'spec', 14),
		('t-backlog', 1, 0, 'backlog', 'linkshort/linkshort', 'agent', 'webhook', 'backlog', NULL, 1),
		('t-done', 1, 0, 'done', 'linkshort/linkshort', 'agent', 'webhook', 'plan', 'spec', 2),
		('t-failed', 1, 0, 'failed', 'linkshort/linkshort', 'agent', 'webhook', 'ready', 'spec', 3),
		('t-roll', 1, 0, 'queued', 'linkshort/linkshort', 'agent', 'roll', NULL, NULL, 4),
		('t-stray', 1, 0, 'queued', 'x/y', 'agent', 'webhook', 'ready', 'maintenance', 5)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenCardsByLane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []OpenCard{{Project: "linkshort", Lane: "backlog", Process: "spec", Issue: 1}, {Project: "linkshort", Lane: "plan", Process: "spec", Issue: 14}}
	if !slices.Equal(got, want) {
		t.Fatalf("open cards = %+v, want %+v", got, want)
	}
}
