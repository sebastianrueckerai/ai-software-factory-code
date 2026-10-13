package factoryd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// Build VMs (ruling 110): one throwaway Hetzner server per
// pr-untrusted-build job, in the CI project, running buildkitd behind mTLS.
const (
	buildVMPort = 1234
	// buildVMServerName is the server certificate's only SAN: the VM's IP
	// is unknown when the certificate is minted, so clients pass it as
	// --tlsservername.
	buildVMServerName = "buildkitd"
	buildkitVersion   = "v0.33.0"
	buildkitSHA256    = "b6242896d343100808dcbe37565caf381e0a444a6a83d7255926bb1519248ead"

	// buildTLSSecret holds the CA and client pair in the job's namespace,
	// mounted at buildTLSDir under buildctl's --tlsdir file names.
	buildTLSSecret = "buildkit-client"
	buildTLSDir    = "/var/run/secrets/buildkit"
)

// ErrNoBuildVMSlot: the VM cap or the project's IP quota is reached; the
// job stays queued in Forgejo.
var ErrNoBuildVMSlot = errors.New("build vm: no free slot")

// buildPKI is one VM's ephemeral mTLS material. The CA key signs the two
// leaves and is dropped: nothing can mint another certificate for this VM.
type buildPKI struct {
	CACert, ServerCert, ServerKey, ClientCert, ClientKey []byte
}

func newBuildPKI(now time.Time, validFor time.Duration) (buildPKI, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return buildPKI{}, err
	}
	notBefore, notAfter := now.Add(-5*time.Minute), now.Add(validFor)
	caTmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "factory ci build vm ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, caCert, err := signCert(caTmpl, nil, &caKey.PublicKey, caKey)
	if err != nil {
		return buildPKI{}, err
	}
	leaf := func(cn string, usage x509.ExtKeyUsage, dns []string) ([]byte, []byte, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, _, err := signCert(&x509.Certificate{
			Subject:     pkix.Name{CommonName: cn},
			DNSNames:    dns,
			NotBefore:   notBefore,
			NotAfter:    notAfter,
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}, caCert, &key.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		return pemBlock("CERTIFICATE", der), pemBlock("EC PRIVATE KEY", keyDER), nil
	}
	p := buildPKI{CACert: pemBlock("CERTIFICATE", caDER)}
	if p.ServerCert, p.ServerKey, err = leaf(buildVMServerName, x509.ExtKeyUsageServerAuth, []string{buildVMServerName}); err != nil {
		return buildPKI{}, err
	}
	if p.ClientCert, p.ClientKey, err = leaf("ci-runner", x509.ExtKeyUsageClientAuth, nil); err != nil {
		return buildPKI{}, err
	}
	return p, nil
}

