package console

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// no_log_pointer_any_case: no refusal sentence under internal/console
// points at the factoryd log, in any case. The Go files, the served
// console.js and the templates count; a refusal that names the log tells
// neither the human nor the chat what to do next.
func TestProofNoLogPointerAnyCase(t *testing.T) {
	searched := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if ext := filepath.Ext(path); ext != ".go" && ext != ".js" && ext != ".html" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		searched++
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(line), "factoryd log") {
				t.Errorf("%s:%d points at the factoryd log: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if searched < 20 {
		t.Fatalf("the search read %d files, want the package's Go files, console.js and the templates", searched)
	}
}
