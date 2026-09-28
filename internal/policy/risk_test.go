package policy

import (
	"reflect"
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
			c := d.Risk.Classify(tc.changed)
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
	if c := r.Classify([]string{"docs/x.yml"}); c.Class.Name != "b" {
		t.Fatalf("docs/x.yml = %s, want b (first rule)", c.Class.Name)
	}
	if c := r.Classify([]string{"k8s/x.yml"}); c.Class.Name != "a" {
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
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.risk))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
