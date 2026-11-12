package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseReviewClaims(t *testing.T) {
	in := `{"verdict":"concerns","summary":"s","findings":["a.go: TasksByPR has no SQL implementation"," ","b.go: helper is dead"],
"claims":[{"finding":0,"kind":"undefined","symbol":"TasksByPR"},{"finding":1,"kind":"undefined","symbol":"Blank"},
{"finding":2,"kind":"uncalled","symbol":"helper"},{"finding":9,"kind":"undefined","symbol":"Gone"},
{"finding":0,"kind":"undefined","symbol":"x; rm -rf"},{"finding":0,"kind":"bogus","symbol":"Y"}]}`
	rv := parseReview(in)
	want := []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "TasksByPR"}, {Finding: 1, Kind: ClaimUncalled, Symbol: "helper"}}
	if !slices.Equal(rv.Claims, want) || len(rv.Findings) != 2 {
		t.Fatalf("claims = %+v over %d findings, want %+v over 2", rv.Claims, len(rv.Findings), want)
	}
	if got := parseReview(`{"verdict":"approve","summary":"s","dropped":[{"finding":"x"}]}`); got.Verdict != "" || !strings.Contains(got.Note, "dropped is not an answer field") {
		t.Fatalf("a smuggled drop = %+v", got)
	}
}

func TestCombineReviewsOffsetsClaims(t *testing.T) {
	got := combineReviews([]Review{
		{Verdict: "concerns", Summary: "a", Findings: []string{"f0"}, Claims: []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "A"}}},
		{Verdict: "concerns", Summary: "b", Findings: []string{"f1", "f2"}, Claims: []Claim{{Finding: 1, Kind: ClaimUncalled, Symbol: "C"}}},
	})
	if want := []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "A"}, {Finding: 2, Kind: ClaimUncalled, Symbol: "C"}}; !slices.Equal(got.Claims, want) {
		t.Fatalf("claims = %+v, want %+v", got.Claims, want)
	}
}

func TestLastReviewCarriesNoClaimsOrDrops(t *testing.T) {
	last, _ := lastReview([]Review{{Verdict: "concerns", Summary: "s", Findings: []string{"a.go: x"}, Claims: []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "X"}},
		Dropped: []Dropped{{Finding: "y"}}, Unsettled: []string{"z"}, GateCriteria: &GateLeft{IDs: []string{"c1"}, Integration: true}}}, [][]fileDiff{nil})
	if last.Claims != nil || last.Dropped != nil || last.Unsettled != nil || last.GateCriteria != nil {
		t.Fatalf("last review = %+v", last)
	}
}

func TestContradiction(t *testing.T) {
	hit := func(ref, text string) grepHit { return grepHit{Ref: ref, Path: "internal/s.go", Line: 12, Text: text} }
	undefined, uncalled := Claim{Finding: 0, Kind: ClaimUndefined, Symbol: "TasksByPR"}, Claim{Finding: 0, Kind: ClaimUncalled, Symbol: "docRootOK"}
	for name, tc := range map[string]struct {
		claim   Claim
		hits    []grepHit
		removed bool
		want    string
	}{
		"method declared at head":           {undefined, []grepHit{hit("head", "func (s *PG) TasksByPR(ctx context.Context) {")}, false, "internal/s.go:12 at head: func (s *PG) TasksByPR"},
		"declared on main only":             {undefined, []grepHit{hit("main", "func TasksByPR() {")}, false, "internal/s.go:12 at main: func TasksByPR"},
		"main only and the diff removes it": {undefined, []grepHit{hit("main", "func TasksByPR() {")}, true, ""},
		"a call is no definition":           {undefined, []grepHit{hit("head", "\tx := s.TasksByPR(ctx)")}, false, ""},
		"no hit":                            {undefined, nil, false, ""},
		"a call at head":                    {uncalled, []grepHit{hit("head", "\tif !docRootOK(p) {")}, false, "internal/s.go:12 at head: \tif !docRootOK"},
		"only the declaration":              {uncalled, []grepHit{hit("head", "func docRootOK(p string) bool {")}, false, ""},
		"only a comment":                    {uncalled, []grepHit{hit("head", "// docRootOK checks the root")}, false, ""},
		"mention only in a mid-line comment": {
			uncalled, []grepHit{hit("head", "\tx := 1 /* docRootOK guards it */")}, false, ""},
		"a call before a trailing comment": {
			uncalled, []grepHit{hit("head", "docRootOK(p) /* fast */")}, false, "internal/s.go:12 at head: docRootOK(p) /* fast */"},
		"code after a leading comment": {
			uncalled, []grepHit{hit("head", "/* shared */ docRootOK(p)")}, false, "internal/s.go:12 at head: /* shared */ docRootOK(p)"},
		"declaration after a leading comment": {
			undefined, []grepHit{hit("head", "/* moved */ func TasksByPR() {}")}, false, "internal/s.go:12 at head: /* moved */ func TasksByPR() {}"},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := contradiction(tc.claim, tc.hits, tc.removed)
			if ok != (tc.want != "") || !strings.HasPrefix(got, tc.want) {
				t.Fatalf("contradiction = %q, %v, want prefix %q", got, ok, tc.want)
			}
		})
	}
}

