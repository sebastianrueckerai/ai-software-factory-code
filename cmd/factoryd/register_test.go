package main

import (
	"context"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
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
	ladder := factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"}.Aliases()
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
	ladder := factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"}.Aliases()
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

// factoryd's Go heap is held at nine tenths of its container's memory
// limit (GOMEMLIMIT): near it the collector works harder before the kernel
// kills the pod, every task's activities with it. 512Mi gives 460MiB.
func TestFactorydHeapLimitIsNineTenthsOfItsPod(t *testing.T) {
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
						Containers []struct {
							Name      string
							Env       []struct{ Name, Value string }
							Resources struct {
								Limits struct{ Memory string }
							}
						}
					}
				}
			}
		}
		if err := dec.Decode(&doc); err != nil {
			t.Fatal("no Deployment factoryd")
		}
		if doc.Kind != "Deployment" || doc.Metadata.Name != "factoryd" {
			continue
		}
		c := doc.Spec.Template.Spec.Containers[0]
		limit, ok := strings.CutSuffix(c.Resources.Limits.Memory, "Mi")
		mib, err := strconv.Atoi(limit)
		if !ok || err != nil {
			t.Fatalf("factoryd's memory limit %q is not in Mi", c.Resources.Limits.Memory)
		}
		var got string
		for _, e := range c.Env {
			if e.Name == "GOMEMLIMIT" {
				got = e.Value
			}
		}
		if want := strconv.Itoa(mib*9/10) + "MiB"; c.Name != "factoryd" || got != want || want != "460MiB" {
			t.Fatalf("container %s: GOMEMLIMIT %q under a limit of %s, want %s", c.Name, got, c.Resources.Limits.Memory, want)
		}
		return
	}
}

// factoryd's ClusterRole lists the cluster's CronJobs and CNPG's
// ScheduledBackups for the Resources page and does nothing else with them.
func TestFactorydOnlyListsTheClustersJobs(t *testing.T) {
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string][]string{}
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
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					verbs[g+"/"+res] = append(verbs[g+"/"+res], r.Verbs...)
				}
			}
		}
	}
	for _, res := range []string{"batch/cronjobs", "postgresql.cnpg.io/scheduledbackups"} {
		if !slices.Equal(verbs[res], []string{"list"}) {
			t.Errorf("factoryd's ClusterRole grants %v on %s, want [list]", verbs[res], res)
		}
	}
}

