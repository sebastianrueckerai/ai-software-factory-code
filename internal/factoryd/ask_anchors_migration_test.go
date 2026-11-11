package factoryd

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). The
// ask_anchors migration carries an old case's comment onto its face step:
// a faced case keeps its one face at the face's time, so its human wait is
// the same and a trusted comment written before the face answers nothing;
// a case with no face step gets one at the time of the ask row that carried
// its comment. A second run of the file changes nothing.
func TestAskAnchorsMigrationCarriesTheCommentOntoTheFaceOfOldCases(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_ask_anchors"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	migrate := func() {
		t.Helper()
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	const name = "factoryd/20261119100000_ask_anchors.sql"
	// the cases are seeded on the schema before this migration: it and every later one wait
	entries, err := os.ReadDir("../schema/migrations/factoryd")
	if err != nil {
		t.Fatal(err)
	}
	exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	held := 0
	for _, e := range entries {
		if n := "factoryd/" + e.Name(); n >= name {
			exec(`INSERT INTO schema_migrations (name) VALUES ('` + n + `')`)
			held++
		}
	}
	if held == 0 {
		t.Fatalf("no migration file is named %s", name)
	}
	migrate()
	exec(
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, comment_id, opened_at, resolved_at) VALUES
		 (1, 't1', 'sf/r', 'session_cap', 'k1', 'resolved', 'the cap is reached', 27001, '2026-11-01 10:00:00+00', '2026-11-01 12:00:00+00'),
		 (2, 't2', 'sf/r', 'agent_escalate', 'k2', 'at_human', 'Which store?', 27002, '2026-11-01 10:00:00+00', NULL),
		 (3, 't3', 'sf/r', 'other', 'k3', 'open', 'p', 0, '2026-11-01 10:00:00+00', NULL)`,
		`INSERT INTO case_steps (case_id, rung, actor, started_at, ended_at, outcome) VALUES
		 (1, 1, 'triage', '2026-11-01 10:00:00+00', '2026-11-01 11:00:00+00', 'handed_up'),
		 (1, 3, 'factoryd', '2026-11-01 11:00:00+00', '2026-11-01 12:00:00+00', 'resolved')`,
		`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES
		 ('2026-11-01 10:30:00+00', 'sf/r', 't2', 'question_asked', 'factoryd', 'factoryd', '{"comment": 27002, "case_id": 2}')`,
	)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	humanWait := func() float64 {
		t.Helper()
		f, err := metricHumanWait(ctx, db, "sf/r", newMetricWindow(day, day))
		if err != nil || !f.Has {
			t.Fatalf("human wait = %+v, %v", f, err)
		}
		return f.Value
	}
	// case 1 waited at the human from its face at 11:00 to its end at 12:00
	if got := humanWait(); got != 60*60*1000 {
		t.Fatalf("human wait before the migration = %v ms, want 3600000", got)
	}
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	migrate()
	if got := humanWait(); got != 60*60*1000 {
		t.Fatalf("human wait after the migration = %v ms, want 3600000 as before", got)
	}

	type face struct {
		CaseID, Comment int64
		Started         time.Time
		Outcome         string
	}
	faces := func() []face {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT case_id, comment_id, started_at, outcome FROM case_steps
			WHERE rung = 3 AND actor = 'factoryd' ORDER BY case_id, id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []face
		for rows.Next() {
			var f face
			if err := rows.Scan(&f.CaseID, &f.Comment, &f.Started, &f.Outcome); err != nil {
				t.Fatal(err)
			}
			f.Started = f.Started.UTC()
			out = append(out, f)
		}
		return out
	}
	want := []face{
		{1, 27001, time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC), "resolved"},
		{2, 27002, time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC), "in_progress"},
	}
	if got := faces(); !reflect.DeepEqual(got, want) {
		t.Fatalf("face steps after the migration = %+v, want %+v", got, want)
	}
	for id, want := range map[int]int64{1: 27001, 2: 27002, 3: 0} {
		var got int64
		if err := db.QueryRowContext(ctx, `SELECT case_face_comment($1)`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("case %d: case_face_comment = %d, %v; want %d", id, got, err, want)
		}
	}

	asks, err := (&PGStore{db: db}).AskAnchors(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 1, 11, 30, 0, 0, time.UTC)
	answers, _, _ := answersFrom([]IntakeEvent{
		{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "before the face", CommentID: 26000, Trusted: true, CreatedAt: at},
		{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "after the face", CommentID: 27050, Trusted: true, CreatedAt: at},
	}, asks)
	if want := []string{`sebastian (2026-11-01), to "the cap is reached": after the face`}; !reflect.DeepEqual(answers, want) {
		t.Fatalf("answers = %q, want %q: a comment before the face answers nothing", answers, want)
	}

	exec(`DELETE FROM schema_migrations WHERE name = '` + name + `'`)
	migrate()
	if got := faces(); !reflect.DeepEqual(got, want) {
		t.Fatalf("face steps after a second run = %+v, want %+v", got, want)
	}
}
