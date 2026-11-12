package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	factorydImage = "git.example/sf/factory-gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	forgeAdminPw  = "forge-admin-password-987654"
)

// tokenForgejo is the site admin's token API: mint, list and revoke, with
// the admin's login checked.
type tokenForgejo struct {
	mu      sync.Mutex
	live    []tokenRec
	minted  []tokenRec
	revoked []string
	badAuth int
	next    int64
	srv     *httptest.Server
}

type tokenRec struct {
	ID     int64
	Name   string
	Scopes []string
}

func newTokenForgejo(t *testing.T) *tokenForgejo {
	t.Helper()
	f := &tokenForgejo{}
	f.srv = httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if u, p, _ := r.BasicAuth(); u != storeAdminUser || p != forgeAdminPw {
			f.badAuth++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tokens"):
			var body tokenRec
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.next++
			body.ID = f.next
			f.live = append(f.live, body)
			f.minted = append(f.minted, body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"sha1":%q}`, body.ID, storeToken)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tokens"):
			_ = json.NewEncoder(w).Encode(f.live)
		case r.Method == http.MethodDelete:
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			f.live = slices.DeleteFunc(f.live, func(t tokenRec) bool {
				if t.ID == id {
					f.revoked = append(f.revoked, t.Name)
				}
				return t.ID == id
			})
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *tokenForgejo) liveNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.live {
		out = append(out, t.Name)
	}
	return out
}

// copyPodKube runs the copy pod's program in the test when its pod is
// created: it reads the pod's login Secret as the pod's mount would and
// copies against the test's registries.
type copyPodKube struct {
	*fakeKube
	run       func(secret map[string]string) (log string, exit int32)
	created   []string
	status    map[string]int32
	logs      map[string]string
	pending   bool // the pod never starts
	diagnosis PodDiagnosis
}

func (k *copyPodKube) CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error {
	if err := k.fakeKube.CreatePod(ctx, namespace, name, pod); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.created = append(k.created, namespace)
	if k.pending {
		return nil
	}
	log, exit := k.run(k.secrets[namespace+"/"+upstreamSecretName])
	k.logs[namespace], k.status[namespace] = log, exit
	return nil
}

func (k *copyPodKube) PodStatus(ctx context.Context, namespace, name string) (string, int32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending {
		return "Pending", -1, nil
	}
	exit, ok := k.status[namespace]
	if !ok {
		return "", -1, ErrNotFound
	}
	if exit != 0 {
		return "Failed", exit, nil
	}
	return "Succeeded", 0, nil
}

func (k *copyPodKube) PodDiagnosis(ctx context.Context, namespace, name string) (PodDiagnosis, error) {
	return k.diagnosis, nil
}

func (k *copyPodKube) PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	all := strings.SplitAfter(k.logs[namespace], "\n")
	return strings.Join(all[max(0, len(all)-lines):], ""), nil
}

// pod is an UpstreamPod whose pod runs the copy against s and up, with the
// site admin's login on a token API of its own.
func (s *fakeImgStore) pod(t *testing.T, up *fakeUpstream) *UpstreamPod {
	t.Helper()
	k := &copyPodKube{fakeKube: newFakeKube(), status: map[string]int32{}, logs: map[string]string{}}
	k.deployments = map[string]string{"factory/factoryd": `{"spec":{"template":{"spec":{"containers":[{"image":"` + factorydImage + `"}]}}}}`}
	k.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	k.run = func(secret map[string]string) (string, int32) {
		c := &UpstreamCopier{
			Registry: &Registry{BaseURL: s.srv.URL, User: secret["user"], Password: secret["password"]},
			Endpoint: func(string) string { return up.srv.URL },
		}
		findings, err := c.Copy(context.Background(), strings.Split(secret["lines"], "\n"))
		if err != nil {
			return "upstream copy: " + err.Error() + "\n", 1
		}
		var out strings.Builder
		for _, f := range findings {
			out.WriteString(UpstreamFinding + f + "\n")
		}
		return out.String(), 0
	}
	tf := newTokenForgejo(t)
	tokenServers[tf.srv.URL] = tf
	return &UpstreamPod{
		Kube: k, Namespace: "factory", PullSecret: "forgejo-registry", StoreURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Forgejo: &Forgejo{BaseURL: tf.srv.URL},
		OpenBao: &fakeKV{data: map[string]string{"username": storeAdminUser, "password": forgeAdminPw}},
		Poll:    time.Millisecond,
	}
}

func podKube(u *UpstreamPod) *copyPodKube { return u.Kube.(*copyPodKube) }

// tokens is the token API behind u.Forgejo.
func tokens(t *testing.T, u *UpstreamPod) *tokenForgejo {
	t.Helper()
	return tokenServers[u.Forgejo.BaseURL]
}

var tokenServers = map[string]*tokenForgejo{}

func shortRuns(t *testing.T) {
	t.Helper()
	d, g, s := upstreamDeadline, upstreamStartGrace, upstreamSweepSlack
	t.Cleanup(func() { upstreamDeadline, upstreamStartGrace, upstreamSweepSlack = d, g, s })
}

func TestUpstreamPodSpecHoldsOnlyTheMintedTokenAndTheRegistryEgress(t *testing.T) {
	im := newFakeImage("spec")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	k := podKube(u)
	var spec map[string]any
	k.run = func(secret map[string]string) (string, int32) {
		// the namespace is live while the pod runs
		for ns := range k.namespaces {
			spec = k.pods[ns+"/"+upstreamPodName].spec
			if got := k.secrets[ns+"/"+upstreamSecretName]; got["password"] != storeToken || got["user"] != storeAdminUser || got["lines"] != goLine(im.digest) || len(got) != 3 {
				t.Errorf("login secret = %v, want the minted token as the password", got)
			}
			if raw, _ := json.Marshal(k.secrets); strings.Contains(string(raw), forgeAdminPw) {
				t.Error("the admin's password reached a Secret")
			}
			if !strings.HasPrefix(ns, "factory-agents-upstream-") {
				t.Errorf("namespace %q lacks the prefix", ns)
			}
			labels := k.namespaces[ns]
			if labels["pod-security.kubernetes.io/enforce"] != "restricted" || !strings.HasPrefix(labels["factory.ai/task"], "upstream-") || labels[UpstreamLabel] != "true" {
				t.Errorf("labels = %v", labels)
			}
			cnp := k.cnps[ns+"/upstream-egress"]
			if _, ok := cnp["egressDeny"]; ok {
				t.Error("the policy denies by CIDR: its allowlist alone is the bound")
			}
			for _, r := range cnp["egress"].([]any) {
				for key := range r.(map[string]any) {
					if key != "toEndpoints" && key != "toFQDNs" && key != "toPorts" {
						t.Errorf("an egress rule of kind %s: only endpoints and names are admitted", key)
					}
				}
			}
		}
		return "", 0
	}
	if _, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n"); err != nil {
		t.Fatal(err)
	}
	if spec == nil {
		t.Fatal("no pod was created")
	}
	pod := spec["spec"].(map[string]any)
	if pod["runtimeClassName"] != "gvisor" || pod["automountServiceAccountToken"] != false {
		t.Errorf("runtimeClassName %v, automount %v", pod["runtimeClassName"], pod["automountServiceAccountToken"])
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["image"] != factorydImage {
		t.Errorf("image = %v, want factoryd's own %s", c["image"], factorydImage)
	}
	sc := c["securityContext"].(map[string]any)
	if sc["runAsNonRoot"] != true || sc["runAsUser"] == 0 || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext = %v", sc)
	}
	if _, ok := c["env"]; ok {
		t.Errorf("env = %v, want none: the token is a file", c["env"])
	}
	raw, _ := json.Marshal(spec)
	if strings.Contains(string(raw), storeToken) || strings.Contains(string(raw), forgeAdminPw) {
		t.Error("the stored pod spec carries a password")
	}
	var mount map[string]any
	for _, m := range c["volumeMounts"].([]any) {
		if m.(map[string]any)["mountPath"] == UpstreamSecretDir {
			mount = m.(map[string]any)
		}
	}
	if mount == nil || mount["readOnly"] != true {
		t.Fatalf("no read-only login mount in %v", c["volumeMounts"])
	}
	var secretVolume string
	for _, v := range pod["volumes"].([]any) {
		if v.(map[string]any)["name"] == mount["name"] {
			secretVolume = v.(map[string]any)["secret"].(map[string]any)["secretName"].(string)
		}
	}
	if secretVolume != upstreamSecretName {
		t.Errorf("the login mount reads secret %q", secretVolume)
	}
	if args := fmt.Sprint(c["args"]); !strings.Contains(args, "upstream-copy") || !strings.Contains(args, u.StoreURL) {
		t.Errorf("args = %s", args)
	}
}

func TestUpstreamPodMintsAPackageTokenAndRevokesItAtEachEnd(t *testing.T) {
	im := newFakeImage("tok")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	for name, c := range map[string]struct {
		exit    int32
		pending bool
	}{"success": {}, "failure": {exit: 1}, "timeout": {pending: true}} {
		t.Run(name, func(t *testing.T) {
			shortRuns(t)
			upstreamDeadline, upstreamStartGrace = 30*time.Millisecond, 30*time.Millisecond
			u := store.pod(t, up)
			f := tokenServers[u.Forgejo.BaseURL]
			k := podKube(u)
			k.pending = c.pending
			k.run = func(map[string]string) (string, int32) { return "", c.exit }
			_, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n")
			if (err != nil) != (c.exit != 0 || c.pending) {
				t.Fatalf("err = %v", err)
			}
			if len(f.minted) != 1 || !strings.HasPrefix(f.minted[0].Name, upstreamTokenPrefix) || !slices.Equal(f.minted[0].Scopes, []string{"write:package"}) {
				t.Fatalf("minted %+v, want one write:package token named %s...", f.minted, upstreamTokenPrefix)
			}
			if live := f.liveNames(); len(live) != 0 || !slices.Equal(f.revoked, []string{f.minted[0].Name}) {
				t.Errorf("live %v, revoked %v: want the minted token revoked", live, f.revoked)
			}
			if f.badAuth != 0 {
				t.Errorf("%d token calls without the admin's login", f.badAuth)
			}
		})
	}
}

func TestUpstreamPodRevokesALeftoverTokenByItsName(t *testing.T) {
	im := newFakeImage("left")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	f := tokenServers[u.Forgejo.BaseURL]
	old := fmt.Sprintf("%s%d-aaaa", upstreamTokenPrefix, time.Now().Add(-time.Hour).Unix())
	young := fmt.Sprintf("%s%d-bbbb", upstreamTokenPrefix, time.Now().Unix())
	f.live = []tokenRec{{ID: 90, Name: old}, {ID: 91, Name: young}, {ID: 92, Name: "registry"}}
	if _, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n"); err != nil {
		t.Fatal(err)
	}
	if live := f.liveNames(); !slices.Equal(live, []string{young, "registry"}) {
		t.Errorf("live tokens %v, want the old leftover revoked and the young and the other kept", live)
	}
}

func TestUpstreamPodNeverStartingPodEndsWithItsState(t *testing.T) {
	im := newFakeImage("pending")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	shortRuns(t)
	upstreamDeadline, upstreamStartGrace = 20*time.Millisecond, 20*time.Millisecond
	u := store.pod(t, up)
	k := podKube(u)
	k.pending = true
	k.diagnosis = PodDiagnosis{Phase: "Pending", Conditions: []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available"}}}
	_, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err == nil {
		t.Fatal("a pod that never started ended the copy without an error")
	}
	for _, want := range []string{"did not end within", "phase Pending", "Unschedulable: 0/3 nodes are available"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if len(k.namespaces) != 0 {
		t.Errorf("namespaces left: %v", k.namespaces)
	}
}

func TestUpstreamPodFailedCopyReachesTheCallerWithItsScrubbedReason(t *testing.T) {
	im := newFakeImage("fails")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	podKube(u).run = func(map[string]string) (string, int32) {
		return "copying\nupstream copy: put https://root:hunter2hunter2@store/v2 Authorization: Bearer abcdefgh12345678 " + storeToken + "\n", 3
	}
	_, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err == nil {
		t.Fatal("a failed copy returned no error")
	}
	for _, want := range []string{"exit 3", "failed", "upstream copy:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for _, leak := range []string{"hunter2hunter2", "abcdefgh12345678", storeToken} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q", err, leak)
		}
	}
}

func TestUpstreamPodNamespaceIsRemovedAfterSuccessAndAfterFailure(t *testing.T) {
	good, bad := newFakeImage("good"), newFakeImage("bad")
	up, store := newFakeUpstream(t, good, bad), newFakeImgStore(t)
	for name, exit := range map[string]int32{"success": 0, "failure": 1} {
		t.Run(name, func(t *testing.T) {
			u := store.pod(t, up)
			k := podKube(u)
			k.run = func(map[string]string) (string, int32) { return "", exit }
			_, err := u.Copy(t.Context(), "", goLine(good.digest)+"\n")
			if (err != nil) != (exit != 0) {
				t.Fatalf("err = %v, exit %d", err, exit)
			}
			if len(k.created) != 1 || len(k.namespaces) != 0 || len(k.deletedNs) != 1 || k.deletedNs[0] != k.created[0] {
				t.Errorf("created %v, live %v, deleted %v: want the one namespace gone", k.created, k.namespaces, k.deletedNs)
			}
		})
	}
}

func TestUpstreamPodFindingsComeBackFromThePodsLog(t *testing.T) {
	good, gone := newFakeImage("good"), newFakeImage("gone")
	up, store := newFakeUpstream(t, good), newFakeImgStore(t)
	u := store.pod(t, up)
	findings, err := u.Copy(t.Context(), "", goLine(good.digest)+"\n"+goLine(gone.digest)+"\n")
	if want := goLine(gone.digest) + ": the upstream does not serve this digest"; err != nil || len(findings) != 1 || findings[0] != want {
		t.Fatalf("findings %q, err %v; want [%q]", findings, err, want)
	}
}

func TestUpstreamPodScrubsTheFindingsItReadsBack(t *testing.T) {
	im := newFakeImage("leaky")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	podKube(u).run = func(map[string]string) (string, int32) {
		return UpstreamFinding + "a line: get https://root:hunter2hunter2@store/v2 Authorization: Bearer abcdefgh12345678 " + storeToken + "\n", 0
	}
	findings, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings %q, err %v", findings, err)
	}
	for _, leak := range []string{"hunter2hunter2", "abcdefgh12345678", storeToken} {
		if strings.Contains(findings[0], leak) {
			t.Errorf("finding %q carries %q", findings[0], leak)
		}
	}
}

func TestUpstreamPodRefusesAnImageWithoutADigest(t *testing.T) {
	im := newFakeImage("nodigest")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	k := podKube(u)
	k.deployments["factory/factoryd"] = `{"spec":{"template":{"spec":{"containers":[{"image":"git.example/sf/factory-gateway:main"}]}}}}`
	_, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err == nil || !strings.Contains(err.Error(), "git.example/sf/factory-gateway:main") || !strings.Contains(err.Error(), "@sha256:") {
		t.Fatalf("err = %v, want a refusal naming the image and the digest pin", err)
	}
	if len(k.created) != 0 || len(tokens(t, u).minted) != 0 {
		t.Errorf("a run began: pods %v, tokens %v", k.created, tokens(t, u).minted)
	}
}

func TestUpstreamPodSweepsOnlyNamespacesOlderThanTheLongestRun(t *testing.T) {
	up, store := newFakeUpstream(t), newFakeImgStore(t)
	u := store.pod(t, up)
	k := podKube(u)
	ago := func(d time.Duration, tag string) string {
		return fmt.Sprintf("factory-agents-upstream-%d-%s", time.Now().Add(-d).Unix(), tag)
	}
	// the longest run is 27 minutes; its sweep age is 32
	old, justUnder, young := ago(40*time.Minute, "aaaa"), ago(30*time.Minute, "bbbb"), ago(0, "cccc")
	for _, ns := range []string{old, justUnder, young} {
		k.namespaces[ns] = map[string]string{UpstreamLabel: "true"}
	}
	u.Sweep(t.Context(), false)
	for ns, want := range map[string]bool{old: false, justUnder: true, young: true} {
		if _, ok := k.namespaces[ns]; ok != want {
			t.Errorf("namespace %s live = %v, want %v", ns, ok, want)
		}
	}
}

// The CI spawner's reconcile pass runs the upstream sweep: a leftover goes
// in the next pass.
func TestCISpawnerReconcileSweepsALeftoverUpstreamNamespace(t *testing.T) {
	kube := newFakeKube()
	spawner := newTestCISpawner(t, kube, &ciHub{})
	im := newFakeImage("x")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	k := podKube(u)
	old := fmt.Sprintf("factory-agents-upstream-%d-aaaa", time.Now().Add(-time.Hour).Unix())
	k.namespaces[old] = map[string]string{UpstreamLabel: "true"}
	spawner.SweepEachPass(func(ctx context.Context) { u.Sweep(ctx, false) })
	spawner.reconcile(t.Context(), codeCIRepos)
	if _, ok := k.namespaces[old]; ok {
		t.Error("the leftover namespace survived a reconcile pass")
	}
}

func TestAdmitLeasesSkipsAnUpstreamCopyNamespace(t *testing.T) {
	kube := newFakeKube()
	kube.namespaces["factory-agents-upstream-1700000000-abcd"] = map[string]string{"factory.ai/task": "upstream-1700000000-abcd"}
	kube.namespaces["factory-agents-t1"] = map[string]string{"factory.ai/task": "t1"}
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{}}
	r := &Runner{cfg: cfg, kube: kube, store: newFakeStore()}
	if err := r.AdmitLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kube.applies, ","); got != "factory-agents-t1/worker-egress" {
		t.Errorf("applies = %s, want the task's alone", got)
	}
}

// The hosts the copy reached in #1698's real runs: the six registries of
// upstream-images.txt, their token endpoints and the hosts their blobs
// redirect to.
var upstreamHostsSeen = []string{
	"registry-1.docker.io", "auth.docker.io", "production.cloudfront.docker.com", "production.cloudflare.docker.com",
	"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com",
	"ghcr.io", "pkg-containers.githubusercontent.com",
	"quay.io", "cdn01.quay.io", "cdn02.quay.io",
	"registry.k8s.io", "cdn.registry.k8s.io",
	"code.forgejo.org",
	"factory.talos.dev", "cdn.factory.talos.dev",
	"europe-west3-docker.pkg.dev", "us-west1-docker.pkg.dev",
}

// podEgressFor is whether the pod's DNS rule resolves host and an FQDN rule
// admits it on 443/TCP alone, by matchName or matchPattern (Cilium's `*` is
// the characters of one DNS label).
func podEgressFor(t *testing.T, host string) (resolves, admits bool) {
	t.Helper()
	raw, err := json.Marshal(upstreamEgressSpec("forgejo-http.forgejo.svc.cluster.local"))
	if err != nil {
		t.Fatal(err)
	}
	type name struct{ MatchName, MatchPattern string }
	type ports []struct {
		Ports []struct{ Port, Protocol string }
		Rules struct{ DNS []name }
	}
	var spec struct {
		Egress []struct {
			ToFQDNs []name
			ToPorts ports
		}
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	has := func(ns []name) bool {
		return slices.ContainsFunc(ns, func(n name) bool {
			if n.MatchPattern == "" {
				return n.MatchName == host
			}
			return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(n.MatchPattern), `\*`, "[-a-zA-Z0-9_]*") + "$").MatchString(host)
		})
	}
	for _, rule := range spec.Egress {
		for _, p := range rule.ToPorts {
			resolves = resolves || has(p.Rules.DNS)
		}
		if has(rule.ToFQDNs) {
			admits = len(rule.ToPorts) == 1 && len(rule.ToPorts[0].Ports) == 1 &&
				rule.ToPorts[0].Ports[0].Port == "443" && rule.ToPorts[0].Ports[0].Protocol == "TCP"
		}
	}
	return resolves, admits
}

func TestUpstreamPodEgressAdmitsTheRegistriesOf1698ByName(t *testing.T) {
	list, err := os.ReadFile("../../upstream-images.txt")
	if err != nil {
		t.Fatal(err)
	}
	hosts := slices.Clone(upstreamHostsSeen)
	for _, l := range strings.Split(string(list), "\n") {
		if f := strings.Fields(l); len(f) > 0 && !strings.HasPrefix(f[0], "#") && !strings.HasPrefix(f[0], "https://") {
			host, _, _ := strings.Cut(f[0], "/")
			if host == "docker.io" {
				host = "registry-1.docker.io"
			}
			hosts = append(hosts, host)
		}
	}
	for _, h := range hosts {
		t.Run(h, func(t *testing.T) {
			if resolves, admits := podEgressFor(t, h); !resolves || !admits {
				t.Errorf("the pod's policy resolves %s %v, admits it on 443/TCP alone %v", h, resolves, admits)
			}
		})
	}
}

// The patterns admit names under them, in the DNS rule and the 443 rule
// alike, and no name outside them.
func TestUpstreamPodEgressPatternsAdmitTheirCDNsAlone(t *testing.T) {
	for name, want := range map[string]bool{
		"cdn01.quay.io": true, "cdn02.quay.io": true, "europe-west3-docker.pkg.dev": true, "us-west1-docker.pkg.dev": true,
		"evil.quay.io": false, "cdn01.quay.io.evil.example": false, "docker.pkg.dev": false, "x.europe-west3-docker.pkg.dev": false, "evil.dev": false,
	} {
		if resolves, admits := podEgressFor(t, name); resolves != want || admits != want {
			t.Errorf("%s: resolves %v, admits %v, want both %v", name, resolves, admits, want)
		}
	}
}

func TestUpstreamPodEgressReachesTheStoreOnItsHTTPPort(t *testing.T) {
	raw, _ := json.Marshal(upstreamEgressSpec("forgejo-http.forgejo.svc.cluster.local"))
	for _, want := range []string{`"k8s:io.kubernetes.pod.namespace":"forgejo"`, `"k8s:app.kubernetes.io/name":"forgejo"`, `"port":"3000"`, `forgejo-http.forgejo.svc.cluster.local`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the pod's policy lacks %s: %s", want, raw)
		}
	}
}
