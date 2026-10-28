package factoryd

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// TripKind names why a session was killed mid-run.
type TripKind string

const (
	TripNoProgress TripKind = "no_progress"
	TripWallClock  TripKind = "wall_clock"
	TripRepetition TripKind = "repetition"
	TripShutdown   TripKind = "shutdown" // not a detector: factoryd is stopping
	// TripAgentEscalation: the agent called escalate_to_human,
	// TripAgentInfoRequest request_info and TripAgentToolRequest
	// request_tool; decided already, no supervisor call
	TripAgentEscalation  TripKind = "agent_escalation"
	TripAgentInfoRequest TripKind = "agent_info_request"
	TripAgentToolRequest TripKind = "agent_tool_request"
	// TripLoop: loopNudges nudges delivered with no new file content in between
	// (NudgeSent), or the provider's refusal of repeated calls
	// (sessionResult); a redirect, no supervisor call
	TripLoop TripKind = "loop"
)

// loopNudges delivered nudges with no new file content in between end
// the session.
const loopNudges = 3

// Trip is a detector firing: the kind, what it saw, and the session's
// findings so far. A wall-clock trip ends the session; every other trip
// goes to the supervisor model.
type Trip struct {
	Kind     TripKind
	Findings []string
	Detail   string // what the detector saw, for the supervisor and the record
	Stuck    string // the stuck rule's line, "" when the rule did not hold
	key      string // which evidence tripped, so a continue resets only that
	decided  *Decision
}

// SupervisorConfig must be validated at task start so a misconfigured
// watchdog fails the task loudly instead of never firing.
type SupervisorConfig struct {
	NoProgressAfter  time.Duration // no events for this long
	WallClockCap     time.Duration // hard runtime cap for the whole task, across sessions
	RepetitionEvents int           // tool calls without a file change before repetition trips
	SameFailure      int           // 0 = DefaultSameFailure
	SameCall         int           // limits.same_call; 0 = policy.DefaultSameCall
	Churn            int           // 0 = DefaultChurn
	// NoWriteAfter: the progress detector (rulings 141, 161); 0 =
	// policy.DefaultDetectors
	NoWriteAfter time.Duration
	// ReadOnly: a report session, which writes no file; repetition and
	// no_write do not apply.
	ReadOnly bool
	// NudgeAfter identical calls in a row earn a nudge, Nudges of them per
	// repetition; the next run trips same_call (limits, 0 = the policy's
	// defaults).
	NudgeAfter, Nudges int
	// CheckIn is the task's resolved check_in (policy.Time); 0 = no
	// check-in.
	CheckIn time.Duration
	// CheapCap: a session on the start rung's first attempt has this long
	// to push before it climbs in place (roadmap 222, item 5); 0 = none.
	CheapCap time.Duration
	// NoPushSpend is the share of the card's budget (0 to under 1) whose
	// spend with nothing pushed trips the supervisor; 0 = off. PriorPush:
	// an earlier session of the card already pushed work.
	NoPushSpend float64
	PriorPush   bool
}

func (c SupervisorConfig) validate() error {
	if c.NoProgressAfter <= 0 {
		return fmt.Errorf("supervisor: NoProgressAfter must be positive, is %s", c.NoProgressAfter)
	}
	if c.WallClockCap <= 0 {
		return fmt.Errorf("supervisor: WallClockCap must be positive, is %s", c.WallClockCap)
	}
	if c.RepetitionEvents < 2 {
		return fmt.Errorf("supervisor: RepetitionEvents must be >= 2, is %d", c.RepetitionEvents)
	}
	if c.SameFailure < 0 || c.SameFailure == 1 || c.SameCall < 0 || c.SameCall == 1 || c.Churn < 0 || c.Churn == 1 ||
		c.NudgeAfter < 0 || c.NudgeAfter == 1 || c.Nudges < 0 {
		return fmt.Errorf("supervisor: detector thresholds must be >= 2 (0 = default): %+v", c)
	}
	if c.NoWriteAfter < 0 {
		return fmt.Errorf("supervisor: NoWriteAfter must be positive (0 = default), is %s", c.NoWriteAfter)
	}
	if c.NoPushSpend < 0 || c.NoPushSpend >= 1 {
		return fmt.Errorf("supervisor: NoPushSpend must be a share from 0 up to under 1, is %v", c.NoPushSpend)
	}
	return nil
}

