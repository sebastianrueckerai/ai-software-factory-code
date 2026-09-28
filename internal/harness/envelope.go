package harness

// WorkerEnvelope is one stdout line from the agent worker: a streamed
// harness event or the final result. Pod stdout is the whole transport —
// no callback endpoint, so the sandbox egress policy never needs a rule
// back to the control plane, and the stream doubles as a log artifact.
type WorkerEnvelope struct {
	Type   string  `json:"type"` // "event", "result" or "repro"
	Event  *Event  `json:"event,omitempty"`
	Result *Result `json:"result,omitempty"`
	Repro  *Repro  `json:"repro,omitempty"`
}

// Repro is the worker's reproduction mode's report: the command's exit
// code (-1: it could not run or timed out) and the tail of its combined
// output.
type Repro struct {
	Exit   int    `json:"exit"`
	Output string `json:"output"`
}
