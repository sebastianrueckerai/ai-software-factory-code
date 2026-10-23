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
	for _, stmt := range splitStatements(text) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// splitStatements cuts a migration file into the statements to run: it
// drops "--" line comments, then splits on ";" outside single-quoted
// strings and dollar-quoted blocks, and skips empty pieces. A semicolon in
// a comment or a string is no separator; without this the runner turns the
// tail of "-- foo; bar" into a statement (the roll of 655f607 crash-looped
// factoryd at boot on exactly such a fragment).
func splitStatements(text string) []string {
	var stmts []string
	for _, piece := range splitOutsideStrings(stripLineComments(text), ';') {
		if s := strings.TrimSpace(piece); s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

// stripLineComments removes "--" to end-of-line comments, skipping over
// single-quoted strings (” escapes included) and dollar-quoted blocks, so
// a -- inside 'a--b' or $tag$...$tag$ stays put.
func stripLineComments(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		switch {
		case text[i] == '\'':
			j := i + 1
			for j < len(text) {
				if text[j] == '\'' {
					if j+1 < len(text) && text[j+1] == '\'' { // '' escape
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(text[i:j])
			i = j
		case text[i] == '$':
			end, tag, ok := dollarTagAt(text, i)
			if !ok {
				b.WriteByte(text[i])
				i++
				continue
			}
			close := strings.Index(text[end:], tag)
			if close < 0 { // unterminated: keep the rest verbatim
				b.WriteString(text[i:])
				return b.String()
			}
			j := end + close + len(tag)
			b.WriteString(text[i:j])
			i = j
		case text[i] == '-' && i+1 < len(text) && text[i+1] == '-':
			if nl := strings.IndexByte(text[i:], '\n'); nl >= 0 {
				i += nl // keep the newline
			} else {
				i = len(text)
			}
		default:
			b.WriteByte(text[i])
			i++
		}
	}
	return b.String()
}

// splitOutsideStrings splits text on every byte sep that lies outside a
// single-quoted string and outside a dollar-quoted block. Comments must be
// stripped before this runs.
func splitOutsideStrings(text string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(text); {
		switch {
		case text[i] == '\'':
			j := i + 1
			for j < len(text) {
				if text[j] == '\'' {
					if j+1 < len(text) && text[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			i = j
		case text[i] == '$':
			end, tag, ok := dollarTagAt(text, i)
			if !ok {
				i++
				continue
			}
			close := strings.Index(text[end:], tag)
			if close < 0 {
				i = end
			} else {
				i = end + close + len(tag)
			}
		case text[i] == sep:
			parts = append(parts, text[start:i])
			i++
			start = i
		default:
			i++
		}
	}
	return append(parts, text[start:])
}

// dollarTagAt reports the open tag "$tag$" of a dollar-quoted block at i:
// its end (just past the closing delimiter) and the closing delimiter,
// which carries the same tag ("$tag$"). ok is false when i does not open
// one, so positional parameters like $1 never look like tags.
func dollarTagAt(text string, i int) (end int, tag string, ok bool) {
	j := i + 1
	for j < len(text) && (isIdentByte(text[j])) {
		j++
	}
	if j >= len(text) || text[j] != '$' {
		return 0, "", false
	}
	delim := text[i : j+1]
	return j + 1, delim, true
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
