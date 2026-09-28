package schema

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Two binaries booting together (factoryd and the gateway after a roll)
// apply each migration once: one applies them all under the lock, the
// other finds them recorded.
func TestConcurrentMigratorsApplyEachFileOnce(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
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
