package factoryd

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func sandboxDockerfile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../Dockerfile.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// aptPackages are the packages dockerfile installs with apt, with the
// versions left off. The image's RUN block carries two apt-get installs on
// one logical line, so every occurrence counts.
func aptPackages(dockerfile string) []string {
	var pkgs []string
	for _, line := range strings.Split(strings.ReplaceAll(dockerfile, "\\\n", " "), "\n") {
		for _, part := range strings.Split(line, "apt-get install -y --no-install-recommends ")[1:] {
			part, _, _ = strings.Cut(part, "&&")
			for _, f := range strings.Fields(part) {
				name, _, _ := strings.Cut(f, "=")
				pkgs = append(pkgs, name)
			}
		}
	}
	return pkgs
}

// The brief's list of tools is the image's: a package added to
// Dockerfile.sandbox without a line here, or a line here with no package,
// fails.
func TestSandboxToolsAreTheImages(t *testing.T) {
	dockerfile := sandboxDockerfile(t)
	holdTools(t, "Dockerfile.sandbox", aptPackages(dockerfile))
	if !strings.Contains(dockerfile, "golang:1.26.") || !strings.Contains(dockerfile, "ARG NODE_VERSION=24.") {
		t.Error("Dockerfile.sandbox no longer carries Go 1.26 and Node 24, which SandboxFacts states")
	}
}

// A project image builds FROM the base stage: the brief's base tools are
// that stage's apt packages, and Go and Node come only after it.
func TestSandboxBaseToolsAreTheBaseStages(t *testing.T) {
	_, stage, ok := strings.Cut(sandboxDockerfile(t), " AS base\n")
	if !ok {
		t.Fatal("Dockerfile.sandbox has no base stage")
	}
	stage, _, _ = strings.Cut(stage, "\nFROM ")
	holdTools(t, "the base stage", aptPackages(stage))
	for _, stack := range []string{"/usr/local/go", "NODE_VERSION", "node.tar"} {
		if strings.Contains(stage, stack) {
			t.Errorf("the base stage carries %s, which the base's tools do not name", stack)
		}
	}
}

// holdTools holds sandboxTools and sandboxIncidental to the packages where
// installs: one installed and not named, or named and not installed, fails.
func holdTools(t *testing.T, where string, installed []string) {
	t.Helper()
	var named []string
	for _, tool := range sandboxTools {
		named = append(named, tool.apt...)
	}
	for _, p := range installed {
		if !slices.Contains(named, p) && !slices.Contains(sandboxIncidental, p) {
			t.Errorf("%s installs %s, which sandboxTools and sandboxIncidental do not name", where, p)
		}
	}
	for _, p := range append(named, sandboxIncidental...) {
		if !slices.Contains(installed, p) {
			t.Errorf("sandboxTools or sandboxIncidental names %s, which %s does not install", p, where)
		}
	}
}

// The brief states the image the session runs: the factory's, as before,
// or the base's tools, then the project image's, and its registries.
func TestSandboxFactsFollowTheImage(t *testing.T) {
	base := "The sandbox has git, curl, ripgrep (rg), ssh, gcc and make, python3, PostgreSQL 17 (server and client, for tests)"
	for name, c := range map[string]struct {
		m    *imagegen.Manifest
		want string
	}{
		"factory": {nil, base + ", Go 1.26, Node 24 with npm. It reaches the factory's gateway and Git server and " +
			"proxy.golang.org, sum.golang.org, storage.googleapis.com, registry.npmjs.org (the Go module proxy and the npm registry); every other name fails to resolve. " +
			"It has no browser and none can be installed, since the download hosts are not reachable: a check that needs a screenshot or a page in a browser cannot run here."},
		"browser": {&imagegen.Manifest{Tools: []string{"chromium", "fonts-liberation"}, Egress: []string{"cdn.example.org", "registry.npmjs.org"}},
			base + ", chromium, fonts-liberation. It reaches the factory's gateway and Git server and cdn.example.org, registry.npmjs.org; every other name fails to resolve. " +
				"A tool it lacks cannot be installed: the download hosts are not reachable."},
		"offline": {&imagegen.Manifest{Tools: []string{"jq"}},
			base + ", jq. It reaches the factory's gateway and Git server and no package registries; every other name fails to resolve. " +
				"A tool it lacks cannot be installed: the download hosts are not reachable."},
	} {
		if got := SandboxFacts(c.m); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if got := briefText(harness.TaskBrief{Objective: "o", Image: c.m}, "T"); !strings.Contains(got, c.want+"\n") {
			t.Errorf("%s: the brief lacks its facts:\n%s", name, got)
		}
	}
}

