package factoryd

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/doclinks"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// BriefText is a card's brief: its title, the opening paragraph of its
// objective, its why line, its summary and the rest of its text.
type BriefText struct{ Title, Why, Summary, Body string }

// BriefJob is what a card does, which sets the rules its brief passes.
type BriefJob int

const (
	// BriefWork is a card that carries work.
	BriefWork BriefJob = iota
	// BriefAsk is a card that asks a human: its title is the ask.
	BriefAsk
	// BriefHumanIssue is a human's own issue: its title stays as the
	// human wrote it, so only its length and its references are checked.
	BriefHumanIssue
)

// BriefJobOf is the job of t's brief: a human's issue or API call keeps
// its title (the project agent's issue is no human's), a human's card
// asks, and every other card carries work.
func BriefJobOf(t Task) BriefJob {
	switch {
	case (t.Source == SourceWebhook || t.Source == SourceAPI) && t.Author != ChatUser:
		return BriefHumanIssue
	case t.Kind == "human":
		return BriefAsk
	}
	return BriefWork
}

// RefResolver resolves a brief's references in its project: Issue is
// card or pull request n with its title and state, Heading whether the
// document at path holds the heading id, or exists when id is "".
type RefResolver interface {
	Issue(n int) (title, state string, ok bool)
	Heading(path, id string) bool
}

// BriefTries is the try on which a brief that still fails files anyway,
// marked unchecked: a writer that cannot meet the rules does not loop.
const BriefTries = 3

// FileBrief is the filing door's verdict on try tries of b: ok when it
// passes, or on the BriefTries-th try with its refusals, when the caller
// files it and sets Task.BriefUnchecked.
func FileBrief(b BriefText, job BriefJob, refs RefResolver, tries int) (ok bool, refusals []string) {
	refusals = CheckBrief(b, job, refs)
	return len(refusals) == 0 || tries >= BriefTries, refusals
}

const (
	titleMax   = 70
	askMax     = 110
	whyMax     = 160
	summaryMax = 600
)

var (
	issueRef     = regexp.MustCompile(`(?:^|[^\w&#/])(#(\d+))\b`)
	hexID        = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	backticked   = regexp.MustCompile("`[^`]*`?")
	camelCase    = regexp.MustCompile(`\b[a-z]{2,}[A-Z][A-Za-z0-9]*\b`)
	snakeCase    = regexp.MustCompile(`\b[A-Za-z0-9]+(?:_[A-Za-z0-9]+)+\b`)
	branchName   = regexp.MustCompile(`\b(?:(?:card|pull)/\d+|factory/(?:t-[0-9a-z]+|schema-[\w.-]+|template-update)|salvage/t-[0-9a-z]+|factory-pins/[\w.-]+)\b`)
	sectionLabel = regexp.MustCompile(`\b(?:(?i:fault|requirement|step|task)\s+\d+[a-z]?(?:\.\d+)*|(?i:phase)\s+(?:\d+[a-z]?(?:\.\d+)*|[A-HJ-Z]))\b`)
	mdLink       = regexp.MustCompile(`\[[^\]]*\]\(\s*([^\s)]*)[^)]*\)`)
	linkTarget   = regexp.MustCompile(`\]\([^)]*\)`)
	linkScheme   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	sentenceEnd  = regexp.MustCompile(`([.!?]+\s+)\p{Lu}`)
	osName       = regexp.MustCompile(`^[a-z]+OS$`)
	abbreviation = regexp.MustCompile(`(?i)\b(?:e\.g|i\.e)$`)
	fencedBlock  = regexp.MustCompile("(?s)```.*?(?:```|$)")
)

// CheckBrief is every refusal of b for a card of job, each naming the
// fault and the fitting action, so the writer corrects itself; none when
// b passes. refs resolves the brief's #N and its document links.
func CheckBrief(b BriefText, job BriefJob, refs RefResolver) []string {
	return checkBrief(b, job, refs, [4]bool{true, true, true, true})
}

// CheckEdit is CheckBrief over the fields edit sets, in the brief b they
// end in: a field the edit leaves as it stands is not checked again, so a
// card filed before the rules takes a why line under its old title.
func CheckEdit(b, edit BriefText, job BriefJob, refs RefResolver) []string {
	return checkBrief(b, job, refs, [4]bool{edit.Title != "", edit.Why != "", edit.Summary != "", edit.Body != ""})
}

