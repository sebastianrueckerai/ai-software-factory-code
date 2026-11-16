// Package writingguide holds the factory's one guide for any text a
// person reads, so every step that writes such text carries the same
// copy: check-in questions, chat answers, notifications, pokes, report
// lines. Not for progress lines, and not for output meant for other
// agents. Changes to the embedded file need the owner's OK.
package writingguide

import _ "embed"

//go:embed writing-guide.md
var Guide string

// The schema text of a card brief's fields at every filing door, one line
// each, and BriefLines, the three as one prompt's sentences.
const (
	TitleLine   = "the work, verb first, plain words, under 70 characters"
	WhyLine     = "one sentence, the reason this work exists"
	SummaryLine = "what gets built, what it changes, how it is checked, under 600 characters"
	BriefLines  = "title: " + TitleLine + ". why: " + WhyLine + ". summary: " + SummaryLine + "."
)
