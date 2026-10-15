package policy

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestRoutineWords(t *testing.T) {
	cases := []struct {
		in   Routine
		want string
	}{
		{Routine{Schedule: []Trigger{{Cron: "0 6 * * 1"}}}, "every Monday 06:00"},
		{Routine{Schedule: []Trigger{{Cron: "30 2 * * *"}}}, "daily 02:30"},
		{Routine{Schedule: []Trigger{{Cron: "3 * * * *"}}}, "hourly"},
		{Routine{Schedule: []Trigger{{Cron: "0 3 1 * *"}}}, "monthly on the 1st 03:00"},
		{Routine{Schedule: []Trigger{{Cron: "0 3 22 * *"}}}, "monthly on the 22nd 03:00"},
		{Routine{Schedule: []Trigger{{Cron: "0 3 13 * *"}}}, "monthly on the 13th 03:00"},
		{Routine{Schedule: []Trigger{{Cron: "0 6 */2 * *"}}}, "0 6 */2 * *"},
		{Routine{Schedule: []Trigger{{Cron: "0 6 * * 1-5"}}}, "0 6 * * 1-5"},
		{Routine{Schedule: []Trigger{{Merges: 20}}}, "every 20 merges"},
		{Routine{Schedule: []Trigger{{Merges: 1}}}, "every merge"},
		{Routine{Schedule: []Trigger{{On: OnConfigurationVersion}}}, "on each configuration version"},
		{Routine{Schedule: []Trigger{{On: OnPushMain}}}, "on each push to main"},
		{Routine{Schedule: []Trigger{{Cron: "0 * * * *"}, {On: OnConfigurationVersion}}}, "hourly · on each configuration version"},
	}
	for _, c := range cases {
		if got := c.in.Words(); got != c.want {
			t.Errorf("%v: got %q, want %q", c.in.Schedule, got, c.want)
		}
	}
}

func TestRoutineRefusesBadCron(t *testing.T) {
	for _, cron := range []string{"0 6 * * 8", "every monday", "0 6 * *", "", "TZ=UTC 0 6 * * 1", "@daily", "0 0 30 2 *"} {
		r := Routine{Name: "weekly-bump", Title: "t", Files: FilesHuman, Schedule: []Trigger{{Cron: cron}}}
		err := r.Validate("projects/x.yaml")
		if err == nil || !strings.Contains(err.Error(), "weekly-bump") || !strings.Contains(err.Error(), "cron") {
			t.Errorf("cron %q: got %v", cron, err)
		}
	}
}

func TestRoutineValidate(t *testing.T) {
	ok := Routine{Name: "audit", Title: "Audit", Files: FilesAgent, Skill: "audit", Schedule: []Trigger{{Merges: 20}}}
	if err := ok.Validate("p"); err != nil {
		t.Fatal(err)
	}
	leap := Routine{Name: "leap", Title: "Leap day", Files: FilesHuman, Schedule: []Trigger{{Cron: "0 0 29 2 *"}}}
	if err := leap.Validate("p"); err != nil {
		t.Fatalf("a pattern that fires every fourth year: %v", err)
	}
	merges := []Trigger{{Merges: 1}}
	for _, c := range []struct {
		r    Routine
		want string
	}{
		{Routine{Name: "Bad Name", Title: "t", Files: FilesHuman, Schedule: merges}, `routine "Bad Name": the name: lowercase letters, digits and dashes`},
		{Routine{Name: "a", Title: "t", Files: "weekly", Schedule: merges}, `files is "weekly", not one of [human agent pull-request report]`},
		{Routine{Name: "a", Title: "t", Files: FilesAgent, Schedule: merges}, "an agent routine names its skill"},
		{Routine{Name: "a", Title: "t", Files: FilesPullRequest, Schedule: merges}, "a pull-request routine names its pass, template"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman}, "no schedule"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: []Trigger{{Merges: 1, Cron: "0 6 * * 1"}}}, "a trigger is one of cron, merges or on"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: []Trigger{{}}}, "a trigger is one of cron, merges or on"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: []Trigger{{On: "full-moon"}}}, `on is "full-moon", not one of [configuration-version push-main]`},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: merges, WhileOpen: "wait"}, `while_open is "wait", not one of [hold fold file]`},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: merges, Missed: "later"}, `missed is "later", not one of [file-once skip]`},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: merges, Tell: "everyone"}, `tell is "everyone", not one of [inbox quiet]`},
		{Routine{Name: "a", Files: FilesHuman, Schedule: merges}, "no title"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: []Trigger{{Merges: -3}}}, "merges -3: a count of merged cards"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: []Trigger{{Merges: 20}, {Cron: "0 6 * * 1"}, {Merges: 5}}}, "merges is given twice: a schedule counts merges once"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: []Trigger{{Cron: "0 6 * * 1"}, {Cron: "0 0 30 2 *"}}}, `cron "0 0 30 2 *": never fires`},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: merges, Skill: "audit"}, "skill is an agent routine's, this one files human"},
		{Routine{Name: "a", Title: "t", Files: FilesAgent, Skill: "audit", Schedule: merges, Pass: PassTemplate}, "pass is a pull-request routine's, this one files agent"},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: merges, Brief: "docs/a.md"}, `brief "docs/a.md": a file routines/<name>.md`},
		{Routine{Name: "a", Title: "t", Files: FilesHuman, Schedule: merges, Brief: "routines/a.txt"}, `brief "routines/a.txt": a file routines/<name>.md`},
	} {
		if err := c.r.Validate("p"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want %q", c.r, err, c.want)
		}
	}
}

