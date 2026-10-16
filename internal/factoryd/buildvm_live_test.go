package factoryd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
)

// Live check of a build VM in the Hetzner CI project: provision, wait until
// buildctl answers over mTLS from this machine, release, collect, and prove
// the project is empty. Skipped unless FACTORYD_LIVE_HETZNER_CI_KEY,
// FACTORYD_LIVE_BUILD_SOURCE (this machine's public IPv4),
// FACTORYD_LIVE_BUILDCTL (a buildctl binary) and
// FACTORYD_LIVE_STORE_LOGIN (the store's "user:token" read:package
// login, without which the setup script's tarball download fails) are
// set.
func TestLiveBuildVM(t *testing.T) {
	env := map[string]string{}
	for _, k := range []string{"FACTORYD_LIVE_HETZNER_CI_KEY", "FACTORYD_LIVE_BUILD_SOURCE", "FACTORYD_LIVE_BUILDCTL", "FACTORYD_LIVE_STORE_LOGIN"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			t.Skip(k + " not set")
		}
	}
	ctx := context.Background()
	hc := hcloud.New(env["FACTORYD_LIVE_HETZNER_CI_KEY"])
	reg := cloudres.NewMemory()
	col := &cloudres.Collector{Project: "ci", Registry: reg, Provider: hc, NodeIPs: []string{env["FACTORYD_LIVE_BUILD_SOURCE"] + "/32"}}
	b, err := NewBuildVMs(BuildVMConfig{AllowedSources: []string{env["FACTORYD_LIVE_BUILD_SOURCE"]},
		StoreLogin: env["FACTORYD_LIVE_STORE_LOGIN"], Lease: time.Hour}, hc, reg, col)
	if err != nil {
		t.Fatal(err)
	}
	const job = 999001
	t.Cleanup(func() {
		if err := b.Release(ctx, job); err != nil {
			t.Errorf("release: %v", err)
		}
	})
	start := time.Now()
	vm, err := b.Provision(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provisioned %s in %s", vm.IPv4, time.Since(start).Round(time.Second))

	dir := t.TempDir()
	for name, data := range buildTLSSecretData(vm.PKI) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		cmd := exec.Command(env["FACTORYD_LIVE_BUILDCTL"], "--addr", "tcp://"+vm.IPv4+":1234",
			"--tlsdir", dir, "--tlsservername", buildVMServerName, "debug", "workers") // #nosec G204 -- test-only binary path from the operator
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Logf("buildkitd answered after %s:\n%s", time.Since(start).Round(time.Second), out)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("buildkitd never answered: %v: %s", err, out)
		}
		time.Sleep(3 * time.Second)
	}

	if err := b.Release(ctx, job); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if err := col.Collect(ctx); err != nil {
			t.Logf("collect: %v", err)
		}
		servers, err1 := hc.Servers(ctx, "")
		firewalls, err2 := hc.Firewalls(ctx, "")
		if err1 == nil && err2 == nil && len(servers) == 0 && len(firewalls) == 0 {
			break
		}
		if i == 20 {
			t.Fatalf("project not empty: %d servers, %d firewalls (%v %v)", len(servers), len(firewalls), err1, err2)
		}
		time.Sleep(3 * time.Second)
	}
	if rows, _ := reg.Open(ctx, "ci"); len(rows) != 0 {
		t.Fatalf("open rows: %+v", rows)
	}
	t.Log("project empty: 0 servers, 0 firewalls")
}
