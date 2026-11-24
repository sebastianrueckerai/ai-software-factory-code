package factoryd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/bump"
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
	// upstreamSecretDir is where the pod finds the store's login and the
	// files it copies by.
	upstreamSecretDir = "/var/run/secrets/upstream" // #nosec G101 -- a mount path
	// upstreamFinding leads each line the pod prints again at its end for a
	// line a script could not copy.
	upstreamFinding = "finding: "
	// upstreamTailLines is how much of the pod's log factoryd reads: the
	// findings, printed last, and the reason of a failure.
	upstreamTailLines   = 400
	upstreamReasonLines = 5
	// upstreamTokenPrefix leads the name of each run's package token.
	upstreamTokenPrefix     = "upstream-copy-"
	upstreamTokenSweepEvery = 10 * time.Minute
)

// How long a copy pod and a bump pod may run, how long its start may take
// on top of that, and the age past which a namespace or token of a run is
// its leftover: the longest run plus 5 minutes. Variables so a test
// shortens them.
var (
	upstreamDeadline   = 25 * time.Minute
	bumpDeadline       = 15 * time.Minute
	upstreamStartGrace = 2 * time.Minute
	upstreamSweepSlack = 5 * time.Minute
)

// The bump pod: its name, and how much of its log factoryd reads, which
// ends with the result (the files are about 80 KB).
const (
	bumpPodName  = "upstream-bump"
	bumpLogBytes = 16 << 20
)

// bumpFQDNs are the hosts the bump pod reaches: the copy's, whose
// registries and release downloads the lookup reads too, and the makers'
// APIs it lists versions from (GitHub's, the PGDG archive's, go.dev's
// downloads and where they redirect), and the Go checksum database, which
// go get asks.
var bumpFQDNs = append(slices.Clone(upstreamFQDNs),
	map[string]any{"matchName": "api.github.com"},
	map[string]any{"matchName": "apt.postgresql.org"},
	map[string]any{"matchName": "go.dev"},
	map[string]any{"matchName": "dl.google.com"},
	map[string]any{"matchName": "sum.golang.org"},
)

// bumpProgram is the bump pod's program: main's files at their repository
// paths in a directory of its own, then upstream-bump, which prints its
// result last.
var bumpProgram = func() string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\nin=" + upstreamSecretDir + "\ncd \"$(mktemp -d)\"\nmkdir -p factory opencode-plugin\n")
	for _, p := range bump.Files {
		fmt.Fprintf(&b, "cp \"$in/%s\" %s\n", path.Base(p), p)
	}
	b.WriteString("exec upstream-bump -result\n")
	return b.String()
}()

// bumpEgressSpec admits the bump pod to bumpFQDNs by name on 443, and DNS
// for those names: no store, no cluster.
func bumpEgressSpec() map[string]any {
	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": bumpPodName},
		},
		"egress": []any{fqdnEgress(bumpFQDNs), dnsEgress(bumpFQDNs)},
	}
}

func upstreamSweepAge() time.Duration {
	return upstreamDeadline + upstreamStartGrace + upstreamSweepSlack
}

// upstreamFQDNs are the upstream registries upstream-images.txt names, their
// token endpoints and the hosts their blobs redirect to; the hosts the https
// lines of both lists download from and where those downloads redirect; and
// the Go module proxy and the npm registry, which fill the two tarballs.
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
	map[string]any{"matchName": "github.com"},
	map[string]any{"matchName": "release-assets.githubusercontent.com"},
	map[string]any{"matchName": "nodejs.org"},
	map[string]any{"matchName": "vuln.go.dev"},
	map[string]any{"matchName": "www.postgresql.org"},
	map[string]any{"matchName": "proxy.golang.org"},
	map[string]any{"matchName": "registry.npmjs.org"},
}

// upstreamHeadFiles are a head's files the copy reads: the two lists and the
// inputs of the Go module and npm tarballs. upstreamScripts copy them, main's
// own: the pod runs no file of the head.
var (
	upstreamHeadFiles = []string{upstreamImagesPath, upstreamFilesPath, "go.mod", "go.sum", "opencode-plugin/package.json", "opencode-plugin/package-lock.json"}
	upstreamScripts   = []string{"scripts/upstream-copy", "scripts/upstream-files"}
)

