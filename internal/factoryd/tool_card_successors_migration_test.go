package factoryd

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres. A card filed before cards that continue
// a tool card carried its image has none: the migration gives each open
// retry and successor the image of the nearest card it continues, along
// the chain of ids the factory derives from that card. An ended card, a
// card no tool card leads to and a card with its own image keep theirs.
// A second run changes nothing.
func TestToolCardSuccessorsMigrationCarriesTheImage(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_tool_card_successors"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	const name = "factoryd/20261121120500_tool_card_successors.sql"
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
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	retry := RetryID("t-tool")
	succ := SuccessorID(retry)
	ended := SuccessorID(succ)
	after := SuccessorID(ended)
	own := SuccessorID("t-tool2")
	for _, r := range []struct {
		id, source, status string
		image              any
	}{
		{"t-tool", "tool_request", "failed", "video"},
		{retry, "tool_request", "failed", nil},
		{succ, "successor", "running", nil},
		{ended, "successor", "done", nil},
		{after, "successor", "queued", nil},
		{"t-tool2", "tool_request", "failed", "audio"},
		{own, "successor", "queued", "audio"},
		{SuccessorID(own), "successor", "queued", nil},
		{SuccessorID("t-plain"), "successor", "queued", nil},
	} {
		exec(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, tool_image, finished_at)
			VALUES ($1, 1, 0, $2, 'sf/r', 'agent', $3, 'direct', $4, CASE WHEN $2 IN ('done', 'failed') THEN now() END)`,
			r.id, r.status, r.source, r.image)
	}
	want := map[string]string{
		"t-tool": "video", retry: "", succ: "video", ended: "", after: "video",
		"t-tool2": "audio", own: "audio", SuccessorID(own): "audio", SuccessorID("t-plain"): "",
	}
	for run := 1; run <= 2; run++ {
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(tool_image, '') FROM tasks`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for rows.Next() {
			var id, image string
			if err := rows.Scan(&id, &image); err != nil {
				t.Fatal(err)
			}
			got[id] = image
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("run %d: images = %v, want %v", run, got, want)
		}
	}
}