func TestRemovesSymbol(t *testing.T) {
	diff := "diff --git a/TasksByPR.go b/TasksByPR.go\n--- a/TasksByPR.go\n+++ b/TasksByPR.go\n@@ -1,2 +1,1 @@\n-func TasksByPR() {}\n context\n"
	if !removesSymbol(diff, "TasksByPR") || removesSymbol(diff, "Other") {
		t.Fatal("removesSymbol must see the removed line and nothing else")
	}
	if removesSymbol("--- a/TasksByPR.go\n+++ b/TasksByPR.go\n@@ -1 +1 @@\n+TasksByPR()\n", "TasksByPR") {
		t.Fatal("a file header and an added line remove nothing")
	}
	callDiff := "diff --git a/s.go b/s.go\n--- a/s.go\n+++ b/s.go\n@@ -1,3 +1,2 @@\n func helper() {}\n-\thelper()\n context\n"
	if removesSymbol(callDiff, "helper") {
		t.Fatal("a removed call is not a removed declaration")
	}
	declDiff := "diff --git a/s.go b/s.go\n--- a/s.go\n+++ b/s.go\n@@ -1,2 +1,1 @@\n-func (s *PG) helper() {}\n context\n"
	if !removesSymbol(declDiff, "helper") {
		t.Fatal("a removed method declaration removes the symbol")
	}
}

func TestContradictionCountsMainWhenTheDiffRemovesOnlyACall(t *testing.T) {
	diff := "diff --git a/s.go b/s.go\n--- a/s.go\n+++ b/s.go\n@@ -1,2 +1,1 @@\n-\thelper()\n context\n"
	rv := Review{Verdict: "concerns", Findings: []string{"helper is uncalled"}, Claims: []Claim{{Finding: 0, Kind: ClaimUncalled, Symbol: "helper"}}}
	got := applyClaims(rv, splitDiff(diff), func(string) ([]grepHit, error) { return mainHit("s.go", 2, "\thelper()"), nil }, nil)
	if len(got.Findings) != 0 || len(got.Dropped) != 1 || got.Dropped[0].Evidence != "s.go:2 at main: \thelper()" {
		t.Fatalf("review = %+v, want the finding refuted by main's call", got)
	}
}

func mainHit(path string, line int, text string) []grepHit {
	return []grepHit{{Ref: "main", Path: path, Line: line, Text: text}}
}

func TestApplyClaimsDropsAContradictedFinding(t *testing.T) {
	rv := Review{Verdict: "concerns", Summary: "s", Findings: []string{"keep", "gone"}, Claims: []Claim{{Finding: 1, Kind: ClaimUndefined, Symbol: "TasksByPR"}}}
	got := applyClaims(rv, nil, func(string) ([]grepHit, error) {
		return mainHit("internal/s.go", 7, "func (s *PG) TasksByPR() {"), nil
	}, nil)
	if !slices.Equal(got.Findings, []string{"keep"}) || got.Claims != nil || got.Verdict != "concerns" {
		t.Fatalf("review = %+v", got)
	}
	want := Dropped{Finding: "gone", Kind: ClaimUndefined, Symbol: "TasksByPR", Evidence: "internal/s.go:7 at main: func (s *PG) TasksByPR() {"}
	if len(got.Dropped) != 1 || got.Dropped[0] != want {
		t.Fatalf("dropped = %+v, want %+v", got.Dropped, want)
	}
}

func TestApplyClaimsKeepsAFindingWhenTheGrepFails(t *testing.T) {
	rv := Review{Verdict: "concerns", Findings: []string{"f"}, Claims: []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "X"}}}
	got := applyClaims(rv, nil, func(string) ([]grepHit, error) { return nil, os.ErrDeadlineExceeded }, nil)
	if !slices.Equal(got.Findings, []string{"f"}) || got.Dropped != nil {
		t.Fatalf("review = %+v, want the finding kept", got)
	}
}

func TestApplyClaimsDropsOnlyWhenEveryClaimIsContradicted(t *testing.T) {
	rv := Review{Verdict: "concerns", Findings: []string{"A and B are undefined"}, Claims: []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "A"}, {Finding: 0, Kind: ClaimUndefined, Symbol: "B"}}}
	got := applyClaims(rv, nil, func(sym string) ([]grepHit, error) {
		if sym == "A" {
			return mainHit("a.go", 1, "func A() {"), nil
		}
		return nil, nil
	}, nil)
	if len(got.Findings) != 1 || got.Dropped != nil {
		t.Fatalf("review = %+v, want the finding kept: B has no definition", got)
	}
}