// Supervisor watches one live session (architecture 6.6). The runner feeds
// every harness event through Event and a ticker through Tick, always
// stamped with factoryd's clock, never the worker's. A trip is sticky
// until Resume (the supervisor model answered continue).
type Supervisor struct {
	cfg      SupervisorConfig
	deadline time.Time
	anchor   time.Time // the task's start: check-ins count from it
	checkAt  time.Time // the next check-in; zero = none
	lastSeen time.Time

	sinceChange int
	traces      *traceDetectors
	findings    []string
	tripped     *Trip
	// messaged: a supervisor's message was delivered and no file written
	// since; the next detector trip is stuck (the stuck rule).
	// messagedOnce: a message was delivered in this session.
	messaged, messagedOnce bool
	// nudges are the nudges delivered since the last new file content;
	// seen is the content the session wrote, by newContent's key
	nudges []Nudge
	seen   map[string]bool

	// no_write's anchor: the last write, or its last continue
	noWriteFrom time.Time
	// started is the session's start; pushed, that it pushed (the cheap cap)
	started time.Time
	pushed  bool

	spendTripped bool

	reads []string // a report session's distinct read paths, in order
}

// NewSupervisor starts watching a session at now; taskStarted anchors the
// wall-clock cap (the agent time earlier sessions used lies before now),
// so resumed sessions share the first session's cap and its check-in times.
func NewSupervisor(cfg SupervisorConfig, taskStarted, now time.Time) (*Supervisor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	s := &Supervisor{cfg: cfg, deadline: taskStarted.Add(cfg.WallClockCap), anchor: taskStarted,
		lastSeen: now, traces: newTraceDetectors(cfg), noWriteFrom: now, seen: map[string]bool{}, started: now}
	s.scheduleCheckIn(now.Add(checkInQuiet - 1))
	return s, nil
}

// checkInQuiet: no check-in in a session's first minutes; the session has
// barely begun.
const checkInQuiet = 10 * time.Minute

// scheduleCheckIn sets the next check-in to the first interval after t.
func (s *Supervisor) scheduleCheckIn(t time.Time) {
	if s.cfg.CheckIn <= 0 {
		return
	}
	s.checkAt = s.anchor.Add((t.Sub(s.anchor)/s.cfg.CheckIn + 1) * s.cfg.CheckIn)
}

func (s *Supervisor) checkIn(now time.Time) *Trip {
	if s.checkAt.IsZero() || now.Before(s.checkAt) {
		return nil
	}
	s.scheduleCheckIn(now)
	return s.trip(&Trip{Kind: TripCheckIn, Detail: "scheduled check-in, no detector tripped"})
}

// Event observes one harness event. A nil return means the session may run on.
func (s *Supervisor) Event(ev harness.Event, at time.Time) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	s.lastSeen = at
	s.noteFinding(ev)
	s.noteRead(ev)
	if ev.Kind == harness.EventPushed {
		s.pushed = true
	}
	subagentCall := (ev.Kind == harness.EventToolCall || ev.Kind == harness.EventCommand) && detailBool(ev, "subagent")
	switch {
	case ev.Kind == harness.EventToolCall, ev.Kind == harness.EventCommand:
		if !subagentCall {
			s.sinceChange++
		}
		if ev.Kind == harness.EventCommand {
			// the command's running time is the machine's, not the agent's
			s.noWriteFrom = s.noWriteFrom.Add(commandDuration(ev))
		}
	case ev.Kind == harness.EventFileChange, s.traces.diffMoved(ev):
		// a file written through a shell shows only as a changed diff
		s.sinceChange = 0
		s.noWriteFrom = at
		s.messaged = false
		if s.newContent(ev) {
			s.nudges = nil
		}
	}
	// every detector sees every event, whichever trips, but a subagent's
	// call: a nudge or trip about it would reach the main agent, which did
	// not make it
	var traced *Trip
	if !subagentCall {
		traced = s.traces.observe(ev)
	}
	if !at.Before(s.deadline) {
		return s.trip(&Trip{Kind: TripWallClock, Detail: "the task's wall-clock cap is reached"})
	}
	if trip := s.cheapCap(at); trip != nil {
		return trip
	}
	if traced != nil {
		return s.trip(traced)
	}
	if !s.cfg.ReadOnly && s.sinceChange >= s.cfg.RepetitionEvents {
		return s.trip(&Trip{Kind: TripRepetition, Detail: fmt.Sprintf("%d tool calls and commands without a file change", s.sinceChange)})
	}
	agent := ev.Kind == harness.EventToolCall || ev.Kind == harness.EventCommand || ev.Kind == harness.EventModelCall
	if agent && !s.cfg.ReadOnly && at.Sub(s.noWriteFrom) >= s.cfg.NoWriteAfter {
		return s.trip(&Trip{Kind: TripNoWrite, Detail: fmt.Sprintf("agent events for %s without a file write or edit", at.Sub(s.noWriteFrom).Round(time.Second))})
	}
	return s.checkIn(at)
}

