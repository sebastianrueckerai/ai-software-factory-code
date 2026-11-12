// Package schematest gives each test package its own Postgres database on
// the server of FACTORYD_TEST_DATABASE_URL, so packages that migrate and
// wipe tables run in parallel without meeting each other's rows.
package schematest

import (
	"database/sql"
	"net/url"
	"os"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	mu    sync.Mutex
	fresh = map[string]string{}
)

// dbPrefix names every test database.
const dbPrefix = "factory_test_"

// Scratch is DSN for a database the test drops at its end, for a test
// binary that makes many: a server holds only so many.
func Scratch(t testing.TB, name string) string {
	t.Helper()
	dsn := DSN(t, name)
	t.Cleanup(func() {
		mu.Lock()
		delete(fresh, name)
		mu.Unlock()
		db, err := sql.Open("pgx", os.Getenv("FACTORYD_TEST_DATABASE_URL"))
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		if _, err := db.Exec(`DROP DATABASE IF EXISTS "` + dbPrefix + name + `" WITH (FORCE)`); err != nil {
			t.Error(err)
		}
	})
	return dsn
}

// DSN is database factory_test_<name>, dropped and created empty at its
// first use in the test binary. The test is skipped without
// FACTORYD_TEST_DATABASE_URL.
func DSN(t testing.TB, name string) string {
	t.Helper()
	base := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	mu.Lock()
	defer mu.Unlock()
	if dsn, ok := fresh[name]; ok {
		return dsn
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("FACTORYD_TEST_DATABASE_URL: %v", err)
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dbName := dbPrefix + name
	for _, stmt := range []string{`DROP DATABASE IF EXISTS "` + dbName + `" WITH (FORCE)`, `CREATE DATABASE "` + dbName + `"`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	u.Path = "/" + dbName
	fresh[name] = u.String()
	return fresh[name]
}
