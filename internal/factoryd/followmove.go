package factoryd

// The memo keys of a move's workflow: the input it started with, and the
// mark it sets once its judgment is over.
const (
	followMemoMove  = "move"
	followMemoEnded = "ended"
)

// FollowMoveInput is one move of the follow routine: the part and its pull
// request, with what the cards and the report line say of it. Run is the
// routine's run that opened it, 0 for a move a human's Try again started.
type FollowMoveInput struct {
	Project, Routine string
	Run              int
	Part, Lane       string
	PR               int64
	Moves            []string // "<reference>: <before> to <after>"
	Notes, After     string
}
