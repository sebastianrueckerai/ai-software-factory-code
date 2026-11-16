package factoryd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
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

// modelPaths are the request paths that run a model: the gateway's chat
// completions, and Replicate's predictions, which the replicate_predict
// tool runs.
var modelPaths = []string{"/chat/completions", "/v1/predictions"}

// unreadTransports run a model by a request whose path the walk cannot
// read, each with why.
var unreadTransports = map[string]string{
	"factoryd.(*Runner).runSession":    "it starts a session, whose harness calls the model gateway inside its pod",
	"factoryd.(*Runner).messageWorker": "it messages a running session, whose harness calls the model gateway inside its pod",
	"gateway.(*Fal).Run":               "it posts to the fal.ai endpoint its caller names, a path built at run time",
}

// carriers hand a prompt their caller wrote on to a model call, so the
// walk goes on to their callers: OperatorModel's methods, the review's
// fan-out and retry, the check-in's conversation, the report session, and
// the gateway's handler that relays each client's call to its provider.
var carriers = []string{
	"factoryd.(*OperatorModel).call", "factoryd.(*OperatorModel).stream", "factoryd.(*OperatorModel).Stream",
	"factoryd.(*OperatorModel).Compact", "factoryd.(*OperatorModel).Digest", "factoryd.(*OperatorModel).Research",
	"factoryd.(*OperatorModel).Vision", "factoryd.(*OperatorModel).Title",
	"factoryd.(*Runner).callAll", "factoryd.(*Runner).reviewRetry", "factoryd.converse", "factoryd.(*Runner).runRung",
	"factoryd.(*Runner).reportSession",
	"gateway.(*Server).handleChat",
}

// agentOnlyCalls are the model calls whose text only agents read: the
// function that makes the call, which call, and why.
var agentOnlyCalls = func() []struct{ Call, What, Why string } {
	const consoleTool = "a tool result for the project agent, which the console shows inside the tool block of the chat"
	const sessionTool = "a tool result for the session that called the tool, which a human sees only inside that call's tool block"
	const intoSession = "factoryd's own text into a running session, which writes on by the forms its start gave it"
	calls := []struct{ Call, What, Why string }{
		{"console.(*turn).research", "the research subagent's report", consoleTool},
		{"console.(*turn).lookImage", "the look at an image the human attached", consoleTool},
		{"console.(*turn).summarizeAttachment", "the digest of an attachment", consoleTool},
		{"console.(*turn).withDigest", "the digest of a read", consoleTool},
		{"factoryd.(*Runner).DigestHandler", "the digest tool's summary", sessionTool},
		{"factoryd.(*Runner).ImageLookHandler", "the analyze_image tool's look", sessionTool},
		{"gateway.(*Server).digest", "the digest tool's call", sessionTool},
		{"gateway.(*Server).analyzeImage", "the analyze_image tool's call", sessionTool},
		{"gateway.(*Server).replicate", "the replicate_predict tool's run of a hosted model", sessionTool},
		{"gateway.(*Server).fal", "the fal_run tool's run of a hosted model", sessionTool},
		{"factoryd.(*Runner).message", "the supervisor's guidance", intoSession},
		{"factoryd.(*Runner).nudge", "the nudge after identical calls", intoSession},
		{"factoryd.(*Runner).watchLimits", "the note on a limit", intoSession},
		{"factoryd.(*Runner).RunReport", "the prover's session", "it files tests, and factoryd writes its summary"},
	}
	for _, capture := range []string{"console.agentDocVision", "factoryd.agentDocAnswer", "factoryd.agentDocAudit", "factoryd.agentDocBehaviours",
		"factoryd.agentDocBrief", "factoryd.agentDocDelta", "factoryd.agentDocGateSupervisor", "factoryd.agentDocHoldout",
		"factoryd.agentDocJudgement", "factoryd.agentDocReview", "factoryd.agentDocTitle"} {
		calls = append(calls, struct{ Call, What, Why string }{capture, "the agent map's capture",
			"a capture of a writer against a fake gateway, whose answer no one reads"})
	}
	return calls
}()

