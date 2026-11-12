package factoryd

import (
	"bytes"
	"cmp"
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
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// point it at a stub token.
var saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// SaDirForTest points the production client's service-account directory at
// dir and returns its former path, for the deployment-health and
// lease-admission tests that run InClusterKube against a scripted apiserver.
func SaDirForTest(dir string) string {
	old := saDir
	saDir = dir
	return old
}

// kubeRetryWait waits d between a throttled answer and its retry and
// returns the context's error if the context ends first; a var so a test
// can record the waits instead of sleeping them.
var kubeRetryWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
	// NodeStorage lists every node with its allocatable ephemeral storage
	// and the ephemeral-storage requests of the pods placed on it, in bytes;
	// the requests of pods no node holds yet come back as Unplaced.
	NodeStorage(ctx context.Context) (nodes []NodeStorage, unplaced []int64, err error)
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
// message. A pod still Pending names its hold through it (waitsOn) and
// a start failure its waiting step (awaitPodStart).
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

// NewInClusterKube builds the pod's client: main, register and
// remove-project each call it. The CA bundle loads here, the token is
// re-read per request (rotation needs no restart), and the API server
// is the kubelet's service address, no DNS — the egress policy admits none.
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

// CreateNamespace creates namespace name with its labels: the session
// provision creates a task's and the CI spawner a job's with it;
// ErrAlreadyExists when the name is taken.
func (k *InClusterKube) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": name, "labels": labels},
	}
	_, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces", body, nil)
	return err
}

// DeleteNamespace deletes the namespace — asynchronous: the name goes
// Terminating while its contents garbage-collect away, so a later list
// still answers it. The provision's failed setup rolls its namespace
// back with it, the session and CI teardowns take theirs down.
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

// NamespacesByLabel lists the names of the namespaces holding key=value
// (value "": the key set to anything), a Terminating one still among
// them. The boot sweep, the CI spawner, remove-project and buildvm's
// owner check read their namespaces through it.
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

// ListNamespacesByLabel is NamespacesByLabel with each namespace's
// labels and deletion state, reconcile's variant, which must not read a
// namespace teardown already deleted (its RoleBinding is gone, so a pod
// read answers Forbidden). AdmitLeases and the CI spawner call it.
func (k *InClusterKube) ListNamespacesByLabel(ctx context.Context, key, value string) ([]NamespaceInfo, error) {
	return k.listNamespaces(ctx, key, value)
}

// listNamespaces is the one GET behind both listings: a bare key when
// value is "" — a "key=" selector would match only empty values — each
// match read into a NamespaceInfo, its deletion state from the API
// server's deletionTimestamp.
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

// NamespaceLabel is namespace name's label key, "" when it has none (no
// error). Register's seed reconcile reads a namespace's project and
// objects labels by it; the SigNoz intake a namespace's project.
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

// CreateServiceAccount creates the service account in namespace: the
// session provision creates the task's ("task-"+id), whose pods bear
// factoryd's binding in it, and the CI spawner its runner's. A second
// create is ErrAlreadyExists.
func (k *InClusterKube) CreateServiceAccount(ctx context.Context, namespace, name string) error {
	body := map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata": map[string]any{"name": name, "namespace": namespace},
	}
	_, err := k.call(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/serviceaccounts", body, nil)
	return err
}

// CreateSecret creates the secret of stringData, secretType "" meaning
// Opaque: the task's git token, the pull-secret copy (dockerconfigjson),
// the CI runner's registration and the build TLS. A name taken is
// ErrAlreadyExists.
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

// GetSecretData reads one secret's data, base64-decoded; ErrNotFound
// when it is gone. The pull-secret copy, the staging registration's
// Flux accounts, the gate supervisor's token scrub and the CI runner's
// token read through it.
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

// SecretsByLabels lists namespace's secrets holding every label, key
// sorted (key=value), with name, type and decoded data: the staging
// registration discovers a project environment's Flux account secrets
// by their labels. One undecodable value fails the whole list.
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

// decodeSecret base64-decodes a secret's data values, an error naming
// namespace and secret: the shared tail of GetSecretData and
// SecretsByLabels.
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

// CreateCiliumNetworkPolicy creates the policy in namespace: the CI
// spawner creates its job namespace's egress policy with it, once per
// namespace. A name taken is ErrAlreadyExists;
// ApplyCiliumNetworkPolicy is the create-or-replace.
func (k *InClusterKube) CreateCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error {
	body := map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     spec,
	}
	_, err := k.call(ctx, http.MethodPost, "/apis/cilium.io/v2/namespaces/"+namespace+"/ciliumnetworkpolicies", body, nil)
	return err
}