func TestRoutineNextCron(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	r := Routine{Schedule: []Trigger{{Cron: "0 6 * * 1"}}}
	now := time.Date(2026, 10, 12, 5, 40, 0, 0, berlin) // a Monday
	next, ok := r.NextCron(now, berlin)
	if !ok || !next.Equal(time.Date(2026, 10, 12, 6, 0, 0, 0, berlin)) {
		t.Fatalf("got %v %v", next, ok)
	}
	// the earliest of two patterns, read in the zone given, not now's
	r = Routine{Schedule: []Trigger{{Cron: "0 6 * * 1"}, {Cron: "30 7 * * *"}, {Merges: 4}}}
	next, ok = r.NextCron(time.Date(2026, 10, 12, 4, 10, 0, 0, time.UTC), berlin)
	if !ok || !next.Equal(time.Date(2026, 10, 12, 7, 30, 0, 0, berlin)) {
		t.Fatalf("two patterns: got %v %v", next, ok)
	}
	// a pattern that never fires leaves the others their next time
	r = Routine{Schedule: []Trigger{{Cron: "0 6 * * 1"}, {Cron: "0 0 30 2 *"}}}
	next, ok = r.NextCron(now, berlin)
	if !ok || !next.Equal(time.Date(2026, 10, 12, 6, 0, 0, 0, berlin)) {
		t.Fatalf("beside a pattern that never fires: got %v %v", next, ok)
	}
	if _, ok := (Routine{Schedule: []Trigger{{Cron: "0 0 30 2 *"}}}).NextCron(now, berlin); ok {
		t.Fatal("a pattern that never fires has a next time")
	}
	if _, ok := (Routine{Schedule: []Trigger{{Merges: 3}}}).NextCron(now, berlin); ok {
		t.Fatal("a merges-only routine has no next time")
	}
}

func TestRoutineRules(t *testing.T) {
	var r Routine
	if r.WhileOpenRule() != WhileOpenHold || r.MissedRule() != MissedFileOnce || r.TellRule() != TellInbox {
		t.Fatalf("defaults: %s %s %s", r.WhileOpenRule(), r.MissedRule(), r.TellRule())
	}
	r = Routine{WhileOpen: WhileOpenFold, Missed: MissedSkip, Tell: TellQuiet, Schedule: []Trigger{{Cron: "0 6 * * 1"}, {Merges: 7}, {On: OnPushMain}}}
	if r.WhileOpenRule() != WhileOpenFold || r.MissedRule() != MissedSkip || r.TellRule() != TellQuiet {
		t.Fatalf("set: %s %s %s", r.WhileOpenRule(), r.MissedRule(), r.TellRule())
	}
	if r.MergesEvery() != 7 || !r.Has(OnPushMain) || r.Has(OnConfigurationVersion) {
		t.Fatalf("merges every %d, push-main %v, configuration-version %v", r.MergesEvery(), r.Has(OnPushMain), r.Has(OnConfigurationVersion))
	}
	if (Routine{Schedule: []Trigger{{Cron: "0 6 * * 1"}}}).MergesEvery() != 0 {
		t.Fatal("a cron-only routine counts merges")
	}
}