// newContent: the write ev records content the session has not written
// before (ruling 280): a file change by its sha256, a moved shell diff by
// its numbers and patch. One without a hash (a file over maxHashedFile, a
// deleted file) counts as new.
func (s *Supervisor) newContent(ev harness.Event) bool {
	key := detailString(ev, "sha256")
	if ev.Kind == harness.EventCheckpoint {
		sum := sha256.Sum256([]byte(fmt.Sprint(ev.Detail["diff"]) + "\n" + detailString(ev, "patch")))
		key = "diff\x00" + hex.EncodeToString(sum[:])
	}
	if key == "" {
		return true
	}
	if s.seen[key] {
		return false
	}
	s.seen[key] = true
	return true
}

// commandDuration is how long a command event's command ran (duration_ms).
func commandDuration(ev harness.Event) time.Duration {
	ms, _ := ev.Detail["duration_ms"].(float64)
	return time.Duration(ms) * time.Millisecond
}

// Tick is the heartbeat check between (or instead of) events: it catches a
// harness that went silent, the check-in and the wall-clock cap.
func (s *Supervisor) Tick(now time.Time) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	if !now.Before(s.deadline) {
		return s.trip(&Trip{Kind: TripWallClock, Detail: "the task's wall-clock cap is reached"})
	}
	if trip := s.cheapCap(now); trip != nil {
		return trip
	}
	if trip := s.checkIn(now); trip != nil {
		return trip
	}
	if silent := now.Sub(s.lastSeen); silent >= s.cfg.NoProgressAfter {
		return s.trip(&Trip{Kind: TripNoProgress, Detail: fmt.Sprintf("no harness events for %s", silent.Round(time.Second))})
	}
	return nil
}

// cheapCap trips once the start rung's first attempt ran CheapCap without
// a push: the decision is the climb, no supervisor call.
func (s *Supervisor) cheapCap(at time.Time) *Trip {
	if s.cfg.CheapCap <= 0 || s.pushed || at.Sub(s.started) < s.cfg.CheapCap {
		return nil
	}
	detail := fmt.Sprintf("the cheap rung's %s passed without a push", s.cfg.CheapCap)
	return s.trip(&Trip{Kind: TripCheapCap, Detail: detail, decided: &Decision{Action: ActionRetryStronger, Reason: detail}})
}

// Spent trips once per session when the card has spent its share of the
// budget with nothing pushed (roadmap 199): a session that reads and
// edits for hours without a push is what the budget pays for. A report
// session pushes nothing and is warned instead.
func (s *Supervisor) Spent(spent, budget float64) *Trip {
	if s.tripped != nil || s.spendTripped || s.cfg.NoPushSpend <= 0 || s.cfg.ReadOnly || s.cfg.PriorPush || s.pushed ||
		budget <= 0 || spent < s.cfg.NoPushSpend*budget {
		return nil
	}
	s.spendTripped = true
	return s.trip(&Trip{Kind: TripSpendNoPush, Detail: fmt.Sprintf("%.2f of the card's %.2f EUR are spent (%.0f percent, the line is %.0f) and no work is pushed",
		spent, budget, 100*spent/budget, 100*s.cfg.NoPushSpend)})
}

// Climbed: the session switched to the stronger rung in place; the cheap
// cap no longer applies.
func (s *Supervisor) Climbed() { s.cfg.CheapCap = 0 }

// AgentAsks opens an agent escalation's reason, before its question;
// agentAsksAuthor an information request's.
const (
	AgentAsks       = "The agent asks: "
	agentAsksAuthor = "The agent asks the issue's author: "
)

// AgentAsk trips on the agent's own escalate_to_human, request_info or
// request_tool: an escalation with its question, the information request,
// or the tool request with its tool and need, the supervisor model not
// asked.
func (s *Supervisor) AgentAsk(a AgentAsk) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	kind, detail := TripAgentEscalation, "the agent called escalate_to_human"
	switch a.Kind {
	case AskInfo:
		kind, detail = TripAgentInfoRequest, "the agent called request_info"
	case AskTool:
		kind, detail = TripAgentToolRequest, "the agent called request_tool"
	}
	return s.trip(&Trip{Kind: kind, Detail: detail, decided: &Decision{Action: ActionEscalate, Reason: a.reason(), Choices: a.Choices, Tool: a.Tool, Need: a.Need}})
}

// TakeNudge is the nudge the last event earned, once.
func (s *Supervisor) TakeNudge() *Nudge {
	n := s.traces.nudge
	s.traces.nudge = nil
	return n
}

