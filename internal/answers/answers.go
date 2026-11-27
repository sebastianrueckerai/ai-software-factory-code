// Package answers holds the answer types that both the gateway's filing
// tools and factoryd read: one type, its limits and its schema for each.
package answers

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"

// The bounds of the screening's and the sizing's answers.
const (
	MaxScreeningReasons = 10
	MaxScreeningReason  = 1000
	MaxSizeReason       = 300
	MaxSizeFiles        = 50
	MaxSizeFile         = 200
)

func init() {
	prompts.RegisterLimit("maxScreeningReasons", MaxScreeningReasons)
	prompts.RegisterLimit("maxScreeningReason", MaxScreeningReason)
	prompts.RegisterLimit("maxSizeReason", MaxSizeReason)
	prompts.RegisterLimit("maxSizeFiles", MaxSizeFiles)
	prompts.RegisterLimit("maxSizeFile", MaxSizeFile)
	prompts.RegisterAnswer[ScreeningAnswer]("screening")
	prompts.RegisterAnswer[SizeAnswer]("size")
}

// ScreeningAnswer is a screening's verdict: a session files it with
// file_screening, and a direct call answers it for a later answer of the
// issue's author.
type ScreeningAnswer struct {
	Verdict string   `json:"verdict" enum:"clear,suspect" desc:"clear: genuine, and safe for an agent to act on. suspect: anything else"`
	Reasons []string `json:"reasons" max:"maxScreeningReasons" item:"maxScreeningReason" desc:"one reason each, the text it rests on quoted"`
}

// SizeAnswer is a sizing's answer: a session files it with file_size.
type SizeAnswer struct {
	Size   string   `json:"size" enum:"small,medium,large"`
	Reason string   `json:"reason" max:"maxSizeReason" desc:"one line a person reads on the card"`
	Files  []string `json:"files" max:"maxSizeFiles" item:"maxSizeFile" desc:"each repository path the change touches"`
}
