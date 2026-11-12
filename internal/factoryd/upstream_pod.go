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
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const (
	upstreamNamespacePrefix = "factory-agents-upstream-"
	upstreamPodName         = "upstream-copy"
	upstreamSecretName      = "upstream-copy-login" // #nosec G101 -- a secret's name
	// UpstreamUser is the Forgejo user the copy writes as: a member of a
	// package-write-only team of org upstream and nothing else.
	UpstreamUser = "factory-upstream-copy"
	upstreamOrg  = "upstream"
	upstreamTeam = "upstream-copy"
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
	// upstreamTokenPrefix leads the name of each run's package token.
	upstreamTokenPrefix     = "upstream-copy-"
	upstreamTokenSweepEvery = 10 * time.Minute
)

// How long a copy pod may run, how long its start may take on top of that,
// and the age past which a namespace or token of a run is its leftover: the
// longest run plus 5 minutes. Variables so a test shortens them.
var (
	upstreamDeadline   = 25 * time.Minute
	upstreamStartGrace = 2 * time.Minute
	upstreamSweepSlack = 5 * time.Minute
)

func upstreamSweepAge() time.Duration {
	return upstreamDeadline + upstreamStartGrace + upstreamSweepSlack
}

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
// store in a one-shot pod of its own: the pod holds a package token of the
// store, minted for the run and revoked when it ends, and egress to the
// upstream registries and the store, so factoryd keeps no egress to the
// internet and the pod never holds the forge's own login. The pod runs
// `factoryd upstream-copy` from factoryd's own image, which has the copy
// code and a pull secret in place; its namespace and the token's Secret
// die with the run.
type UpstreamPod struct {
	Kube Kube
	// Forgejo mints and revokes the run's token; OpenBao holds the site
	// admin's login that does it, which never leaves factoryd.
	Forgejo *Forgejo
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

	sweepMu    sync.Mutex
	tokenSwept time.Time
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
	got, token, err := u.run(ctx, lines, admin)
	for _, f := range got {
		findings = append(findings, u.scrub(f, token))
	}
	return findings, u.scrubbed(err, token)
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

// run returns the pod's findings and the token it minted, for the caller's
// scrub.
func (u *UpstreamPod) run(ctx context.Context, lines []string, admin AdminAuth) (findings []string, token string, err error) {
	img, err := u.image(ctx)
	if err != nil {
		return nil, "", err
	}
	store, err := url.Parse(u.StoreURL)
	if err != nil || store.Hostname() == "" {
		return nil, "", fmt.Errorf("upstream copy: the store URL %q is not absolute", u.StoreURL)
	}
	u.Sweep(ctx, true)
	suffix := runSuffix()
	id, ns, tokenName := "upstream-"+suffix, "factory-agents-upstream-"+suffix, upstreamTokenPrefix+suffix
	if err := u.ensureUser(ctx, admin); err != nil {
		return nil, "", err
	}
	token, err = u.Forgejo.AdminMintToken(ctx, admin, UpstreamUser, tokenName, []string{"write:package"}, nil)
	if err != nil {
		return nil, "", fmt.Errorf("mint the copy's package token: %w", err)
	}
	// the run's end, or its context's, takes the namespace and the token
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := u.Kube.DeleteNamespace(ctx, ns); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("upstream copy: delete namespace %s: %v", ns, err)
		}
		if _, err := u.Forgejo.AdminDeleteTokens(ctx, admin, UpstreamUser, tokenName); err != nil {
			log.Printf("upstream copy: revoke token %s: %v", tokenName, err)
		}
	}()
	labels := map[string]string{
		"factory.ai/task":                    id,
		UpstreamLabel:                        "true",
		"pod-security.kubernetes.io/enforce": "restricted",
	}
	if err := createFactorydNamespace(ctx, u.Kube, u.Namespace, ns, labels); err != nil {
		return nil, token, err
	}
	steps := []ciSetupStep{
		{"egress policy", func() error {
			return u.Kube.CreateCiliumNetworkPolicy(ctx, ns, "upstream-egress", upstreamEgressSpec(store.Hostname()))
		}},
		// the token reaches the pod as a file of a Secret that dies with its
		// namespace, as the CI runner's registration token does, never in the
		// stored pod spec or an env value
		{"login secret", func() error {
			return u.Kube.CreateSecret(ctx, ns, upstreamSecretName, "", map[string]string{
				"user": UpstreamUser, "password": token, "lines": strings.Join(lines, "\n"),
			})
		}},
		{"pull secret", func() error {
			return copyPullSecret(ctx, u.Kube, u.Namespace, u.PullSecret, ns)
		}},
	}
	if err := runSetupSteps(ctx, steps); err != nil {
		return nil, token, err
	}
	if err := u.Kube.CreatePod(ctx, ns, upstreamPodName, u.podSpec(img)); err != nil {
		return nil, token, fmt.Errorf("create the copy pod: %w", err)
	}
	phase, exit, err := u.wait(ctx, ns)
	if err != nil {
		return nil, token, err
	}
	tail, err := u.Kube.PodLogTail(ctx, ns, upstreamPodName, upstreamTailLines)
	if err != nil {
		return nil, token, fmt.Errorf("read the copy pod's log (phase %s, exit %d): %w", phase, exit, err)
	}
	if phase != "Succeeded" {
		return nil, token, fmt.Errorf("the upstream copy pod %s with exit %d: %s%s", strings.ToLower(phase), exit,
			strings.Join(lastLines(tail, upstreamReasonLines), " | "), u.state(ctx, ns))
	}
	for _, l := range strings.Split(tail, "\n") {
		if f, ok := strings.CutPrefix(l, UpstreamFinding); ok {
			findings = append(findings, f)
		}
	}
	return findings, token, nil
}