func TestContradictionIgnoresMainWhenTheDiffRemovesTheSymbol(t *testing.T) {
	diff := "diff --git a/s.go b/s.go\n--- a/s.go\n+++ b/s.go\n@@ -1,2 +1,1 @@\n-func TasksByPR() {}\n context\n"
	rv := Review{Verdict: "concerns", Findings: []string{"TasksByPR is gone"}, Claims: []Claim{{Finding: 0, Kind: ClaimUndefined, Symbol: "TasksByPR"}}}
	got := applyClaims(rv, splitDiff(diff), func(string) ([]grepHit, error) { return mainHit("s.go", 1, "func TasksByPR() {}"), nil }, nil)
	if len(got.Findings) != 1 || got.Dropped != nil {
		t.Fatalf("review = %+v, want the true finding kept", got)
	}
}

// gitFixture is a repository whose main defines Sym at a.go:3 and whose
// branch head adds a call at b.go:2 and onlyHead's declaration at b.go:3.
func gitFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := git(ctx, dir, "", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...); err != nil {
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

func TestGitGrepFindsTheSymbolAtEachRef(t *testing.T) {
	dir := gitFixture(t)
	got, err := gitGrep(context.Background(), dir, []namedRef{{"head", "head"}, {"main", "main"}}, "Sym")
	if err != nil {
		t.Fatal(err)
	}
	want := []grepHit{{"head", "a.go", 3, "func Sym() {}"}, {"head", "b.go", 2, "var _ = Sym"}, {"main", "a.go", 3, "func Sym() {}"}}
	if !slices.Equal(got, want) {
		t.Fatalf("hits = %+v, want %+v", got, want)
	}
	if none, err := gitGrep(context.Background(), dir, []namedRef{{"head", "head"}}, "Absent"); !errors.Is(err, errNoMatch) || len(none) != 0 {
		t.Fatalf("no match = %+v, %v, want no hits and the no-match answer", none, err)
	}
	mixed, err := gitGrep(context.Background(), dir, []namedRef{{"main", "main"}, {"head", "head"}}, "onlyHead")
	if err != nil || !slices.Equal(mixed, []grepHit{{"head", "b.go", 3, "var onlyHead = 0"}}) {
		t.Fatalf("a head-only symbol = %+v, %v, want the head hit and no error", mixed, err)
	}
	if _, err := gitGrep(context.Background(), dir, []namedRef{{"head", "nosuchref"}}, "Sym"); err == nil || errors.Is(err, errNoMatch) {
		t.Fatalf("a broken ref = %v, want an execution error", err)
	}
}

func TestModelReviewDropsAFindingTheGrepContradicts(t *testing.T) {
	dir := gitFixture(t)
	old := checkoutRefs
	checkoutRefs = func(context.Context, *Forgejo, Repo, int64, string) (string, []namedRef, func(), error) {
		return dir, []namedRef{{"head", "head"}, {"main", "main"}}, func() {}, nil
	}
	t.Cleanup(func() { checkoutRefs = old })
	fj := &gateForgejo{diff: "diff --git a/b.go b/b.go\n--- /dev/null\n+++ b/b.go\n@@ -0,0 +1,2 @@\n+package a\n+var _ = Sym\n"}
	r, _, objects := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"Sym is missing","findings":["b.go: Sym is never defined"],"claims":[{"finding":0,"kind":"undefined","symbol":"Sym"}]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{Policy: PolicyGate{Class: "normal"}})
	rv := getInto[Review](t, val, err)
	if rv.Verdict != "concerns" || len(rv.Findings) != 0 || len(rv.Dropped) != 1 || rv.Dropped[0].Evidence != "a.go:3 at head: func Sym() {}" {
		t.Fatalf("review = %+v, want the finding dropped with a.go:3 at head", rv)
	}
	if !strings.Contains(timeline(t, objects), `"dropped":[{"finding":"b.go: Sym is never defined"`) {
		t.Fatalf("the model_review step lacks the drop: %s", timeline(t, objects))
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "Dropped by code") || !strings.Contains(fj.comments[0], "a.go:3 at head") {
		t.Fatalf("comments = %q", fj.comments)
	}
}

func TestModelReviewKeepsAClaimedFindingWhenTheCheckoutFails(t *testing.T) {
	old := checkoutRefs
	checkoutRefs = func(context.Context, *Forgejo, Repo, int64, string) (string, []namedRef, func(), error) {
		return "", nil, nil, errors.New("forgejo down")
	}
	t.Cleanup(func() { checkoutRefs = old })
	r, _, _ := newGateRunner(t, gateTask, &gateForgejo{diff: "diff --git a/b.go b/b.go\n--- /dev/null\n+++ b/b.go\n@@ -0,0 +1 @@\n+package a\n"})
	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"s","findings":["b.go: Sym is never defined"],"claims":[{"finding":0,"kind":"undefined","symbol":"Sym"}]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{Policy: PolicyGate{Class: "normal"}})
	rv := getInto[Review](t, val, err)
	if len(rv.Findings) != 1 || rv.Dropped != nil || rv.Claims != nil {
		t.Fatalf("review = %+v, want the finding kept and the claims spent", rv)
	}
}

