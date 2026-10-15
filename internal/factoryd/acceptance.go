package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// A plan card's acceptance commands are the runs that prove its work:
// when a session of the card ran one and it exited 0, the work is its own
// evidence, and the card closes done on the fact (roadmap 208) instead of
// waiting for a model verdict, another session or a human's answer.

// AcceptanceHit is the plan acceptance command a session ran with exit 0.
type AcceptanceHit struct {
	Command string `json:"command"`
	Exit    int    `json:"exit"`
	// Session is the session that ran the command; "" until the scanner
	// has read whose transcript the hit came from.
	Session string `json:"session,omitempty"`
}

// AcceptanceEvidence is what a green run says wherever the factory
// records it, the event row's trail and a close's reason alike.
func AcceptanceEvidence(command, session string) string {
	return fmt.Sprintf("acceptance %q exited 0 in session %s", command, session)
}

// acceptanceReason is why a card closed done without a push.
func acceptanceReason(h *AcceptanceHit) string {
	return AcceptanceEvidence(h.Command, h.Session)
}

// CloseCardDone ends card taskID done on its acceptance evidence
// (roadmap 208): an approver's close of a card waiting on a human whose
// event trail shows one of its plan's acceptance commands run green in a
// session, so no answer, no further session and no merge is owed. The end
// is Finish's: the same completion, mirror and dependents as any done
// card. A card not waiting on a human refuses, as does one gone.
func (r *Runner) CloseCardDone(ctx context.Context, taskID, by, evidence string) error {
	t, ok, err := r.store.Task(ctx, taskID)
	switch {
	case err != nil:
		return err
	case !ok:
		return ErrNotFound
	case t.Status != "waiting-human":
		return fmt.Errorf("task %s is %s, not waiting on a human", taskID, t.Status)
	}
	return r.Finish(ctx, taskID, "done", evidence+" (closed by "+by+" on its acceptance)")
}

// planAcceptance is plan card t's acceptance commands, nil for a task no
// plan filed or whose plan card names none. A read that fails is a
// terminal one: a card whose plan cannot be read cannot name its
// evidence, and the session already read this revision when it started.
func (r *Runner) planAcceptance(ctx context.Context, t Task) ([]string, error) {
	if t.Source != SourcePlan || t.Parent == "" {
		return nil, nil
	}
	rev, ok, err := r.store.PlanRevision(ctx, t.Parent, 0)
	if err != nil || !ok {
		return nil, cmp.Or(err, error(terminalError("plan %s of card %s has no revision", t.Parent, t.ID)))
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return nil, terminalError("plan %s revision %d: %v", t.Parent, rev.Revision, err)
	}
	for _, c := range p.Cards {
		if c.Key == t.PlanKey {
			return c.Acceptance, nil
		}
	}
	return nil, nil
}

// sessionAcceptance scans session's transcript of task t for a run of one
// of the plan's acceptance commands that exited 0, records the green run
// on the card's event trail and returns it; nil when the card names no
// acceptance commands, none ran green, or the transcript could not be
// read, which leaves the card to end as it always did.
//
// Only green runs are recorded: the trail is the card's evidence, and a
// red run is a session's private history, not a card event.
func (r *Runner) sessionAcceptance(ctx context.Context, t Task, session string, acceptance []string) *AcceptanceHit {
	if len(acceptance) == 0 {
		return nil
	}
	hit := acceptanceHit(r.sessionTranscript(ctx, t.ID, session), acceptance)
	if hit == nil {
		return nil
	}
	hit.Session = session
	r.timeline.recordEvent(ctx, t.ID, EventAcceptancePassed, map[string]any{
		"command": hit.Command, "exit": hit.Exit, "session": session})
	return hit
}

// sessionTranscript is the session's recorded events, its commands only;
// nil when none was kept or the read failed, which is logged: an
// unreadable transcript must not fail the session that wrote it.
func (r *Runner) sessionTranscript(ctx context.Context, taskID, session string) []harness.Event {
	if r.timeline == nil || r.timeline.Objects == nil {
		return nil
	}
	b, err := r.timeline.Objects.Get(ctx, "sessions/"+taskID+"/"+session+".jsonl")
	if err != nil {
		log.Printf("task %s: session %s's transcript: %v", taskID, session, err)
		return nil
	}
	var events []harness.Event
	for len(b) > 0 {
		var line []byte
		line, b, _ = bytes.Cut(b, []byte("\n"))
		var ev harness.Event
		if err := json.Unmarshal(line, &ev); err == nil && ev.Kind == harness.EventCommand {
			events = append(events, ev)
		}
	}
	return events
}

// acceptanceHit is the newest of events' command runs that ran an
// acceptance command and exited 0, nil when none did: the newest green
// run wins, so a red after a green does not undo the card's evidence — a
// plan's acceptance list is a floor the agent satisfies again, not a race
// it can lose. This differs from the console's read of the trail, which
// needs no such rule: the session-end path records only exit-0 rows, so
// every row it left is green.
func acceptanceHit(events []harness.Event, acceptance []string) *AcceptanceHit {
	if len(acceptance) == 0 {
		return nil
	}
	var hit *AcceptanceHit
	for _, ev := range events {
		if ev.Kind != harness.EventCommand {
			continue
		}
		command, _ := ev.Detail["command"].(string)
		exit, ok := commandExit(ev.Detail)
		if !ok || exit != 0 {
			continue
		}
		for _, item := range acceptance {
			if runsAcceptance(command, item) {
				hit = &AcceptanceHit{Command: command, Exit: exit}
				break
			}
		}
	}
	return hit
}

// commandExit is a command event's exit code: a JSON number arrives as a
// float64, a record built in Go as an int.
func commandExit(detail map[string]any) (int, bool) {
	switch n := detail["exit"].(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// runsAcceptance is whether run ran an acceptance command: equal after
// whitespace, or the command with arguments added — "bash check -- ./..."
// runs what "bash check" names, the arguments the same command's — as the
// whole run or as a step of an && chain (andSteps). The words are
// compared as said: a shell command's case is its meaning.
func runsAcceptance(run, item string) bool {
	item = strings.Join(strings.Fields(item), " ")
	if item == "" {
		return false
	}
	for _, step := range append([]string{run}, andSteps(run)...) {
		step = strings.Join(strings.Fields(step), " ")
		if step == item || strings.HasPrefix(step, item+" ") {
			return true
		}
	}
	return false
}

// andSteps are the steps of run when it is an && chain whose exit 0 says
// every step passed; none when another control operator could hide a
// step's exit (;, |, ||, a lone &, a second line, $?). A redirection's &
// (2>&1, &>) is none.
func andSteps(run string) []string {
	ops := strings.NewReplacer("&&", " ", ">&", " ", "<&", " ", "&>", " ").Replace(run)
	if !strings.Contains(run, "&&") || strings.ContainsAny(ops, ";|&\n") || strings.Contains(ops, "$?") {
		return nil
	}
	return strings.Split(run, "&&")
}
