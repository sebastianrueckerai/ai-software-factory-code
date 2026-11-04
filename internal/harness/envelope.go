package harness

// WorkerEnvelope is one stdout line from the agent worker: a streamed
// harness event or the final result. Pod stdout is the whole transport —
// no callback endpoint, so the sandbox egress policy never needs a rule
// back to the control plane, and the stream doubles as a log artifact.
type WorkerEnvelope struct {
	Type   string       `json:"type"` // "event", "result", "repro", "proof", "proofs" or "push"
	Event  *Event       `json:"event,omitempty"`
	Result *Result      `json:"result,omitempty"`
	Repro  *Repro       `json:"repro,omitempty"`
	Proof  *ProofRun    `json:"proof,omitempty"`
	Proofs *Proofs      `json:"proofs,omitempty"`
	Push   *PushRequest `json:"push,omitempty"`
}

// PushRequest is the worker asking for its push token once every agent
// process is dead and Commit is made: factoryd mints the token then and
// posts it to the worker's POST /push on MessagePort.
type PushRequest struct {
	Commit string `json:"commit"`
}

// Repro is the worker's reproduction mode's report: the command's exit
// code (-1: it could not run or timed out) and the tail of its combined
// output.
type Repro struct {
	Exit   int    `json:"exit"`
	Output string `json:"output"`
}

// Proof is one entry of the proofs mode's list: the check script's suite,
// the one test's name when given, and a file the worker puts in the tree
// for this entry's own run alone.
type Proof struct {
	ID    string     `json:"id"`
	Suite string     `json:"suite"`
	Name  string     `json:"name,omitempty"`
	File  *ProofFile `json:"file,omitempty"`
}

type ProofFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ProofRun is one entry's run, a stdout line of its own: whether check
// test started for it, whatever its exit (false when the run's budget
// ran out before the entry or cut it short, or the worker could not set
// it up), check test's exit (-1: it could not run or ran out of time),
// its output tail, its report gzipped (nil when it wrote none or one too
// large for the line), and the regular files it wrote under
// CHECK_ARTIFACTS, each cut at 256 KiB, as many as the line holds. Output
// names what was left out.
type ProofRun struct {
	ID        string            `json:"id"`
	Started   bool              `json:"started,omitempty"`
	Exit      int               `json:"exit"`
	Output    string            `json:"output"`
	Report    []byte            `json:"report,omitempty"`
	Artifacts map[string][]byte `json:"artifacts,omitempty"`
}

// Proofs ends the proofs mode's report: Error says why no entry ran.
type Proofs struct {
	Error string `json:"error,omitempty"`
}