// upstreamProgram is the pod's program, main CI's upstream job's: the
// files at their repository paths in a directory of its own, crane's login
// to the store, then both scripts. Each line a script could not copy is
// printed again at the end, led by upstreamFinding, where the log's tail
// holds it.
var upstreamProgram = func() string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\nin=" + upstreamSecretDir + "\ncd \"$(mktemp -d)\"\nmkdir -p opencode-plugin scripts\n")
	for _, p := range append(slices.Clone(upstreamHeadFiles), upstreamScripts...) {
		fmt.Fprintf(&b, "cp \"$in/%s\" %s\n", path.Base(p), p)
	}
	b.WriteString(`REGISTRY_USER=$(cat "$in/user")
REGISTRY_PASSWORD=$(cat "$in/password")
export REGISTRY_USER REGISTRY_PASSWORD
printf %s "$REGISTRY_PASSWORD" | crane auth login "${UPSTREAM_STORE%%/*}" -u "$REGISTRY_USER" --password-stdin
status=0
bash scripts/upstream-copy 2>&1 | tee -a copy.log || status=1
bash scripts/upstream-files 2>&1 | tee -a copy.log || status=1
sed -n 's/^upstream-\(copy\|files\): /` + upstreamFinding + `&/p' copy.log
exit "$status"
`)
	return b.String()
}()

// UpstreamPod copies what a pushed head adds to the factory's build inputs
// into the store, before the head's checks, in a one-shot pod of its own:
// main's scripts/upstream-copy and scripts/upstream-files over the head's
// lists, go.mod, go.sum and plugin lockfile, in the CI runner image, which
// has crane, go and npm. The pod holds a package token of the store, minted
// for the run and revoked when it ends, and egress to the upstreams and the
// store, so factoryd keeps no egress to the internet and the pod never holds
// the forge's own login; its namespace and the token's Secret die with the
// run.
type UpstreamPod struct {
	Kube Kube
	// Forgejo mints and revokes the run's token; OpenBao holds the site
	// admin's login that does it, which never leaves factoryd.
	Forgejo *Forgejo
	OpenBao SecretReader
	// Namespace is factoryd's own, whose pull secret the pod's namespace
	// copies.
	Namespace  string
	PullSecret string
	// RunnerImage is the CI runner image at the digest factoryd pins
	// (CI_RUNNER_IMAGE).
	RunnerImage string
	// StoreHost is the store's public host (REGISTRY_HOST), which in-cluster
	// DNS answers with the edge gateway.
	StoreHost string
	Scrub     *redact.Scrubber
	// Poll is the wait between reads of the pod's phase; zero is 2 s.
	Poll time.Duration

	sweepMu    sync.Mutex
	tokenSwept time.Time
}

// Copy runs the pod over files, each path of upstreamHeadFiles and
// upstreamScripts with its bytes, and returns the findings: each line a
// script could not copy, as it printed it, and each line whose host the
// pod's egress does not admit. A run that fails returns the pod's exit and
// its last log lines, scrubbed.
func (u *UpstreamPod) Copy(ctx context.Context, files map[string][]byte) ([]string, error) {
	findings := unadmittedLines(files)
	admin, err := adminAuth(ctx, u.OpenBao)
	if err != nil {
		return findings, u.scrubbed(err, "")
	}
	var got []string
	job := podRun{pod: upstreamPodName, container: "copy", program: upstreamProgram, files: files, login: true,
		egress: upstreamEgressSpec(u.StoreHost), env: []any{map[string]any{"name": "UPSTREAM_STORE", "value": u.StoreHost + "/" + upstreamOrg}},
		deadline: upstreamDeadline}
	token, err := u.run(ctx, job, admin, func(ns, phase string, exit int32) error {
		tail, err := u.Kube.PodLogTail(ctx, ns, upstreamPodName, upstreamTailLines)
		if err != nil {
			return fmt.Errorf("read the copy pod's log (phase %s, exit %d): %w", phase, exit, err)
		}
		for _, l := range strings.Split(tail, "\n") {
			if f, ok := strings.CutPrefix(l, upstreamFinding); ok {
				got = append(got, f)
			}
		}
		// a script exits 1 when it could not copy a line, which it then
		// names; a failure that names none is the pod's own
		if phase != "Succeeded" && len(got) == 0 {
			return fmt.Errorf("the upstream copy pod %s with exit %d: %s%s", strings.ToLower(phase), exit,
				strings.Join(lastLines(tail, upstreamReasonLines), " | "), u.state(ctx, ns, upstreamPodName))
		}
		return nil
	})
	if err != nil {
		got = nil
	}
	for _, f := range got {
		findings = append(findings, u.scrub(f, token))
	}
	return findings, u.scrubbed(err, token)
}

