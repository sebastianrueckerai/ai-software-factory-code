package factoryd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Sentinel errors so callers can treat already-exists and gone as normal
// control flow instead of inspecting messages.
var (
	ErrAlreadyExists = errors.New("kubernetes: already exists")
	ErrNotFound      = errors.New("not found") // shared by the kube, S3, Forgejo and store clients
	ErrForbidden     = errors.New("kubernetes: forbidden")
	// ErrRefused is a Forgejo 4xx no retry can fix: everything but a 404
	// (errForgejoNotFound) and the transient 408, 409 and 429.
	ErrRefused = errors.New("forgejo: refused")
	// errForgejoForbidden is Forgejo's 403, one of ErrRefused: the
	// credential works but lacks the right (a reader listing hooks).
	errForgejoForbidden = fmt.Errorf("%w (forbidden)", ErrRefused)
	// errForgejoNotFound is Forgejo's 404. It is also ErrNotFound, so a
	// caller tolerating an absent object needs no second check, but only
	// it, not a kube, S3 or store not-found, ends an activity's retries.
	errForgejoNotFound = fmt.Errorf("forgejo: %w", ErrNotFound)
)

// saDir is where the pod's service-account token lives; a var so a test can
// point it at a stub token. The acceptance tests (roadmap 188) set
// FACTORYD_SA_DIR before they run, to drive this production client against
// their scripted apiserver.
var saDir = envOr("FACTORYD_SA_DIR", "/var/run/secrets/kubernetes.io/serviceaccount")

// SaDirForTest points the production client's service-account directory at
// dir and returns its former path, for an acceptance test that runs
// InClusterKube against a scripted apiserver outside the cluster.
func SaDirForTest(dir string) string {
	old := saDir
	saDir = dir
	return old
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Kube is the slice of the Kubernetes API the runner needs. The production
// client talks raw REST with the pod's own service account (token re-read
// per request, because projected tokens rotate); tests script a fake.
type Kube interface {
	CreateNamespace(ctx context.Context, name string, labels map[string]string) error
	DeleteNamespace(ctx context.Context, name string) error
	NamespacesByLabel(ctx context.Context, key, value string) ([]string, error)
	// ListNamespacesByLabel lists the same namespaces with their deletion
	// state. NamespacesByLabel's meaning for its callers is simply "the
	// namespace exists" — a Terminating one still answers there; this is
	// reconcile's variant, which must not read a namespace teardown already
	// deleted (its RoleBinding is gone, so a pod read answers Forbidden).
	ListNamespacesByLabel(ctx context.Context, key, value string) ([]NamespaceInfo, error)
	CreateServiceAccount(ctx context.Context, namespace, name string) error
	CreateSecret(ctx context.Context, namespace, name, secretType string, stringData map[string]string) error
	GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error)
	CreateCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error
	// ApplyCiliumNetworkPolicy creates or replaces the policy (server-side
	// apply, factoryd as the field manager).
	ApplyCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error
	// CiliumNetworkPolicySpec reads a policy's spec; ErrNotFound when it
	// does not exist.
	CiliumNetworkPolicySpec(ctx context.Context, namespace, name string) (map[string]any, error)
	// Create POSTs a complete object to a collection path such as
	// /api/v1/namespaces/<ns>/resourcequotas.
	Create(ctx context.Context, path string, obj map[string]any) error
	CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error
	DeletePod(ctx context.Context, namespace, name string) error
	// PodStatus reports the pod phase and the container exit code (-1 while
	// the container has not terminated).
	PodStatus(ctx context.Context, namespace, name string) (phase string, exitCode int32, err error)
	// PodIP is the running pod's address; ErrNotFound before it has one.
	PodIP(ctx context.Context, namespace, name string) (string, error)
	// PodInitFinished is when the pod's first init container exited 0
	// with the termination message "ready"; false otherwise.
	PodInitFinished(ctx context.Context, namespace, name string) (time.Time, bool, error)
	// PodLog streams the pod's stdout, following until the pod terminates or
	// ctx is cancelled. The caller closes the stream.
	PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error)
	// PodDiagnosis reads the pod's phase, conditions and container
	// states, its init containers' first.
	PodDiagnosis(ctx context.Context, namespace, name string) (PodDiagnosis, error)
	// PodLogTail is the pod's last lines of stdout, not followed.
	PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error)
	PersistentVolumes(ctx context.Context) ([]PersistentVolume, error)
	DeletePersistentVolume(ctx context.Context, name string) error
	CronJobs(ctx context.Context, namespace string) ([]CronJobState, error)
	ScheduledBackups(ctx context.Context, namespace string) ([]CronJobState, error)
	// Deployment reads a Deployment: a roll's rollout wait reads the
	// factory's own.
	Deployment(ctx context.Context, namespace, name string) (Deployment, error)
}

