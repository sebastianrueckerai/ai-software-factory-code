package policy

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		changed    []string
		class      string
		paths      []string
		migrations []string
	}{
		"no paths is the default":                      {nil, "normal", nil, nil},
		"plain code":                                   {[]string{"internal/factoryd/workflow.go", "README.md"}, "normal", []string{"README.md", "internal/factoryd/workflow.go"}, nil},
		"a workflow file is forbidden":                 {[]string{"README.md", ".forgejo/workflows/pr.yml"}, "forbidden", []string{".forgejo/workflows/pr.yml"}, nil},
		"a nested .github dir is not":                  {[]string{"docs/.github/x.yml"}, "normal", []string{"docs/.github/x.yml"}, nil},
		"the directory entry itself":                   {[]string{".gitea"}, "forbidden", []string{".gitea"}, nil},
		"a rename out counts its old name":             {[]string{".github/workflows/ci.yml", "ci.yml"}, "forbidden", []string{".github/workflows/ci.yml"}, nil},
		"a migration":                                  {[]string{"internal/factoryd/migrations/0016_x.sql", "internal/factoryd/store.go"}, "migration", []string{"internal/factoryd/migrations/0016_x.sql"}, []string{"internal/factoryd/migrations/0016_x.sql"}},
		"deep ** under migrations":                     {[]string{"internal/gateway/migrations/a/b.sql"}, "migration", []string{"internal/gateway/migrations/a/b.sql"}, []string{"internal/gateway/migrations/a/b.sql"}},
		"* is one segment":                             {[]string{"internal/a/b/migrations/x.sql"}, "normal", []string{"internal/a/b/migrations/x.sql"}, nil},
		"high by Dockerfile* and go.sum":               {[]string{"Dockerfile.runner", "go.sum"}, "high", []string{"Dockerfile.runner", "go.sum"}, nil},
		"a Dockerfile at any depth is high":            {[]string{"cmd/x/Dockerfile.dev", "README.md"}, "high", []string{"cmd/x/Dockerfile.dev"}, nil},
		"Dockerfile* names a file, not a dir":          {[]string{"docs/Dockerfiles/x.md"}, "normal", []string{"docs/Dockerfiles/x.md"}, nil},
		"migrations flagged under a more severe class": {[]string{"internal/policy/schema.sql", ".forgejo/x"}, "forbidden", []string{".forgejo/x"}, []string{"internal/policy/schema.sql"}},
		"k8s is high, over normal":                     {[]string{"k8s/factoryd.yaml", "main.go"}, "high", []string{"k8s/factoryd.yaml"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			c := d.Risk.Classify(tc.changed, 0)
			if c.Class.Name != tc.class || !reflect.DeepEqual(c.Paths, tc.paths) || !reflect.DeepEqual(c.Migrations, tc.migrations) {
				t.Fatalf("Classify(%v) = %+v, want class %s paths %v migrations %v", tc.changed, c, tc.class, tc.paths, tc.migrations)
			}
		})
	}
}

// The first matching rule decides a path, whatever class a later rule
// would give it.
func TestClassifyFirstRuleWins(t *testing.T) {
	r := Risk{
		Classes: []RiskClass{{Name: "a", Outcome: OutcomeForbidden}, {Name: "b", Outcome: OutcomeRequiresApproval}},
		Rules:   []RiskRule{{Paths: []string{"docs/**"}, Class: "b"}, {Paths: []string{"**/*.yml"}, Class: "a"}},
		Default: "b",
	}
	if c := r.Classify([]string{"docs/x.yml"}, 0); c.Class.Name != "b" {
		t.Fatalf("docs/x.yml = %s, want b (first rule)", c.Class.Name)
	}
	if c := r.Classify([]string{"k8s/x.yml"}, 0); c.Class.Name != "a" {
		t.Fatalf("k8s/x.yml = %s, want a", c.Class.Name)
	}
}

