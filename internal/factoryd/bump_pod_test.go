package factoryd

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/bump"
)

// bumpFiles are main's files of a bump, each by its path.
func bumpFiles() map[string][]byte {
	files := map[string][]byte{}
	for _, p := range bump.Files {
		files[p] = []byte("main's " + p + "\n")
	}
	return files
}

// bumpLog is what a bump pod prints: go's lines, then the result.
func bumpLog(t *testing.T, r bump.Result) string {
	t.Helper()
	enc, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return "go: downloading golang.org/x/vuln v1.10.0\n" + r.Text + enc
}

// The bump pod runs upstream-bump in the runner image over main's files,
// with no login to the store: its Secret holds the files alone, no token is
// minted, and its egress reaches the makers' hosts and nothing else.
func TestBumpPodRunsUpstreamBumpOverMainsFilesWithoutALogin(t *testing.T) {
	u := newUpstreamPod(t)
	k := podKube(u)
	want := bump.Result{Files: map[string]string{"go.mod": "module m\n\ntoolchain go1.26.11\n"}, Text: "Moved:\n- node v24.22.2 → v24.22.3\n"}
	var spec, policy map[string]any
	k.run = func(secret map[string]string) (string, int32) {
		for ns := range k.namespaces {
			spec, policy = k.pods[ns+"/"+bumpPodName].spec, k.cnps[ns+"/upstream-egress"]
			if !strings.HasPrefix(ns, upstreamNamespacePrefix) || k.namespaces[ns][UpstreamLabel] != "true" {
				t.Errorf("namespace %s %v: not one the sweep takes", ns, k.namespaces[ns])
			}
		}
		wantSecret := map[string]string{}
		for p, b := range bumpFiles() {
			wantSecret[path.Base(p)] = string(b)
		}
		if !maps.Equal(secret, wantSecret) {
			t.Errorf("the pod's Secret = %v, want main's files alone %v", secret, wantSecret)
		}
		return bumpLog(t, want), 0
	}
	got, err := u.Bump(t.Context(), bumpFiles())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Bump = %+v, %v; want %+v", got, err, want)
	}
	if n := len(tokens(t, u).minted); n != 0 {
		t.Errorf("%d tokens minted for a lookup that writes nothing to the store", n)
	}
	pod := spec["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["image"] != runnerImage || !reflect.DeepEqual(c["command"], []any{"bash", "-c", bumpProgram}) || c["name"] != "bump" {
		t.Errorf("container %v %v %v", c["name"], c["image"], c["command"])
	}
	if env, _ := json.Marshal(c["env"]); string(env) != `[{"name":"GOTOOLCHAIN","value":"local"},{"name":"GOPROXY","value":"https://proxy.golang.org"}]` {
		t.Errorf("env %s: want the local Go and the module proxy alone", env)
	}
	if pod["runtimeClassName"] != "gvisor" || pod["automountServiceAccountToken"] != false || pod["activeDeadlineSeconds"] != int64(bumpDeadline.Seconds()) {
		t.Errorf("pod %v", pod)
	}
	if !reflect.DeepEqual(policy, bumpEgressSpec()) {
		t.Errorf("the pod's policy %v", policy)
	}
	raw, _ := json.Marshal(policy)
	for _, no := range []string{"ingress", storeHost, `"port":"3000"`} {
		if strings.Contains(string(raw), no) {
			t.Errorf("the bump pod reaches %s: %s", no, raw)
		}
	}
}

// A bump pod that fails says how, with its last lines.
func TestBumpPodThatFailsSaysWhy(t *testing.T) {
	u := newUpstreamPod(t)
	podKube(u).run = func(map[string]string) (string, int32) {
		return "bash: line 13: upstream-bump: command not found\n", 127
	}
	_, err := u.Bump(t.Context(), bumpFiles())
	if err == nil || !strings.HasPrefix(err.Error(), "the bump pod failed with exit 127: bash: line 13: upstream-bump: command not found") {
		t.Errorf("err = %v", err)
	}
	podKube(u).run = func(map[string]string) (string, int32) { return "go: downloading x\n", 0 }
	if _, err := u.Bump(t.Context(), bumpFiles()); err == nil || err.Error() != "the log holds no result between its marker lines" {
		t.Errorf("a pod that printed no result: %v", err)
	}
}

// The program puts each file at its repository path and runs upstream-bump
// there, whose last lines are the result.
func TestBumpPodProgramRunsUpstreamBumpOverTheFilesInPlace(t *testing.T) {
	in, bin := t.TempDir(), t.TempDir()
	for p, b := range bumpFiles() {
		if err := os.WriteFile(filepath.Join(in, path.Base(p)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\nfor f in " + strings.Join(bump.Files, " ") + "; do grep -qx \"main's $f\" \"$f\" || { echo \"$f is not in place\"; exit 3; }; done\necho \"upstream-bump $*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "upstream-bump"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", strings.ReplaceAll(bumpProgram, upstreamSecretDir, in))
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "upstream-bump -result\n" {
		t.Errorf("the program: %v: %s", err, out)
	}
}

// The bump pod's egress admits every host the lookup reached in its dry run
// against the real hosts on 2026-11-24, redirects among them, and those go
// and npm ask.
func TestBumpPodEgressAdmitsTheHostsTheLookupReaches(t *testing.T) {
	for _, host := range []string{
		"registry-1.docker.io", "auth.docker.io", "ghcr.io", "quay.io", "registry.k8s.io", "europe-west3-docker.pkg.dev",
		"code.forgejo.org", "factory.talos.dev", "api.github.com", "github.com", "release-assets.githubusercontent.com",
		"nodejs.org", "go.dev", "dl.google.com", "vuln.go.dev", "apt.postgresql.org",
		"proxy.golang.org", "sum.golang.org", "registry.npmjs.org",
	} {
		if !admits(bumpFQDNs, host) {
			t.Errorf("the bump pod's egress does not admit %s", host)
		}
	}
}