// PodDiagnosis is what the gate supervisor and a pod's start read of a
// sandbox pod. Reason and Message are the pod's own status fields, set by
// the kubelet for an end outside the container's own exit, eviction
// included.
type PodDiagnosis struct {
	Phase      string           `json:"phase"`
	Reason     string           `json:"reason,omitempty"`
	Message    string           `json:"message,omitempty"`
	Conditions []PodCondition   `json:"conditions"`
	Containers []ContainerState `json:"containers,omitempty"`
}

type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerState is one container's state: waiting, running or
// terminated, with its reason, and a terminated one's exit code.
type ContainerState struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode int32  `json:"exit_code,omitempty"`
	Restarts int    `json:"restarts"`
}

// waiting says what the container waits with: the kubelet's reason and
// message.
func (c ContainerState) waiting() string {
	s := "container " + c.Name + " waits with " + c.Reason
	if c.Message != "" {
		s += ": " + c.Message
	}
	return s
}

// InClusterKube is the raw-REST Kubernetes client. No client-go: the runner
// needs a dozen endpoints, and owning the wire keeps the dependency out.
type InClusterKube struct {
	Host string // https://<KUBERNETES_SERVICE_HOST>:<KUBERNETES_SERVICE_PORT>
	HTTP *http.Client
}

// NewInClusterKube loads the pod's CA bundle; the bearer token is re-read
// on every request so rotation needs no restart. The API server is
// addressed by the service address the kubelet injects, as client-go does:
// no DNS lookup, which factoryd's egress policy does not admit.
func NewInClusterKube() (*InClusterKube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("kube in-cluster: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt")) // #nosec G304 -- saDir is the pod's service-account mount or a test stub set by SaDirForTest; never user input
	if err != nil {
		return nil, fmt.Errorf("kube in-cluster: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kube in-cluster: ca.crt holds no certificates")
	}
	return &InClusterKube{
		Host: "https://" + net.JoinHostPort(host, port),
		HTTP: &http.Client{Transport: &http.Transport{
			// RootCAs pins the API server's key. A test that scripts the
			// API outside the cluster (roadmap 188) serves it on an
			// ephemeral address, whose certificate no in-cluster CA
			// would name; there FACTORYD_KUBE_INSECURE skips the name
			// check and the real deployment never sets it.
			TLSClientConfig: &tls.Config{RootCAs: pool, InsecureSkipVerify: os.Getenv("FACTORYD_KUBE_INSECURE") != ""}, // #nosec G402 -- pinned by RootCAs; the name is only loose where a test says so
		}},
	}, nil
}

func (k *InClusterKube) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": name, "labels": labels},
	}
	_, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces", body, nil)
	return err
}

func (k *InClusterKube) DeleteNamespace(ctx context.Context, name string) error {
	_, err := k.call(ctx, http.MethodDelete, "/api/v1/namespaces/"+name, nil, nil)
	return err
}

// Delete removes the object at path (e.g.
// /api/v1/namespaces/x/secrets/y) and reports whether it was there; an
// object already gone is no error, so remove-project's second run after a
// partial failure finishes the rest (roadmap 272).
func (k *InClusterKube) Delete(ctx context.Context, path string) (bool, error) {
	if err := k.getJSON(ctx, path, nil); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if _, err := k.call(ctx, http.MethodDelete, path, nil, nil); err != nil {
		if errors.Is(err, ErrNotFound) { // deleted between the read and the delete
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (k *InClusterKube) NamespacesByLabel(ctx context.Context, key, value string) ([]string, error) {
	nsList, err := k.listNamespaces(ctx, key, value)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nsList))
	for _, ns := range nsList {
		names = append(names, ns.Name)
	}
	return names, nil
}

// NamespaceInfo is a listed namespace's name, its labels and whether the
// API server has stamped metadata.deletionTimestamp on it: a Terminating
// namespace still matches its label selector and still shows in a list
// while its contents are being garbage-collected away.
type NamespaceInfo struct {
	Name        string
	Terminating bool
	Labels      map[string]string
}

func (k *InClusterKube) ListNamespacesByLabel(ctx context.Context, key, value string) ([]NamespaceInfo, error) {
	return k.listNamespaces(ctx, key, value)
}

func (k *InClusterKube) listNamespaces(ctx context.Context, key, value string) ([]NamespaceInfo, error) {
	sel := key
	if value != "" {
		sel += "=" + value // "key=" would select only empty values
	}
	q := url.Values{"labelSelector": []string{sel}}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				DeletionTimestamp string            `json:"deletionTimestamp"`
				Labels            map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	out := make([]NamespaceInfo, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, NamespaceInfo{Name: it.Metadata.Name, Terminating: it.Metadata.DeletionTimestamp != "", Labels: it.Metadata.Labels})
	}
	return out, nil
}

