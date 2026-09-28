package textutil

import "unicode/utf8"

// Truncate keeps at most s's first n bytes, cut on a character boundary and
// marked "…" when cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// Tail keeps at most s's last n bytes, cut on a character boundary and
// marked "…" in front when cut.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}
