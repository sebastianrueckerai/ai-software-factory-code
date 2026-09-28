package factoryd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
)

func TestBuildPKIMutualTLS(t *testing.T) {
	p, err := newBuildPKI(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(p.CACert) {
		t.Fatal("CA does not parse")
	}
	serverPair, err := tls.X509KeyPair(p.ServerCert, p.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPair, err := tls.X509KeyPair(p.ClientCert, p.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{serverPair}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte("ok")) // completes the handshake, or fails it
			}()
		}
	}()
	dial := func(cfg *tls.Config) error {
		cfg.RootCAs, cfg.MinVersion = pool, tls.VersionTLS12
		c, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = io.ReadAll(c)
		return err
	}
	if err := dial(&tls.Config{ServerName: buildVMServerName, Certificates: []tls.Certificate{clientPair}}); err != nil {
		t.Fatalf("mTLS with the client pair: %v", err)
	}
	if err := dial(&tls.Config{ServerName: buildVMServerName}); err == nil {
		t.Fatal("a client without a certificate got through")
	}
	if err := dial(&tls.Config{ServerName: "other", Certificates: []tls.Certificate{clientPair}}); err == nil {
		t.Fatal("the server cert verified for a name it does not carry")
	}
	// the server cert must not work as a client cert
	if err := dial(&tls.Config{ServerName: buildVMServerName, Certificates: []tls.Certificate{serverPair}}); err == nil {
		t.Fatal("the server pair was accepted as a client")
	}
}

func TestBuildVMUserData(t *testing.T) {
	p, err := newBuildPKI(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ud, err := buildVMUserData(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"echo \"" + buildkitSHA256 + "  buildkit.tar.gz\" | sha256sum -c -",
		"https://github.com/moby/buildkit/releases/download/v0.33.0/buildkit-v0.33.0.linux-amd64.tar.gz",
		"--addr tcp://0.0.0.0:1234 --tlscacert /etc/buildkit/ca.pem --tlscert /etc/buildkit/server-cert.pem --tlskey /etc/buildkit/server-key.pem --oci-worker-snapshotter=overlayfs",
		"bin/buildkit-runc",
		strings.TrimSpace(string(p.CACert)), strings.TrimSpace(string(p.ServerCert)), strings.TrimSpace(string(p.ServerKey)),
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data lacks %q", want)
		}
	}
	if strings.Contains(ud, strings.TrimSpace(string(p.ClientKey))) || strings.Contains(ud, strings.TrimSpace(string(p.ClientCert))) {
		t.Error("user data carries the client pair")
	}
	if n := strings.Count(ud, "-----BEGIN"); n != 3 || strings.Count(ud, "-----BEGIN EC PRIVATE KEY") != 1 {
		t.Errorf("user data holds %d private keys, want only the server key", n)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(ud)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v: %s", err, out)
		}
	}
}

