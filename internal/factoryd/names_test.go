package factoryd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var numberedField = regexp.MustCompile(`^v\d+$`)

// removedChangeIDs are the change ids the workflow read once and reads no
// more. Live histories and the recordings hold their markers, so a new
// change takes a new name.
var removedChangeIDs = []string{
	"adjudicated", "alert-precheck", "answer-burst", "answered-routing", "answers", "answer-slice",
	"approval-green", "approval-pending", "attempt-funded", "audit", "audit-beside", "auto-merge",
	"beside-ladder", "burst-accept", "cap-wait", "chain-done", "cheap-turn", "checked-head", "checkin-answer",
	"checkin-grant", "checkin-raise", "checkin-repeat", "checkin-spend", "checks-rerun", "closed-by-human",
	"companions", "conflict-fact", "criteria-gate", "day-cap", "dep-wait", "direct-to-plan", "extend-raise",
	"factory-pr-actions", "factory-pr-card", "factory-pr-findings", "factory-pr-source", "gates", "gates-pinned",
	"gate-started", "gate-supervisor", "gate-supervisor-race", "head-holds", "keep-work", "merge-conflict",
	"merge-failure-reported", "merge-queue", "merge-retry-cap", "needs-info", "persist-usage", "plan",
	"plan-answers", "plan-burst", "planner-retry", "pod-wait", "poster-cap", "processes", "provider-wait",
	"provider-wait-sessions", "queue-turns", "raise-reload", "reconcile-findings", "reconcile-race",
	"red-head-restage", "replan", "resume-hold", "review-holds-merge", "review-parallel", "review-remark",
	"review-verdict", "roll", "roll-gap", "roll-no-queue", "roll-rollout", "screen-answers", "session-cap",
	"session-ladder", "signals-drained", "size-time", "sizing", "split", "stages", "stopped-parent", "successor",
	"supervisor", "tool-park", "trip-routing", "verified-head",
}

func changeIDFindings(dir string) []string {
	var out []string
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return []string{err.Error()}
	}
	fset := token.NewFileSet()
	seen := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "GetVersion" || fmt.Sprint(sel.X) != "workflow" {
					return true
				}
				pos := fset.Position(n.Pos()).String()
				lit, ok := n.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					out = append(out, pos+": the change id is not a string literal")
					return true
				}
				id, _ := strconv.Unquote(lit.Value)
				if slices.Contains(removedChangeIDs, id) {
					out = append(out, fmt.Sprintf("%s: change id %q was removed; live histories hold its marker, so name the change anew", pos, id))
				}
				if first, ok := seen[id]; ok {
					out = append(out, fmt.Sprintf("%s: change id %q again (first at %s)", pos, id, first))
				}
				seen[id] = pos
			case *ast.TypeSpec:
				st, ok := n.Type.(*ast.StructType)
				if !ok || n.Name.Name != "taskRun" {
					return true
				}
				for _, f := range st.Fields.List {
					for _, name := range f.Names {
						if numberedField.MatchString(name.Name) {
							out = append(out, fmt.Sprintf("%s: taskRun field %s; name a new change id's field after it", fset.Position(name.Pos()), name.Name))
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// A new Temporal change id is a name kept in a named taskRun field, never
// vNN: two parallel branches both took v21 in Stage 10.
func TestChangeIDsUnique(t *testing.T) {
	if f := changeIDFindings("."); len(f) > 0 {
		t.Fatalf("shipped workflow: %v", f)
	}
	src, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatal(err)
	}
	const fields = "upstreamCopy      workflow.Version"
	const surge = `run.surge = workflow.GetVersion(ctx, "surge-resume", workflow.DefaultVersion, versionSurgeResume)`
	const wipStart = `run.wipStart = workflow.GetVersion(ctx, "wip-start", workflow.DefaultVersion, versionWipStart)`
	for _, tc := range []struct{ name, old, new, bad string }{
		{"a named field passes", fields, fields + "\n\tmergeQueue workflow.Version", ""},
		{"surge-resume twice", surge, surge + "\n\trun.mergeQueue = " + surge[len("run.surge = "):], `"surge-resume"`},
		{"a repeated change id", wipStart, wipStart + `
	workflow.GetVersion(ctx, "wip-start", workflow.DefaultVersion, versionWipStart)`, `"wip-start"`},
		{"a v24 field", fields, fields + "\n\tv24 workflow.Version", "v24"},
		{"a v2 field", fields, fields + "\n\tv2 workflow.Version", "v2"},
		{"a removed change id", wipStart, wipStart + `
	workflow.GetVersion(ctx, "roll", workflow.DefaultVersion, 1)`, `"roll"`},
		{"a repeated change id in another file", "", "", `"cases"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			planted := string(src)
			if tc.old != "" {
				if !strings.Contains(planted, tc.old) {
					t.Fatalf("workflow.go lacks %q", tc.old)
				}
				planted = strings.Replace(planted, tc.old, tc.new, 1)
			} else {
				other := "package factoryd\n\nimport \"go.temporal.io/sdk/workflow\"\n\nfunc other(ctx workflow.Context) workflow.Version {\n\treturn workflow.GetVersion(ctx, \"cases\", workflow.DefaultVersion, 1)\n}\n"
				if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte(other), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "workflow.go"), []byte(planted), 0o600); err != nil {
				t.Fatal(err)
			}
			f := changeIDFindings(dir)
			switch {
			case tc.bad == "" && len(f) != 0:
				t.Fatalf("findings = %v, want none", f)
			case tc.bad != "" && (len(f) != 1 || !strings.Contains(f[0], tc.bad)):
				t.Fatalf("findings = %v, want one naming %s", f, tc.bad)
			}
		})
	}
}