// checkBrief is the refusals of b's fields that on marks: title, why line,
// summary and body.
func checkBrief(b BriefText, job BriefJob, refs RefResolver, on [4]bool) []string {
	var out []string
	add := func(lines ...string) {
		for _, l := range lines {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	title := strings.TrimSpace(b.Title)
	if on[0] {
		add(titleLength(title, job)...)
		if job != BriefHumanIssue {
			for _, check := range []func(string) []string{titleIssueRefs, branches, titleHexIDs, titleBackticks, titleCamelCase, titleSnakeCase, titleColon} {
				for _, x := range check(title) {
					add(fmt.Sprintf("The title names %q. A title uses plain words. Move %q to the text.", x, x))
				}
			}
		}
		switch job {
		case BriefWork:
			if startsWithArticle(title) {
				add("A work title starts with its verb, for example 'Narrow the close refusal to plans that hold criteria'.")
			}
		case BriefAsk:
			if !oneAsk(title) {
				add("An ask is one question or one instruction.")
			}
		}
	}
	if on[1] && job != BriefHumanIssue && !whyLine(b.Why, title) {
		add("The why line gives the reason in one sentence: who needs this, or which problem it removes.")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(b.Summary)); on[2] && job == BriefWork && (n == 0 || n > summaryMax) {
		add("The summary says what gets built, what it changes and how it is checked, under 600 characters.")
	}
	resolved := map[string]bool{}
	for i, f := range [4]string{title, b.Why, b.Summary, b.Body} {
		if !on[i] {
			continue
		}
		// a fenced block quotes a log or a file: no reference of the text's
		f = fencedBlock.ReplaceAllString(f, "")
		for _, m := range issueRef.FindAllStringSubmatch(linkTarget.ReplaceAllString(f, "]"), -1) {
			ok, asked := resolved[m[2]]
			if !asked {
				n, err := strconv.Atoi(m[2])
				ok = err == nil && resolves(refs, n)
				resolved[m[2]] = ok
			}
			if !ok {
				add(m[1] + " is no card or pull request of this project.")
			}
		}
		for _, m := range mdLink.FindAllStringSubmatch(f, -1) {
			add(linkRefusal(m[1], refs)...)
		}
		// a body may number its own steps
		if i < 3 {
			for _, l := range sectionLabels(f) {
				add("'" + l + "' names a part of a document. Link it as [words](path#heading), or say what it is.")
			}
		}
		// the title rule names a branch in a title already
		if i > 0 || job == BriefHumanIssue {
			for _, x := range branches(f) {
				add("The text names the branch " + x + ". The factory adds the start line itself. Remove the branch.")
			}
		}
		if i < 3 && job != BriefHumanIssue {
			for _, l := range writingguide.Lint(f) {
				add([3]string{titleName(job), "The why line", "The summary"}[i] + strings.TrimPrefix(l, "The text"))
			}
		}
	}
	return out
}

// sectionLabels are the bare section labels of text outside its links: a
// label followed by "times" counts, so "the task 3 times" names none.
func sectionLabels(text string) []string {
	text = mdLink.ReplaceAllString(text, "")
	var out []string
	for _, m := range sectionLabel.FindAllStringIndex(text, -1) {
		if rest := strings.ToLower(text[m[1]:]); !strings.HasPrefix(rest, " times") && !strings.HasPrefix(rest, " time ") {
			out = append(out, text[m[0]:m[1]])
		}
	}
	return out
}

func titleName(job BriefJob) string {
	if job == BriefAsk {
		return "The ask"
	}
	return "The title"
}

func titleLength(title string, job BriefJob) []string {
	limit := titleMax
	if job == BriefAsk {
		limit = askMax
	}
	name := titleName(job)
	switch n := utf8.RuneCountInString(title); {
	case n == 0:
		return []string{fmt.Sprintf("%s is empty. Write it in 1 to %d characters.", name, limit)}
	case strings.Contains(title, "\n"):
		return []string{fmt.Sprintf("%s has %d lines. Write it on one line.", name, strings.Count(title, "\n")+1)}
	case n > limit:
		return []string{fmt.Sprintf("%s has %d characters. Keep it under %d.", name, n, limit)}
	}
	return nil
}

func titleIssueRefs(title string) []string {
	var out []string
	for _, m := range issueRef.FindAllStringSubmatch(title, -1) {
		out = append(out, m[1])
	}
	return out
}

// titleHexIDs are the hex runs of 7 or more characters that hold a digit
// and a letter, as a commit or a task id does: a number or a word passes.
func titleHexIDs(title string) []string {
	var out []string
	for _, h := range hexID.FindAllString(title, -1) {
		if strings.ContainsAny(h, "0123456789") && strings.ContainsAny(h, "abcdef") {
			out = append(out, h)
		}
	}
	return out
}

func titleBackticks(title string) []string { return backticked.FindAllString(title, -1) }

// titleCamelCase are the words with two lower case letters or more before
// a capital, as a code name has: GitHub passes, and so does a system named
// like macOS.
func titleCamelCase(title string) []string {
	var out []string
	for _, w := range camelCase.FindAllString(title, -1) {
		if !osName.MatchString(w) {
			out = append(out, w)
		}
	}
	return out
}

func titleSnakeCase(title string) []string { return snakeCase.FindAllString(title, -1) }

// titleColon is the title from its first colon on, the second statement
// the colon joins, which belongs in the text. A colon between two digits,
// as in a time, joins nothing.
func titleColon(title string) []string {
	for i := 0; i < len(title); i++ {
		if title[i] == ':' && !(i > 0 && i < len(title)-1 && isDigit(title[i-1]) && isDigit(title[i+1])) {
			return []string{title[i:]}
		}
	}
	return nil
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// branches are the branches text names by the names the factory gives
// them: card/N, pull/N, factory/t-…, factory/schema-…,
// factory/template-update, salvage/t-… and factory-pins/…, each standing
// alone, not inside a URL or a path.
func branches(text string) []string {
	var out []string
	for _, m := range branchName.FindAllStringIndex(text, -1) {
		if m[0] > 0 && text[m[0]-1] == '/' {
			continue
		}
		out = append(out, strings.TrimRight(text[m[0]:m[1]], "."))
	}
	return out
}

func startsWithArticle(s string) bool {
	return strings.HasPrefix(s, "The ") || strings.HasPrefix(s, "A ") || strings.HasPrefix(s, "An ")
}

// oneAsk is whether title asks one thing: a question, or an instruction
// that starts with its verb, with at most one statement before it.
func oneAsk(title string) bool {
	ends := sentenceBreaks(title)
	last := title
	switch len(ends) {
	case 0:
	case 1:
		if strings.Contains(title[ends[0][2]:ends[0][3]], "?") {
			return false
		}
		last = title[ends[0][3]:]
	default:
		return false
	}
	return strings.HasSuffix(last, "?") || !startsWithArticle(last)
}

// sentenceBreaks are the ends of text's sentences before its last, each
// the punctuation and the space as a submatch: a full stop, a question
// mark or an exclamation mark before a capital, never after e.g. or i.e.
func sentenceBreaks(text string) [][]int {
	var out [][]int
	for _, m := range sentenceEnd.FindAllStringSubmatchIndex(text, -1) {
		if !abbreviation.MatchString(text[:m[2]]) {
			out = append(out, m)
		}
	}
	return out
}

// whyLine is whether why is one sentence of 1 to 160 runes that does not
// open with the title. A sentence ends at a full stop, a question mark or
// an exclamation mark before a capital, so "e.g. in" ends none.
func whyLine(why, title string) bool {
	why = strings.TrimSpace(why)
	n := utf8.RuneCountInString(why)
	return n > 0 && n <= whyMax && !strings.Contains(why, "\n") && len(sentenceBreaks(why)) == 0 &&
		(title == "" || len(why) < len(title) || !strings.EqualFold(why[:len(title)], title))
}

func resolves(refs RefResolver, n int) bool {
	_, _, ok := refs.Issue(n)
	return ok
}

// linkRefusal is the refusal of a markdown link to target that names no
// document, or no heading of it. Only a document, a path ending in .md, is
// checked: a link to the web, to a code file or inside the same text
// passes.
func linkRefusal(target string, refs RefResolver) []string {
	path, id, _ := strings.Cut(target, "#")
	path, _, _ = strings.Cut(path, "?")
	if linkScheme.MatchString(target) || !strings.HasSuffix(path, ".md") {
		return nil
	}
	return docRefusal(path, id, refs)
}

// docRefusal is the refusal of the document at path, or of its heading
// id when id is set, when the document repository lacks it.
func docRefusal(path, id string, refs RefResolver) []string {
	switch {
	case refs.Heading(path, id):
		return nil
	case id != "":
		return []string{fmt.Sprintf("The document %s has no heading %s.", path, id)}
	}
	return []string{fmt.Sprintf("There is no document %s.", path)}
}

// HumanIssueBrief is the brief of a human's issue of title and body: the
// title as the human wrote it, and as its why line the body's opening
// sentence when it has 1 to 160 characters, else none.
func HumanIssueBrief(title, body string) BriefText {
	first, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if ends := sentenceBreaks(first); len(ends) > 0 {
		first = first[:ends[0][3]]
	}
	why := strings.TrimSpace(first)
	if utf8.RuneCountInString(why) > whyMax {
		why = ""
	}
	return BriefText{Title: title, Why: why, Body: body}
}

// continuesLine is the line a card that continues another opens its text
// with.
var continuesLine = regexp.MustCompile(`^Continues #\d+\.(?:\n\n|$)`)

// ContinuesBody is the full text of a card that continues card old, a
// successor, a retry, a hand-over or a restarted plan: "Continues #N."
// first, then old's own text without the line it opened with when it
// continued another card.
func ContinuesBody(old Task) string {
	_, body, _ := strings.Cut(old.Objective, "\n\n")
	return IssueObjective(fmt.Sprintf("Continues #%d.", old.IssueNumber), continuesLine.ReplaceAllString(body, ""))
}

// BriefRefusal is the filing door's refusal of a writer's brief: each
// line names a fault and its fix, so the writer corrects itself.
type BriefRefusal struct{ Lines []string }

func (e *BriefRefusal) Error() string { return strings.Join(e.Lines, " ") }

type briefTryKey struct{}

// WithBriefTry is ctx for a filing on its writer's try n, from 1: the
// door refuses a brief that fails before try BriefTries. A filing without
// one is Go code's, a carry's or a webhook's, which no writer corrects:
// it files at once, marked unchecked when its brief fails.
func WithBriefTry(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, briefTryKey{}, n)
}

// WithBriefDocs is ctx for a writer's filing on repository, whose
// document links the door checks against the document repository tree
// names for it.
func WithBriefDocs(ctx context.Context, tree policy.Tree, repository string) (context.Context, error) {
	docs, err := docsOf(tree, repository)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, briefDocsKey{}, docs), nil
}

type briefDocsKey struct{}

// BriefTry is the writer's try ctx carries, BriefTries for none.
func BriefTry(ctx context.Context) int {
	n, _ := writerTry(ctx)
	return n
}

// writerTry is the writer's try ctx carries and whether it carries one.
func writerTry(ctx context.Context) (int, bool) {
	if n, ok := ctx.Value(briefTryKey{}).(int); ok {
		return n, true
	}
	return BriefTries, false
}

// BriefTally counts the refused tries of each writer at a door, by key,
// so the door files a writer's BriefTries-th try with its last text.
type BriefTally struct {
	mu sync.Mutex
	n  map[string]int
}

// Try is the number of key's coming try, from 1.
func (b *BriefTally) Try(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n[key] + 1
}

// Refused counts one more refused try of key.
func (b *BriefTally) Refused(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == nil {
		b.n = map[string]int{}
	}
	b.n[key]++
}

// Reset forgets key's tries: its card is filed, or its writer starts anew.
func (b *BriefTally) Reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.n, key)
}