// imageRunner is a runner whose policy in force pins demo-app's browser
// image, which the images key signed, and whose imager verifies it; the
// digest is the image's.
func imageRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	r, digest, _ := signedImageRunner(t, nil)
	return r, digest
}

// signedImageRunner is imageRunner with the image signed by signer, the
// images key for nil, and the imager's rig.
func signedImageRunner(t *testing.T, signer *ecdsa.PrivateKey) (*Runner, string, *imagerRig) {
	t.Helper()
	rig := newImagerRig(t, "", nil)
	digest := rig.publish("sf/sandbox-demo-app.browser", keyBrowser, cmp.Or(signer, rig.key), 1000)
	v := rig.fj.version()
	name, c, _ := v.ProjectOf("demo-app/demo-app")
	c.Images = map[string]policy.ImagePin{"browser": {Digest: digest, Key: keyBrowser, Tools: []string{"chromium"}, Egress: []string{"cdn.example.org"}}}
	v.Projects[name] = c
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	return &Runner{cfg: cfg, Imager: rig.im}, digest, rig
}

// A card runs on the image it names, else on default, as the policy in
// force pins it and the images key signed it; an unpinned default runs
// the factory's image, and an unpinned variant is refused for good: it
// waits for its image.
func TestTaskImage(t *testing.T) {
	r, digest := imageRunner(t)
	ctx := context.Background()
	image, m, err := r.taskImage(ctx, Task{ID: "t1", Repository: "demo-app/demo-app", Image: "browser"})
	want := &imagegen.Manifest{Tools: []string{"chromium"}, Egress: []string{"cdn.example.org"}}
	if err != nil || image != "store.test/sf/sandbox-demo-app.browser@"+digest || !reflect.DeepEqual(m, want) {
		t.Fatalf("pinned: %q %+v %v, want the store's image at the pin and its manifest", image, m, err)
	}
	for _, task := range []Task{{ID: "t1", Repository: "demo-app/demo-app"}, {ID: "t1", Repository: "demo-app/demo-app", Image: "default"}} {
		if image, m, err := r.taskImage(ctx, task); err != nil || image != "reg/worker@sha256:abc" || m != nil {
			t.Fatalf("default unpinned (%q): %q %+v %v, want the factory's image", task.Image, image, m, err)
		}
	}
	_, _, err = r.taskImage(ctx, Task{ID: "t1", Repository: "demo-app/demo-app", Image: "video"})
	var notPinned ErrImageNotPinned
	var app *temporal.ApplicationError
	if !errors.As(err, &notPinned) || notPinned != (ErrImageNotPinned{Project: "demo-app", Image: "video"}) ||
		!errors.As(err, &app) || !app.NonRetryable() || app.Type() != errImageNotPinned {
		t.Fatalf("variant unpinned: %v, want ErrImageNotPinned as a non-retryable activity error", err)
	}
	if want := "image video of project demo-app is declared but not built and pinned yet: the card waits until the factory pins it"; errMessage(err) != want {
		t.Fatalf("variant unpinned says %q, want %q", errMessage(err), want)
	}
	if _, _, err := r.taskImage(ctx, Task{ID: "t1", Repository: "linkshort/linkshort", Image: "browser"}); !errors.As(err, &notPinned) {
		t.Fatalf("another project's image: %v, want ErrImageNotPinned", err)
	}
}

// A pin whose digest the images key did not sign runs no sandbox: the
// refusal is final, and one alert per image and digest says so.
func TestTaskImageRefusesAnUnsignedPin(t *testing.T) {
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, digest, rig := signedImageRunner(t, other)
	task := Task{ID: "t1", Repository: "demo-app/demo-app", Image: "browser"}
	for range 2 {
		if _, _, err := r.taskImage(context.Background(), task); !isTerminal(err) || !strings.Contains(err.Error(), "image browser of project demo-app is pinned at "+digest) {
			t.Fatalf("taskImage = %v, want a terminal refusal naming the image and its digest", err)
		}
	}
	want := []Alert{{Kind: AlertImage, Source: "unsigned", Fingerprint: "unsigned sf/sandbox-demo-app.browser@" + digest,
		Detail: map[string]any{"project": "demo-app", "image": "browser", "digest": digest}}}
	if !reflect.DeepEqual(rig.book.alerts, want) {
		t.Fatalf("alerts %+v, want %+v", rig.book.alerts, want)
	}
}