// Bump runs the weekly bump's lookup, upstream-bump in the runner image,
// over files, each path of bump.Files with main's bytes, and returns what
// it hands back: the files it changed and the pull request's text. The
// pod holds no login, since the lookup writes nothing to the store, and
// reaches the makers' hosts of bumpFQDNs alone.
func (u *UpstreamPod) Bump(ctx context.Context, files map[string][]byte) (bump.Result, error) {
	var res bump.Result
	job := podRun{pod: bumpPodName, container: "bump", program: bumpProgram, files: files, egress: bumpEgressSpec(),
		env: []any{
			map[string]any{"name": "GOTOOLCHAIN", "value": "local"},
			map[string]any{"name": "GOPROXY", "value": "https://proxy.golang.org"},
		},
		deadline: bumpDeadline}
	_, err := u.run(ctx, job, AdminAuth{}, func(ns, phase string, exit int32) error {
		rc, err := u.Kube.PodLog(ctx, ns, bumpPodName)
		if err != nil {
			return fmt.Errorf("read the bump pod's log (phase %s, exit %d): %w", phase, exit, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, bumpLogBytes))
		if err != nil {
			return fmt.Errorf("read the bump pod's log (phase %s, exit %d): %w", phase, exit, err)
		}
		if phase != "Succeeded" {
			return fmt.Errorf("the bump pod %s with exit %d: %s%s", strings.ToLower(phase), exit,
				strings.Join(lastLines(string(b), upstreamReasonLines), " | "), u.state(ctx, ns, bumpPodName))
		}
		res, err = bump.DecodeResult(string(b))
		return err
	})
	return res, u.scrubbed(err, "")
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

// podRun is one run of a pod in the runner image: its name, program and
// the files it reads, the egress its policy admits, its env and its bound,
// and whether it holds a package token of the store.
type podRun struct {
	pod, container, program string
	files                   map[string][]byte
	login                   bool
	egress                  map[string]any
	env                     []any
	deadline                time.Duration
}

// run runs job in a namespace of its own and hands the ended pod to read,
// before the namespace goes. It returns the token it minted, for the
// caller's scrub.
func (u *UpstreamPod) run(ctx context.Context, job podRun, admin AdminAuth, read func(ns, phase string, exit int32) error) (token string, err error) {
	if !strings.Contains(u.RunnerImage, "@sha256:") {
		return "", fmt.Errorf("the CI runner image %q carries no digest: pin CI_RUNNER_IMAGE as name@sha256:<digest>, which a roll writes", u.RunnerImage)
	}
	u.Sweep(ctx, true)
	suffix := runSuffix()
	id, ns, tokenName := "upstream-"+suffix, upstreamNamespacePrefix+suffix, upstreamTokenPrefix+suffix
	labels := map[string]string{
		"factory.ai/task":                    id,
		UpstreamLabel:                        "true",
		"pod-security.kubernetes.io/enforce": "restricted",
	}
	// the namespace comes before the token: a token with no live namespace is
	// a run's leftover, which the sweep revokes at once
	if err := createFactorydNamespace(ctx, u.Kube, u.Namespace, ns, labels); err != nil {
		return "", err
	}
	minted := false
	// the run's end, or its context's, takes the token and the namespace,
	// each on a context of its own
	defer func() {
		if minted {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if _, err := u.Forgejo.AdminDeleteTokens(rctx, admin, UpstreamUser, tokenName); err != nil {
				log.Printf("upstream copy: revoke token %s: %v", tokenName, err)
			}
			cancel()
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := u.Kube.DeleteNamespace(dctx, ns); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("upstream copy: delete namespace %s: %v", ns, err)
		}
	}()
	data := map[string]string{}
	for p, b := range job.files {
		data[path.Base(p)] = string(b)
	}
	if job.login {
		if err := u.ensureUser(ctx, admin); err != nil {
			return "", err
		}
		token, err = u.Forgejo.AdminMintToken(ctx, admin, UpstreamUser, tokenName, []string{"write:package"}, nil)
		if err != nil {
			return "", fmt.Errorf("mint the copy's package token: %w", err)
		}
		minted = true
		data["user"], data["password"] = UpstreamUser, token
	}
	steps := []ciSetupStep{
		{"egress policy", func() error {
			return u.Kube.CreateCiliumNetworkPolicy(ctx, ns, "upstream-egress", job.egress)
		}},
		// the token reaches the pod as a file of a Secret that dies with its
		// namespace, as the CI runner's registration token does, never in the
		// stored pod spec or an env value; the files ride beside it, some
		// 80 KB of the 1 MiB a Secret holds
		{"login secret", func() error {
			return u.Kube.CreateSecret(ctx, ns, upstreamSecretName, "", data)
		}},
		{"pull secret", func() error {
			return copyPullSecret(ctx, u.Kube, u.Namespace, u.PullSecret, ns)
		}},
	}
	if err := runSetupSteps(ctx, steps); err != nil {
		return token, err
	}
	if err := u.Kube.CreatePod(ctx, ns, job.pod, u.podSpec(job)); err != nil {
		return token, fmt.Errorf("create the %s pod: %w", job.container, err)
	}
	phase, exit, err := u.wait(ctx, ns, job)
	if err != nil {
		return token, err
	}
	return token, read(ns, phase, exit)
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
func (u *UpstreamPod) state(ctx context.Context, ns, pod string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	d, err := u.Kube.PodDiagnosis(ctx, ns, pod)
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
	if e, err := u.Kube.NewestPodEvent(ctx, ns, pod); err == nil && e != "" {
		parts = append(parts, "newest event "+e)
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// startedAt is when the run named by name began, read from its suffix.
func startedAt(name, prefix string) (time.Time, bool) {
	started, _, _ := strings.Cut(strings.TrimPrefix(name, prefix), "-")
	at, err := strconv.ParseInt(started, 10, 64)
	return time.Unix(at, 0), err == nil && strings.HasPrefix(name, prefix)
}

// Sweep deletes the namespaces an earlier run left behind (a failed delete):
// those begun more than the longest run plus 5 minutes ago, another run's
// live one being younger. It revokes each token of the copy's user whose run
// has no live namespace, at once: a restart or a failed revoke leaves one.
// The CI spawner's reconcile pass calls it every tick for the namespaces; the
// tokens are listed at most every 10 minutes, or when force is set (a run's
// start, factoryd's boot). A run's namespace exists before its token does, so
// the tokens are listed before the namespaces.
func (u *UpstreamPod) Sweep(ctx context.Context, force bool) {
	var admin AdminAuth
	var tokens []string
	u.sweepMu.Lock()
	due := (force || time.Since(u.tokenSwept) >= upstreamTokenSweepEvery) && u.Forgejo != nil
	if due {
		u.tokenSwept = time.Now()
	}
	u.sweepMu.Unlock()
	if due {
		var err error
		if admin, err = adminAuth(ctx, u.OpenBao); err == nil {
			tokens, err = u.Forgejo.AdminTokenNames(ctx, admin, UpstreamUser)
		}
		// no copy user yet, before the first run: nothing to revoke
		if err != nil && errors.Is(err, ErrNotFound) {
			tokens, err = nil, nil
		}
		if err != nil {
			log.Printf("upstream copy: list the copy's tokens: %v", err)
			due = false
		}
	}
	infos, err := u.Kube.ListNamespacesByLabel(ctx, UpstreamLabel, "true")
	if err != nil {
		log.Printf("upstream copy: list namespaces: %v", err)
		return
	}
	live := map[string]bool{}
	for _, info := range infos {
		if info.Terminating {
			continue
		}
		if at, ok := startedAt(info.Name, upstreamNamespacePrefix); ok && time.Since(at) < upstreamSweepAge() {
			live[info.Name] = true
			continue
		}
		if err := u.Kube.DeleteNamespace(ctx, info.Name); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("upstream copy: delete the old namespace %s: %v", info.Name, err)
		}
	}
	if !due {
		return
	}
	for _, name := range tokens {
		suffix, ok := strings.CutPrefix(name, upstreamTokenPrefix)
		if !ok || live[upstreamNamespacePrefix+suffix] {
			continue
		}
		if _, err := u.Forgejo.AdminDeleteTokens(ctx, admin, UpstreamUser, name); err != nil {
			log.Printf("upstream copy: revoke the leftover token %s: %v", name, err)
		}
	}
}

// wait returns the pod's final phase and its exit code. It ends at the
// job's bound with the pod's state: a pod that never starts says why.
func (u *UpstreamPod) wait(ctx context.Context, ns string, job podRun) (string, int32, error) {
	poll := u.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	bound := job.deadline + upstreamStartGrace
	runCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	for {
		phase, exit, _, err := u.Kube.PodStatus(runCtx, ns, job.pod)
		if err != nil && ctx.Err() == nil && runCtx.Err() == nil {
			return "", -1, fmt.Errorf("read the %s pod: %w", job.container, err)
		}
		if err == nil && (phase == "Succeeded" || phase == "Failed") {
			return phase, exit, nil
		}
		select {
		case <-ctx.Done():
			return "", -1, fmt.Errorf("waiting for the %s pod: %w%s", job.container, ctx.Err(), u.state(ctx, ns, job.pod))
		case <-runCtx.Done():
			// the run's own end shows as the bound's too: it is told as itself
			if err := ctx.Err(); err != nil {
				return "", -1, fmt.Errorf("waiting for the %s pod: %w%s", job.container, err, u.state(ctx, ns, job.pod))
			}
			return "", -1, fmt.Errorf("the %s pod did not end within %s%s", job.container, bound, u.state(ctx, ns, job.pod))
		case <-time.After(poll):
		}
	}
}

func (u *UpstreamPod) podSpec(job podRun) map[string]any {
	pullSecrets := []any{}
	if u.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": u.PullSecret})
	}
	return requestEmptyDirs(map[string]any{
		"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": job.pod}},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"activeDeadlineSeconds":        int64(job.deadline / time.Second),
			"runtimeClassName":             "gvisor",
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"imagePullSecrets":              pullSecrets,
			"terminationGracePeriodSeconds": 30,
			"dnsConfig":                     map[string]any{"options": []any{map[string]any{"name": "ndots", "value": "1"}}},
			"containers": []any{map[string]any{
				"name":    job.container,
				"image":   u.RunnerImage,
				"command": []any{"bash", "-c", job.program},
				"env":     job.env,
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "250m", "memory": "512Mi"},
					"limits":   map[string]any{"cpu": "2", "memory": "2Gi"},
				},
				"volumeMounts": []any{
					map[string]any{"name": "login", "mountPath": upstreamSecretDir, "readOnly": true},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
					map[string]any{"name": "home", "mountPath": "/home/runner"},
				},
				"securityContext": ciSecurityContext(),
			}},
			"volumes": []any{
				map[string]any{"name": "login", "secret": map[string]any{"secretName": upstreamSecretName}},
				// the work directory: the files a run downloads stay until it
				// ends, beside the two tarballs
				map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "2Gi"}},
				// HOME: crane's login, npm's logs and go's module cache, which
				// starts empty (275 MB for the go.sum of 2026-11-24)
				map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
			},
		},
	})
}

