package factoryd

import (
	"strings"
	"testing"
)

const skipDiff = "diff --git a/x_test.go b/x_test.go\n--- a/x_test.go\n+++ b/x_test.go\n@@ -10,3 +10,4 @@ func TestA(t *testing.T) {\n \ta := 1\n+\tt.Skip(\"flaky\")\n \t_ = a\n}\n"

func TestCheats(t *testing.T) {
	for name, tc := range map[string]struct {
		diff string
		want string // prefix of the one finding; "" none
	}{
		"an unconditional skip":      {skipDiff, "anti-cheat: x_test.go:11 adds a skip to a test: t.Skip(\"flaky\")"},
		"skipf":                      {strings.Replace(skipDiff, "t.Skip(\"flaky\")", "t.Skipf(\"%s\", why)", 1), "anti-cheat: x_test.go:11 adds a skip to a test: t.Skipf"},
		"a conditional skip":         {strings.Replace(skipDiff, "\n+\tt.Skip(", "\n+\t\tt.Skip(", 1), ""},
		"a skip outside a test file": {strings.ReplaceAll(skipDiff, "x_test.go", "x.go"), ""},
		"a removed test":             {"diff --git a/x_test.go b/x_test.go\n--- a/x_test.go\n+++ b/x_test.go\n@@ -5,3 +5,0 @@\n-func TestGone(t *testing.T) {\n-\tt.Fatal(\"x\")\n-}\n", "anti-cheat: x_test.go:5 (base) removes TestGone"},
		"a moved test": {"diff --git a/a_test.go b/a_test.go\n--- a/a_test.go\n+++ b/a_test.go\n@@ -3,1 +3,0 @@\n-func TestMoved(t *testing.T) {\n" +
			"diff --git a/b_test.go b/b_test.go\n--- a/b_test.go\n+++ b/b_test.go\n@@ -1,0 +2,1 @@\n+func TestMoved(t *testing.T) {\n", ""},
		"a build tag on an old test file": {"diff --git a/x_test.go b/x_test.go\n--- a/x_test.go\n+++ b/x_test.go\n@@ -1,1 +1,2 @@\n+//go:build integration\n package a\n", "anti-cheat: x_test.go:1 adds a build constraint to a test file"},
		"a build tag on a new test file":  {"diff --git a/x_test.go b/x_test.go\n--- /dev/null\n+++ b/x_test.go\n@@ -0,0 +1,2 @@\n+//go:build linux\n+package a\n", ""},
		"a test file renamed away":        {"diff --git a/x_test.go b/x.go.bak\nsimilarity index 100%\nrename from x_test.go\nrename to x.go.bak\n", "anti-cheat: x_test.go is renamed to x.go.bak"},
	} {
		t.Run(name, func(t *testing.T) {
			got := cheats(splitDiff(tc.diff))
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("cheats = %q, want none", got)
			case tc.want != "" && (len(got) != 1 || !strings.HasPrefix(got[0], tc.want)):
				t.Fatalf("cheats = %q, want one starting %q", got, tc.want)
			}
		})
	}
}

func TestCheatsIgnoresAConditionalSkip(t *testing.T) {
	d := "diff --git a/db_test.go b/db_test.go\n--- /dev/null\n+++ b/db_test.go\n@@ -0,0 +1,6 @@\n+func TestDB(t *testing.T) {\n+\turl := os.Getenv(\"URL\")\n+\tif url == \"\" {\n+\t\tt.Skip(\"no database\")\n+\t}\n+}\n"
	if got := cheats(splitDiff(d)); len(got) != 0 {
		t.Fatalf("cheats = %q, want none", got)
	}
}

func TestCheatsAMovedTestIsNotRemoved(t *testing.T) {
	d := "diff --git a/a_test.go b/a_test.go\n--- a/a_test.go\n+++ b/a_test.go\n@@ -3,1 +3,0 @@\n-func TestMoved(t *testing.T) {\n" +
		"diff --git a/b_test.go b/b_test.go\n--- a/b_test.go\n+++ b/b_test.go\n@@ -1,0 +2,1 @@\n+func TestMoved(t *testing.T) {\n" +
		"diff --git a/c_test.go b/c_test.go\n--- a/c_test.go\n+++ b/c_test.go\n@@ -1,1 +1,0 @@\n-func TestReallyGone(t *testing.T) {\n"
	got := cheats(splitDiff(d))
	if len(got) != 1 || !strings.Contains(got[0], "removes TestReallyGone") {
		t.Fatalf("cheats = %q, want TestReallyGone alone", got)
	}
}

func TestCheatsAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/x_test.go b/x_test.go\n--- a/x_test.go\n+++ b/x_test.go\n@@ -1,0 +1,12 @@\n")
	for range 12 {
		b.WriteString("+\tt.Skip()\n")
	}
	if got := cheats(splitDiff(b.String())); len(got) != maxCheats {
		t.Fatalf("cheats = %d, want %d", len(got), maxCheats)
	}
}