// Every pod of a card runs on the image its sandbox resolved, which the
// task row records with the names the sandbox reaches: the image's own,
// beside the gateway's and Git's, held by the lease loop too. An unpinned
// variant leaves no namespace.
func TestSandboxRunsOnTheCardsImage(t *testing.T) {
	r, digest := imageRunner(t)
	ref := "store.test/sf/sandbox-demo-app.browser@" + digest
	ctx := context.Background()
	kube := newFakeKube()
	r.kube = kube
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	store := newFakeStore()
	store.projects = []Project{{Name: "demo-app", Repository: "demo-app/demo-app"}}
	r.store = store
	r.forgejo = &Forgejo{BotUser: "factory-agent"}
	task := Task{ID: "t1", Repository: "demo-app/demo-app", Image: "browser", Status: "running"}
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	cache, m, err := r.setupNamespace(ctx, &task, ForgejoToken{})
	if err != nil || cache != "cache-agent-demo-app" || !reflect.DeepEqual(m, &imagegen.Manifest{Tools: []string{"chromium"}, Egress: []string{"cdn.example.org"}}) {
		t.Fatalf("setupNamespace = %q %+v %v", cache, m, err)
	}
	if row := store.tasks["t1"]; task.ImageDigest != ref || row.ImageDigest != ref || !slices.Equal(row.ImageEgress, []string{"cdn.example.org"}) {
		t.Fatalf("task %q, row %q %q; want %q reaching cdn.example.org", task.ImageDigest, row.ImageDigest, row.ImageEgress, ref)
	}
	pods := map[string]map[string]any{
		"session": r.workerPodSpec(task, sessionSpec{id: "s-1", cache: cache, image: m}),
		"repro":   r.reproPodSpec(task, Repo{Owner: "demo-app", Name: "demo-app"}, pullRef(1), "c1", ""),
	}
	for name, pod := range pods {
		spec := pod["spec"].(map[string]any)
		inits := spec["initContainers"].([]any)
		if got := inits[0].(map[string]any)["image"]; got != "reg/worker@sha256:abc" {
			t.Errorf("%s: harness on %v, want the factory's image", name, got)
		}
		for _, c := range slices.Concat(inits[1:], spec["containers"].([]any)) {
			if got := c.(map[string]any)["image"]; got != ref {
				t.Errorf("%s: container %v on %v, want %s", name, c.(map[string]any)["name"], got, ref)
			}
		}
	}
	worker := pods["session"]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if got := containerEnv(worker)["IMAGE_MANIFEST"]; got != `{"tools":["chromium"],"egress":["cdn.example.org"]}` {
		t.Errorf("session IMAGE_MANIFEST = %q, want the image's manifest", got)
	}
	fqdns, dns := egressNames(kube.cnps["factory-agents-t1/worker-egress"])
	if !slices.Equal(fqdns, []string{"cdn.example.org"}) ||
		!slices.Equal(dns, []string{"factory-gateway.factory.svc.cluster.local", "forgejo-http.forgejo.svc.cluster.local", "cdn.example.org"}) {
		t.Fatalf("egress reaches %q, resolves %q; want the image's registry beside the gateway and Git", fqdns, dns)
	}
	if err := r.AdmitLeases(ctx); err != nil || len(kube.applies) != 1 {
		t.Fatalf("the lease loop applied %v (%v), want the image's egress kept", kube.applies, err)
	}

	waiting := Task{ID: "t2", Repository: "demo-app/demo-app", Image: "video"}
	var notPinned ErrImageNotPinned
	if _, _, err := r.setupNamespace(ctx, &waiting, ForgejoToken{}); !errors.As(err, &notPinned) || kube.createdNs["factory-agents-t2"] != nil {
		t.Fatalf("unpinned variant: %v, namespace %v; want ErrImageNotPinned before any namespace", err, kube.createdNs["factory-agents-t2"])
	}
}

// An image whose egress names the egress canary's host would fail every
// sandbox's egress probe: its sandbox is refused for good, naming the
// image and the host, before any namespace.
func TestSandboxRefusesAnImageReachingTheCanary(t *testing.T) {
	r, _ := imageRunner(t)
	kube := newFakeKube()
	r.kube, r.store = kube, newFakeStore()
	_, c, _ := r.cfg.Policy().ProjectOf("demo-app/demo-app")
	pin := c.Images["browser"]
	pin.Egress = []string{"cdn.example.org", "Example.com."}
	c.Images["browser"] = pin
	task := Task{ID: "t1", Repository: "demo-app/demo-app", Image: "browser"}
	r.store.CreateTask(context.Background(), task)
	_, _, err := r.setupNamespace(context.Background(), &task, ForgejoToken{})
	if !isTerminal(err) || !strings.Contains(err.Error(), "image browser") || !strings.Contains(err.Error(), "example.com, the egress canary") || len(kube.createdNs) != 0 {
		t.Fatalf("setupNamespace = %v, namespaces %v; want a terminal refusal naming the image and the canary, no namespace", err, kube.createdNs)
	}
}

