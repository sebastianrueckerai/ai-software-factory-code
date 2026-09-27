package factoryd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// TripKind names why a session was killed mid-run.
type TripKind string

const (
	TripNoProgress TripKind = "no_progress"
	TripWallClock  TripKind = "wall_clock"
	TripRepetition TripKind = "repetition"
)

// Trip is the supervisor's verdict: kill the session, snapshot findings, and
// either resume as a new pod (carrying Findings and the parent session id)
// or retry with a stronger model.
type Trip struct {
	Kind               TripKind
	Findings           []string
	RetryStrongerModel bool
}

// SupervisorConfig must be validated at task start so a misconfigured
// watchdog fails the task loudly instead of never firing.
type SupervisorConfig struct {
	NoProgressAfter  time.Duration // no events for this long
	WallClockCap     time.Duration // hard runtime cap for the whole task, across sessions
	RepetitionEvents int           // tool calls without a file change before repetition trips
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
	return nil
}

// Supervisor watches one live session (architecture: session supervision).
// The runner feeds every harness event through Event and a ticker through
// Tick, always stamped with factoryd's clock, never the worker's; the first
// trip is sticky and becomes the kill decision.
type Supervisor struct {
	cfg      SupervisorConfig
	deadline time.Time
	lastSeen time.Time

	sinceChange int
	findings    []string
	tripped     *Trip
}

// NewSupervisor starts watching a session at now; taskStarted anchors the
// wall-clock cap, so resumed sessions share the first session's budget.
func NewSupervisor(cfg SupervisorConfig, taskStarted, now time.Time) (*Supervisor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Supervisor{cfg: cfg, deadline: taskStarted.Add(cfg.WallClockCap), lastSeen: now}, nil
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
	if !at.Before(s.deadline) {
		return s.trip(TripWallClock, false)
	}
	if s.sinceChange >= s.cfg.RepetitionEvents {
		return s.trip(TripRepetition, true)
	}
	return nil
}

// Tick is the heartbeat check between (or instead of) events: it catches a
// harness that went silent and enforces the wall-clock cap.
func (s *Supervisor) Tick(now time.Time) *Trip {
	if s.tripped != nil {
		return s.tripped
	}
	if !now.Before(s.deadline) {
		return s.trip(TripWallClock, false)
	}
	if now.Sub(s.lastSeen) >= s.cfg.NoProgressAfter {
		return s.trip(TripNoProgress, false)
	}
	return nil
}

// Findings is a copy of what the session has done so far.
func (s *Supervisor) Findings() []string {
	return append([]string(nil), s.findings...)
}

func (s *Supervisor) trip(kind TripKind, retry bool) *Trip {
	s.tripped = &Trip{Kind: kind, Findings: append([]string(nil), s.findings...), RetryStrongerModel: retry}
	return s.tripped
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