// ensureUser keeps the user the copy runs as and its team of package write
// in the organisation that holds the upstream images, and in no other. A
// token of the site admin would write every owner's packages. A registry
// bearer drawn from a token at /v2/token stays valid up to 24 hours after the
// token's revocation (Forgejo 17.0.1's bearer lifetime), so this user's narrow
// rights are the bound, and the revocation only ends new bearers.
func (u *UpstreamPod) ensureUser(ctx context.Context, admin AdminAuth) error {
	if _, err := u.Forgejo.AdminEnsureUser(ctx, admin, UpstreamUser); err != nil {
		return fmt.Errorf("the copy's user %s: %w", UpstreamUser, err)
	}
	if _, err := u.Forgejo.AdminEnsurePackageTeam(ctx, admin, upstreamOrg, upstreamTeam, UpstreamUser); err != nil {
		return fmt.Errorf("the copy's package team %s of %s: %w", upstreamTeam, upstreamOrg, err)
	}
	return nil
}

// runSuffix names a run: its start time leads, so a later sweep can tell a
// run left behind.
func runSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().Unix(), hex.EncodeToString(b))
}

// state is what the pod's diagnosis says of it, led by " (": its phase, its
// reason and message, and what it waits on; "" when nothing can be read.
func (u *UpstreamPod) state(ctx context.Context, ns string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	d, err := u.Kube.PodDiagnosis(ctx, ns, upstreamPodName)
	if err != nil {
		return ""
	}
	parts := []string{"phase " + d.Phase}
	if d.Reason != "" {
		parts = append(parts, strings.TrimSpace(d.Reason+" "+d.Message))
	}
	if w := waitsOn(d); w != "" {
		parts = append(parts, w)
	}
	return " (" + strings.Join(parts, "; ") + ")"
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
	if !strings.Contains(c[0].Image, "@sha256:") {
		return "", fmt.Errorf("factoryd's image %q carries no digest: pin the Deployment's image as name@sha256:<digest>, which a roll writes", c[0].Image)
	}
	return c[0].Image, nil
}

// startedAt is when the run named by name began, read from its suffix.
func startedAt(name, prefix string) (time.Time, bool) {
	started, _, _ := strings.Cut(strings.TrimPrefix(name, prefix), "-")
	at, err := strconv.ParseInt(started, 10, 64)
	return time.Unix(at, 0), err == nil && strings.HasPrefix(name, prefix)
}

// Sweep deletes the namespaces and revokes the tokens an earlier run left
// behind (a failed delete, a restart): those begun more than the longest run
// plus 5 minutes ago. Another run's live ones are younger. The CI spawner's
// reconcile pass calls it every tick for the namespaces; the tokens are
// listed at most every 10 minutes, or at each run's start (force).
func (u *UpstreamPod) Sweep(ctx context.Context, force bool) {
	infos, err := u.Kube.ListNamespacesByLabel(ctx, UpstreamLabel, "true")
	if err != nil {
		log.Printf("upstream copy: list namespaces: %v", err)
	}
	for _, info := range infos {
		if info.Terminating {
			continue
		}
		if at, ok := startedAt(info.Name, upstreamNamespacePrefix); ok && time.Since(at) < upstreamSweepAge() {
			continue
		}
		if err := u.Kube.DeleteNamespace(ctx, info.Name); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("upstream copy: delete the old namespace %s: %v", info.Name, err)
		}
	}
	u.sweepMu.Lock()
	due := force || time.Since(u.tokenSwept) >= upstreamTokenSweepEvery
	if due {
		u.tokenSwept = time.Now()
	}
	u.sweepMu.Unlock()
	if !due || u.Forgejo == nil {
		return
	}
	admin, err := adminAuth(ctx, u.OpenBao)
	if err != nil {
		log.Printf("upstream copy: sweep tokens: %v", err)
		return
	}
	if _, err := u.Forgejo.adminDeleteTokensWhere(ctx, admin, UpstreamUser, func(name string) bool {
		at, ok := startedAt(name, upstreamTokenPrefix)
		return ok && time.Since(at) >= upstreamSweepAge()
	}); err != nil {
		log.Printf("upstream copy: sweep tokens: %v", err)
	}
}

// wait returns the pod's final phase and its exit code. It ends at the
// longest run's bound with the pod's state: a pod that never starts says
// why.
func (u *UpstreamPod) wait(ctx context.Context, ns string) (string, int32, error) {
	poll := u.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	bound := upstreamDeadline + upstreamStartGrace
	runCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	for {
		phase, exit, err := u.Kube.PodStatus(runCtx, ns, upstreamPodName)
		if err != nil && ctx.Err() == nil && runCtx.Err() == nil {
			return "", -1, fmt.Errorf("read the copy pod: %w", err)
		}
		if err == nil && (phase == "Succeeded" || phase == "Failed") {
			return phase, exit, nil
		}
		select {
		case <-ctx.Done():
			return "", -1, fmt.Errorf("waiting for the copy pod: %w", ctx.Err())
		case <-runCtx.Done():
			return "", -1, fmt.Errorf("the copy pod did not end within %s%s", bound, u.state(ctx, ns))
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
			"activeDeadlineSeconds":        int64(upstreamDeadline / time.Second),
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
		"egress": []any{forgejoHTTPEgress(), fqdnEgress(upstreamFQDNs), dnsEgress(dns)},
	}
}
