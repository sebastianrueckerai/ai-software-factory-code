package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// routineLate is how long after its due a cron run may begin before it
// counts as missed: the factory was down at its time.
const routineLate = 10 * time.Minute

// RoutineRun is a routine's one activity (roadmap 267). It begins the run, applies a pending Skip once, the missed rule and
// the hold rule, files what the routine's kind asks for, records the outcome and pokes the inbox. A run that began earlier
// and has its outcome returns it. One an attempt left without its outcome goes on, so a retry files what that attempt did not.
func (r *Runner) RoutineRun(ctx context.Context, in RoutineRunInput) (RoutineRunResult, error) {
	v := r.cfg.Policy()
	rt, ok := v.Routine(in.Project, in.Routine)
	if !ok {
		return RoutineRunResult{Outcome: "gone", Note: "the routine is no longer in the configuration"}, nil
	}
	loc, err := v.Document.Location()
	if err != nil {
		return RoutineRunResult{}, err
	}
	// policy.yaml's routines (project "") are passes over every project:
	// they file on no project and poke nobody
	var p Project
	if in.Project != "" {
		if p, ok, err = r.Routines.Project(ctx, in.Project); err != nil || !ok {
			return RoutineRunResult{}, cmp.Or(err, fmt.Errorf("routine %s/%s: no project %s in the store", in.Project, in.Routine, in.Project))
		}
	}
	run, began, err := r.Routines.BeginRoutineRun(ctx, RoutineRun{Project: in.Project, Routine: in.Routine, Due: in.Due, Trigger: in.Trigger})
	if err != nil {
		return RoutineRunResult{}, err
	}
	if !began && (!run.Ended.IsZero() || run.TaskID != "") {
		return RoutineRunResult{Outcome: run.Outcome, Note: run.Note, TaskID: run.TaskID, Issue: run.Issue}, nil
	}
	tell := rt.TellRule() == policy.TellInbox
	poke := func(taskID, text string) { r.pokeRoutine(ctx, v.Tree, p.Repository, rt, run.Run, taskID, text) }
	end := func(outcome, note string) (RoutineRunResult, error) {
		if err := r.Routines.EndRoutineRun(ctx, in.Project, in.Routine, run.Run, outcome, note, "", 0, 0); err != nil {
			return RoutineRunResult{}, err
		}
		return RoutineRunResult{Outcome: outcome, Note: note}, nil
	}
	filed := func(taskID string, issue int64) (RoutineRunResult, error) {
		if err := r.Routines.RoutineRunFiled(ctx, in.Project, in.Routine, run.Run, taskID, issue); err != nil {
			return RoutineRunResult{}, err
		}
		return RoutineRunResult{Outcome: "filed", TaskID: taskID, Issue: issue}, nil
	}

	if run.Outcome == "skipped" {
		return end("skipped", run.Note)
	}
	if in.Trigger == "cron" && rt.MissedRule() == policy.MissedSkip && run.Started.Sub(in.Due) > routineLate {
		return end("skipped", "missed: the factory was down at "+in.Due.In(loc).Format("Mon 15:04"))
	}
	open, ok, err := r.Routines.OpenRoutineTask(ctx, in.Project, in.Routine)
	if err != nil {
		return RoutineRunResult{}, err
	}
	switch {
	case ok && rt.WhileOpenRule() == policy.WhileOpenHold:
		res, err := end("held", fmt.Sprintf("held: #%d is still open", open.IssueNumber))
		if err == nil && tell {
			poke(open.ID, fmt.Sprintf("%s is held: #%d from the last run is still open.", rt.Title, open.IssueNumber))
		}
		return res, err
	case ok && rt.WhileOpenRule() == policy.WhileOpenFold:
		return end("folded", fmt.Sprintf("folded into #%d", open.IssueNumber))
	}

	brief, _ := v.Brief(rt.Brief)
	brief = strings.TrimSpace(brief)
	provenance := fmt.Sprintf("filed by the factory · routine %s · run %d", rt.Name, run.Run)
	switch rt.Files {
	case policy.FilesHuman:
		body := strings.TrimSpace(brief + "\n\n" + provenance)
		t, issue, err := fileHumanTask(ctx, r.store, r.forgejo, p.Repo(), rt.Title, body, Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard",
			Objective: rt.Title + "\n\n" + body, Source: SourceRoutine, Author: r.forgejo.BotUser})
		if err != nil {
			return RoutineRunResult{}, err
		}
		res, err := filed(t.ID, issue)
		if err == nil {
			// a card that needs a person pokes under tell quiet too
			poke(t.ID, fmt.Sprintf("#%d needs you: %s.", issue, rt.Title))
		}
		return res, err
	case policy.FilesAgent:
		// the skill is the objective's first line until the prompt pack
		// maps skills (roadmap 226)
		body := strings.TrimSpace("Run the skill " + rt.Skill + ".\n\n" + brief)
		t, ref, err := FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, p, rt.Title, body+"\n\n"+provenance, []string{"routine"},
			Task{ID: NewTaskID(), RiskClass: "standard", BudgetEUR: v.Document.Budget.ClampBudget(0), Objective: rt.Title + "\n\n" + body, Source: SourceRoutine})
		if err != nil {
			return RoutineRunResult{}, err
		}
		res, err := filed(t.ID, ref.Number)
		if err == nil && tell {
			poke(t.ID, fmt.Sprintf("Filed #%d %q.", ref.Number, rt.Title))
		}
		return res, err
	case policy.FilesPullRequest:
		cards, err := r.TemplatePass(ctx)
		if err != nil {
			return RoutineRunResult{}, err
		}
		if cards == 0 {
			return end("no change", "no change")
		}
		note := fmt.Sprintf("%d cards", cards)
		if cards == 1 {
			note = "1 card"
		}
		res, err := end("filed", note)
		if err == nil && tell {
			poke("", rt.Title+": "+note+".")
		}
		return res, err
	default: // a report: a poke and no card
		res, err := end("reported", "")
		if err == nil && tell {
			poke("", strings.TrimSpace(rt.Title+"\n\n"+brief))
		}
		return res, err
	}
}

