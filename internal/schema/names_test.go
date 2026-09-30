package schema

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	stampedName  = regexp.MustCompile(`^(\d{14})_[a-z0-9_]+\.sql$`)
	numberedName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)
	lastNumbered = map[string]int{"factoryd": 36, "gateway": 10}
)

func migrationNameFindings(fsys fs.FS) []string {
	var out []string
	for _, set := range sets {
		entries, err := fs.ReadDir(fsys, "migrations/"+set)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		seen := map[string]string{}
		for _, e := range entries {
			name := set + "/" + e.Name()
			var prefix string
			if m := stampedName.FindStringSubmatch(e.Name()); m != nil {
				if _, err := time.Parse("20060102150405", m[1]); err != nil {
					out = append(out, name+": "+m[1]+" is not a time")
					continue
				}
				prefix = m[1]
			} else if m := numberedName.FindStringSubmatch(e.Name()); m != nil {
				if n, _ := strconv.Atoi(m[1]); n > lastNumbered[set] {
					out = append(out, fmt.Sprintf("%s: numbered past %s/%04d; name it <UTC yyyymmddhhmmss>_<slug>.sql", name, set, lastNumbered[set]))
					continue
				}
				prefix = m[1]
			} else {
				out = append(out, name+": not <UTC yyyymmddhhmmss>_<slug>.sql")
				continue
			}
			if first, ok := seen[prefix]; ok {
				out = append(out, name+" shares "+prefix+" with "+first)
			}
			seen[prefix] = name
		}
	}
	return out
}

// A new migration is named by its UTC time, so parallel branches never
// pick the same name; the numbered names up to factoryd/0036 and
// gateway/0010 are recorded in schema_migrations and stay.
func TestMigrationNames(t *testing.T) {
	if f := migrationNameFindings(files); len(f) > 0 {
		t.Fatalf("shipped migrations: %v", f)
	}
	for _, tc := range []struct {
		name  string
		plant []string
		bad   string // the planted file the one finding names; "" for none
	}{
		{"a timestamp name passes", []string{"factoryd/20261001120000_lanes.sql"}, ""},
		{"the next number", []string{"factoryd/0037_lanes.sql"}, "factoryd/0037_lanes.sql"},
		{"the gateway's next number", []string{"gateway/0011_x.sql"}, "gateway/0011_x.sql"},
		{"a frozen number again", []string{"factoryd/0036_again.sql"}, "factoryd/0036_again.sql"},
		{"thirteen digits", []string{"factoryd/2026100112000_x.sql"}, "factoryd/2026100112000_x.sql"},
		{"an upper-case slug", []string{"factoryd/20261001120000_Lanes.sql"}, "factoryd/20261001120000_Lanes.sql"},
		{"no slug", []string{"factoryd/20261001120000.sql"}, "factoryd/20261001120000.sql"},
		{"month 13", []string{"factoryd/20261301120000_x.sql"}, "factoryd/20261301120000_x.sql"},
		{"two equal timestamps", []string{"factoryd/20261001120000_a.sql", "factoryd/20261001120000_b.sql"}, "factoryd/20261001120000_b.sql"},
		{"equal timestamps in two sets", []string{"factoryd/20261001120000_a.sql", "gateway/20261001120000_a.sql"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.CopyFS(dir, files); err != nil {
				t.Fatal(err)
			}
			for _, p := range tc.plant {
				if err := os.WriteFile(filepath.Join(dir, "migrations", p), []byte("SELECT 1;\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f := migrationNameFindings(os.DirFS(dir))
			switch {
			case tc.bad == "" && len(f) != 0:
				t.Fatalf("findings = %v, want none", f)
			case tc.bad != "" && (len(f) != 1 || !strings.Contains(f[0], tc.bad)):
				t.Fatalf("findings = %v, want one naming %s", f, tc.bad)
			}
		})
	}
}
