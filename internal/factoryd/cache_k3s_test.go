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

// k3sAdmin is a client on the API server FACTORYD_APISERVER (host:port)
// as an admin whose token and the CA are in the directory
// FACTORYD_APISERVER_ADMIN; the cluster runs local-path v0.0.31 with the
// infra repository's StorageClass factory-cache.
func k3sAdmin(t *testing.T) *InClusterKube {
	t.Helper()
	addr, dir := os.Getenv("FACTORYD_APISERVER"), os.Getenv("FACTORYD_APISERVER_ADMIN")
	if addr == "" || dir == "" {
		t.Skip("FACTORYD_APISERVER and FACTORYD_APISERVER_ADMIN not set")
	}
	host, port, _ := net.SplitHostPort(addr)
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	kube, err := NewInClusterKube()
	if err != nil {
		t.Fatal(err)
	}
	return kube
}

// awaitPodEnd waits for a pod to succeed or fail.
func awaitPodEnd(t *testing.T, kube *InClusterKube, ns, name string) string {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		phase, _, err := kube.PodStatus(ctx, ns, name)
		if err != nil {
			t.Fatalf("pod %s/%s: %v", ns, name, err)
		}
		if phase == "Succeeded" || phase == "Failed" {
			return phase
		}
	}
	d, _ := kube.PodDiagnosis(ctx, ns, name)
	t.Fatalf("pod %s/%s did not end: %+v", ns, name, d)
	return ""
}

// TestCachePathSharedAcrossNamespaces: a claim of class factory-cache lands
// in the node directory named after the claim, so the note a pod writes
// through cache-x in one namespace is read through cache-x in another.
func TestCachePathSharedAcrossNamespaces(t *testing.T) {
	kube := k3sAdmin(t)
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	run := hex.EncodeToString(b)
	note := "note-" + run
	nsA, nsB := "t113-a-"+run, "t113-b-"+run
	t.Cleanup(func() {
		for _, ns := range []string{nsA, nsB} {
			kube.DeleteNamespace(context.Background(), ns)
		}
	})
	pod := func(ns, name, script string) {
		t.Helper()
		if err := kube.Create(ctx, "/api/v1/namespaces/"+ns+"/persistentvolumeclaims", cacheClaim("cache-x")); err != nil {
			t.Fatal(err)
		}
		if err := kube.CreatePod(ctx, ns, name, map[string]any{"spec": map[string]any{
			"restartPolicy":   "Never",
			"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox:1.37", "command": []any{"sh", "-c", script},
				"volumeMounts": []any{map[string]any{"name": "cache", "mountPath": "/cache"}},
				"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000,
					"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
			}},
			"volumes": []any{map[string]any{"name": "cache", "persistentVolumeClaim": map[string]any{"claimName": "cache-x"}}},
		}}); err != nil {
			t.Fatal(err)
		}
		if phase := awaitPodEnd(t, kube, ns, name); phase != "Succeeded" {
			log, _ := kube.PodLogTail(ctx, ns, name, 20)
			t.Fatalf("pod %s/%s %s: %s", ns, name, phase, log)
		}
	}
	for _, ns := range []string{nsA, nsB} {
		if err := kube.CreateNamespace(ctx, ns, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}); err != nil {
			t.Fatal(err)
		}
	}
	pod(nsA, "writer", "echo "+note+" > /cache/note")
	pod(nsB, "reader", "cat /cache/note")
	got, err := kube.PodLogTail(ctx, nsB, "reader", 5)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != note {
		t.Fatalf("read through cache-x in %s: %q, want %q", nsB, got, note)
	}
	// Retain keeps both volumes; they are this test's to remove
	t.Cleanup(func() {
		pvs, _ := kube.PersistentVolumes(context.Background())
		for _, pv := range pvs {
			if pv.ClaimNamespace == nsA || pv.ClaimNamespace == nsB {
				kube.DeletePersistentVolume(context.Background(), pv.Name)
			}
		}
	})
}

