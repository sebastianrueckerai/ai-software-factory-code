// Package textutil provides small helpers for formatting text.
package textutil

import (
	"fmt"
	"strings"
)

// Plural returns a formatted string with n followed by the appropriate word
// form: "n singular" when n is exactly 1, and "n plural" for any other count,
// including zero and negative counts.
func Plural(n int, singular, plural string) string {
	word := plural
	if n == 1 {
		word = singular
	}
	return fmt.Sprintf("%d %s", n, word)
}

// AndList joins items as a person reads them: "a", "a and b", "a, b and c".
func AndList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
