package factoryd

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Folded is the steps with each run of steps in a row of the same kind,
// actor and outcome folded into its first, a session's attempt aside:
// Count is the run's length, CostEUR and the calls the run's.
func (d CardDigest) Folded() []DigestStep {
	var out []DigestStep
	for _, s := range d.Steps {
		if n := len(out); n > 0 && foldActor(out[n-1]) == foldActor(s) && out[n-1].Kind == s.Kind && out[n-1].Outcome == s.Outcome {
			out[n-1].Count++
			out[n-1].CostEUR += s.CostEUR
			out[n-1].calls += s.calls
			continue
		}
		out = append(out, s)
	}
	return out
}

// foldActor is s's actor as Folded compares it: a session's model
// without its attempt, which every session counts up.
func foldActor(s DigestStep) string {
	if s.Kind != "session" {
		return s.Actor
	}
	_, model, _ := strings.Cut(s.Actor, " ")
	return model
}

// Render is the digest as text: the head, one line per folded step, and
// the flags.
func (d CardDigest) Render() string {
	h := d.Head
	head := []string{fmt.Sprintf("#%d", h.Issue)}
	if h.Plan > 0 {
		head = append(head, strings.TrimSpace(fmt.Sprintf("plan #%d %s", h.Plan, h.PlanName)))
	}
	if h.ProofsWanted > 0 {
		head = append(head, fmt.Sprintf("proofs %d of %d stored", h.ProofsStored, h.ProofsWanted))
	}
	head = append(head, fmt.Sprintf("budget %.2f spent %.2f holds %.2f EUR", h.BudgetEUR, h.SpentEUR, h.HoldsEUR))
	if h.SlotHeld {
		head = append(head, "slot held")
	}
	var b strings.Builder
	b.WriteString(strings.Join(append(head, statusText(h.Status, h.Gate)), " · ") + "\n")
	for _, s := range d.Folded() {
		b.WriteString(digestLine(s))
	}
	b.WriteString("flags: " + cmp.Or(flagsText(d.Flags), "none") + "\n")
	return b.String()
}

// DigestText is d rendered, or, when err stopped its read, one line that
// says why: a card read keeps the rest of what it read.
func DigestText(d CardDigest, err error) string {
	if err != nil {
		return "digest unreadable: " + err.Error() + "\n"
	}
	return d.Render()
}

// digestLine is step s as one line of the digest: its number before the
// fold, the one CardStep takes, its offset, its kind with its actor, and
// its outcome, with the run's length and a session's cost and calls.
func digestLine(s DigestStep) string {
	label := s.Kind
	switch {
	case s.Actor == "":
	case s.Kind == "session" || s.Kind == "checkin":
		label += " " + s.Actor
	default:
		label += " by " + s.Actor
	}
	outcome := s.Outcome
	if s.Count > 1 {
		outcome += fmt.Sprintf(" ×%d", s.Count)
	}
	if s.calls > 0 {
		outcome += fmt.Sprintf(", %.2f EUR, %s", s.CostEUR, textutil.Plural(s.calls, "call", "calls"))
	}
	return fmt.Sprintf(" %3d %-6s  %-22s → %s\n", s.N, offsetText(s.Offset), label, outcome)
}

// flagsText is flags by name, each with its detail in brackets.
func flagsText(flags []DigestFlag) string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = f.Name
		if f.Detail != "" {
			out[i] += "(" + f.Detail + ")"
		}
	}
	return strings.Join(out, ", ")
}

// statusText is a status and its gate as the digest prints them.
func statusText(status, gate string) string {
	if gate == "" {
		return status
	}
	return status + ", " + gate
}

// offsetText is d as +h:mm, the hours unbounded.
func offsetText(d time.Duration) string {
	return fmt.Sprintf("+%d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
}