// NudgeSent counts n against its repetition's nudges: only a nudge the
// session took is spent. The loopNudges-th since the last new file
// content trips loop; a report session writes no file, so its nudges never do.
func (s *Supervisor) NudgeSent(n Nudge) *Trip {
	s.traces.nudged[n.key]++
	if s.cfg.ReadOnly {
		return nil
	}
	if s.nudges = append(s.nudges, n); len(s.nudges) < loopNudges {
		return nil
	}
	calls := make([]string, len(s.nudges))
	for i, n := range s.nudges {
		calls[i] = fmt.Sprintf("%s `%s` %d times", n.Tool, textutil.Truncate(n.Call, 200), n.Count)
	}
	return s.trip(&Trip{Kind: TripLoop, Detail: countWords[loopNudges] + " nudges without new file content in between: " + strings.Join(calls, "; "),
		decided: &Decision{Action: ActionRedirect}})
}

// nudgeUndelivered trips on a nudge that did not reach the session: a
// harness without messages gets the redirect with the nudge's text, decided
// already; any other failure asks the supervisor.
func (s *Supervisor) nudgeUndelivered(n Nudge, err error) *Trip {
	t := &Trip{Kind: TripSameCall, key: n.key, Detail: fmt.Sprintf("%s: the same call %d times in a row; the nudge was not delivered: %v", n.Tool, n.Count, err)}
	if errors.Is(err, harness.ErrNotSupported) {
		t.decided = &Decision{Action: ActionRedirect, Guidance: n.Text()}
	}
	return s.trip(t)
}

// Resume clears the trip after the supervisor answered continue and resets
// the detector that tripped; the time the supervisor took is not silence.
func (s *Supervisor) Resume(now time.Time) {
	switch s.tripped.Kind {
	case TripRepetition:
		s.sinceChange = 0
	case TripNoWrite:
		s.noWriteFrom = now
	default:
		s.traces.reset(s.tripped)
	}
	s.lastSeen = now
	s.tripped = nil
}

// Messaged resumes the session after the supervisor's message reached it
// and arms the stuck rule until the next write. The command detectors
// start afresh at the message; the alternation memory (contents, churn)
// does not, so a circle of writes that spans messages still trips
// flip_flop or churn, stuck (ruling 224: circles of writes are not
// progress; round 2's drill 4 looped for 38 minutes on the cleared
// memory).
func (s *Supervisor) Messaged(now time.Time) {
	s.Resume(now)
	s.sinceChange = 0
	s.noWriteFrom = now
	d := s.traces
	clear(d.runs)
	clear(d.failed)
	clear(d.calls)
	d.streakKey, d.streak = "", 0
	s.messaged, s.messagedOnce = true, true
}

// stuck is the stuck rule: after a delivered message, a detector trip with
// no write since, or a circle of writes (flip_flop, churn; ruling 224).
// Silence (no_progress: a long command) and the clock are no evidence.
func (s *Supervisor) stuck(t *Trip) bool {
	switch t.Kind {
	case TripCheckIn, TripWallClock, TripNoProgress, TripSpendNoPush:
		return false
	case TripFlipFlop, TripChurn:
		return s.messagedOnce
	}
	return s.messaged
}

func (s *Supervisor) trip(t *Trip) *Trip {
	s.traces.nudge = nil // the trip decides; a nudge earned with it is not sent
	t.Findings = append([]string(nil), s.findings...)
	if t.decided == nil && s.stuck(t) {
		stuck := fmt.Sprintf("stuck after the supervisor's message: %s: %s", t.Kind, t.Detail)
		t.Findings, t.Stuck = append(t.Findings, stuck), stuck
	}
	s.tripped = t
	return t
}

// noteFinding distills the event stream into the compact lines a resumed
// session (or a post-mortem) reads: what was changed and what was run.
func (s *Supervisor) noteFinding(ev harness.Event) {
	switch ev.Kind {
	case harness.EventFileChange:
		if path, ok := ev.Detail["path"].(string); ok {
			s.findings = append(s.findings, "changed "+path)
		}
	case harness.EventCommand:
		line := "ran command"
		if cmd, ok := ev.Detail["command"].(string); ok {
			line = "ran `" + CommandLine(cmd) + "`"
		}
		if exit, ok := ev.Detail["exit"]; ok {
			line += " (exit " + fmt.Sprint(exit) + ")"
		}
		s.findings = append(s.findings, line)
	case harness.EventToolCall:
		if status, _ := ev.Detail["status"].(string); status == "error" {
			s.findings = append(s.findings, "tool error: "+fmt.Sprint(ev.Detail["tool"])+
				" "+strconv.Quote(fmt.Sprint(ev.Detail["error"])))
		}
	}
	if len(s.findings) > 20 {
		s.findings = append(s.findings[:0], s.findings[len(s.findings)-20:]...)
	}
}

