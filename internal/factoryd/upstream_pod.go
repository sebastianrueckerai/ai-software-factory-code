package factoryd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const (
	upstreamNamespacePrefix = "factory-agents-upstream-"
	upstreamPodName         = "upstream-copy"
	upstreamSecretName      = "upstream-copy-login" // #nosec G101 -- a secret's name
	// UpstreamLabel marks a copy pod's namespace.
	UpstreamLabel = "factory.ai/upstream-copy"
	// UpstreamSecretDir is where the pod finds the store's login and the
	// lines to copy.
	UpstreamSecretDir = "/var/run/secrets/upstream" // #nosec G101 -- a mount path
	// UpstreamFinding leads each line the copy pod prints for an image line
	// it cannot copy.
	UpstreamFinding = "finding: "
	// upstreamTailLines is how much of the pod's log factoryd reads: the
	// findings, printed last, and the reason of a failure.
	upstreamTailLines   = 400
	upstreamReasonLines = 5
)

// upstreamFQDNs are the upstream registries upstream-images.txt names, their
// token endpoints and the hosts their blobs redirect to.
var upstreamFQDNs = []any{
	map[string]any{"matchName": "registry-1.docker.io"},
	map[string]any{"matchName": "auth.docker.io"},
	map[string]any{"matchName": "production.cloudfront.docker.com"},
	map[string]any{"matchName": "production.cloudflare.docker.com"},
	map[string]any{"matchName": "docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com"},
	map[string]any{"matchName": "ghcr.io"},
	map[string]any{"matchName": "pkg-containers.githubusercontent.com"},
	map[string]any{"matchName": "quay.io"},
	map[string]any{"matchName": "registry.k8s.io"},
	map[string]any{"matchName": "cdn.registry.k8s.io"},
	map[string]any{"matchName": "code.forgejo.org"},
	map[string]any{"matchName": "factory.talos.dev"},
	map[string]any{"matchName": "cdn.factory.talos.dev"},
	map[string]any{"matchPattern": "cdn*.quay.io"},
	map[string]any{"matchPattern": "*-docker.pkg.dev"},
}

// UpstreamPod copies a pull request's new upstream image lines into the
// store in a one-shot pod of its own: the pod holds the store's write login
// and egress to the upstream registries and the store, so factoryd keeps no
// egress to the internet. The pod runs `factoryd upstream-copy` from
// factoryd's own image, which has the copy code and a pull secret in
// place; its namespace and the login Secret in it die with the run.
type UpstreamPod struct {
	Kube Kube
	// OpenBao holds the site admin's login, which writes the store.
	OpenBao SecretReader
	// Namespace is factoryd's own, whose Deployment names the image and
	// whose pull secret the pod's namespace copies.
	Namespace  string
	PullSecret string
	// StoreURL is the in-cluster Forgejo, where the store lives.
	StoreURL string
	Scrub    *redact.Scrubber
	// Poll is the wait between reads of the pod's phase; zero is 2 s.
	Poll time.Duration
	// Deadline is how long a pod may run; zero is 25 minutes.
	Deadline time.Duration
}

func (u *UpstreamPod) deadline() time.Duration {
	if u.Deadline > 0 {
		return u.Deadline
	}
	return 25 * time.Minute
}

// Copy copies the image lines head adds to base into the store and returns
// the findings, the lines the upstream cannot serve. A run that fails
// returns the pod's exit and its last log lines, scrubbed.
func (u *UpstreamPod) Copy(ctx context.Context, base, head string) ([]string, error) {
	lines, findings := addedImageLines(base, head)
	if len(lines) == 0 {
		return findings, nil
	}
	admin, err := adminAuth(ctx, u.OpenBao)
	if err != nil {
		return findings, u.scrubbed(err, "")
	}
	got, err := u.run(ctx, lines, admin)
	findings = append(findings, got...)
	return findings, u.scrubbed(err, admin.Password)
}

func (u *UpstreamPod) scrubbed(err error, secret string) error {
	if err == nil {
		return nil
	}
	return errors.New(u.scrub(err.Error(), secret))
}

func (u *UpstreamPod) scrub(s, secret string) string {
	sc := u.Scrub
	if sc == nil {
		sc = redact.New()
	}
	return redact.New(secret).String(sc.String(s))
}

func (u *UpstreamPod) run(ctx context.Context, lines []string, admin AdminAuth) ([]string, error) {
	img, err := u.image(ctx)
	if err != nil {
		return nil, err
	}
	store, err := url.Parse(u.StoreURL)
	if err != nil || store.Hostname() == "" {
		return nil, fmt.Errorf("upstream copy: the store URL %q is not absolute", u.StoreURL)
	}
	u.sweep(ctx)
	ns, id := u.namespaceName()
	// the run's end, or its context's, takes the namespace and the login in it
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := u.Kube.DeleteNamespace(ctx, ns); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("upstream copy: delete namespace %s: %v", ns, err)
		}
	}()
	labels := map[string]string{
		"factory.ai/task":                    id,
		UpstreamLabel:                        "true",
		"pod-security.kubernetes.io/enforce": "restricted",
	}
	if err := createFactorydNamespace(ctx, u.Kube, u.Namespace, ns, labels); err != nil {
		return nil, err
	}
	steps := []ciSetupStep{
		{"egress policy", func() error {
			return u.Kube.CreateCiliumNetworkPolicy(ctx, ns, "upstream-egress", upstreamEgressSpec(store.Hostname()))
		}},
		// the login reaches the pod as files of a Secret that dies with its
		// namespace, as the CI runner's registration token does, never in the
		// stored pod spec or an env value
		{"login secret", func() error {
			return u.Kube.CreateSecret(ctx, ns, upstreamSecretName, "", map[string]string{
				"user": admin.User, "password": admin.Password, "lines": strings.Join(lines, "\n"),
			})
		}},
		{"pull secret", func() error {
			return copyPullSecret(ctx, u.Kube, u.Namespace, u.PullSecret, ns)
		}},
	}
	if err := runSetupSteps(ctx, steps); err != nil {
		return nil, err
	}
	if err := u.Kube.CreatePod(ctx, ns, upstreamPodName, u.podSpec(img)); err != nil {
		return nil, fmt.Errorf("create the copy pod: %w", err)
	}
	phase, exit, err := u.wait(ctx, ns)
	if err != nil {
		return nil, err
	}
	tail, err := u.Kube.PodLogTail(ctx, ns, upstreamPodName, upstreamTailLines)
	if err != nil {
		return nil, fmt.Errorf("read the copy pod's log (phase %s, exit %d): %w", phase, exit, err)
	}
	if phase != "Succeeded" {
		return nil, fmt.Errorf("the upstream copy pod %s with exit %d: %s", strings.ToLower(phase), exit, strings.Join(lastLines(tail, upstreamReasonLines), " | "))
	}
	var findings []string
	for _, l := range strings.Split(tail, "\n") {
		if f, ok := strings.CutPrefix(l, UpstreamFinding); ok {
			findings = append(findings, f)
		}
	}
	return findings, nil
}