// upstreamEgressSpec admits the copy pod to the upstreams by name on 443, to
// the store's public host, which in-cluster DNS answers with the edge
// gateway (entity ingress, as a build job's runner reaches it), and DNS for
// those names.
func upstreamEgressSpec(storeHost string) map[string]any {
	dns := append(append([]any{}, upstreamFQDNs...), matchNames(storeHost)...)
	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": upstreamPodName},
		},
		"egress": []any{fqdnEgress(upstreamFQDNs), map[string]any{
			"toEntities": []any{"ingress"},
			"toPorts":    []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
		}, dnsEgress(dns)},
	}
}

// unadmittedLines are the findings for the lines of the lists whose host the
// pod's egress does not admit: an image line's registry, docker.io's being
// registry-1.docker.io, and an https line's URL host. A line of neither
// list's shape is the copy scripts' to refuse.
func unadmittedLines(files map[string][]byte) []string {
	var out []string
	check := func(list, ref string) {
		var host string
		if u, err := url.Parse(ref); err == nil && u.Scheme == "https" {
			host = u.Hostname()
		} else if host, _, _ = strings.Cut(ref, "/"); host == "docker.io" {
			host = "registry-1.docker.io"
		}
		if !upstreamAdmits(host) {
			out = append(out, fmt.Sprintf("%s: %s names %s, outside the copy's egress: a commit adds the host to upstreamFQDNs in internal/factoryd/upstream_pod.go", list, ref, host))
		}
	}
	images, _ := pins.ParseImages(string(files[upstreamImagesPath]))
	for _, l := range images {
		check(upstreamImagesPath, l.Ref)
	}
	downloads, _ := pins.ParseFiles(string(files[upstreamFilesPath]))
	for _, l := range downloads {
		check(upstreamFilesPath, l.URL)
	}
	return out
}

// upstreamAdmits says whether an entry of upstreamFQDNs admits host: a
// matchPattern's `*` stands inside one DNS label, as Cilium's does.
func upstreamAdmits(host string) bool {
	for _, e := range upstreamFQDNs {
		m := e.(map[string]any)
		if m["matchName"] == host {
			return true
		}
		if p, ok := m["matchPattern"].(string); ok && strings.Count(p, ".") == strings.Count(host, ".") {
			if ok, _ := path.Match(p, host); ok {
				return true
			}
		}
	}
	return false
}
