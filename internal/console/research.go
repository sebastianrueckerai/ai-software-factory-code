package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The chat's research subagent (2026-10-31): one bounded loop on the mid
// tier behind one tool. The researcher drives the chat's own reads, the
// premium thread sees only the report, and the trail of its steps lands
// beside the thread for the human's audit.

const (
	// researchSteps bounds one research call's steps, a starting point
	// tuned from live use; researchKeep is the stream's keepalive while
	// the loop runs, safely under the chat window.
	researchSteps = 12
	researchKeep  = time.Minute
	// researchAnswer bounds one step's answer as the trail records it.
	researchAnswer = 2000
)

// researchWall bounds one research call's whole run; a test shortens it.
var researchWall = 5 * time.Minute

// researchTools are the reads the researcher may drive, in the menu's
// order: the same run funcs the chat's own dispatch runs, so the digest
// wrapper on big reads and pages rides along. No sandbox, no writes.
var researchTools = []string{"tree", "grep", "file", "read_doc", "web_search", "web_scrape"}

// researchPrompt is the researcher's whole brief.
const researchPrompt = "You research one question for the project agent. Work alone: call a tool, read its answer, decide the next. " +
	"Search the repository with tree, grep and file; the documents with read_doc; the web with web_search and web_scrape. " +
	"Stop as soon as the question is answered; a narrow question needs one or two calls. " +
	"Your last answer is the report the agent reads, nothing else follows it: the findings in plain sentences first, " +
	"then the sources, each a path at its ref or a url, then the open questions you could not answer. " +
	"Name no source you did not read."

// research answers one question through the bounded loop and answers the
// report with the trail of every step. The bounds' refusals name the stop
// and the fitting action; a gateway refusal of the loop's own call comes
// back as the error.
func (t *turn) research(question string) (string, string, error) {
	ctx, cancel := context.WithTimeout(t.r.Context(), researchWall)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the stream's write deadline needs a write while the loop runs
		tick := time.NewTicker(researchKeep)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				t.send("ping", nil)
			}
		}
	}()
	var defs []factoryd.OperatorTool
	for _, name := range researchTools {
		if tool, ok := t.c.chatTool(name); ok {
			defs = append(defs, tool.def(false))
		}
	}
	msgs := []factoryd.OperatorMessage{{Role: "system", Content: researchPrompt}, {Role: "user", Content: question}}
	var trail strings.Builder
	for step := 1; ; step++ {
		answer, err := t.c.cfg.Operator.Research(ctx, t.task, t.threadRef(), msgs, defs)
		var refused *policy.Refusal
		switch {
		case errors.As(err, &refused):
			return "", "", &refusal{http.StatusPaymentRequired, "The research was refused: " + refused.Reason}
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Sprintf("The research stopped at its %v time bound: ask a narrower question, or read the trail's best address directly.", researchWall), trail.String(), nil
		case t.r.Context().Err() != nil:
			return "", "", nil // the caller's context ended: the turn's own failure speaks
		case err != nil:
			return "", "", err
		}
		if len(answer.ToolCalls) == 0 {
			return answer.Content, trail.String(), nil
		}
		if step == researchSteps {
			return fmt.Sprintf("The research stopped at its %d-step bound: ask a narrower question, or read the trail's best address directly.", researchSteps), trail.String(), nil
		}
		msgs = append(msgs, answer)
		for _, c := range answer.ToolCalls {
			result := t.researchCall(c, &trail)
			msgs = append(msgs, factoryd.OperatorMessage{Role: "tool", ToolCallID: c.ID, Content: result})
		}
	}
}

// researchRun is the research tool's run, set in init: the loop's tool
// lookup reaches the tools table the entry itself lives in, a cycle Go
// refuses at package init.
var researchRun func(t *turn, in toolInput) (string, *inverse, error)

func init() { researchRun = (*turn).researchTool }

// researchTool is the chat's research tool: the question runs through
// the bounded loop, the report answers, and the trail rides the turn to
// the row beside the thread.
func (t *turn) researchTool(in toolInput) (string, *inverse, error) {
	if strings.TrimSpace(in.Ask) == "" {
		return "", nil, refuse(http.StatusBadRequest, "The research needs a question.")
	}
	report, trail, err := t.research(in.Ask)
	t.trail = trail
	return report, nil, err
}

// researchCall runs one of the researcher's tool calls, the reads the
// chat's own dispatch runs, and records the call and its answer on the
// trail.
func (t *turn) researchCall(call factoryd.OperatorCall, trail *strings.Builder) string {
	tool, ok := t.c.chatTool(call.Name)
	if !ok || !slices.Contains(researchTools, call.Name) {
		return call.Name + " is not one of the research tools."
	}
	var in toolInput
	args := tool.normalized(call.Arguments)
	if json.Unmarshal([]byte(cmp.Or(args, "{}")), &in) != nil {
		return "The arguments are no JSON object of " + call.Name + "'s parameters."
	}
	fmt.Fprintf(trail, "\n%s %s\n", call.Name, cmp.Or(args, "{}"))
	if err := t.admitTool(call.Name, in); err != nil {
		out := "Refused: " + t.sentence(err)
		trail.WriteString(out)
		return out
	}
	out, _, err := tool.run(t, in)
	if err != nil {
		out = "Refused: " + t.sentence(err)
	}
	out = sanitize.Inbound(out)
	trail.WriteString(textutil.Truncate(out, researchAnswer))
	trail.WriteString("\n")
	return out
}