// NamespaceLabel is namespace name's label key, "" when it has none.
func (k *InClusterKube) NamespaceLabel(ctx context.Context, name, key string) (string, error) {
	var ns struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+name, &ns); err != nil {
		return "", err
	}
	return ns.Metadata.Labels[key], nil
}

func (k *InClusterKube) CreateServiceAccount(ctx context.Context, namespace, name string) error {
	body := map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata": map[string]any{"name": name, "namespace": namespace},
	}
	_, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/serviceaccounts", body, nil)
	return err
}

func (k *InClusterKube) CreateSecret(ctx context.Context, namespace, name, secretType string, stringData map[string]string) error {
	if secretType == "" {
		secretType = "Opaque"
	}
	body := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"type":     secretType, "stringData": stringData,
	}
	_, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/secrets", body, nil)
	return err
}

func (k *InClusterKube) GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error) {
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/secrets/"+name, &sec); err != nil {
		return nil, err
	}
	return decodeSecret(namespace, name, sec.Data)
}

// KubeSecret is a listed Secret: its name, type and decoded data.
type KubeSecret struct {
	Name, Type string
	Data       map[string]string
}

func (k *InClusterKube) SecretsByLabels(ctx context.Context, namespace string, labels map[string]string) ([]KubeSecret, error) {
	var sel []string
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		sel = append(sel, key+"="+labels[key])
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Type string            `json:"type"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	q := url.Values{"labelSelector": []string{strings.Join(sel, ",")}}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/secrets?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	out := make([]KubeSecret, 0, len(list.Items))
	for _, it := range list.Items {
		data, err := decodeSecret(namespace, it.Metadata.Name, it.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, KubeSecret{Name: it.Metadata.Name, Type: it.Type, Data: data})
	}
	return out, nil
}

func decodeSecret(namespace, name string, data map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(data))
	for key, b64 := range data {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("kube secret %s/%s: %w", namespace, name, err)
		}
		out[key] = string(raw)
	}
	return out, nil
}

func (k *InClusterKube) CreateCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error {
	body := map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     spec,
	}
	_, err := k.call(ctx, http.MethodPost, "/apis/cilium.io/v2/namespaces/"+namespace+"/ciliumnetworkpolicies", body, nil)
	return err
}

func (k *InClusterKube) ApplyCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error {
	body := map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     spec,
	}
	_, err := k.call(ctx, http.MethodPatch,
		"/apis/cilium.io/v2/namespaces/"+namespace+"/ciliumnetworkpolicies/"+name+"?fieldManager=factoryd&force=true", body, nil)
	return err
}

func (k *InClusterKube) CiliumNetworkPolicySpec(ctx context.Context, namespace, name string) (map[string]any, error) {
	var out struct {
		Spec map[string]any `json:"spec"`
	}
	_, err := k.call(ctx, http.MethodGet, "/apis/cilium.io/v2/namespaces/"+namespace+"/ciliumnetworkpolicies/"+name, nil, &out)
	return out.Spec, err
}

func (k *InClusterKube) Create(ctx context.Context, path string, obj map[string]any) error {
	_, err := k.call(ctx, http.MethodPost, path, obj, nil)
	return err
}

func (k *InClusterKube) CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error {
	meta, _ := pod["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		pod["metadata"] = meta
	}
	meta["name"] = name
	meta["namespace"] = namespace
	pod["apiVersion"] = "v1"
	pod["kind"] = "Pod"
	_, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/pods", pod, nil)
	return err
}

func (k *InClusterKube) DeletePod(ctx context.Context, namespace, name string) error {
	_, err := k.call(ctx, http.MethodDelete, "/api/v1/namespaces/"+namespace+"/pods/"+name, nil, nil)
	return err
}

func (k *InClusterKube) PodStatus(ctx context.Context, namespace, name string) (phase string, exitCode int32, err error) {
	var pod struct {
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				State struct {
					Terminated struct {
						ExitCode int32 `json:"exitCode"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods/"+name, &pod); err != nil {
		return "", -1, err
	}
	exitCode = -1
	if len(pod.Status.ContainerStatuses) > 0 {
		exitCode = pod.Status.ContainerStatuses[0].State.Terminated.ExitCode
	}
	return pod.Status.Phase, exitCode, nil
}

