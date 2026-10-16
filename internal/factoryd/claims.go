package factoryd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A Claim is a review finding's machine-readable assertion that a name is
// absent: Finding indexes the review's findings, Symbol is a bare
// identifier.
type Claim struct {
	Finding int    `json:"finding"`
	Kind    string `json:"kind"`
	Symbol  string `json:"symbol"`
}

const (
	ClaimUndefined = "undefined"
	ClaimUncalled  = "uncalled"
)

var symbolName = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]{0,127}$`)

func validClaim(c Claim) bool {
	return (c.Kind == ClaimUndefined || c.Kind == ClaimUncalled) && symbolName.MatchString(c.Symbol)
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

func comment(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*")
}

// removesSymbol: the diff removes a line that names symbol.
func removesSymbol(diff, symbol string) bool {
	return regexp.MustCompile(`(?m)^-[^-\n].*\b` + regexp.QuoteMeta(symbol) + `\b`).MatchString(diff)
}

// contradiction is the first hit that refutes claim: a declaration of the
// symbol (undefined) or a use that is neither declaration nor comment
// (uncalled), at the head, or at main unless the diff removes the symbol
// (removed): the head then lacks it on purpose.
func contradiction(c Claim, hits []grepHit, removed bool) (evidence string, ok bool) {
	decl := declaration(c.Symbol)
	for _, h := range hits {
		if h.Ref == "main" && removed {
			continue
		}
		isDecl := decl.MatchString(h.Text)
		if c.Kind == ClaimUndefined && isDecl || c.Kind == ClaimUncalled && !isDecl && !comment(h.Text) {
			return fmt.Sprintf("%s:%d at %s: %s", h.Path, h.Line, h.Ref, h.Text), true
		}
	}
	return "", false
}

// applyClaims moves every finding that all its claims contradict from
// rv.Findings to rv.Dropped, grep naming the symbol's hits. A failed grep
// contradicts nothing. The claims are spent either way.
func applyClaims(rv Review, diff string, grep func(symbol string) ([]grepHit, error)) Review {
	total, refuted := map[int]int{}, map[int]Dropped{}
	counted := map[int]int{}
	for _, c := range rv.Claims {
		total[c.Finding]++
		hits, err := grep(c.Symbol)
		if err != nil {
			log.Printf("review claim %s: %v", c.Symbol, err)
			continue
		}
		if ev, ok := contradiction(c, hits, removesSymbol(diff, c.Symbol)); ok {
			counted[c.Finding]++
			refuted[c.Finding] = Dropped{Finding: rv.Findings[c.Finding], Kind: c.Kind, Symbol: c.Symbol, Evidence: ev}
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
	return rv
}

// gitGrep lists the whole-word uses of symbol in each ref's tree, at most
// maxHits a ref; a ref without a match has none.
func gitGrep(ctx context.Context, dir string, refs []namedRef, symbol string) ([]grepHit, error) {
	const maxHits = 200
	var hits []grepHit
	for _, ref := range refs {
		out, err := git(ctx, dir, "", "grep", "-n", "-w", "-F", "-I", "--no-color", "-e", symbol, ref.Rev, "--")
		if err != nil {
			if out == "" && strings.Contains(err.Error(), "exit status 1") {
				continue
			}
			return nil, err
		}
		for i, l := range strings.Split(out, "\n") {
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
	return hits, nil
}

// checkoutRefs fetches main and the pull request's head, both at depth
// 1 without a working tree, into a temporary directory and names the two
// refs to search; a test substitutes it, git being no fake's. The head
// must still be commit.
var checkoutRefs = func(ctx context.Context, f *Forgejo, repo Repo, pr int64, commit string) (dir string, refs []namedRef, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "claims-")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	login, src := basicLogin(f.BotUser, f.BotPassword), filepath.Join(tmp, "src")
	if _, err = git(ctx, tmp, login, "clone", "-q", "--depth=1", "--no-checkout", "--branch", prBase, f.BaseURL+"/"+repo.String()+".git", src); err == nil {
		_, err = git(ctx, src, login, "fetch", "-q", "--depth=1", "origin", fmt.Sprintf("refs/pull/%d/head:refs/factory/head", pr))
	}
	var head string
	if err == nil {
		head, err = git(ctx, src, "", "rev-parse", "refs/factory/head")
	}
	if err == nil && head != commit {
		err = fmt.Errorf("pull request %d head is %s, not %s", pr, head, commit)
	}
	if err != nil {
		cleanup()
		return "", nil, nil, err
	}
	return src, []namedRef{{"head", "refs/factory/head"}, {"main", prBase}}, cleanup, nil
}

// dropContradicted checks rv's claims against the repository at the head
// and at main and drops the findings it contradicts. No claim, no
// checkout: a checkout that fails drops nothing.
func (r *Runner) dropContradicted(ctx context.Context, repo Repo, pr int64, commit string, rv Review, diff string) Review {
	if len(rv.Claims) == 0 {
		return rv
	}
	dir, refs, cleanup, err := checkoutRefs(ctx, r.forgejo, repo, pr, commit)
	if err != nil {
		log.Printf("review claims of pull request %d: %v", pr, err)
		rv.Claims = nil
		return rv
	}
	defer cleanup()
	return applyClaims(rv, diff, func(symbol string) ([]grepHit, error) { return gitGrep(ctx, dir, refs, symbol) })
}
