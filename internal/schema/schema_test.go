package schema

import (
	"bytes"
	"context"
	"database/sql"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"io/fs"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Two binaries booting together (factoryd and the gateway after a roll)
// apply each migration once: one applies them all under the lock, the
// other finds them recorded.
func TestConcurrentMigratorsApplyEachFileOnce(t *testing.T) {
	dsn := schematest.DSN(t, "schema")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	// an empty database: the rows other packages' tests leave behind would
	// meet a later migration's check on the re-run
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Migrate(ctx, db)
		}()
	}
	wg.Wait()
	log.SetOutput(os.Stderr)
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("migrate = %v", errs)
	}
	var names []string
	for _, set := range sets {
		entries, _ := fs.ReadDir(files, "migrations/"+set)
		for _, e := range entries {
			names = append(names, set+"/"+e.Name())
		}
	}
	out := buf.String()
	for _, name := range names {
		if n := strings.Count(out, "migration "+name+" applied"); n != 1 {
			t.Fatalf("%s applied %d times, want once:\n%s", name, n, out)
		}
	}
	if n := strings.Count(out, "migrations up to date"); n != 1 {
		t.Fatalf("up to date logged %d times, want once (the second migrator):\n%s", n, out)
	}
	var recorded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil || recorded != len(names) {
		t.Fatalf("recorded = %d, %v; want %d", recorded, err, len(names))
	}
}

// The spend repair: the final settlement of 2026-09-29 20:38:20Z added
// LiteLLM's figure onto 41 tasks settled before the ledger existed, whose
// spend already held it, so a settled task whose spend differs from its
// ledger sum gets that sum. Done and failed tasks from before finished_at
// was stamped get it from their last audit row or session, else from
// created_at, so the final settlement sees them. It runs on an empty
// database too: the ledger is the policy schema's.
func TestSpendRepairMigration(t *testing.T) {
	dsn := schematest.DSN(t, "spendrepair")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate an empty database: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, finished_at, settled_at, created_at) VALUES
			('doubled', 1, 0.02595072, 'done', '2026-09-26 20:53:41Z', '2026-09-29 20:38:20Z', '2026-09-28 16:56:56Z'),
			('noise', 1, 0.031115548, 'done', '2026-09-27 22:55:14Z', '2026-09-30 00:55:40Z', '2026-09-28 16:56:56Z'),
			('equal', 1, 0.5, 'failed', '2026-09-29 21:00:00Z', '2026-09-29 23:01:00Z', '2026-09-29 20:40:00Z'),
			('stage4-audit', 1, 0.01017076, 'done', NULL, NULL, '2026-09-28 16:56:56Z'),
			('stage4-session', 1, 0, 'failed', NULL, NULL, '2026-09-28 16:56:56Z'),
			('stage4-none', 1, 0, 'failed', NULL, NULL, '2026-09-28 16:56:56Z'),
			('running', 1, 0, 'running', NULL, NULL, '2026-09-30 09:00:00Z')`,
		`INSERT INTO ledger (task_id, kind, ref, eur) VALUES
			('doubled', 'settlement', 'litellm-final', 0.01297536),
			('noise', 'model', 'coding-cheap', 0.00277194799999999999), ('noise', 'settlement', 'litellm', 0.0283436),
			('equal', 'model', 'coding-cheap', 0.5)`,
		`INSERT INTO audit (task_id, ts, decision, reason) VALUES
			('stage4-audit', '2026-09-26 20:22:23.816334Z', 'allowed', ''),
			('stage4-audit', '2026-09-26 20:22:01Z', 'allowed', ''),
			('running', '2026-09-30 09:01:00Z', 'allowed', '')`,
		`INSERT INTO sessions (id, task_id, created_at) VALUES
			('s1', 'stage4-audit', '2026-09-26 20:22:00.816171Z'),
			('s2', 'stage4-session', '2026-09-26 18:35:19.91198Z'), ('s3', 'stage4-session', '2026-09-26 18:35:29.219136Z')`,
		`DELETE FROM schema_migrations WHERE name = 'gateway/0009_task_spend_repair.sql'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"doubled":        "0.01297536 2026-09-26 20:53:41",
		"noise":          "0.031115548 2026-09-27 22:55:14",
		"equal":          "0.5 2026-09-29 21:00:00",
		"stage4-audit":   "0.01017076 2026-09-26 20:22:23.816334",
		"stage4-session": "0 2026-09-26 18:35:29.219136",
		"stage4-none":    "0 2026-09-28 16:56:56",
		"running":        "0 ",
	}
	rows, err := db.QueryContext(ctx, `SELECT id, spent_eur::text || ' ' || COALESCE((finished_at AT TIME ZONE 'UTC')::text, '') FROM tasks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatal(err)
		}
		got[id] = row
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks (spent_eur finished_at) = %q, want %q", got, want)
	}
}