func (k *InClusterKube) PodIP(ctx context.Context, namespace, name string) (string, error) {
	var pod struct {
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods/"+name, &pod); err != nil {
		return "", err
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s/%s has no address yet: %w", namespace, name, ErrNotFound)
	}
	return pod.Status.PodIP, nil
}

func (k *InClusterKube) PodInitFinished(ctx context.Context, namespace, name string) (time.Time, bool, error) {
	var pod struct {
		Status struct {
			InitContainerStatuses []struct {
				State struct {
					Terminated *struct {
						ExitCode   int32     `json:"exitCode"`
						Message    string    `json:"message"`
						FinishedAt time.Time `json:"finishedAt"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"initContainerStatuses"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods/"+name, &pod); err != nil {
		return time.Time{}, false, err
	}
	if len(pod.Status.InitContainerStatuses) == 0 {
		return time.Time{}, false, nil
	}
	t := pod.Status.InitContainerStatuses[0].State.Terminated
	if t == nil || t.ExitCode != 0 || strings.TrimSpace(t.Message) != buildReadyMessage {
		return time.Time{}, false, nil
	}
	return t.FinishedAt, true, nil
}

func (k *InClusterKube) PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error) {
	res, err := k.request(ctx, http.MethodGet,
		"/api/v1/namespaces/"+namespace+"/pods/"+name+"/log?follow=true", nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (k *InClusterKube) PodDiagnosis(ctx context.Context, namespace, name string) (PodDiagnosis, error) {
	type detail struct {
		Reason   string `json:"reason"`
		Message  string `json:"message"`
		ExitCode int32  `json:"exitCode"`
	}
	type status struct {
		Name         string `json:"name"`
		RestartCount int    `json:"restartCount"`
		State        struct {
			Waiting    *detail   `json:"waiting"`
			Running    *struct{} `json:"running"`
			Terminated *detail   `json:"terminated"`
		} `json:"state"`
	}
	var pod struct {
		Status struct {
			Phase                 string         `json:"phase"`
			Reason                string         `json:"reason"`
			Message               string         `json:"message"`
			Conditions            []PodCondition `json:"conditions"`
			InitContainerStatuses []status       `json:"initContainerStatuses"`
			ContainerStatuses     []status       `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods/"+name, &pod); err != nil {
		return PodDiagnosis{}, err
	}
	d := PodDiagnosis{Phase: pod.Status.Phase, Reason: pod.Status.Reason, Message: pod.Status.Message, Conditions: pod.Status.Conditions}
	for _, c := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		cs := ContainerState{Name: c.Name, Restarts: c.RestartCount, State: "running"}
		switch {
		case c.State.Waiting != nil:
			cs.State, cs.Reason, cs.Message = "waiting", c.State.Waiting.Reason, c.State.Waiting.Message
		case c.State.Terminated != nil:
			cs.State, cs.Reason, cs.Message, cs.ExitCode = "terminated", c.State.Terminated.Reason, c.State.Terminated.Message, c.State.Terminated.ExitCode
		}
		d.Containers = append(d.Containers, cs)
	}
	return d, nil
}

// podLogTailBytes bounds a log tail however long its lines are.
const podLogTailBytes = 64 << 10

func (k *InClusterKube) PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error) {
	b, err := k.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?tailLines=%d&limitBytes=%d", namespace, name, lines, podLogTailBytes), nil, nil)
	return string(b), err
}

// CacheClass is the StorageClass (infra k8s/platform/local-path) whose
// volume path is the claim's name: every namespace's claim of one name
// reaches the same directory on a node, and Retain keeps it.
const CacheClass = "factory-cache"

// cacheClaim is the claim name of class CacheClass; local-path does not
// enforce the request.
func cacheClaim(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"storageClassName": CacheClass,
			"accessModes":      []any{"ReadWriteOnce"},
			"resources":        map[string]any{"requests": map[string]any{"storage": "20Gi"}},
		},
	}
}