func TestParseRejectsBadRisk(t *testing.T) {
	base := valid[:strings.Index(valid, "risk:")]
	for name, tc := range map[string]struct{ risk, want string }{
		"missing":         {"", "risk.classes is empty"},
		"unknown outcome": {"risk: {classes: [{name: a, outcome: merge}], default: a}\n", `outcome "merge"`},
		"unknown default": {"risk: {classes: [{name: a, outcome: forbidden}], default: b}\n", `risk.default "b"`},
		"duplicate class": {"risk: {classes: [{name: a, outcome: forbidden}, {name: a, outcome: forbidden}], default: a}\n", "listed twice"},
		"rule class":      {"risk: {classes: [{name: a, outcome: forbidden}], rules: [{class: z, paths: [x]}], default: a}\n", `unknown class "z"`},
		"rule no paths":   {"risk: {classes: [{name: a, outcome: forbidden}], rules: [{class: a}], default: a}\n", "has no paths"},
		"bad glob":        {"risk: {classes: [{name: a, outcome: forbidden}], rules: [{class: a, paths: [\"[x\"]}], default: a}\n", "not a valid glob"},
		"absolute glob":   {"risk: {classes: [{name: a, outcome: forbidden}], rules: [{class: a, paths: [/k8s]}], default: a}\n", "relative glob"},
		"unknown key":     {"risk: {classes: [{name: a, outcome: forbidden, auto: true}], default: a}\n", "field auto not found"},
		"negative lines":  {"risk: {classes: [{name: a, outcome: forbidden}], default: a, large_lines: -1}\n", "risk.large_lines -1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.risk))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// auto merges without a human only on class normal (ruling 149): the
// decoder refuses it on any other class.
func TestParseAutoOnlyOnNormal(t *testing.T) {
	base, rest := valid[:strings.Index(valid, "risk:")], valid[strings.Index(valid, "envelope_default:"):]
	for name, tc := range map[string]struct {
		risk string
		ok   bool
	}{
		"normal auto":      {"risk: {classes: [{name: high, outcome: requires_approval}, {name: normal, outcome: auto}], default: normal}\n", true},
		"high auto":        {"risk: {classes: [{name: high, outcome: auto}, {name: normal, outcome: requires_approval}], default: normal}\n", false},
		"migration auto":   {"risk: {classes: [{name: migration, outcome: auto, migration: true}, {name: normal, outcome: requires_approval}], default: normal}\n", false},
		"forbidden auto":   {"risk: {classes: [{name: forbidden, outcome: auto}, {name: normal, outcome: requires_approval}], default: normal}\n", false},
		"normal approval":  {"risk: {classes: [{name: normal, outcome: requires_approval}], default: normal}\n", true},
		"normal forbidden": {"risk: {classes: [{name: normal, outcome: forbidden}], default: normal}\n", true},
		"large auto":       {"risk: {classes: [{name: large, outcome: auto}, {name: normal, outcome: requires_approval}], default: normal}\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.risk + rest))
			if tc.ok && err != nil {
				t.Fatalf("Parse: %v, want it accepted", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "outcome auto is allowed on class normal only")) {
				t.Fatalf("Parse error = %v, want the auto refusal", err)
			}
		})
	}
}

// infraRisk is the risk block of the infra repository's factory/policy.yaml.
const infraRisk = `risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: auto}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: migration, paths: ["**/migrations/**", "internal/policy/schema.sql"]}
    - class: high
      paths: ["k8s/**", "deploy/**", "**/Dockerfile*",
        "**/go.mod", "**/go.sum", "**/go.work", "**/go.work.sum",
        "**/package.json", "**/package-lock.json", "**/npm-shrinkwrap.json", "**/yarn.lock", "**/pnpm-lock.yaml",
        "**/Cargo.toml", "**/Cargo.lock",
        "**/bun.lock", "**/bun.lockb",
        "**/pyproject.toml", "**/requirements*.txt", "**/requirements/*.txt", "**/poetry.lock", "**/uv.lock", "**/Pipfile", "**/Pipfile.lock",
        "**/Gemfile", "**/Gemfile.lock", "**/pom.xml", "**/build.gradle*", "**/settings.gradle*", "**/gradle.lockfile",
        "**/*.csproj", "**/packages.lock.json", "**/Directory.Packages.props", "**/composer.json", "**/composer.lock",
        "**/AGENTS.md", "**/CLAUDE.md", "**/opencode.json", "**/opencode.jsonc", "**/CODEOWNERS",
        ".*/**"]
  default: normal
  large_lines: 1000
`

// The copy above is the infra repository's block: with INFRA_FACTORY_DIR
// set to its factory/ directory, the two must be equal.
func TestInfraRiskIsTheCopy(t *testing.T) {
	dir := os.Getenv("INFRA_FACTORY_DIR")
	if dir == "" {
		t.Skip("INFRA_FACTORY_DIR unset")
	}
	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	base, rest := valid[:strings.Index(valid, "risk:")], valid[strings.Index(valid, "envelope_default:"):]
	copied, err := Parse([]byte(base + infraRisk + rest))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tree.Document.Risk, copied.Risk) {
		t.Fatalf("infra risk = %+v\nthe copy = %+v", tree.Document.Risk, copied.Risk)
	}
}

// Manifests and lockfiles of every common ecosystem, agent instruction
// files, CODEOWNERS and every top-level dot directory are protected: class
// high, at any depth for the files.
func TestClassifyProtectedFilesHigh(t *testing.T) {
	base, rest := valid[:strings.Index(valid, "risk:")], valid[strings.Index(valid, "envelope_default:"):]
	d, err := Parse([]byte(base + infraRisk + rest))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"go.mod", "go.sum", "tools/go.mod", "go.work", "go.work.sum",
		"package.json", "web/package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "web/pnpm-lock.yaml",
		"Cargo.toml", "crates/x/Cargo.toml", "Cargo.lock",
		"pyproject.toml", "requirements.txt", "requirements-dev.txt", "svc/requirements.txt", "poetry.lock", "uv.lock",
		"Gemfile", "Gemfile.lock", "pom.xml", "app/pom.xml", "build.gradle", "build.gradle.kts", "app/build.gradle",
		"composer.json", "composer.lock",
		"bun.lock", "web/bun.lockb", "Pipfile", "svc/Pipfile.lock", "requirements/base.txt", "settings.gradle", "settings.gradle.kts", "gradle.lockfile",
		"src/App/App.csproj", "packages.lock.json", "Directory.Packages.props",
		"AGENTS.md", "docs/AGENTS.md", "CLAUDE.md", "opencode.json", "opencode.jsonc", ".opencode/agent/review.md",
		"CODEOWNERS", "docs/CODEOWNERS",
		".husky/pre-commit", ".vscode/settings.json", ".devcontainer/devcontainer.json", ".config/x/y.toml",
		".gitignore", ".golangci.yml",
		"k8s/factoryd.yaml", "deploy/prod/app.yaml", "cmd/x/Dockerfile",
	} {
		if c := d.Risk.Classify([]string{p, "README.md"}, 0); c.Class.Name != "high" || !slices.Equal(c.Paths, []string{p}) {
			t.Errorf("Classify(%s) = %s %v, want high on %s", p, c.Class.Name, c.Paths, p)
		}
	}
	for p, want := range map[string]string{
		".forgejo/workflows/pr.yml":  "forbidden",
		".github/CODEOWNERS":         "forbidden",
		"db/migrations/0001.sql":     "migration",
		"internal/x/main.go":         "normal",
		"docs/.hidden/x.md":          "normal",
		"docs/package.json.md":       "normal",
		"requirements/base.in":       "normal",
		"internal/gomod/go.mod.tmpl": "normal",
		"docs/agents.md":             "normal",
		"src/Gemfile.example":        "normal",
	} {
		if c := d.Risk.Classify([]string{p}, 0); c.Class.Name != want {
			t.Errorf("Classify(%s) = %s, want %s", p, c.Class.Name, want)
		}
	}
}

