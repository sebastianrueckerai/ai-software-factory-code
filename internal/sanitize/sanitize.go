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
	mention         = regexp.MustCompile(`(^|[^\w@./-])@([A-Za-z0-9][\w.-]*)`)
	// Forgejo's closing and reopening keywords followed by a reference
	closing = regexp.MustCompile(`(?i)\b(close[sd]?|fix(?:e[sd])?|resolve[sd]?|reopen(?:s|ed)?)(:?[ \t]+)((?:[\w.-]+/[\w.-]+)?#\d|https?://)`)
	link    = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s<>"'` + "`" + `)\]]+|\bwww\.[^\s<>"'` + "`" + `)\]]+|(?:^|[\s(<\["'])//[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+[^\s<>"'` + "`" + `)\]]*`)
)

// Inbound is issue text or a comment as an agent's brief carries it: HTML
// and Markdown comments, which Forgejo renders as nothing, removed;
// mentions and closing keywords neutralised.
func Inbound(s string) string {
	s = htmlComment.ReplaceAllString(s, "")
	s = markdownComment.ReplaceAllString(s, "")
	return neutralise(s)
}

// Outbound is agent-written text as factoryd posts it on Forgejo: mentions
// and closing keywords neutralised, and every link whose host is not one of
// hosts or under one defanged (hxxps://evil[.]example).
func Outbound(s string, hosts []string) string {
	s = neutralise(s)
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

// neutralise breaks mentions (＠name) and closing keywords ("(fixes) #12")
// so Forgejo neither notifies nor closes anything.
func neutralise(s string) string {
	s = mention.ReplaceAllString(s, "${1}＠${2}")
	return closing.ReplaceAllString(s, "(${1})${2}${3}")
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