// callGraph is what the walk reads of the code's functions, each named by
// its package's directory under internal/ (cmd/ keeps its prefix), its
// receiver and its name: who calls a name, which paths a function sends
// a request to, and which functions serve a path.
type callGraph struct {
	callers  map[string][]callSite
	requests map[string][]string
	routes   map[string][]string // path -> pkg.name of each function its handler names
	funcs    map[string]bool
	params   map[string]int // fn -> its parameters, -1 less for a variadic one
}

// callSite is one call in function fn with args arguments: recv is the
// type its receiver expression names when the walk can tell (the caller's
// own receiver, a composite literal), "" when it cannot, "-" for a plain
// function call.
type callSite struct {
	fn, pos, recv string
	args          int
}

// shortName is fn's name without its package and receiver.
func shortName(fn string) string { return fn[strings.LastIndex(fn, ".")+1:] }

// recvOf is fn's receiver type, "-" for a function.
func recvOf(fn string) string {
	i := strings.Index(fn, ".(")
	if i < 0 {
		return "-"
	}
	return strings.TrimPrefix(fn[i+2:strings.Index(fn, ")")], "*")
}

// pkgOf is fn's package.
func pkgOf(fn string) string { return fn[:strings.Index(fn, ".")] }

func readCallGraph(t *testing.T) callGraph {
	t.Helper()
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	module := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(mod), "\n", 2)[0], "module")) + "/"
	type file struct {
		pkg string
		f   *ast.File
	}
	var files []file
	// values are the package-level constants and variables by package and
	// name: a path a request names through one is read through it
	values := map[string]map[string]ast.Expr{}
	fset := token.NewFileSet()
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			pkg := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(strings.TrimPrefix(p, "../../"))), "internal/")
			files = append(files, file{pkg, f})
			for _, decl := range f.Decls {
				if gd, ok := decl.(*ast.GenDecl); ok {
					for _, spec := range gd.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for i, v := range vs.Values {
								if values[pkg] == nil {
									values[pkg] = map[string]ast.Expr{}
								}
								values[pkg][vs.Names[min(i, len(vs.Names)-1)].Name] = v
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
	g := callGraph{callers: map[string][]callSite{}, requests: map[string][]string{}, routes: map[string][]string{}, funcs: map[string]bool{}, params: map[string]int{}}
	for _, fl := range files {
		pkg := fl.pkg
		imports, local := map[string]bool{}, map[string]string{}
		for _, im := range fl.f.Imports {
			ip, _ := strconv.Unquote(im.Path.Value)
			name := path.Base(ip)
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = true
			if rest, ok := strings.CutPrefix(ip, module); ok {
				local[name] = strings.TrimPrefix(rest, "internal/")
			}
		}
		// paths are the request paths node names: its string literals, and
		// those of the constants and variables of this module it names; a
		// function literal in a constant's or variable's value is code, no
		// path value, while one in a body is the body's own
		var paths func(pkg string, node ast.Node, body bool, seen map[string]bool, skip map[ast.Node]bool) []string
		paths = func(pkg string, node ast.Node, body bool, seen map[string]bool, skip map[ast.Node]bool) []string {
			var out []string
			follow := func(p, name string) {
				if v := values[p][name]; v != nil && !seen[p+"."+name] {
					seen[p+"."+name] = true
					out = append(out, paths(p, v, false, seen, nil)...)
				}
			}
			ast.Inspect(node, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncLit:
					return body
				case *ast.BasicLit:
					if v, _ := strconv.Unquote(x.Value); x.Kind == token.STRING && !skip[x] {
						if _, rest, ok := strings.Cut(v, "://"); ok {
							v = rest[max(0, strings.Index(rest, "/")):]
						}
						if strings.HasPrefix(v, "/") {
							out = append(out, v)
						}
					}
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && local[id.Name] != "" {
						follow(local[id.Name], x.Sel.Name)
						return false
					}
				case *ast.Ident:
					follow(pkg, x.Name)
				}
				return true
			})
			return out
		}
		record := func(fn, self, selfType string, body ast.Node, requests bool) {
			patterns := map[ast.Node]bool{}
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name, recv string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					x, isIdent := fun.X.(*ast.Ident)
					switch {
					case isIdent && local[x.Name] != "":
						name, recv = fun.Sel.Name, "-"
					case !isIdent || !imports[x.Name]:
						name, recv = fun.Sel.Name, typeOf(fun.X, self, selfType)
					}
					if (name == "Handle" || name == "HandleFunc") && len(call.Args) > 0 {
						g.route(pkg, call, imports)
						patterns[call.Args[0]] = true
						return true
					}
				case *ast.Ident:
					name, recv = fun.Name, "-"
				}
				if name != "" {
					g.callers[name] = append(g.callers[name], callSite{pkg + "." + fn, fset.Position(call.Pos()).String(), recv, len(call.Args)})
				}
				return true
			})
			if requests {
				g.requests[pkg+"."+fn] = append(g.requests[pkg+"."+fn], paths(pkg, body, true, map[string]bool{}, patterns)...)
			}
		}
		for _, decl := range fl.f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					self, selfType := receiver(d)
					g.funcs[pkg+"."+funcName(d)] = true
					g.params[pkg+"."+funcName(d)] = params(d.Type)
					record(funcName(d), self, selfType, d.Body, true)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, v := range vs.Values {
							record(vs.Names[min(i, len(vs.Names)-1)].Name, "", "", v, false)
						}
					}
				}
			}
		}
	}
	return g
}