// maxReads bounds the paths a report session keeps.
const maxReads = 100

// noteRead keeps the paths a report session read, for the next planner
// session when this one ends without its plan.
func (s *Supervisor) noteRead(ev harness.Event) {
	if !s.cfg.ReadOnly || ev.Kind != harness.EventToolCall || detailString(ev, "tool") != "read" || detailString(ev, "status") != "completed" {
		return
	}
	args, _ := ev.Detail["args"].(map[string]any)
	path, _ := args["filePath"].(string)
	if path == "" || slices.Contains(s.reads, path) || len(s.reads) >= maxReads {
		return
	}
	s.reads = append(s.reads, path)
}

func (s *Supervisor) Reads() []string { return slices.Clone(s.reads) }

// Trace detectors (architecture 6.6): cheap checks over the event stream
// for the trace a circling agent leaves. A trip of any of them, like a
// stall or reading without writing, calls the supervisor model.
const (
	TripSameFailure TripKind = "same_failure"
	TripSameCall    TripKind = "same_call"
	TripFlipFlop    TripKind = "flip_flop"
	TripChurn       TripKind = "churn"
	// TripCheckIn: no detector, the scheduled check-in (SupervisorConfig.CheckIn)
	TripCheckIn TripKind = "check_in"
	// progress detectors (ruling 141), their waits in the policy
	TripNoWrite TripKind = "no_write"
	// TripCheapCap: the start rung's first attempt ran its cap without a
	// push (roadmap 222, item 5)
	TripCheapCap TripKind = "cheap_cap"
	// TripSpendNoPush: the card spent its policy share of the budget and nothing is pushed
	TripSpendNoPush TripKind = "spend_no_push"
)

// Detector defaults; each has an env override in cmd/factoryd.
const (
	DefaultSameFailure = 3  // same command, same error signature, in a row
	DefaultChurn       = 15 // file changes since the diff last grew, no newly passing command
)

// withDefaults fills unset detector thresholds.
func (c SupervisorConfig) withDefaults() SupervisorConfig {
	if c.SameFailure == 0 {
		c.SameFailure = DefaultSameFailure
	}
	c.SameCall = policy.Limits{SameCall: c.SameCall}.SameCallAfter()
	if c.Churn == 0 {
		c.Churn = DefaultChurn
	}
	if c.NoWriteAfter == 0 {
		c.NoWriteAfter = policy.Document{}.Detector(policy.DetectorNoWrite)
	}
	after, nudges := policy.Limits{NudgeAfter: c.NudgeAfter, Nudges: c.Nudges}.Nudge()
	c.NudgeAfter, c.Nudges = after, nudges
	return c
}

// outputRun is a command's current run of identical output.
type outputRun struct {
	sig     string
	n       int
	changed bool // a file changed since the run's last occurrence
}

// traceDetectors holds the per-session state of the trace detectors.
type traceDetectors struct {
	cfg      SupervisorConfig
	runs     map[string]outputRun // by command
	failed   map[string]bool      // commands whose last run failed
	lastDiff string               // the last checkpoint's diff numbers
	calls    map[string]int       // by tool + argument hash
	contents map[string][]string  // by path: content hashes in the order they appeared
	churn    int                  // file changes since the diff last grew
	maxDiff  int                  // largest diff (insertions + deletions) seen
	// pending are the paths that returned to a content they had before,
	// each with the commands that were failing when it happened: the
	// flip-flop trips when one of those fails again after the return
	// (roadmap 184); a trial edit undone while the checks pass never does.
	pending map[string][]string

	streakKey string         // the last call: tool and input hash
	streak    int            // identical calls in a row ending with it
	nudged    map[string]int // nudges sent, by call
	nudge     *Nudge         // the nudge the last event earned, until taken
}

func newTraceDetectors(cfg SupervisorConfig) *traceDetectors {
	return &traceDetectors{cfg: cfg, runs: map[string]outputRun{}, failed: map[string]bool{},
		calls: map[string]int{}, contents: map[string][]string{}, pending: map[string][]string{}, nudged: map[string]int{}}
}

// Nudge asks the running session to change approach after Count identical
// calls of Tool in a row (queue item C): a message into the session, not a
// trip; it comes before the trip once per repetition.
type Nudge struct {
	Tool  string
	Call  string // callText
	Count int
	key   string
}

var countWords = strings.Fields("zero once twice three four five six seven eight nine")

