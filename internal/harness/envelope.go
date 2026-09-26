package harness

// WorkerEnvelope is one stdout line from the agent worker: a streamed
// harness event or the final result. Pod stdout is the whole transport —
// no callback endpoint, so the sandbox egress policy never needs a rule
// back to the control plane, and the stream doubles as a log artifact.
type WorkerEnvelope struct {
	Type   string  `json:"type"` // "event" or "result"
	Event  *Event  `json:"event,omitempty"`
	Result *Result `json:"result,omitempty"`
}
