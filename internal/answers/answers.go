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
	prompts.RegisterAnswer[SpecAnswer]("spec")
	prompts.RegisterAnswer[CriteriaAnswer]("criteria")
	prompts.RegisterAnswer[CardsAnswer]("cards")
	prompts.RegisterAnswer[ReplanAnswer]("replan")
	prompts.RegisterAnswer[RevertAnswer]("revert")
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

// SpecAnswer is the planner's first step: a session files it with
// file_spec. The keys are the plan's.
type SpecAnswer struct {
	Spec      SpecBody   `json:"spec"`
	Summary   string     `json:"summary" desc:"a paragraph: what gets built and in which order"`
	Questions []Question `json:"questions,omitempty" desc:"open points the spec leaves to the human, from your attack on your own draft"`
}

// SpecBody is the spec: the outcome and the glossary.
type SpecBody struct {
	Outcome  string   `json:"outcome" desc:"the outcome in a few sentences"`
	Glossary []string `json:"glossary,omitempty" desc:"one entry per component, as term: what it is"`
}

// CriteriaAnswer is the planner's second step: a session files it with
// file_criteria.
type CriteriaAnswer struct {
	Criteria []Criterion `json:"criteria" desc:"the spec's proof, one object per criterion"`
}

// Criterion is one criterion of the spec. The kind names the fields it uses,
// and the rest stay empty.
type Criterion struct {
	ID       string `json:"id" desc:"snake_case, unique in the plan"`
	Kind     string `json:"kind" enum:"test,invariant,judgement,human" desc:"test: names claim, inputs, expected, suite and name; the citing card, or a proof card you file, lands the test, so a card whose directories hold the suite writes it. invariant: names suite alone, a package path whose tests stay green. judgement: names claim, inputs (what the artifact a test writes must show) and prompt. human: names claim and evidence"`
	Promise  string `json:"promise,omitempty" desc:"the claim in plain words, for a human who reads the board and not the plan"`
	Claim    string `json:"claim,omitempty"`
	Inputs   string `json:"inputs,omitempty"`
	Expected string `json:"expected,omitempty" desc:"the numbers the test expects"`
	Suite    string `json:"suite,omitempty" desc:"a package path such as ./..."`
	Name     string `json:"name,omitempty" desc:"the test's name, such as TestSizeBound"`
	Prompt   string `json:"prompt,omitempty" desc:"the pinned prompt a model judges the artifact by"`
	Evidence string `json:"evidence,omitempty" desc:"what the approver will see"`
	Replaces string `json:"replaces,omitempty" desc:"the id of the criterion this one changes"`
}

// CardsAnswer is the planner's third step: a session files it with
// file_cards. Its cards are the round's.
type CardsAnswer struct {
	Outline   []string   `json:"outline,omitempty" desc:"the parts of the work a later round plans, one short line each, in order; leave it out when this round finishes the feature"`
	Cards     []Card     `json:"cards" desc:"the cards of this round"`
	Questions []Question `json:"questions,omitempty" desc:"open points the plan leaves to the human"`
}

// ReplanAnswer is the replan step's answer: the whole plan again, with a
// session's file_replan. It holds what the step changes and keeps.
type ReplanAnswer struct {
	Spec      SpecBody    `json:"spec"`
	Summary   string      `json:"summary" desc:"a paragraph: what gets built and in which order"`
	Outline   []string    `json:"outline,omitempty" desc:"the parts of the work a later round plans, one short line each, in order"`
	Criteria  []Criterion `json:"criteria,omitempty" desc:"the spec's proof, one object per criterion"`
	Cards     []Card      `json:"cards" desc:"the cards of the revision: a kept card by its key, a changed card as a new card that names the old one in from"`
	Questions []Question  `json:"questions,omitempty" desc:"open points the plan leaves to the human"`
}

// RevertAnswer is the revert step's answer: a session files it with
// file_revert.
type RevertAnswer struct {
	Cards []Revert `json:"cards" desc:"the merged cards whose work comes off main"`
}

// Revert is one merged card whose work comes off main.
type Revert struct {
	Key    string `json:"key" desc:"the merged card's key"`
	Reason string `json:"reason" desc:"what breaks if the card's work stays on main"`
}

// Question is an open point for a human with the choices offered.
type Question struct {
	Text        string   `json:"text" desc:"the question, in one sentence"`
	Choices     []string `json:"choices" desc:"two or three answers"`
	Changes     []string `json:"changes" desc:"one short line per choice, in its order: what the choice changes in the plan"`
	Recommended string   `json:"recommended" desc:"one of the choices, verbatim"`
	Grounds     string   `json:"grounds" desc:"why, in one sentence"`
	Origin      string   `json:"origin,omitempty" enum:"self_attack" desc:"self_attack for a question your attack on your draft found"`
}

// Card is one card of a plan, as the planner writes it: one pull request,
// one session. The stamps factoryd adds are not here.
type Card struct {
	Key         string   `json:"key" desc:"lowercase letters, digits and dashes, 2 to 32"`
	Role        string   `json:"role" enum:"foundation,feature,integration"`
	Title       string   `json:"title"`
	Why         string   `json:"why"`
	Summary     string   `json:"summary"`
	Section     string   `json:"section,omitempty" desc:"the part of a document the card builds: its path in the document repository with its #heading"`
	Brief       string   `json:"brief" desc:"markdown: what to build and how it is checked"`
	Directories []string `json:"directories,omitempty" desc:"paths relative to the repository's root, guidance for the card's agent"`
	DependsOn   []string `json:"depends_on,omitempty" desc:"the keys of the cards this card needs merged first"`
	Acceptance  []Item   `json:"acceptance"`
	Size        string   `json:"size" enum:"small,medium,large" desc:"anything bigger is two cards"`
	Rung        string   `json:"rung,omitempty" enum:"cheap,frontier" desc:"the model rung the card starts on: cheap for routine, well-specified work, frontier for a large change; leave it out to derive it from the size"`
	RungReason  string   `json:"rung_reason,omitempty" desc:"one line"`
	Image       string   `json:"image,omitempty" desc:"the project's pinned image the card runs on; leave it out for the default"`
	From        string   `json:"from,omitempty" desc:"the key of the old card whose work this card starts from"`
}

// Item is one acceptance item of a card.
type Item struct {
	Criterion string `json:"criterion,omitempty" desc:"the criterion id"`
	Text      string `json:"text" desc:"what the card's tests prove, with the inputs and the numbers they expect"`
}
