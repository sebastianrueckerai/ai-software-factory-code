package factoryd

import (
	"context"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// RungOutcome is how a rung's step ended: an Answer to run, or handed up.
// A hand_up answer sets HandedUp and keeps the Answer for its
// recommendation; two refused answers set HandedUp with Found naming both
// refusals and no Answer.
type RungOutcome struct {
	Answer   *RungAnswer
	HandedUp bool
	Found    string
	Read     []CaseRead
	CostEUR  float64
}

// runRung asks the model of rung's step for its answer on f: every call
// posts the answer tool beside the evidence tools and the last forces it.
// An answer outside the schema's enums is refused once with the refusal
// named and re-asked; a second refusal hands the rung up. It acts on
// nothing.
func runRung(ctx context.Context, chat checkinChat, taskID, alias string, f CaseFacts, rung int, system, statement string, evidence []CheckinTool, tools podTools, callCap int, timeCap time.Duration) RungOutcome {
	scrub := redact.New().String
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
		CallCap: callCap,
		TimeCap: timeCap,
		Scrub:   scrub,
	})
	out := RungOutcome{CostEUR: end.CostEUR}
	for _, c := range end.Evidence {
		out.Read = append(out.Read, CaseRead{Tool: c.Name, Args: c.Args, Bytes: c.ResultBytes})
	}
	switch end.Kind {
	case convAnswered:
		out.Answer = &parsed
		out.HandedUp = parsed.Decision == RungHandUp
	case convTimeCap:
		out.HandedUp, out.Found = true, "the rung's time cap of "+timeCap.String()+" ended it before an answer"
	case convCallCap:
		out.HandedUp, out.Found = true, "the rung's call cap ended it before an answer"
	case convRefused:
		out.HandedUp, out.Found = true, "the rung's calls were refused: "+end.Detail
	case convUnreadable:
		out.HandedUp, out.Found = true, "the rung's answers were refused twice: "+strings.Join(end.Refusals, "; ")
	}
	return out
}
