package factoryd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseReviewClaims(t *testing.T) {
	in := `{"verdict":"concerns","summary":"s","findings":["a.go: TasksByPR has no SQL implementation"," ","b.go: helper is dead"],
"claims":[{"finding":0,"kind":"undefined","symbol":"TasksByPR"},{"finding":1,"kind":"undefined","symbol":"Blank"},
{"finding":2,"kind":"uncalled","symbol":"helper"},{"finding":9,"kind":"undefined","symbol":"Gone"},
{"finding":0,"kind":"undefined","symbol":"x; rm -rf"},{"finding":0,"kind":"bogus","symbol":"Y"}]}`
	rv := parseReview(in)
	want := []Claim{{0, ClaimUndefined, "TasksByPR"}, {1, ClaimUncalled, "helper"}}
	if !slices.Equal(rv.Claims, want) || len(rv.Findings) != 2 {
		t.Fatalf("claims = %+v over %d findings, want %+v over 2", rv.Claims, len(rv.Findings), want)
	}
	if got := parseReview(`{"verdict":"approve","summary":"s","dropped":[{"finding":"x"}]}`); got.Verdict != "" || !strings.Contains(got.Note, "dropped is not an answer field") {
		t.Fatalf("a smuggled drop = %+v", got)
	}
}

func TestCombineReviewsOffsetsClaims(t *testing.T) {
	got := combineReviews([]Review{
		{Verdict: "concerns", Summary: "a", Findings: []string{"f0"}, Claims: []Claim{{0, ClaimUndefined, "A"}}},
		{Verdict: "concerns", Summary: "b", Findings: []string{"f1", "f2"}, Claims: []Claim{{1, ClaimUncalled, "C"}}},
	})
	if want := []Claim{{0, ClaimUndefined, "A"}, {2, ClaimUncalled, "C"}}; !slices.Equal(got.Claims, want) {
		t.Fatalf("claims = %+v, want %+v", got.Claims, want)
	}
}

func TestLastReviewCarriesNoClaimsOrDrops(t *testing.T) {
	last, _ := lastReview([]Review{{Verdict: "concerns", Summary: "s", Findings: []string{"a.go: x"}, Claims: []Claim{{0, ClaimUndefined, "X"}},
		Dropped: []Dropped{{Finding: "y"}}}}, [][]fileDiff{nil})
	if last.Claims != nil || last.Dropped != nil {
		t.Fatalf("last review = %+v", last)
	}
}

func TestContradiction(t *testing.T) {
	hit := func(ref, text string) grepHit { return grepHit{Ref: ref, Path: "internal/s.go", Line: 12, Text: text} }
	undefined, uncalled := Claim{0, ClaimUndefined, "TasksByPR"}, Claim{0, ClaimUncalled, "docRootOK"}
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
	rv := Review{Verdict: "concerns", Findings: []string{"helper is uncalled"}, Claims: []Claim{{0, ClaimUncalled, "helper"}}}
	got := applyClaims(rv, diff, func(string) ([]grepHit, error) { return mainHit("s.go", 2, "\thelper()"), nil })
	if len(got.Findings) != 0 || len(got.Dropped) != 1 || got.Dropped[0].Evidence != "s.go:2 at main: \thelper()" {
		t.Fatalf("review = %+v, want the finding refuted by main's call", got)
	}
}

func mainHit(path string, line int, text string) []grepHit {
	return []grepHit{{Ref: "main", Path: path, Line: line, Text: text}}
}

func TestApplyClaimsDropsAContradictedFinding(t *testing.T) {
	rv := Review{Verdict: "concerns", Summary: "s", Findings: []string{"keep", "gone"}, Claims: []Claim{{1, ClaimUndefined, "TasksByPR"}}}
	got := applyClaims(rv, "", func(string) ([]grepHit, error) {
		return mainHit("internal/s.go", 7, "func (s *PG) TasksByPR() {"), nil
	})
	if !slices.Equal(got.Findings, []string{"keep"}) || got.Claims != nil || got.Verdict != "concerns" {
		t.Fatalf("review = %+v", got)
	}
	want := Dropped{Finding: "gone", Kind: ClaimUndefined, Symbol: "TasksByPR", Evidence: "internal/s.go:7 at main: func (s *PG) TasksByPR() {"}
	if len(got.Dropped) != 1 || got.Dropped[0] != want {
		t.Fatalf("dropped = %+v, want %+v", got.Dropped, want)
	}
}

func TestApplyClaimsKeepsAFindingWhenTheGrepFails(t *testing.T) {
	rv := Review{Verdict: "concerns", Findings: []string{"f"}, Claims: []Claim{{0, ClaimUndefined, "X"}}}
	got := applyClaims(rv, "", func(string) ([]grepHit, error) { return nil, os.ErrDeadlineExceeded })
	if !slices.Equal(got.Findings, []string{"f"}) || got.Dropped != nil {
		t.Fatalf("review = %+v, want the finding kept", got)
	}
}

func TestApplyClaimsDropsOnlyWhenEveryClaimIsContradicted(t *testing.T) {
	rv := Review{Verdict: "concerns", Findings: []string{"A and B are undefined"}, Claims: []Claim{{0, ClaimUndefined, "A"}, {0, ClaimUndefined, "B"}}}
	got := applyClaims(rv, "", func(sym string) ([]grepHit, error) {
		if sym == "A" {
			return mainHit("a.go", 1, "func A() {"), nil
		}
		return nil, nil
	})
	if len(got.Findings) != 1 || got.Dropped != nil {
		t.Fatalf("review = %+v, want the finding kept: B has no definition", got)
	}
}

func TestContradictionIgnoresMainWhenTheDiffRemovesTheSymbol(t *testing.T) {
	diff := "diff --git a/s.go b/s.go\n--- a/s.go\n+++ b/s.go\n@@ -1,2 +1,1 @@\n-func TasksByPR() {}\n context\n"
	rv := Review{Verdict: "concerns", Findings: []string{"TasksByPR is gone"}, Claims: []Claim{{0, ClaimUndefined, "TasksByPR"}}}
	got := applyClaims(rv, diff, func(string) ([]grepHit, error) { return mainHit("s.go", 1, "func TasksByPR() {}"), nil })
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