// signCert signs tmpl with a random serial; a nil parent self-signs.
func signCert(tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) ([]byte, *x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = serial
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return der, cert, err
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// buildVMUserData is the VM's cloud-init script: the CA (to verify
// clients), the server pair, the checksum-pinned buildkitd release with its
// own runc, and a systemd unit serving mTLS on buildVMPort. It carries no
// secret beyond the VM's own server key.
func buildVMUserData(p buildPKI) (string, error) {
	var b bytes.Buffer
	err := userDataTmpl.Execute(&b, map[string]any{
		"CA": strings.TrimSpace(string(p.CACert)), "Cert": strings.TrimSpace(string(p.ServerCert)),
		"Key": strings.TrimSpace(string(p.ServerKey)), "Version": buildkitVersion, "SHA256": buildkitSHA256,
		"Port": buildVMPort,
	})
	return b.String(), err
}

var userDataTmpl = template.Must(template.New("user-data").Parse(`#!/bin/bash
set -euo pipefail
exec > /var/log/build-vm-setup.log 2>&1
install -d -m 700 /etc/buildkit
cat > /etc/buildkit/ca.pem <<'EOF'
{{.CA}}
EOF
cat > /etc/buildkit/server-cert.pem <<'EOF'
{{.Cert}}
EOF
(umask 077; cat > /etc/buildkit/server-key.pem <<'EOF'
{{.Key}}
EOF
)
cd /root
curl -fsSL --retry 5 -o buildkit.tar.gz https://github.com/moby/buildkit/releases/download/{{.Version}}/buildkit-{{.Version}}.linux-amd64.tar.gz
echo "{{.SHA256}}  buildkit.tar.gz" | sha256sum -c -
tar -C /usr/local -xzf buildkit.tar.gz bin/buildkitd bin/buildctl bin/buildkit-runc
ln -sf /usr/local/bin/buildkit-runc /usr/local/bin/runc
rm -f buildkit.tar.gz
cat > /etc/systemd/system/buildkitd.service <<'EOF'
[Unit]
Description=buildkitd
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/buildkitd --addr tcp://0.0.0.0:{{.Port}} --tlscacert /etc/buildkit/ca.pem --tlscert /etc/buildkit/server-cert.pem --tlskey /etc/buildkit/server-key.pem --oci-worker-snapshotter=overlayfs
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now buildkitd
`))

// buildVMFirewallRules: buildkitd reachable only from the cluster's public
// egress IPs; outbound only DNS, HTTP and HTTPS (the builds' base images,
// module proxies, apt and release downloads).
func buildVMFirewallRules(sources []string) []hcloud.FirewallRule {
	anywhere := []string{"0.0.0.0/0"}
	return []hcloud.FirewallRule{
		{Direction: "in", Protocol: "tcp", Port: strconv.Itoa(buildVMPort), SourceIPs: sources},
		{Direction: "out", Protocol: "tcp", Port: "80", DestinationIPs: anywhere},
		{Direction: "out", Protocol: "tcp", Port: "443", DestinationIPs: anywhere},
		{Direction: "out", Protocol: "udp", Port: "53", DestinationIPs: anywhere},
		{Direction: "out", Protocol: "tcp", Port: "53", DestinationIPs: anywhere},
	}
}

// buildVMEgressRule admits the runner pod to exactly its VM's buildkitd:
// toCIDR and toPorts in one rule item, so the port applies to that CIDR.
func buildVMEgressRule(ipv4 string) map[string]any {
	return map[string]any{
		"toCIDR": []string{ipv4 + "/32"},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": strconv.Itoa(buildVMPort), "protocol": "TCP"}},
		}},
	}
}

// buildVMEnv is what pr.yml's image-build job reads: buildctl takes
// BUILDKIT_HOST itself; the TLS directory and server name go to --tlsdir
// and --tlsservername.
func buildVMEnv(ipv4 string) []any {
	return []any{
		map[string]any{"name": "BUILDKIT_HOST", "value": "tcp://" + net.JoinHostPort(ipv4, strconv.Itoa(buildVMPort))},
		map[string]any{"name": "BUILDKIT_TLS_DIR", "value": buildTLSDir},
		map[string]any{"name": "BUILDKIT_TLS_SERVERNAME", "value": buildVMServerName},
	}
}

// buildTLSSecretData uses buildctl's --tlsdir file names; the server key
// and the CA key never enter the cluster.
func buildTLSSecretData(p buildPKI) map[string]string {
	return map[string]string{"ca.pem": string(p.CACert), "cert.pem": string(p.ClientCert), "key.pem": string(p.ClientKey)}
}

// BuildVMProvider is the Hetzner API slice build VMs use.
type BuildVMProvider interface {
	cloudres.Provider
	CreateServer(ctx context.Context, r hcloud.CreateServerRequest) (hcloud.Server, error)
	CreateFirewall(ctx context.Context, r hcloud.CreateFirewallRequest) (hcloud.Firewall, error)
	Pricing(ctx context.Context) (hcloud.Pricing, error)
}

type BuildVMConfig struct {
	Max int // CI_BUILD_VMS_MAX; 0 = 2, the CI project's IP quota
	// AllowedSources are the CIDRs the cluster's pods reach the internet
	// from (the nodes' public IPv4s): the only sources the VM's firewall
	// admits to buildkitd.
	AllowedSources []string
	ServerType     string             // "" = cx23
	Location       string             // "" = fsn1
	Image          string             // "" = debian-12
	Lease          time.Duration      // job deadline + margin; the collector's backstop
	Metrics        *telemetry.Metrics // nil = the no-op meter
}

