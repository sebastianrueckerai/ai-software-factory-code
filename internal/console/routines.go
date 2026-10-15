package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The routines' acts (roadmap 267): Skip once and Run now on a routine of
// a project's board, each one event with its via.

// Run now waits up to runNowWait for its run's outcome, read every
// runNowPoll, so its note names what the run did; settable for the tests.
var (
	runNowWait = 20 * time.Second
	runNowPoll = 500 * time.Millisecond
)

// routineSkip is a routine's Skip once, and with the form's undo=1 its
// undo (doRoutineSkip).
func (c *Console) routineSkip(w http.ResponseWriter, r *http.Request) {
	note, err := c.doRoutineSkip(r.Context(), button(r), r.PathValue("project"), r.PathValue("name"), r.PostFormValue("undo") == "1")
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.routineActed(w, r, note)
}

// routineRun is a routine's Run now (doRoutineRun).
func (c *Console) routineRun(w http.ResponseWriter, r *http.Request) {
	note, err := c.doRoutineRun(r.Context(), button(r), r.PathValue("project"), r.PathValue("name"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.routineActed(w, r, note)
}

// routineActed answers an act with note: the routine's drawer when its
// form sat there (from=drawer), else the board.
func (c *Console) routineActed(w http.ResponseWriter, r *http.Request, note string) {
	if r.PostFormValue("from") == "drawer" {
		c.showRoutineDrawer(w, r, "overview", note)
		return
	}
	c.showBoard(w, r, http.StatusOK, note, nil)
}

// doRoutineSkip sets project's routine name to skip its next run for a,
// or with undo clears it, and answers with the next run that files.
func (c *Console) doRoutineSkip(ctx context.Context, a actor, project, name string, undo bool) (string, error) {
	repository, err := c.routineRepository(ctx, project)
	if err != nil {
		return "", err
	}
	if err := c.cfg.Routines.Skip(ctx, project, name, !undo, a.s.user); err != nil {
		return "", routineFailed(err, project, name)
	}
	log.Printf("console: %s set the skip of routine %s of %s to %t via %s", a.s.user, name, project, !undo, a.via)
	c.record(ctx, a, repository, "", 0, factoryd.EventRoutineSkipped, map[string]any{"project": project, "routine": name, "undo": undo})
	if undo {
		return "Skip once undone", nil
	}
	views, err := c.cfg.Routines.Views(ctx, project)
	if err != nil {
		log.Printf("console: the routines of %s: %v", project, err)
	}
	if i := slices.IndexFunc(views, func(v factoryd.RoutineView) bool { return v.Routine.Name == name }); i >= 0 && views[i].NextWords != "" {
		return "Skipped once · next " + views[i].NextWords, nil
	}
	return "Skipped once", nil
}

// doRoutineRun starts a run of project's routine name now and waits for
// its outcome, which its event records and its note names: the card it
// filed, the hold that kept it, or "Filing…" when none came in time.
func (c *Console) doRoutineRun(ctx context.Context, a actor, project, name string) (string, error) {
	repository, err := c.routineRepository(ctx, project)
	if err != nil {
		return "", err
	}
	runs, err := c.cfg.Routines.Runs(ctx, project, name, 1)
	if err != nil {
		return "", routineFailed(err, project, name)
	}
	after := 0
	if len(runs) > 0 {
		after = runs[0].Run
	}
	if err := c.cfg.Routines.RunNow(ctx, project, name); err != nil {
		return "", routineFailed(err, project, name)
	}
	log.Printf("console: %s ran routine %s of %s now via %s", a.s.user, name, project, a.via)
	run, ok := c.awaitRun(ctx, project, name, after)
	outcome, note := "pending", "Filing…"
	if ok {
		outcome, note = run.Outcome, c.runNote(ctx, a, run)
	}
	c.record(ctx, a, repository, run.TaskID, run.Issue, factoryd.EventRoutineRunNow, map[string]any{"project": project, "routine": name, "outcome": outcome})
	return note, nil
}

// awaitRun is the run-now run of project's routine name numbered above
// after once its outcome is in; false when none came within runNowWait.
// It reads the newest five runs, as a cron or merges run may land after it.
func (c *Console) awaitRun(ctx context.Context, project, name string, after int) (factoryd.RoutineRun, bool) {
	for deadline := time.Now().Add(runNowWait); ; {
		runs, err := c.cfg.Routines.Runs(ctx, project, name, 5)
		if err != nil {
			log.Printf("console: the runs of routine %s of %s: %v", name, project, err)
			return factoryd.RoutineRun{}, false
		}
		if i := slices.IndexFunc(runs, func(run factoryd.RoutineRun) bool {
			return run.Run > after && run.Trigger == factoryd.TriggerRunNow && run.Outcome != ""
		}); i >= 0 {
			return runs[i], true
		}
		if time.Now().After(deadline) {
			return factoryd.RoutineRun{}, false
		}
		select {
		case <-ctx.Done():
			return factoryd.RoutineRun{}, false
		case <-time.After(runNowPoll):
		}
	}
}

// runNote is what run did as a's note: the card it filed and the column
// it waits in, else its note or its outcome.
func (c *Console) runNote(ctx context.Context, a actor, run factoryd.RoutineRun) string {
	switch {
	case run.Outcome == "filed" && run.Issue > 0:
		note := fmt.Sprintf("Filed #%d", run.Issue)
		if t, ok, err := c.cfg.Tasks.Task(ctx, run.TaskID); err == nil && ok {
			if col := fiveColumn(t, a.s.user); col >= 0 {
				note += " in " + columnTitles[col]
			}
		}
		return note
	case run.Outcome == "filed":
		return "Filed " + run.Note
	}
	s := cmp.Or(run.Note, run.Outcome)
	return strings.ToUpper(s[:1]) + s[1:]
}

// routineRepository is the repository of project, which a routine's
// event carries.
func (c *Console) routineRepository(ctx context.Context, project string) (string, error) {
	p, ok, err := c.projectOf(ctx, project)
	switch {
	case err != nil:
		log.Printf("console: projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	case !ok:
		return "", refuse(http.StatusNotFound, "There is no project "+project+".")
	}
	return p.Repository, nil
}

// routineFailed is the refusal of a routine port's err: 404 for a
// routine the project does not run.
func routineFailed(err error, project, name string) error {
	if errors.Is(err, factoryd.ErrNotFound) {
		return refuse(http.StatusNotFound, "There is no routine "+name+" in project "+project+".")
	}
	return err
}
