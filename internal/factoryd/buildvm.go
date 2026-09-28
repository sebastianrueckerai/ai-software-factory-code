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
	"math/big"
	"net"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
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
	ServerType     string        // "" = cx23
	Location       string        // "" = fsn1
	Image          string        // "" = debian-12
	Lease          time.Duration // job deadline + margin; the collector's backstop
}

// BuildVMs creates and deletes build VMs; each resource is registered
// before it is created, so the collector finds whatever a crash leaves.
type BuildVMs struct {
	cfg       BuildVMConfig
	provider  BuildVMProvider
	registry  cloudres.Registry
	collector *cloudres.Collector
	now       func() time.Time
}

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
	return &BuildVMs{cfg: cfg, provider: provider, registry: registry, collector: collector, now: time.Now}, nil
}

// BuildVM is a provisioned VM and the material its job's runner needs.
type BuildVM struct {
	IPv4 string
	PKI  buildPKI
}

// FreeSlot reports whether another VM may be created: open server rows
// count, deleting ones included, since their IP is not yet released.
func (b *BuildVMs) FreeSlot(ctx context.Context) (bool, error) {
	rows, err := b.registry.Open(ctx, "ci")
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
			if rerr := b.Release(context.WithoutCancel(ctx), jobID); rerr != nil {
				log.Printf("build vm: release after failed provision for job %d: %v", jobID, rerr)
			}
		}
	}()
	pricing, err := b.provider.Pricing(ctx)
	if err != nil {
		return BuildVM{}, fmt.Errorf("pricing: %w", err)
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
	row := func(kind string, price float64) (cloudres.Resource, error) {
		return b.registry.Insert(ctx, cloudres.Resource{
			Project: "ci", Kind: kind, OwnerRef: owner, PricePerHourEUR: price,
			// unique while an earlier VM of the same job is still deleting
			Name:         fmt.Sprintf("ci-build-%d-%s-%d", jobID, kind, now.Unix()),
			Labels:       map[string]string{"job": owner},
			LeaseExpires: now.Add(b.cfg.Lease),
		})
	}

	fwRow, err := row(cloudres.KindFirewall, 0)
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

	srvRow, err := row(cloudres.KindServer, serverPrice+ipPrice)
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

// Release deletes every open resource of jobID, servers first, and closes
// the rows (cloudres.Collector.Release).
func (b *BuildVMs) Release(ctx context.Context, jobID int64) error {
	rows, err := b.registry.OpenByOwner(ctx, "ci", strconv.FormatInt(jobID, 10))
	if err != nil {
		return err
	}
	return b.collector.Release(ctx, rows)
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
