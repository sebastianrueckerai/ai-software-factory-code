// Package textutil provides small helpers for formatting text.
package textutil

import "fmt"

// Plural returns a formatted string with n followed by the appropriate word
// form. It uses singular when n == 1 and plural otherwise.
func Plural(n int, singular, plural string) string {
	word := plural
	if n == 1 {
		word = singular
	}
	return fmt.Sprintf("%d %s", n, word)
}
