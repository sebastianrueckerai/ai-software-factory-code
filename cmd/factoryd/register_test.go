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

// The chat's project file change reads infra's head the same way, an
// existing project's file included: a change the loader takes answers no
// line, one it refuses its lines.
func TestCheckChangeReadsTheHead(t *testing.T) {
	files, err := policy.ReadDir("../../internal/policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	ladder := factoryd.ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}.Aliases()
	file := files["projects/linkshort.yaml"]
	if refused, err := checkChange(context.Background(), treeSource(files), map[string][]byte{"projects/linkshort.yaml": file}, ladder); refused != nil || err != nil {
		t.Fatalf("the file as it is: %q, %v", refused, err)
	}
	bad := []byte(strings.Replace(string(file), "process: spec", "process: specs", 1))
	refused, err := checkChange(context.Background(), treeSource(files), map[string][]byte{"projects/linkshort.yaml": bad}, ladder)
	want := []string{`projects/linkshort.yaml: process "specs": no file processes/specs.yaml (processes: maintenance, spec)`}
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

// factoryd's pod shares its process namespace, so the pause container is
// PID 1 and reaps the git helpers a killed git call leaves.
func TestFactorydPodReapsGitHelpers(t *testing.T) {
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Spec     struct {
				Template struct {
					Spec struct {
						Share bool `yaml:"shareProcessNamespace"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			t.Fatal("no Deployment factoryd")
		}
		if doc.Kind == "Deployment" && doc.Metadata.Name == "factoryd" {
			if !doc.Spec.Template.Spec.Share {
				t.Fatal("factoryd's pod does not share its process namespace: killed git helpers stay zombies of PID 1")
			}
			return
		}
	}
}

// factoryd's ClusterRole lists the cluster's CronJobs for the Resources
// page and does nothing else with them.
func TestFactorydOnlyListsCronJobs(t *testing.T) {
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Rules    []struct {
				APIGroups []string `yaml:"apiGroups"`
				Resources []string `yaml:"resources"`
				Verbs     []string `yaml:"verbs"`
			} `yaml:"rules"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind != "ClusterRole" || doc.Metadata.Name != "factoryd" {
			continue
		}
		for _, r := range doc.Rules {
			if slices.Contains(r.APIGroups, "batch") && slices.Contains(r.Resources, "cronjobs") {
				verbs = append(verbs, r.Verbs...)
			}
		}
	}
	if !slices.Equal(verbs, []string{"list"}) {
		t.Fatalf("factoryd's ClusterRole grants %v on cronjobs, want [list]", verbs)
	}
}
