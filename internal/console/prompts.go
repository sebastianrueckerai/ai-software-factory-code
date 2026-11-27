package console

import (
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// agentFacts are the project agent's facts at a turn's start: the project,
// the human it works for and the autonomy level, the day, the project's
// status, the thread's summary and the question a reply passed.
type agentFacts struct {
	Project, Human, Level string
	Today, Now            string
	Summary, Question     string
}

func agentText(f agentFacts) (string, error) {
	text := factoryd.ProjectAgentPrompt(f.Project, f.Human, f.Level) + "\n\nThe project now:\nToday is " + f.Today + " UTC.\n" + f.Now
	if f.Summary != "" {
		text += "\n\nThis thread before its recent turns, summarised:\n" + f.Summary
	}
	if f.Question != "" {
		text += "\n\nA human asked a question the reply before this one may not have answered:\n" + f.Question + "\nAnswer it in this reply if it still stands."
	}
	return text, nil
}

func init() {
	prompts.RegisterFacts("project-agent", agentText)
	prompts.RegisterStep("project-agent", []string{"console.(*turn).run"},
		agentFacts{Project: "app", Human: "sebastian", Level: policy.AutonomyManual, Today: "2026-11-27", Now: "No card is open."})
	prompts.RegisterStep("title", []string{"console.(*Console).nameThread"}, factoryd.TitlePrompt)
	prompts.RegisterStep("compact", []string{"console.(*turn).compact"}, factoryd.CompactPrompt)
	prompts.RegisterStep("research", []string{"console.(*turn).research"}, "")
}
