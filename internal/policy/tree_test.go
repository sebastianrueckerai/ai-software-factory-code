package policy

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// Parse decodes a policy.yaml body as ParseTree does.
func Parse(b []byte) (Document, error) {
	d, err := parseBody(b, kindPolicy, Document.Validate)
	if err != nil {
		err = fmt.Errorf("policy: %w", err)
	}
	return d, err
}

func fixtureTree(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := ReadDir("testdata/factory")
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestParseTreeAccepts(t *testing.T) {
	files := fixtureTree(t)
	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"policy.yaml", "projects/code.yaml", "projects/demo-app.yaml", "projects/infra.yaml", "workflow.yaml"}) {
		t.Fatalf("ReadDir keys = %v", got)
	}
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1, Steps: 200}
	if tree.Document.Limits != want {
		t.Fatalf("limits = %+v, want %+v", tree.Document.Limits, want)
	}
	if tree.Document.Bound("GateReproduction") != 30*time.Minute {
		t.Fatalf("GateReproduction bound = %s", tree.Document.Bound("GateReproduction"))
	}
	if got := slices.Sorted(maps.Keys(tree.Projects)); !slices.Equal(got, []string{"code", "demo-app", "infra"}) {
		t.Fatalf("projects = %v", got)
	}
	demo := tree.Projects["demo-app"]
	if demo.Repository != "demo-app/demo-app" || demo.MergeRule != MergeAuto || demo.Objects != ObjectsFactory || demo.Agents != AgentsCoder ||
		len(demo.Environments) != 2 || demo.Environments[0].Hostname != "demo-staging.example.org" || demo.Quota == nil || demo.Quota.Memory != "4Gi" {
		t.Fatalf("demo-app = %+v", demo)
	}
	if infra := tree.Projects["infra"]; infra.Objects != ObjectsSeed || infra.Agents != AgentsNone || infra.Quota != nil ||
		*infra.Tokens != (SeedTokens{Read: SecretConfig}) {
		t.Fatalf("infra = %+v", infra)
	}
	if code := tree.Projects["code"]; *code.Tokens != (SeedTokens{Read: SecretCIRegistrar, Actions: SecretCIRegistrar, Merge: SecretMerge}) || !code.Audit {
		t.Fatalf("code = %+v", code)
	}
	if demo.Audit || tree.Document.FactoryModel([]string{ClassCoder}, PurposeAudit) != "coding-frontier" || tree.Document.Bound("GateAudit") != 6*time.Minute {
		t.Fatalf("demo-app audit %v, audit model %q, bound %s", demo.Audit, tree.Document.FactoryModel([]string{ClassCoder}, PurposeAudit), tree.Document.Bound("GateAudit"))
	}
	var names []string
	for _, s := range tree.Workflow.Stages {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"screening", "checks", "policy", "migration", "reproduction", "review", "audit", "approval", "merge"}) {
		t.Fatalf("stages = %v", names)
	}
	if s := tree.Workflow.Stages[0]; s.Enabled || s.When != WhenOutsider || s.OnFindings != FindingsHuman {
		t.Fatalf("screening = %+v", s)
	}
	// the stored form: the tree round-trips through JSON unchanged
	b, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var back Tree
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, tree) {
		t.Fatalf("JSON round trip = %+v, %v", back, err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("stored tree does not validate: %v", err)
	}
}

