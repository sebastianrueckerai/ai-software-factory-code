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
	files, err := ReadDir("testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestParseTreeAccepts(t *testing.T) {
	files := fixtureTree(t)
	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"policy.yaml", "processes/maintenance.yaml", "processes/spec.yaml", "projects/ai-software-factory-code.yaml", "projects/ai-software-factory-infra.yaml", "projects/demo-app.yaml", "projects/linkshort.yaml", "routines/weekly-bump.md",
		"templates/ts-go/.forgejo/issue_template/bug.yaml", "templates/ts-go/.forgejo/issue_template/improvement.yaml", "templates/ts-go/.forgejo/roll.sh",
		"templates/ts-go/.forgejo/workflows/ci.yml", "templates/ts-go/.forgejo/workflows/pr.yml", "templates/ts-go/.gitignore", "templates/ts-go/AGENTS.md",
		"templates/ts-go/deploy/base/kustomization.yaml", "templates/ts-go/deploy/staging/kustomization.yaml", "templates/ts-go/factory/images.yaml"}) {
		t.Fatalf("ReadDir keys = %v", got)
	}
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1, Steps: 200, NudgeAfter: 3, Nudges: 1}
	if tree.Document.Limits != want {
		t.Fatalf("limits = %+v, want %+v", tree.Document.Limits, want)
	}
	if tree.Document.Bound("GateReproduction") != 30*time.Minute {
		t.Fatalf("GateReproduction bound = %s", tree.Document.Bound("GateReproduction"))
	}
	if got := slices.Sorted(maps.Keys(tree.Projects)); !slices.Equal(got, []string{"ai-software-factory-code", "ai-software-factory-infra", "demo-app", "linkshort"}) {
		t.Fatalf("projects = %v", got)
	}
	demo := tree.Projects["demo-app"]
	if demo.Repository != "demo-app/demo-app" || demo.MergeRule != MergeAuto || demo.Objects != ObjectsFactory || demo.Agents != AgentsCoder ||
		len(demo.Environments) != 2 || demo.Environments[0].Hostname != "demo-staging.example.org" || demo.Quota == nil || demo.Quota.Memory != "4Gi" {
		t.Fatalf("demo-app = %+v", demo)
	}
	if infra := tree.Projects["ai-software-factory-infra"]; infra.Objects != ObjectsSeed || infra.Agents != AgentsNone || infra.Quota != nil ||
		*infra.Tokens != (SeedTokens{Read: SecretConfig, Chat: SecretChat}) {
		t.Fatalf("infra = %+v", infra)
	}
	if code := tree.Projects["ai-software-factory-code"]; *code.Tokens != (SeedTokens{Read: SecretCIRegistrar, Actions: SecretCIRegistrar, Merge: SecretMerge, Gate: SecretGate, Chat: SecretChat}) || !code.Audit {
		t.Fatalf("code = %+v", code)
	}
	if demo.Audit || tree.Document.FactoryModel([]string{ClassCoder}, PurposeAudit) != "big" || tree.Document.Bound("GateAudit") != 20*time.Minute {
		t.Fatalf("demo-app audit %v, audit model %q, bound %s", demo.Audit, tree.Document.FactoryModel([]string{ClassCoder}, PurposeAudit), tree.Document.Bound("GateAudit"))
	}
	if demo.WorkInProgress != 2 || tree.WorkInProgressOf("demo-app/demo-app") != 2 || tree.WorkInProgressOf("no-such/repo") != DefaultWorkInProgress {
		t.Fatalf("work_in_progress: demo-app %d, of demo-app %d, of none %d (default %d)",
			demo.WorkInProgress, tree.WorkInProgressOf("demo-app/demo-app"), tree.WorkInProgressOf("no-such/repo"), DefaultWorkInProgress)
	}
	var names []string
	for _, s := range tree.Processes["maintenance"].Stages {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"screening", "checks", "policy", "migration", "reproduction", "review", "audit", "approval", "merge"}) {
		t.Fatalf("stages = %v", names)
	}
	if s := tree.Processes["maintenance"].Stages[0]; !s.Enabled || s.When != WhenOutsider || s.OnFindings != FindingsHuman {
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
		"second document": {func(f map[string][]byte) {
			f["projects/ai-software-factory-infra.yaml"] = append(f["projects/ai-software-factory-infra.yaml"], "---\nagents: coder\n"...)
		}, "projects/ai-software-factory-infra.yaml: more than one YAML document"},
		"invalid objects":       {edit("projects/ai-software-factory-infra.yaml", "objects: seed", "objects: flux"), `projects/ai-software-factory-infra.yaml: project ai-software-factory-infra: objects "flux": want one of [factory seed]`},
		"invalid agents":        {edit("projects/ai-software-factory-infra.yaml", "agents: none", "agents: all"), `project ai-software-factory-infra: agents "all"`},
		"stage enabled missing": {edit("processes/spec.yaml", "{name: review, on_findings: agent, enabled: true}", "{name: review, on_findings: agent}"), "processes/spec.yaml: schema:"},
		"no limits":             {edit("policy.yaml", "limits:\n  sessions: 3\n  agent_time: 20m\n  supervisor_calls: 3\n  supervisor_timeout: 6m\n  stronger_after: 1\n  steps: 200\n  nudge_after: 3\n  nudges: 1\n", ""), "limits.sessions must be positive"},
		"limit missing":         {edit("policy.yaml", "  stronger_after: 1\n", ""), "policy.yaml: schema:"},
		"an alias without an output limit": {edit("policy.yaml", "big: 24576, ", ""),
			"policy.yaml: prices.max_output_tokens: big has no output limit; every alias a class or the chat calls needs one"},
		"the small class without an output limit": {edit("policy.yaml", "small: 24576, ", ""),
			"policy.yaml: prices.max_output_tokens: small has no output limit; every alias a class or the chat calls needs one"},
		"a premium chat without its output limit": {edit("policy.yaml", "budget:\n", "chat: {model: premium}\nbudget:\n"),
			"policy.yaml: prices.max_output_tokens: premium has no output limit; every alias a class or the chat calls needs one"},
		"a negative premium cap": {edit("policy.yaml", "  month_eur: 50\n", "  month_eur: 50\n  premium_month_eur: -1\n"),
			"policy: budget.premium_month_eur must not be negative (0: premium has no cap of its own)"},
		"a session model on a code class": {edit("policy.yaml", "    session: code\n", "    session: code\n    session_model: big\n"),
			`policy.yaml: classes.coder: session_model "big": a code session runs on factoryd's ladder, only a plan or report class names its model`},
		"a session model the class may not call": {edit("policy.yaml", "    session: plan\n", "    session: plan\n    session_model: premium\n"),
			`policy.yaml: classes.planner: session_model "premium" is not one of its models [small big]`},
		"limit not a duration":        {edit("policy.yaml", "agent_time: 20m", "agent_time: twenty"), "policy.yaml: yaml:"},
		"supervisor timeout too long": {edit("policy.yaml", "supervisor_timeout: 6m", "supervisor_timeout: 20m"), "limits.supervisor_timeout must lie between 30s and 15m0s"},
		"no header":                   {edit("policy.yaml", "# yaml-language-server: $schema=schema/policy.schema.json\n", ""), "policy.yaml: the first line must be # yaml-language-server: $schema=schema/policy.schema.json"},
		"project header path":         {edit("projects/ai-software-factory-infra.yaml", "../schema/", "schema/"), "projects/ai-software-factory-infra.yaml: the first line must be # yaml-language-server: $schema=../schema/project.schema.json"},
		"unknown file":                {func(f map[string][]byte) { f["notes.txt"] = []byte("hi") }, `unknown file "notes.txt"`},
		"nested project":              {func(f map[string][]byte) { f["projects/x/y.yaml"] = f["projects/ai-software-factory-infra.yaml"] }, `unknown file "projects/x/y.yaml"`},
		"bad project name":            {func(f map[string][]byte) { f["projects/Infra_2.yaml"] = f["projects/ai-software-factory-infra.yaml"] }, "project Infra_2: the file name"},
		"no policy":                   {func(f map[string][]byte) { delete(f, "policy.yaml") }, "policy.yaml is missing"},
		"seed without tokens":         {edit("projects/ai-software-factory-infra.yaml", "tokens: {read: forgejo-config, chat: forgejo-chat}\n", ""), "project ai-software-factory-infra: objects seed: tokens.read names the secret"},
		"seed coder without merge":    {edit("projects/ai-software-factory-code.yaml", ", merge: forgejo-merge", ""), "project ai-software-factory-code: objects seed with agents coder: tokens.actions and tokens.merge"},
		"seed names the projects token": {edit("projects/ai-software-factory-infra.yaml", "read: forgejo-config", "read: forgejo-projects"),
			`project ai-software-factory-infra: tokens.read "forgejo-projects": want one of [forgejo-ci-registrar forgejo-merge forgejo-config]`},
		"seed with environments": {edit("projects/ai-software-factory-infra.yaml", "agents: none\n", "agents: none\nnamespaces: [x]\nenvironments: [{namespace: x, path: deploy}]\n"),
			"project ai-software-factory-infra: objects seed: environments, quota and secrets are registration's"},
		"factory with tokens":          {edit("projects/demo-app.yaml", "agents: coder\n", "agents: coder\ntokens: {read: forgejo-config}\n"), "project demo-app: objects factory: tokens are registration's"},
		"negative work_in_progress":    {edit("projects/demo-app.yaml", "work_in_progress: 2", "work_in_progress: -1"), "project demo-app: work_in_progress -1: a count of cards"},
		"factory not its organisation": {edit("projects/demo-app.yaml", "repository: demo-app/demo-app", "repository: demo/app"), `project demo-app: objects factory: repository "demo/app": a project is its own organisation`},
		"factory in a reserved namespace": {edit("projects/demo-app.yaml", "namespaces: [demo-staging, demo-prod]", "namespaces: [demo-staging, demo-prod, platform]"),
			`project demo-app: objects factory: namespace "platform" is the cluster's or the factory's`},
		"factory project named sf": {func(f map[string][]byte) {
			f["projects/sf.yaml"] = []byte(strings.ReplaceAll(string(f["projects/demo-app.yaml"]), "demo-app", "sf"))
			delete(f, "projects/demo-app.yaml")
		}, "project sf: objects factory: the name is its Forgejo organisation's"},
		"bad deploy path":           {edit("projects/demo-app.yaml", "path: deploy/prod", "path: ../prod"), `environment demo-prod: path "../prod"`},
		"environment elsewhere":     {edit("projects/demo-app.yaml", "{namespace: demo-prod, path", "{namespace: demo-qa, path"), `environment "demo-qa": not one of the project's namespaces`},
		"bad hostname":              {edit("projects/demo-app.yaml", "hostname: demo-staging.example.org", "hostname: Demo_Staging"), `hostname "Demo_Staging"`},
		"bad secret":                {edit("projects/demo-app.yaml", "secrets: [demo-write-token/token]", "secrets: [token]"), `secret "token": want secret-name/key`},
		"bad notify":                {edit("projects/demo-app.yaml", "agents: coder\n", "agents: coder\nnotify: [a b]\n"), `notify "a b": not a Forgejo user name`},
		"bad envelope":              {edit("projects/demo-app.yaml", "envelope: {monthly_eur: 5", "envelope: {monthly_eur: 0"), "project demo-app: envelope.monthly_eur must be positive"},
		"bad standing":              {edit("projects/demo-app.yaml", "standing: {monthly_eur: 3", "standing: {monthly_eur: -3"), "project demo-app: standing.monthly_eur must not be negative"},
		"one repository, two files": {func(f map[string][]byte) { f["projects/infra2.yaml"] = f["projects/ai-software-factory-infra.yaml"] }, "project infra2: repository sf/ai-software-factory-infra is project ai-software-factory-infra's"},
		"one namespace, two files":  {edit("projects/ai-software-factory-infra.yaml", "agents: none\n", "agents: none\nnamespaces: [demo-prod]\n"), "project demo-app: namespace demo-prod is project ai-software-factory-infra's"},
		"empty project": {func(f map[string][]byte) {
			f["projects/ai-software-factory-infra.yaml"] = []byte("# yaml-language-server: $schema=../schema/project.schema.json\n")
		}, "projects/ai-software-factory-infra.yaml: empty document"},
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

// A project file may only tighten what policy.yaml bounds (stricter
// wins): each quota, envelope and standing value at most the policy's,
// each server type one the policy lists; an equal value in other units
// passes. The fixture's policy: project_default quota 4, 8Gi, 20Gi and
// standing 5 EUR, cx23 and cx33, 2; envelope_default 10 EUR, cx23, 2
// servers, 4 hours, 20 GB.
func TestParseTreeStricterWins(t *testing.T) {
	for name, tc := range map[string]struct{ old, new, want string }{
		"quota cpu":            {`cpu: "2"`, `cpu: "8"`, `project demo-app: quota.cpu "8" is looser than policy.yaml's project_default.quota.cpu "4"`},
		"quota cpu in milli":   {`cpu: "2"`, `cpu: 4001m`, `project demo-app: quota.cpu "4001m" is looser than policy.yaml's project_default.quota.cpu "4"`},
		"quota memory":         {`memory: 4Gi`, `memory: 9Gi`, `project demo-app: quota.memory "9Gi" is looser than policy.yaml's project_default.quota.memory "8Gi"`},
		"quota storage":        {`storage: 10Gi`, `storage: 21Gi`, `project demo-app: quota.storage "21Gi" is looser than policy.yaml's project_default.quota.storage "20Gi"`},
		"envelope monthly":     {`envelope: {monthly_eur: 5`, `envelope: {monthly_eur: 11`, "project demo-app: envelope.monthly_eur 11 is looser than policy.yaml's envelope_default.monthly_eur 10"},
		"envelope server type": {`server_types: [cx23], max_count: 1`, `server_types: [cx23, cx53], max_count: 1`, "project demo-app: envelope.server_types lists cx53, which policy.yaml's envelope_default.server_types does not"},
		"envelope count":       {`max_count: 1, max_lease_hours`, `max_count: 3, max_lease_hours`, "project demo-app: envelope.max_count 3 is looser than policy.yaml's envelope_default.max_count 2"},
		"envelope lease":       {`max_lease_hours: 2`, `max_lease_hours: 5`, "project demo-app: envelope.max_lease_hours 5 is looser than policy.yaml's envelope_default.max_lease_hours 4"},
		"envelope volume":      {`volume_max_gb: 0`, `volume_max_gb: 30`, "project demo-app: envelope.volume_max_gb 30 is looser than policy.yaml's envelope_default.volume_max_gb 20"},
		"standing monthly":     {`standing: {monthly_eur: 3`, `standing: {monthly_eur: 6`, "project demo-app: standing.monthly_eur 6 is looser than policy.yaml's project_default.standing.monthly_eur 5"},
		"standing server type": {`server_types: [cx33]`, `server_types: [cx33, cpx51]`, "project demo-app: standing.server_types lists cpx51, which policy.yaml's project_default.standing.server_types does not"},
		"standing count":       {`server_types: [cx33], max_count: 1`, `server_types: [cx33], max_count: 3`, "project demo-app: standing.max_count 3 is looser than policy.yaml's project_default.standing.max_count 2"},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			f := string(files["projects/demo-app.yaml"])
			if !strings.Contains(f, tc.old) {
				t.Fatalf("demo-app.yaml lacks %s", tc.old)
			}
			files["projects/demo-app.yaml"] = []byte(strings.Replace(f, tc.old, tc.new, 1))
			if _, err := ParseTree(files); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	for name, tc := range map[string]struct{ old, new string }{
		"equal values":         {`quota: {cpu: "2", memory: 4Gi, storage: 10Gi}`, `quota: {cpu: "4", memory: 8Gi, storage: 20Gi}`},
		"equal in other units": {`quota: {cpu: "2", memory: 4Gi, storage: 10Gi}`, `quota: {cpu: 4000m, memory: 8192Mi, storage: 20480Mi}`},
		"the whole envelope":   {`envelope: {monthly_eur: 5, server_types: [cx23], max_count: 1, max_lease_hours: 2, volume_max_gb: 0}`, `envelope: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}`},
		"the whole standing":   {`standing: {monthly_eur: 3, server_types: [cx33], max_count: 1}`, `standing: {monthly_eur: 5, server_types: [cx23, cx33], max_count: 2}`},
	} {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			f := string(files["projects/demo-app.yaml"])
			if !strings.Contains(f, tc.old) {
				t.Fatalf("demo-app.yaml lacks %s", tc.old)
			}
			files["projects/demo-app.yaml"] = []byte(strings.Replace(f, tc.old, tc.new, 1))
			tree, err := ParseTree(files)
			if err != nil {
				t.Fatal(err)
			}
			if err := tree.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
	// a tree read back from Postgres is held to the same bound
	files := fixtureTree(t)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	p := tree.Projects["demo-app"]
	p.Quota = &Quota{CPU: "5", Memory: "1Gi", Storage: "1Gi"}
	tree.Projects["demo-app"] = p
	if err := tree.Validate(); err == nil || !strings.Contains(err.Error(), `quota.cpu "5" is looser`) {
		t.Fatalf("Validate = %v, want the looser quota refused", err)
	}
}

// Limits' durations are strings in JSON, as in YAML: the stored tree, GET
// /policy and the console show 20m0s, not nanoseconds.
func TestLimitsJSON(t *testing.T) {
	l := Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1, Steps: 150, NudgeAfter: 4, Nudges: 2, SameCall: 5, ReviewDiffKiB: 256,
		CheckinCalls: 12, CheckinShare: 10, CheckinTailKiB: 24, CheckinHandoverChars: 2000, CheckinSessions: 1}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"sessions":3,"agent_time":"20m0s","check_in":"0s","supervisor_calls":3,"supervisor_timeout":"6m0s","stronger_after":1,"steps":150,"nudge_after":4,"nudges":2,"same_call":5,"review_diff_kib":256,"checkin_calls":12,"checkin_share":10,"checkin_tail_kib":24,"checkin_handover_chars":2000,"checkin_sessions":1}`; string(b) != want {
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
	files["processes/spec.yaml"] = []byte(strings.Replace(string(files["processes/spec.yaml"]), "on_findings: fail", "on_findings: maybe", 1))
	files["projects/ai-software-factory-infra.yaml"] = []byte(strings.Replace(string(files["projects/ai-software-factory-infra.yaml"]), "agents: none", "agents: all", 1))
	_, err := ParseTree(files)
	if err == nil || !strings.Contains(err.Error(), "processes/spec.yaml:") || !strings.Contains(err.Error(), "projects/ai-software-factory-infra.yaml:") {
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
		switch path.Dir(file) {
		case "projects":
			return "project.schema.json"
		case "processes":
			return "process.schema.json"
		}
		return strings.TrimSuffix(file, ".yaml") + ".schema.json"
	}
	for file, b := range files {
		if strings.HasPrefix(file, DirTemplates+"/") || path.Dir(file) == DirRoutines {
			continue
		}
		if err := resolvedSchema(t, kindOf(file)).Validate(yamlInstance(t, b)); err != nil {
			t.Errorf("%s against %s: %v", file, kindOf(file), err)
		}
	}
	for name, tc := range map[string]struct{ file, old, new string }{
		"invalid enum":         {"processes/spec.yaml", "on_findings: fail", "on_findings: maybe"},
		"invalid stage name":   {"processes/spec.yaml", "name: review", "name: lint"},
		"invalid predicate":    {"processes/spec.yaml", "when: outsider", "when: insider"},
		"unknown key":          {"projects/ai-software-factory-infra.yaml", "agents: none", "agents: none\nowner: me"},
		"required key missing": {"projects/ai-software-factory-infra.yaml", "objects: seed\n", ""},
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
	if got := slices.Sorted(maps.Keys(schemas)); !slices.Equal(got, []string{"commands.schema.json", "policy.schema.json", "process.schema.json", "project.schema.json"}) {
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
	stage := root["process.schema.json"].Properties["stages"].Items
	if got := stage.Properties["on_findings"].Enum; !reflect.DeepEqual(got, []any{"agent", "human", "fail"}) {
		t.Errorf("on_findings enum = %v", got)
	}
	if got := stage.Properties["name"].Enum; len(got) != 9 {
		t.Errorf("stage name enum = %v", got)
	}
	if !slices.Equal(stage.Required, []string{"name", "on_findings", "enabled"}) {
		t.Errorf("stage required = %v", stage.Required)
	}
	proc := root["process.schema.json"]
	plan := proc.Properties["tracks"].Properties["plan"]
	if got := proc.Properties["tracks"].Required; !slices.Equal(got, []string{"direct"}) {
		t.Errorf("tracks required = %v, want direct", got)
	}
	if mc := plan.Properties["plan"].Properties["max_cards"]; mc.Minimum == nil || *mc.Minimum != 2 || mc.Maximum == nil || *mc.Maximum != 20 {
		t.Errorf("max_cards = %v to %v, want 2 to 20", mc.Minimum, mc.Maximum)
	}
	if mc := plan.Properties["plan"].Properties["min_cards"]; mc.Minimum == nil || *mc.Minimum != 1 || mc.Maximum == nil || *mc.Maximum != 20 {
		t.Errorf("min_cards = %v to %v, want 1 to 20", mc.Minimum, mc.Maximum)
	}
	if got := proc.Properties["merge"].Properties["queue"].Enum; !reflect.DeepEqual(got, []any{"none", "project"}) {
		t.Errorf("merge queue enum = %v", got)
	}
	if !slices.Equal(proc.Required, []string{"tracks", "stages", "merge"}) || !strings.Contains(proc.Properties["stages"].Items.Properties["on_findings"].Description, "limits.sessions is the round cap") {
		t.Errorf("process required = %v, on_findings %q", proc.Required, proc.Properties["stages"].Items.Properties["on_findings"].Description)
	}
	if got := root["policy.schema.json"].Properties["classes"].AdditionalProperties.Properties["session"].Enum; !reflect.DeepEqual(got, []any{"code", "plan", "report"}) {
		t.Errorf("class session enum = %v", got)
	}
	project := root["project.schema.json"]
	if got := project.Properties["agents"].Enum; !reflect.DeepEqual(got, []any{"coder", "none"}) {
		t.Errorf("agents enum = %v", got)
	}
	if got := project.Properties["objects"].Enum; !reflect.DeepEqual(got, []any{"factory", "seed"}) {
		t.Errorf("objects enum = %v", got)
	}
	tokens := project.Properties["tokens"]
	if got := tokens.Properties["gate"].Enum; !reflect.DeepEqual(got, []any{"forgejo-gate"}) {
		t.Errorf("tokens.gate enum = %v, want the gate's secret alone", got)
	}
	if got := tokens.Properties["chat"].Enum; !reflect.DeepEqual(got, []any{"forgejo-chat"}) {
		t.Errorf("tokens.chat enum = %v, want the chat's secret alone", got)
	}
	if got := tokens.Properties["merge"].Enum; !reflect.DeepEqual(got, []any{"forgejo-ci-registrar", "forgejo-merge", "forgejo-config"}) {
		t.Errorf("tokens.merge enum = %v", got)
	}
	if a := project.Properties["approvers"]; a == nil || a.Items == nil || a.Items.Type != "string" || !strings.Contains(a.Description, "the policy's approvers") {
		t.Errorf("project approvers = %+v, want a list of user names that falls back to the policy's", a)
	}
	if got := root["policy.schema.json"].Properties["chat"].Properties["confirm"].Items.Enum; !slices.Contains(got, any("invite")) {
		t.Errorf("chat confirm enum = %v, want invite in it", got)
	}
	pol := root["policy.schema.json"]
	tools := pol.Properties["classes"].AdditionalProperties.Properties["tools"]
	if tools.PropertyNames == nil || len(tools.PropertyNames.Enum) != len(PaidTools)+len(FreeTools) {
		t.Errorf("tools property names = %+v", tools.PropertyNames)
	}
	if ll := pol.Properties["risk"].Properties["large_lines"]; ll == nil || ll.Type != "integer" || ll.Minimum == nil || *ll.Minimum != 0 || slices.Contains(pol.Properties["risk"].Required, "large_lines") {
		t.Errorf("risk.large_lines = %+v, want an optional integer from 0", ll)
	}
	if got := pol.Properties["limits"].Properties["agent_time"]; got.Type != "string" || got.Pattern == "" {
		t.Errorf("agent_time = type %q pattern %q, want a duration string", got.Type, got.Pattern)
	}
	for key, bound := range map[string]string{"quota": "project_default.quota", "envelope": "envelope_default", "standing": "project_default.standing", "budget": "budget.month_eur"} {
		if d := project.Properties[key].Description; !strings.Contains(d, "Stricter wins: bounded by policy.yaml's "+bound) {
			t.Errorf("project %s description = %q, want its bound marked", key, d)
		}
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
	// a stale or missing copy is DiffSchemas' to report, not a refusal
	for name, tc := range map[string]struct {
		change func(map[string][]byte)
		want   string
	}{
		"equal": {func(map[string][]byte) {}, ""},
		"differs": {func(f map[string][]byte) {
			f["schema/policy.schema.json"] = append(f["schema/policy.schema.json"], ' ')
		}, ""},
		"missing": {func(f map[string][]byte) { delete(f, "schema/process.schema.json") }, ""},
		"extra":   {func(f map[string][]byte) { f["schema/old.schema.json"] = []byte("{}") }, "schema/old.schema.json is not a schema factoryd writes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := committed()
			tc.change(f)
			err := CheckSchemas(f)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || err.Error() != tc.want) {
				t.Fatalf("CheckSchemas = %v, want %q", err, tc.want)
			}
		})
	}
	// ParseTree ignores the schema directory
	if _, err := ParseTree(committed()); err != nil {
		t.Fatalf("ParseTree with schema/: %v", err)
	}
}

func TestDiffSchemas(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	copies := map[string][]byte{}
	for name, b := range schemas {
		copies["schema/"+name] = b
	}
	if d, err := DiffSchemas(copies); err != nil || len(d) != 0 {
		t.Fatalf("equal copies = (%+v, %v), want none", d, err)
	}
	copies["schema/policy.schema.json"] = []byte("{}\n")
	delete(copies, "schema/process.schema.json")
	d, err := DiffSchemas(copies)
	if err != nil {
		t.Fatal(err)
	}
	want := []SchemaDifference{
		{Path: "schema/policy.schema.json", Want: schemas["policy.schema.json"]},
		{Path: "schema/process.schema.json", Missing: true, Want: schemas["process.schema.json"]},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("differences = %+v, want %+v", d, want)
	}
}

// sha256 of "a.schema.json\0{}\n\0b.schema.json\0[]\n\0", by sha256sum
func TestSchemaContentHash(t *testing.T) {
	schemas := map[string][]byte{"b.schema.json": []byte("[]\n"), "a.schema.json": []byte("{}\n")}
	if got := SchemaContentHash(schemas); got != "c94bf51428aa" {
		t.Errorf("SchemaContentHash = %q, want c94bf51428aa", got)
	}
	schemas["b.schema.json"] = []byte("[] \n")
	if got := SchemaContentHash(schemas); got != "d3acf55b8262" {
		t.Errorf("SchemaContentHash = %q, want d3acf55b8262", got)
	}
}

func TestProjectSchemaHasImages(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	s := string(schemas["project.schema.json"])
	for _, w := range []string{`"images"`, `"digest"`, `"tools"`} {
		if !strings.Contains(s, w) {
			t.Errorf("project.schema.json lacks %s", w)
		}
	}
}