// BuildVMs creates and deletes build VMs; each resource is registered
// before it is created, so the collector finds whatever a crash leaves.
type BuildVMs struct {
	cfg       BuildVMConfig
	provider  BuildVMProvider
	registry  cloudres.Registry
	collector *cloudres.Collector
	now       func() time.Time

	// the spawner's poll loop is the only caller: no lock
	pricing      hcloud.Pricing
	pricedAt     time.Time
	backoffUntil time.Time

	metrics *telemetry.Metrics
	// ready: open server row id → buildkitd's first answer, set by the
	// spawner right before it releases the VM and taken by Deleted
	readyMu sync.Mutex
	ready   map[int64]time.Time
}

const (
	buildVMPricesTTL = time.Hour
	// buildVMBackoff pauses build claims after a failed provision, so a
	// provider outage or a bad token costs no runner registration and
	// namespace per tick.
	buildVMBackoff = time.Minute
)

func NewBuildVMs(cfg BuildVMConfig, provider BuildVMProvider, registry cloudres.Registry, collector *cloudres.Collector) (*BuildVMs, error) {
	if cfg.Max <= 0 {
		cfg.Max = 2
	}
	if cfg.ServerType == "" {
		cfg.ServerType = "cx23"
	}
	if cfg.Location == "" {
		cfg.Location = "fsn1"
	}
	if cfg.Image == "" {
		cfg.Image = "debian-12"
	}
	if cfg.Lease <= 0 {
		return nil, errors.New("build vms: Lease is required")
	}
	sources, err := cloudres.IPv4Sources(cfg.AllowedSources)
	if err != nil {
		return nil, fmt.Errorf("build vms: %w", err)
	}
	cfg.AllowedSources = sources
	if cfg.Metrics == nil {
		cfg.Metrics = telemetry.Noop()
	}
	return &BuildVMs{cfg: cfg, provider: provider, registry: registry, collector: collector, now: time.Now,
		metrics: cfg.Metrics, ready: map[int64]time.Time{}}, nil
}

// BuildVM is a provisioned VM and the material its job's runner needs.
type BuildVM struct {
	IPv4 string
	PKI  buildPKI
}

// FreeSlot reports whether another VM may be created: not while backing
// off a failed provision, and only held rows count — requested, live, or
// deleting with a release still retrying its deletes. A deleting row the
// collector owns holds no slot: its delete was already accepted, so its IP
// is the provider's to give back and waits on nothing factoryd still has to
// do. Counting it held the slot through the release of the rest of its
// commission (a firewall a just-deleted server still answers
// resource_in_use for) and through the collector passes that finish an
// interrupted teardown, which is what made a build job wait minutes past
// the cap rather than seconds.
func (b *BuildVMs) FreeSlot(ctx context.Context) (bool, error) {
	if b.now().Before(b.backoffUntil) {
		return false, nil
	}
	rows, err := b.registry.HeldOpen(ctx, "ci")
	if err != nil {
		return false, err
	}
	n := 0
	for _, r := range rows {
		if r.Kind == cloudres.KindServer {
			n++
		}
	}
	return n < b.cfg.Max, nil
}

