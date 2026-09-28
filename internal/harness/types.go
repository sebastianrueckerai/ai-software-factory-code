package harness

import (
	"context"
	"time"
)

// TaskBrief is the whole task as data, handed to a harness adapter
// (architecture 6.2). Capability requirements deliberately do not travel
// here: they live on the task row (required_capabilities) and are applied
// by the control plane when it picks the sandbox and mints credentials.
// Policy stays outside the LLM, so a harness cannot ask for capabilities.
type TaskBrief struct {
	Objective  string   `json:"objective"`
	Repository string   `json:"repository"`
	SHA        string   `json:"sha"`
	IssueURL   string   `json:"issue_url,omitempty"`
	Findings   []string `json:"findings,omitempty"`
	// Answers are the human's answers to the task's escalations, newest
	// first (ruling 118).
	Answers   []string `json:"answers,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

type EventKind string

// Detail keys factoryd's supervision reads, per kind:
//   - command: command, exit, output (the output's last 4 KiB, whole lines)
//   - tool_call: tool, status, args (long strings capped), args_sha256 (of
//     the full arguments' canonical JSON), error when status is "error"
//   - file_change: path, sha256 of the content after the change (absent or
//     empty when the file is gone or not a regular file)
//   - checkpoint from the worker: diff {files, insertions, deletions}
//     against the base commit (git diff --shortstat) and patch, the diff
//     text capped; the adapter's own checkpoints (a model step starting)
//     carry no detail
//   - supervisor (factoryd): detector, trip, digest summary, decision,
//     guidance, reason
const (
	EventModelCall  EventKind = "model_call"
	EventToolCall   EventKind = "tool_call"
	EventFileChange EventKind = "file_change"
	EventCommand    EventKind = "command"
	EventCheckpoint EventKind = "checkpoint"
	EventFinished   EventKind = "finished"
	EventPushed     EventKind = "pushed"      // the worker pushed the task branch; detail carries branch and commit
	EventPushFailed EventKind = "push_failed" // the worker's push failed or was refused; detail carries branch and error
	EventSupervisor EventKind = "supervisor"  // factoryd's supervisor decided after a trip; never from a worker
)

// Event is one normalized stream item from a running harness. The git
// diff and the artifact store stay the ground truth.
type Event struct {
	Time   time.Time      `json:"time"`
	Kind   EventKind      `json:"kind"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Result is the adapter's final verdict.
type Result struct {
	Success bool   `json:"success"`
	Summary string `json:"summary"`
}

// SandboxEnv names where a task runs.
type SandboxEnv struct {
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"service_account"`
	Image          string `json:"image"`
}

// Harness is the only thing factoryd knows about harnesses
// (architecture 6.2).
type Harness interface {
	Prepare(ctx context.Context, env SandboxEnv, brief TaskBrief) error
	Run(ctx context.Context, brief TaskBrief, emit func(Event)) Result
}