// Over large_lines changed lines a pull request is class large, which
// needs a human, unless a more severe class applies: large never
// downgrades. 0 is off.
func TestClassifyLargeLines(t *testing.T) {
	base, rest := valid[:strings.Index(valid, "risk:")], valid[strings.Index(valid, "envelope_default:"):]
	d, err := Parse([]byte(base + infraRisk + rest))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		changed        []string
		lines          int
		class, outcome string
		paths          []string
	}{
		"over":           {[]string{"internal/a.go", "README.md"}, 1200, "large", OutcomeRequiresApproval, []string{"README.md", "internal/a.go"}},
		"under":          {[]string{"internal/a.go"}, 900, "normal", OutcomeAuto, []string{"internal/a.go"}},
		"at the limit":   {[]string{"internal/a.go"}, 1000, "normal", OutcomeAuto, []string{"internal/a.go"}},
		"high wins":      {[]string{"k8s/x.yaml", "internal/a.go"}, 1200, "high", OutcomeRequiresApproval, []string{"k8s/x.yaml"}},
		"forbidden wins": {[]string{".forgejo/x"}, 1200, "forbidden", OutcomeForbidden, []string{".forgejo/x"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := d.Risk.Classify(tc.changed, tc.lines)
			if c.Class.Name != tc.class || c.Class.Outcome != tc.outcome || !slices.Equal(c.Paths, tc.paths) {
				t.Fatalf("Classify(%v, %d) = %+v, want %s %s on %v", tc.changed, tc.lines, c, tc.class, tc.outcome, tc.paths)
			}
		})
	}
	d.Risk.LargeLines = 0
	if c := d.Risk.Classify([]string{"internal/a.go"}, 5000); c.Class.Name != "normal" {
		t.Fatalf("large_lines 0, 5000 lines: class %s, want normal", c.Class.Name)
	}
}

