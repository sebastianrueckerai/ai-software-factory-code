package factoryd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// k3sCluster is a client on the API server FACTORYD_APISERVER (host:port),
// the cluster running local-path v0.0.31 with the infra repository's
// StorageClass factory-cache and k8s/factoryd.yaml's RBAC and admission
// policies. It acts as an admin (token and CA in FACTORYD_APISERVER_ADMIN)
// after asAdmin, as factoryd (token and CA in FACTORYD_APISERVER_SA) after
// asFactoryd.
type k3sCluster struct {
	*InClusterKube
	asAdmin, asFactoryd func()
}

func newK3sCluster(t *testing.T) k3sCluster {
	t.Helper()
	addr, admin, sa := os.Getenv("FACTORYD_APISERVER"), os.Getenv("FACTORYD_APISERVER_ADMIN"), os.Getenv("FACTORYD_APISERVER_SA")
	if addr == "" || admin == "" || sa == "" {
		t.Skip("FACTORYD_APISERVER, FACTORYD_APISERVER_ADMIN and FACTORYD_APISERVER_SA not set")
	}
	host, port, _ := net.SplitHostPort(addr)
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	oldDir := saDir
	saDir = admin
	t.Cleanup(func() { saDir = oldDir })
	kube, err := NewInClusterKube()
	if err != nil {
		t.Fatal(err)
	}
	return k3sCluster{InClusterKube: kube, asAdmin: func() { saDir = admin }, asFactoryd: func() { saDir = sa }}
}

// taskNamespace is a namespace factoryd makes as for a session, with its
// RoleBinding and the cache claim; the admin deletes it at the end.
func (c k3sCluster) taskNamespace(t *testing.T, ns, claim string) {
	t.Helper()
	ctx := context.Background()
	c.asFactoryd()
	defer c.asAdmin()
	if err := c.CreateNamespace(ctx, ns, map[string]string{"factory.ai/task": "t113", "pod-security.kubernetes.io/enforce": "restricted"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.asAdmin()
		c.DeleteNamespace(context.Background(), ns)
	})
	if err := c.Create(ctx, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+ns+"/rolebindings", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "factoryd"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "factoryd-task"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": "factory"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := retryForbidden(ctx, func() error {
		return c.Create(ctx, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims", cacheClaim(claim))
	}); err != nil {
		t.Fatalf("factoryd's cache claim in %s: %v", ns, err)
	}
}

