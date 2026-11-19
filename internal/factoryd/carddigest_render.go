package factoryd

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Folded is the steps with each run of steps in a row of the same kind,
// actor and outcome folded into its first, a session's attempt aside:
// Count is the run's length, CostEUR and the calls the run's.
func (d CardDigest) Folded() []DigestStep {
	var out []DigestStep
	for _, s := range d.Steps {
		if n := len(out); n > 0 && foldActor(out[n-1]) == foldActor(s) && out[n-1].Kind == s.Kind && out[n-1].Outcome == s.Outcome && out[n-1].Message == s.Message {
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
	head = append(head, fmt.Sprintf("budget %s spent %s holds %s EUR", textutil.EUR(h.BudgetEUR), textutil.EUR(h.SpentEUR), textutil.EUR(h.HoldsEUR)))
	if h.SlotHeld {
		head = append(head, "slot held")
	}
	head = append(head, statusText(h.Status, h.Gate))
	if h.MergeQueue != nil && h.MergeQueue.Ahead > 0 && h.MergeQueue.HeadIssue > 0 {
		head = append(head, fmt.Sprintf("waits to merge behind #%d", h.MergeQueue.HeadIssue))
	}
	if h.Hold != "" {
		head = append(head, h.Hold)
	}
	var b strings.Builder
	b.WriteString(strings.Join(head, " · ") + "\n")
	for _, s := range d.Folded() {
		b.WriteString(digestLine(s))
		if s.Message != "" {
			fmt.Fprintf(&b, "%s message: %s\n", strings.Repeat(" ", 4), s.Message)
		}
	}
	b.WriteString("flags: " + cmp.Or(flagsText(d.Flags), "none") + "\n")
	return b.String()
}

// LifeLine is one step of a card's life as a human reads it: when it
// came, what came of it, and the words a supervisor sent the session,
// which are the supervisor's.
type LifeLine struct {
	At          time.Time
	Text, Quote string
}

var (
	// lifeIDs are the finding ids after the word that leads them
	lifeIDs = regexp.MustCompile(`\b(findings|confirmed|dropped|remark)((?: [0-9a-f]{12})+)`)
	// lifeHead is a commit's hash, with the "at" that leads it
	lifeHead = regexp.MustCompile(` ?(?:\bat )?\b[0-9a-f]{7,40}\b:?`)
	// lifeKind is the kind of an ask that opens its line
	lifeKind = regexp.MustCompile(`^[a-z]+(?:_[a-z]+)+: `)
)

// lifeWriters are the kinds of steps whose outcome is a writer's words: a
// session's handover and a human's answer.
var lifeWriters = []string{"handover", "answer"}

// lifeLeads are the kinds of steps as a human reads them.
var lifeLeads = map[string]string{
	"filed": "Filed", "session": "Session", "review": "The model review", "checkin": "The check-in", "checks": "The checks",
	"audit": "The security audit", "proofs": "The proofs", "opened": "A proof", "ask": "It asked", "answer": "Answered",
	"granted": "Granted", "funded": "Money added", "ceiling": "The spend limit", "extended": "More allowed",
	"retried": "Tried again", "replanned": "Sent back to the planner", "replan": "Sent back to the planner",
	"cut": "Cut into cards", "dependency": "Made to wait", "status": "Status", "approval": "Asked to approve", "merged": "Merged",
	"ended": "Ended", "criteria": "The promises", "revision": "The plan", "handover": "Handed over", "judgement": "Judged",
}

// Life is the digest as a human reads it, one line per folded step: the
// log Render gives the agents, in words, with a count for the finding
// ids and no hash.
func (d CardDigest) Life() []LifeLine {
	var out []LifeLine
	for _, s := range d.Folded() {
		l := LifeLine{At: s.At, Text: lifeText(s), Quote: s.Message}
		if slices.Contains(lifeWriters, s.Kind) && s.Outcome != "" {
			l.Quote, s.Outcome = s.Outcome, ""
			l.Text = strings.TrimSuffix(lifeText(s), ".") + ":"
		}
		out = append(out, l)
	}
	return out
}

// lifeText is step s as a line of the card's life: its kind and who took
// it, its outcome with a count for its finding ids and no hash, the run's
// length and a session's cost and calls.
func lifeText(s DigestStep) string {
	words := func(w string) string { return strings.NewReplacer("_", " ", "-", " ").Replace(w) }
	lead := cmp.Or(lifeLeads[s.Kind], capitalize(words(s.Kind)))
	switch {
	case s.Actor == "":
	case s.Kind == "session":
		attempt, model, _ := strings.Cut(s.Actor, " ")
		lead += " " + attempt + " on " + model
	case s.Kind == "checkin":
		lead += " at the " + words(s.Actor)
	default:
		lead += " by " + words(DeciderWords(s.Actor))
	}
	outcome := lifeIDs.ReplaceAllStringFunc(s.Outcome, func(m string) string {
		g := lifeIDs.FindStringSubmatch(m)
		n := len(strings.Fields(g[2]))
		if g[1] == "findings" {
			return textutil.Plural(n, "finding", "findings")
		}
		return g[1] + " " + strconv.Itoa(n)
	})
	if s.state == string(OutcomePushed) {
		outcome = "pushed its work"
	}
	outcome = lifeHead.ReplaceAllStringFunc(outcome, func(m string) string {
		if strings.ContainsAny(m, "0123456789") {
			return ""
		}
		return m
	})
	outcome = strings.TrimSpace(lifeKind.ReplaceAllString(strings.TrimSpace(outcome), ""))
	if s.Count > 1 {
		outcome += fmt.Sprintf(", %d times", s.Count)
	}
	if s.calls > 0 {
		outcome += fmt.Sprintf(", %s EUR in %s", textutil.EUR(s.CostEUR), textutil.Plural(s.calls, "call", "calls"))
	}
	if outcome == "" {
		return lead + "."
	}
	return endSentence(lead + ": " + outcome)
}

// DigestText is d rendered, or, when err stopped its read, one line that
// says why, scrubbed by scrub: a card read keeps the rest of what it read.
func DigestText(scrub *redact.Scrubber, d CardDigest, err error) string {
	if err != nil {
		return "digest unreadable: " + scrub.String(err.Error()) + "\n"
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
		outcome += fmt.Sprintf(", %s EUR, %s", textutil.EUR(s.CostEUR), textutil.Plural(s.calls, "call", "calls"))
	}
	return fmt.Sprintf(" %3d %-6s  %-22s → %s\n", s.N, OffsetText(s.Offset), label, outcome)
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

// OffsetText is d as +h:mm, the hours unbounded.
func OffsetText(d time.Duration) string {
	return fmt.Sprintf("+%d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
}
