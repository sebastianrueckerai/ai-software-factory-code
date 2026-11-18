package pg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The helpers the ladder pair shared with factoryd's own test files and
// cannot reach across the package line: copies, kept verbatim except where
// they reach factoryd's unexported names through the export surface.

// caseEventStore is a store whose one task, caseTask, waits on a human
// with a budget of 5 EUR, its cases, events, intake rows, approvals and
// plans empty.
func caseEventStore(t *testing.T) *factoryd.PGStore {
	t.Helper()
	return cardCaseStore(t, "cases_events")
}

// gitFixture is a repository whose main defines Sym at a.go:3 and whose
// branch head adds a call at b.go:2 and onlyHead's declaration at b.go:3.
func gitFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := factoryd.PgGit(ctx, dir, "", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("a.go", "package a\n\nfunc Sym() {}\n")
	run("add", ".")
	run("commit", "-q", "-m", "main")
	run("checkout", "-q", "-b", "head")
	write("b.go", "package a\nvar _ = Sym\nvar onlyHead = 0\n")
	run("add", ".")
	run("commit", "-q", "-m", "head")
	return dir
}

// logLines is go test output as Forgejo serves it: every line after a
// timestamp.
func logLines(lines ...string) string {
	var b strings.Builder
	for i, l := range lines {
		b.WriteString("2026-11-10T10:48:4" + string(rune('0'+i%10)) + ".1723690Z " + l + "\n")
	}
	return b.String()
}

// offeredNames are the names of the actions Offered gives rung on f.
func offeredNames(f CaseFacts, rung int) []string {
	var out []string
	for _, a := range Offered(f, rung) {
		out = append(out, a.Name)
	}
	return out
}

// afterRung2 lets rung 2's step on case id run past its bound, as the sweep
// finds it: the step ends handed up and the case climbs to the human.
func (g rungRig) afterRung2(t *testing.T, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 2`, id); err != nil {
		t.Fatal(err)
	}
	own := g.stepsOfRung(t, id, 2)
	if len(own) != 1 || own[0].DueAt == nil {
		t.Fatalf("rung 2 steps = %+v, want one with a bound", own)
	}
	g.r.PgSetNow(func() time.Time { return own[0].DueAt.Add(time.Minute) })
	expired, err := g.s.ExpireSteps(ctx, g.r.Clock())
	if err != nil {
		t.Fatal(err)
	}
	if err := g.r.ClimbExpired(ctx, expired); err != nil {
		t.Fatal(err)
	}
	g.settle()
}

var recommendedBy = factoryd.PgRecommendedBy
