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

// RoutineRun is a routine's one activity (roadmap 267). It begins the
// run, applies a pending Skip once, the missed rule and the hold rule,
// files what the routine's kind asks for and records the outcome, whose
// event pokes the inbox by tell; a human card's needs-you row pokes
// whatever tell says. A run that began earlier and has its
// outcome returns it. One an attempt left without its outcome goes on, so
// a retry files what that attempt did not.
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
	// a Run now leaves a pending Skip once to the run it was set for
	run, began, err := r.Routines.BeginRoutineRun(ctx, RoutineRun{Project: in.Project, Routine: in.Routine, Due: in.Due, Trigger: in.Trigger},
		in.Trigger != TriggerRunNow)
	if err != nil {
		return RoutineRunResult{}, err
	}
	if !began && (!run.Ended.IsZero() || run.TaskID != "") {
		return RoutineRunResult{Outcome: run.Outcome, Note: run.Note, TaskID: run.TaskID, Issue: run.Issue}, nil
	}
	tell := rt.TellRule() == policy.TellInbox
	// end and filed record the outcome; poke is the inbox's words, "" for
	// none
	end := func(outcome, note, poke string) (RoutineRunResult, error) {
		if err := r.Routines.EndRoutineRun(ctx, in.Project, in.Routine, run.Run, outcome, note, "", 0, 0); err != nil {
			return RoutineRunResult{}, err
		}
		r.routineRan(ctx, p.Repository, in, rt.Words(), run.Run, outcome, note, "", 0, poke)
		return RoutineRunResult{Outcome: outcome, Note: note}, nil
	}
	filed := func(taskID string, issue int64, poke string) (RoutineRunResult, error) {
		if err := r.Routines.RoutineRunFiled(ctx, in.Project, in.Routine, run.Run, taskID, issue); err != nil {
			return RoutineRunResult{}, err
		}
		r.routineRan(ctx, p.Repository, in, rt.Words(), run.Run, "filed", "", taskID, issue, poke)
		return RoutineRunResult{Outcome: "filed", TaskID: taskID, Issue: issue}, nil
	}

	if run.Outcome == "skipped" {
		return end("skipped", run.Note, "")
	}
	if in.Trigger == "cron" && rt.MissedRule() == policy.MissedSkip && run.Started.Sub(in.Due) > routineLate {
		return end("skipped", "missed: the factory was down at "+in.Due.In(loc).Format("Mon 15:04"), "")
	}
	// fold is a pull-request routine's, whose pass folds into its own open
	// pull request: a routine's card holds only a human or agent routine
	if open, ok, err := r.Routines.OpenRoutineTask(ctx, in.Project, in.Routine); err != nil {
		return RoutineRunResult{}, err
	} else if ok && rt.WhileOpenRule() == policy.WhileOpenHold {
		return end("held", fmt.Sprintf("held: #%d is still open", open.IssueNumber), "")
	}

	brief, _ := v.Brief(rt.Brief)
	brief = strings.TrimSpace(brief)
	provenance := fmt.Sprintf("%s%s · run %d", RoutineProvenance, rt.Name, run.Run)
	told := func(text string) string {
		if !tell {
			return ""
		}
		return text
	}
	files := rt.Files
	if files == policy.FilesHuman || files == policy.FilesAgent {
		st, err := r.Routines.RoutineState(ctx, in.Project, in.Routine)
		if err != nil {
			return RoutineRunResult{}, err
		}
		files = filesInForce(rt, st)
	}
	switch files {
	case policy.FilesHuman:
		body := strings.TrimSpace(brief + "\n\n" + provenance)
		t, ref, err := fileHumanCard(ctx, r.store, r.forgejo, r.timeline, p.Repo(), rt.Title, body, []string{"routine"}, Task{ID: NewTaskID(),
			RiskClass: "standard", Objective: rt.Title + "\n\n" + body, Source: SourceRoutine, Author: r.forgejo.BotUser})
		if err != nil {
			return RoutineRunResult{}, err
		}
		// the card's needs-you row pokes, under tell quiet too
		return filed(t.ID, ref.Number, "")
	case policy.FilesAgent:
		// the skill is the objective's first line until the prompt pack
		// maps skills (roadmap 226)
		body := strings.TrimSpace(brief)
		if rt.Skill != "" {
			body = strings.TrimSpace("Run the skill " + rt.Skill + ".\n\n" + body)
		}
		t, ref, err := FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, p, rt.Title, body+"\n\n"+provenance, []string{"routine"},
			Task{ID: NewTaskID(), RiskClass: "standard", BudgetEUR: v.Document.Budget.ClampBudget(0), Objective: rt.Title + "\n\n" + body, Source: SourceRoutine})
		if err != nil {
			return RoutineRunResult{}, err
		}
		return filed(t.ID, ref.Number, told(fmt.Sprintf("Filed #%d %q.", ref.Number, rt.Title)))
	case policy.FilesPullRequest:
		cards, err := r.TemplatePass(ctx)
		if err != nil {
			return RoutineRunResult{}, err
		}
		if cards == 0 {
			return end("no change", "", "")
		}
		note := fmt.Sprintf("%d cards", cards)
		if cards == 1 {
			note = "1 card"
		}
		return end("filed", note, told(rt.Title+": "+note+"."))
	default: // a report: a poke and no card
		return end("reported", "", told(strings.TrimSpace(rt.Title+"\n\n"+brief)))
	}
}

