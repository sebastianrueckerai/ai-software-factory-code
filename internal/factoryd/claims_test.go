package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		Dropped: []Dropped{{Finding: "y"}}, GateCriteria: &GateLeft{IDs: []string{"c1"}, Integration: true}}}, [][]fileDiff{nil})
	if last.Claims != nil || last.Dropped != nil || last.GateCriteria != nil {
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
		{testClaim(0, "TestY", "internal/x", RefBase, OutcomePass), []string{"go", "test", "-count=1", "-run", "^TestY$", "./internal/x"}},
		{testClaim(0, "TestY/a.b-c", "./internal/x", RefHead, OutcomePass), []string{"go", "test", "-count=1", "-run", `^TestY/a\.b-c$`, "./internal/x"}},
		{testClaim(0, "TestY", ".", RefHead, OutcomePass), []string{"go", "test", "-count=1", "-run", "^TestY$", "./"}},
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
		"cannot start":   {err: errors.New("no sandbox"), want: "Y passes: TestY in internal/x at base could not be run: no sandbox"},
		"timed out":      {run: claimRun{Exit: -1, Output: "(timed out after 10m0s)"}, want: "Y passes: TestY in internal/x at base is unsettled: the run did not finish (exit -1)"},
		"build failed":   {run: claimRun{Exit: 1, Output: "FAIL\tx [build failed]"}, want: "Y passes: TestY in internal/x at base is unsettled: the package did not build"},
		"setup failed":   {run: claimRun{Exit: 1, Output: "FAIL\tx [setup failed]"}, want: "Y passes: TestY in internal/x at base is unsettled: the package did not build"},
		"test not there": {run: claimRun{Exit: 0, Output: "ok  \tx\t0.01s [no tests to run]"}, want: "Y passes: TestY in internal/x at base is unsettled: no such test at that ref"},
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
		got = r.dropContradicted(context.Background(), gateTask, Repo{Owner: "sf", Name: "repo"}, 9, "c1", time.Minute, rv, nil)
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
	for _, want := range []string{`{"name":"REPRO_ARGV","value":"[\"go\",\"test\",\"-count=1\",\"-run\",\"^TestY/sub$\",\"./internal/x\"]"}`,
		`{"name":"CHECKOUT_SHA","value":"base"}`, `{"name":"MODE","value":"repro"}`} {
		if !strings.Contains(string(spec), want) {
			t.Fatalf("base pod spec lacks %s: %s", want, spec)
		}
	}
	if strings.Contains(string(spec), "REPRO_COMMAND") {
		t.Fatalf("pod spec carries a shell command: %s", spec)
	}
	if head, _ := json.Marshal(kube.podAt(1).spec); strings.Contains(string(head), "CHECKOUT_SHA") || !strings.Contains(string(head), `{"name":"REPO_SHA","value":"c1"}`) {
		t.Fatalf("head pod spec = %s, want the head commit and no base checkout", head)
	}
}

func TestDropContradictedKeepsTheFindingWhenTheSandboxCannotStart(t *testing.T) {
	kube := newFakeKube()
	r, _, _ := newTestRunner(t, gateTask, kube, &fakeObjectStore{})
	rv := Review{Verdict: "concerns", Findings: []string{"Y passes at base"}, Claims: []Claim{testClaim(0, "TestY", "internal/x", RefBase, OutcomePass)}}
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: "start failed: egress probe: gateway answered 503", Step: "egress probe"}})
	var got Review
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = r.dropContradicted(context.Background(), gateTask, Repo{Owner: "sf", Name: "repo"}, 9, "c1", time.Minute, rv, nil)
	}()
	scriptReproPods(kube, string(b))
	<-done
	if len(got.Findings) != 1 || got.Dropped != nil || len(got.Unsettled) != 1 || !strings.Contains(got.Unsettled[0], "could not be run") {
		t.Fatalf("review = %+v, want the finding kept and unsettled", got)
	}
}