// route records a ServeMux registration in package pkg: its path, without
// the method, served by every function its handler expression names. A
// handler written in place is its registering function's own code.
func (g callGraph) route(pkg string, call *ast.CallExpr, imports map[string]bool) {
	if len(call.Args) < 2 {
		return
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok {
		return
	}
	pattern, _ := strconv.Unquote(lit.Value)
	if _, p, ok := strings.Cut(pattern, " "); ok {
		pattern = p
	}
	ast.Inspect(call.Args[1], func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); !ok || !imports[id.Name] {
				g.routes[pattern] = append(g.routes[pattern], pkg+"."+x.Sel.Name)
			}
		case *ast.Ident:
			g.routes[pattern] = append(g.routes[pattern], pkg+"."+x.Name)
		}
		return true
	})
}

// modelCallSites are the functions that make a model call, each with the
// place of one call, and the carriers the walk passed. The walk starts at
// the transports: a function that sends a request to a model path, an
// unread transport, and a function that sends a request to a path the
// module serves with a function the walk reached, such as the gateway's
// relay to factoryd's digest. From a transport or a carrier it
// goes to every call of its name that passes as many arguments as it
// takes: across the module for an exported name; for an unexported one
// inside its package, and for a method only on a receiver of its type or
// one the walk cannot tell. A carrier walks on,
// any other caller is a site.
func modelCallSites(t *testing.T) (sites map[string]string, passed map[string]bool) {
	g := readCallGraph(t)
	transports := map[string]bool{}
	for fn, why := range unreadTransports {
		if !g.funcs[fn] {
			t.Errorf("unreadTransports holds %s, which the code does not declare (its reason: %s)", fn, why)
		}
		transports[fn] = true
	}
	for fn, paths := range g.requests {
		if slices.ContainsFunc(paths, func(p string) bool {
			return slices.ContainsFunc(modelPaths, func(m string) bool { return strings.HasSuffix(p, m) })
		}) {
			transports[fn] = true
		}
	}
	for {
		sites, passed = map[string]string{}, map[string]bool{}
		queue := slices.Sorted(maps.Keys(transports))
		for len(queue) > 0 {
			fn := queue[0]
			queue = queue[1:]
			name, typ := shortName(fn), recvOf(fn)
			for _, s := range g.callers[name] {
				if !takes(g.params[fn], s.args) {
					continue
				}
				if ast.IsExported(name) || pkgOf(s.fn) == pkgOf(fn) && (s.recv == typ || s.recv == "" && typ != "-") {
					switch {
					case slices.Contains(carriers, s.fn) || transports[s.fn]:
						if !passed[s.fn] {
							passed[s.fn] = true
							queue = append(queue, s.fn)
						}
					default:
						sites[s.fn] = s.pos
					}
				}
			}
		}
		// a handler reaches a function by an exported name from any
		// package, by an unexported one from its own
		reached := map[string]bool{}
		for fn := range sites {
			reached[fn] = true
		}
		for fn := range passed {
			reached[fn] = true
		}
		serves := func(h string) bool {
			pkg, name, _ := strings.Cut(h, ".")
			for fn := range reached {
				if shortName(fn) == name && (ast.IsExported(name) || pkgOf(fn) == pkg) {
					return true
				}
			}
			return false
		}
		grown := false
		for fn, paths := range g.requests {
			for _, p := range paths {
				if !transports[fn] && slices.ContainsFunc(g.routes[p], serves) {
					transports[fn], grown = true, true
				}
			}
		}
		if !grown {
			return sites, passed
		}
	}
}

