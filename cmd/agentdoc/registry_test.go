package main

import (
	"context"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The registry is hand-maintained; this keeps its shape honest: every
// entry has exactly one driver, every capture the drivers know has its
// page, and no entry ships without its descriptive lines.
func TestRegistryShape(t *testing.T) {
	ids := make([]string, 0, len(registry))
	for _, a := range registry {
		drivers := 0
		for _, on := range []bool{a.Capture != nil, a.Loop} {
			if on {
				drivers++
			}
		}
		if drivers != 1 {
			t.Errorf("%s: %d drivers, want exactly one", a.ID, drivers)
		}
		if a.ID == "" || a.Job == "" || a.Runs == "" || a.Family == "" || a.Pkg == "" {
			t.Errorf("%s: an inventory line is empty", a.ID)
		}
		if (a.Capture == nil) != (a.Answer == nil) {
			t.Errorf("%s: a direct capture and its answer come together", a.ID)
		}
		if !slices.Contains(ids, a.ID) {
			ids = append(ids, a.ID)
		} else {
			t.Errorf("%s: listed twice", a.ID)
		}
	}
	for _, id := range factoryd.AgentDocLoopIDs() {
		a := agentByID(id)
		if a == nil {
			t.Errorf("loop mode %s has no page", id)
			continue
		}
		if !a.Loop {
			t.Errorf("loop mode %s is not marked Loop", id)
		}
	}
	for _, id := range slices.Concat(factoryd.AgentDocDirectIDs(), console.AgentDocConsoleIDs()) {
		if a := agentByID(id); a == nil || a.Capture == nil {
			t.Errorf("the direct capture %s has no page", id)
		}
	}
}

// noPage are the model calls the map gives no page of their own, each
// with why.
var noPage = map[string]string{
	"factoryd.(*Runner).message":     "the supervisor's guidance rides the running session as its next turn, the turn the message-responder page shows",
	"factoryd.(*Runner).nudge":       "the nudge rides the running session as its next turn, the turn the message-responder page shows",
	"factoryd.(*Runner).watchLimits": "the note on a limit rides the running session as its next turn, the turn the message-responder page shows",
	"gateway.(*Server).replicate":    "the replicate_predict tool runs a hosted model on the session's own arguments, which hold no word of the factory's",
	"gateway.(*Server).fal":          "the fal_run tool runs a hosted model on the session's own arguments, which hold no word of the factory's",
	"gateway.(*Server).digest":       "it hands a session's digest call to factoryd's door, whose call the digest page shows",
	"gateway.(*Server).analyzeImage": "it hands a session's analyze_image call to factoryd's door, whose call the image-look page shows",
}

// captureSites are the map's own captures that call a model client
// themselves: no step of the factory, so they need no page.
var captureSites = []string{"factoryd.agentDocAudit", "factoryd.agentDocBehaviours", "factoryd.agentDocDelta",
	"factoryd.agentDocGateSupervisor", "factoryd.agentDocJudgement", "factoryd.agentDocReview"}

// builderCaptures are the direct pages whose capture drives the step's
// builders and its client instead of the function in its Calls, each with
// why: the function makes more calls than the one the page shows, or
// runs on state no fixture stands in for. Each names one call.
var builderCaptures = map[string]string{
	"model-review":      "ModelReview reads the pull request from the forge and makes the behaviours call and one call per diff part, and the capture makes the first part's call",
	"review-behaviours": "ModelReview makes the behaviours call beside its part calls, and the capture makes the behaviours call alone",
	"security-audit":    "GateAudit reads the pull request from the forge and makes one call per diff part, and the capture makes the first part's call",
	"gate-supervisor":   "SuperviseGate runs in a workflow on a stalled gate's live state, and the capture makes its call on a fixture bundle",
	"judgement":         "judgeArtifacts stores each artifact and records each verdict, and the capture makes its call through callAll on the same judgementCall",
	"delta-planner":     "draftDelta reads the plan's verdicts and files the delta card, and the capture makes its call on a fixture plan",
}

// sessionTransports are the walk's transports that start or message a
// harness session: a loop page's call reaches the model through one.
var sessionTransports = []string{"factoryd.(*Runner).runSession", "factoryd.(*Runner).messageWorker"}

// Every function that makes a model call, as the walk over the module
// finds it, is a call some page's capture drives, one noPage gives a
// reason for, or one of the map's own captures. No entry and no list
// names a function that makes no model call. A loop page names only calls
// that reach the model through a harness session. A direct page's
// capture runs through every call it names (stackOf), unless
// builderCaptures says why it drives the builders instead.
func TestRegistryHoldsEveryModelCall(t *testing.T) {
	sites, err := agentdoc.ModelCalls(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, a := range registry {
		if len(a.Calls) == 0 {
			t.Errorf("%s names no model call its capture drives", a.ID)
		}
		for _, c := range a.Calls {
			held[c] = true
			site, ok := sites[c]
			switch {
			case !ok:
				t.Errorf("%s names %s, which makes no model call", a.ID, c)
			case a.Loop && !slices.Contains(sessionTransports, site.Via):
				t.Errorf("%s is a loop page and names %s, whose call goes through %s, no harness session", a.ID, c, site.Via)
			}
		}
		if a.Capture == nil {
			continue
		}
		stack := stackOf(t, a)
		why, builders := builderCaptures[a.ID]
		for _, c := range a.Calls {
			switch {
			case builders && len(a.Calls) != 1:
				t.Errorf("%s drives builders (%s) and names %d calls, want one", a.ID, why, len(a.Calls))
			case builders && stack[c]:
				t.Errorf("%s's capture runs through %s: drop it from builderCaptures", a.ID, c)
			case !builders && !stack[c]:
				t.Errorf("%s names %s, which its capture never runs through", a.ID, c)
			}
		}
	}
	for c, why := range noPage {
		if _, ok := sites[c]; !ok {
			t.Errorf("noPage holds %s, which makes no model call (its reason: %s)", c, why)
		}
		if held[c] {
			t.Errorf("%s has a page and a reason for none", c)
		}
		held[c] = true
	}
	for _, c := range captureSites {
		if _, ok := sites[c]; !ok {
			t.Errorf("captureSites holds %s, which makes no model call", c)
		}
		held[c] = true
	}
	for _, c := range slices.Sorted(maps.Keys(sites)) {
		if !held[c] {
			t.Errorf("%s makes a model call at %s that no page captures: add a registry entry whose capture drives it, or its reason to noPage", c, sites[c].Pos)
		}
	}
}

// stackRecorder records the functions on the stack of every model call
// it sends, named as the walk names them.
type stackRecorder struct {
	module string
	mu     sync.Mutex
	funcs  map[string]bool
}

func (s *stackRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		pcs := make([]uintptr, 256)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
		s.mu.Lock()
		for more := true; more; {
			var f runtime.Frame
			f, more = frames.Next()
			for _, name := range s.walkNames(f.Function) {
				s.funcs[name] = true
			}
		}
		s.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// closureName is the part of a runtime name a closure adds: its number,
// the numbers of the closures inside it, and the number an inlined
// closure ends on.
var closureName = regexp.MustCompile(`\.(func|gowrap)\d+(\.\d+)*|\.\d+$`)

// walkNames are the functions a runtime frame's name holds, named as the
// walk names them: the package's directory under internal/, the receiver
// and the name. A closure is the function it sits in, and a closure the
// compiler inlined into another names both, as
// pkg.f.func1.(*R).M.1 does.
func (s *stackRecorder) walkNames(fn string) []string {
	pkg, rest, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(fn, s.module), "internal/"), ".")
	var out []string
	for _, part := range closureName.Split(rest, -1) {
		if part = strings.Trim(part, "."); part != "" {
			out = append(out, pkg+"."+part)
		}
	}
	return out
}

// stackOf runs a's capture against the fake gateway and answers every
// function its model call ran through.
func stackOf(t *testing.T, a Agent) map[string]bool {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	module := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(mod), "\n", 2)[0], "module")) + "/"
	rec := &stackRecorder{module: module, funcs: map[string]bool{}}
	g := agentdoc.NewGateway(a.Answer(a.ID))
	defer g.Close()
	v := policy.Version{Commit: "fixture", Tree: policy.Tree{Document: policy.Document{Version: 1,
		Classes: map[string]policy.Class{"coder": {ReviewModel: "big", SupervisorModel: "big", AuditModel: "big", CheckinModel: "big", VisionModel: "vision"}},
		Limits:  policy.Limits{SupervisorTimeout: 10 * time.Second}}}}
	if _, err := a.Capture(context.Background(), a.ID, g.URL, v, &http.Client{Transport: rec}); err != nil {
		t.Fatalf("%s: capture: %v", a.ID, err)
	}
	return rec.funcs
}
