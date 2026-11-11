package factoryd

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RungEnd is why a rung's step ended.
type RungEnd string

const (
	// RungAnswered: a valid answer with a decision to run.
	RungAnswered RungEnd = "answered"
	// RungHandedUp: the rung judged and answered hand_up.
	RungHandedUp RungEnd = "handed_up"
	// The rung did not judge: a cap, a refused call or refused answers
	// ended it before an answer.
	RungTimeCap    RungEnd = "time_cap"
	RungCallCap    RungEnd = "call_cap"
	RungRefused    RungEnd = "calls_refused"
	RungUnreadable RungEnd = "answers_refused"
)

// RungOutcome is how a rung's step ended: an Answer to run, or handed up.
// A hand_up answer sets HandedUp and keeps the Answer for its
// recommendation; every end that did not judge sets HandedUp with Found
// naming the cause and no Answer. End tells them apart.
type RungOutcome struct {
	Answer   *RungAnswer
	End      RungEnd
	HandedUp bool
	Found    string
	Read     []CaseRead
	CostEUR  float64
}

// runRung asks the model of rung's step for its answer on f: every call
// posts the answer tool beside the evidence tools and the last forces it.
// An answer outside the schema's enums is refused with the refusal named
// and re-asked once, on a forced call past the call cap when it came on
// the last one; a second refusal hands the rung up. It acts on nothing.
func (r *Runner) runRung(ctx context.Context, chat checkinChat, taskID, alias string, f CaseFacts, rung int, system, statement string, evidence []CheckinTool, tools podTools, callCap int, timeCap time.Duration) RungOutcome {
	var parsed RungAnswer
	end := converse(ctx, chat, tools, taskID, alias, conversation{
		System:    system,
		Statement: statement,
		Evidence:  evidence,
		Answer:    CheckinTool{Name: checkinAnswerTool, Description: "the rung's answer, one JSON object as this tool's arguments", Parameters: AnswerSchema(f, rung)},
		Parse: func(text string) error {
			a, err := ParseRungAnswer(f, rung, text)
			parsed = a
			return err
		},
		CallCap:       callCap,
		TimeCap:       timeCap,
		Scrub:         r.scrub,
		RepairPastCap: true,
		DueOwnLine:    true,
	})
	out := RungOutcome{CostEUR: end.CostEUR}
	for _, c := range end.Evidence {
		out.Read = append(out.Read, CaseRead{Tool: c.Name, Args: c.Args, Bytes: c.ResultBytes})
	}
	switch end.Kind {
	case convAnswered:
		out.Answer = &parsed
		out.End = RungAnswered
		if parsed.Decision == RungHandUp {
			out.End, out.HandedUp = RungHandedUp, true
		}
		return out
	case convTimeCap:
		out.End, out.Found = RungTimeCap, "the rung's time cap of "+timeCap.String()+" ended it before an answer"+refusedText(end.Refusals)
	case convCallCap:
		out.End, out.Found = RungCallCap, fmt.Sprintf("the rung's call cap of %d calls ended it before an answer", callCap)+refusedText(end.Refusals)
	case convRefused:
		out.End, out.Found = RungRefused, "the rung's calls were refused: "+end.Detail
	case convUnreadable:
		out.End, out.Found = RungUnreadable, "the rung's answers were refused twice: "+strings.Join(end.Refusals, "; ")
	}
	out.HandedUp = true
	return out
}

// refusedText closes a cap's Found with the answers the rung had refused.
func refusedText(refusals []string) string {
	if len(refusals) == 0 {
		return ""
	}
	return "; its answer was refused: " + strings.Join(refusals, "; ")
}