// An image that declares no egress reaches no registry: its policy holds
// no 443 rule (an empty toFQDNs would admit 443 to every destination), and
// its DNS answers the gateway, Git and the collector only.
func TestEgressLessImageReachesNoRegistry(t *testing.T) {
	cfg := testRunnerConfig()
	cfg.OTLPEndpoint = testCollector
	spec := (&Runner{cfg: cfg}).workerEgressSpec(nil, nil)
	for _, rule := range spec["egress"].([]any) {
		if _, ok := rule.(map[string]any)["toFQDNs"]; ok {
			t.Errorf("a toFQDNs rule: %v", rule)
		}
		if strings.Contains(string(mustJSON(rule)), `"port":"443"`) {
			t.Errorf("a 443 rule: %v", rule)
		}
	}
	if _, dns := egressNames(spec); !slices.Equal(dns, []string{"factory-gateway.factory.svc.cluster.local",
		"forgejo-http.forgejo.svc.cluster.local", "signoz-otel-collector.platform.svc.cluster.local"}) {
		t.Fatalf("DNS answers %q, want the gateway, Git and the collector", dns)
	}
}

// egressNames are the names spec's egress reaches on 443 and those its DNS
// rule answers.
func egressNames(spec map[string]any) (fqdns, dns []string) {
	for _, rule := range spec["egress"].([]any) {
		rule := rule.(map[string]any)
		fq, _ := rule["toFQDNs"].([]any)
		for _, n := range fq {
			fqdns = append(fqdns, n.(map[string]any)["matchName"].(string))
		}
		if ports, ok := rule["toPorts"].([]any); ok {
			if rules, ok := ports[0].(map[string]any)["rules"].(map[string]any); ok {
				for _, n := range rules["dns"].([]any) {
					dns = append(dns, n.(map[string]any)["matchName"].(string))
				}
			}
		}
	}
	return fqdns, dns
}

func TestSandboxFactsNameToolsNamesAndNoBrowser(t *testing.T) {
	got := SandboxFacts(nil)
	for _, want := range []string{"git", "ripgrep (rg)", "gcc and make", "Go 1.26", "Node 24 with npm", "PostgreSQL 17",
		"proxy.golang.org", "registry.npmjs.org", "no browser"} {
		if !strings.Contains(got, want) {
			t.Errorf("SandboxFacts lacks %q:\n%s", want, got)
		}
	}
}

// The harness init container copies what the factory's image holds:
// each path harnessCopy reads is where Dockerfile.sandbox installs it.
func TestHarnessCopyReadsWhatTheImageInstalls(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, install := range []string{
		"install -m 0755 /tmp/opencode /usr/local/bin/opencode",
		"COPY --from=build /out/agent-worker /usr/local/bin/agent-worker",
		"COPY --from=opencode-plugin --chown=1000:1000 /opencode-home/.config/opencode /opt/opencode-home/.config/opencode",
	} {
		if !strings.Contains(string(b), install) {
			t.Errorf("Dockerfile.sandbox lacks %q", install)
		}
	}
	for _, read := range []string{"/usr/local/bin/opencode ", "/usr/local/bin/agent-worker ", "cp -a /opt/opencode-home /factory/"} {
		if !strings.Contains(harnessCopy, read) {
			t.Errorf("harnessCopy %q does not read %q", harnessCopy, read)
		}
	}
}

// opencode's switches belong to the harness, not to an image: a session's
// worker sets the ones that keep opencode from phoning home past the
// egress allowlist, whatever image it runs, and Dockerfile.sandbox sets
// none of them.
func TestSessionPodTurnsOpencodesFetchesOff(t *testing.T) {
	pod := (&Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(Task{ID: "t1"}, sessionSpec{id: "s-1"})
	env := containerEnv(pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))
	for _, name := range []string{"OPENCODE_DISABLE_AUTOUPDATE", "OPENCODE_DISABLE_MODELS_FETCH", "OPENCODE_DISABLE_LSP_DOWNLOAD"} {
		if env[name] != "1" {
			t.Errorf("session worker env %s = %q, want 1", name, env[name])
		}
	}
	b, err := os.ReadFile("../../Dockerfile.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^\s*(?:ENV\s+)?OPENCODE_[A-Z_]+=.*$`).FindAllString(string(b), -1); len(m) != 0 {
		t.Errorf("Dockerfile.sandbox sets opencode's env %q: a project image FROM the base would lack it", m)
	}
}
