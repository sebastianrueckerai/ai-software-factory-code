package console

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The filing flags of roadmap 125: the text of a card's title or body the
// sanitiser or the provider would change on the card's way to the agent,
// read at the two doors that file a card — the chat's new_card tool,
// which refuses with the findings, and the console's New task form, which
// shows them above the form and files the text as written only on a
// second submit. Words are never flagged: the provider's content filter
// cannot be predicted by a word list.

var (
	// a closing keyword before an issue reference, which the sanitiser
	// neutralises as "(fixes) #12" so nothing closes
	filedClosing = regexp.MustCompile(`(?i)(^|[^\pL\pN])(close[sd]?|fix(?:e[sd])?|resolve[sd]?)(:?[ \t]+)((?:[\w.-]+/[\w.-]+)?#\d+)`)
	// a mention of a user or a team, which the sanitiser widens as "＠name"
	// so nobody is notified
	filedMention = regexp.MustCompile(`(^|[^A-Za-z0-9@./-])@([A-Za-z0-9][\w.-]*(?:/[\w.-]+)?)`)
	// an HTML comment, however many lines it spans, which the sanitiser
	// strips: the agent never reads it
	filedComment = regexp.MustCompile(`(?s)<!--.*?(-->|$)`)
)

// filedInvisible are the characters no eye sees: each rides a text unseen
// (roadmap 125: a zero-width character sat invisible in a Go string).
var filedInvisible = map[rune]string{
	'\u200b': "U+200B, a zero-width space",
	'\u200c': "U+200C, a zero-width non-joiner",
	'\u200d': "U+200D, a zero-width joiner",
	'\u2060': "U+2060, a word joiner",
	'\ufeff': "U+FEFF, a zero-width no-break space",
	'\u00ad': "U+00AD, a soft hyphen",
}

// why each kind changes, the finding's second half
const (
	filedClosingWhy = `a closing keyword before an issue reference; the sanitiser neutralises it as "(fixes) #12" so nothing closes`
	filedMentionWhy = "a mention of a user or a team; the sanitiser widens its @ so nobody is notified"
	filedCommentWhy = "an HTML comment; the sanitiser strips it and the agent never reads it"
	filedCodeWhy    = "is not ASCII inside code; write its escape so the fixture reads the same on its way"
)

// filingFlag is one finding in a card's title or body: the line the text
// sits on (the title is line 1, the body's lines follow it), the text
// itself and why it changes on its way to the agent.
type filingFlag struct {
	Line int
	Text string
	Why  string
	col  int // where the text sits on its line, for the reading order
}

// String is the finding on one line, its text quoted so an invisible
// character shows as its escape.
func (f filingFlag) String() string {
	return fmt.Sprintf("line %d: %q %s", f.Line, f.Text, f.Why)
}

// filingFlags are the findings in a card's title and body, in reading
// order: every text the sanitiser or the provider would change on its way
// to the agent (roadmap 125) — a closing keyword before an issue
// reference, a mention of a user or a team, an HTML comment, an invisible
// character, a non-ASCII character inside a fenced code block or inline
// code — each with its line, the text and why it changes. Words are not
// flagged: the provider's content filter cannot be predicted by a word
// list.
func filingFlags(title, body string) []filingFlag {
	lines := append([]string{title}, strings.Split(body, "\n")...)
	var out []filingFlag
	add := func(line, col int, text, why string) {
		out = append(out, filingFlag{Line: line, Text: text, Why: why, col: col})
	}
	// the plain finds: every line, fences and code spans included, for
	// the sanitiser changes them there too
	for i, line := range lines {
		for _, m := range filedClosing.FindAllStringSubmatchIndex(line, -1) {
			add(i+1, m[4], line[m[4]:m[9]], filedClosingWhy)
		}
		for _, m := range filedMention.FindAllStringSubmatchIndex(line, -1) {
			add(i+1, m[3], line[m[3]:m[5]], filedMentionWhy) // from the @ to the name's end
		}
		for col, r := range line {
			if why, ok := filedInvisible[r]; ok {
				add(i+1, col, string(r), why+"; invisible, it rides the text unseen")
			}
		}
	}
	// an HTML comment may span lines; its finding sits where it starts
	whole := strings.Join(lines, "\n")
	for _, m := range filedComment.FindAllStringSubmatchIndex(whole, -1) {
		before := whole[:m[0]]
		add(1+strings.Count(before, "\n"), m[0]-strings.LastIndexByte(before, '\n')-1, whole[m[0]:m[1]], filedCommentWhy)
	}
	// non-ASCII inside code: an inline span anywhere, a fenced block whole
	filedSpans(lines[0], 1, add) // the title opens no fence
	fenced, fenceCh, fenceLen := false, byte(0), 0
	for i := 1; i < len(lines); i++ {
		line, open := lines[i], strings.TrimLeft(lines[i], " ")
		run, ch, ok := fenceTicks(open)
		indent := len(line) - len(open)
		switch {
		case !fenced && indent <= 3 && ok:
			fenced, fenceCh, fenceLen = true, ch, run
			filedCode(line, i+1, 0, add) // the fence's own line is part of the block
		case fenced && indent <= 3 && ok && ch == fenceCh && run >= fenceLen && strings.TrimSpace(open[run:]) == "":
			fenced = false
			filedCode(line, i+1, 0, add) // so is its closing fence
		case fenced:
			filedCode(line, i+1, 0, add)
		default:
			filedSpans(line, i+1, add)
		}
	}
	slices.SortStableFunc(out, func(a, b filingFlag) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.col, b.col))
	})
	return out
}

// filedCode flags the non-ASCII characters of code on line n at base and
// on: a card the agent must reproduce writes them as escapes. The
// invisible characters are filed wherever they sit, so they are not filed
// twice here.
func filedCode(code string, n, base int, add func(line, col int, text, why string)) {
	for off, r := range code {
		if _, invisible := filedInvisible[r]; r >= 0x80 && !invisible {
			add(n, base+off, string(r), fmt.Sprintf("(U+%04X) %s", r, filedCodeWhy))
		}
	}
}

// filedSpans flags the non-ASCII characters of s's inline code, the spans
// a run of backticks opens and the next run of as many closes; a run no
// such run closes is literal text.
func filedSpans(s string, n int, add func(line, col int, text, why string)) {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := 1
		for i+run < len(s) && s[i+run] == '`' {
			run++
		}
		closed, j := -1, i+run
		for j < len(s) {
			if s[j] != '`' {
				j++
				continue
			}
			m := 1
			for j+m < len(s) && s[j+m] == '`' {
				m++
			}
			if m == run {
				closed = j
				break
			}
			j += m
		}
		if closed < 0 {
			i += run
			continue
		}
		filedCode(s[i+run:closed], n, i+run, add)
		i = closed + run
	}
}

// fenceTicks is the leading run of backticks or tildes at the start of s,
// and whether it is long enough (3 or more) to open or close a fenced
// code block.
func fenceTicks(s string) (n int, ch byte, ok bool) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0, false
	}
	ch = s[0]
	for n < len(s) && s[n] == ch {
		n++
	}
	return n, ch, n >= 3
}

// flaggedCard is the refusal a card's flagged text earns at the chat's
// door: every finding on its line, and the ask to rewrite and file again.
func flaggedCard(flags []filingFlag) string {
	lines := make([]string, len(flags))
	for i, f := range flags {
		lines[i] = f.String()
	}
	return "The card's title or description holds text the sanitiser or the provider would change on its way to the agent: " +
		strings.Join(lines, "; ") + ". Rewrite those and file the card again."
}