// cacheEnv points Go's build cache, go install and npm's cache at the
// cache volume at /cache, and Go's module cache at the pod's own modcache:
// the module unpack relies on file locks, which gVisor keeps inside each
// sandbox (ruling 223). PATH is the sandbox's and the CI runner's own with
// go install's target first.
var cacheEnv = []map[string]any{
	{"name": "GOPATH", "value": "/cache/go"},
	{"name": "GOMODCACHE", "value": "/modcache"},
	{"name": "GOCACHE", "value": "/cache/go-build"},
	{"name": "npm_config_cache", "value": "/cache/npm"},
	{"name": "PATH", "value": "/cache/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
}

var cacheMounts = []any{
	map[string]any{"name": "cache", "mountPath": "/cache"},
	map[string]any{"name": "modcache", "mountPath": "/modcache"},
}

// cacheUmaskSh runs a container command with umask 0000: gVisor's gofer
// serves the cache volume to each sandbox on its own and checks an open
// of a file another sandbox created against the group/other bits, so the
// 0644 Go writes under umask 022 denies O_RDWR to the same uid in the
// next pod sharing the claim, which cmd/go reports as `package X is not
// in std` (debug-cache-race.md: 19/33 concurrent cold-cache pod-runs
// failed; umask 0000 0/30, same speed). Run as `/bin/sh -c cacheUmaskSh
// argv0` with the real command in argv0 and its args in args, both by
// absolute path: PATH leads with /cache/go/bin, which pull request jobs
// and agents write.
const cacheUmaskSh = `umask 0000; exec "$0" "$@"`

func cacheVolumes(claim string) []any {
	return []any{
		map[string]any{"name": "cache", "persistentVolumeClaim": map[string]any{"claimName": claim}},
		map[string]any{"name": "modcache", "emptyDir": map[string]any{"sizeLimit": "8Gi"}},
	}
}

// deleteReleasedCaches deletes the cache volumes whose claims went with
// their namespaces; Retain keeps their directory for the next claim. A
// namespace deletion is asynchronous, so the claim of the namespace just
// deleted is not Released yet: each sweep takes the earlier ones', and
// the last lingers until the next teardown (harmless: a Released volume
// never binds again, and a new claim gets a new volume on the same path).
func deleteReleasedCaches(ctx context.Context, kube Kube) {
	pvs, err := kube.PersistentVolumes(ctx)
	if err != nil {
		log.Printf("cache volumes: list: %v", err)
		return
	}
	for _, pv := range pvs {
		if pv.StorageClass != CacheClass || pv.Phase != "Released" {
			continue
		}
		if err := kube.DeletePersistentVolume(ctx, pv.Name); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("cache volumes: delete %s: %v", pv.Name, err)
		}
	}
}

// PersistentVolume is what the cache sweep reads of a volume.
type PersistentVolume struct {
	Name         string
	StorageClass string
	Phase        string
}

func (k *InClusterKube) PersistentVolumes(ctx context.Context) ([]PersistentVolume, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				StorageClassName string `json:"storageClassName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/api/v1/persistentvolumes", &list); err != nil {
		return nil, err
	}
	out := make([]PersistentVolume, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, PersistentVolume{Name: it.Metadata.Name, StorageClass: it.Spec.StorageClassName,
			Phase: it.Status.Phase})
	}
	return out, nil
}

func (k *InClusterKube) DeletePersistentVolume(ctx context.Context, name string) error {
	_, err := k.call(ctx, http.MethodDelete, "/api/v1/persistentvolumes/"+name, nil, nil)
	return err
}

// The kinds of a cluster job.
const (
	KindCronJob = "CronJob"
	// KindScheduledBackup is CNPG's: its cron has six fields, seconds
	// first, and it records no success.
	KindScheduledBackup = "ScheduledBackup"
)

// CronJobState is a CronJob or a ScheduledBackup as the console's
// Resources page lists it. LastScheduled is zero for a job never
// scheduled, LastSuccessful for one that never succeeded or does not
// record it.
type CronJobState struct {
	Kind, Namespace, Name, Schedule string
	Suspended                       bool
	LastScheduled, LastSuccessful   time.Time
}

// RecordsSuccess is whether the job's resource records its last success:
// a ScheduledBackup's does not.
func (j CronJobState) RecordsSuccess() bool { return j.Kind != KindScheduledBackup }

func (k *InClusterKube) CronJobs(ctx context.Context, namespace string) ([]CronJobState, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Schedule string `json:"schedule"`
				Suspend  bool   `json:"suspend"`
			} `json:"spec"`
			Status struct {
				LastScheduleTime   time.Time `json:"lastScheduleTime"`
				LastSuccessfulTime time.Time `json:"lastSuccessfulTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/apis/batch/v1/namespaces/"+namespace+"/cronjobs", &list); err != nil {
		return nil, err
	}
	out := make([]CronJobState, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, CronJobState{Kind: KindCronJob, Namespace: namespace, Name: it.Metadata.Name, Schedule: it.Spec.Schedule,
			Suspended: it.Spec.Suspend, LastScheduled: it.Status.LastScheduleTime, LastSuccessful: it.Status.LastSuccessfulTime})
	}
	return out, nil
}