// git grep output past its cap fails the call rather than returning a
// cut-off list.
func TestGitGrepFailsPastItsOutputCap(t *testing.T) {
	old := grepLimits
	grepLimits.maxOut = 1 << 10
	t.Cleanup(func() { grepLimits = old })
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	wipRun(t, "init", "-q", "-b", "main", dir)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(strings.Repeat("Sym()\n", 500)), 0o644)
	wipCommit(t, dir, "x")
	if _, err := gitGrep(context.Background(), dir, []namedRef{{"main", "main"}}, "Sym"); !errors.Is(err, errGitOutput) {
		t.Fatalf("gitGrep = %v, want the output cap's failure", err)
	}
}

func testClaim(finding int, name, pkg, ref, outcome string) Claim {
	return Claim{Finding: finding, Kind: ClaimTestOutcome, Symbol: name, Package: pkg, Ref: ref, Outcome: outcome}
}

func TestValidTestOutcomeClaim(t *testing.T) {
	good := testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)
	for name, c := range map[string]Claim{
		"plain":           good,
		"subtest":         testClaim(0, "TestY/case_1-a.b", "./internal/x", RefHead, OutcomeFail),
		"root package":    testClaim(0, "TestY", ".", RefHead, OutcomeFail),
		"symbol claim":    {Kind: ClaimUndefined, Symbol: "Foo"},
		"head fail claim": testClaim(2, "TestÜber", "a/b-c_d.e", RefHead, OutcomeFail),
	} {
		if !validClaim(c) {
			t.Errorf("%s: %+v refused", name, c)
		}
	}
	for name, c := range map[string]Claim{
		"shell in name":     testClaim(0, "Test; rm -rf /", "internal/x", RefBase, OutcomePass),
		"not a test":        testClaim(0, "helper", "internal/x", RefBase, OutcomePass),
		"subshell in name":  testClaim(0, "Test$(id)", "internal/x", RefBase, OutcomePass),
		"newline in name":   testClaim(0, "TestY\nx", "internal/x", RefBase, OutcomePass),
		"empty subtest":     testClaim(0, "TestY/", "internal/x", RefBase, OutcomePass),
		"dotdot package":    testClaim(0, "TestY", "../x", RefBase, OutcomePass),
		"inner dotdot":      testClaim(0, "TestY", "a/../../x", RefBase, OutcomePass),
		"absolute package":  testClaim(0, "TestY", "/etc", RefBase, OutcomePass),
		"flag package":      testClaim(0, "TestY", "-exec=id", RefBase, OutcomePass),
		"shell in package":  testClaim(0, "TestY", "x;id", RefBase, OutcomePass),
		"wildcard package":  testClaim(0, "TestY", "./...", RefBase, OutcomePass),
		"wildcard suffix":   testClaim(0, "TestY", "internal/x...", RefBase, OutcomePass),
		"empty package":     testClaim(0, "TestY", "", RefBase, OutcomePass),
		"ref main":          testClaim(0, "TestY", "internal/x", "main", OutcomePass),
		"ref empty":         testClaim(0, "TestY", "internal/x", "", OutcomePass),
		"outcome skip":      testClaim(0, "TestY", "internal/x", RefBase, "skip"),
		"symbol with a ref": {Kind: ClaimUndefined, Symbol: "Foo", Ref: RefBase},
		"unknown kind":      {Kind: "bogus", Symbol: "Foo"},
	} {
		if validClaim(c) {
			t.Errorf("%s: %+v accepted", name, c)
		}
	}
}