// Text is the message the session gets.
func (n Nudge) Text() string {
	times := fmt.Sprintf("%d times", n.Count)
	if n.Count > 2 && n.Count < len(countWords) {
		times = countWords[n.Count] + " times"
	}
	return fmt.Sprintf("You repeated the same call %s (%s). Change approach, then continue.", times, n.Tool)
}

// observe feeds one event to every trace detector and returns the first
// trip; a nudge the event earns waits in d.nudge until the next event. A
// repeated call, in a row (NudgeAfter) or across the session (SameCall
// tool calls), gets its nudges before it trips same_call.
func (d *traceDetectors) observe(ev harness.Event) *Trip {
	d.nudge = nil
	trip := d.detect(ev)
	tool, key := callKey(ev)
	if key == "" {
		return trip
	}
	if key != d.streakKey {
		d.streakKey, d.streak = key, 0
	}
	d.streak++
	inRow, n := d.streak >= d.cfg.NudgeAfter, d.calls[key]
	if trip != nil || (!inRow && n < d.cfg.SameCall) {
		return trip
	}
	count := d.streak
	if !inRow {
		count = n
	}
	d.streak = 0
	if d.nudged[key] < d.cfg.Nudges {
		delete(d.calls, key)
		d.nudge = &Nudge{Tool: tool, Call: callText(ev), Count: count, key: key}
		return nil
	}
	nudges := textutil.Plural(d.nudged[key], "nudge", "nudges")
	if inRow {
		return &Trip{Kind: TripSameCall, key: key, Detail: fmt.Sprintf("%s: the same call %d times in a row after %s", tool, count, nudges)}
	}
	return &Trip{Kind: TripSameCall, key: key,
		Detail: fmt.Sprintf("tool %s called %d times with the same arguments %s after %s", tool, count, textutil.Truncate(callText(ev), 300), nudges)}
}

// callText is a call as a person reads it: a command's line, a file
// change's path, a tool call's arguments.
func callText(ev harness.Event) string {
	switch ev.Kind {
	case harness.EventCommand:
		return detailString(ev, "command")
	case harness.EventFileChange:
		return detailString(ev, "path")
	}
	args, _ := json.Marshal(ev.Detail["args"])
	return string(args)
}

// callKey names a call by its tool and input: a tool call's arguments, a
// command's line (the bash tool), a file change's path and content. Other
// events are no call.
func callKey(ev harness.Event) (tool, key string) {
	switch ev.Kind {
	case harness.EventToolCall:
		tool = detailString(ev, "tool")
		h := detailString(ev, "args_sha256")
		if h == "" {
			h = argsHash(ev.Detail["args"])
		}
		return tool, tool + "\x00" + h
	case harness.EventCommand:
		return "bash", "bash\x00" + argsHash(map[string]any{"command": detailString(ev, "command")})
	case harness.EventFileChange:
		tool = cmp.Or(detailString(ev, "tool"), "write")
		return tool, tool + "\x00" + detailString(ev, "path") + "\x00" + detailString(ev, "sha256")
	}
	return "", ""
}

func (d *traceDetectors) detect(ev harness.Event) *Trip {
	switch ev.Kind {
	case harness.EventCommand:
		return d.command(ev)
	case harness.EventToolCall:
		return d.toolCall(ev)
	case harness.EventFileChange:
		return d.fileChange(ev)
	case harness.EventCheckpoint:
		if size, ok := diffSize(ev); ok {
			if size > d.maxDiff {
				d.maxDiff, d.churn = size, 0
			}
			if d.diffMoved(ev) {
				d.changed()
			}
			d.lastDiff = fmt.Sprint(ev.Detail["diff"])
		}
	}
	return nil
}

// diffMoved: ev is a checkpoint whose diff differs from the last one's,
// the trace of edits made through a shell. The first checkpoint is the
// baseline.
func (d *traceDetectors) diffMoved(ev harness.Event) bool {
	if ev.Kind != harness.EventCheckpoint {
		return false
	}
	_, ok := diffSize(ev)
	return ok && d.lastDiff != "" && fmt.Sprint(ev.Detail["diff"]) != d.lastDiff
}

// CommandLine is a bash command as a model reads it (roadmap 201), the
// supervisor or the project agent's chat: like a tool's string argument
// its first maxArgString bytes, and when cut a note of its size and hash,
// so two long commands with one start read as two. Wherever factoryd
// decides on a command (the detectors, the acceptance runs) it reads the
// whole one the event carries.
func CommandLine(cmd string) string {
	if len(cmd) <= maxArgString {
		return cmd
	}
	sum := sha256.Sum256([]byte(cmd))
	return fmt.Sprintf("%s (%d bytes, sha256 %x)", textutil.Truncate(cmd, maxArgString), len(cmd), sum[:8])
}

