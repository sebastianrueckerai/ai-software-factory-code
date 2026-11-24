package schematest

import (
	"strings"
	"testing"
)

func TestDatabaseNameIsScopedToThePackageDirectory(t *testing.T) {
	a := databaseName("/repo/internal/factoryd", "factoryd")
	b := databaseName("/repo/internal/factoryd/pg", "factoryd")
	if a == b {
		t.Fatalf("two directories share %q", a)
	}
	if again := databaseName("/repo/internal/factoryd", "factoryd"); again != a {
		t.Fatalf("same directory gave %q then %q", a, again)
	}
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, "factory_test_factoryd_") || len(n) != len("factory_test_factoryd_")+8 {
			t.Fatalf("name %q is not factory_test_factoryd_<8 hex>", n)
		}
	}
}

func TestDatabaseNameStaysWithinPostgresLimit(t *testing.T) {
	n := databaseName("/repo/x", strings.Repeat("n", 100))
	if len(n) != 63 {
		t.Fatalf("len(%q) = %d, want 63", n, len(n))
	}
	if other := databaseName("/repo/y", strings.Repeat("n", 100)); other == n {
		t.Fatal("the hash was truncated away")
	}
}