// Provision creates jobID's firewall and server. On any failure it
// releases what it made; ErrNoBuildVMSlot means try again later.
func (b *BuildVMs) Provision(ctx context.Context, jobID int64) (vm BuildVM, err error) {
	free, err := b.FreeSlot(ctx)
	if err != nil {
		return BuildVM{}, err
	}
	if !free {
		return BuildVM{}, ErrNoBuildVMSlot
	}
	defer func() {
		if err != nil {
			// Any failed provision backs off: while the provider answers
			// errors, further ticks claim nothing, so an outage or a bad
			// token costs no runner registration and namespace per tick.
			// What the failure left behind decides only the log: a release
			// that closed everything (a quota refusal whose firewall went)
			// leaves no trace, a refused delete stays deleting for the
			// collector — the slot is free either way, and the minute of
			// quiet is what keeps the next poll from churning.
			if rerr := b.Release(context.WithoutCancel(ctx), jobID); rerr != nil {
				log.Printf("build vm: release after failed provision for job %d: %v", jobID, rerr)
			}
			b.backoffUntil = b.now().Add(buildVMBackoff)
		}
	}()
	pricing, err := b.prices(ctx)
	if err != nil {
		return BuildVM{}, err
	}
	serverPrice, err := pricing.ServerHourlyNet(b.cfg.ServerType, b.cfg.Location)
	if err != nil {
		return BuildVM{}, err
	}
	ipPrice, err := pricing.PrimaryIPHourlyNet("ipv4", b.cfg.Location)
	if err != nil {
		return BuildVM{}, err
	}
	now := b.now()
	pki, err := newBuildPKI(now, b.cfg.Lease+time.Hour)
	if err != nil {
		return BuildVM{}, fmt.Errorf("pki: %w", err)
	}
	userData, err := buildVMUserData(pki)
	if err != nil {
		return BuildVM{}, err
	}
	owner := strconv.FormatInt(jobID, 10)
	row := func(kind string, price float64, size string) (cloudres.Resource, error) {
		return b.registry.Insert(ctx, cloudres.Resource{
			Project: "ci", Kind: kind, OwnerRef: owner, PricePerHourEUR: price, Size: size,
			// unique while an earlier VM of the same job is still deleting
			Name:         fmt.Sprintf("ci-build-%d-%s-%d", jobID, kind, now.Unix()),
			Labels:       map[string]string{"job": owner},
			LeaseExpires: now.Add(b.cfg.Lease),
		})
	}

	fwRow, err := row(cloudres.KindFirewall, 0, "")
	if err != nil {
		return BuildVM{}, fmt.Errorf("register firewall: %w", err)
	}
	fw, err := b.provider.CreateFirewall(ctx, hcloud.CreateFirewallRequest{
		Name: fwRow.Name, Labels: fwRow.Labels, Rules: buildVMFirewallRules(b.cfg.AllowedSources),
	})
	if err != nil {
		return BuildVM{}, fmt.Errorf("create firewall: %w", err)
	}
	if _, err := b.registry.SetLive(ctx, fwRow.ID, strconv.FormatInt(fw.ID, 10)); err != nil {
		return BuildVM{}, err
	}

	srvRow, err := row(cloudres.KindServer, serverPrice+ipPrice, b.cfg.ServerType)
	if err != nil {
		return BuildVM{}, fmt.Errorf("register server: %w", err)
	}
	srv, err := b.provider.CreateServer(ctx, hcloud.CreateServerRequest{
		Name: srvRow.Name, ServerType: b.cfg.ServerType, Location: b.cfg.Location, Image: b.cfg.Image,
		UserData: userData, Labels: srvRow.Labels,
		Firewalls: []hcloud.FirewallRef{{Firewall: fw.ID}},
		// IPv4 only: GitHub release downloads have no IPv6, and each
		// primary IP counts against the project's quota.
		PublicNet: &hcloud.PublicNet{EnableIPv4: true},
	})
	if hcloud.IsCode(err, "resource_limit_exceeded") {
		return BuildVM{}, fmt.Errorf("%w: %v", ErrNoBuildVMSlot, err)
	}
	if err != nil {
		return BuildVM{}, fmt.Errorf("create server: %w", err)
	}
	if _, err := b.registry.SetLive(ctx, srvRow.ID, strconv.FormatInt(srv.ID, 10)); err != nil {
		return BuildVM{}, err
	}
	if srv.IPv4() == "" {
		return BuildVM{}, fmt.Errorf("server %d has no public IPv4", srv.ID)
	}
	return BuildVM{IPv4: srv.IPv4(), PKI: pki}, nil
}

// SetReady records when buildkitd first answered jobID's build runner:
// its init container's finish. Only an open server row takes it.
func (b *BuildVMs) SetReady(ctx context.Context, jobID int64, at time.Time) error {
	rows, err := b.registry.OpenByOwner(ctx, "ci", strconv.FormatInt(jobID, 10))
	if err != nil {
		return err
	}
	b.readyMu.Lock()
	defer b.readyMu.Unlock()
	for _, r := range rows {
		if r.Kind == cloudres.KindServer {
			b.ready[r.ID] = at
		}
	}
	return nil
}

// Deleted is the CI collector's hook for a closed build VM row:
// factory.ci.buildvm.lifetime, .ready and .cost (started hours times the
// row's hourly price, as the ledger counts them).
func (b *BuildVMs) Deleted(ctx context.Context, row cloudres.Resource, at time.Time, by string) {
	b.readyMu.Lock()
	ready, ok := b.ready[row.ID]
	delete(b.ready, row.ID)
	b.readyMu.Unlock()
	attrs := metric.WithAttributes(attribute.String("server_type", row.Size), attribute.String("deleted_by", by))
	lifetime := at.Sub(row.CreatedAt)
	b.metrics.BuildVMLifetime.Record(ctx, lifetime.Seconds(), attrs)
	b.metrics.BuildVMCost.Add(ctx, max(1, math.Ceil(lifetime.Hours()))*row.PricePerHourEUR, attrs)
	if ok {
		b.metrics.BuildVMReady.Record(ctx, ready.Sub(row.CreatedAt).Seconds(), attrs)
	}
}