// admit is the door of factoryd's filings: brief b of card t on its
// writer's try. On a pass, or on the try that files anyway, t takes the
// brief's why line, summary and unchecked mark, and admit answers nil;
// else the refusal, which goes back to the writer (pass).
func admit(t *Task, b BriefText, refs *botRefs, try int) error {
	unchecked, err := pass(b, BriefJobOf(*t), refs, try)
	if err != nil {
		return err
	}
	t.Why, t.Summary = strings.TrimSpace(b.Why), strings.TrimSpace(b.Summary)
	t.BriefUnchecked = unchecked
	return nil
}

// pass is the door's verdict on brief b as job on its writer's try:
// whether it files unchecked, or the *BriefRefusal its writer's tries
// count. refs resolves a writer's references, and one it could not read
// fails the writer's try and marks its last. A filing no writer
// corrects, refs nil, takes its references as the factory read them
// (filedRefs).
func pass(b BriefText, job BriefJob, refs *botRefs, try int) (bool, error) {
	var check RefResolver = filedRefs{}
	if refs != nil {
		check = refs
	}
	ok, refusals := FileBrief(b, job, check, try)
	failed := refs != nil && refs.err != nil
	switch {
	case failed && try < BriefTries:
		return false, &BriefRefusal{Lines: []string{"The brief's references could not be checked, as " + refs.err.Error() + ". File it again."}}
	case !ok:
		return false, &BriefRefusal{Lines: refusals}
	}
	return len(refusals) > 0 || failed, nil
}