// pokeRoutine writes text about card taskID ("" for none) into the inbox
// of repository's notify list, else its approvers, as Poker.Pass does,
// with run n of rt as the decider line. A factory routine has no
// repository and pokes nobody. A failed poke is logged: the run stands.
func (r *Runner) pokeRoutine(ctx context.Context, tree policy.Tree, repository string, rt policy.Routine, n int, taskID, text string) {
	if repository == "" {
		return
	}
	m := ChatMessage{Role: "assistant", Kind: MessagePoke, Content: text, TaskID: taskID,
		Decider: fmt.Sprintf("routine %s · run %d · %s", rt.Name, n, rt.Words())}
	// event 0: the poke answers no event, so the cursor stays
	to := pokeTo(tree, repository)
	if _, err := r.Routines.Poke(ctx, 0, repository, to, make([]bool, len(to)), m); err != nil {
		log.Printf("routine %s: run %d: poke: %v", rt.Name, n, err)
	}
}

// endRoutineRun ends the run that filed routine card t, which ended in
// state final: the state is the run's outcome, the card's spend its cost.
// A failure is logged, as taskEnded's are.
func endRoutineRun(ctx context.Context, store Store, t Task, final string) {
	run, ok, err := store.RoutineRunOfTask(ctx, t.ID)
	if err == nil && ok {
		err = store.EndRoutineRun(ctx, run.Project, run.Routine, run.Run, final, run.Note, t.ID, t.IssueNumber, t.SpentEUR)
	}
	if err != nil {
		log.Printf("task %s: the run of its routine: %v", t.ID, err)
	}
}
