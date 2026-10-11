package schema

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSplitStatementsDropsCommentHoldingSemicolon(t *testing.T) {
	text := "-- events (ruling 252): one row per action; the feed reads it by repository.\n" +
		"CREATE TABLE events (id bigserial PRIMARY KEY);\n" +
		"CREATE INDEX events_id_idx ON events (id);\n"
	got := splitStatements(text)
	want := []string{
		"CREATE TABLE events (id bigserial PRIMARY KEY)",
		"CREATE INDEX events_id_idx ON events (id)",
	}
	if len(got) != len(want) {
		t.Fatalf("statements = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitStatementsKeepsTrailingCommentOfLastStatement(t *testing.T) {
	got := splitStatements("SELECT 1;\n-- done;")
	if len(got) != 1 || got[0] != "SELECT 1" {
		t.Fatalf("statements = %#v", got)
	}
}

func TestSplitStatementsDoesNotSplitInsideStringLiteral(t *testing.T) {
	text := "INSERT INTO cfg (k, v) VALUES ('a;b', 'c');\n" +
		"INSERT INTO cfg (k, v) VALUES ('it''s; fine', 'x;y');\n"
	got := splitStatements(text)
	if len(got) != 2 {
		t.Fatalf("statements = %d, want 2: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "'a;b'") {
		t.Errorf("statement 0 lost its literal: %q", got[0])
	}
	if !strings.Contains(got[1], "'it''s; fine'") || !strings.Contains(got[1], "'x;y'") {
		t.Errorf("statement 1 mangled: %q", got[1])
	}
}

func TestSplitStatementsKeepsDollarQuotedBodyAsOneStatement(t *testing.T) {
	text := "CREATE FUNCTION add(a int, b int) RETURNS int LANGUAGE sql AS $$\n" +
		"  SELECT a + b;\n" +
		"  SELECT 1; -- semicolons and a comment in here\n" +
		"$$;\n" +
		"CREATE FUNCTION neg(a int) RETURNS int AS $body$\n" +
		"  SELECT -a; SELECT 2;\n" +
		"$body$;\n" +
		"UPDATE t SET x = 'has $ in it;' WHERE id = 1;\n"
	got := splitStatements(text)
	if len(got) != 3 {
		t.Fatalf("statements = %d, want 3: %#v", len(got), got)
	}
	// the body's semicolons survive; only the terminator's own ";" is gone
	if strings.Count(got[0], ";") != 2 || strings.Count(got[1], ";") != 2 {
		t.Errorf("dollar-quoted bodies were split: %q / %q", got[0], got[1])
	}
	if !strings.HasSuffix(strings.TrimSpace(got[0]), "$$") || !strings.HasSuffix(strings.TrimSpace(got[1]), "$body$") {
		t.Errorf("dollar quoting lost: %q / %q", got[0], got[1])
	}
	if !strings.Contains(got[2], "'has $ in it;'") {
		t.Errorf("statement 2 mangled: %q", got[2])
	}
}

func TestSplitStatementsSkipsEmptyPieces(t *testing.T) {
	for _, text := range []string{"", "   ", ";;;", "\n\t\n", "-- nothing but a comment\n"} {
		if got := splitStatements(text); len(got) != 0 {
			t.Errorf("splitStatements(%q) = %#v, want none", text, got)
		}
	}
	if got := splitStatements("SELECT 1;;\n;SELECT 2; ;"); len(got) != 2 {
		t.Fatalf("statements = %d, want 2: %#v", len(got), got)
	}
}

var leadingKeyword = regexp.MustCompile(`^(?i)(CREATE|ALTER|INSERT|UPDATE|DROP|COMMENT|GRANT|WITH)\b`)

// Every statement of every migration file starts with a SQL keyword: no
// comment fragment can ever reach the database. WITH counts because a CTE
// wrapping a data-modifying statement (0014_task_capabilities) is a
// statement in its own right; a comment's tail ("a card's activity line")
// never does.
func TestEveryMigrationFileSplitsIntoRealStatements(t *testing.T) {
	filesWalked := 0
	for _, set := range sets {
		entries, err := fs.ReadDir(files, "migrations/"+set)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
				continue
			}
			name := set + "/" + e.Name()
			text, err := files.ReadFile("migrations/" + name)
			if err != nil {
				t.Fatal(err)
			}
			stmts := splitStatements(string(text))
			if len(stmts) == 0 {
				t.Errorf("%s: no statements", name)
			}
			for i, s := range stmts {
				if !leadingKeyword.MatchString(s) {
					t.Errorf("%s statement %d does not start with a SQL keyword: %q", name, i, firstLine(s))
				}
			}
			filesWalked++
		}
	}
	if filesWalked == 0 {
		t.Fatal("no migration files walked")
	}
}

func firstLine(s string) string {
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		return s[:nl]
	}
	return s
}
