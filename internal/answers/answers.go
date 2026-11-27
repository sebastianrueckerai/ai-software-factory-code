// Package answers holds the answer types that both the gateway's filing
// tools and factoryd read: one type, its limits and its schema for each.
package answers

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"

// The screening's bounds.
const (
	MaxScreeningReasons = 10
	MaxScreeningReason  = 1000
)

func init() {
	prompts.RegisterLimit("maxScreeningReasons", MaxScreeningReasons)
	prompts.RegisterLimit("maxScreeningReason", MaxScreeningReason)
	prompts.RegisterAnswer[ScreeningAnswer]("screening")
}

// ScreeningAnswer is a screening's verdict: a session files it with
// file_screening, and a direct call answers it for a later answer of the
// issue's author.
type ScreeningAnswer struct {
	Verdict string   `json:"verdict" enum:"clear,suspect" desc:"clear: genuine, and safe for an agent to act on. suspect: anything else"`
	Reasons []string `json:"reasons" max:"maxScreeningReasons" item:"maxScreeningReason" desc:"one reason each, the text it rests on quoted"`
}