// masksExitStatus: the command's exit code need not be its test's.
func masksExitStatus(cmd string) bool {
	return strings.ContainsAny(cmd, ";|&\n") || strings.Contains(cmd, "$?")
}

// changed notes a file change for every command's output run.
func (d *traceDetectors) changed() {
	for cmd, run := range d.runs {
		run.changed = true
		d.runs[cmd] = run
	}
}

// command runs the same-failure detector: the circle "edit, test, same
// failure". A run is the same command giving the same output signature
// (exit code folded in) with a file change before each repeat; a rerun
// with nothing changed and a command without output are no evidence. A
// pass ends the run, unless the command masks its own exit status (`;`,
// `|`, `||`, `&`, `&&`, a second line, `$?`, as in `cmd; echo $?`): then
// only its output tells.
func (d *traceDetectors) command(ev harness.Event) *Trip {
	cmd := detailString(ev, "command")
	exit, hasExit := detailInt(ev, "exit")
	if hasExit && exit == 0 {
		if d.failed[cmd] {
			d.churn = 0 // newly passing
		}
		delete(d.failed, cmd)
		d.disarm(cmd)
		if !masksExitStatus(cmd) {
			delete(d.runs, cmd)
			return nil
		}
	} else if hasExit {
		if d.failed[cmd] {
			if trip := d.refail(cmd); trip != nil {
				return trip
			}
		}
		d.failed[cmd] = true
	}
	out := detailString(ev, "output")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	// a git refusal the sandbox's identity cannot pass: no edit can fix it,
	// so every refusal counts toward one shared run whatever the command
	// (a supply-blocked session varies its probes) and no file change is
	// asked between them. An indented log line — code under test printing
	// git's words — is no refusal.
	if hasExit && exit != 0 && gitAuthRefusal.MatchString(out) {
		run := d.runs[gitRefusalKey]
		run.n++
		d.runs[gitRefusalKey] = run
		if run.n >= d.cfg.SameFailure {
			return &Trip{Kind: TripSameFailure, key: gitRefusalKey,
				Detail: fmt.Sprintf("%d git commands were refused authentication (last: `%s`): the sandbox holds no git credential, so no edit can fix this — a card that carries an existing pull request names it on its issue (Carries line), which puts its head in the clone", run.n, CommandLine(cmd))}
		}
		return nil
	}
	code := "?"
	if hasExit {
		code = strconv.Itoa(exit)
	}
	sig := ErrorSignature(cmd, "exit "+code+"\n"+out)
	run := d.runs[cmd]
	switch {
	case run.sig != sig:
		run = outputRun{sig: sig, n: 1}
	case run.changed:
		run.n++
	default:
		return nil
	}
	run.changed = false
	d.runs[cmd] = run
	if run.n >= d.cfg.SameFailure {
		return &Trip{Kind: TripSameFailure, key: cmd,
			Detail: fmt.Sprintf("`%s` gave the same output %d times, with file changes in between (exit %s, signature %s)", CommandLine(cmd), run.n, code, sig)}
	}
	return nil
}

// toolCall counts the call for same_call, which observe judges with the
// nudge.
func (d *traceDetectors) toolCall(ev harness.Event) *Trip {
	_, key := callKey(ev)
	d.calls[key]++
	return nil
}

