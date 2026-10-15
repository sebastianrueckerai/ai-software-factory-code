package console

import (
	"cmp"
	"context"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// routineProvenance is the line under the top block of a card a routine
// filed (C16): "filed by the factory · routine Weekly bump · run 15 · last
// run #781 merged in 2 d". Title is "" once the routine left the
// configuration, which leaves it no drawer to open.
type routineProvenance struct {
	Project, Name, Title string
	Run                  int
	LastWords            string
}

// routineLineOf is the provenance of card taskID, nil when no run filed
// it. Its last run is the routine's newest run other than the card's.
func (c *Console) routineLineOf(ctx context.Context, taskID string) (*routineProvenance, error) {
	run, ok, err := c.cfg.Routines.RunOfTask(ctx, taskID)
	if err != nil || !ok {
		return nil, err
	}
	l := &routineProvenance{Project: run.Project, Name: run.Routine, Run: run.Run}
	views, err := c.cfg.Routines.Views(ctx, run.Project)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(views, func(v factoryd.RoutineView) bool { return v.Routine.Name == run.Routine })
	if i < 0 {
		return l, nil
	}
	l.Title = views[i].Routine.Title
	runs, err := c.cfg.Routines.Runs(ctx, run.Project, run.Routine, 2)
	if err != nil {
		return nil, err
	}
	if j := slices.IndexFunc(runs, func(o factoryd.RoutineRun) bool { return o.Run != run.Run }); j >= 0 {
		l.LastWords = runWords(runs[j])
	}
	return l, nil
}

// routineTabs are the routine drawer's tabs.
var routineTabs = []drawerTab{{"overview", "Overview"}, {"runs", "Runs"}, {"config", "Config"}}

// routineRunsShown bounds the runs the drawer lists: an hourly routine
// runs some 700 times a month.
const routineRunsShown = 100

// runDue is a run's due time as the drawer shows it.
const runDue = "Mon 2 Jan 15:04"

// routineDrawerData is a routine's drawer on one tab (C16). Sub is the
// head's line and Rows the top block; Acts carries the ghost's forms.
// Runs are the newest runs, Recent the Overview's first three of them and
// Older those past the list; Config is the routine as its project file
// holds it.
type routineDrawerData struct {
	View         factoryd.RoutineView
	Tab          string
	Tabs         []drawerTab
	Sub          string
	Rows         []topRow
	Acts         ghost
	Brief        string
	Runs, Recent []routineRun
	Older        int
	Config       string
	Problem      string
}

// topRow is a row of a drawer's top block: its term, its value with the
// value's class, and the quieter words after it.
type topRow struct{ Term, Value, Class, Why string }

// routineRun is a run as the routine's drawer lists it, in the Time
// section's shape (C10): when it was due, its card, its outcome in its
// hue, how long it took or "open", the card's cost once it ended, and
// its note.
type routineRun struct {
	Due, Outcome, Class, Duration, Note, TaskID string
	Issue                                       int64
	EUR                                         float64
	Costs                                       bool
}

// outcomeClasses are the hues of a run's outcomes, an outcome-less run's
// "running" among them.
var outcomeClasses = map[string]string{
	"merged": "st-done", "done": "st-done",
	"no change": "st-closed", "closed": "st-closed", "skipped": "st-closed", "folded": "st-closed", "gone": "st-closed",
	"held": "st-waiting", "findings": "st-waiting",
	"failed": "st-failed",
	"filed":  "st-running", "reported": "st-running", "running": "st-running",
}

// The words after a routine's rules in its drawer's top block.
var (
	whileOpenWords = map[string]string{policy.WhileOpenHold: "a run skips while the last card is open, and says so",
		policy.WhileOpenFold: "a run adds to the open card", policy.WhileOpenFile: "a run files another card"}
	tellWords   = map[string]string{policy.TellInbox: "a filing pokes the inbox", policy.TellQuiet: "only a human card pokes the inbox"}
	missedWords = map[string]string{policy.MissedFileOnce: "a missed run files once when the factory is back", policy.MissedSkip: "a missed run is skipped"}
)

// routineDrawer answers GET /routines/{project}/{name}/drawer?tab=overview|runs|config
// to a writer of the project, 403 to anyone else.
func (c *Console) routineDrawer(w http.ResponseWriter, r *http.Request) {
	tab := cmp.Or(r.URL.Query().Get("tab"), "overview")
	if !slices.ContainsFunc(routineTabs, func(t drawerTab) bool { return t.Name == tab }) {
		http.Error(w, "tab must be overview, runs or config", http.StatusBadRequest)
		return
	}
	if c.cfg.Routines == nil {
		http.NotFound(w, r)
		return
	}
	ctx, project, name := r.Context(), r.PathValue("project"), r.PathValue("name")
	views, err := c.cfg.Routines.Views(ctx, project)
	if err != nil {
		log.Printf("console: the routines of %s: %v", project, err)
		http.Error(w, "cannot read the routines", http.StatusInternalServerError)
		return
	}
	i := slices.IndexFunc(views, func(v factoryd.RoutineView) bool { return v.Routine.Name == name })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	rv := views[i]
	rt := rv.Routine
	f := urlFilter(r)
	f.Project = cmp.Or(f.Project, project)
	d := routineDrawerData{View: rv, Tab: tab, Tabs: routineTabs, Acts: ghost{Project: project, Name: name, Title: rt.Title, Filter: f, CSRF: current(r).csrf}}
	v, ok := c.inForce(ctx)
	if !ok {
		d.Problem = "The configuration in force could not be read. See the factoryd log."
	}
	loc, err := v.Document.Location()
	if err != nil {
		loc = time.UTC
	}

	when, in, kind := nextWords(rv, time.Now())
	next := rv.NextWords
	d.Sub = "routine · " + rt.Words() + " · " + kind
	switch {
	case in != "":
		d.Sub, next = d.Sub+" · next "+when+", in "+in, when+" · in "+in
	case when != "":
		d.Sub += " · next " + when
	}
	schedule := topRow{Term: "Schedule", Value: rt.Words()}
	if slices.ContainsFunc(rt.Schedule, func(t policy.Trigger) bool { return t.Cron != "" }) {
		schedule.Why = loc.String()
	}
	last := topRow{Term: "Last", Value: runWords(rv.Last)}
	switch {
	case rv.Last.Run == 0:
		last.Value, last.Class = "no run yet", "muted"
	case rv.Held:
		last.Value, last.Class = rv.Last.Note, "warn"
	}
	if rv.Last.Run > 0 {
		last.Why = rv.Last.Due.In(loc).Format(runDue)
	}
	d.Rows = []topRow{schedule, {Term: "Next", Value: next}, last,
		{Term: "While open", Value: rt.WhileOpenRule(), Why: whileOpenWords[rt.WhileOpenRule()]},
		{Term: "Tells", Value: rt.TellRule(), Why: tellWords[rt.TellRule()]}}
	if rt.Missed != "" {
		d.Rows = append(d.Rows, topRow{Term: "Missed", Value: rt.Missed, Why: missedWords[rt.Missed]})
	}

	brief, _ := v.Tree.Brief(rt.Brief)
	d.Brief = strings.TrimSpace(brief)
	if d.Config, err = routineYAML(rt); err != nil {
		log.Printf("console: routine %s of %s as YAML: %v", name, project, err)
	}
	if tab != "config" {
		runs, err := c.cfg.Routines.Runs(ctx, project, name, routineRunsShown)
		if err != nil {
			log.Printf("console: the runs of routine %s of %s: %v", name, project, err)
			d.Problem = "The routine's runs could not be read. See the factoryd log."
		}
		for _, run := range runs {
			d.Runs = append(d.Runs, routineRunOf(run, loc))
		}
		d.Recent = d.Runs[:min(3, len(d.Runs))]
		if len(runs) == routineRunsShown {
			d.Older = rv.State.Runs - len(runs)
		}
	}
	c.render(w, "routine_drawer.html", d)
}

// routineRunOf is run as its drawer row, its due time read in loc.
func routineRunOf(run factoryd.RoutineRun, loc *time.Location) routineRun {
	o := cmp.Or(run.Outcome, "running")
	row := routineRun{Due: run.Due.In(loc).Format(runDue), Outcome: o, Class: cmp.Or(outcomeClasses[o], "st-closed"), Duration: "open", Note: run.Note,
		TaskID: run.TaskID, Issue: run.Issue, EUR: run.CostEUR, Costs: run.TaskID != "" && !run.Ended.IsZero()}
	if !run.Ended.IsZero() {
		row.Duration = minutes(run.Ended.Sub(run.Started))
	}
	return row
}