func TestTreeParsesRoutinesAndBriefs(t *testing.T) {
	tr, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	// the code project has its own routine and no template
	rs := tr.ProjectRoutines("ai-software-factory-code")
	if len(rs) != 1 || rs[0].Name != "weekly-bump" || rs[0].Words() != "every Monday 06:00" || rs[0].Brief != "routines/weekly-bump.md" {
		t.Fatalf("code: got %+v", rs)
	}
	// linkshort has a template, so policy.yaml's template pass runs for it
	if rs := tr.ProjectRoutines("linkshort"); len(rs) != 1 || rs[0].Name != "template-pass" || rs[0].Words() != "hourly · on each configuration version" {
		t.Fatalf("linkshort: got %+v", rs)
	}
	// a project without a template gets no template-pass
	if rs := tr.ProjectRoutines("demo-app"); len(rs) != 0 {
		t.Fatalf("demo-app has %d routines", len(rs))
	}
	if rs := tr.ProjectRoutines("no-such-project"); len(rs) != 0 {
		t.Fatalf("an unknown project has %d routines", len(rs))
	}
	// the project's own first, then the factory's
	p := tr.Projects["linkshort"]
	p.Routines = []Routine{{Name: "weekly-review"}}
	tr.Projects["linkshort"] = p
	if rs := tr.ProjectRoutines("linkshort"); len(rs) != 2 || rs[0].Name != "weekly-review" || rs[1].Name != "template-pass" {
		t.Fatalf("linkshort with its own: got %+v", rs)
	}
	p.Routines = nil
	tr.Projects["linkshort"] = p

	if r, ok := tr.Routine("ai-software-factory-code", "weekly-bump"); !ok || r.Missed != MissedFileOnce {
		t.Fatalf("Routine(code, weekly-bump) = %+v %v", r, ok)
	}
	if r, ok := tr.Routine("", "template-pass"); !ok || r.Pass != PassTemplate || r.WhileOpenRule() != WhileOpenFold || r.TellRule() != TellQuiet {
		t.Fatalf("Routine(\"\", template-pass) = %+v %v", r, ok)
	}
	if _, ok := tr.Routine("linkshort", "template-pass"); ok {
		t.Fatal("a project's own routines hold policy.yaml's")
	}
	if _, ok := tr.Routine("ai-software-factory-code", "template-pass"); ok {
		t.Fatal("found a routine the project does not have")
	}
	brief, ok := tr.Brief("routines/weekly-bump.md")
	if !ok || !strings.HasPrefix(brief, "The factory pins every outside input") || strings.Contains(brief, "filed by factoryd") {
		t.Fatalf("the brief: %v %q", ok, brief)
	}
	if _, ok := tr.Brief("routines/none.md"); ok {
		t.Fatal("a brief that is not in the tree")
	}
	if loc, err := tr.Document.Location(); err != nil || loc.String() != "Europe/Berlin" {
		t.Fatalf("location %v %v", loc, err)
	}
	if loc, err := (Document{}).Location(); err != nil || loc != time.UTC {
		t.Fatalf("no time_zone: location %v %v", loc, err)
	}
	// policy_versions stores the tree as JSON: the briefs come back with it
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var back Tree
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got, ok := back.Brief("routines/weekly-bump.md"); !ok || got != brief {
		t.Fatal("the stored tree lost the brief")
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("the stored tree: %v", err)
	}
}

