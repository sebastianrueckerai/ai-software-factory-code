package console

// FaceKinds is the board's own set of card kinds: every kind the board
// can render, the set the face-conformance acceptance test enumerates so
// a kind added to the board without its fixture and face rules fails the
// conformance run with a message naming the kind.
var FaceKinds = []string{
	"agent",
	"human",
	"chat",
	"merge",
	"resource",
	"plan",
	"criteria",
	"criteria-rerun",
}
