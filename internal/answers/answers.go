// Package answers holds the answer types that both the gateway's filing
// tools and factoryd read: one type, its limits and its schema for each.
package answers

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"

// The bounds of the screening's, the sizing's and the map's answers.
const (
	MaxScreeningReasons = 10
	MaxScreeningReason  = 1000
	MaxSizeReason       = 300
	MaxSizeFiles        = 50
	MaxSizeFile         = 200
	MaxMapDirs          = 40
	MaxMapDirPath       = 200
	MaxMapRole          = 160
	MaxMapConventions   = 12
	MaxMapConvention    = 200
	MaxMapCheck         = 200
)

func init() {
	prompts.RegisterLimit("maxScreeningReasons", MaxScreeningReasons)
	prompts.RegisterLimit("maxScreeningReason", MaxScreeningReason)
	prompts.RegisterLimit("maxSizeReason", MaxSizeReason)
	prompts.RegisterLimit("maxSizeFiles", MaxSizeFiles)
	prompts.RegisterLimit("maxSizeFile", MaxSizeFile)
	prompts.RegisterLimit("maxMapDirs", MaxMapDirs)
	prompts.RegisterLimit("maxMapDirPath", MaxMapDirPath)
	prompts.RegisterLimit("maxMapRole", MaxMapRole)
	prompts.RegisterLimit("maxMapConventions", MaxMapConventions)
	prompts.RegisterLimit("maxMapConvention", MaxMapConvention)
	prompts.RegisterLimit("maxMapCheck", MaxMapCheck)
	prompts.RegisterAnswer[ScreeningAnswer]("screening")
	prompts.RegisterAnswer[SizeAnswer]("size")
	prompts.RegisterAnswer[MapAnswer]("map")
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

// MapAnswer is the repository map step's answer: a session files it with
// file_map, and the planner reads it instead of the repository.
type MapAnswer struct {
	Directories []DirNote `json:"directories" max:"maxMapDirs" desc:"the directories that matter, each with its role"`
	Conventions []string  `json:"conventions" max:"maxMapConventions" item:"maxMapConvention" desc:"one convention every card follows, each in one line"`
	Check       string    `json:"check" max:"maxMapCheck" desc:"the command that checks a change"`
}

// DirNote is one directory of the map and what it is for.
type DirNote struct {
	Path string `json:"path" max:"maxMapDirPath" desc:"relative to the repository's root"`
	Role string `json:"role" max:"maxMapRole" desc:"what the directory holds, in one line"`
}
