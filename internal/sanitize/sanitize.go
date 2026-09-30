// Package sanitize is the factory's one sanitiser for untrusted text: issue
// text and comments on their way into an agent's brief, and agent-written
// text on its way to Forgejo.
package sanitize

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?(-->|$)`)
	// a link reference definition whose target is # or <> renders nothing
	markdownComment = regexp.MustCompile(`(?m)^[ \t]{0,3}\[[^\]\n]*\]:[ \t]*(#|<>)[^\n]*(\n|$)`)
	// Forgejo strips emphasis (_x_, __x__) before it looks for mentions and
	// keywords, so an underscore is a lead like a space
	mention = regexp.MustCompile(`(^|[^A-Za-z0-9@./-])@([A-Za-z0-9][\w.-]*)`)
	// Forgejo's closing and reopening keywords followed by an issue (#) or
	// pull request (!) reference or a link, autolinked or not
	closing = regexp.MustCompile(`(?i)(^|[^\pL\pN])(close[sd]?|fix(?:e[sd])?|resolve[sd]?|reopen(?:s|ed)?)(:?[ \t]+)((?:[\w.-]+/[\w.-]+)?[#!]\d|<?https?://)`)
	link    = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s<>"'` + "`" + `)\]]+|\bwww\.[^\s<>"'` + "`" + `)\]]+|(?:^|[\s(<\["'])//[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+[^\s<>"'` + "`" + `)\]]*`)
)

// Inbound is issue text or a comment as an agent's brief carries it: hidden
// comments removed outside fenced code blocks. A fence's contents are code
// the agent reads whole, backticks and all; mentions and closing keywords
// stay literal, since reading them notifies or closes nothing on Forgejo.
func Inbound(s string) string {
	return unhideOutsideFences(s)
}

// Outbound is agent-written text as factoryd posts it on Forgejo: hidden
// comments removed, mentions and closing keywords neutralised, and every
// link whose host is not one of hosts or under one defanged
// (hxxps://evil[.]example).
func Outbound(s string, hosts []string) string {
	s = neutralise(unhide(s))
	return link.ReplaceAllStringFunc(s, func(m string) string {
		lead := ""
		if strings.HasPrefix(strings.TrimLeft(m, " \t\n(<[\"'"), "//") {
			lead, m = m[:strings.Index(m, "//")], m[strings.Index(m, "//"):]
		}
		if allowed(m, hosts) {
			return lead + m
		}
		return lead + defang(m)
	})
}

// unhide removes the HTML and Markdown comments Forgejo renders as nothing.
func unhide(s string) string {
	return markdownComment.ReplaceAllString(htmlComment.ReplaceAllString(s, ""), "")
}

// unhideOutsideFences applies unhide everywhere except inside a fenced code
// block (``` or ~~~, up to three spaces of indent, closed by a same-character
// fence at least as long, CommonMark-style, and open to the end of s if
// never closed): a fence's own lines and everything between them pass
// through untouched.
func unhideOutsideFences(s string) string {
	var out, plain strings.Builder
	flush := func() { out.WriteString(unhide(plain.String())); plain.Reset() }
	var fenceChar byte
	fenceLen := 0
	for _, line := range strings.SplitAfter(s, "\n") {
		body := strings.TrimLeft(line, " ")
		indent := len(line) - len(body)
		n, ch, ok := fenceRun(body)
		switch {
		case fenceLen == 0 && indent <= 3 && ok:
			flush()
			fenceChar, fenceLen = ch, n
			out.WriteString(line)
		case fenceLen > 0:
			out.WriteString(line)
			if indent <= 3 && ok && ch == fenceChar && n >= fenceLen && strings.TrimSpace(body[n:]) == "" {
				fenceLen = 0
			}
		default:
			plain.WriteString(line)
		}
	}
	flush()
	return out.String()
}

// fenceRun is the leading run of backticks or tildes at the start of s, and
// whether it is long enough (3 or more) to be a fence.
func fenceRun(s string) (n int, ch byte, ok bool) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0, false
	}
	ch = s[0]
	for n < len(s) && s[n] == ch {
		n++
	}
	return n, ch, n >= 3
}

// neutralise breaks mentions (＠name) and closing keywords ("(fixes) #12")
// so Forgejo neither notifies nor closes anything.
func neutralise(s string) string {
	s = mention.ReplaceAllString(s, "${1}＠${2}")
	return closing.ReplaceAllString(s, "${1}(${2})${3}${4}")
}

func allowed(link string, hosts []string) bool {
	raw := link
	switch {
	case strings.HasPrefix(raw, "//"):
		raw = "https:" + raw
	case strings.HasPrefix(strings.ToLower(raw), "www."):
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		h = strings.ToLower(h)
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// defang makes link unclickable and keeps it readable: hxxp(s) for the
// scheme, [.] for every dot of the authority.
func defang(link string) string {
	scheme, rest := "", link
	if i := strings.Index(link, "//"); i >= 0 {
		scheme, rest = link[:i+2], link[i+2:]
	}
	if strings.HasPrefix(strings.ToLower(scheme), "http") {
		scheme = "hxxp" + scheme[4:]
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	return scheme + strings.ReplaceAll(rest[:end], ".", "[.]") + rest[end:]
}