func TestParseTreeRejects(t *testing.T) {
	edit := func(file, old, new string) func(map[string][]byte) {
		return func(f map[string][]byte) {
			if !strings.Contains(string(f[file]), old) {
				panic(file + " lacks " + old)
			}
			f[file] = []byte(strings.Replace(string(f[file]), old, new, 1))
		}
	}
	for name, tc := range map[string]struct {
		change func(map[string][]byte)
		want   string
	}{
		"unknown key in the workflow": {edit("workflow.yaml", "enabled: true}", "enabled: true, retries: 2}"), "workflow.yaml: yaml: unmarshal errors:\n  line 4: field retries not found"},
		"second document": {func(f map[string][]byte) {
			f["projects/infra.yaml"] = append(f["projects/infra.yaml"], "---\nagents: coder\n"...)
		}, "projects/infra.yaml: more than one YAML document"},
		"invalid on_findings":         {edit("workflow.yaml", "on_findings: fail", "on_findings: maybe"), `workflow.yaml: workflow: stage policy: on_findings "maybe", want one of [agent human fail]`},
		"invalid objects":             {edit("projects/infra.yaml", "objects: seed", "objects: flux"), `projects/infra.yaml: project infra: objects "flux": want one of [factory seed]`},
		"invalid agents":              {edit("projects/infra.yaml", "agents: none", "agents: all"), `project infra: agents "all"`},
		"stage enabled missing":       {edit("workflow.yaml", "{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent}"), "workflow.yaml: schema:"},
		"merge not last":              {edit("workflow.yaml", "  - {name: merge, on_findings: human, enabled: true}\n", "  - {name: merge, on_findings: human, enabled: true}\n  - {name: checks, on_findings: agent, enabled: true}\n"), "stage checks is listed twice"},
		"no approval":                 {edit("workflow.yaml", "  - {name: approval, on_findings: human, enabled: true}\n", ""), "stage approval is required"},
		"merge before approval":       {edit("workflow.yaml", "  - {name: approval, on_findings: human, enabled: true}\n  - {name: merge, on_findings: human, enabled: true}\n", "  - {name: merge, on_findings: human, enabled: true}\n  - {name: approval, on_findings: human, enabled: true}\n"), "stage merge must be last"},
		"no limits":                   {edit("policy.yaml", "limits:\n  sessions: 3\n  agent_time: 20m\n  supervisor_calls: 3\n  supervisor_timeout: 6m\n  stronger_after: 1\n  steps: 200\n", ""), "limits.sessions must be positive"},
		"limit missing":               {edit("policy.yaml", "  stronger_after: 1\n", ""), "policy.yaml: schema:"},
		"limit not a duration":        {edit("policy.yaml", "agent_time: 20m", "agent_time: twenty"), "policy.yaml: yaml:"},
		"supervisor timeout too long": {edit("policy.yaml", "supervisor_timeout: 6m", "supervisor_timeout: 20m"), "limits.supervisor_timeout must lie between 30s and 15m0s"},
		"no header":                   {edit("policy.yaml", "# yaml-language-server: $schema=schema/policy.schema.json\n", ""), "policy.yaml: the first line must be # yaml-language-server: $schema=schema/policy.schema.json"},
		"project header path":         {edit("projects/infra.yaml", "../schema/", "schema/"), "projects/infra.yaml: the first line must be # yaml-language-server: $schema=../schema/project.schema.json"},
		"unknown file":                {func(f map[string][]byte) { f["notes.txt"] = []byte("hi") }, `unknown file "notes.txt"`},
		"nested project":              {func(f map[string][]byte) { f["projects/x/y.yaml"] = f["projects/infra.yaml"] }, `unknown file "projects/x/y.yaml"`},
		"bad project name":            {func(f map[string][]byte) { f["projects/Infra_2.yaml"] = f["projects/infra.yaml"] }, "project Infra_2: the file name"},
		"no workflow":                 {func(f map[string][]byte) { delete(f, "workflow.yaml") }, "workflow.yaml is missing"},
		"no policy":                   {func(f map[string][]byte) { delete(f, "policy.yaml") }, "policy.yaml is missing"},
		"seed without tokens":         {edit("projects/infra.yaml", "tokens: {read: forgejo-config}\n", ""), "project infra: objects seed: tokens.read names the secret"},
		"seed coder without merge":    {edit("projects/code.yaml", ", merge: forgejo-merge", ""), "project code: objects seed with agents coder: tokens.actions and tokens.merge"},
		"seed names the projects token": {edit("projects/infra.yaml", "read: forgejo-config", "read: forgejo-projects"),
			`project infra: tokens.read "forgejo-projects": want one of [forgejo-ci-registrar forgejo-merge forgejo-config]`},
		"seed with environments": {edit("projects/infra.yaml", "agents: none\n", "agents: none\nnamespaces: [x]\nenvironments: [{namespace: x, path: deploy}]\n"),
			"project infra: objects seed: environments, quota and secrets are registration's"},
		"factory with tokens":          {edit("projects/demo-app.yaml", "agents: coder\n", "agents: coder\ntokens: {read: forgejo-config}\n"), "project demo-app: objects factory: tokens are registration's"},
		"factory not its organisation": {edit("projects/demo-app.yaml", "repository: demo-app/demo-app", "repository: demo/app"), `project demo-app: objects factory: repository "demo/app": a project is its own organisation`},
		"factory in a reserved namespace": {edit("projects/demo-app.yaml", "namespaces: [demo-staging, demo-prod]", "namespaces: [demo-staging, demo-prod, platform]"),
			`project demo-app: objects factory: namespace "platform" is the cluster's or the factory's`},
		"factory project named sf": {func(f map[string][]byte) {
			f["projects/sf.yaml"] = []byte(strings.ReplaceAll(string(f["projects/demo-app.yaml"]), "demo-app", "sf"))
			delete(f, "projects/demo-app.yaml")
		}, "project sf: objects factory: the name is its Forgejo organisation's"},
		"bad deploy path":               {edit("projects/demo-app.yaml", "path: deploy/prod", "path: ../prod"), `environment demo-prod: path "../prod"`},
		"environment elsewhere":         {edit("projects/demo-app.yaml", "{namespace: demo-prod, path", "{namespace: demo-qa, path"), `environment "demo-qa": not one of the project's namespaces`},
		"bad hostname":                  {edit("projects/demo-app.yaml", "hostname: demo-staging.example.org", "hostname: Demo_Staging"), `hostname "Demo_Staging"`},
		"bad secret":                    {edit("projects/demo-app.yaml", "secrets: [demo-write-token/token]", "secrets: [token]"), `secret "token": want secret-name/key`},
		"bad notify":                    {edit("projects/demo-app.yaml", "agents: coder\n", "agents: coder\nnotify: [a b]\n"), `notify "a b": not a Forgejo user name`},
		"bad envelope":                  {edit("projects/demo-app.yaml", "envelope: {monthly_eur: 5", "envelope: {monthly_eur: 0"), "project demo-app: envelope.monthly_eur must be positive"},
		"bad standing":                  {edit("projects/demo-app.yaml", "standing: {monthly_eur: 3", "standing: {monthly_eur: -3"), "project demo-app: standing.monthly_eur must not be negative"},
		"one repository, two files":     {func(f map[string][]byte) { f["projects/infra2.yaml"] = f["projects/infra.yaml"] }, "project infra2: repository sf/infra is project infra's"},
		"one namespace, two files":      {edit("projects/infra.yaml", "agents: none\n", "agents: none\nnamespaces: [demo-prod]\n"), "project infra: namespace demo-prod is project demo-app's"},
		"audit switched on, not listed": {edit("workflow.yaml", "  - {name: audit, on_findings: human, enabled: true}\n", ""), "project code: audit: the workflow lists no audit stage"},
		"empty project": {func(f map[string][]byte) {
			f["projects/infra.yaml"] = []byte("# yaml-language-server: $schema=../schema/project.schema.json\n")
		}, "projects/infra.yaml: empty document"},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			tc.change(files)
			_, err := ParseTree(files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Limits' durations are strings in JSON, as in YAML: the stored tree, GET
// /policy and the console show 20m0s, not nanoseconds.
func TestLimitsJSON(t *testing.T) {
	l := Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1, Steps: 150}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"sessions":3,"agent_time":"20m0s","supervisor_calls":3,"supervisor_timeout":"6m0s","stronger_after":1,"steps":150}`; string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
	var back Limits
	if err := json.Unmarshal(b, &back); err != nil || back != l {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"agent_time":"soon"}`), &back); err == nil {
		t.Fatal("a bad duration decoded")
	}
}

