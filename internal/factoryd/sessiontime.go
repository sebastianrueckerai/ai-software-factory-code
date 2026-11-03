package factoryd

import (
	"fmt"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// EventSessionTime is where one session's time went, the session-time
// panel's row: detail {"session", "attempt", "wall_ms", "commands_ms",
// "classes": {name: {"ms", "count", "runs"}}, "tools": {name: ms},
// "longest": {"class", "ms", "command"}}, other's class without runs.
// Identity: the session and the attempt, as the session's end.
const EventSessionTime = "session_time"

// otherClass is the class of a command no class of the project takes.
const otherClass = "other"

// SessionTimeFact is where one session's time went, by what its harness
// reported ran: its bash calls by the project's command classes, its other
// tools by name.
type SessionTimeFact struct {
	Session  string // with Attempt: the identity
	Attempt  int
	Started  time.Time
	Commands time.Duration
	Classes  map[string]*ClassTime
	Tools    map[string]time.Duration
	Longest  *LongestCommand
}

// ClassTime is one class's commands: their time, their count and, for a
// named class, each run's time in order, so a median is a query.
type ClassTime struct {
	Total time.Duration
	Count int
	Runs  []time.Duration
}

// LongestCommand is a session's longest command: its class, its time and
// its text's first 120 bytes.
type LongestCommand struct {
	Class   string
	Took    time.Duration
	Command string
}

// SessionTimeOf sums a session's transcript: classOf names a command's
// class, "" for other. A subagent's calls are left out, since the parent's
// task call holds their time; a call's start holds none, its end does.
func SessionTimeOf(events []harness.Event, classOf func(command string) string) SessionTimeFact {
	f := SessionTimeFact{Classes: map[string]*ClassTime{}, Tools: map[string]time.Duration{}}
	for _, ev := range events {
		if detailBool(ev, "subagent") {
			continue
		}
		took := commandDuration(ev)
		command, isCommand := "", false
		switch {
		case ev.Kind == harness.EventCommand:
			command, isCommand = detailString(ev, "command"), true
		case ev.Kind == harness.EventToolCall && detailString(ev, "tool") == "bash":
			// a bash call that failed arrives as a tool call, its command in
			// its arguments
			args, _ := ev.Detail["args"].(map[string]any)
			command, _ = args["command"].(string)
			isCommand = true
		case ev.Kind == harness.EventToolCall, ev.Kind == harness.EventFileChange:
			f.Tools[detailString(ev, "tool")] += took
			continue
		}
		if !isCommand {
			continue
		}
		class := classOf(command)
		if class == "" {
			class = otherClass
		}
		c := f.Classes[class]
		if c == nil {
			c = &ClassTime{}
			f.Classes[class] = c
		}
		c.Total += took
		c.Count++
		if class != otherClass {
			c.Runs = append(c.Runs, took)
		}
		f.Commands += took
		if f.Longest == nil || took > f.Longest.Took {
			f.Longest = &LongestCommand{Class: class, Took: took,
				Command: textutil.Truncate(strings.Join(strings.Fields(command), " "), 120)}
		}
	}
	return f
}

// Event is the session's time row on task's card in repository's issue,
// the session ended at ended.
func (f SessionTimeFact) Event(task, repository string, issue int64, ended time.Time) Event {
	classes := map[string]any{}
	for name, c := range f.Classes {
		row := map[string]any{"ms": c.Total.Milliseconds(), "count": c.Count}
		if name != otherClass {
			runs := make([]int64, len(c.Runs))
			for i, r := range c.Runs {
				runs[i] = r.Milliseconds()
			}
			row["runs"] = runs
		}
		classes[name] = row
	}
	tools := map[string]any{}
	for name, took := range f.Tools {
		tools[name] = took.Milliseconds()
	}
	detail := map[string]any{"session": f.Session, "attempt": f.Attempt, "wall_ms": ended.Sub(f.Started).Milliseconds(),
		"commands_ms": f.Commands.Milliseconds(), "classes": classes, "tools": tools}
	if f.Longest != nil {
		detail["longest"] = map[string]any{"class": f.Longest.Class, "ms": f.Longest.Took.Milliseconds(), "command": f.Longest.Command}
	}
	return Event{Repository: repository, TaskID: task, IssueNumber: issue, Kind: EventSessionTime,
		Actor: FactoryDecider, Via: ViaFactoryd, At: ended, Detail: detail,
		IdentityKey: identityKey(EventSessionTime, f.Session, fmt.Sprintf("%d", f.Attempt))}
}
