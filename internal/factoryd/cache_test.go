package factoryd

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// checkCache asserts the pod mounts claim as volume cache at /cache and
// points Go's and npm's caches there.
func checkCache(t *testing.T, pod map[string]any, claim string) {
	t.Helper()
	spec := pod["spec"].(map[string]any)
	var vol any
	for _, v := range spec["volumes"].([]any) {
		if v.(map[string]any)["name"] == "cache" {
			vol = v
		}
	}
	want := map[string]any{"name": "cache", "persistentVolumeClaim": map[string]any{"claimName": claim}}
	if !reflect.DeepEqual(vol, want) {
		t.Errorf("volume cache = %v, want %v", vol, want)
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(c["volumeMounts"]), "map[mountPath:/cache name:cache]") {
		t.Errorf("volumeMounts = %v, want cache at /cache", c["volumeMounts"])
	}
	env := map[string]string{}
	add := func(e map[string]any) {
		if v, ok := e["value"].(string); ok {
			env[e["name"].(string)] = v
		}
	}
	switch es := c["env"].(type) {
	case []map[string]any:
		for _, e := range es {
			add(e)
		}
	case []any:
		for _, e := range es {
			add(e.(map[string]any))
		}
	}
	for k, v := range map[string]string{"GOPATH": "/cache/go", "GOCACHE": "/cache/go-build", "npm_config_cache": "/cache/npm"} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	if !strings.HasPrefix(env["PATH"], "/cache/go/bin:") {
		t.Errorf("env PATH = %q, want it to start with /cache/go/bin:", env["PATH"])
	}
}

// checkClaim asserts kube holds the claim name of class factory-cache in ns.
func checkClaim(t *testing.T, kube *fakeKube, ns, name string) {
	t.Helper()
	obj := kube.objects["/api/v1/namespaces/"+ns+"/persistentvolumeclaims"]
	if obj == nil {
		t.Fatalf("no claim in %s", ns)
	}
	if got := obj["metadata"].(map[string]any)["name"]; got != name {
		t.Errorf("claim in %s = %v, want %s", ns, got, name)
	}
	if got := obj["spec"].(map[string]any)["storageClassName"]; got != "factory-cache" {
		t.Errorf("claim class = %v, want factory-cache", got)
	}
}

func TestWorkerPodMountsProjectCache(t *testing.T) {
	kube := newFakeKube()
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	store := newFakeStore()
	store.projects = []Project{{Name: "linkshort", Repository: "linkshort/linkshort"}}
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{}}
	r := &Runner{cfg: cfg, kube: kube, store: store, forgejo: &Forgejo{BotUser: "factory-agent"}}
	task := Task{ID: "t1", Repository: "linkshort/linkshort"}
	cache, err := r.setupNamespace(context.Background(), task, ForgejoToken{}, ForgejoToken{})
	if err != nil {
		t.Fatal(err)
	}
	checkClaim(t, kube, "factory-agents-t1", "cache-linkshort")
	checkCache(t, r.workerPodSpec(task, sessionSpec{id: "s-1", cache: cache}), "cache-linkshort")
	checkCache(t, r.reproPodSpec(task, cache, Repo{Owner: "linkshort", Name: "linkshort"}, 1, "c1", ""), "cache-linkshort")
}

func TestCIPodMountsCICache(t *testing.T) {
	store := &memProjects{m: map[string]Project{
		"linkshort": {Name: "linkshort", Repository: "linkshort/linkshort", Objects: "factory", Agents: "coder", File: true, GitToken: "g"},
	}}
	repos, err := CIReposOf(&Forgejo{}, store)(context.Background())
	if err != nil || len(repos) != 1 || repos[0].Cache != "cache-ci-linkshort" {
		t.Fatalf("repos = %+v, %v; want one with cache cache-ci-linkshort", repos, err)
	}
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 42, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.cfg.Repos = func(context.Context) ([]CIRepo, error) {
		r := codeCIRepos[0]
		r.Cache = "cache-ci-linkshort"
		return []CIRepo{r}, nil
	}
	spawner.poll(context.Background())
	checkClaim(t, kube, "factory-agents-ci-42", "cache-ci-linkshort")
	pod, ok := kube.pods["factory-agents-ci-42/"+ciPodName]
	if !ok {
		t.Fatal("runner pod not created")
	}
	checkCache(t, pod.spec, "cache-ci-linkshort")
}

func TestReleasedCachePVsDeleted(t *testing.T) {
	kube := newFakeKube()
	kube.pvs = []PersistentVolume{
		{Name: "pv-released-cache", StorageClass: "factory-cache", Phase: "Released"},
		{Name: "pv-bound-cache", StorageClass: "factory-cache", Phase: "Bound"},
		{Name: "pv-released-local", StorageClass: "local-path", Phase: "Released"},
		{Name: "pv-released-cache-2", StorageClass: "factory-cache", Phase: "Released"},
	}
	want := []string{"pv-released-cache", "pv-released-cache-2"}

	r := &Runner{cfg: testRunnerConfig(), kube: kube}
	r.forgetNamespace(context.Background(), "t1")
	if !reflect.DeepEqual(kube.deletedPVs, want) {
		t.Fatalf("forgetNamespace deleted %v, want %v", kube.deletedPVs, want)
	}

	kube.deletedPVs = nil
	spawner := newTestCISpawner(t, kube, &ciHub{})
	spawner.teardown(context.Background(), "factory-agents-ci-1", codeCIRepos)
	if !reflect.DeepEqual(kube.deletedPVs, want) {
		t.Fatalf("CI teardown deleted %v, want %v", kube.deletedPVs, want)
	}
	if slices.Contains(kube.deletedPVs, "pv-bound-cache") {
		t.Fatal("a bound cache volume was deleted")
	}
}