// ScheduledBackups lists namespace's CNPG ScheduledBackups. Their status
// has lastScheduleTime, the last backup started, and no success time.
func (k *InClusterKube) ScheduledBackups(ctx context.Context, namespace string) ([]CronJobState, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Schedule string `json:"schedule"`
				Suspend  bool   `json:"suspend"`
			} `json:"spec"`
			Status struct {
				LastScheduleTime time.Time `json:"lastScheduleTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/apis/postgresql.cnpg.io/v1/namespaces/"+namespace+"/scheduledbackups", &list); err != nil {
		return nil, err
	}
	out := make([]CronJobState, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, CronJobState{Kind: KindScheduledBackup, Namespace: namespace, Name: it.Metadata.Name, Schedule: it.Spec.Schedule,
			Suspended: it.Spec.Suspend, LastScheduled: it.Status.LastScheduleTime})
	}
	return out, nil
}

// ClusterJobs are the cluster's CronJobs and ScheduledBackups in
// Namespaces, platform, factory and forgejo when none are set: the
// console's Resources page lists them.
type ClusterJobs struct {
	Kube       Kube
	Namespaces []string
}

func (j ClusterJobs) CronJobs(ctx context.Context) ([]CronJobState, error) {
	namespaces := j.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{"platform", "factory", "forgejo"}
	}
	var out []CronJobState
	for _, ns := range namespaces {
		jobs, err := j.Kube.CronJobs(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("cronjobs of %s: %w", ns, err)
		}
		backups, err := j.Kube.ScheduledBackups(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("scheduled backups of %s: %w", ns, err)
		}
		out = append(append(out, jobs...), backups...)
	}
	return out, nil
}

// Deployment reads one Deployment (the factory namespace's roll record).
func (k *InClusterKube) Deployment(ctx context.Context, namespace, name string) (Deployment, error) {
	var d Deployment
	err := k.getJSON(ctx, "/apis/apps/v1/namespaces/"+namespace+"/deployments/"+name, &d)
	return d, err
}

// FluxState is a Flux Kustomization as its status tells it.
type FluxState struct {
	// Ready is the Ready condition's status: True, False, or Unknown while
	// Flux reconciles; "" while Flux has set none.
	Ready string
	// Message is Flux's message on the Ready condition.
	Message string
	// Applied is the revision Flux last applied whole, set with Ready
	// True: "" while it never has, as on a path no green build wrote.
	Applied string
}

// KustomizationState reads the Flux Kustomization name in namespace.
func (k *InClusterKube) KustomizationState(ctx context.Context, namespace, name string) (FluxState, error) {
	var ks struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
			LastAppliedRevision string `json:"lastAppliedRevision"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+namespace+"/kustomizations/"+name, &ks); err != nil {
		return FluxState{}, err
	}
	s := FluxState{Applied: ks.Status.LastAppliedRevision}
	for _, c := range ks.Status.Conditions {
		if c.Type == "Ready" {
			s.Ready, s.Message = c.Status, c.Message
		}
	}
	return s, nil
}

// FluxRollouts are the newest rollouts of every HelmRelease and every
// Kustomization but the factory's own, the one applying the roll targets'
// Deployments in namespace, whose rolls those Deployments' own window
// judges (ruling 237). A project environment's Kustomization (ProjectLabel)
// holds its own namespace's alerts, the platform's objects every
// namespace's.
func (k *InClusterKube) FluxRollouts(ctx context.Context, namespace string) ([]Rollout, error) {
	var ks struct {
		Items []fluxKustomization `json:"items"`
	}
	if err := k.getJSON(ctx, "/apis/kustomize.toolkit.fluxcd.io/v1/kustomizations", &ks); err != nil {
		return nil, err
	}
	var hrs struct {
		Items []fluxHelmRelease `json:"items"`
	}
	if err := k.getJSON(ctx, "/apis/helm.toolkit.fluxcd.io/v2/helmreleases", &hrs); err != nil {
		return nil, err
	}
	var out []Rollout
	for _, item := range ks.Items {
		if !item.appliesRollTargets(namespace) {
			out = append(out, item.rollout())
		}
	}
	for _, item := range hrs.Items {
		out = append(out, item.rollout())
	}
	return out, nil
}