// filedRefs resolves every reference: Go code's text names the cards and
// documents it read, and a carried text was checked at its own filing.
type filedRefs struct{}

func (filedRefs) Issue(int) (string, string, bool) { return "", "", true }
func (filedRefs) Heading(string, string) bool      { return true }

// botRefs resolves a brief's references in repo as the bot: #N is
// issue or pull request N of repo, and a document link a file of docs,
// the project's document repository, with the heading the renderer gives
// that id. A project with no document repository holds no documents, so
// its links pass. err keeps the first read that failed.
type botRefs struct {
	ctx        context.Context
	f          *Forgejo
	repo, docs Repo
	err        error
	tree       []string
}

func (r *botRefs) Issue(n int) (string, string, bool) {
	title, state, ok, err := r.f.Issue(r.ctx, r.repo.Owner, r.repo.Name, int64(n))
	r.fail(err)
	return title, state, ok
}

func (r *botRefs) Heading(path, id string) bool {
	if r.docs == (Repo{}) {
		return true
	}
	if r.tree == nil {
		entries, err := r.f.TreeEntries(r.ctx, r.docs.Owner, r.docs.Name, "HEAD")
		if err != nil {
			r.fail(fmt.Errorf("the documents of %s: %w", r.docs, err))
			return false
		}
		r.tree = make([]string, 0, len(entries))
		for _, e := range entries {
			r.tree = append(r.tree, e.Path)
		}
	}
	files := doclinks.Resolve(path, r.tree)
	if id == "" {
		return len(files) > 0
	}
	for _, f := range files {
		content, _, err := r.f.FileContent(r.ctx, r.docs.Owner, r.docs.Name, "", f)
		if err != nil {
			r.fail(fmt.Errorf("the document %s of %s: %w", f, r.docs, err))
			return false
		}
		if slices.Contains(doclinks.HeadingIDs(string(content)), id) {
			return true
		}
	}
	return false
}

