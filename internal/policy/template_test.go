package policy

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy/policytest"
)

// The templates' refusals (stage11-design.md section 3, P4 to P6, T1, T2),
// one subtest each: the valid fixture tree plus one change, the exact line.
func TestTemplateRefusals(t *testing.T) {
	for name, tc := range policytest.TemplateRefusals {
		t.Run(name, func(t *testing.T) {
			files := fixtureTree(t)
			tc.Change(files)
			_, err := ParseTree(files)
			if err == nil {
				t.Fatal("the tree passes")
			}
			if tc.Exact && err.Error() != strings.Join(tc.Want, "\n") {
				t.Errorf("ParseTree error =\n%v\nwant exactly:\n%s", err, strings.Join(tc.Want, "\n"))
			}
			for _, want := range tc.Want {
				if !hasLines(err.Error(), want) {
					t.Errorf("ParseTree error lacks the line\n%s\ngot:\n%v", want, err)
				}
			}
		})
	}
}

// A stored tree's templates are judged as the files are.
func TestTreeValidateJudgesAStoredTemplate(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Templates["ts-go"]) != 10 || tree.Projects["linkshort"].Template != "ts-go" {
		t.Fatalf("templates %v, linkshort %+v", len(tree.Templates["ts-go"]), tree.Projects["linkshort"])
	}
	delete(tree.Templates["ts-go"], ".forgejo/roll.sh")
	const want = "templates/ts-go: .forgejo/roll.sh is required: a template carries the roll main's CI runs"
	if err := tree.Validate(); err == nil || err.Error() != want {
		t.Fatalf("Validate = %v, want %s", err, want)
	}
}

// Every file of the fixture's ts-go, rendered with a sample project,
// holds the project's values where the template names them, and every
// YAML file among them decodes.
func TestRenderTemplate(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	data := TemplateData{Name: "linkshort", Repository: "linkshort/linkshort", Image: "git.example/linkshort/linkshort",
		Namespaces: map[string]string{"deploy/staging": "linkshort-staging"}}
	out, err := RenderTemplate(tree.Templates["ts-go"], data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 10 {
		t.Fatalf("rendered %d files", len(out))
	}
	for p, b := range out {
		if strings.Contains(string(b), "[[") || strings.Contains(string(b), "]]") {
			t.Errorf("%s keeps a delimiter", p)
		}
		if strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
			var doc any
			if err := yaml.Unmarshal(b, &doc); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		}
	}
	staging := string(out["deploy/staging/kustomization.yaml"])
	const wantStaging = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: linkshort-staging
resources: [../base]
images:
- name: git.example/linkshort/linkshort
  # all zeros until ci.yml's first roll commits a built image
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
`
	if staging != wantStaging {
		t.Errorf("deploy/staging/kustomization.yaml =\n%s", staging)
	}
	ci := string(out[".forgejo/workflows/ci.yml"])
	for _, want := range []string{"  IMAGE: git.example/linkshort/linkshort\n", "  GIT_SSH_REPO: linkshort/linkshort\n",
		`url="$FACTORY_URL/projects/linkshort/alerts?namespace=linkshort-staging"`, "${{ secrets.DEPLOY_KEY }}"} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml lacks %q", want)
		}
	}
	if !strings.Contains(string(out["AGENTS.md"]), "module `git.example/linkshort/linkshort`") {
		t.Errorf("AGENTS.md names no module")
	}
}