// fluxKustomization is the slice of a Flux Kustomization its rollout reads.
type fluxKustomization struct {
	Metadata struct {
		Namespace string            `json:"namespace"`
		Name      string            `json:"name"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Interval string `json:"interval"`
	} `json:"spec"`
	Status struct {
		// History is Flux's record of the contents it applied, one entry
		// per digest and outcome, the one applied last first.
		History []struct {
			FirstReconciled time.Time `json:"firstReconciled"`
			LastReconciled  time.Time `json:"lastReconciled"`
		} `json:"history"`
		Inventory struct {
			Entries []struct {
				ID string `json:"id"` // namespace_name_group_kind
			} `json:"entries"`
		} `json:"inventory"`
	} `json:"status"`
}

// rollout is the Kustomization's newest content's. Flux applies a
// Kustomization in one reconcile, so its rollout starts and settles when
// Flux first applied that content. An older content brought back (a
// revert) keeps its old first reconcile: it was applied at the first
// reconcile after the content before it last reconciled, so within one
// interval of that.
func (ks fluxKustomization) rollout() Rollout {
	r, h := Rollout{Kind: "Kustomization", Namespace: ks.Metadata.Namespace, Name: ks.Metadata.Name}, ks.Status.History
	if ks.Metadata.Labels[ProjectLabel] != "" {
		r.Scope = ks.Metadata.Namespace
	}
	if len(h) == 0 {
		return r
	}
	r.Applied, r.Settled = h[0].FirstReconciled, h[0].FirstReconciled
	if len(h) > 1 && h[1].LastReconciled.After(r.Applied) {
		interval, _ := time.ParseDuration(ks.Spec.Interval)
		r.Applied, r.Settled = h[1].LastReconciled, h[1].LastReconciled.Add(interval)
	}
	return r
}

// appliesRollTargets reports whether the Kustomization applies a roll
// target's Deployment in namespace.
func (ks fluxKustomization) appliesRollTargets(namespace string) bool {
	for _, e := range ks.Status.Inventory.Entries {
		for _, t := range RollTargets {
			if e.ID == namespace+"_"+t.Deployment+"_apps_Deployment" {
				return true
			}
		}
	}
	return false
}

// fluxHelmRelease is the slice of a Flux HelmRelease its rollout reads.
type fluxHelmRelease struct {
	Metadata struct {
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			Message            string    `json:"message"`
			ObservedGeneration int64     `json:"observedGeneration"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
		// History is the Helm releases, the newest first.
		History []struct {
			LastDeployed time.Time `json:"lastDeployed"`
		} `json:"history"`
	} `json:"status"`
}

// rollout is the HelmRelease's newest release's: from its deploy (an
// install, upgrade or rollback, of a new generation or a new chart) until
// Ready reads True or False for the object's generation, a release failed
// for good settled as much as a ready one. Ready Unknown, or a generation
// the controller has not observed yet, still runs.
func (hr fluxHelmRelease) rollout() Rollout {
	r := Rollout{Kind: "HelmRelease", Namespace: hr.Metadata.Namespace, Name: hr.Metadata.Name}
	if len(hr.Status.History) == 0 {
		return r
	}
	r.Applied = hr.Status.History[0].LastDeployed
	for _, c := range hr.Status.Conditions {
		if c.Type == "Ready" {
			r.Message = c.Message
		}
		if c.Type == "Ready" && c.Status != "Unknown" && c.ObservedGeneration == hr.Metadata.Generation {
			r.Settled = c.LastTransitionTime
			if r.Settled.Before(r.Applied) {
				r.Settled = r.Applied
			}
		}
	}
	return r
}

func (k *InClusterKube) getJSON(ctx context.Context, path string, out any) error {
	_, err := k.call(ctx, http.MethodGet, path, nil, out)
	return err
}

