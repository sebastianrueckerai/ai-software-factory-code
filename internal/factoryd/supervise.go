package factoryd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// TripKind names why a session was killed mid-run.
type TripKind string

const (
	TripNoProgress TripKind = "no_progress"
	TripWallClock  TripKind = "wall_clock"
	TripRepetition TripKind = "repetition"
	TripShutdown   TripKind = "shutdown" // not a detector: factoryd is stopping
	// TripAgentEscalation: the agent called escalate_to_human; decided
	// already, no supervisor call
	TripAgentEscalation TripKind = "agent_escalation"
)

// Trip is a detector firing: the kind, what it saw, and the session's
// findings so far. A wall-clock trip ends the session; every other trip
// goes to the supervisor model.
type Trip struct {
	Kind     TripKind
	Findings []string
	Detail   string // what the detector saw, for the supervisor and the record
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
	SameCall         int           // 0 = DefaultSameCall
	Churn            int           // 0 = DefaultChurn
	MaxCalls         int           // 0 = DefaultMaxCalls
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
	if c.SameFailure < 0 || c.SameFailure == 1 || c.SameCall < 0 || c.SameCall == 1 || c.Churn < 0 || c.Churn == 1 || c.MaxCalls < 0 {
		return fmt.Errorf("supervisor: detector thresholds must be >= 2 (0 = default), calls >= 0: %+v", c)
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
	halfAt   time.Time
	lastSeen time.Time

	sinceChange int
	traces      *traceDetectors
	halfWayDone bool    // fires once per task: the workflow carries it across sessions
	spent       float64 // the task's spend as a fraction of its budget
	findings    []string
	tripped     *Trip
}

// NewSupervisor starts watching a session at now; taskStarted anchors the
// wall-clock cap (the agent time earlier sessions used lies before now),
// so resumed sessions share the first session's cap and its half-way point.
func NewSupervisor(cfg SupervisorConfig, taskStarted, now time.Time) (*Supervisor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	return &Supervisor{cfg: cfg, deadline: taskStarted.Add(cfg.WallClockCap), halfAt: taskStarted.Add(cfg.WallClockCap / 2),
		lastSeen: now, traces: newTraceDetectors(cfg)}, nil
}

// Event observes one harness event. A nil return means the session may run on.
func (s *Supervisor) Event(ev harness.Event, at time.Time) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	s.lastSeen = at
	s.noteFinding(ev)
	switch ev.Kind {
	case harness.EventToolCall, harness.EventCommand:
		s.sinceChange++
	case harness.EventFileChange:
		s.sinceChange = 0
	}
	// every detector sees every event, whichever trips
	traced := s.traces.observe(ev)
	if !at.Before(s.deadline) {
		return s.trip(&Trip{Kind: TripWallClock, Detail: "the task's wall-clock cap is reached"})
	}
	if traced != nil {
		return s.trip(traced)
	}
	if s.sinceChange >= s.cfg.RepetitionEvents {
		return s.trip(&Trip{Kind: TripRepetition, Detail: fmt.Sprintf("%d tool calls and commands without a file change", s.sinceChange)})
	}
	return s.halfWay(at)
}

// Tick is the heartbeat check between (or instead of) events: it catches a
// harness that went silent, the half-way point and the wall-clock cap.
func (s *Supervisor) Tick(now time.Time) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	if !now.Before(s.deadline) {
		return s.trip(&Trip{Kind: TripWallClock, Detail: "the task's wall-clock cap is reached"})
	}
	if trip := s.halfWay(now); trip != nil {
		return trip
	}
	if silent := now.Sub(s.lastSeen); silent >= s.cfg.NoProgressAfter {
		return s.trip(&Trip{Kind: TripNoProgress, Detail: fmt.Sprintf("no harness events for %s", silent.Round(time.Second))})
	}
	return nil
}

// agentAsks opens an agent escalation's reason, before its question.
const agentAsks = "The agent asks: "

// AgentEscalation trips on the agent's own escalate_to_human: an
// escalation with its question, the supervisor model not asked.
func (s *Supervisor) AgentEscalation(question, reason string) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	return s.trip(&Trip{Kind: TripAgentEscalation, Detail: "the agent called escalate_to_human",
		decided: &Decision{Action: ActionEscalate, Reason: agentAsks + question + "\nWhy: " + reason}})
}

// SetSpend tells the supervisor the task's spend so far.
func (s *Supervisor) SetSpend(spentEUR, budgetEUR float64) {
	if budgetEUR > 0 {
		s.spent = spentEUR / budgetEUR
	}
}