func TestTestArgvIsFixed(t *testing.T) {
	for _, tc := range []struct {
		c    Claim
		want []string
	}{
		{testClaim(0, "TestY", "internal/x", RefBase, OutcomePass), []string{"go", "test", "-count=1", "-timeout", "0", "-run", "^TestY$", "./internal/x"}},
		{testClaim(0, "TestY/a.b-c", "./internal/x", RefHead, OutcomePass), []string{"go", "test", "-count=1", "-timeout", "0", "-run", `^TestY$/^a\.b-c$`, "./internal/x"}},
		{testClaim(0, "TestY", ".", RefHead, OutcomePass), []string{"go", "test", "-count=1", "-timeout", "0", "-run", "^TestY$", "./"}},
	} {
		if got := testArgv(tc.c); !slices.Equal(got, tc.want) {
			t.Errorf("argv of %+v = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestParseReviewKeepsTestOutcomeClaims(t *testing.T) {
	in := `{"verdict":"concerns","summary":"s","findings":["a","b","c"],"claims":[
{"finding":0,"kind":"test","symbol":"TestY","package":"internal/x","ref":"base","outcome":"pass"},
{"finding":1,"kind":"test","symbol":"Test; rm -rf /","package":"internal/x","ref":"base","outcome":"pass"},
{"finding":2,"kind":"test","symbol":"TestY","package":"../x","ref":"base","outcome":"pass"},
{"finding":2,"kind":"test","symbol":"TestY","package":"x","ref":"main","outcome":"pass"}]}`
	rv := parseReview(in)
	if want := []Claim{testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)}; !slices.Equal(rv.Claims, want) || len(rv.Findings) != 3 {
		t.Fatalf("claims = %+v over %d findings, want %+v over 3", rv.Claims, len(rv.Findings), want)
	}
}

func TestApplyClaimsSettlesTestOutcomes(t *testing.T) {
	var ran []Claim
	run := func(exit int, out string) func(Claim) (claimRun, error) {
		return func(c Claim) (claimRun, error) {
			ran = append(ran, c)
			return claimRun{Commit: "abc", Exit: exit, Output: out}, nil
		}
	}
	base := testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)
	// the finding says the test passes at base; base exits 1: dropped with the run
	got := applyClaims(Review{Findings: []string{"Y passes at base"}, Claims: []Claim{base}}, nil, nil, run(1, "--- FAIL: TestY\nFAIL"))
	if len(got.Findings) != 0 || len(got.Dropped) != 1 || got.Unsettled != nil {
		t.Fatalf("review = %+v, want the finding dropped", got)
	}
	if want := "go test -run TestY internal/x at base (abc): exit 1, the test did not pass:\n--- FAIL: TestY\nFAIL"; got.Dropped[0].Evidence != want {
		t.Fatalf("evidence = %q, want %q", got.Dropped[0].Evidence, want)
	}
	// base exits 0: the finding stands
	got = applyClaims(Review{Findings: []string{"Y passes at base"}, Claims: []Claim{base}}, nil, nil, run(0, "ok"))
	if len(got.Findings) != 1 || got.Dropped != nil || got.Unsettled != nil {
		t.Fatalf("review = %+v, want the finding kept", got)
	}
	// fails at head, head exits 0: dropped
	head := testClaim(0, "TestY", "internal/x", RefHead, OutcomeFail)
	got = applyClaims(Review{Findings: []string{"Y fails at head"}, Claims: []Claim{head}}, nil, nil, run(0, "ok"))
	if len(got.Findings) != 0 || len(got.Dropped) != 1 || !strings.Contains(got.Dropped[0].Evidence, "at head (abc): exit 0, the test did not fail") {
		t.Fatalf("review = %+v, want the finding dropped", got)
	}
	// fails at head, head exits 1: stands
	got = applyClaims(Review{Findings: []string{"Y fails at head"}, Claims: []Claim{head}}, nil, nil, run(1, "FAIL"))
	if len(got.Findings) != 1 || got.Dropped != nil {
		t.Fatalf("review = %+v, want the finding kept", got)
	}
}

func TestApplyClaimsLeavesUnsettledRunsStanding(t *testing.T) {
	base := testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)
	for name, tc := range map[string]struct {
		run  claimRun
		err  error
		want string
	}{
		"cannot start":        {err: errors.New("no sandbox"), want: "Y passes: TestY in internal/x at base could not be run: no sandbox"},
		"timed out":           {run: claimRun{Exit: -1, Output: "(timed out after 10m0s)"}, want: "Y passes: TestY in internal/x at base is unsettled: the run did not finish (exit -1)"},
		"build failed":        {run: claimRun{Exit: 1, Output: "FAIL\tx [build failed]"}, want: "Y passes: TestY in internal/x at base is unsettled: the package did not build"},
		"setup failed":        {run: claimRun{Exit: 1, Output: "FAIL\tx [setup failed]"}, want: "Y passes: TestY in internal/x at base is unsettled: the package did not build"},
		"killed at its bound": {run: claimRun{Exit: -1, Output: "--- FAIL: TestY\nFAIL\tx\t0.1s\n(timed out after 10m0s)"}, want: "Y passes: TestY in internal/x at base is unsettled: the run did not finish (exit -1)"},
		"no test files":       {run: claimRun{Exit: 0, Output: "?   \tx\t[no test files]"}, want: "Y passes: TestY in internal/x at base is unsettled: no such test at that ref"},
		"test not there":      {run: claimRun{Exit: 0, Output: "ok  \tx\t0.01s [no tests to run]"}, want: "Y passes: TestY in internal/x at base is unsettled: no such test at that ref"},
	} {
		got := applyClaims(Review{Findings: []string{"Y passes"}, Claims: []Claim{base}}, nil, nil, func(Claim) (claimRun, error) { return tc.run, tc.err })
		if len(got.Findings) != 1 || got.Dropped != nil || !slices.Equal(got.Unsettled, []string{tc.want}) {
			t.Errorf("%s: review = %+v, want the finding kept, unsettled %q", name, got, tc.want)
		}
	}
}

func TestApplyClaimsRunsTwoTestClaimsAtMost(t *testing.T) {
	runs := 0
	settle := func(Claim) (claimRun, error) { runs++; return claimRun{Exit: 1, Output: "FAIL"}, nil }
	var claims []Claim
	for i := range 3 {
		claims = append(claims, testClaim(i, "TestY", "internal/x", RefBase, OutcomePass))
	}
	got := applyClaims(Review{Findings: []string{"f0", "f1", "f2"}, Claims: claims}, nil, nil, settle)
	if runs != 2 || !slices.Equal(got.Findings, []string{"f2"}) || len(got.Dropped) != 2 ||
		!slices.Equal(got.Unsettled, []string{"f2: TestY in internal/x at base was not run: at most 2 test runs for one review"}) {
		t.Fatalf("runs = %d, review = %+v; want two runs, f0 and f1 dropped, f2 standing unsettled", runs, got)
	}
}