// ApplyCiliumNetworkPolicy creates or replaces the policy by one
// server-side apply, factoryd as the field manager, so an apply of the
// spec in place changes nothing. The session provision, AdmitLeases and
// the CI spawner's smoke keep their egress policies current with it.
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

// CiliumNetworkPolicySpec reads the policy's spec, ErrNotFound when none
// exists. AdmitLeases compares the live worker egress spec with the one
// it wants and applies only on a difference.
func (k *InClusterKube) CiliumNetworkPolicySpec(ctx context.Context, namespace, name string) (map[string]any, error) {
	var out struct {
		Spec map[string]any `json:"spec"`
	}
	_, err := k.call(ctx, http.MethodGet, "/apis/cilium.io/v2/namespaces/"+namespace+"/ciliumnetworkpolicies/"+name, nil, &out)
	return out.Spec, err
}

// Create POSTs a complete object to a collection path such as
// /api/v1/namespaces/<ns>/resourcequotas: the session provision and
// AdmitLeases create rolebindings, quotas, limit ranges and cache
// claims with it. A name taken is ErrAlreadyExists.
func (k *InClusterKube) Create(ctx context.Context, path string, obj map[string]any) error {
	_, err := k.call(ctx, http.MethodPost, path, obj, nil)
	return err
}

// CreatePod creates the pod of the spec map, stamping name, namespace,
// apiVersion and kind into it: a session starts its worker pod, a gate
// reproduction its pod, the CI spawner its runner. A name taken is
// ErrAlreadyExists.
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

// DeletePod deletes the pod — asynchronous, a status read right after
// still answers it. The session deletes its worker pod on every end
// but a detach and on a stop, tolerating ErrNotFound.
func (k *InClusterKube) DeletePod(ctx context.Context, namespace, name string) error {
	_, err := k.call(ctx, http.MethodDelete, "/api/v1/namespaces/"+namespace+"/pods/"+name, nil, nil)
	return err
}

// PodStatus reports the pod's phase and its first container's exit code,
// -1 while it has not terminated. The log follower ends its stream on a
// terminal phase, the session's reattach and the CI spawner's pod watch
// read it.
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

// errPodEnded is a pod whose phase is Succeeded or Failed: nothing serves
// at its address, which the cluster may give another pod.
var errPodEnded = errors.New("it has ended, and its address may be another pod's")

// podEnded is PodIP's refusal of a pod in phase, nil while it may run.
func podEnded(namespace, name, phase string) error {
	if phase == "Succeeded" || phase == "Failed" {
		return fmt.Errorf("pod %s/%s is %s: %w", namespace, name, phase, errPodEnded)
	}
	return nil
}

