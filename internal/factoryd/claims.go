package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// A Claim is a review finding's machine-readable assertion: that a name is
// absent (Symbol is a bare identifier), or that a Go test has an outcome at
// a ref (Symbol is the test's name, Package its directory, Ref base or head,
// Outcome pass or fail). Finding indexes the review's findings.
type Claim struct {
	Finding int    `json:"finding"`
	Kind    string `json:"kind"`
	Symbol  string `json:"symbol"`
	Package string `json:"package,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

const (
	ClaimUndefined   = "undefined"
	ClaimUncalled    = "uncalled"
	ClaimTestOutcome = "test"

	RefBase, RefHead         = "base", "head"
	OutcomePass, OutcomeFail = "pass", "fail"

	// maxKeptTestClaims is how many test-outcome claims one review's
	// parse keeps: every kept claim runs this round, so the bound keeps
	// a reviewer from spending unbounded pods (the issue's bundled
	// findings alone reach eight claims)
	maxKeptTestClaims = 16
)

var (
	symbolName = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]{0,127}$`)
	// a Go test function with an optional subtest path
	testName = regexp.MustCompile(`^Test[\p{L}\p{N}_]{0,127}(?:/[\p{L}\p{N}_.-]{1,128}){0,4}$`)
	// a package directory inside the repository: relative, no .. segment
	packagePath = regexp.MustCompile(`^(?:\.|(?:\./)?[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}(?:/[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}){0,15})$`)
)

func validClaim(c Claim) bool {
	if c.Kind == ClaimTestOutcome {
		return testName.MatchString(c.Symbol) && packagePath.MatchString(c.Package) &&
			(c.Ref == RefBase || c.Ref == RefHead) && (c.Outcome == OutcomePass || c.Outcome == OutcomeFail) && !strings.Contains(c.Package, "...")
	}
	return (c.Kind == ClaimUndefined || c.Kind == ClaimUncalled) && symbolName.MatchString(c.Symbol) &&
		c.Package == "" && c.Ref == "" && c.Outcome == ""
}

// PendingClaim is a test-outcome claim no run settled, carried on the
// review to the next round: the claim itself and its finding's text,
// the one a re-filed finding is matched by.
type PendingClaim struct {
	Claim   Claim  `json:"claim"`
	Finding string `json:"finding"`
}

// claimKey is c's substance without its finding index: an index the
// next review's answer names is that answer's own, the substance the
// claim's, so a run settles the re-filed claim's key.
func claimKey(c Claim) Claim {
	c.Finding = 0
	return c
}

// claimOutcome is what one run showed of one claim's substance: the run
// itself when it answered, or why it could not — cached, so an
// identical re-filed claim does not run twice.
type claimOutcome struct {
	run claimRun
	err error
}

// testArgv is the fixed command that settles a valid test-outcome claim:
// no shell reads it. Each subtest element is anchored, for go test matches
// the elements of -run apart; go's own timeout is off, so the run's bound
// alone ends an overlong run, and a killed run is unsettled.
func testArgv(c Claim) []string {
	pkg := "./" + strings.TrimPrefix(strings.TrimPrefix(c.Package, "."), "/")
	parts := strings.Split(c.Symbol, "/")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	pattern := "^" + strings.Join(parts, "$/^") + "$"
	return []string{"go", "test", "-count=1", "-timeout", "0", "-run", pattern, pkg}
}

// claimRun is one settling run: the commit it ran at, its exit and output.
type claimRun struct {
	Commit string
	Exit   int
	Output string
}

// outcome is what the run shows of the test: pass, fail, or "" and why when
// it shows neither (it could not start or time out, did not build, or the
// test is not there).
func (t claimRun) outcome() (outcome, why string) {
	switch {
	case t.Exit < 0:
		return "", fmt.Sprintf("the run did not finish (exit %d)", t.Exit)
	case strings.Contains(t.Output, "[build failed]") || strings.Contains(t.Output, "[setup failed]"):
		return "", "the package did not build"
	case t.Exit == 0 && (strings.Contains(t.Output, "no tests to run") || strings.Contains(t.Output, "[no test files]")):
		return "", "no such test at that ref"
	case t.Exit == 0:
		return OutcomePass, ""
	}
	return OutcomeFail, ""
}

