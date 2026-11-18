package factoryd

import "sync"

// The unexported names the pg tests of the faces and of an information
// request reach.
var (
	PgOffersOf        = offersOf
	PgOffersOfAnswers = offersOfAnswers
	PgIsTerminal      = isTerminal
)

func (a AgentAsk) PgReason() string { return a.reason() }

func (r *Runner) PgRequestLock(id string) *sync.Mutex { return r.requestLock(id) }

func (r *Runner) PgStore() Store { return r.store }