// PodIP is the running pod's cluster address, ErrNotFound until it has
// one, errPodEnded once it ended. The session's post to its worker's
// endpoint, the message push and a workspace file read all address the
// pod by it.
func (k *InClusterKube) PodIP(ctx context.Context, namespace, name string) (string, error) {
	var pod struct {
		Status struct {
			Phase string `json:"phase"`
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods/"+name, &pod); err != nil {
		return "", err
	}
	if err := podEnded(namespace, name, pod.Status.Phase); err != nil {
		return "", err
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s/%s has no address yet: %w", namespace, name, ErrNotFound)
	}
	return pod.Status.PodIP, nil
}

// PodInitFinished is when the pod's first init container exited 0 with
// the termination message "ready", false with no error otherwise — also
// while no init container has run. The CI spawner tells a provisioned
// build VM by it and times its retries from it.
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

// PodLog streams the pod's stdout following (follow=true) until the pod
// ends or the stream drops; the log follower is the one caller and
// closes the stream. A one-shot tail is PodLogTail.
func (k *InClusterKube) PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error) {
	res, err := k.request(ctx, http.MethodGet,
		"/api/v1/namespaces/"+namespace+"/pods/"+name+"/log?follow=true", nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// PodDiagnosis reads the pod's phase, its pod-level reason and message,
// conditions and container states, init containers first, each running,
// waiting or terminated with the kubelet's reason and exit code. The
// gate supervisor's bundle and a session's start read it.
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

// PodLogTail is the pod's last lines of stdout, not followed and bounded
// by podLogTailBytes however long its lines are: the gate supervisor's
// bundle carries its slice of a sandbox's log.
func (k *InClusterKube) PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error) {
	b, err := k.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?tailLines=%d&limitBytes=%d", namespace, name, lines, podLogTailBytes), nil, nil)
	return string(b), err
}

// CacheClass is the StorageClass (infra k8s/platform/local-path) whose
// volume path is the claim's name: every namespace's claim of one name
// reaches the same directory on a node, and Retain keeps it.
const CacheClass = "factory-cache"

// cacheClaim is the claim manifest of the cache of name, class
// CacheClass, 20Gi ReadWriteOnce — local-path does not enforce the
// request — so every namespace's claim of one name reaches one
// directory on a node. The provision and the CI spawner Create it.
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
// sandbox. PATH is the sandbox's and the CI runner's own with
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

// cacheVolumes is a pod spec's volumes for the cache claim of that name
// plus the pod's own modcache emptyDir, 8Gi: the module cache keeps its
// file locks inside each sandbox. The worker pod spec and
// the CI runner's mount them.
func cacheVolumes(claim string) []any {
	return []any{
		map[string]any{"name": "cache", "persistentVolumeClaim": map[string]any{"claimName": claim}},
		map[string]any{"name": "modcache", "emptyDir": map[string]any{"sizeLimit": "8Gi"}},
	}
}

// deleteReleasedCaches deletes the Released volumes of CacheClass —
// Retain keeps their directory for the next claim; the session and CI
// teardowns sweep it. Deletion is async, so each sweep takes the earlier
// claims' and the last lingers: harmless, a Released volume never binds.
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

// PersistentVolumes lists every volume's name, storage class and phase;
// deleteReleasedCaches, its only caller, skips the volumes not of
// CacheClass or not Released.
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

// DeletePersistentVolume deletes the volume; deleteReleasedCaches
// tolerates ErrNotFound — a concurrent sweep or the API server's own
// reclamation was first.
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
// a ScheduledBackup's does not, so the console's jobs listing says "not
// reported" for one.
func (j CronJobState) RecordsSuccess() bool { return j.Kind != KindScheduledBackup }

// CronJobs lists namespace's batch/v1 CronJobs as CronJobState —
// schedule and suspension from the spec, the last two times from the
// status. ClusterJobs.CronJobs, for the console's jobs listing, is its
// only caller.
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
// ClusterJobs.CronJobs appends them beside the CronJobs of a namespace.
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

// CronJobs lists every namespace's CronJobs and ScheduledBackups —
// Namespaces, or platform, factory and forgejo when none are set — for
// the console's jobs listing; one namespace's failure names it and
// fails the whole.
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

// Deployment reads one apps/v1 Deployment as a roll record reads it —
// managedFields, pod template and conditions — ErrNotFound when it is
// gone. The roll watchers measure a roll by it, the SigNoz intake
// builds a factory Deployment's window from it.
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

// KustomizationState reads the Kustomization's Ready condition — status
// and message, "" while Flux has set none — and its last applied
// revision, "" while it never has; ErrNotFound for a name Flux never
// wrote. The roller's fluxApplied and the intake's flux alert read it.
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

// fluxNamespace and factoryKustomization name the Flux Kustomization that
// applies the code repository's k8s/, the one the roller suspends while a
// plan is open on main.
const (
	fluxNamespace        = "flux-system"
	factoryKustomization = "factory"
)

// SuspendKustomization sets spec.suspend of the Flux Kustomization name
// in namespace by one server-side apply, with no read first: an apply of
// the value it has changes nothing. The roller's setLevel is its only
// caller, suspend and resume both.
func (k *InClusterKube) SuspendKustomization(ctx context.Context, namespace, name string, suspend bool) error {
	_, err := k.call(ctx, http.MethodPatch, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+namespace+"/kustomizations/"+name+"?fieldManager=factoryd&force=true",
		map[string]any{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{"name": name, "namespace": namespace}, "spec": map[string]any{"suspend": suspend}}, nil)
	return err
}

// FluxRollouts are the newest rollouts of every HelmRelease and every
// Kustomization but the factory's own, the one applying the roll targets'
// Deployments in namespace, whose rolls those Deployments' own window
// judges; the SigNoz intake's RolloutPass and rolloutAt read them.
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

// rollout is the Kustomization's newest content's — FluxRollouts builds
// each listed one. Flux applies one content per reconcile, so its
// rollout starts and settles at that content's first reconcile; a
// revert keeps its old first, within one interval of the prior's last.
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

// appliesRollTargets reports whether the Kustomization's applied
// inventory holds a roll target's Deployment of namespace:
// FluxRollouts excludes the factory's own Kustomization by it, whose
// rolls its Deployments' own window judges.
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

// rollout is the HelmRelease's newest release's — FluxRollouts builds
// each listed one: from its deploy (an install, upgrade or rollback)
// until Ready reads True or False for its generation, one failed for
// good settled as much as a ready one; Unknown or unobserved still runs.
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

// getJSON is the GET whose body decodes into out: the read behind every
// listing and single object of this client.
func (k *InClusterKube) getJSON(ctx context.Context, path string, out any) error {
	_, err := k.call(ctx, http.MethodGet, path, nil, out)
	return err
}

// call performs one request — every method of this client speaks through
// it — and returns the raw body, a non-nil into decoded from it, the body
// read bounded at 4 MiB. A 429 that carries a Retry-After in whole seconds
// is sent again after that wait, held to 1 to 5 seconds, up to 3 times;
// the wait ends with the context, whose error call then returns. A 409 is
// ErrAlreadyExists, a 404 ErrNotFound, a 403 ErrForbidden; any other
// non-2xx, and a 429 left after the retries, names method, path, status, body.
func (k *InClusterKube) call(ctx context.Context, method, path string, body any, into any) ([]byte, error) {
	var res *http.Response
	var raw []byte
	for retries := 0; ; retries++ {
		var err error
		if res, err = k.request(ctx, method, path, body); err != nil {
			return nil, err
		}
		raw, err = io.ReadAll(io.LimitReader(res.Body, 4<<20))
		_ = res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("kube %s %s: %w", method, path, err)
		}
		secs, err := strconv.Atoi(res.Header.Get("Retry-After"))
		if res.StatusCode != http.StatusTooManyRequests || err != nil || retries == 3 {
			break
		}
		if err := kubeRetryWait(ctx, time.Duration(min(max(secs, 1), 5))*time.Second); err != nil {
			return nil, err
		}
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

// request builds and performs the HTTP request for call and PodLog: the
// service-account token is read afresh each call — projected tokens
// rotate — a body is JSON, a patch apply-patch+yaml, and the client
// defaults to 30 seconds. PodLog borrows its stream; call owns the rest.
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

// Apply creates or brings the object at path to obj by server-side
// apply, factoryd as the field manager; an apply that changes nothing
// keeps the resourceVersion, telling Created, Updated and Unchanged
// apart. Register's seed reconcile applies a project's objects with it.
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
// updated one is available. Otherwise it says what lags; the project
// alerts handler asks it of a namespace's Deployments.
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

// Deployments lists namespace's Deployments with their rollout state —
// spec.replicas read as 1 when unset, the images in spec order. The
// project alerts handler and the health page's name listing read them.
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

// NodeStorage is one schedulable node's allocatable ephemeral storage and
// the ephemeral storage its live pods request, in bytes.
type NodeStorage struct {
	Name        string
	Allocatable int64
	Requested   int64
}

// podListPage is how many pods one list call asks for, so no response
// outgrows the client's read bound.
const podListPage = 200

// storageNamespaces are the namespaces besides the task namespaces
// (factory.ai/task) whose pods request ephemeral storage: the forgejo
// namespace holds the Actions runner.
var storageNamespaces = []string{"forgejo"}

// NodeStorage reads the schedulable nodes (not cordoned, Ready) and the
// pods not yet ended in the task namespaces and storageNamespaces. A pod's
// request is the larger of its containers' sum and its largest init
// container's, as the scheduler counts it; a pod whose request cannot be
// read counts as filling its node.
func (k *InClusterKube) NodeStorage(ctx context.Context) ([]NodeStorage, []int64, error) {
	var nodeList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/api/v1/nodes", &nodeList); err != nil {
		return nil, nil, err
	}
	nodes := make([]NodeStorage, 0, len(nodeList.Items))
	at := map[string]int{}
	for _, n := range nodeList.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status == "True"
			}
		}
		if n.Spec.Unschedulable || !ready {
			continue
		}
		alloc, err := parseBytes(n.Status.Allocatable["ephemeral-storage"])
		if err != nil {
			return nil, nil, fmt.Errorf("node %s ephemeral-storage: %w", n.Metadata.Name, err)
		}
		at[n.Metadata.Name] = len(nodes)
		nodes = append(nodes, NodeStorage{Name: n.Metadata.Name, Allocatable: alloc})
	}
	listed, err := k.ListNamespacesByLabel(ctx, "factory.ai/task", "")
	if err != nil {
		return nil, nil, err
	}
	var namespaces []string
	for _, ns := range listed {
		if !ns.Terminating {
			namespaces = append(namespaces, ns.Name)
		}
	}
	task := len(namespaces)
	namespaces = append(namespaces, storageNamespaces...)
	var unplaced []int64
	for i, ns := range namespaces {
		err := k.eachPod(ctx, ns, func(nodeName string, req int64, readable bool) {
			switch i, placed := at[nodeName]; {
			case placed && !readable:
				nodes[i].Requested = nodes[i].Allocatable
			case placed:
				nodes[i].Requested += req
			case nodeName == "" && readable && req > 0:
				unplaced = append(unplaced, req)
			}
		})
		if errors.Is(err, ErrNotFound) && i < task {
			// a task namespace torn down since the listing
			continue
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return nodes, unplaced, nil
}

// eachPod pages through namespace's pods not yet ended and calls fn with
// each one's node ("" before it has one) and its ephemeral-storage
// request; readable is false when a quantity of the pod does not parse,
// which is logged.
func (k *InClusterKube) eachPod(ctx context.Context, namespace string, fn func(nodeName string, req int64, readable bool)) error {
	type container struct {
		Resources struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	}
	cont := ""
	for {
		q := url.Values{"fieldSelector": []string{"status.phase!=Succeeded,status.phase!=Failed"}, "limit": []string{strconv.Itoa(podListPage)}}
		if cont != "" {
			q.Set("continue", cont)
		}
		var list struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					NodeName       string      `json:"nodeName"`
					Containers     []container `json:"containers"`
					InitContainers []container `json:"initContainers"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods?"+q.Encode(), &list); err != nil {
			return err
		}
		for _, p := range list.Items {
			var sum, most int64
			var bad error
			for _, c := range p.Spec.Containers {
				b, err := parseBytes(c.Resources.Requests["ephemeral-storage"])
				sum, bad = sum+b, cmp.Or(bad, err)
			}
			for _, c := range p.Spec.InitContainers {
				b, err := parseBytes(c.Resources.Requests["ephemeral-storage"])
				most, bad = max(most, b), cmp.Or(bad, err)
			}
			if bad != nil {
				log.Printf("kube: pod %s/%s: %v: counted as filling its node", namespace, p.Metadata.Name, bad)
			}
			fn(p.Spec.NodeName, max(sum, most), bad == nil)
		}
		if cont = list.Metadata.Continue; cont == "" {
			return nil
		}
	}
}

// byteUnits are the Kubernetes quantity suffixes as exact ratios; the
// binary ones are checked before the single letters they end in.
var byteUnits = []struct {
	suffix string
	mult   *big.Rat
}{
	{"Ki", big.NewRat(1<<10, 1)}, {"Mi", big.NewRat(1<<20, 1)}, {"Gi", big.NewRat(1<<30, 1)}, {"Ti", big.NewRat(1<<40, 1)},
	{"Pi", big.NewRat(1<<50, 1)}, {"Ei", big.NewRat(1<<60, 1)},
	{"n", big.NewRat(1, 1e9)}, {"u", big.NewRat(1, 1e6)}, {"m", big.NewRat(1, 1e3)},
	{"k", big.NewRat(1e3, 1)}, {"M", big.NewRat(1e6, 1)}, {"G", big.NewRat(1e9, 1)}, {"T", big.NewRat(1e12, 1)},
	{"P", big.NewRat(1e15, 1)}, {"E", big.NewRat(1e18, 1)},
}

// parseBytes reads a Kubernetes quantity as whole bytes, rounded up: a
// decimal with an optional binary or decimal suffix or an exponent (1e9,
// 129e6, 1.5Gi, 500m); "" is 0.
func parseBytes(q string) (int64, error) {
	if q == "" {
		return 0, nil
	}
	r, ok := new(big.Rat).SetString(q)
	if !ok {
		for _, u := range byteUnits {
			if num, found := strings.CutSuffix(q, u.suffix); found {
				if r, ok = new(big.Rat).SetString(num); ok {
					r.Mul(r, u.mult)
				}
				break
			}
		}
	}
	if !ok || r.Sign() < 0 {
		return 0, fmt.Errorf("quantity %q is not one", q)
	}
	n := new(big.Int).Div(new(big.Int).Add(r.Num(), new(big.Int).Sub(r.Denom(), big.NewInt(1))), r.Denom())
	if !n.IsInt64() {
		return 0, fmt.Errorf("quantity %q is too large", q)
	}
	return n.Int64(), nil
}