// A policy may list class large itself, where its place is its rank.
func TestLargeListed(t *testing.T) {
	base, rest := valid[:strings.Index(valid, "risk:")], valid[strings.Index(valid, "envelope_default:"):]
	d, err := Parse([]byte(base + "risk: {classes: [{name: large, outcome: requires_approval}, {name: high, outcome: requires_approval}, {name: normal, outcome: auto}], " +
		"rules: [{class: high, paths: [k8s/**]}], default: normal, large_lines: 10}\n" + rest))
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Risk.Classify([]string{"k8s/x.yaml"}, 11); c.Class.Name != "large" || !slices.Equal(c.Paths, []string{"k8s/x.yaml"}) {
		t.Fatalf("large listed above high, 11 lines: %+v, want large", c)
	}
}

// MigrationGlobs are the globs of the rules whose class is a migration
// class (roadmap 186): what factoryd hands the worker, whose push guard
// refuses a migration file added under them with a name off the
// timestamped pattern. GlobMatch is the match Classify makes, exported
// for that guard.
func TestMigrationGlobsAndGlobMatch(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Risk.MigrationGlobs(); !reflect.DeepEqual(got, []string{"internal/*/migrations/**", "internal/policy/schema.sql"}) {
		t.Fatalf("MigrationGlobs = %v, want the migration rule's paths", got)
	}
	flagless := Risk{Classes: []RiskClass{{Name: "migration", Outcome: OutcomeRequiresApproval}}, Rules: d.Risk.Rules}
	if got := flagless.MigrationGlobs(); got != nil {
		t.Fatalf("MigrationGlobs with no migration-flagged class = %v, want none", got)
	}
	two := Risk{Classes: []RiskClass{{Name: "m", Outcome: OutcomeRequiresApproval, Migration: true}},
		Rules: []RiskRule{{Class: "m", Paths: []string{"b/**", "a/**"}}, {Class: "m", Paths: []string{"a/**"}}}}
	if got := two.MigrationGlobs(); !reflect.DeepEqual(got, []string{"a/**", "b/**"}) {
		t.Fatalf("MigrationGlobs = %v, want them sorted and compacted", got)
	}
	for p, want := range map[string]bool{
		"internal/factoryd/migrations/0016_x.sql": true,
		"internal/policy/schema.sql":              true,
		"internal/factoryd/store.go":              false,
		"internal/a/b/migrations/x.sql":           false,
	} {
		if got := GlobMatch("internal/*/migrations/**", p) || GlobMatch("internal/policy/schema.sql", p); got != want {
			t.Fatalf("GlobMatch of %s = %v, want %v", p, got, want)
		}
	}
}