// RoutineProvenance starts the line that ends the body of a routine's
// card: "filed by the factory · routine weekly-bump · run 15".
const RoutineProvenance = "filed by the factory · routine "

// filesInForce is the side routine rt's next run files on: its files,
// unless the live toggle set a side for a human or agent routine.
func filesInForce(rt policy.Routine, st RoutineState) string {
	switch {
	case st.NeedsMe == nil, rt.Files != policy.FilesHuman && rt.Files != policy.FilesAgent:
		return rt.Files
	case *st.NeedsMe:
		return policy.FilesHuman
	default:
		return policy.FilesAgent
	}
}

// RoutineRunFailed records run in, whose activity failed for good, as
// failed with note, the error's words: the run begins first when the
// failure came before it did, leaving a pending Skip once to the next
// run. A run that ended or filed its card stays as it is; one that took
// its outcome when it began, a skip, ends with it.
func (r *Runner) RoutineRunFailed(ctx context.Context, in RoutineRunInput, note string) error {
	run, _, err := r.Routines.BeginRoutineRun(ctx, RoutineRun{Project: in.Project, Routine: in.Routine, Due: in.Due, Trigger: in.Trigger}, false)
	if err != nil {
		return err
	}
	if !run.Ended.IsZero() || run.TaskID != "" {
		return nil
	}
	outcome := "failed"
	if run.Outcome != "" {
		outcome, note = run.Outcome, run.Note
	}
	if err := r.Routines.EndRoutineRun(ctx, in.Project, in.Routine, run.Run, outcome, note, "", 0, 0); err != nil {
		return err
	}
	var repository, words string
	if p, ok, err := r.Routines.Project(ctx, in.Project); err == nil && ok {
		repository = p.Repository
	}
	if rt, ok := r.cfg.Policy().Routine(in.Project, in.Routine); ok {
		words = rt.Words()
	}
	r.routineRan(ctx, repository, in, words, run.Run, outcome, note, "", 0, "")
	return nil
}

// routineRan logs run n of in's routine reaching outcome and, for a
// project's routine (repository set), records it as a routine_ran event
// on the card taskID ("" for none), which pokes the inbox with poke
// unless it is "". words are the routine's schedule in words, the poke's
// decider line. A failed record is logged: the run stands.
func (r *Runner) routineRan(ctx context.Context, repository string, in RoutineRunInput, words string, n int, outcome, note, taskID string, issue int64, poke string) {
	said := note
	if issue > 0 {
		said = fmt.Sprintf("#%d", issue)
	}
	name := in.Routine
	if in.Project != "" {
		name = in.Project + "/" + in.Routine
	}
	log.Printf("routines: %s", strings.TrimSpace(fmt.Sprintf("%s run %d %s %s", name, n, outcome, said)))
	if repository == "" || r.timeline == nil || r.timeline.Events == nil {
		return
	}
	detail := map[string]any{"project": in.Project, "routine": in.Routine, "run": n, "outcome": outcome, "schedule": words}
	if note != "" {
		detail["note"] = note
	}
	if poke != "" {
		detail["poke"] = poke
	}
	if _, err := r.timeline.Events.Record(ctx, Event{Repository: repository, TaskID: taskID, IssueNumber: issue, Kind: EventRoutineRan,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}); err != nil {
		log.Printf("routines: %s run %d: the event: %v", name, n, err)
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