func TestDropContradictedRunsTheClaimAsAnArgvInTheSandbox(t *testing.T) {
	kube := newFakeKube()
	r, _, _ := newTestRunner(t, gateTask, kube, &fakeObjectStore{})
	rv := Review{Verdict: "concerns", Findings: []string{"Y passes at base", "Y fails at head", "Z passes at base"}, Claims: []Claim{
		testClaim(0, "TestY/sub", "internal/x", RefBase, OutcomePass),
		testClaim(1, "TestY", "internal/x", RefHead, OutcomeFail),
		testClaim(2, "TestZ", "internal/z", RefBase, OutcomePass),
	}}
	secret := "pushing to https://user:pass@host.example/r.git\nAuthorization: Bearer abcdef0123456789\n--- FAIL: TestY/sub"
	var got Review
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = r.dropContradicted(context.Background(), gateTask, Repo{Owner: "sf", Name: "repo"}, 9, "c1", time.Minute, 0, rv, nil)
	}()
	scriptReproPods(kube, reproLine(1, secret), reproLine(0, "ok"))
	<-done
	if len(kube.podOrder) != 2 {
		t.Fatalf("pods = %d, want 2: the third claim is not run", len(kube.podOrder))
	}
	if !slices.Equal(got.Findings, []string{"Z passes at base"}) || len(got.Dropped) != 2 || len(got.Unsettled) != 1 {
		t.Fatalf("review = %+v", got)
	}
	ev := got.Dropped[0].Evidence
	if !strings.HasPrefix(ev, "go test -run TestY/sub internal/x at base (base): exit 1, the test did not pass:\n") ||
		strings.Contains(ev, "user:pass") || strings.Contains(ev, "abcdef0123456789") || !strings.Contains(ev, "--- FAIL: TestY/sub") {
		t.Fatalf("evidence = %q, want the run with both secrets masked", ev)
	}
	spec, _ := json.Marshal(kube.podAt(0).spec)
	for _, want := range []string{`{"name":"REPRO_ARGV","value":"[\"go\",\"test\",\"-count=1\",\"-timeout\",\"0\",\"-run\",\"^TestY$/^sub$\",\"./internal/x\"]"}`,
		`{"name":"CHECKOUT_SHA","value":"base"}`, `{"name":"MODE","value":"repro"}`} {
		if !strings.Contains(string(spec), want) {
			t.Fatalf("base pod spec lacks %s: %s", want, spec)
		}
	}
	if strings.Contains(string(spec), "REPRO_COMMAND") {
		t.Fatalf("pod spec carries a shell command: %s", spec)
	}
	if !strings.Contains(string(spec), `{"name":"OVERLAY_PKG","value":"internal/x"}`) || !strings.Contains(string(spec), `"-timeout\",\"0\"`) {
		t.Fatalf("base pod spec lacks the overlay of the claimed package or go's timeout: %s", spec)
	}
	if head, _ := json.Marshal(kube.podAt(1).spec); strings.Contains(string(head), "OVERLAY_PKG") {
		t.Fatalf("head pod spec overlays tests: %s", head)
	}
	if head, _ := json.Marshal(kube.podAt(1).spec); strings.Contains(string(head), "CHECKOUT_SHA") || !strings.Contains(string(head), `{"name":"REPO_SHA","value":"c1"}`) {
		t.Fatalf("head pod spec = %s, want the head commit and no base checkout", head)
	}
}

func TestDropContradictedKeepsTheFindingWhenTheSandboxCannotStart(t *testing.T) {
	kube := newFakeKube()
	r, _, _ := newTestRunner(t, gateTask, kube, &fakeObjectStore{})
	rv := Review{Verdict: "concerns", Findings: []string{"Y passes at base"}, Claims: []Claim{testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)}}
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: "start failed: egress probe: https://user:pass@host.example/x answered 503; Authorization: Bearer abcdef0123456789", Step: "egress probe"}})
	var got Review
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = r.dropContradicted(context.Background(), gateTask, Repo{Owner: "sf", Name: "repo"}, 9, "c1", time.Minute, 0, rv, nil)
	}()
	scriptReproPods(kube, string(b))
	<-done
	if len(got.Findings) != 1 || got.Dropped != nil || len(got.Unsettled) != 1 || !strings.Contains(got.Unsettled[0], "could not be run") ||
		strings.Contains(got.Unsettled[0], "user:pass") || strings.Contains(got.Unsettled[0], "abcdef0123456789") {
		t.Fatalf("review = %+v, want the finding kept and unsettled", got)
	}
}