// call performs one request; a non-nil into receives the decoded body.
func (k *InClusterKube) call(ctx context.Context, method, path string, body any, into any) ([]byte, error) {
	res, err := k.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("kube %s %s: %w", method, path, err)
	}
	if res.StatusCode == http.StatusConflict {
		return nil, ErrAlreadyExists
	}
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if res.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s %s: %s", ErrForbidden, method, path, raw)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("kube %s %s: status %d: %s", method, path, res.StatusCode, raw)
	}
	if into != nil {
		if err := json.Unmarshal(raw, into); err != nil {
			return nil, fmt.Errorf("kube %s %s: %w", method, path, err)
		}
	}
	return raw, nil
}

func (k *InClusterKube) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		enc, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(enc)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.Host+path, reader) // #nosec G704 -- the API server the kubelet injects; paths are built from validated names
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(filepath.Join(saDir, "token")) // #nosec G304 -- saDir is the pod's service-account mount or a test stub set by SaDirForTest; never user input
	if err != nil {
		return nil, fmt.Errorf("kube in-cluster token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPatch { // the only patch factoryd sends: a server-side apply (JSON is YAML)
			req.Header.Set("Content-Type", "application/apply-patch+yaml")
		}
	}
	client := k.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req) // #nosec G704 -- the API server the kubelet injects; paths are built from validated names
	if err != nil {
		return nil, fmt.Errorf("kube %s %s: %w", method, path, err)
	}
	return res, nil
}

// Change is what an apply did to an object.
type Change string

const (
	Created   Change = "created"
	Updated   Change = "updated"
	Unchanged Change = "no change"
)

// Apply creates or brings the object at item path (e.g.
// /api/v1/namespaces/x/resourcequotas/project) to obj by server-side
// apply, factoryd as the field manager. An apply that changes nothing
// keeps the resourceVersion, which tells the three outcomes apart.
func (k *InClusterKube) Apply(ctx context.Context, path string, obj map[string]any) (Change, error) {
	var before, after struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	err := k.getJSON(ctx, path, &before)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if _, err := k.call(ctx, http.MethodPatch, path+"?fieldManager=factoryd&force=true", obj, &after); err != nil {
		return "", err
	}
	switch before.Metadata.ResourceVersion {
	case "":
		return Created, nil
	case after.Metadata.ResourceVersion:
		return Unchanged, nil
	}
	return Updated, nil
}

// DeploymentState is how far one Deployment's rollout got.
type DeploymentState struct {
	Name               string
	Generation         int64
	ObservedGeneration int64
	Replicas           int32 // spec
	StatusReplicas     int32 // every pod of every ReplicaSet
	UpdatedReplicas    int32
	AvailableReplicas  int32
	Images             []string // the pod template's containers, in spec order
}

// RolledOut is kubectl rollout status's test: the controller saw the
// newest spec, every replica runs it, no old one is left, and every
// updated one is available. Otherwise it says what lags.
func (d DeploymentState) RolledOut() (bool, string) {
	switch {
	case d.ObservedGeneration < d.Generation:
		return false, fmt.Sprintf("observed generation %d of %d", d.ObservedGeneration, d.Generation)
	case d.UpdatedReplicas < d.Replicas:
		return false, fmt.Sprintf("%d of %d replicas updated", d.UpdatedReplicas, d.Replicas)
	case d.StatusReplicas > d.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas pending termination", d.StatusReplicas-d.UpdatedReplicas)
	case d.AvailableReplicas < d.UpdatedReplicas:
		return false, fmt.Sprintf("%d of %d updated replicas available", d.AvailableReplicas, d.UpdatedReplicas)
	}
	return true, ""
}

// Deployments lists namespace's Deployments with their rollout state.
func (k *InClusterKube) Deployments(ctx context.Context, namespace string) ([]DeploymentState, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name       string `json:"name"`
				Generation int64  `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int32 `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				Replicas           int32 `json:"replicas"`
				UpdatedReplicas    int32 `json:"updatedReplicas"`
				AvailableReplicas  int32 `json:"availableReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/apis/apps/v1/namespaces/"+namespace+"/deployments", &list); err != nil {
		return nil, err
	}
	out := make([]DeploymentState, 0, len(list.Items))
	for _, d := range list.Items {
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		var images []string
		for _, c := range d.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}
		out = append(out, DeploymentState{Name: d.Metadata.Name, Generation: d.Metadata.Generation,
			ObservedGeneration: d.Status.ObservedGeneration, Replicas: replicas, StatusReplicas: d.Status.Replicas,
			UpdatedReplicas: d.Status.UpdatedReplicas, AvailableReplicas: d.Status.AvailableReplicas, Images: images})
	}
	return out, nil
}
