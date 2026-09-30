// Package schema migrates the factory's one database, which factoryd and
// the gateway share: both apply every migration at boot, whichever boots
// first, under one advisory lock.
package schema

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

//go:embed migrations
var files embed.FS

// sets are the migration directories in the order they apply; within one,
// files apply in name order.
var sets = []string{"factoryd", "gateway"}

// Migrate applies the policy schema, then every migration not yet recorded
// in schema_migrations, each file with its record in one transaction. It
// logs each migration it applies, or that the schema was up to date.
func Migrate(ctx context.Context, db *sql.DB) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() // #nosec G104 -- returns the connection to the pool; closing the session also drops its lock
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('factory-schema'))`); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() {
		_, uerr := conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('factory-schema'))`)
		err = errors.Join(err, uerr)
	}()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	applied := 0
	// first: a migration may read the ledger or the holds
	if err := policy.Migrate(ctx, db); err != nil {
		return err
	}
	for _, set := range sets {
		entries, err := files.ReadDir("migrations/" + set)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := set + "/" + e.Name()
			var done bool
			if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
				return err
			}
			if done {
				continue
			}
			text, err := files.ReadFile("migrations/" + name)
			if err != nil {
				return err
			}
			if err := apply(ctx, conn, name, string(text)); err != nil {
				return fmt.Errorf("migrate %s: %w", name, err)
			}
			log.Printf("migration %s applied", name)
			applied++
		}
	}
	if applied == 0 {
		log.Printf("migrations up to date")
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, name, text string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	for _, stmt := range strings.Split(text, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit()
}