// go test splits -run at "/" and matches each element unanchored: a
// subtest claim must not run another top-level test that shares the prefix.
func TestTestArgvRunsOnlyTheNamedSubtest(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module m\n\ngo 1.21\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestY(t *testing.T) { t.Run(\"sub\", func(t *testing.T) {}); t.Run(\"subx\", func(t *testing.T) { t.Fatal(\"subx\") }) }\n\nfunc TestYZ(t *testing.T) { t.Fatal(\"TestYZ ran\") }\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	argv := testArgv(testClaim(0, "TestY/sub", "a", RefHead, OutcomePass))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%q: %v\n%s", argv, err, out)
	}
}

func TestReviewCommentFencesTheRunsOutputAndListsUnsettled(t *testing.T) {
	rv := Review{Verdict: "concerns", Summary: "s", Dropped: []Dropped{{Finding: "Y passes", Evidence: "go test -run TestY a at base (abc): exit 1, the test did not pass:\n--- FAIL: TestY\nFAIL"}},
		Unsettled: []string{"Z passes: TestZ in a at head could not be run: no sandbox"}}
	got := reviewComment("m", "c1", rv, "Read.")
	for _, want := range []string{"\n- Dropped by code, the repository contradicts it (go test -run TestY a at base (abc): exit 1, the test did not pass:): Y passes\n\n```\n--- FAIL: TestY\nFAIL\n```\n",
		"\n- Not settled by a run: Z passes: TestZ in a at head could not be run: no sandbox"} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment lacks %q:\n%s", want, got)
		}
	}
}

func TestModelReviewRecordsUnsettledTestClaims(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/b.go b/b.go\n--- /dev/null\n+++ b/b.go\n@@ -0,0 +1 @@\n+package a\n",
		mintError: `{"message":"denied for https://user:pass@host.example/x with Authorization: Bearer abcdef0123456789"}`}
	r, _, objects := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"s","findings":["TestY passes at base"],"claims":[{"finding":0,"kind":"test","symbol":"TestY","package":"internal/x","ref":"base","outcome":"pass"}]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{Policy: PolicyGate{Class: "normal"}})
	rv := getInto[Review](t, val, err)
	if len(rv.Findings) != 1 || len(rv.Unsettled) != 1 || !strings.HasPrefix(rv.Unsettled[0], "TestY passes at base: TestY in internal/x at base could not be run: ") {
		t.Fatalf("review = %+v, want the finding kept and unsettled", rv)
	}
	for _, secret := range []string{"user:pass", "abcdef0123456789"} {
		if strings.Contains(rv.Unsettled[0], secret) || strings.Contains(timeline(t, objects), secret) || strings.Contains(strings.Join(fj.comments, "\n"), secret) {
			t.Fatalf("%s reached the review, the step or the comment", secret)
		}
	}
	step := timelineStep(t, objects, "t1", "model_review")
	if u, _ := step["unsettled"].([]any); len(u) != 1 || u[0] != rv.Unsettled[0] {
		t.Fatalf("model_review step = %v, want the unsettled line", step)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "Not settled by a run: TestY passes at base: TestY in internal/x at base could not be run: forgejo POST") {
		t.Fatalf("comments = %q", fj.comments)
	}
}

func TestReviewSchemaNamesTestClaims(t *testing.T) {
	if !strings.Contains(reviewFormat, "or that a named Go test passes or fails at the base or at the head") {
		t.Fatal("the schema's opening sentence on claims does not name test claims")
	}
}

// The settle tells the gate watch the waves of its calls so far and the
// waves its runs add, in the supervisor timeout's own unit: 2 runs of
// (10 min start + 10 min command) and one 10 min setup are 3000 s: 100
// waves of 30 s, ceil(66.7) = 67 of 45 s, 50 of a minute.
func TestDropContradictedRaisesTheGateWatchBound(t *testing.T) {
	type tc struct {
		call time.Duration
		told int
	}
	for c, want := range map[tc]int{
		{time.Minute, 0}:                      policy.ReviewParts + 50,
		{time.Minute, policy.ReviewParts + 7}: policy.ReviewParts + 57,
		{30 * time.Second, 0}:                 policy.ReviewParts + 100,
		{45 * time.Second, 0}:                 policy.ReviewParts + 67,
	} {
		kube := newFakeKube()
		r, _, _ := newTestRunner(t, gateTask, kube, &fakeObjectStore{})
		var waves int
		ctx := context.WithValue(context.Background(), wavesKey{}, func(n int) { waves = n })
		rv := Review{Findings: []string{"Y"}, Claims: []Claim{testClaim(0, "TestY", "x", RefHead, OutcomePass)}}
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.dropContradicted(ctx, gateTask, Repo{Owner: "sf", Name: "repo"}, 9, "c1", c.call, c.told, rv, nil)
		}()
		scriptReproPods(kube, reproLine(0, "ok"))
		<-done
		if waves != want {
			t.Errorf("%+v: bound waves = %d, want %d", c, waves, want)
		}
	}
}

