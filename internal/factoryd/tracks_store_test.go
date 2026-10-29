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

const trackMigration = "factoryd/20261103100000_task_track.sql"

// The track migration on a database in the state before it: an agent row
// of lane plan takes the plan track, every other agent row (a lane ready
// card, a roll, a screening, a finished card) the direct track, a person's
// card none, and a backlog card and an open write-up of one end closed with
// their finish stamped. Then
// the schema refuses an agent row without a track.
func TestTrackMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_track"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`ALTER TABLE tasks DROP CONSTRAINT tasks_agent_track`,
		`ALTER TABLE tasks DROP COLUMN track`,
		`ALTER TABLE tasks ADD COLUMN lane text CHECK (lane IN ('backlog', 'plan', 'ready'))`,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task, lane, finished_at) VALUES
			('t-plan', 1, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL),
			('t-ready', 1, 0, 'running', 'sf/r', 'agent', 'webhook', NULL, 'ready', NULL),
			('t-roll', 1, 0, 'queued', 'sf/r', 'agent', 'roll', NULL, NULL, NULL),
			('t-scr', 1, 0, 'running', 'sf/r', 'agent', 'screening', 't-ready', NULL, NULL),
			('t-human', 1, 0, 'waiting-human', 'sf/r', 'human', 'webhook', NULL, NULL, NULL),
			('t-backlog', 1, 0, 'backlog', 'sf/r', 'agent', 'webhook', NULL, 'backlog', NULL),
			('t-done', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'ready', now()),
			('t-pre', 1, 0, 'running', 'sf/r', 'agent', 'presort', 't-backlog', NULL, NULL),
			('t-pre-done', 1, 0, 'done', 'sf/r', 'agent', 'presort', 't-done', NULL, now())`,
		`DELETE FROM schema_migrations WHERE name = '` + trackMigration + `'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := schema.Migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id || ' ' || status || ' ' || COALESCE(track, '-') || ' ' || (finished_at IS NOT NULL)::text FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []string{
		"t-backlog closed direct true",
		"t-done done direct true",
		"t-human waiting-human - false",
		"t-plan waiting-human plan false",
		"t-pre closed direct true",
		"t-pre-done done direct true",
		"t-ready running direct false",
		"t-roll queued direct false",
		"t-scr running direct false",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows after the track migration:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var lane bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'tasks' AND column_name = 'lane')`).Scan(&lane); err != nil || lane {
		t.Fatalf("column lane after the migration: %v, %v", lane, err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-none", Status: "queued", Objective: "o", Repository: "sf/r", Kind: "agent", Source: "webhook"}); err == nil ||
		!strings.Contains(err.Error(), "tasks_agent_track") {
		t.Fatalf("an agent row without a track: %v, want the tasks_agent_track refusal", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET track = 'ready' WHERE id = 't-ready'`); err == nil {
		t.Fatal("track ready was accepted")
	}
}

// OpenReportTask's conflict clause names the narrowed index: a screening
// opens once per parent, beside the many plan cards of one parent.
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
		Track: "plan", Process: "spec"}
	if err := s.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	first := Task{ID: "t-screening", RiskClass: "standard", BudgetEUR: 2, Status: "queued", Objective: "o", Repository: "sf/r", Source: SourceScreening,
		Parent: "t-plan", RequiredCapabilities: []string{policy.ClassAnalyzer}, Track: "direct"}
	if got, err := s.OpenReportTask(ctx, first); err != nil || got.ID != first.ID {
		t.Fatalf("open: %+v %v", got, err)
	}
	again := first
	again.ID = "t-screening-again"
	if got, err := s.OpenReportTask(ctx, again); err != nil || got.ID != first.ID {
		t.Fatalf("open again: %+v %v, want the first row", got, err)
	}
	child := func(id, key string) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task, plan_key, track, process)
			VALUES ($1, 1, 0, 'queued', 'sf/r', 'agent', 'plan', 't-plan', $2, 'direct', 'spec')`, id, key)
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
	if err != nil || !ok || got.Track != "direct" || got.Process != "spec" || got.Parent != "t-plan" {
		t.Fatalf("child: %+v %v %v", got, ok, err)
	}
	if got, _, _ := s.Task(ctx, "t-plan"); got.Track != "plan" || got.Process != "spec" {
		t.Fatalf("plan task: track %q process %q", got.Track, got.Process)
	}
}

// OpenCardsByTrack reads the open agent cards, each with its track, the
// project whose file in force names its repository and the process it
// follows (a row with none, a roll, takes the project's in force):
// finished cards, a person's cards and repositories of no project are
// left out.
func TestOpenCardsByTrack(t *testing.T) {
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, process, issue_number, finished_at) VALUES
		('t-plan', 1, 0, 'queued', 'linkshort/linkshort', 'agent', 'webhook', 'plan', 'spec', 14, NULL),
		('t-direct', 1, 0, 'running', 'linkshort/linkshort', 'agent', 'webhook', 'direct', 'spec', 1, NULL),
		('t-done', 1, 0, 'done', 'linkshort/linkshort', 'agent', 'webhook', 'plan', 'spec', 2, now()),
		('t-failed', 1, 0, 'failed', 'linkshort/linkshort', 'agent', 'webhook', 'direct', 'spec', 3, now()),
		('t-roll', 1, 0, 'queued', 'linkshort/linkshort', 'agent', 'roll', 'direct', NULL, 4, NULL),
		('t-human', 1, 0, 'waiting-human', 'linkshort/linkshort', 'human', 'webhook', NULL, NULL, 6, NULL),
		('t-stray', 1, 0, 'queued', 'x/y', 'agent', 'webhook', 'direct', 'maintenance', 5, NULL)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenCardsByTrack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []OpenCard{{Project: "linkshort", Track: "direct", Process: "spec", Issue: 1}, {Project: "linkshort", Track: "direct", Process: "spec", Issue: 4},
		{Project: "linkshort", Track: "plan", Process: "spec", Issue: 14}}
	if !slices.Equal(got, want) {
		t.Fatalf("open cards = %+v, want %+v", got, want)
	}
}
