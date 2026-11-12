package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const factorydImage = "git.example/sf/factory-gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// copyPodKube runs the copy pod's program in the test when its pod is
// created: it reads the pod's login Secret as the pod's mount would and
// copies against the test's registries.
type copyPodKube struct {
	*fakeKube
	run     func(secret map[string]string) (log string, exit int32)
	created []string
	status  map[string]int32
	logs    map[string]string
}

func (k *copyPodKube) CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error {
	if err := k.fakeKube.CreatePod(ctx, namespace, name, pod); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.created = append(k.created, namespace)
	log, exit := k.run(k.secrets[namespace+"/"+upstreamSecretName])
	k.logs[namespace], k.status[namespace] = log, exit
	return nil
}

func (k *copyPodKube) PodStatus(ctx context.Context, namespace, name string) (string, int32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	exit, ok := k.status[namespace]
	if !ok {
		return "", -1, ErrNotFound
	}
	if exit != 0 {
		return "Failed", exit, nil
	}
	return "Succeeded", 0, nil
}

func (k *copyPodKube) PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	all := strings.SplitAfter(k.logs[namespace], "\n")
	return strings.Join(all[max(0, len(all)-lines):], ""), nil
}

// pod is an UpstreamPod whose pod runs the copy against s and up.
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
	return &UpstreamPod{
		Kube: k, Namespace: "factory", PullSecret: "forgejo-registry", StoreURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		OpenBao: &fakeKV{data: map[string]string{"username": storeAdminUser, "password": storeAdminPw}},
		Poll:    time.Millisecond,
	}
}

func podKube(u *UpstreamPod) *copyPodKube { return u.Kube.(*copyPodKube) }

func TestUpstreamPodSpecHoldsOnlyTheStoreLoginAndTheRegistryEgress(t *testing.T) {
	im := newFakeImage("spec")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	k := podKube(u)
	var spec map[string]any
	k.run = func(secret map[string]string) (string, int32) {
		// the namespace is live while the pod runs
		for ns := range k.namespaces {
			spec = k.pods[ns+"/"+upstreamPodName].spec
			if got := k.secrets[ns+"/"+upstreamSecretName]; got["password"] != storeAdminPw || got["user"] != storeAdminUser || got["lines"] != goLine(im.digest) {
				t.Errorf("login secret = %v", got)
			}
			if !strings.HasPrefix(ns, "factory-agents-upstream-") {
				t.Errorf("namespace %q lacks the prefix", ns)
			}
			labels := k.namespaces[ns]
			if labels["pod-security.kubernetes.io/enforce"] != "restricted" || !strings.HasPrefix(labels["factory.ai/task"], "upstream-") || labels[UpstreamLabel] != "true" {
				t.Errorf("labels = %v", labels)
			}
			cnp := k.cnps[ns+"/upstream-egress"]
			body, _ := json.Marshal(cnp)
			for _, host := range upstreamFQDNs {
				name := host.(map[string]any)
				for _, v := range name {
					if !strings.Contains(string(body), `"`+v.(string)+`"`) {
						t.Errorf("egress policy lacks %s", v)
					}
				}
			}
			egress := cnp["egress"].([]any)
			fqdn := egress[1].(map[string]any)
			if ports := fqdn["toPorts"].([]any)[0].(map[string]any)["ports"].([]any); len(ports) != 1 || ports[0].(map[string]any)["port"] != "443" {
				t.Errorf("registry ports = %v", ports)
			}
			if !strings.Contains(string(body), "forgejo-http.forgejo.svc.cluster.local") {
				t.Error("the store's name is not in the DNS rule")
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
		t.Errorf("env = %v, want none: the login is a file", c["env"])
	}
	raw, _ := json.Marshal(spec)
	if strings.Contains(string(raw), storeAdminPw) {
		t.Error("the stored pod spec carries the store's password")
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

func TestUpstreamPodFailedCopyReachesTheCallerWithItsScrubbedReason(t *testing.T) {
	im := newFakeImage("fails")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	podKube(u).run = func(map[string]string) (string, int32) {
		return "copying\nupstream copy: put https://root:hunter2hunter2@store/v2 Authorization: Bearer abcdefgh12345678 " + storeAdminPw + "\n", 3
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
	for _, leak := range []string{"hunter2hunter2", "abcdefgh12345678", storeAdminPw} {
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

func TestUpstreamPodSweepsANamespaceAnEarlierRunLeft(t *testing.T) {
	im := newFakeImage("sweep")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	u := store.pod(t, up)
	k := podKube(u)
	old := fmt.Sprintf("factory-agents-upstream-%d-aaaa", time.Now().Add(-2*time.Hour).Unix())
	young := fmt.Sprintf("factory-agents-upstream-%d-bbbb", time.Now().Unix())
	for _, ns := range []string{old, young} {
		k.namespaces[ns] = map[string]string{UpstreamLabel: "true"}
	}
	if _, err := u.Copy(t.Context(), "", goLine(im.digest)+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.namespaces[old]; ok {
		t.Error("the old namespace stays")
	}
	if _, ok := k.namespaces[young]; !ok {
		t.Error("another run's live namespace was deleted")
	}
}