// notePod runs script as uid 1000 with claim at /cache, as the admin (a
// runc pod: factoryd's own must run under gVisor).
func (c k3sCluster) notePod(t *testing.T, ns, name, claim, script string) {
	t.Helper()
	ctx := context.Background()
	c.asAdmin()
	if err := c.CreatePod(ctx, ns, name, map[string]any{"spec": map[string]any{
		"restartPolicy":   "Never",
		"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers": []any{map[string]any{
			"name": "c", "image": "busybox:1.37", "command": []any{"sh", "-c", script},
			"volumeMounts": cacheMounts[:1],
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000,
				"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
		}},
		"volumes": cacheVolumes(claim)[:1],
	}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		phase, _, err := c.PodStatus(ctx, ns, name)
		if err != nil {
			t.Fatalf("pod %s/%s: %v", ns, name, err)
		}
		switch phase {
		case "Succeeded":
			return
		case "Failed":
			log, _ := c.PodLogTail(ctx, ns, name, 20)
			t.Fatalf("pod %s/%s failed: %s", ns, name, log)
		}
	}
	d, _ := c.PodDiagnosis(ctx, ns, name)
	t.Fatalf("pod %s/%s did not end: %+v", ns, name, d)
}

// claimedVolumes are the volumes whose claim lives in namespace.
func (c k3sCluster) claimedVolumes(t *testing.T, namespace string) []PersistentVolume {
	t.Helper()
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				StorageClassName string `json:"storageClassName"`
				ClaimRef         struct {
					Namespace string `json:"namespace"`
				} `json:"claimRef"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.getJSON(context.Background(), "/api/v1/persistentvolumes", &list); err != nil {
		t.Fatal(err)
	}
	var out []PersistentVolume
	for _, it := range list.Items {
		if it.Spec.ClaimRef.Namespace == namespace {
			out = append(out, PersistentVolume{Name: it.Metadata.Name, StorageClass: it.Spec.StorageClassName, Phase: it.Status.Phase})
		}
	}
	return out
}

// removeVolumes deletes, as the admin, the retained volumes of claims in
// the namespaces.
func (c k3sCluster) removeVolumes(t *testing.T, namespaces ...string) {
	t.Cleanup(func() {
		c.asAdmin()
		for _, ns := range namespaces {
			for _, pv := range c.claimedVolumes(t, ns) {
				c.DeletePersistentVolume(context.Background(), pv.Name)
			}
		}
	})
}

func testRun() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// TestCachePathSharedAcrossNamespaces: a claim of class factory-cache lands
// in the node directory named after the claim, so the note a pod writes
// through cache-x in one namespace is read through cache-x in another.
func TestCachePathSharedAcrossNamespaces(t *testing.T) {
	c := newK3sCluster(t)
	run := testRun()
	note := "note-" + run
	nsA, nsB := "factory-agents-t113-a-"+run, "factory-agents-t113-b-"+run
	c.removeVolumes(t, nsA, nsB)
	c.taskNamespace(t, nsA, "cache-x")
	c.taskNamespace(t, nsB, "cache-x")
	c.notePod(t, nsA, "writer", "cache-x", "echo "+note+" > /cache/note")
	c.notePod(t, nsB, "reader", "cache-x", "cat /cache/note")
	got, err := c.PodLogTail(context.Background(), nsB, "reader", 5)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != note {
		t.Fatalf("read through cache-x in %s: %q, want %q", nsB, got, note)
	}
}

// TestCacheRightsOnAnAPIServer runs the cache claim and the sweep of
// released cache volumes as factoryd's service account against
// k8s/factoryd.yaml's RBAC and admission policies, then tries what they
// must refuse, factoryd's and anyone else's.
func TestCacheRightsOnAnAPIServer(t *testing.T) {
	c := newK3sCluster(t)
	ctx := context.Background()
	run := testRun()
	ns, nsNext, plain := "factory-agents-t113-"+run, "factory-agents-t113-next-"+run, "t113-plain-"+run
	claim, static := "cache-t113-"+run, "t113-static-"+run
	c.removeVolumes(t, nsNext)
	t.Cleanup(func() {
		c.asAdmin()
		c.DeleteNamespace(context.Background(), plain)
		c.DeletePersistentVolume(context.Background(), static)
	})
	refused := func(name string, err error) {
		t.Helper()
		// an expression that fails to evaluate refuses too, but by a policy bug
		if err == nil || strings.Contains(err.Error(), "fails to evaluate") ||
			!(errors.Is(err, ErrForbidden) || strings.Contains(err.Error(), "ValidatingAdmissionPolicy 'factory")) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	cacheVolumeOf := func(namespace string) (PersistentVolume, bool) {
		t.Helper()
		for _, pv := range c.claimedVolumes(t, namespace) {
			if pv.StorageClass == CacheClass {
				return pv, true
			}
		}
		return PersistentVolume{}, false
	}
	await := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if done() {
				return
			}
		}
		t.Fatalf("%s: timed out", what)
	}

	c.taskNamespace(t, ns, claim)
	claims := "/api/v1/namespaces/" + ns + "/persistentvolumeclaims"
	c.asFactoryd()
	refused("factoryd: a claim not named cache-*", c.Create(ctx, claims, cacheClaim("other")))
	other := cacheClaim("cache-other")
	other["spec"].(map[string]any)["storageClassName"] = "local-path"
	refused("factoryd: a cache claim of another class", c.Create(ctx, claims, other))
	for field, value := range map[string]any{
		"volumeName":    "pvc-somebody-elses",
		"selector":      map[string]any{"matchLabels": map[string]any{"a": "b"}},
		"dataSource":    map[string]any{"kind": "PersistentVolumeClaim", "name": claim},
		"dataSourceRef": map[string]any{"kind": "PersistentVolumeClaim", "name": claim},
	} {
		pinned := cacheClaim("cache-pinned-" + strings.ToLower(field))
		pinned["spec"].(map[string]any)[field] = value
		refused("factoryd: a cache claim with "+field, c.Create(ctx, claims, pinned))
	}

	// anyone else, in factoryd's namespaces or their own (review-task113 I1)
	c.asAdmin()
	refused("admin: a factory-cache claim in a factory-agents namespace", c.Create(ctx, claims, cacheClaim("cache-ci-ai-software-factory-code")))
	if err := c.CreateNamespace(ctx, plain, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}); err != nil {
		t.Fatal(err)
	}
	plainClaims := "/api/v1/namespaces/" + plain + "/persistentvolumeclaims"
	refused("admin: a factory-cache claim in its own namespace", c.Create(ctx, plainClaims, cacheClaim("cache-ci-ai-software-factory-code")))
	beta := cacheClaim("cache-beta")
	delete(beta["spec"].(map[string]any), "storageClassName")
	beta["metadata"].(map[string]any)["annotations"] = map[string]any{"volume.beta.kubernetes.io/storage-class": "factory-cache"}
	refused("admin: a factory-cache claim by the beta annotation", c.Create(ctx, plainClaims, beta))
	local := cacheClaim("data")
	local["spec"].(map[string]any)["storageClassName"] = "local-path"
	if err := c.Create(ctx, plainClaims, local); err != nil {
		t.Errorf("admin: a local-path claim: %v, want admitted", err)
	}

	c.notePod(t, ns, "writer", claim, "echo "+run+" > /cache/note")
	c.asFactoryd()
	bound, ok := cacheVolumeOf(ns)
	if !ok || bound.Phase != "Bound" {
		t.Fatalf("cache volume of %s: %+v %v, want Bound", ns, bound, ok)
	}
	refused("factoryd: delete a bound cache volume", c.DeletePersistentVolume(ctx, bound.Name))

	c.asAdmin()
	if err := c.Create(ctx, "/api/v1/persistentvolumes", map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": static},
		"spec": map[string]any{"storageClassName": "local-path", "capacity": map[string]any{"storage": "1Gi"},
			"accessModes": []any{"ReadWriteOnce"}, "persistentVolumeReclaimPolicy": "Retain",
			"hostPath": map[string]any{"path": "/tmp/" + static},
			"claimRef": map[string]any{"namespace": plain, "name": "gone", "uid": "00000000-0000-0000-0000-000000000000"}},
	}); err != nil {
		t.Fatal(err)
	}
	c.asFactoryd()
	if err := c.DeleteNamespace(ctx, ns); err != nil {
		t.Fatal(err)
	}
	await("the cache volume released", func() bool { pv, _ := cacheVolumeOf(ns); return pv.Phase == "Released" })
	await("the other class's volume released", func() bool {
		return slices.ContainsFunc(c.claimedVolumes(t, plain), func(pv PersistentVolume) bool { return pv.Name == static && pv.Phase == "Released" })
	})
	refused("factoryd: delete a released volume of another class", c.DeletePersistentVolume(ctx, static))
	deleteReleasedCaches(ctx, c)
	await("the released cache volume deleted", func() bool { _, ok := cacheVolumeOf(ns); return !ok })
	if !slices.ContainsFunc(c.claimedVolumes(t, plain), func(pv PersistentVolume) bool { return pv.Name == static }) {
		t.Fatal("the sweep deleted another class's volume")
	}

	// the directory outlives its volume: the next claim of the name reads it
	c.taskNamespace(t, nsNext, claim)
	c.notePod(t, nsNext, "reader", claim, "test \"$(cat /cache/note)\" = "+run)
}
