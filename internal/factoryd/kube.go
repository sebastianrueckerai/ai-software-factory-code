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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Sentinel errors so callers can treat already-exists and gone as normal
// control flow instead of inspecting messages.
var (
	ErrAlreadyExists = errors.New("kubernetes: already exists")
	ErrNotFound      = errors.New("kubernetes: not found")
	ErrForbidden     = errors.New("kubernetes: forbidden")
)

// saDir is a var so the wire test can point it at a stub token.
var saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Kube is the slice of the Kubernetes API the runner needs. The production
// client talks raw REST with the pod's own service account (token re-read
// per request, because projected tokens rotate); tests script a fake.
type Kube interface {
	CreateNamespace(ctx context.Context, name string, labels map[string]string) error
	DeleteNamespace(ctx context.Context, name string) error
	NamespacesByLabel(ctx context.Context, key, value string) ([]string, error)
	CreateServiceAccount(ctx context.Context, namespace, name string) error
	CreateSecret(ctx context.Context, namespace, name, secretType string, stringData map[string]string) error
	GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error)
	CreateCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error
	// Create POSTs a complete object to a collection path such as
	// /api/v1/namespaces/<ns>/resourcequotas.
	Create(ctx context.Context, path string, obj map[string]any) error
	CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error
	DeletePod(ctx context.Context, namespace, name string) error
	// PodStatus reports the pod phase and the container exit code (-1 while
	// the container has not terminated).
	PodStatus(ctx context.Context, namespace, name string) (phase string, exitCode int32, err error)
	// PodLog streams the pod's stdout, following until the pod terminates or
	// ctx is cancelled. The caller closes the stream.
	PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error)
}

// InClusterKube is the raw-REST Kubernetes client. No client-go: the runner
// needs a dozen endpoints, and owning the wire keeps the dependency out.
type InClusterKube struct {
	Host string // https://kubernetes.default.svc
	HTTP *http.Client
}

// NewInClusterKube loads the pod's CA bundle; the bearer token is re-read
// on every request so rotation needs no restart.
func NewInClusterKube() (*InClusterKube, error) {
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("kube in-cluster: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kube in-cluster: ca.crt holds no certificates")
	}
	return &InClusterKube{
		Host: "https://kubernetes.default.svc",
		HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
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

func (k *InClusterKube) NamespacesByLabel(ctx context.Context, key, value string) ([]string, error) {
	sel := key
	if value != "" {
		sel += "=" + value // "key=" would select only empty values
	}
	q := url.Values{"labelSelector": []string{sel}}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		names = append(names, it.Metadata.Name)
	}
	return names, nil
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
	out := make(map[string]string, len(sec.Data))
	for key, b64 := range sec.Data {
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

func (k *InClusterKube) PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error) {
	res, err := k.request(ctx, http.MethodGet,
		"/api/v1/namespaces/"+namespace+"/pods/"+name+"/log?follow=true", nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
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
	req, err := http.NewRequestWithContext(ctx, method, k.Host+path, reader)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return nil, fmt.Errorf("kube in-cluster token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := k.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kube %s %s: %w", method, path, err)
	}
	return res, nil
}
