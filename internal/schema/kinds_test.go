package schema

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// kindConstants reads the string constants of package factoryd whose names
// match pattern from its source: factoryd imports this package, so the test
// cannot import factoryd, and a list kept here would miss the next kind.
func kindConstants(t *testing.T, pattern string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("../factoryd/*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(pattern)
	out := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !re.MatchString(name.Name) || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no constant of factoryd matches %s", pattern)
	}
	return out
}

// Every migration that adds a kind rewrites the whole check, so of two
// parallel ones only the last applied keeps its kind: after all migrations,
// every alert kind, alert verdict, approval kind, roll request state, case
// kind, case status, case step outcome, criteria verdict source and plan
// proof state, origin and flag factoryd names is accepted.
func TestKindChecksAccept(t *testing.T) {
	db, err := sql.Open("pgx", schematest.DSN(t, "schema_kinds"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM alerts; DELETE FROM approval_requests; DELETE FROM roll_requests`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status, track) VALUES ('kinds-verdicts', 5, 'running', 'direct') ON CONFLICT (id) DO NOTHING;
		DELETE FROM criteria_verdicts WHERE task_id = 'kinds-verdicts'`); err != nil {
		t.Fatal(err)
	}
	for name, kind := range kindConstants(t, `^Alert[A-Z]`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO alerts (kind, source, task_id) VALUES ($1, 'kinds', $2)`, kind, "t-"+kind); err != nil {
			t.Errorf("alert kind %s (%q): %v", name, kind, err)
		}
	}
	for name, verdict := range kindConstants(t, `^Verdict[A-Z]`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO alerts (kind, source, task_id, verdict, resolved_at) VALUES ('escalation', 'kinds', $1, $2, now())`, "v-"+verdict, verdict); err != nil {
			t.Errorf("alert verdict %s (%q): %v", name, verdict, err)
		}
	}
	for name, kind := range kindConstants(t, `^Approval[A-Z]`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO approval_requests (kind, task_id, project) VALUES ($1, $2, 'o/r')`, kind, "t-"+kind); err != nil {
			t.Errorf("approval kind %s (%q): %v", name, kind, err)
		}
	}
	// a skip alone has no task
	for name, state := range kindConstants(t, `^Roll[A-Z][a-z]+ed$`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO roll_requests (sha, repository, run_id, state, task_id)
			VALUES ($1, 'o/r', 1, $1, CASE WHEN $1 = 'skipped' THEN NULL ELSE 't-' || $1 END)`, state); err != nil {
			t.Errorf("roll request state %s (%q): %v", name, state, err)
		}
	}
	// a Case constant that is no status is a kind
	statuses := kindConstants(t, `^Case(Open|AtHuman|Resolved|Withdrawn)$`)
	for name, kind := range kindConstants(t, `^Case[A-Z]`) {
		if _, ok := statuses[name]; ok {
			continue
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO cases (task_id, kind, state_key, status, problem) VALUES ($1, $2, 'kinds', 'resolved', 'kinds')`, "t-"+kind, kind); err != nil {
			t.Errorf("case kind %s (%q): %v", name, kind, err)
		}
	}
	for name, status := range statuses {
		if _, err := db.ExecContext(ctx, `INSERT INTO cases (task_id, kind, state_key, status, problem) VALUES ($1, 'other', 'kinds', $2, 'kinds')`, "s-"+status, status); err != nil {
			t.Errorf("case status %s (%q): %v", name, status, err)
		}
	}
	var caseID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO cases (task_id, kind, state_key, status, problem) VALUES ('steps', 'other', 'kinds', 'resolved', 'kinds') RETURNING id`).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	for name, outcome := range kindConstants(t, `^Step(InProgress|Resolved|HandedUp|Withdrawn)$`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO case_steps (case_id, rung, actor, outcome) VALUES ($1, 0, 'kinds', $2)`, caseID, outcome); err != nil {
			t.Errorf("case step outcome %s (%q): %v", name, outcome, err)
		}
	}
	for name, source := range kindConstants(t, `^CriteriaSource[A-Z]`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO criteria_verdicts (task_id, criterion_id, kind, verdict, source) VALUES ('kinds-verdicts', $1, 'test', 'met', $1)`, source); err != nil {
			t.Errorf("criteria verdict source %s (%q): %v", name, source, err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM plan_proofs WHERE task_id = 'kinds-proofs'`); err != nil {
		t.Fatal(err)
	}
	n := 0
	proof := func(kind, name, value, origin, state, flag string) {
		n++
		id, criterion := fmt.Sprintf("p%d", n), ""
		if origin == "criterion" {
			criterion = id
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO plan_proofs (task_id, idx, round, id, origin, criterion, claim, state, flag, path, suite, name, code)
			VALUES ('kinds-proofs', $1, 1, $2, $3, $4, 'c', $5, $6, 'a/a_test.go', './a', 'TestA', 'package a')`, n, id, origin, criterion, state, flag); err != nil {
			t.Errorf("plan proof %s %s (%q): %v", kind, name, value, err)
		}
	}
	for name, state := range kindConstants(t, `^Proof(Proposed|Okayed|Dropped|Filed|Stored|Opened)$`) {
		proof("state", name, state, "hold-out", state, "")
	}
	for name, origin := range kindConstants(t, `^ProofOrigin[A-Z]`) {
		proof("origin", name, origin, origin, "stored", "")
	}
	for name, flag := range kindConstants(t, `^ProofFlag[A-Z]`) {
		proof("flag", name, flag, "hold-out", "stored", flag)
	}
}