// refail is the flip-flop's second half: a command fails again after a
// path returned to a content it had before, and it was failing when the
// path returned. The paths whose return it completes leave the pending
// set with the trip.
func (d *traceDetectors) refail(cmd string) *Trip {
	var paths []string
	for p, cmds := range d.pending {
		if slices.Contains(cmds, cmd) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	slices.Sort(paths)
	clear(d.pending)
	return &Trip{Kind: TripFlipFlop, key: paths[0],
		Detail: fmt.Sprintf("after %s returned to a content it had before, `%s` failed again, unchanged", strings.Join(paths, ", "), CommandLine(cmd))}
}

// disarm drops cmd from the pending returns: a check that failed before a
// file's return and now passes proves the undo held.
func (d *traceDetectors) disarm(cmd string) {
	for p, cmds := range d.pending {
		if i := slices.Index(cmds, cmd); i >= 0 {
			if cmds = slices.Delete(cmds, i, i+1); len(cmds) == 0 {
				delete(d.pending, p)
			} else {
				d.pending[p] = cmds
			}
		}
	}
}

func (d *traceDetectors) fileChange(ev harness.Event) *Trip {
	path, h := detailString(ev, "path"), detailString(ev, "sha256")
	d.churn++
	d.changed()
	var trip *Trip
	if h != "" {
		seen := d.contents[path]
		switch {
		case len(seen) > 0 && seen[len(seen)-1] == h:
		case slices.Contains(seen, h):
			var cmds []string
			for c := range d.failed {
				cmds = append(cmds, c)
			}
			slices.Sort(cmds)
			d.pending[path] = cmds
			d.contents[path] = append(seen, h)
		default:
			d.contents[path] = append(seen, h)
		}
	}
	if trip == nil && d.churn >= d.cfg.Churn {
		trip = &Trip{Kind: TripChurn,
			Detail: fmt.Sprintf("%d file changes since the diff against the base last grew (largest: %d lines), and no command newly passes", d.churn, d.maxDiff)}
	}
	return trip
}

// reset clears the tripped detector after a supervisor's continue, so the
// same evidence does not trip it again at once; the others keep their state.
func (d *traceDetectors) reset(trip *Trip) {
	switch trip.Kind {
	case TripSameFailure:
		delete(d.runs, trip.key)
	case TripSameCall:
		delete(d.calls, trip.key)
	case TripFlipFlop:
		if seen := d.contents[trip.key]; len(seen) > 0 {
			d.contents[trip.key] = seen[len(seen)-1:]
		}
		clear(d.pending)
	case TripChurn:
		d.churn = 0
	}
}

// ErrorSignature names a failure independent of when and where it ran:
// the command plus the normalised tail of its output.
func ErrorSignature(command, output string) string {
	sum := sha256.Sum256([]byte(command + "\n" + normaliseOutput(output)))
	return hex.EncodeToString(sum[:8])
}

// signatureLines is how much of an output's end the signature reads; the
// adapter's tail is cut by bytes, so its first lines vary with the
// length of what came before.
const signatureLines = 20

// gitAuthRefusal is a git command's own fatal line of a refusal the
// sandbox's identity cannot pass: authentication failed, no credential to
// offer, or the server's 401 or 403. It begins the line, so a test's log
// that embeds the words indented is no refusal.
var gitAuthRefusal = regexp.MustCompile(`(?m)^fatal: .*(?:Authentication failed|could not read Username|returned error: 40[13])`)

// gitRefusalKey keys the run every git auth refusal shares.
const gitRefusalKey = "git auth refusal"

var volatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "<ts>"},
	{regexp.MustCompile(`\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun) (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) +\d{1,2} \d{2}:\d{2}:\d{2}(?: [A-Z]{2,5})? \d{4}\b`), "<ts>"},
	{regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?\b`), "<ts>"},
	{regexp.MustCompile(`(\.[A-Za-z0-9]+):\d+(?::\d+)?`), "$1:N"},
	{regexp.MustCompile(`(?:/private)?/(?:tmp|var/tmp|var/folders)/[^\s"':]*`), "<tmp>"},
	{regexp.MustCompile(`0x[0-9a-fA-F]+`), "0x?"},
	{regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|µs|us|ms|h|m|s))+\b`), "<dur>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)? ?(?:seconds?|secs?|minutes?|mins?)\b`), "<dur>"},
	{regexp.MustCompile(`\b(line|column|col|pid) \d+`), "$1 N"},
}

// normaliseOutput strips what differs between two runs of the same failure:
// timestamps, hex addresses, durations, temp paths, line and column numbers
// and pids, over the output's last signatureLines non-empty lines.
func normaliseOutput(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > signatureLines {
		lines = lines[len(lines)-signatureLines:]
	}
	s := strings.Join(lines, "\n")
	for _, v := range volatile {
		s = v.re.ReplaceAllString(s, v.with)
	}
	return s
}

// argsHash is the sha256 of the canonical JSON of a tool's arguments
// (encoding/json sorts map keys).
func argsHash(args any) string {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func diffSize(ev harness.Event) (int, bool) {
	diff, ok := ev.Detail["diff"].(map[string]any)
	if !ok {
		return 0, false
	}
	ins, ok1 := toInt(diff["insertions"])
	del, ok2 := toInt(diff["deletions"])
	return ins + del, ok1 && ok2
}

func detailString(ev harness.Event, key string) string {
	s, _ := ev.Detail[key].(string)
	return s
}

func detailBool(ev harness.Event, key string) bool {
	b, _ := ev.Detail[key].(bool)
	return b
}

func detailInt(ev harness.Event, key string) (int, bool) {
	return toInt(ev.Detail[key])
}

// toInt reads a number from an event detail: float64 after the JSON trip
// through the pod log, int when built in process.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}