// funcName is d's name with its receiver, as (*Runner).callAll.
func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	_, typ := receiver(d)
	if _, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
		typ = "*" + typ
	}
	return "(" + typ + ")." + d.Name.Name
}

// params is the parameter count of a function of type ft, as a negative
// count of the parameters before a variadic one.
func params(ft *ast.FuncType) int {
	n := 0
	for _, f := range ft.Params.List {
		if _, ok := f.Type.(*ast.Ellipsis); ok {
			return -n - 1
		}
		n += max(1, len(f.Names))
	}
	return n
}

// takes is whether a function of n params (params) takes args arguments.
func takes(n, args int) bool {
	if n < 0 {
		return args >= -n-1
	}
	return args == n
}

// receiver is d's receiver's name and type, "" for a function.
func receiver(d *ast.FuncDecl) (name, typ string) {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return "", ""
	}
	field := d.Recv.List[0]
	if len(field.Names) > 0 {
		name = field.Names[0].Name
	}
	return name, typeName(field.Type)
}

// typeName is the name of the type expression x.
func typeName(x ast.Expr) string {
	switch e := x.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.IndexExpr:
		return typeName(e.X)
	}
	return "?"
}

// typeOf is the type of the receiver expression x when the walk can tell:
// the caller's own receiver self is of selfType, a composite literal is of
// its type; "" for any other value.
func typeOf(x ast.Expr, self, selfType string) string {
	switch e := x.(type) {
	case *ast.Ident:
		if self != "" && e.Name == self {
			return selfType
		}
	case *ast.ParenExpr:
		return typeOf(e.X, self, selfType)
	case *ast.UnaryExpr:
		return typeOf(e.X, self, selfType)
	case *ast.CompositeLit:
		return typeName(e.Type)
	}
	return ""
}

// Every model call is a writer's, whose prompt carries the forms of its
// jobs, or one whose text only agents read; no list names a function the
// walk does not reach.
func TestWritersHoldsEveryModelCall(t *testing.T) {
	sites, passed := modelCallSites(t)
	held := map[string]bool{}
	for _, w := range Writers {
		for _, c := range w.Calls {
			held[c] = true
			if _, ok := sites[c]; !ok {
				t.Errorf("writer %s names %s, which makes no model call", w.Name, c)
			}
		}
	}
	for _, a := range agentOnlyCalls {
		held[a.Call] = true
		if _, ok := sites[a.Call]; !ok {
			t.Errorf("agentOnlyCalls holds %s for %s, which makes no model call (its reason: %s)", a.Call, a.What, a.Why)
		}
	}
	for _, c := range carriers {
		if !passed[c] {
			t.Errorf("carriers holds %s, which hands on no model call", c)
		}
	}
	for _, c := range slices.Sorted(maps.Keys(sites)) {
		if !held[c] {
			t.Errorf("%s makes a model call at %s: add it to Writers with the forms of its job, to agentOnlyCalls with why only agents read its text, "+
				"or to carriers when it hands its caller's prompt on", c, sites[c])
		}
	}
}