// TestCacheRightsOnAnAPIServer runs the cache claim and the sweep of
// released cache volumes as factoryd's service account against
// k8s/factoryd.yaml's RBAC and admission policy (FACTORYD_APISERVER_SA:
// factoryd's token and the CA), then tries what the policy must refuse.
func TestCacheRightsOnAnAPIServer(t *testing.T) {
	kube := k3sAdmin(t)
	adminDir, saDirEnv := saDir, os.Getenv("FACTORYD_APISERVER_SA")
	if saDirEnv == "" {
		t.Skip("FACTORYD_APISERVER_SA not set")
	}
	asFactoryd, asAdmin := func() { saDir = saDirEnv }, func() { saDir = adminDir }
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	run := hex.EncodeToString(b)
	ns, nsNext := "factory-agents-t113-"+run, "t113-next-"+run
	claim, static := "cache-t113-"+run, "t113-static-"+run
	t.Cleanup(func() {
		asAdmin()
		kube.DeleteNamespace(context.Background(), ns)
		kube.DeleteNamespace(context.Background(), nsNext)
		kube.DeletePersistentVolume(context.Background(), static)
	})
	refused := func(name string, err error) {
		t.Helper()
		// an expression that fails to evaluate refuses too, but by a policy bug
		if err == nil || strings.Contains(err.Error(), "fails to evaluate") ||
			!(errors.Is(err, ErrForbidden) || strings.Contains(err.Error(), "ValidatingAdmissionPolicy 'factoryd-task-namespaces'")) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	cacheVolumeOf := func(namespace string) (PersistentVolume, bool) {
		t.Helper()
		pvs, err := kube.PersistentVolumes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, pv := range pvs {
			if pv.ClaimNamespace == namespace && pv.StorageClass == CacheClass {
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
	notePod := func(namespace, name, script string) {
		t.Helper()
		asAdmin()
		if err := kube.CreatePod(ctx, namespace, name, map[string]any{"spec": map[string]any{
			"restartPolicy":   "Never",
			"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox:1.37", "command": []any{"sh", "-c", script},
				"volumeMounts": []any{cacheMount},
				"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000,
					"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
			}},
			"volumes": []any{cacheVolume(claim)},
		}}); err != nil {
			t.Fatal(err)
		}
		if phase := awaitPodEnd(t, kube, namespace, name); phase != "Succeeded" {
			log, _ := kube.PodLogTail(ctx, namespace, name, 20)
			t.Fatalf("pod %s/%s %s: %s", namespace, name, phase, log)
		}
	}

	asFactoryd()
	if err := kube.CreateNamespace(ctx, ns, map[string]string{"factory.ai/task": "t113", "pod-security.kubernetes.io/enforce": "restricted"}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+ns+"/rolebindings", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "factoryd"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "factoryd-task"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": "factory"}},
	}); err != nil {
		t.Fatal(err)
	}
	claims := "/api/v1/namespaces/" + ns + "/persistentvolumeclaims"
	if err := retryForbidden(ctx, func() error { return kube.Create(ctx, claims, cacheClaim(claim)) }); err != nil {
		t.Fatalf("factoryd's cache claim: %v", err)
	}
	refused("a claim not named cache-*", kube.Create(ctx, claims, cacheClaim("other")))
	other := cacheClaim("cache-other")
	other["spec"].(map[string]any)["storageClassName"] = "local-path"
	refused("a cache claim of another class", kube.Create(ctx, claims, other))

	notePod(ns, "writer", "echo "+run+" > /cache/note")
	asFactoryd()
	bound, ok := cacheVolumeOf(ns)
	if !ok || bound.Phase != "Bound" {
		t.Fatalf("cache volume of %s: %+v %v, want Bound", ns, bound, ok)
	}
	refused("delete a bound cache volume", kube.DeletePersistentVolume(ctx, bound.Name))

	asAdmin()
	if err := kube.Create(ctx, "/api/v1/persistentvolumes", map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": static},
		"spec": map[string]any{"storageClassName": "local-path", "capacity": map[string]any{"storage": "1Gi"},
			"accessModes": []any{"ReadWriteOnce"}, "persistentVolumeReclaimPolicy": "Retain",
			"hostPath": map[string]any{"path": "/tmp/" + static},
			"claimRef": map[string]any{"namespace": nsNext, "name": "gone", "uid": "00000000-0000-0000-0000-000000000000"}},
	}); err != nil {
		t.Fatal(err)
	}
	asFactoryd()
	if err := kube.DeleteNamespace(ctx, ns); err != nil {
		t.Fatal(err)
	}
	await("the cache volume released", func() bool { pv, _ := cacheVolumeOf(ns); return pv.Phase == "Released" })
	await("the other class's volume released", func() bool {
		pvs, _ := kube.PersistentVolumes(ctx)
		return slices.ContainsFunc(pvs, func(pv PersistentVolume) bool { return pv.Name == static && pv.Phase == "Released" })
	})
	refused("delete a released volume of another class", kube.DeletePersistentVolume(ctx, static))
	deleteReleasedCaches(ctx, kube)
	await("the released cache volume deleted", func() bool { _, ok := cacheVolumeOf(ns); return !ok })
	pvs, _ := kube.PersistentVolumes(ctx)
	if !slices.ContainsFunc(pvs, func(pv PersistentVolume) bool { return pv.Name == static }) {
		t.Fatal("the sweep deleted another class's volume")
	}

	// the directory outlives its volume: the next claim of the name reads it
	asAdmin()
	if err := kube.CreateNamespace(ctx, nsNext, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, "/api/v1/namespaces/"+nsNext+"/persistentvolumeclaims", cacheClaim(claim)); err != nil {
		t.Fatal(err)
	}
	notePod(nsNext, "reader", "test \"$(cat /cache/note)\" = "+run)
	t.Cleanup(func() {
		asAdmin()
		if pv, ok := cacheVolumeOf(nsNext); ok {
			kube.DeletePersistentVolume(context.Background(), pv.Name)
		}
	})
}
