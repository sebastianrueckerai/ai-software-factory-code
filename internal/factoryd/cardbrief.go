package factoryd

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

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
	camelCase    = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z0-9]*\b`)
	snakeCase    = regexp.MustCompile(`\b[A-Za-z0-9]+(?:_[A-Za-z0-9]+)+\b`)
	branchName   = regexp.MustCompile("\\bbranch(?:es)?\\s+`?([^\\s`]*/[^\\s`]*)|\\b((?:card|pull)/\\d+|factory/t-[0-9a-f]+)\\b")
	sectionLabel = regexp.MustCompile(`\b(?i:fault|phase|requirement|step|task)\s+(?:\d+[a-z]?(?:\.\d+)*|[A-Z])\b`)
	mdLink       = regexp.MustCompile(`\[[^\]]*\]\(\s*([^\s)]*)[^)]*\)`)
	linkScheme   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	sentenceEnd  = regexp.MustCompile(`[.!?]+\s+`)
)

// CheckBrief is every refusal of b for a card of job, each naming the
// fault and the fitting action, so the writer corrects itself; none when
// b passes. refs resolves the brief's #N and its document links.
func CheckBrief(b BriefText, job BriefJob, refs RefResolver) []string {
	var out []string
	add := func(lines ...string) {
		for _, l := range lines {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	title := strings.TrimSpace(b.Title)
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
	if job != BriefHumanIssue && !whyLine(b.Why, title) {
		add("The why line gives the reason in one sentence: who needs this, or which problem it removes.")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(b.Summary)); job == BriefWork && (n == 0 || n > summaryMax) {
		add("The summary says what gets built, what it changes and how it is checked, under 600 characters.")
	}
	fields := []string{title, b.Why, b.Summary, b.Body}
	resolved := map[string]bool{}
	for _, f := range fields {
		for _, m := range issueRef.FindAllStringSubmatch(f, -1) {
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
		for _, l := range sectionLabel.FindAllString(mdLink.ReplaceAllString(f, ""), -1) {
			add("'" + l + "' names a part of a document. Link it as [words](path#heading), or say what it is.")
		}
	}
	texts := fields[1:]
	if job == BriefHumanIssue {
		texts = fields
	}
	for _, f := range texts {
		for _, x := range branches(f) {
			add("The text names the branch " + x + ". The factory adds the start line itself. Remove the branch.")
		}
	}
	if job != BriefHumanIssue {
		for _, f := range []struct{ name, text string }{{titleName(job), title}, {"The why line", b.Why}, {"The summary", b.Summary}} {
			for _, l := range writingguide.Lint(f.text) {
				add(f.name + strings.TrimPrefix(l, "The text"))
			}
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

// titleCamelCase are the words that start lower case and hold a capital,
// as a code name does: a brand such as GitHub starts with its capital.
func titleCamelCase(title string) []string { return camelCase.FindAllString(title, -1) }

func titleSnakeCase(title string) []string { return snakeCase.FindAllString(title, -1) }

// titleColon is the title from its first colon on: the second statement
// the colon joins, which belongs in the text.
func titleColon(title string) []string {
	if i := strings.IndexByte(title, ':'); i >= 0 {
		return []string{title[i:]}
	}
	return nil
}

// branches are the branch names text holds: the ref after the word
// branch, and the factory's own card/N, pull/N and factory/t-… names
// that stand alone, not inside a URL.
func branches(text string) []string {
	var out []string
	for _, m := range branchName.FindAllStringSubmatchIndex(text, -1) {
		g := 2
		if m[2] < 0 {
			g = 4
			if m[g] > 0 && text[m[g]-1] == '/' {
				continue
			}
		}
		out = append(out, strings.TrimRight(text[m[g]:m[g+1]], ".,;:!?)"))
	}
	return out
}

func startsWithArticle(s string) bool {
	return strings.HasPrefix(s, "The ") || strings.HasPrefix(s, "A ") || strings.HasPrefix(s, "An ")
}

// oneAsk is whether title asks one thing: a question, or an instruction
// that starts with its verb, with at most one statement before it.
func oneAsk(title string) bool {
	ends := sentenceEnd.FindAllStringIndex(title, -1)
	last := title
	switch len(ends) {
	case 0:
	case 1:
		if strings.Contains(title[ends[0][0]:ends[0][1]], "?") {
			return false
		}
		last = title[ends[0][1]:]
	default:
		return false
	}
	return strings.HasSuffix(last, "?") || !startsWithArticle(last)
}

// whyLine is whether why is one sentence of 1 to 160 runes that does not
// open with the title.
func whyLine(why, title string) bool {
	why = strings.TrimSpace(why)
	n := utf8.RuneCountInString(why)
	return n > 0 && n <= whyMax && !strings.Contains(why, "\n") && !sentenceEnd.MatchString(why) &&
		(title == "" || len(why) < len(title) || !strings.EqualFold(why[:len(title)], title))
}

func resolves(refs RefResolver, n int) bool {
	_, _, ok := refs.Issue(n)
	return ok
}

// linkRefusal is the refusal of a markdown link to target that names no
// document, or no heading of it; a link to the web or inside the same
// text passes.
func linkRefusal(target string, refs RefResolver) []string {
	if target == "" || linkScheme.MatchString(target) || strings.HasPrefix(target, "#") {
		return nil
	}
	path, id, _ := strings.Cut(target, "#")
	path, _, _ = strings.Cut(path, "?")
	switch {
	case refs.Heading(path, id):
		return nil
	case id != "":
		return []string{fmt.Sprintf("The document %s has no heading %s.", path, id)}
	}
	return []string{fmt.Sprintf("There is no document %s.", path)}
}
