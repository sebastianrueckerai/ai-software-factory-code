// Package schematest gives each test package its own Postgres database on
// the server of FACTORYD_TEST_DATABASE_URL, so packages that migrate and
// wipe tables run in parallel without meeting each other's rows.
package schematest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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

// databaseName is the database of name for tests in the package directory
// dir: the directory's hash keeps two packages that pick the same name
// apart. Postgres cuts identifiers at 63 bytes; the name gives way, not the hash.
func databaseName(dir, name string) string {
	sum := sha256.Sum256([]byte(dir))
	hash := hex.EncodeToString(sum[:4])
	if room := 63 - len(dbPrefix) - len(hash) - 1; len(name) > room {
		name = name[:room]
	}
	return dbPrefix + name + "_" + hash
}

// callerDir is the directory of the test file skip frames up.
func callerDir(skip int) string {
	_, file, _, _ := runtime.Caller(skip + 1)
	return filepath.Dir(file)
}

// Scratch is DSN for a database the test drops at its end, for a test
// binary that makes many: a server holds only so many.
func Scratch(t testing.TB, name string) string {
	t.Helper()
	dir := callerDir(1)
	dsn := dsnIn(t, dir, name)
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
		if _, err := db.Exec(`DROP DATABASE IF EXISTS "` + databaseName(dir, name) + `" WITH (FORCE)`); err != nil {
			t.Error(err)
		}
	})
	return dsn
}

// DSN is database factory_test_<name>_<hash of the caller's directory>, dropped and created empty at its
// first use in the test binary. The test is skipped without
// FACTORYD_TEST_DATABASE_URL.
func DSN(t testing.TB, name string) string {
	t.Helper()
	return dsnIn(t, callerDir(1), name)
}

func dsnIn(t testing.TB, dir, name string) string {
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
	dbName := databaseName(dir, name)
	for _, stmt := range []string{`DROP DATABASE IF EXISTS "` + dbName + `" WITH (FORCE)`, `CREATE DATABASE "` + dbName + `"`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	u.Path = "/" + dbName
	fresh[name] = u.String()
	return fresh[name]
}