func (s *Supervisor) halfWay(now time.Time) *Trip {
	if s.halfWayDone {
		return nil
	}
	var detail string
	switch {
	case !now.Before(s.halfAt):
		detail = fmt.Sprintf("the task passed half its wall-clock cap of %s", s.cfg.WallClockCap)
	case s.spent >= 0.5:
		detail = fmt.Sprintf("the task spent %.0f%% of its budget", s.spent*100)
	default:
		return nil
	}
	s.halfWayDone = true
	return s.trip(&Trip{Kind: TripHalfWay, Detail: detail})
}

// HalfWayDone reports whether the half-way check-in has fired for the task.
func (s *Supervisor) HalfWayDone() bool { return s.halfWayDone }

// Resume clears the trip after the supervisor answered continue and resets
// the detector that tripped; the time the supervisor took is not silence.
func (s *Supervisor) Resume(now time.Time) {
	switch s.tripped.Kind {
	case TripRepetition:
		s.sinceChange = 0
	default:
		s.traces.reset(s.tripped)
	}
	s.lastSeen = now
	s.tripped = nil
}

// Findings is a copy of what the session has done so far.
func (s *Supervisor) Findings() []string {
	return append([]string(nil), s.findings...)
}

func (s *Supervisor) trip(t *Trip) *Trip {
	t.Findings = append([]string(nil), s.findings...)
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
			line = "ran `" + cmd + "`"
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

// Trace detectors (architecture 6.6): cheap checks over the event stream
// for the trace a circling agent leaves. A trip of any of them, like a
// stall or reading without writing, calls the supervisor model.
const (
	TripSameFailure TripKind = "same_failure"
	TripSameCall    TripKind = "same_call"
	TripFlipFlop    TripKind = "flip_flop"
	TripChurn       TripKind = "churn"
	TripHalfWay     TripKind = "half_way"
)

// Detector defaults; each has an env override in cmd/factoryd.
const (
	DefaultSameFailure = 3  // same command, same error signature, in a row
	DefaultSameCall    = 4  // same tool, same arguments, per session
	DefaultChurn       = 15 // file changes since the diff last grew, no newly passing command
	DefaultMaxCalls    = 3  // supervisor calls per session; the next trip escalates without one
)

// withDefaults fills unset detector thresholds.
func (c SupervisorConfig) withDefaults() SupervisorConfig {
	if c.SameFailure == 0 {
		c.SameFailure = DefaultSameFailure
	}
	if c.SameCall == 0 {
		c.SameCall = DefaultSameCall
	}
	if c.Churn == 0 {
		c.Churn = DefaultChurn
	}
	if c.MaxCalls == 0 {
		c.MaxCalls = DefaultMaxCalls
	}
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
}

func newTraceDetectors(cfg SupervisorConfig) *traceDetectors {
	return &traceDetectors{cfg: cfg, runs: map[string]outputRun{}, failed: map[string]bool{},
		calls: map[string]int{}, contents: map[string][]string{}}
}

// observe feeds one event to every trace detector and returns the first trip.
func (d *traceDetectors) observe(ev harness.Event) *Trip {
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
			// edits made through a shell show only here; the first
			// checkpoint is the baseline
			if key := fmt.Sprint(ev.Detail["diff"]); key != d.lastDiff {
				if d.lastDiff != "" {
					d.changed()
				}
				d.lastDiff = key
			}
		}
	}
	return nil
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
		if !masksExitStatus(cmd) {
			delete(d.runs, cmd)
			return nil
		}
	} else if hasExit {
		d.failed[cmd] = true
	}
	out := detailString(ev, "output")
	if strings.TrimSpace(out) == "" {
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
			Detail: fmt.Sprintf("`%s` gave the same output %d times, with file changes in between (exit %s, signature %s)", cmd, run.n, code, sig)}
	}
	return nil
}

func (d *traceDetectors) toolCall(ev harness.Event) *Trip {
	tool := detailString(ev, "tool")
	h := detailString(ev, "args_sha256")
	if h == "" {
		h = argsHash(ev.Detail["args"])
	}
	key := tool + "\x00" + h
	d.calls[key]++
	if n := d.calls[key]; n >= d.cfg.SameCall {
		args, _ := json.Marshal(ev.Detail["args"])
		return &Trip{Kind: TripSameCall, key: key,
			Detail: fmt.Sprintf("tool %s called %d times with the same arguments %s", tool, n, textutil.Truncate(string(args), 300))}
	}
	return nil
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
			trip = &Trip{Kind: TripFlipFlop, key: path,
				Detail: fmt.Sprintf("%s returned to a content it had before, after other content in between", path)}
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
