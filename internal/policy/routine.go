package policy

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	// The zone database travels in the binary: time_zone means the same
	// in config check, factoryd and the gateway, whatever the image holds.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// Trigger is one entry of a routine's schedule: a cron pattern, a merge
// count or an event. Exactly one field is set.
type Trigger struct {
	// Cron is five fields, minute hour day-of-month month day-of-week, in
	// the factory's time zone.
	Cron string `yaml:"cron,omitempty" json:"cron,omitempty"`
	// Merges runs the routine every n merged cards of the project.
	Merges int `yaml:"merges,omitempty" json:"merges,omitempty"`
	// On runs the routine on an event: configuration-version or
	// push-main.
	On string `yaml:"on,omitempty" json:"on,omitempty"`
}

// Routine is recurring work the factory does on its own (roadmap 267):
// what a run files, when, and the rules of a run.
type Routine struct {
	// Name is the routine's slug, unique in its file.
	Name string `yaml:"name" json:"name"`
	// Title is the filed card's title.
	Title string `yaml:"title" json:"title"`
	// Schedule lists the triggers, any one of which starts a run.
	Schedule []Trigger `yaml:"schedule" json:"schedule"`
	// Files is the kind of work a run files: human, agent, pull-request
	// or report.
	Files string `yaml:"files" json:"files"`
	// Brief is the path of the card's body in the tree,
	// routines/<name>.md.
	Brief string `yaml:"brief,omitempty" json:"brief,omitempty"`
	// Skill is the skill an agent routine's card runs.
	Skill string `yaml:"skill,omitempty" json:"skill,omitempty"`
	// Pass names the pass a pull-request routine runs: template.
	Pass string `yaml:"pass,omitempty" json:"pass,omitempty"`
	// WhileOpen says what a run does while the last card is open: hold
	// (the default) skips the run, file opens another, and fold, a
	// pull-request routine's only, has the pass add to its open pull
	// request.
	WhileOpen string `yaml:"while_open,omitempty" json:"while_open,omitempty"`
	// Missed says what a run does when its time passed while the factory
	// was down: file-once (the default) or skip.
	Missed string `yaml:"missed,omitempty" json:"missed,omitempty"`
	// Tell says whether a filing pokes the inbox: inbox (the default) or
	// quiet. A human card pokes in any case, and a report must use inbox:
	// its report is the poke.
	Tell string `yaml:"tell,omitempty" json:"tell,omitempty"`
}

const (
	FilesHuman       = "human"
	FilesAgent       = "agent"
	FilesPullRequest = "pull-request"
	FilesReport      = "report"

	OnConfigurationVersion = "configuration-version"
	OnPushMain             = "push-main"

	PassTemplate = "template"

	WhileOpenHold  = "hold"
	WhileOpenFold  = "fold"
	WhileOpenFile  = "file"
	MissedFileOnce = "file-once"
	MissedSkip     = "skip"
	TellInbox      = "inbox"
	TellQuiet      = "quiet"
)

// The values of a routine's enumerated keys. A key's default comes first.
var (
	RoutineFiles    = []string{FilesHuman, FilesAgent, FilesPullRequest, FilesReport}
	RoutineEvents   = []string{OnConfigurationVersion, OnPushMain}
	RoutinePasses   = []string{PassTemplate}
	WhileOpenValues = []string{WhileOpenHold, WhileOpenFold, WhileOpenFile}
	MissedValues    = []string{MissedFileOnce, MissedSkip}
	TellValues      = []string{TellInbox, TellQuiet}
)

// cronParser reads a cron trigger's five fields, without descriptors.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// cronEpoch is where Validate starts a pattern's search for its next time.
// A fixed time judges a tree the same on every day, and a pattern that
// matches any date matches one within cronHorizon years of it.
var cronEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// weekdays word a cron pattern's day of the week.
var weekdays = map[string]string{"0": "Sunday", "1": "Monday", "2": "Tuesday", "3": "Wednesday", "4": "Thursday", "5": "Friday", "6": "Saturday"}