func (r *botRefs) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// IssueText is brief b's issue body, its mirror on Forgejo: the why line
// and the summary, each when set, then the full text after a rule.
// ParseIssueText reads it back.
func IssueText(b BriefText) string {
	var head []string
	if why := strings.TrimSpace(b.Why); why != "" {
		head = append(head, mirrorWhy+why)
	}
	if summary := strings.TrimSpace(b.Summary); summary != "" {
		head = append(head, mirrorSummary+summary)
	}
	if len(head) == 0 {
		return b.Body
	}
	return strings.Join(head, "\n\n") + mirrorRule + b.Body
}

const (
	mirrorWhy     = "Why: "
	mirrorSummary = "Summary: "
	mirrorRule    = "\n\n---\n\n"
)

// ParseIssueText is the why line, the summary and the full text of an
// issue body IssueText wrote; a body of another shape is all full text.
func ParseIssueText(text string) (why, summary, body string) {
	head, body, ok := strings.Cut(text, mirrorRule)
	if !ok || !strings.HasPrefix(head, mirrorWhy) && !strings.HasPrefix(head, mirrorSummary) {
		return "", "", text
	}
	if rest, ok := strings.CutPrefix(head, mirrorWhy); ok {
		why, head, _ = strings.Cut(rest, "\n\n")
	}
	summary, _ = strings.CutPrefix(head, mirrorSummary)
	return why, summary, body
}