// image is the image of factoryd's own Deployment: the copy code is its
// subcommand.
func (u *UpstreamPod) image(ctx context.Context) (string, error) {
	d, err := u.Kube.Deployment(ctx, u.Namespace, "factoryd")
	if err != nil {
		return "", fmt.Errorf("read factoryd's image: %w", err)
	}
	c := d.Spec.Template.Spec.Containers
	if len(c) == 0 || c[0].Image == "" {
		return "", errors.New("factoryd's Deployment names no image")
	}
	return c[0].Image, nil
}

// namespaceName is the run's namespace and its task label: the start time
// leads the name, so a later run can tell a namespace left behind.
func (u *UpstreamPod) namespaceName() (ns, id string) {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id = fmt.Sprintf("upstream-%d-%s", time.Now().Unix(), hex.EncodeToString(b))
	return "factory-agents-" + id, id
}

// sweep deletes the copy namespaces an earlier run left behind (a failed
// delete): those started more than a pod's deadline ago. Another run's live
// namespace is younger.
func (u *UpstreamPod) sweep(ctx context.Context) {
	infos, err := u.Kube.ListNamespacesByLabel(ctx, UpstreamLabel, "true")
	if err != nil {
		log.Printf("upstream copy: list namespaces: %v", err)
		return
	}
	for _, info := range infos {
		if info.Terminating {
			continue
		}
		started, _, _ := strings.Cut(strings.TrimPrefix(info.Name, upstreamNamespacePrefix), "-")
		at, err := strconv.ParseInt(started, 10, 64)
		if err == nil && time.Since(time.Unix(at, 0)) < u.deadline()+time.Minute {
			continue
		}
		if err := u.Kube.DeleteNamespace(ctx, info.Name); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("upstream copy: delete the old namespace %s: %v", info.Name, err)
		}
	}
}

// wait returns the pod's final phase and its exit code.
func (u *UpstreamPod) wait(ctx context.Context, ns string) (string, int32, error) {
	poll := u.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		phase, exit, err := u.Kube.PodStatus(ctx, ns, upstreamPodName)
		if err != nil {
			return "", -1, fmt.Errorf("read the copy pod: %w", err)
		}
		if phase == "Succeeded" || phase == "Failed" {
			return phase, exit, nil
		}
		select {
		case <-ctx.Done():
			return "", -1, fmt.Errorf("waiting for the copy pod: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

func (u *UpstreamPod) podSpec(img string) map[string]any {
	pullSecrets := []any{}
	if u.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": u.PullSecret})
	}
	return requestEmptyDirs(map[string]any{
		"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": upstreamPodName}},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"activeDeadlineSeconds":        int64(u.deadline() / time.Second),
			"runtimeClassName":             "gvisor",
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"imagePullSecrets":              pullSecrets,
			"terminationGracePeriodSeconds": 30,
			"dnsConfig":                     map[string]any{"options": []any{map[string]any{"name": "ndots", "value": "1"}}},
			"containers": []any{map[string]any{
				"name":    "copy",
				"image":   img,
				"command": []any{"/usr/local/bin/factoryd"},
				"args":    []any{"upstream-copy", "-store", u.StoreURL, "-dir", UpstreamSecretDir},
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "250m", "memory": "256Mi"},
					"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
				},
				"volumeMounts": []any{
					map[string]any{"name": "login", "mountPath": UpstreamSecretDir, "readOnly": true},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
				},
				"securityContext": ciSecurityContext(),
			}},
			"volumes": []any{
				map[string]any{"name": "login", "secret": map[string]any{"secretName": upstreamSecretName}},
				// a copy holds one blob, up to maxBlobBytes, at a time
				map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
			},
		},
	})
}

// upstreamEgressSpec admits the copy pod to the in-cluster Forgejo (the
// store), the upstream registries by name on 443 and DNS for those names.
func upstreamEgressSpec(storeHost string) map[string]any {
	dns := append(append([]any{}, upstreamFQDNs...), matchNames(storeHost)...)
	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": upstreamPodName},
		},
		"egressDeny": []any{
			map[string]any{"toCIDR": []string{"169.254.169.254/32"}},
		},
		"egress": []any{forgejoHTTPEgress(), fqdnEgress(upstreamFQDNs), dnsEgress(dns)},
	}
}