// Validate checks the routine's shape. Its errors start with where, the
// block that holds the routine, such as "project <name>".
func (r Routine) Validate(where string) error {
	at := fmt.Sprintf("%s: routine %q", where, r.Name)
	if !projectFileName.MatchString(r.Name) {
		return fmt.Errorf("%s: the name: lowercase letters, digits and dashes, at most 37", at)
	}
	if r.Title == "" {
		return fmt.Errorf("%s: no title", at)
	}
	switch r.Files {
	case FilesHuman, FilesReport:
	case FilesAgent:
		if r.Skill == "" {
			return fmt.Errorf("%s: an agent routine names its skill", at)
		}
	case FilesPullRequest:
		if r.Pass != PassTemplate {
			return fmt.Errorf("%s: a pull-request routine names its pass, template", at)
		}
	default:
		return fmt.Errorf("%s: files is %q, not one of %v", at, r.Files, RoutineFiles)
	}
	if r.Skill != "" && r.Files != FilesAgent {
		return fmt.Errorf("%s: skill is an agent routine's, this one files %s", at, r.Files)
	}
	if r.Pass != "" && r.Files != FilesPullRequest {
		return fmt.Errorf("%s: pass is a pull-request routine's, this one files %s", at, r.Files)
	}
	if len(r.Schedule) == 0 {
		return fmt.Errorf("%s: no schedule", at)
	}
	for i, tr := range r.Schedule {
		set := 0
		for _, ok := range []bool{tr.Cron != "", tr.Merges != 0, tr.On != ""} {
			if ok {
				set++
			}
		}
		switch {
		case set != 1:
			return fmt.Errorf("%s: a trigger is one of cron, merges or on", at)
		case tr.Cron != "" && len(strings.Fields(tr.Cron)) != 5:
			return fmt.Errorf("%s: cron %q: five fields, minute hour day-of-month month day-of-week, in policy.yaml's time_zone", at, tr.Cron)
		case tr.Merges < 0:
			return fmt.Errorf("%s: merges %d: a count of merged cards", at, tr.Merges)
		case tr.Merges != 0 && slices.ContainsFunc(r.Schedule[:i], func(o Trigger) bool { return o.Merges != 0 }):
			return fmt.Errorf("%s: merges is given twice: a schedule counts merges once", at)
		case tr.On != "" && !slices.Contains(RoutineEvents, tr.On):
			return fmt.Errorf("%s: on is %q, not one of %v", at, tr.On, RoutineEvents)
		}
		if tr.Cron != "" {
			s, err := cronParser.Parse(tr.Cron)
			if err != nil {
				return fmt.Errorf("%s: cron %q: %v", at, tr.Cron, err)
			}
			if nextCron(s, cronEpoch).IsZero() {
				return fmt.Errorf("%s: cron %q: never fires, no date matches it", at, tr.Cron)
			}
		}
	}
	for _, rule := range []struct {
		key, v string
		values []string
	}{{"while_open", r.WhileOpen, WhileOpenValues}, {"missed", r.Missed, MissedValues}, {"tell", r.Tell, TellValues}} {
		if rule.v != "" && !slices.Contains(rule.values, rule.v) {
			return fmt.Errorf("%s: %s is %q, not one of %v", at, rule.key, rule.v, rule.values)
		}
	}
	switch {
	case r.WhileOpen == WhileOpenFold && r.Files != FilesPullRequest:
		return fmt.Errorf("%s: while_open fold is a pull-request routine's, whose pass folds into its open pull request: this one files %s, so hold or file", at, r.Files)
	case r.Files == FilesReport && r.Tell == TellQuiet:
		return fmt.Errorf("%s: a report routine with tell quiet tells nobody: its report is the poke, so tell inbox", at)
	}
	if r.Brief != "" && (path.Dir(r.Brief) != DirRoutines || path.Ext(r.Brief) != ".md") {
		return fmt.Errorf("%s: brief %q: a file %s/<name>.md", at, r.Brief, DirRoutines)
	}
	return nil
}

// Words states the schedule for the card, its triggers joined by " · ".
func (r Routine) Words() string {
	parts := make([]string, len(r.Schedule))
	for i, t := range r.Schedule {
		parts[i] = t.Words()
	}
	return strings.Join(parts, " · ")
}