// Dropped is a review finding the repository contradicts: dropped by code,
// never given to a session. Evidence is "file:line at head|main: line".
type Dropped struct {
	Finding  string `json:"finding"`
	Kind     string `json:"kind"`
	Symbol   string `json:"symbol"`
	Evidence string `json:"evidence"`
}

type grepHit struct {
	Ref, Path string
	Line      int
	Text      string
}

type namedRef struct{ Name, Rev string }

var declaration = func(symbol string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*(?:export\s+|pub\s+|async\s+|static\s+)*(?:func(?:\s+|\s*\([^)]*\)\s*)|type\s+|var\s+|const\s+|def\s+|class\s+|function\s+|fn\s+|interface\s+|enum\s+|struct\s+|trait\s+|let\s+)` + regexp.QuoteMeta(symbol) + `\b`)
}

var blockSpan = regexp.MustCompile(`/\*.*?\*/`)

func comment(text string) bool {
	t := strings.TrimSpace(blockSpan.ReplaceAllString(text, ""))
	return t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*")
}

// removesSymbol: the diff removes a declaration of symbol.
func removesSymbol(diff, symbol string) bool {
	decl := declaration(symbol)
	for _, l := range strings.Split(diff, "\n") {
		if len(l) > 1 && l[0] == '-' && l[1] != '-' && decl.MatchString(l[1:]) {
			return true
		}
	}
	return false
}

// contradiction is the first hit that refutes claim: a declaration of the
// symbol (undefined) or a use that is neither declaration nor comment
// (uncalled), at the head, or at main unless the diff removes the symbol's
// declaration (removed): the head then lacks it on purpose.
func contradiction(c Claim, hits []grepHit, removed bool) (evidence string, ok bool) {
	decl, word := declaration(c.Symbol), regexp.MustCompile(`\b`+regexp.QuoteMeta(c.Symbol)+`\b`)
	for _, h := range hits {
		if h.Ref == "main" && removed {
			continue
		}
		code := blockSpan.ReplaceAllString(h.Text, "")
		isDecl := decl.MatchString(code)
		if c.Kind == ClaimUndefined && isDecl || c.Kind == ClaimUncalled && !isDecl && !comment(code) && word.MatchString(code) {
			return fmt.Sprintf("%s:%d at %s: %s", h.Path, h.Line, h.Ref, h.Text), true
		}
	}
	return "", false
}

// applyClaims moves every finding that all its claims contradict from
// rv.Findings to rv.Dropped: grep names a symbol's hits, settle runs a
// test-outcome claim (nil: none runs, and no claim settles), every kept
// claim in the same review. A finding several claims refute keeps one
// drop whose evidence names each refutation in claim order. A failed
// grep or run contradicts nothing; a run that settles nothing carries
// its claim to the next round on rv.Pending. A concerns review whose
// every finding was dropped turns approve (turnVerdict). The claims are
// spent either way.
func applyClaims(rv Review, files []fileDiff, grep func(symbol string) ([]grepHit, error), settle func(Claim) (claimRun, error)) Review {
	total, refuted := map[int]int{}, map[int]Dropped{}
	counted := map[int]int{}
	// note is one claim's refutation of finding: a finding bundling
	// several claims keeps one drop whose evidence names each run (or
	// grep hit) that refuted it, in claim order
	note := func(finding int, d Dropped) {
		if prior, ok := refuted[finding]; ok {
			prior.Evidence += "\n" + d.Evidence
			prior.Symbol += ", " + d.Symbol
			d = prior
		}
		refuted[finding] = d
	}
	for _, c := range rv.Claims {
		total[c.Finding]++
		if c.Kind == ClaimTestOutcome {
			if ev, ok, upheld, why := settleTest(c, settle); ok {
				counted[c.Finding]++
				note(c.Finding, Dropped{Finding: rv.Findings[c.Finding], Kind: c.Kind, Symbol: c.Symbol, Evidence: ev})
			} else if upheld != "" {
				rv.Upheld = append(rv.Upheld, fmt.Sprintf("%s: %s", rv.Findings[c.Finding], upheld))
			} else {
				rv.Unsettled = append(rv.Unsettled, fmt.Sprintf("%s: %s", rv.Findings[c.Finding], why))
				rv.Pending = append(rv.Pending, PendingClaim{Claim: c, Finding: rv.Findings[c.Finding]})
			}
			continue
		}
		hits, err := grep(c.Symbol)
		if err != nil {
			log.Printf("review claim %s: %v", c.Symbol, err)
			continue
		}
		removed := slices.ContainsFunc(files, func(f fileDiff) bool { return removesSymbol(f.Text, c.Symbol) })
		if ev, ok := contradiction(c, hits, removed); ok {
			counted[c.Finding]++
			note(c.Finding, Dropped{Finding: rv.Findings[c.Finding], Kind: c.Kind, Symbol: c.Symbol, Evidence: ev})
		}
	}
	var kept []string
	for i, f := range rv.Findings {
		if total[i] > 0 && counted[i] == total[i] {
			rv.Dropped = append(rv.Dropped, refuted[i])
			continue
		}
		kept = append(kept, f)
	}
	rv.Findings, rv.Claims = kept, nil
	turnVerdict(&rv)
	return rv
}

// turnVerdict turns a concerns review whose every finding was dropped —
// by a run, a grep or a carried claim — to approve: the loop's own
// evidence settled everything the model held against the head. The
// record keeps what the model said: TurnedFrom names its verdict beside
// the turned one, and the drops and the summary stay. A review with a
// finding standing, or none it ever held, is not turned.
func turnVerdict(rv *Review) {
	if rv.Verdict != "concerns" || len(rv.Findings) > 0 || len(rv.Dropped) == 0 || rv.TurnedFrom != "" {
		return
	}
	rv.Verdict, rv.TurnedFrom = "approve", rv.Verdict
}

// settleTest runs c through settle and compares its outcome with the one
// c asserts: ok with the evidence when the run shows the other, else why
// the claim stands unsettled ("" when the run settled it for the finding),
// or upheld, the run's own words, when the run showed the outcome the claim
// asserts.
func settleTest(c Claim, settle func(Claim) (claimRun, error)) (evidence string, ok bool, upheld, why string) {
	name := fmt.Sprintf("%s in %s at %s", c.Symbol, c.Package, c.Ref)
	if settle == nil {
		return "", false, "", fmt.Sprintf("%s could not be run: no sandbox", name)
	}
	run, err := settle(c)
	if err != nil {
		return "", false, "", fmt.Sprintf("%s could not be run: %v", name, err)
	}
	got, why := run.outcome()
	if got == "" {
		return "", false, "", fmt.Sprintf("%s is unsettled: %s", name, why)
	}
	if got == c.Outcome {
		return "", false, fmt.Sprintf("go test -run %s %s at %s (%s): exit %d, the test did %s as claimed:\n%s", c.Symbol, c.Package, c.Ref, run.Commit, run.Exit, c.Outcome,
			textutil.Tail(run.Output, claimEvidenceTail)), ""
	}
	return fmt.Sprintf("go test -run %s %s at %s (%s): exit %d, the test did not %s:\n%s", c.Symbol, c.Package, c.Ref, run.Commit, run.Exit, c.Outcome,
		textutil.Tail(run.Output, claimEvidenceTail)), true, "", ""
}

// claimEvidenceTail is the last bytes of a run's output the evidence keeps.
const claimEvidenceTail = 600

// errNoMatch is git grep's exit status 1: no line names the symbol.
var errNoMatch = errors.New("git grep: no matches")

// grepLimits caps git grep on an agent-written tree: its output past the cap
// fails the call, never a cut-off list.
var grepLimits = gitLimits{maxOut: 16 << 20, addressSpace: wipGitMemory}

// gitGrep lists the whole-word uses of symbol in each ref's tree, at most
// maxHits a ref; a ref without a match has none, and no match in any ref
// answers errNoMatch.
func gitGrep(ctx context.Context, dir string, refs []namedRef, symbol string) ([]grepHit, error) {
	const maxHits = 200
	var hits []grepHit
	for _, ref := range refs {
		out, err := gitRun(ctx, dir, "", nil, grepLimits, "grep", "-n", "-w", "-F", "-I", "--no-color", "-e", symbol, ref.Rev, "--")
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				continue
			}
			return nil, err
		}
		for i, l := range strings.SplitN(out, "\n", maxHits+1) {
			if i == maxHits {
				break
			}
			path, rest, _ := strings.Cut(strings.TrimPrefix(l, ref.Rev+":"), ":")
			num, text, _ := strings.Cut(rest, ":")
			n, err := strconv.Atoi(num)
			if err != nil {
				continue
			}
			hits = append(hits, grepHit{ref.Name, path, n, text})
		}
	}
	if len(hits) == 0 {
		return nil, errNoMatch
	}
	return hits, nil
}

// CheckoutRefs fetches main and the pull request's head, both at depth
// 1 without a working tree, into a temporary directory and names the two
// refs to search; with no pull request (pr 0) it fetches main alone. A
// test substitutes it, git being no fake's. The head must still be commit.
var CheckoutRefs = func(ctx context.Context, f *Forgejo, repo Repo, pr int64, commit string) (dir string, refs []namedRef, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "claims-")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) } // #nosec G104 -- best-effort cleanup of a temporary directory
	login, src := basicLogin(f.BotUser, f.BotPassword), filepath.Join(tmp, "src")
	_, err = gitCapped(ctx, tmp, login, "clone", "-q", "--depth=1", "--no-checkout", "--branch", prBase, f.BaseURL+"/"+repo.String()+".git", src)
	refs = []namedRef{{"main", prBase}}
	if err == nil && pr > 0 {
		_, err = gitCapped(ctx, src, login, "fetch", "-q", "--depth=1", "origin", fmt.Sprintf("refs/pull/%d/head:refs/factory/head", pr))
		var head string
		if err == nil {
			head, err = gitCapped(ctx, src, "", "rev-parse", "refs/factory/head")
		}
		if err == nil && head != commit {
			err = fmt.Errorf("pull request %d head is %s, not %s", pr, head, commit)
		}
		refs = []namedRef{{"head", "refs/factory/head"}, {"main", prBase}}
	}
	if err != nil {
		cleanup()
		return "", nil, nil, err
	}
	return src, refs, cleanup, nil
}

// dropContradicted checks rv's claims and drops the findings they contradict:
// a name's claims against the repository at the head and at main (no such
// claim, no checkout: a checkout that fails drops nothing), a test's against
// a run in the task's sandbox at its ref. cache answers a claim whose
// substance an earlier run this round settled (a carried claim's) without a
// second run.
func (r *Runner) dropContradicted(ctx context.Context, t Task, repo Repo, pr int64, commit string, call time.Duration, told int, rv Review, files []fileDiff, cache map[Claim]claimOutcome) Review {
	if len(rv.Claims) == 0 {
		return rv
	}
	grep := func(string) ([]grepHit, error) { return nil, errors.New("no checkout") }
	if slices.ContainsFunc(rv.Claims, func(c Claim) bool { return c.Kind != ClaimTestOutcome }) {
		if dir, refs, cleanup, err := CheckoutRefs(ctx, r.forgejo, repo, pr, commit); err != nil {
			log.Printf("review claims of pull request %d: %v", pr, err)
		} else {
			defer cleanup()
			grep = func(symbol string) ([]grepHit, error) {
				hits, err := gitGrep(ctx, dir, refs, symbol)
				if errors.Is(err, errNoMatch) {
					return nil, nil
				}
				return hits, err
			}
		}
	}
	var settle func(Claim) (claimRun, error)
	testClaims := 0
	for _, c := range rv.Claims {
		if c.Kind == ClaimTestOutcome {
			testClaims++
		}
	}
	if testClaims > 0 {
		// one pod a kept claim runs, each a start and the command, after
		// one sandbox setup. The gate watch's bound counts a review's
		// waves of model calls: it is told the waves told so far (at
		// least its first bound's) and the waves the runs add.
		budget := time.Duration(testClaims)*(podStartWait+r.cfg.ReproTimeout) + podStartWait
		call = max(call, time.Second)
		boundByParts(ctx, max(policy.ReviewParts, told)+int((budget+call-1)/call))
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
		var done func()
		settle, done = r.testSettler(ctx, t, repo, pr, commit)
		defer done()
	}
	if cache != nil && settle != nil {
		base := settle
		settle = func(c Claim) (claimRun, error) {
			if o, ok := cache[claimKey(c)]; ok {
				return o.run, o.err
			}
			run, err := base(c)
			cache[claimKey(c)] = claimOutcome{run, err}
			return run, err
		}
	}
	return applyClaims(rv, files, grep, settle)
}

// carriedRun is one carried claim's run on the head the next review
// reads: the pending claim and what the run showed — the evidence that
// refutes it, the run's own words upholding it, or why no run settled
// it (refuted names the first).
type carriedRun struct {
	pending PendingClaim
	shown   string
	refuted bool
}

// settleCarried runs the claims the last verdict-bearing review of
// earlier carried — the ones no run settled — on the head this review
// reads, each once in the task's sandbox, before the review's model
// call. The cache it answers an identical re-filed claim without a
// second run (dropContradicted reads it).
func (r *Runner) settleCarried(ctx context.Context, t Task, repo Repo, pr int64, commit string, call time.Duration, told int, earlier []Review) ([]carriedRun, map[Claim]claimOutcome) {
	found := lastVerdict(earlier)
	if found == nil || len(found[0].Pending) == 0 {
		return nil, nil
	}
	pending := found[0].Pending
	// one pod a carried claim runs, each a start and the command, after
	// one sandbox setup — the settle's own bound (dropContradicted's)
	budget := time.Duration(len(pending))*(podStartWait+r.cfg.ReproTimeout) + podStartWait
	call = max(call, time.Second)
	boundByParts(ctx, max(policy.ReviewParts, told)+int((budget+call-1)/call))
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, budget)
	defer cancel()
	base, done := r.testSettler(ctx, t, repo, pr, commit)
	defer done()
	cache := map[Claim]claimOutcome{}
	settle := func(c Claim) (claimRun, error) {
		if o, ok := cache[claimKey(c)]; ok {
			return o.run, o.err
		}
		run, err := base(c)
		cache[claimKey(c)] = claimOutcome{run, err}
		return run, err
	}
	var carried []carriedRun
	for _, p := range pending {
		evidence, ok, upheld, why := settleTest(p.Claim, settle)
		switch {
		case ok:
			carried = append(carried, carriedRun{pending: p, shown: evidence, refuted: true})
		case upheld != "":
			carried = append(carried, carriedRun{pending: p, shown: upheld})
		default:
			carried = append(carried, carriedRun{pending: p, shown: why})
		}
	}
	return carried, cache
}

// dropCarried drops a re-filed finding whose carried claim a run on this
// head refuted, matched by the finding's text, with the run's evidence:
// the one the model re-filed without its claim — a re-filed claim is
// dropped by its own run (dropContradicted, the cache answering it).
func dropCarried(rv Review, carried []carriedRun) Review {
	for _, c := range carried {
		if !c.refuted {
			continue
		}
		if i := slices.Index(rv.Findings, c.pending.Finding); i >= 0 {
			rv.Findings = slices.Delete(rv.Findings, i, i+1)
			rv.Dropped = append(rv.Dropped, Dropped{Finding: c.pending.Finding,
				Kind: c.pending.Claim.Kind, Symbol: c.pending.Claim.Symbol, Evidence: c.shown})
		}
	}
	return rv
}

// testSettler runs a test-outcome claim's fixed argv in the task's
// reproduction sandbox. At the base it runs the head's test files of the
// claimed package over the base's tree: the question is whether the head's
// test fails on the base's code. The sandbox is built on the first run and torn down by done; a
// sandbox that cannot start fails every run. Its output is scrubbed.
func (r *Runner) testSettler(ctx context.Context, t Task, repo Repo, pr int64, commit string) (settle func(Claim) (claimRun, error), done func()) {
	var (
		sandboxDone func()
		sandboxErr  error
		started     bool
	)
	done = func() {
		if sandboxDone != nil {
			sandboxDone()
		}
	}
	return func(c Claim) (claimRun, error) {
		if !started {
			started = true
			sandboxDone, sandboxErr = r.reproSandbox(ctx, &t, repo)
		}
		if sandboxErr != nil {
			return claimRun{}, errors.New(r.Scrub.String(sandboxErr.Error()))
		}
		at, ran, overlay := "", commit, ""
		if c.Ref == RefBase {
			overlay = path.Clean(c.Package)
			if at = cmp.Or(t.ReproBase, t.SHA); at == "" {
				return claimRun{}, errors.New("the task has no base commit")
			}
			ran = at
		}
		rep, err := r.runArgv(ctx, t, repo, pullRef(pr), commit, at, testArgv(c), overlay)
		if err != nil {
			return claimRun{}, errors.New(r.Scrub.String(err.Error()))
		}
		return claimRun{Commit: ran, Exit: rep.Exit, Output: r.Scrub.String(rep.Output)}, nil
	}, done
}
