package factoryd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// Every writer's prompt carries the forms of its jobs.
func TestEveryWriterCarriesItsForms(t *testing.T) {
	for _, w := range Writers {
		if len(w.Jobs) == 0 {
			t.Errorf("writer %s has no job", w.Name)
			continue
		}
		if !strings.Contains(w.Prompt(), writingguide.For(w.Jobs...)) {
			t.Errorf("the prompt of writer %s lacks the forms of %v", w.Name, w.Jobs)
		}
	}
}

// modelMethods make a model call: the gateway clients' methods
// (SupervisorModel.Chat, OperatorModel's, the check-in's Call, the digest
// and vision readers), a session's start (runSession) and a message into
// a running session (messageWorker). An unexported name holds its package,
// the one place it can be called from.
var modelMethods = []string{"Chat", "Stream", "Compact", "Digest", "Research", "Vision", "Title", "Call", "factoryd.runSession", "factoryd.messageWorker"}

// carriers hand a prompt their caller wrote on to a model call: their
// callers are the call sites. Each holds its package, as modelMethods.
var carriers = []string{"factoryd.callAll", "factoryd.reviewRetry", "factoryd.converse", "factoryd.runRung", "factoryd.reportSession"}

// agentOnlyCalls are the model calls whose text only agents read, each
// with why.
var agentOnlyCalls = func() map[string]string {
	m := map[string]string{
		"console.(*turn).lookImage":           "the look at an image the human attached goes to the project agent",
		"console.(*turn).research":            "the researcher's report goes to the project agent",
		"console.(*turn).summarizeAttachment": "the digest of an attachment goes to the project agent",
		"console.(*turn).withDigest":          "the digest of a read goes to the project agent",
		"factoryd.(*Runner).DigestHandler":    "the digest tool's summary goes to the agent that called it",
		"factoryd.(*Runner).ImageLookHandler": "the analyze_image tool's look goes to the agent that called it",
		"gateway.(*Server).digest":            "the digest tool's summary goes to the agent that called it",
		"factoryd.(*Runner).message":          "factoryd's guidance into a running session, which writes on by the forms its start gave it",
		"factoryd.(*Runner).nudge":            "factoryd's nudge into a running session, which writes on by the forms its start gave it",
		"factoryd.(*Runner).watchLimits":      "factoryd's note on a limit into a running session, which writes on by the forms its start gave it",
	}
	for _, capture := range []string{"console.agentDocVision", "factoryd.agentDocAnswer", "factoryd.agentDocAudit", "factoryd.agentDocBehaviours",
		"factoryd.agentDocBrief", "factoryd.agentDocDelta", "factoryd.agentDocGateSupervisor", "factoryd.agentDocHoldout",
		"factoryd.agentDocJudgement", "factoryd.agentDocReview", "factoryd.agentDocTitle"} {
		m[capture] = "the agent map's capture of a writer against a fake gateway, whose answer no one reads"
	}
	return m
}()

// modelCallSites are the functions of the code's packages that make a
// model call, each with the place of one call: a call of a model method
// on a value, not on a package, and through a carrier its callers. A
// function is named by its package's directory under internal/, or under
// the module for cmd/, its receiver and its name.
func modelCallSites(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	type site struct{ fn, pos string }
	callers := map[string][]site{}
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			imports := map[string]bool{}
			for _, im := range f.Imports {
				ip, _ := strconv.Unquote(im.Path.Value)
				name := path.Base(ip)
				if im.Name != nil {
					name = im.Name.Name
				}
				imports[name] = true
			}
			pkg := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(strings.TrimPrefix(p, "../../"))), "internal/")
			record := func(fn string, body ast.Node) {
				ast.Inspect(body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					var name string
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						if x, ok := fun.X.(*ast.Ident); ok && imports[x.Name] {
							return true
						}
						name = fun.Sel.Name
					case *ast.Ident:
						name = fun.Name
					}
					callers[name] = append(callers[name], site{pkg + "." + fn, fset.Position(call.Pos()).String()})
					return true
				})
			}
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body != nil {
						record(funcName(d), d.Body)
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for i, v := range vs.Values {
								record(vs.Names[min(i, len(vs.Names)-1)].Name, v)
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sites := map[string]string{}
	var expand func(callee string, seen []string)
	expand = func(callee string, seen []string) {
		pkg, name, local := strings.Cut(callee, ".")
		if !local {
			name = callee
		}
		for _, s := range callers[name] {
			if local && !strings.HasPrefix(s.fn, pkg+".") {
				continue
			}
			fn := s.fn[:strings.Index(s.fn, ".")+1] + s.fn[strings.LastIndex(s.fn, ".")+1:]
			switch {
			case slices.Contains(seen, fn):
			case slices.Contains(carriers, fn):
				expand(fn, append(seen, fn))
			default:
				sites[s.fn] = s.pos
			}
		}
	}
	for _, m := range modelMethods {
		expand(m, nil)
	}
	return sites
}

// funcName is d's name with its receiver, as (*Runner).callAll.
func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	typ, star := d.Recv.List[0].Type, ""
	if s, ok := typ.(*ast.StarExpr); ok {
		typ, star = s.X, "*"
	}
	if g, ok := typ.(*ast.IndexExpr); ok {
		typ = g.X
	}
	recv := "?"
	if id, ok := typ.(*ast.Ident); ok {
		recv = id.Name
	}
	return "(" + star + recv + ")." + d.Name.Name
}

// Every model call is a writer's, whose prompt carries the forms of its
// jobs, or one whose text only agents read; neither list names a function
// that makes no model call.
func TestWritersHoldsEveryModelCall(t *testing.T) {
	sites := modelCallSites(t)
	held := map[string]string{}
	for _, w := range Writers {
		for _, c := range w.Calls {
			held[c] = w.Name
			if _, ok := sites[c]; !ok {
				t.Errorf("writer %s names %s, which makes no model call", w.Name, c)
			}
		}
	}
	for c, why := range agentOnlyCalls {
		if _, ok := sites[c]; !ok {
			t.Errorf("agentOnlyCalls holds %s, which makes no model call (its reason: %s)", c, why)
		}
		if w, ok := held[c]; ok {
			t.Errorf("%s is writer %s's and in agentOnlyCalls (%s)", c, w, why)
		}
	}
	for _, c := range slices.Sorted(maps.Keys(sites)) {
		if _, ok := held[c]; !ok && agentOnlyCalls[c] == "" {
			t.Errorf("%s makes a model call at %s: add it to Writers with the forms of its job, or to agentOnlyCalls with why only agents read its text", c, sites[c])
		}
	}
}
