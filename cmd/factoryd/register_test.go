package main

import (
	"context"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// register-project takes the project's name and --rotate only: its
// settings are its file's, the admin's login is OpenBao's.
func TestParseRegistration(t *testing.T) {
	name, rotate, err := parseRegistration([]string{"--name", "demo-app", "--rotate"})
	if err != nil || name != "demo-app" || !rotate {
		t.Fatalf("= %q, %v, %v", name, rotate, err)
	}
	for _, args := range [][]string{{}, {"--name", "x", "--merge-rule", "auto"}, {"--name", "x", "extra"}, {"--name", "x", "--admin-user", "sebastian"}} {
		if _, _, err := parseRegistration(args); err == nil {
			t.Errorf("parseRegistration(%q) accepted", args)
		}
	}
}

// The console's check reads infra's head through the loader's source and
// answers a refusal as its lines, one per joined error.
func TestCheckProjectReadsTheHead(t *testing.T) {
	files, err := policy.ReadDir("../../internal/policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	ladder := factoryd.ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}.Aliases()
	file := files["projects/linkshort.yaml"]
	delete(files, "projects/linkshort.yaml")
	if refused, err := checkProject(context.Background(), treeSource(files), "linkshort", file, ladder); refused != nil || err != nil {
		t.Fatalf("the exit test's file: %q, %v", refused, err)
	}
	bad := strings.Replace(strings.Replace(string(file), "process: spec", "process: specs", 1), "template: ts-go", "template: ts-go2", 1)
	refused, err := checkProject(context.Background(), treeSource(files), "linkshort", []byte(bad), ladder)
	want := []string{`projects/linkshort.yaml: process "specs": no file processes/specs.yaml (processes: maintenance, spec)`,
		`projects/linkshort.yaml: template "ts-go2": no directory templates/ts-go2/ (templates: ts-go)`}
	if err != nil || !slices.Equal(refused, want) {
		t.Fatalf("refused %q, %v; want %q", refused, err, want)
	}
}

// factoryd's default-deny egress resolves GitHub's API, which registration
// asks whether an import's source is empty and the exports call, and
// GitHub's git host, and admits each on 443 alone.
func TestEgressAdmitsTheGitHubAPI(t *testing.T) {
	gh := newRegistrar(nil, nil, nil, nil, "").GitHub
	for _, base := range []string{gh.API, gh.Git} {
		egressAdmits(t, base)
	}
}

func egressAdmits(t *testing.T, base string) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Port() != "" {
		t.Fatalf("%v, %v: want https on its default port", u, err)
	}
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type name struct {
		MatchName string `yaml:"matchName"`
	}
	type names []name
	type ports []struct {
		Ports []struct{ Port, Protocol string } `yaml:"ports"`
		Rules struct {
			DNS names `yaml:"dns"`
		} `yaml:"rules"`
	}
	has := func(ns names) bool { return slices.Contains(ns, name{u.Hostname()}) }
	var resolves, admits bool
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Spec     struct {
				Egress []struct {
					ToFQDNs names `yaml:"toFQDNs"`
					ToPorts ports `yaml:"toPorts"`
				} `yaml:"egress"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind != "CiliumNetworkPolicy" || doc.Metadata.Name != "factoryd-egress" {
			continue
		}
		for _, rule := range doc.Spec.Egress {
			for _, p := range rule.ToPorts {
				resolves = resolves || has(p.Rules.DNS)
			}
			if has(rule.ToFQDNs) {
				admits = len(rule.ToPorts) == 1 && len(rule.ToPorts[0].Ports) == 1 &&
					rule.ToPorts[0].Ports[0].Port == "443" && rule.ToPorts[0].Ports[0].Protocol == "TCP"
			}
		}
	}
	if !resolves || !admits {
		t.Fatalf("factoryd-egress: resolves %s %v, admits it on 443/TCP alone %v", u.Hostname(), resolves, admits)
	}
}