// Words states a trigger in words where the pattern is a common one, else
// the expression itself.
func (t Trigger) Words() string {
	switch {
	case t.Merges == 1:
		return "every merge"
	case t.Merges > 0:
		return fmt.Sprintf("every %d merges", t.Merges)
	case t.On == OnConfigurationVersion:
		return "on each configuration version"
	case t.On == OnPushMain:
		return "on each push to main"
	}
	f := strings.Fields(t.Cron)
	if len(f) != 5 {
		return t.Cron
	}
	minute, mok := atoi(f[0])
	hour, hok := atoi(f[1])
	dom, mon, dow := f[2], f[3], f[4]
	switch {
	case mok && f[1] == "*" && dom == "*" && mon == "*" && dow == "*":
		return "hourly"
	case mok && hok && dom == "*" && mon == "*" && dow == "*":
		return fmt.Sprintf("daily %02d:%02d", hour, minute)
	case mok && hok && dom == "*" && mon == "*" && weekdays[dow] != "":
		return fmt.Sprintf("every %s %02d:%02d", weekdays[dow], hour, minute)
	case mok && hok && mon == "*" && dow == "*":
		if d, ok := atoi(dom); ok {
			return fmt.Sprintf("monthly on the %s %02d:%02d", ordinal(d), hour, minute)
		}
	}
	return t.Cron
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func ordinal(d int) string {
	switch {
	case d%10 == 1 && d != 11:
		return strconv.Itoa(d) + "st"
	case d%10 == 2 && d != 12:
		return strconv.Itoa(d) + "nd"
	case d%10 == 3 && d != 13:
		return strconv.Itoa(d) + "rd"
	}
	return strconv.Itoa(d) + "th"
}

// NextCron is the earliest next time of the routine's cron triggers after
// now, read in loc, and false when the routine has none.
func (r Routine) NextCron(now time.Time, loc *time.Location) (time.Time, bool) {
	var best time.Time
	for _, t := range r.Schedule {
		if t.Cron == "" {
			continue
		}
		s, err := cronParser.Parse(t.Cron)
		if err != nil {
			continue
		}
		if n := nextCron(s, now.In(loc)); !n.IsZero() && (best.IsZero() || n.Before(best)) {
			best = n
		}
	}
	return best, !best.IsZero()
}

// cronHorizon is how far nextCron looks: every day of the month falls on
// every day of the week within it, the 29th of February too.
const cronHorizon = 30

// nextCron is s's next time after t as Temporal fires it, zero when none
// comes within cronHorizon years. Temporal matches the day of the month
// and the day of the week both, robfig either one when both are
// restricted (no star bit), so a day that matches only one is stepped
// past: "0 3 1-7 * 0" is the first Sunday of the month.
func nextCron(s cron.Schedule, t time.Time) time.Time {
	spec, ok := s.(*cron.SpecSchedule)
	const star = 1 << 63
	if !ok || spec.Dom&star != 0 || spec.Dow&star != 0 {
		return s.Next(t)
	}
	limit := t.AddDate(cronHorizon, 0, 0)
	for n := s.Next(t); !n.IsZero() && n.Before(limit); n = s.Next(n) {
		if 1<<uint(n.Day())&spec.Dom != 0 && 1<<uint(n.Weekday())&spec.Dow != 0 {
			return n
		}
		// the rest of the day matches neither: on to its last second
		n = time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, n.Location()).Add(-time.Second)
	}
	return time.Time{}
}

// MergesEvery is the routine's count of merged cards between runs, 0 when
// no trigger counts merges.
func (r Routine) MergesEvery() int {
	for _, t := range r.Schedule {
		if t.Merges > 0 {
			return t.Merges
		}
	}
	return 0
}

// Has says whether a trigger of the routine is the event on.
func (r Routine) Has(on string) bool {
	return slices.ContainsFunc(r.Schedule, func(t Trigger) bool { return t.On == on })
}

// WhileOpenRule is while_open, hold when the file leaves it out.
func (r Routine) WhileOpenRule() string { return cmp.Or(r.WhileOpen, WhileOpenHold) }

// MissedRule is missed, file-once when the file leaves it out.
func (r Routine) MissedRule() string { return cmp.Or(r.Missed, MissedFileOnce) }

// TellRule is tell, inbox when the file leaves it out.
func (r Routine) TellRule() string { return cmp.Or(r.Tell, TellInbox) }

// Routine is project's routine name, policy.yaml's when project is "".
func (t Tree) Routine(project, name string) (Routine, bool) {
	rs := t.Document.Routines
	if project != "" {
		rs = t.Projects[project].Routines
	}
	if i := slices.IndexFunc(rs, func(r Routine) bool { return r.Name == name }); i >= 0 {
		return rs[i], true
	}
	return Routine{}, false
}

// ProjectRoutines are the routines that run for project: its own, then
// policy.yaml's template pass when the project has a template.
func (t Tree) ProjectRoutines(project string) []Routine {
	p, ok := t.Projects[project]
	if !ok {
		return nil
	}
	rs := slices.Clone(p.Routines)
	if p.Template != "" {
		for _, r := range t.Document.Routines {
			if r.Pass == PassTemplate {
				rs = append(rs, r)
			}
		}
	}
	return rs
}

// Brief is the text of the tree's file name, a routine's brief.
func (t Tree) Brief(name string) (string, bool) {
	b, ok := t.Briefs[name]
	return b, ok
}