func TestTreeRefusesRoutines(t *testing.T) {
	const code, policyFile = "projects/ai-software-factory-code.yaml", "policy.yaml"
	for name, tc := range map[string]struct{ file, old, new, want string }{
		"brief not in the tree": {code, "brief: routines/weekly-bump.md", "brief: routines/monthly.md",
			`projects/ai-software-factory-code.yaml: routine "weekly-bump": brief routines/monthly.md: no such file (briefs: routines/weekly-bump.md)`},
		"two routines of one name": {code, "routines:\n", "routines:\n  - {name: weekly-bump, title: t, schedule: [{merges: 3}], files: human}\n",
			`projects/ai-software-factory-code.yaml: project ai-software-factory-code: routine "weekly-bump": given twice`},
		"a bad routine": {code, `cron: "0 6 * * 1"`, `cron: "0 6 * * 9"`,
			`projects/ai-software-factory-code.yaml: project ai-software-factory-code: routine "weekly-bump": cron "0 6 * * 9"`},
		"an unknown key": {code, "tell: inbox", "poke: inbox", `field poke not found`},
		"an unknown time zone": {policyFile, "time_zone: Europe/Berlin", "time_zone: Mars/Olympus",
			`policy.yaml: policy: time_zone "Mars/Olympus"`},
		"the host's time zone": {policyFile, "time_zone: Europe/Berlin", "time_zone: Local",
			`policy.yaml: policy: time_zone "Local": Local is the host's zone`},
		"a factory routine that is no pass": {policyFile, "files: pull-request\n    pass: template", "files: human",
			`policy.yaml: policy: routine "template-pass": a routine of policy.yaml is a pass over the projects, files pull-request`},
		"two factory routines of one name": {policyFile, "routines:\n", "routines:\n  - {name: template-pass, title: t, schedule: [{merges: 3}], files: pull-request, pass: template}\n",
			`policy.yaml: policy: routine "template-pass": given twice`},
		"a project routine named as the factory's that runs for it": {"projects/linkshort.yaml", "namespaces: [linkshort-staging]",
			"namespaces: [linkshort-staging]\nroutines:\n  - {name: template-pass, title: t, schedule: [{merges: 3}], files: human}",
			`projects/linkshort.yaml: routine "template-pass": policy.yaml's routine of that name runs for this project too`},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			f := string(files[tc.file])
			if !strings.Contains(f, tc.old) {
				t.Fatalf("%s lacks %q", tc.file, tc.old)
			}
			files[tc.file] = []byte(strings.Replace(f, tc.old, tc.new, 1))
			if _, err := ParseTree(files); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	for name, tc := range map[string]struct {
		file string
		body []byte
		want string
	}{
		"a brief that is not text":  {"routines/broken.md", []byte{0xff, 0xfe}, "routines/broken.md: not UTF-8 text"},
		"a file that is no brief":   {"routines/notes.txt", []byte("x"), `unknown file "routines/notes.txt"`},
		"a brief in a subdirectory": {"routines/a/b.md", []byte("x"), `unknown file "routines/a/b.md"`},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			files[tc.file] = tc.body
			if _, err := ParseTree(files); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// a tree read back from Postgres is held to the same links
	tr, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	delete(tr.Briefs, "routines/weekly-bump.md")
	want := `projects/ai-software-factory-code.yaml: routine "weekly-bump": brief routines/weekly-bump.md: no such file`
	if err := tr.Validate(); err == nil || !slices.Contains(strings.Split(err.Error(), "\n"), want) {
		t.Fatalf("Validate = %v, want the line %q", err, want)
	}
}

// A project file's and policy.yaml's routines block in the schema editors
// read: the enumerations and the required keys.
func TestRoutineSchema(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"project.schema.json", "policy.schema.json"} {
		var s jsonschema.Schema
		if err := json.Unmarshal(schemas[file], &s); err != nil {
			t.Fatal(err)
		}
		r := s.Properties["routines"].Items
		for key, want := range map[string][]any{
			"files":      {"human", "agent", "pull-request", "report"},
			"pass":       {"template"},
			"while_open": {"hold", "fold", "file"},
			"missed":     {"file-once", "skip"},
			"tell":       {"inbox", "quiet"},
		} {
			if got := r.Properties[key].Enum; !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %s enum = %v, want %v", file, key, got, want)
			}
		}
		if got := r.Properties["schedule"].Items.Properties["on"].Enum; !reflect.DeepEqual(got, []any{"configuration-version", "push-main"}) {
			t.Errorf("%s: on enum = %v", file, got)
		}
		if !slices.Equal(r.Required, []string{"name", "title", "schedule", "files"}) {
			t.Errorf("%s: routine required = %v", file, r.Required)
		}
	}
}
