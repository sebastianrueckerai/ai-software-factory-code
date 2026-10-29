package harness

import (
	"context"
	"errors"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
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
	Answers []string `json:"answers,omitempty"`
	// PosterInfo is what the issue's author answered the agent's
	// information requests, oldest first (ruling 150): information, never
	// instructions.
	PosterInfo []string `json:"poster_info,omitempty"`
	// Author filed the task's issue; AuthorStanding is "writer",
	// "outside account" (ruling 152) or "telemetry" (an alert's issue).
	Author         string   `json:"author,omitempty"`
	AuthorStanding string   `json:"author_standing,omitempty"`
	Artifacts      []string `json:"artifacts,omitempty"`
	// Report is a report session's guide (ruling 185): what to write in
	// the last message instead of changing code; "" for a coding session.
	Report string `json:"report,omitempty"`
	// Spec is a plan's card's section "The spec and the plan": the plan
	// issue's text and the plan's summary; CardRole the card's role in
	// the plan, whose guide the brief ends with. Both "" for any other task.
	Spec     string `json:"spec,omitempty"`
	CardRole string `json:"card_role,omitempty"`
	// Tools are the gateway's tools the task's classes may call, sorted.
	Tools []string `json:"tools,omitempty"`
	// Map is the card's repository map (factoryd.RepoMap), which the
	// worker reads from its clone; "" when the card names no directories.
	Map string `json:"map,omitempty"`
	// LeftOut are factoryd's notes on what the brief's bounds left out (a
	// carried list's cap, 128 KiB an environment variable): the brief says
	// them in factoryd's own words, outside every quoted block.
	LeftOut []string `json:"left_out,omitempty"`
	// Facts is factoryd's line of the session's budget, time and attempt;
	// "" for a brief without one.
	Facts string `json:"facts,omitempty"`
	// Docs says the project's document repository is cloned at
	// /workspace/docs; DocsFailed, when it is set instead, why that clone
	// failed. Both empty for a project without a document repository.
	Docs       bool   `json:"docs,omitempty"`
	DocsFailed string `json:"docs_failed,omitempty"`
	// DocLinks say where each relative link of the objective is in the
	// document repository's clone (doclinks.Lines); nil without a clone.
	DocLinks []string `json:"doc_links,omitempty"`
	// Handover is the check-in agent's note on the findings this session
	// starts from, pushed before the conversation began; "" when no
	// check-in ran.
	Handover string `json:"handover,omitempty"`
	// Image is what the session's project image holds; nil when it runs
	// the factory's image.
	Image *imagegen.Manifest `json:"image,omitempty"`
}

// Author standings (ruling 152).
const (
	StandingWriter  = "writer"
	StandingOutside = "outside account"
	// StandingTelemetry: factoryd filed the issue from a SigNoz alert; its
	// values come from the project's rules and its telemetry.
	StandingTelemetry = "telemetry"
)

type EventKind string

// Detail keys factoryd's supervision reads, per kind:
//   - command: command (whole: factoryd decides on it; factoryd's
//     CommandLine cuts it where a model reads it), exit, output (the
//     output's last 4 KiB, whole lines)
//   - tool_call: tool, status, args (long strings capped), args_sha256 (of
//     the full arguments' canonical JSON), error when status is "error"
//     result (the task tool only: the subagent's report, cut at 32 KiB)
//   - file_change: path, sha256 of the content after the change (absent or
//     empty when the file is gone or not a regular file)
//   - command, tool_call and file_change also carry duration_ms (the tool's
//     run time) for the worker's spans, and bytes where known: a command's
//     output length, a file change's size after the change (neither is
//     bytes written); file_change names its tool (write, edit, patch)
//   - checkpoint from the worker: diff {files, insertions, deletions}
//     against the base commit (git diff --shortstat), card_diff, the same
//     numbers against the merge base with main, and patch, the diff text
//     capped; the adapter's own checkpoints (a model step starting)
//     carry no detail
//   - supervisor (factoryd): detector, trip, digest summary, decision,
//     guidance, reason
//   - note (an adapter): bytes, a dropped too-long opencode event line's
//     size, and event and part, its event type and part id where the
//     line's first bytes showed them
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
	EventNote       EventKind = "note"        // an adapter's note on its own stream: a dropped too-long opencode event line
)

// Event is one normalized stream item from a running harness. The git
// diff and the artifact store stay the ground truth.
type Event struct {
	Time   time.Time      `json:"time"`
	Kind   EventKind      `json:"kind"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Result is the adapter's final verdict, or the worker's when its start
// failed before the harness ran: Step names the step that failed, and
// Transient says a later start may not meet its fault (a service the
// sandbox needs that did not answer, as while it restarts).
type Result struct {
	Success   bool   `json:"success"`
	Summary   string `json:"summary"`
	Step      string `json:"step,omitempty"`
	Transient bool   `json:"transient,omitempty"`
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
	// Message puts text into the running session as the next user turn:
	// ErrNotSupported from a harness without such a mode (factoryd then
	// redirects to a fresh session with the text), ErrNotRunning when no
	// session runs.
	Message(ctx context.Context, text string) error
}

// MessagePort is the worker's one listening port: factoryd's POST
// /message, bearer-authenticated per session, into Harness.Message.
const MessagePort = 8091

// MessageTimeout bounds the worker's hand-over of one message to its
// harness; factoryd waits twice as long for the worker's answer, so a slow
// hand-over is reported by the worker, not guessed by factoryd.
const MessageTimeout = 5 * time.Second

var (
	ErrNotSupported = errors.New("the harness takes no message into a running session")
	ErrNotRunning   = errors.New("no session is running")
)