// deployment_health reads pods, events, ReplicaSets and Deployments. A seed
// project's namespace, factoryd's own among them, binds ClusterRole
// factoryd-health, which holds those reads and nothing else: factoryd-project
// also lets registration keep Secrets, Kustomizations and network policies,
// which a seed namespace (the platform's, Forgejo's) must not hand to
// factoryd. A registered project's namespace reads through factoryd-project's
// binding, so that role holds the same reads. Neither grants a pod's log,
// exec or attach, nor writes any of the four kinds.
func TestDeploymentHealthReadsThroughARoleOfReadsOnly(t *testing.T) {
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]map[string][]string{}
	var bound string
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                           `yaml:"kind"`
			Metadata struct{ Name, Namespace string } `yaml:"metadata"`
			RoleRef  struct{ Kind, Name string }      `yaml:"roleRef"`
			Subjects []struct{ Kind, Name, Namespace string }
			Rules    []struct {
				APIGroups []string `yaml:"apiGroups"`
				Resources []string `yaml:"resources"`
				Verbs     []string `yaml:"verbs"`
			} `yaml:"rules"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		switch {
		case doc.Kind == "RoleBinding" && doc.Metadata.Namespace == "factory" && doc.RoleRef.Name == "factoryd-project":
			t.Errorf("RoleBinding %s binds factoryd-project in the seed namespace factory", doc.Metadata.Name)
		case doc.Kind == "RoleBinding" && doc.Metadata.Name == "factoryd-health" && doc.Metadata.Namespace == "factory" &&
			len(doc.Subjects) == 1 && doc.Subjects[0].Kind == "ServiceAccount" && doc.Subjects[0].Name == "factoryd" && doc.Subjects[0].Namespace == "factory":
			bound = doc.RoleRef.Kind + "/" + doc.RoleRef.Name
		case doc.Kind == "ClusterRole" && (doc.Metadata.Name == "factoryd-health" || doc.Metadata.Name == "factoryd-project"):
			v := map[string][]string{}
			for _, r := range doc.Rules {
				for _, g := range r.APIGroups {
					for _, res := range r.Resources {
						v[g+"/"+res] = append(v[g+"/"+res], r.Verbs...)
					}
				}
			}
			verbs[doc.Metadata.Name] = v
		}
	}
	if bound != "ClusterRole/factoryd-health" {
		t.Errorf("RoleBinding factoryd-health in factory for ServiceAccount factoryd binds %q, want ClusterRole/factoryd-health", bound)
	}
	reads := map[string][]string{"/pods": {"get", "list"}, "/events": {"get", "list"}, "apps/replicasets": {"get", "list"}, "apps/deployments": {"get", "list"}}
	if got := verbs["factoryd-health"]; !maps.EqualFunc(got, reads, func(a, b []string) bool { return slices.Equal(slices.Sorted(slices.Values(a)), b) }) {
		t.Errorf("ClusterRole factoryd-health grants %v, want exactly %v", got, reads)
	}
	project := verbs["factoryd-project"]
	for res := range reads {
		for _, v := range []string{"get", "list"} {
			if !slices.Contains(project[res], v) {
				t.Errorf("factoryd-project lacks %s on %s: %v", v, res, project[res])
			}
		}
		for _, v := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			if slices.Contains(project[res], v) {
				t.Errorf("factoryd-project may %s %s", v, res)
			}
		}
	}
	for _, res := range []string{"/pods/log", "/pods/exec", "/pods/attach"} {
		if len(project[res]) != 0 {
			t.Errorf("factoryd-project grants %v on %s", project[res], res)
		}
	}
}

// factoryd suspends and resumes the factory's own Kustomization while a
// plan is open on main: a Role in flux-system grants get and patch on
// that one Kustomization, bound to factoryd alone, and the admission
// policy admits the change by its own case.
func TestFactorydMaySuspendItsKustomization(t *testing.T) {
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type rule struct {
		APIGroups     []string `yaml:"apiGroups"`
		Resources     []string `yaml:"resources"`
		Verbs         []string `yaml:"verbs"`
		ResourceNames []string `yaml:"resourceNames"`
	}
	type ref struct{ APIGroup, Kind, Name string }
	type subject struct{ Kind, Name, Namespace string }
	var role []rule
	var roleRef *ref
	var subjects []subject
	var variable bool
	var validations []string
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                           `yaml:"kind"`
			Metadata struct{ Name, Namespace string } `yaml:"metadata"`
			Rules    []rule                           `yaml:"rules"`
			RoleRef  struct {
				APIGroup string `yaml:"apiGroup"`
				Kind     string `yaml:"kind"`
				Name     string `yaml:"name"`
			} `yaml:"roleRef"`
			Subjects []subject `yaml:"subjects"`
			Spec     struct {
				Variables   []struct{ Name, Expression string } `yaml:"variables"`
				Validations []struct{ Expression string }       `yaml:"validations"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		switch {
		case doc.Kind == "Role" && doc.Metadata.Name == "factoryd-flux" && doc.Metadata.Namespace == "flux-system":
			role = doc.Rules
		case doc.Kind == "RoleBinding" && doc.Metadata.Name == "factoryd-flux" && doc.Metadata.Namespace == "flux-system":
			roleRef = &ref{doc.RoleRef.APIGroup, doc.RoleRef.Kind, doc.RoleRef.Name}
			subjects = doc.Subjects
		case doc.Kind == "ValidatingAdmissionPolicy" && doc.Metadata.Name == "factoryd-task-namespaces":
			for _, v := range doc.Spec.Variables {
				variable = variable || v.Name == "factorySuspend"
			}
			for _, v := range doc.Spec.Validations {
				validations = append(validations, v.Expression)
			}
		}
	}
	want := []rule{{APIGroups: []string{"kustomize.toolkit.fluxcd.io"}, Resources: []string{"kustomizations"}, Verbs: []string{"get", "patch"}, ResourceNames: []string{"factory"}}}
	if !slices.EqualFunc(role, want, func(a, b rule) bool {
		return slices.Equal(a.APIGroups, b.APIGroups) && slices.Equal(a.Resources, b.Resources) && slices.Equal(a.Verbs, b.Verbs) && slices.Equal(a.ResourceNames, b.ResourceNames)
	}) {
		t.Errorf("Role flux-system/factoryd-flux rules %+v, want exactly %+v", role, want)
	}
	if roleRef == nil || *roleRef != (ref{"rbac.authorization.k8s.io", "Role", "factoryd-flux"}) {
		t.Errorf("RoleBinding flux-system/factoryd-flux roleRef %+v, want Role factoryd-flux", roleRef)
	}
	if !slices.Equal(subjects, []subject{{"ServiceAccount", "factoryd", "factory"}}) {
		t.Errorf("RoleBinding flux-system/factoryd-flux subjects %+v, want ServiceAccount factory/factoryd alone", subjects)
	}
	if !variable {
		t.Error("policy factoryd-task-namespaces has no variable factorySuspend")
	}
	var first, kustomization bool
	for i, v := range validations {
		named := strings.Contains(v, "variables.factorySuspend")
		first = first || i == 0 && named
		kustomization = kustomization || strings.HasPrefix(v, "variables.kind != 'Kustomization'") && named
	}
	if !first || !kustomization {
		t.Errorf("variables.factorySuspend in the first validation %v, in the Kustomization rule %v; want both", first, kustomization)
	}
}
