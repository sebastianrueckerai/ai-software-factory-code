// Package writingguide holds the factory's one guide for any text a
// person reads, so every step that writes such text carries the same
// copy: check-in questions, chat answers, notifications, pokes, report
// lines. Not for progress lines, and not for output meant for other
// agents. Changes to the embedded file need the owner's OK.
package writingguide

import _ "embed"

//go:embed writing-guide.md
var Guide string