func TestBuildVMFirewallAndEgressShapes(t *testing.T) {
	got := buildVMFirewallRules([]string{"198.51.100.1/32", "198.51.100.2/32"})
	want := []hcloud.FirewallRule{
		{Direction: "in", Protocol: "tcp", Port: "1234", SourceIPs: []string{"198.51.100.1/32", "198.51.100.2/32"}},
		{Direction: "out", Protocol: "tcp", Port: "80", DestinationIPs: []string{"0.0.0.0/0"}},
		{Direction: "out", Protocol: "tcp", Port: "443", DestinationIPs: []string{"0.0.0.0/0"}},
		{Direction: "out", Protocol: "udp", Port: "53", DestinationIPs: []string{"0.0.0.0/0"}},
		{Direction: "out", Protocol: "tcp", Port: "53", DestinationIPs: []string{"0.0.0.0/0"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules = %+v", got)
	}
	rule, _ := json.Marshal(buildVMEgressRule("203.0.113.7"))
	if string(rule) != `{"toCIDR":["203.0.113.7/32"],"toPorts":[{"ports":[{"port":"1234","protocol":"TCP"}]}]}` {
		t.Fatalf("egress rule = %s", rule)
	}
	env, _ := json.Marshal(buildVMEnv("203.0.113.7"))
	if string(env) != `[{"name":"BUILDKIT_HOST","value":"tcp://203.0.113.7:1234"},{"name":"BUILDKIT_TLS_DIR","value":"/var/run/secrets/buildkit"},{"name":"BUILDKIT_TLS_SERVERNAME","value":"buildkitd"}]` {
		t.Fatalf("env = %s", env)
	}
	p := buildPKI{CACert: []byte("ca"), ClientCert: []byte("cert"), ClientKey: []byte("key"), ServerKey: []byte("skey")}
	if data := buildTLSSecretData(p); !reflect.DeepEqual(data, map[string]string{"ca.pem": "ca", "cert.pem": "cert", "key.pem": "key"}) {
		t.Fatalf("secret data = %v", data)
	}
}

// fakeHetzner is an in-memory CI project.
type fakeHetzner struct {
	mu            sync.Mutex
	nextID        int64
	servers       map[int64]hcloud.Server
	firewalls     map[int64]hcloud.Firewall
	serverReqs    []hcloud.CreateServerRequest
	firewallReqs  []hcloud.CreateFirewallRequest
	calls         []string
	serverErr     error
	fwInUseTimes  int // DeleteFirewall answers resource_in_use this many times
	pricingCalled int
	pricingErr    error
}

func newFakeHetzner() *fakeHetzner {
	return &fakeHetzner{nextID: 100, servers: map[int64]hcloud.Server{}, firewalls: map[int64]hcloud.Firewall{}}
}

func (f *fakeHetzner) record(c string) { f.calls = append(f.calls, c) }

func (f *fakeHetzner) Pricing(ctx context.Context) (hcloud.Pricing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pricingCalled++
	if f.pricingErr != nil {
		return hcloud.Pricing{}, f.pricingErr
	}
	var p hcloud.Pricing
	err := json.Unmarshal([]byte(`{"server_types":[{"name":"cx23","prices":[{"location":"fsn1","price_hourly":{"net":"0.0088"}}]}],
		"primary_ips":[{"type":"ipv4","prices":[{"location":"fsn1","price_hourly":{"net":"0.0008"}}]}]}`), &p)
	return p, err
}

func (f *fakeHetzner) CreateFirewall(ctx context.Context, r hcloud.CreateFirewallRequest) (hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.firewallReqs = append(f.firewallReqs, r)
	fw := hcloud.Firewall{ID: f.nextID, Name: r.Name, Labels: r.Labels}
	f.firewalls[fw.ID] = fw
	f.record("create firewall/" + strconv.FormatInt(fw.ID, 10))
	return fw, nil
}

func (f *fakeHetzner) CreateServer(ctx context.Context, r hcloud.CreateServerRequest) (hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverReqs = append(f.serverReqs, r)
	if f.serverErr != nil {
		return hcloud.Server{}, f.serverErr
	}
	f.nextID++
	var s hcloud.Server
	if err := json.Unmarshal([]byte(`{"public_net":{"ipv4":{"ip":"203.0.113.`+strconv.FormatInt(f.nextID%250, 10)+`"}}}`), &s); err != nil {
		return s, err
	}
	s.ID, s.Name, s.Labels = f.nextID, r.Name, r.Labels
	f.servers[s.ID] = s
	f.record("create server/" + strconv.FormatInt(s.ID, 10))
	return s, nil
}

func matchSelector(labels map[string]string, sel string) bool {
	if sel == "" {
		return true
	}
	k, v, _ := strings.Cut(sel, "=")
	return labels[k] == v
}

func (f *fakeHetzner) Servers(ctx context.Context, sel string) ([]hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Server
	for _, s := range f.servers {
		if matchSelector(s.Labels, sel) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeHetzner) Server(ctx context.Context, id int64) (hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return hcloud.Server{}, &hcloud.APIError{Status: 404}
}

func (f *fakeHetzner) DeleteServer(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.servers, id)
	f.record("delete server/" + strconv.FormatInt(id, 10))
	return nil
}

func (f *fakeHetzner) Firewalls(ctx context.Context, sel string) ([]hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Firewall
	for _, fw := range f.firewalls {
		if matchSelector(fw.Labels, sel) {
			out = append(out, fw)
		}
	}
	return out, nil
}

func (f *fakeHetzner) Firewall(ctx context.Context, id int64) (hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fw, ok := f.firewalls[id]; ok {
		return fw, nil
	}
	return hcloud.Firewall{}, &hcloud.APIError{Status: 404}
}

func (f *fakeHetzner) DeleteFirewall(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fwInUseTimes > 0 {
		f.fwInUseTimes--
		f.record("in use firewall/" + strconv.FormatInt(id, 10))
		return &hcloud.APIError{Status: 409, Code: "resource_in_use"}
	}
	delete(f.firewalls, id)
	f.record("delete firewall/" + strconv.FormatInt(id, 10))
	return nil
}

// The CI project holds no volumes or loose addresses in these tests.
func (f *fakeHetzner) Volumes(ctx context.Context, sel string) ([]hcloud.Volume, error) {
	return nil, nil
}
func (f *fakeHetzner) Volume(ctx context.Context, id int64) (hcloud.Volume, error) {
	return hcloud.Volume{}, &hcloud.APIError{Status: 404}
}
func (f *fakeHetzner) DetachVolume(ctx context.Context, id int64) error { return nil }
func (f *fakeHetzner) DeleteVolume(ctx context.Context, id int64) error { return nil }
func (f *fakeHetzner) PrimaryIPs(ctx context.Context, sel string) ([]hcloud.PrimaryIP, error) {
	return nil, nil
}
func (f *fakeHetzner) DeletePrimaryIP(ctx context.Context, id int64) error { return nil }

var buildVMNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newTestBuildVMs(t *testing.T, max int) (*BuildVMs, *fakeHetzner, *cloudres.Memory) {
	t.Helper()
	hz, reg := newFakeHetzner(), cloudres.NewMemory()
	col := &cloudres.Collector{Project: "ci", Registry: reg, Provider: hz, RetryWait: time.Nanosecond, NodeIPs: []string{"198.51.100.1/32"}}
	b, err := NewBuildVMs(BuildVMConfig{Max: max, AllowedSources: []string{"198.51.100.1", "198.51.100.2/32"}, Lease: time.Hour}, hz, reg, col)
	if err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return buildVMNow }
	return b, hz, reg
}

func TestNewBuildVMsValidatesSources(t *testing.T) {
	for _, src := range []string{"", "not-an-ip", "2001:db8::1"} {
		srcs := []string{src}
		if src == "" {
			srcs = nil
		}
		if _, err := NewBuildVMs(BuildVMConfig{AllowedSources: srcs, Lease: time.Hour}, nil, nil, nil); err == nil {
			t.Errorf("sources %q accepted", srcs)
		}
	}
	if _, err := NewBuildVMs(BuildVMConfig{AllowedSources: []string{"198.51.100.1"}}, nil, nil, nil); err == nil {
		t.Error("a zero lease was accepted")
	}
	// CI_BUILD_VM_SOURCES is a comma-separated list, spaces allowed
	b, err := NewBuildVMs(BuildVMConfig{AllowedSources: strings.Split("198.51.100.1, 198.51.100.2/32", ","), Lease: time.Hour}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"198.51.100.1/32", "198.51.100.2/32"}; !reflect.DeepEqual(b.cfg.AllowedSources, want) {
		t.Fatalf("sources = %q, want %q", b.cfg.AllowedSources, want)
	}
}

func TestProvisionRegistersBeforeCreating(t *testing.T) {
	b, hz, reg := newTestBuildVMs(t, 2)
	vm, err := b.Provision(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if vm.IPv4 != "203.0.113.102" || len(vm.PKI.ClientKey) == 0 {
		t.Fatalf("vm = %+v", vm)
	}
	if strings.Join(hz.calls, ",") != "create firewall/101,create server/102" {
		t.Fatalf("calls = %v", hz.calls)
	}
	rows, _ := reg.Open(context.Background(), "ci")
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	fw, srv := rows[0], rows[1]
	wantLease := buildVMNow.Add(time.Hour)
	if fw.Kind != cloudres.KindFirewall || fw.State != cloudres.StateLive || fw.ProviderID != "101" || fw.OwnerRef != "77" ||
		fw.Name != "ci-build-77-firewall-1790596800" || !fw.LeaseExpires.Equal(wantLease) || fw.PricePerHourEUR != 0 {
		t.Fatalf("firewall row = %+v", fw)
	}
	if srv.Kind != cloudres.KindServer || srv.State != cloudres.StateLive || srv.ProviderID != "102" || srv.OwnerRef != "77" ||
		srv.Name != "ci-build-77-server-1790596800" || !srv.LeaseExpires.Equal(wantLease) || math.Abs(srv.PricePerHourEUR-0.0096) > 1e-12 {
		t.Fatalf("server row = %+v", srv)
	}
	if want := map[string]string{"factory": "ci", "job": "77", "resource": "1"}; !reflect.DeepEqual(hz.firewallReqs[0].Labels, want) {
		t.Fatalf("firewall labels = %v", hz.firewallReqs[0].Labels)
	}
	if !reflect.DeepEqual(hz.firewallReqs[0].Rules, buildVMFirewallRules([]string{"198.51.100.1/32", "198.51.100.2/32"})) {
		t.Fatalf("firewall rules = %+v", hz.firewallReqs[0].Rules)
	}
	req := hz.serverReqs[0]
	if want := map[string]string{"factory": "ci", "job": "77", "resource": "2"}; !reflect.DeepEqual(req.Labels, want) {
		t.Fatalf("server labels = %v", req.Labels)
	}
	if req.ServerType != "cx23" || req.Location != "fsn1" || req.Image != "debian-12" || req.Name != "ci-build-77-server-1790596800" ||
		!reflect.DeepEqual(req.Firewalls, []hcloud.FirewallRef{{Firewall: 101}}) ||
		*req.PublicNet != (hcloud.PublicNet{EnableIPv4: true}) ||
		!strings.Contains(req.UserData, strings.TrimSpace(string(vm.PKI.ServerCert))) {
		t.Fatalf("server request = %+v", req)
	}
}

func TestProvisionCapsLiveVMs(t *testing.T) {
	b, hz, _ := newTestBuildVMs(t, 1)
	if _, err := b.Provision(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if free, _ := b.FreeSlot(context.Background()); free {
		t.Fatal("slot free at the cap")
	}
	if _, err := b.Provision(context.Background(), 2); !errors.Is(err, ErrNoBuildVMSlot) {
		t.Fatalf("err = %v, want ErrNoBuildVMSlot", err)
	}
	if len(hz.calls) != 2 || hz.pricingCalled != 1 {
		t.Fatalf("the capped provision called the API: %v", hz.calls)
	}
	if err := b.Release(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if free, _ := b.FreeSlot(context.Background()); !free {
		t.Fatal("no slot after release")
	}
}

func TestProvisionQuotaErrorReleasesFirewall(t *testing.T) {
	b, hz, reg := newTestBuildVMs(t, 2)
	hz.serverErr = &hcloud.APIError{Status: 403, Code: "resource_limit_exceeded", Message: "Primary IP limit exceeded"}
	if _, err := b.Provision(context.Background(), 5); !errors.Is(err, ErrNoBuildVMSlot) {
		t.Fatalf("err = %v, want ErrNoBuildVMSlot", err)
	}
	if strings.Join(hz.calls, ",") != "create firewall/101,delete firewall/101" {
		t.Fatalf("calls = %v", hz.calls)
	}
	if rows, _ := reg.Open(context.Background(), "ci"); len(rows) != 0 {
		t.Fatalf("open rows = %+v", rows)
	}
	for id := int64(1); id <= 2; id++ {
		if r, _ := reg.Get(id); r.State != cloudres.StateDeleted || r.DeletedAt == nil {
			t.Fatalf("row %d = %+v", id, r)
		}
	}
}

func TestReleaseDeletesServerFirstAndRetriesFirewall(t *testing.T) {
	b, hz, reg := newTestBuildVMs(t, 2)
	if _, err := b.Provision(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	other, err := b.Provision(context.Background(), 6)
	if err != nil {
		t.Fatal(err)
	}
	hz.calls, hz.fwInUseTimes = nil, 2
	if err := b.Release(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if strings.Join(hz.calls, ",") != "delete server/102,in use firewall/101,in use firewall/101,delete firewall/101" {
		t.Fatalf("calls = %v", hz.calls)
	}
	rows, _ := reg.Open(context.Background(), "ci")
	if len(rows) != 2 || rows[0].OwnerRef != "6" || rows[1].OwnerRef != "6" || other.IPv4 == "" {
		t.Fatalf("open rows = %+v", rows)
	}
	if err := b.Release(context.Background(), 5); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestReleaseFindsUnrecordedResourceByLabel(t *testing.T) {
	b, hz, reg := newTestBuildVMs(t, 2)
	ctx := context.Background()
	row, _ := reg.Insert(ctx, cloudres.Resource{Project: "ci", Kind: cloudres.KindServer, OwnerRef: "9", LeaseExpires: buildVMNow})
	hz.servers[500] = hcloud.Server{ID: 500, Labels: row.Labels} // created; factoryd died before recording the id
	never, _ := reg.Insert(ctx, cloudres.Resource{Project: "ci", Kind: cloudres.KindFirewall, OwnerRef: "9", LeaseExpires: buildVMNow})
	if err := b.Release(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if strings.Join(hz.calls, ",") != "delete server/500" {
		t.Fatalf("calls = %v", hz.calls)
	}
	for _, id := range []int64{row.ID, never.ID} {
		if r, _ := reg.Get(id); r.State != cloudres.StateDeleted {
			t.Fatalf("row %d = %s", id, r.State)
		}
	}
}

func TestCIOwnerDone(t *testing.T) {
	kube := newFakeKube()
	kube.namespaces["factory-agents-ci-7"] = map[string]string{"factory.ai/task": "ci-7"}
	done := CIOwnerDone(kube)
	if d, err := done(context.Background(), "7"); err != nil || d {
		t.Fatalf("job 7 = %v, %v, want live", d, err)
	}
	if d, err := done(context.Background(), "8"); err != nil || !d {
		t.Fatalf("job 8 = %v, %v, want done", d, err)
	}
	if _, err := done(context.Background(), "x"); err == nil {
		t.Fatal("want an error for a non-numeric owner")
	}
}

// The price catalogue is read once an hour; a failed refresh keeps the last.
func TestProvisionCachesPricing(t *testing.T) {
	b, hz, _ := newTestBuildVMs(t, 1)
	ctx := context.Background()
	provisionAndRelease := func(job int64) {
		t.Helper()
		if _, err := b.Provision(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := b.Release(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	provisionAndRelease(1)
	provisionAndRelease(2)
	if hz.pricingCalled != 1 {
		t.Fatalf("pricing read %d times within the hour, want 1", hz.pricingCalled)
	}
	b.now = func() time.Time { return buildVMNow.Add(time.Hour + time.Second) }
	hz.pricingErr = errors.New("hetzner down")
	provisionAndRelease(3)
	if hz.pricingCalled != 2 {
		t.Fatalf("pricing read %d times, want a refresh after the hour", hz.pricingCalled)
	}
}

// While the provider fails, no build job is claimed: FreeSlot is false for
// a minute after a failed provision, then open again.
func TestProvisionFailureBacksOffClaims(t *testing.T) {
	b, hz, _ := newTestBuildVMs(t, 2)
	ctx := context.Background()
	hz.pricingErr = errors.New("hetzner down")
	if _, err := b.Provision(ctx, 1); err == nil {
		t.Fatal("provision succeeded without a price catalogue")
	}
	if free, _ := b.FreeSlot(ctx); free {
		t.Fatal("slot free right after the provider failed")
	}
	b.now = func() time.Time { return buildVMNow.Add(time.Minute) }
	if free, _ := b.FreeSlot(ctx); !free {
		t.Fatal("still backing off a minute later")
	}
}

// A closed build VM row yields its lifetime, its ready time and its cost:
// started hours times the row's hourly price (a cx23 with its IPv4,
// 0.0088 + 0.0008 = 0.0096 EUR an hour).
func TestBuildVMDeletionEmitsLifetimeReadyAndCost(t *testing.T) {
	b, _, reg := newTestBuildVMs(t, 2)
	m, reader := testMetrics(t)
	b.metrics = m
	b.collector.Deleted = b.Deleted
	reg.Now = func() time.Time { return buildVMNow }
	ctx := context.Background()

	if _, err := b.Provision(ctx, 50); err != nil {
		t.Fatal(err)
	}
	// buildkitd answered 45 s after create; the job's own teardown 7
	// minutes after create: one started hour
	if err := b.SetReady(ctx, 50, buildVMNow.Add(45*time.Second)); err != nil {
		t.Fatal(err)
	}
	b.collector.Now = func() time.Time { return buildVMNow.Add(7 * time.Minute) }
	if err := b.Release(ctx, 50); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Provision(ctx, 51); err != nil {
		t.Fatal(err)
	}
	// the job died with its VM before buildkitd answered: the collector
	// deletes it at its lease, 1 h 1 s after create, two started hours
	b.collector.Now = func() time.Time { return buildVMNow.Add(time.Hour + time.Second) }
	if err := b.collector.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	// a ready time for a job whose rows are closed is not kept
	if err := b.SetReady(ctx, 50, buildVMNow); err != nil {
		t.Fatal(err)
	}
	if len(b.ready) != 0 {
		t.Fatalf("ready entries left: %v", b.ready)
	}

	lifetimes := histograms(t, reader, "factory.ci.buildvm.lifetime")
	if !reflect.DeepEqual(lifetimes, map[string]histPoint{
		"deleted_by=teardown,server_type=cx23":  {1, 420},
		"deleted_by=collector,server_type=cx23": {1, 3601},
	}) {
		t.Fatalf("lifetimes %v", lifetimes)
	}
	ready := histograms(t, reader, "factory.ci.buildvm.ready")
	if !reflect.DeepEqual(ready, map[string]histPoint{"deleted_by=teardown,server_type=cx23": {1, 45}}) {
		t.Fatalf("ready %v", ready)
	}
	costs := sums(t, reader, "factory.ci.buildvm.cost")
	if len(costs) != 2 || math.Abs(costs["deleted_by=teardown,server_type=cx23"]-0.0096) > 1e-12 ||
		math.Abs(costs["deleted_by=collector,server_type=cx23"]-0.0192) > 1e-12 {
		t.Fatalf("costs %v", costs)
	}
}
