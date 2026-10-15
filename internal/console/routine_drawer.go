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
// Runs are the newest runs, three on the Overview, and Older those past
// the Runs tab's list; Config is the routine as its project file holds
// it, Factory says the file is policy.yaml. Note is what the member's act
// did, Problems what could not be read.
type routineDrawerData struct {
	View     factoryd.RoutineView
	Tab      string
	Tabs     []drawerTab
	Sub      string
	Rows     []topRow
	Acts     ghost
	Brief    string
	Runs     []routineRun
	Older    int
	Config   string
	Factory  bool
	Note     string
	Problems []string
}

// topRow is a row of a drawer's top block: its term, its value with the
// value's class, the card the value links (its task id, "" for none), and
// the quieter words after it.
type topRow struct{ Term, Value, Class, Card, Why string }

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

// filedNothing are the outcomes of a run that filed nothing, whose row
// shows no duration.
var filedNothing = []string{"held", "skipped", "gone", "no change", "reported"}

// outcomeClasses are the hues of a run's outcomes, an outcome-less run's
// "running" among them.
var outcomeClasses = map[string]string{
	"merged": "st-done", "done": "st-done",
	"no change": "st-closed", "closed": "st-closed", "skipped": "st-closed", "gone": "st-closed",
	"held": "st-waiting", "findings": "st-waiting",
	"failed": "st-failed",
	"filed":  "st-running", "reported": "st-running", "running": "st-running",
}

// The words after a routine's rules in its drawer's top block.
var (
	whileOpenWords = map[string]string{policy.WhileOpenHold: "a run skips while the last card is open, and says so",
		policy.WhileOpenFold: "the pass adds to its open pull request", policy.WhileOpenFile: "a run files another card"}
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
	c.showRoutineDrawer(w, r, tab, "")
}

// showRoutineDrawer renders the drawer of the path's routine on tab with
// note, what the member's act did; an act's drawer keeps the board's
// filter its form carried.
func (c *Console) showRoutineDrawer(w http.ResponseWriter, r *http.Request, tab, note string) {
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
	if r.Method == http.MethodPost {
		f = formFilter(r)
	}
	f.Project = cmp.Or(f.Project, project)
	d := routineDrawerData{View: rv, Tab: tab, Tabs: routineTabs, Note: note, Factory: rv.State.Project == "",
		Acts: ghost{Project: project, Name: name, Title: rt.Title, Skipped: rv.State.SkipNext, Filter: f, CSRF: current(r).csrf, Drawer: true, Tab: tab}}
	v, ok := c.inForce(ctx)
	if !ok {
		d.Problems = append(d.Problems, "The configuration in force could not be read. See the factoryd log.")
	}
	loc, err := v.Document.Location()
	if err != nil {
		loc = time.UTC
	}

	when, in, kind := nextWords(rv, time.Now())
	next := topRow{Term: "Next", Value: when}
	d.Sub = "routine · " + rt.Words() + " · " + kind
	switch {
	case in != "":
		d.Sub, next.Value = d.Sub+" · next "+when+", in "+in, when+" · in "+in
	case when != "":
		d.Sub += " · next " + when
	default: // an event's: the schedule's words name it
		next.Value, next.Class = "—", "muted"
	}
	if rv.State.SkipNext {
		next.Value, next.Class = "skipped once · next "+next.Value, ""
		if when == "" {
			next.Value = "skipped once"
		}
	}
	schedule := topRow{Term: "Schedule", Value: rt.Words()}
	if slices.ContainsFunc(rt.Schedule, func(t policy.Trigger) bool { return t.Cron != "" }) {
		schedule.Why = loc.String()
	}
	last := topRow{Term: "Last", Value: runWords(rv.Last), Card: rv.Last.TaskID}
	switch {
	case rv.Last.Run == 0:
		last.Value, last.Class = "no run yet", "muted"
	case rv.Held:
		last.Value, last.Class, last.Card = rv.Last.Note, "warn", rv.Open.ID
	}
	if rv.Last.Run > 0 {
		last.Why = rv.Last.Due.In(loc).Format(runDue)
	}
	d.Rows = []topRow{schedule, next, last,
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
		limit := 3
		if tab == "runs" {
			limit = routineRunsShown
		}
		runs, err := c.cfg.Routines.Runs(ctx, project, name, limit)
		if err != nil {
			log.Printf("console: the runs of routine %s of %s: %v", name, project, err)
			d.Problems = append(d.Problems, "The routine's runs could not be read. See the factoryd log.")
		}
		for _, run := range runs {
			d.Runs = append(d.Runs, routineRunOf(run, loc))
		}
		if tab == "runs" && len(runs) == routineRunsShown {
			d.Older = rv.State.Runs - len(runs)
		}
	}
	c.render(w, "routine_drawer.html", d)
}

// routineRunOf is run as its drawer row, its due time read in loc.
func routineRunOf(run factoryd.RoutineRun, loc *time.Location) routineRun {
	o := cmp.Or(run.Outcome, "running")
	row := routineRun{Due: run.Due.In(loc).Format(runDue), Outcome: o, Class: cmp.Or(outcomeClasses[o], "st-closed"), Note: run.Note,
		TaskID: run.TaskID, Issue: run.Issue, EUR: run.CostEUR, Costs: run.TaskID != "" && !run.Ended.IsZero()}
	switch {
	case slices.Contains(filedNothing, o):
	case run.Ended.IsZero():
		row.Duration = "open"
	default:
		row.Duration = minutes(run.Ended.Sub(run.Started))
	}
	return row
}