// The tree holds regular files only: a symbolic link is refused, as the
// loader refuses one in Forgejo, not skipped.
func TestReadDirRefusesSymbolicLinks(t *testing.T) {
	dir := t.TempDir()
	for name, b := range fixtureTree(t) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadDir(dir); err != nil {
		t.Fatalf("regular tree: %v", err)
	}
	if err := os.Symlink("demo-app.yaml", filepath.Join(dir, "projects", "demo.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDir(dir); err == nil || !strings.Contains(err.Error(), "projects/demo.yaml: the tree holds regular files only") {
		t.Fatalf("ReadDir with a symbolic link = %v", err)
	}
}

// Every error of a tree is reported, not only the first.
func TestParseTreeReportsEveryFile(t *testing.T) {
	files := fixtureTree(t)
	files["workflow.yaml"] = []byte(strings.Replace(string(files["workflow.yaml"]), "on_findings: fail", "on_findings: maybe", 1))
	files["projects/infra.yaml"] = []byte(strings.Replace(string(files["projects/infra.yaml"]), "agents: none", "agents: all", 1))
	_, err := ParseTree(files)
	if err == nil || !strings.Contains(err.Error(), "workflow.yaml:") || !strings.Contains(err.Error(), "projects/infra.yaml:") {
		t.Fatalf("ParseTree error = %v, want both files named", err)
	}
}

func resolvedSchema(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(schemas[name], &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

func yamlInstance(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(j, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every fixture file validates against the schema of its kind, as an
// editor would check it, and a broken one does not.
func TestSchemaRoundTrip(t *testing.T) {
	files := fixtureTree(t)
	kindOf := func(file string) string {
		if path.Dir(file) == "projects" {
			return "project.schema.json"
		}
		return strings.TrimSuffix(file, ".yaml") + ".schema.json"
	}
	for file, b := range files {
		if err := resolvedSchema(t, kindOf(file)).Validate(yamlInstance(t, b)); err != nil {
			t.Errorf("%s against %s: %v", file, kindOf(file), err)
		}
	}
	for name, tc := range map[string]struct{ file, old, new string }{
		"invalid enum":         {"workflow.yaml", "on_findings: fail", "on_findings: maybe"},
		"invalid stage name":   {"workflow.yaml", "name: review", "name: lint"},
		"invalid predicate":    {"workflow.yaml", "when: outsider", "when: insider"},
		"unknown key":          {"projects/infra.yaml", "agents: none", "agents: none\nowner: me"},
		"required key missing": {"projects/infra.yaml", "objects: seed\n", ""},
		"wrong type":           {"policy.yaml", "sessions: 3", "sessions: three"},
		"not a duration":       {"policy.yaml", "agent_time: 20m", "agent_time: 20"},
		"unknown tool":         {"policy.yaml", "escalate_to_human: {}", "firecrawl: {}"},
		"unknown bound":        {"policy.yaml", "GateReproduction: 1800", "Lunch: 1800"},
		"invalid outcome":      {"policy.yaml", "outcome: auto", "outcome: maybe"},
		"invalid merge rule":   {"projects/demo-app.yaml", "merge_rule: auto", "merge_rule: sometimes"},
		"wrong version":        {"policy.yaml", "version: 1", "version: 2"},
	} {
		t.Run(name, func(t *testing.T) {
			b := strings.Replace(string(files[tc.file]), tc.old, tc.new, 1)
			if b == string(files[tc.file]) {
				t.Fatalf("%s lacks %q", tc.file, tc.old)
			}
			if err := resolvedSchema(t, kindOf(tc.file)).Validate(yamlInstance(t, []byte(b))); err == nil {
				t.Fatalf("%s: the broken file validates", tc.file)
			}
		})
	}
}

// The schemas carry the enumerations and a description on every
// property, from the Go doc comments.
func TestSchemasDescribeTheTypes(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(schemas)); !slices.Equal(got, []string{"policy.schema.json", "project.schema.json", "workflow.schema.json"}) {
		t.Fatalf("schema files = %v", got)
	}
	var walk func(where string, s *jsonschema.Schema)
	walk = func(where string, s *jsonschema.Schema) {
		for name, p := range s.Properties {
			if p.Description == "" {
				t.Errorf("%s.%s has no description", where, name)
			}
			walk(where+"."+name, p)
		}
		if s.Items != nil {
			walk(where+"[]", s.Items)
		}
		if s.AdditionalProperties != nil {
			walk(where+"{}", s.AdditionalProperties)
		}
	}
	root := map[string]*jsonschema.Schema{}
	for name, b := range schemas {
		var s jsonschema.Schema
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		if s.Description == "" || s.Schema != "http://json-schema.org/draft-07/schema#" {
			t.Errorf("%s: description %q, $schema %q", name, s.Description, s.Schema)
		}
		walk(name, &s)
		root[name] = &s
	}
	stage := root["workflow.schema.json"].Properties["stages"].Items
	if got := stage.Properties["on_findings"].Enum; !reflect.DeepEqual(got, []any{"agent", "human", "fail"}) {
		t.Errorf("on_findings enum = %v", got)
	}
	if got := stage.Properties["name"].Enum; len(got) != 9 {
		t.Errorf("stage name enum = %v", got)
	}
	if !slices.Equal(stage.Required, []string{"name", "on_findings", "enabled"}) {
		t.Errorf("stage required = %v", stage.Required)
	}
	project := root["project.schema.json"]
	if got := project.Properties["agents"].Enum; !reflect.DeepEqual(got, []any{"coder", "none"}) {
		t.Errorf("agents enum = %v", got)
	}
	if got := project.Properties["objects"].Enum; !reflect.DeepEqual(got, []any{"factory", "seed"}) {
		t.Errorf("objects enum = %v", got)
	}
	pol := root["policy.schema.json"]
	tools := pol.Properties["classes"].AdditionalProperties.Properties["tools"]
	if tools.PropertyNames == nil || len(tools.PropertyNames.Enum) != len(PaidTools)+len(FreeTools) {
		t.Errorf("tools property names = %+v", tools.PropertyNames)
	}
	if got := pol.Properties["limits"].Properties["agent_time"]; got.Type != "string" || got.Pattern == "" {
		t.Errorf("agent_time = type %q pattern %q, want a duration string", got.Type, got.Pattern)
	}
	if !strings.Contains(pol.Properties["limits"].Properties["sessions"].Description, "round cap") {
		t.Errorf("sessions description = %q, want the field comment", pol.Properties["limits"].Properties["sessions"].Description)
	}
}

func TestCheckSchemas(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	committed := func() map[string][]byte {
		f := fixtureTree(t)
		for name, b := range schemas {
			f["schema/"+name] = b
		}
		return f
	}
	if err := CheckSchemas(committed()); err != nil {
		t.Fatalf("generated schemas: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(map[string][]byte)
		want   string
	}{
		"differs": {func(f map[string][]byte) {
			f["schema/policy.schema.json"] = append(f["schema/policy.schema.json"], ' ')
		}, "schema/policy.schema.json differs from factoryd schema"},
		"missing": {func(f map[string][]byte) { delete(f, "schema/workflow.schema.json") }, "schema/workflow.schema.json is missing"},
		"extra":   {func(f map[string][]byte) { f["schema/old.schema.json"] = []byte("{}") }, "schema/old.schema.json is not a schema factoryd writes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := committed()
			tc.change(f)
			if err := CheckSchemas(f); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckSchemas = %v, want %q", err, tc.want)
			}
		})
	}
	// ParseTree ignores the schema directory
	if _, err := ParseTree(committed()); err != nil {
		t.Fatalf("ParseTree with schema/: %v", err)
	}
}
