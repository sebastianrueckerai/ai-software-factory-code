package console

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// no_log_pointer_any_case: no refusal sentence under internal/console
// points at the factoryd log, in any case. The eleven Go files, the
// served console.js and the templates count; a refusal that names the
// log tells neither the human nor the chat what to do next.
func TestProofNoLogPointerAnyCase(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test's own file is unknown")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(here)))
	grep := exec.Command("git", "-C", root, "grep", "-i", "see the factoryd log", "--", "internal/console", ":!*_test.go")
	out, err := grep.Output()
	if err != nil && !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("git grep: %v", err)
	}
	if len(out) > 0 {
		t.Errorf("refusal sentences still point at the factoryd log:\n%s", out)
	}
}