// prices is GET /v1/pricing, refreshed hourly; a failed refresh keeps the
// last catalogue.
func (b *BuildVMs) prices(ctx context.Context) (hcloud.Pricing, error) {
	if !b.pricedAt.IsZero() && b.now().Sub(b.pricedAt) < buildVMPricesTTL {
		return b.pricing, nil
	}
	p, err := b.provider.Pricing(ctx)
	if err != nil {
		if b.pricedAt.IsZero() {
			return hcloud.Pricing{}, fmt.Errorf("pricing: %w", err)
		}
		log.Printf("build vm: pricing: %v; keeping the catalogue from %s", err, b.pricedAt.Format(time.RFC3339))
		return b.pricing, nil
	}
	b.pricing, b.pricedAt = p, b.now()
	return p, nil
}

// Release deletes what jobID still holds and closes those rows. A row left
// deleting is the collector's to finish — it lists every open row of the
// project, missing namespace included — so a firewall a just-deleted server
// still answers resource_in_use for neither fails this release nor keeps
// the next build waiting on it. A firewall already left deleting by an
// earlier release comes back into this one: its server's delete was
// accepted, so the provider's own attachment teardown is nearly done, and
// a few spaced retries usually get it past resource_in_use in this poll —
// the slot is free either way; this is only what closes the row.
func (b *BuildVMs) Release(ctx context.Context, jobID int64) error {
	rows, err := b.registry.OpenByOwner(ctx, "ci", strconv.FormatInt(jobID, 10))
	if err != nil {
		return err
	}
	return b.collector.Release(ctx, rows)
}

// CISweepLabelSelector selects every CI build machine in the Hetzner CI
// project: the registry's provider labels carry factory=ci on each one
// (cloudres.providerLabels).
const CISweepLabelSelector = "factory=ci"

// CISweepProvider is the narrower Hetzner API slice the CI sweep needs;
// both hcloud.Client and BuildVMProvider satisfy it.
type CISweepProvider interface {
	Servers(ctx context.Context, labelSelector string) ([]hcloud.Server, error)
	DeleteServer(ctx context.Context, id int64) error
}

// SweepCIMachines deletes every server labelled factory=ci in the CI
// project behind provider (the token decides the project): the shutdown's
// last word on CI build machines. It is called during shutdown after the
// CI spawner stops, so nothing it deletes belongs to a live job, and
// before the backups and the zero check, so no machine created late for
// the last infra push's CI survives the cluster (roadmap 189, Stage 18
// item 3.15). A failed list ends the sweep at once; a failed delete does
// not stop the rest, and the failures come back joined (errors.Join).
// It returns nil when every listed server is gone. The sweep only touches
// servers: firewalls and primary IPs of a deleted server go with it or
// fall to the next round's collector.
func SweepCIMachines(ctx context.Context, provider CISweepProvider) error {
	servers, err := provider.Servers(ctx, CISweepLabelSelector)
	if err != nil {
		return fmt.Errorf("sweep ci machines: list servers: %w", err)
	}
	var errs []error
	for _, s := range servers {
		if err := provider.DeleteServer(ctx, s.ID); err != nil {
			errs = append(errs, fmt.Errorf("sweep ci machines: delete server %d: %w", s.ID, err))
		}
	}
	return errors.Join(errs...)
}

// CIOwnerDone is the CI collector's owner check: a build VM's job is
// finished once its CI namespace is gone (the spawner releases the VM
// before it deletes the namespace; this catches a factoryd that died
// between the two).
func CIOwnerDone(kube Kube) func(ctx context.Context, owner string) (bool, error) {
	return func(ctx context.Context, owner string) (bool, error) {
		jobID, err := strconv.ParseInt(owner, 10, 64)
		if err != nil {
			return false, fmt.Errorf("owner %q is not a job id", owner)
		}
		names, err := kube.NamespacesByLabel(ctx, "factory.ai/task", jobLabel(jobID))
		if err != nil {
			return false, err
		}
		return len(names) == 0, nil
	}
}