func TestDropContradictedOverlaysTheCleanedPackage(t *testing.T) {
	kube := newFakeKube()
	r, _, _ := newTestRunner(t, gateTask, kube, &fakeObjectStore{})
	rv := Review{Findings: []string{"Y passes at base"}, Claims: []Claim{testClaim(0, "TestY", "./internal/x", RefBase, OutcomePass)}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.dropContradicted(context.Background(), gateTask, Repo{Owner: "sf", Name: "repo"}, 9, "c1", time.Minute, 0, rv, nil)
	}()
	scriptReproPods(kube, reproLine(0, "ok"))
	<-done
	if spec, _ := json.Marshal(kube.podAt(0).spec); !strings.Contains(string(spec), `{"name":"OVERLAY_PKG","value":"internal/x"}`) ||
		!strings.Contains(string(spec), `\"./internal/x\"]`) {
		t.Fatalf("pod spec = %s, want ./internal/x overlaid as internal/x and run as ./internal/x", spec)
	}
}

func TestWavesSinceCountsInTheRealCallUnit(t *testing.T) {
	began := time.Now().Add(-10 * time.Minute)
	for call, want := range map[time.Duration]int{45 * time.Second: 14, 6 * time.Minute: 2} {
		if got := wavesSince(began, call); got != want {
			t.Errorf("waves of %v = %d, want %d", call, got, want)
		}
	}
}

// modelReviewWaves runs a model review under a pinned supervisor timeout of
// 1 s with the diff answered 3.1 s late and returns the waves it told its
// watch, in order.
func modelReviewWaves(t *testing.T, answer string) []int {
	t.Helper()
	fj := &gateForgejo{diff: "diff --git a/b.go b/b.go\n--- /dev/null\n+++ b/b.go\n@@ -0,0 +1 @@\n+package a\n", diffDelay: 3100 * time.Millisecond,
		mintError: `{"message":"no"}`}
	r, _, _ := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: answer}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	pinned := testPolicyVersion()
	pinned.Document.Limits.SupervisorTimeout = time.Second
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var mu sync.Mutex
	var told []int
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{&TerminalForgejoErrors{},
		&GateWatch{Store: r.store, Bound: func(_ context.Context, _ string, _ int64, waves int) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, waves)
			return time.Hour
		}}}})
	r.RegisterActivities(env)
	if _, err := env.ExecuteActivity(ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{Pin: 7, Policy: PolicyGate{Class: "normal"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(told)
}

// The diff is 3.1 s late: the activity has run ceil(3.1 s / 1 s) = 4 waves
// when the settle tells the watch, so it tells 4 + 3000 s / 1 s = 3004
// waves, not ReviewParts + 3000.
func TestModelReviewTellsTheWatchItsElapsedWaves(t *testing.T) {
	told := modelReviewWaves(t, `{"verdict":"concerns","summary":"s","findings":["Y"],"claims":[{"finding":0,"kind":"test","symbol":"TestY","package":"x","ref":"head","outcome":"pass"}]}`)
	if last := told[len(told)-1]; last != 3004 {
		t.Fatalf("waves told = %v, want the last 3004", told)
	}
}

// The retry of an answer that does not parse tells the elapsed waves and
// one more: 4 + 1 = 5.
func TestModelReviewRetryTellsItsElapsedWavesAndOne(t *testing.T) {
	if told := modelReviewWaves(t, "LGTM"); !slices.Contains(told, 5) {
		t.Fatalf("waves told = %v, want a retry tell of 5", told)
	}
}

// The retry of an answer that did not parse gets a wave of its own: 10 min
// in at 6 min a wave is ceil(1.67) = 2 waves, so it tells 3.
func TestReviewRetryTellsAWaveBeyondTheElapsedOnes(t *testing.T) {
	r, _, _ := newGateRunner(t, gateTask, &gateForgejo{})
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"ok"}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	var told []int
	ctx := context.WithValue(context.Background(), wavesKey{}, func(n int) { told = append(told, n) })
	call := reviewCall{Prompt: "p", Input: func() any { return map[string]string{} }}
	r.reviewRetry(ctx, "t1", "big", 6*time.Minute, time.Now().Add(-10*time.Minute), call, "why")
	if len(told) != 1 || told[0] != 3 {
		t.Fatalf("told = %v, want 3 (two elapsed waves, and one for the retry)", told)
	}
}

// A run that shows the outcome a claim asserts leaves the finding standing,
// with the run's own words on the review for the check-in to judge on.
func TestApplyClaimsRecordsUpheldRuns(t *testing.T) {
	base := testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)
	got := applyClaims(Review{Findings: []string{"Y passes at base"}, Claims: []Claim{base}}, nil, nil,
		func(Claim) (claimRun, error) { return claimRun{Commit: "abc", Exit: 0, Output: "ok"}, nil })
	want := []string{"Y passes at base: go test -run TestY internal/x at base (abc): exit 0, the test did pass as claimed:\nok"}
	if len(got.Findings) != 1 || got.Dropped != nil || got.Unsettled != nil || !slices.Equal(got.Upheld, want) {
		t.Fatalf("review = %+v, want the finding kept and upheld as %q", got, want)
	}
}
