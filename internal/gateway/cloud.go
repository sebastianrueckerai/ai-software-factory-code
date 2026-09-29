package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Commissioned servers: one location and image, root login by certificate
// only, certificates at most maxCertTTL. A certificate's principal names
// one server (principal), which alone lists it (ruling 125).
const (
	cloudLocation = "fsn1"
	cloudImage    = "debian-12"
	loginUser     = "root"
	maxCertTTL    = 30 * time.Minute
	// createTimeout bounds a commission's provider calls, cut or not.
	createTimeout = 2 * time.Minute
	pricesTTL     = time.Hour
)

// CloudProvider is the agent project's Hetzner API.
type CloudProvider interface {
	cloudres.Provider
	CreateServer(ctx context.Context, r hcloud.CreateServerRequest) (hcloud.Server, error)
	CreateFirewall(ctx context.Context, r hcloud.CreateFirewallRequest) (hcloud.Firewall, error)
	CreateVolume(ctx context.Context, r hcloud.CreateVolumeRequest) (hcloud.Volume, error)
	Pricing(ctx context.Context) (hcloud.Pricing, error)
}

// CloudStore is the commissioning state (cloudres.AgentStore).
type CloudStore interface {
	Use(ctx context.Context, repo string) (cloudres.EnvelopeUse, error)
	Reserve(ctx context.Context, r cloudres.CommissionRequest) (cloudres.Commission, error)
	SetAddress(ctx context.Context, id int64, address string) error
	Owned(ctx context.Context, taskID string) ([]cloudres.Resource, error)
	Group(ctx context.Context, holdID int64) ([]cloudres.Resource, error)
	Approval(ctx context.Context, id int64, taskID string) (cloudres.Approval, bool, error)
	Accrue(ctx context.Context) (cloudres.Accrual, error)
	Session(ctx context.Context, taskID string) (string, error)
}

// SSHSigner is the SSH CA (OpenBao).
type SSHSigner interface {
	CAPublicKey(ctx context.Context) (string, error)
	Sign(ctx context.Context, publicKey, principal string, ttl time.Duration, keyID string) (string, error)
}

var (
	_ CloudProvider = (*hcloud.Client)(nil)
	_ CloudStore    = cloudres.AgentStore{}
	_ SSHSigner     = (*OpenBao)(nil)
)

// Cloud is the commissioning connector: the agent project's API, the
// registry and its collector (project agent), the SSH CA, and the cluster
// nodes' IPv4s, the only SSH sources a server's firewall admits.
type Cloud struct {
	Provider  CloudProvider
	Store     CloudStore
	Registry  cloudres.Registry
	Collector *cloudres.Collector
	Signer    SSHSigner
	Sources   []string
	// Audit records accrual overruns (the gateway's store).
	Audit interface {
		InsertAudit(ctx context.Context, row AuditRow) error
	}
	Now func() time.Time

	mu       sync.Mutex
	pricing  hcloud.Pricing
	pricedAt time.Time
}

func (c *Cloud) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// prices is GET /v1/pricing, refreshed hourly; a failed refresh keeps the
// last catalogue.
func (c *Cloud) prices(ctx context.Context) (hcloud.Pricing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pricedAt.IsZero() && c.now().Sub(c.pricedAt) < pricesTTL {
		return c.pricing, nil
	}
	p, err := c.Provider.Pricing(ctx)
	if err != nil {
		if c.pricedAt.IsZero() {
			return hcloud.Pricing{}, fmt.Errorf("hetzner pricing: %w", err)
		}
		log.Printf("hetzner pricing: %v; keeping the catalogue from %s", err, c.pricedAt.Format(time.RFC3339))
		return c.pricing, nil
	}
	c.pricing, c.pricedAt = p, c.now()
	return p, nil
}

// Run collects the agent project and accrues cost every collector
// interval.
func (c *Cloud) Run(ctx context.Context) {
	tick := time.NewTicker(c.Collector.Interval)
	defer tick.Stop()
	for {
		if err := c.Collector.Collect(ctx); err != nil {
			log.Printf("cloud collector (agent): %v", err)
		}
		if err := c.accrue(ctx); err != nil {
			log.Printf("cloud accrual (agent): %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// accrue books the resources' started hours; a booking past its
// reservation is booked in full and flagged, loud in the log and as an
// accrued audit row carrying the flag (rulings 117, 124).
func (c *Cloud) accrue(ctx context.Context) error {
	acc, err := c.Store.Accrue(ctx)
	for _, o := range acc.Overruns {
		spend := o.EUR
		if aerr := c.Audit.InsertAudit(context.WithoutCancel(ctx), AuditRow{TaskID: o.TaskID, Decision: "accrued",
			Reason: fmt.Sprintf("resource %d: %.6f EUR booked", o.ResourceID, o.EUR), Tool: policy.ToolCommission,
			HoldEUR: &o.HeldEUR, SpendEUR: &spend, OverHold: o.Reason}); aerr != nil {
			err = errors.Join(err, aerr)
		}
	}
	return err
}

// TaskOwnerDone is the agent collector's owner check: a task that reached
// done, failed or closed (or has no row) ends its leases.
func TaskOwnerDone(store Store) func(ctx context.Context, taskID string) (bool, error) {
	return func(ctx context.Context, taskID string) (bool, error) {
		t, ok, err := store.Task(ctx, taskID)
		if err != nil {
			return false, err
		}
		return !ok || t.Status == "done" || t.Status == "failed" || t.Status == "closed", nil
	}
}

type CloudPricesIn struct{}

type CommissionIn struct {
	ServerType   string `json:"server_type" jsonschema:"a Hetzner server type the project's envelope allows (see cloud_prices), e.g. cx23"`
	Purpose      string `json:"purpose" jsonschema:"what the server is for, shown to the human on approval cards"`
	LeaseMinutes int    `json:"lease_minutes" jsonschema:"how long the server may live, at least 5; billed per started hour; access ends and it is deleted shortly before the lease ends, or when the task finishes"`
	VolumeGB     int    `json:"volume_gb,omitempty" jsonschema:"an optional block volume in GB (at least 10), mounted on the server"`
	SSHPublicKey string `json:"ssh_public_key,omitempty" jsonschema:"optional: your SSH public key (one line); the answer then carries a certificate for it"`
	Lifetime     string `json:"lifetime,omitempty" jsonschema:"lease (the default and the only one a task may ask for): the server ends with its lease or the task"`
}

type ResourceIn struct {
	ResourceID int64 `json:"resource_id" jsonschema:"the id commission_server answered"`
}

type CertificateIn struct {
	ResourceID int64  `json:"resource_id" jsonschema:"the server's id from commission_server"`
	PublicKey  string `json:"public_key" jsonschema:"your SSH public key, one line, e.g. the content of ~/.ssh/id_ed25519.pub"`
}

type RequestStatusIn struct {
	RequestID int64 `json:"request_id" jsonschema:"the approval request id commission_server answered"`
}

// addCloudTools lists the commissioning tools on srv.
func (s *Server) addCloudTools(srv *mcp.Server, c *toolCaller) {
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolCloudPrices, Description: "The server types your project may commission, with EUR per hour and month (net, incl. the IPv4), " +
		"the volume price and the project's envelope (monthly cap, count, longest lease). Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ CloudPricesIn) (*mcp.CallToolResult, any, error) {
			return s.cloudPrices(ctx, c), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolCommission, Description: "Commission a throwaway server (Debian 12, public IPv4, SSH as root by certificate only, from this sandbox only) " +
		"for lease_minutes. Its cost is reserved against the task budget and charged per started hour; release it when done. " +
		"A request beyond the project's envelope becomes a human approval request."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CommissionIn) (*mcp.CallToolResult, any, error) {
			return s.commission(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolListResources, Description: "List this task's commissioned resources: id, type, address, state, lease end and cost so far. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ CloudPricesIn) (*mcp.CallToolResult, any, error) {
			return s.listResources(ctx, c), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRelease, Description: "Delete a commissioned server now, with its firewall and volume; the unused reservation returns to the task. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ResourceIn) (*mcp.CallToolResult, any, error) {
			return s.releaseResource(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolSSHCertificate, Description: "Sign your SSH public key for a commissioned server: a certificate that logs in as root on that server only, valid at most 30 minutes " +
		"and never past the lease. Save it next to the private key as <key>-cert.pub. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CertificateIn) (*mcp.CallToolResult, any, error) {
			return s.sshCertificate(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestStatus, Description: "The status of a resource approval request: pending, approved (then call commission_server again with the same arguments) or denied. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestStatusIn) (*mcp.CallToolResult, any, error) {
			return s.requestStatus(ctx, c, in), nil, nil
		})
}

// admitCloud: the commissioning tools (architecture 5.5: agents get
// servers in the agent Hetzner project through the gateway, which alone
// holds its token) are for running tasks whose classes list the tool and
// that have a repository (the envelope's project).
func (s *Server) admitCloud(c *toolCaller, tool string) *mcp.CallToolResult {
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	if c.task.Repository == "" {
		return s.refuseTool(c, tool, "refused_arguments", "the task has no repository, so no project envelope applies")
	}
	return nil
}

// envelope is the task's project's envelope in the policy version in
// force: its file's, else the default.
func envelope(c *toolCaller) (policy.Envelope, string) {
	if name, _, ok := c.pv.ProjectOf(c.task.Repository); ok {
		return c.pv.EnvelopeOf(c.task.Repository), fmt.Sprintf("project %s's envelope (projects/%s.yaml, policy version %d)", name, name, c.pv.ID)
	}
	return c.pv.EnvelopeOf(c.task.Repository), fmt.Sprintf("the standard envelope (policy version %d): no project file names %s", c.pv.ID, c.task.Repository)
}

func (s *Server) cloudPrices(ctx context.Context, c *toolCaller) *mcp.CallToolResult {
	tool := policy.ToolCloudPrices
	if r := s.admitCloud(c, tool); r != nil {
		return r
	}
	env, which := envelope(c)
	use, err := s.opts.Cloud.Store.Use(ctx, c.task.Repository)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	p, err := s.opts.Cloud.prices(ctx)
	if err != nil {
		return s.cloudError(c, tool, "error_price", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Location %s, net EUR. Server prices include the server's public IPv4.\n", cloudLocation)
	for _, typ := range env.ServerTypes {
		q, err := cloudres.PriceCommission(p, cloudLocation, typ, 60, 0)
		if err != nil {
			fmt.Fprintf(&b, "- %s: no price (%v)\n", typ, err)
			continue
		}
		fmt.Fprintf(&b, "- %s: %.4f EUR/h, %.2f EUR/month\n", typ, q.ServerHourly, q.MonthlyEUR)
	}
	if gb, err := p.VolumeGBMonthNet(); err == nil {
		fmt.Fprintf(&b, "Volumes: %.4f EUR per GB and month, prorated per started hour, %d to %d GB.\n", gb, policy.MinVolumeGB, env.VolumeMaxGB)
	}
	fmt.Fprintf(&b, "Envelope (%s): %.2f EUR a month (%.6f used or reserved this month), at most %d servers at once (%d open), leases up to %d h.\n"+
		"A lease is billed per started hour and reserved against the task budget up front. Beyond the envelope, commission_server files a human approval request.",
		which, env.MonthlyEUR, use.MonthEUR, env.MaxCount, use.OpenServers, env.MaxLeaseHours)
	s.auditTool(c, tool, "allowed", "", &c.task.SpentEUR, nil)
	return textResult(b.String())
}

var (
	serverTypeArg = regexp.MustCompile(`^[a-z0-9]{2,16}$`)
	sshPublicKey  = regexp.MustCompile(`^(ssh-ed25519|ecdsa-sha2-nistp(256|384|521)|ssh-rsa) [A-Za-z0-9+/]+={0,2}( [^\r\n]{0,200})?$`)
)

// A lease runs past the collector's cutoff (cloudres.LeaseCutoff, 3.5
// minutes before its end) by at least a minute and a half, and at most a
// week, approved or not.
const (
	minLeaseMinutes = 5
	maxLeaseMinutes = 7 * 24 * 60
	maxVolumeGB     = 1024
	maxPurpose      = 500
)

func (in CommissionIn) validate() error {
	switch {
	case !serverTypeArg.MatchString(in.ServerType):
		return errors.New("server_type must be a Hetzner server type name such as cx23")
	case strings.TrimSpace(in.Purpose) == "" || len(in.Purpose) > maxPurpose:
		return fmt.Errorf("purpose is required, at most %d bytes", maxPurpose)
	case in.LeaseMinutes < minLeaseMinutes || in.LeaseMinutes > maxLeaseMinutes:
		return fmt.Errorf("lease_minutes must be %d to %d", minLeaseMinutes, maxLeaseMinutes)
	case in.VolumeGB != 0 && (in.VolumeGB < policy.MinVolumeGB || in.VolumeGB > maxVolumeGB):
		return fmt.Errorf("volume_gb must be 0 (none) or %d to %d", policy.MinVolumeGB, maxVolumeGB)
	case in.Lifetime == cloudres.LifetimeStanding:
		return errors.New("a task commissions leased servers only: standing ones come from project registration or a human in the console")
	case in.Lifetime != "" && in.Lifetime != cloudres.LifetimeLease:
		return errors.New("lifetime must be lease")
	case in.SSHPublicKey != "" && !sshPublicKey.MatchString(strings.TrimSpace(in.SSHPublicKey)):
		return errors.New("ssh_public_key must be one OpenSSH public key line (ssh-ed25519, ecdsa-sha2-nistp* or ssh-rsa)")
	}
	return nil
}

func (s *Server) commission(ctx context.Context, c *toolCaller, in CommissionIn) *mcp.CallToolResult {
	tool := policy.ToolCommission
	if r := s.admitCloud(c, tool); r != nil {
		return r
	}
	if err := in.validate(); err != nil {
		return s.refuseTool(c, tool, "refused_arguments", err.Error())
	}
	env, _ := envelope(c)
	if len(env.ServerTypes) == 0 {
		return s.refuseTool(c, tool, "refused_capability", fmt.Sprintf("policy version %d gives %s no envelope", c.pv.ID, c.task.Repository))
	}
	cloud := s.opts.Cloud
	p, err := cloud.prices(ctx)
	if err != nil {
		return s.cloudError(c, tool, "error_price", err)
	}
	q, err := cloudres.PriceCommission(p, cloudLocation, in.ServerType, in.LeaseMinutes, in.VolumeGB)
	if err != nil {
		return s.refuseTool(c, tool, "refused_price", err.Error())
	}
	session, err := cloud.Store.Session(ctx, c.task.ID)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	com, err := cloud.Store.Reserve(ctx, cloudres.CommissionRequest{
		TaskID: c.task.ID, Repo: c.task.Repository, Session: session, ServerType: in.ServerType,
		Purpose: strings.TrimSpace(in.Purpose), LeaseMinutes: in.LeaseMinutes, VolumeGB: in.VolumeGB,
		Location: cloudLocation, Quote: q, Envelope: env, DayEUR: c.pv.Document.Budget.DayEUR, Now: cloud.now(),
	})
	var refusal *policy.Refusal
	if errors.As(err, &refusal) {
		return s.refuseTool(c, tool, refusal.Decision, refusal.Reason)
	}
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	switch com.Status {
	case cloudres.Pending:
		s.auditTool(c, tool, "pending_approval", fmt.Sprintf("approval request %d: %s", com.ApprovalID, strings.Join(com.Violations, "; ")), &c.task.SpentEUR, nil)
		return textResult(fmt.Sprintf("pending human approval %d: the request is beyond the project's envelope (%s). "+
			"Nothing was created or charged. Poll with resource_request_status (request_id %d); once approved, call commission_server again with the same arguments.",
			com.ApprovalID, strings.Join(com.Violations, "; "), com.ApprovalID))
	case cloudres.Denied:
		s.auditTool(c, tool, "refused_denied", fmt.Sprintf("approval request %d denied: %s", com.ApprovalID, com.Note), &c.task.SpentEUR, nil)
		return toolError(fmt.Sprintf("a human denied this request (approval request %d): %s. Do not ask for it again; work within the envelope (%s).",
			com.ApprovalID, com.Note, strings.Join(com.Violations, "; ")))
	}

	// the provider bills whether or not the agent still listens
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createTimeout)
	defer cancel()
	srv, vol, err := s.create(cctx, com, in)
	if err != nil {
		note := s.abandon(cctx, com)
		s.auditTool(c, tool, "error_tool", err.Error()+note, &c.task.SpentEUR, &com.Hold.EUR)
		return toolError("commissioning failed: " + err.Error() + ". Nothing is left running; the reservation returns to the task.")
	}
	row := com.Rows[1]
	lease := row.LeaseExpires.UTC().Format(time.RFC3339)
	var b strings.Builder
	fmt.Fprintf(&b, "commissioned resource %d: %s server %s at %s, login %s; lease until %s (billed per started hour; your access ends and it is deleted up to %s before then, at the collector's last pass, or when the task ends).\n",
		row.ID, in.ServerType, row.Name, srv.IPv4(), loginUser, lease, cloudres.LeaseCutoff)
	fmt.Fprintf(&b, "Reserved %.6f EUR against the task budget (%.4f EUR/h incl. the IPv4", q.ReservationEUR, q.ServerHourly)
	if vol.ID != 0 {
		fmt.Fprintf(&b, ", plus %.6f EUR/h for the %d GB volume mounted at /mnt/HC_Volume_%d", q.VolumeHourly, in.VolumeGB, vol.ID)
	}
	b.WriteString("). Release it with release_resource as soon as you are done.\n")
	b.WriteString("SSH accepts only certificates from the factory CA (no passwords, no keys), from this sandbox only, about a minute after creation. " +
		"Get one with ssh_certificate (resource_id, your public key) and connect with ssh -i <key> -o CertificateFile=<key>-cert.pub " + loginUser + "@" + srv.IPv4() + ".")
	if in.SSHPublicKey != "" {
		cert, ttl, err := s.sign(ctx, c, row, strings.TrimSpace(in.SSHPublicKey))
		if err != nil {
			fmt.Fprintf(&b, "\nNo certificate yet: %v; ask ssh_certificate again.", err)
		} else {
			fmt.Fprintf(&b, "\nCertificate for your key (valid %s), save it as <key>-cert.pub:\n%s", ttl, cert)
		}
	}
	s.auditTool(c, tool, "allowed", fmt.Sprintf("resource %d (%s, %d min, %d GB) at %s", row.ID, in.ServerType, in.LeaseMinutes, in.VolumeGB, srv.IPv4()),
		&c.task.SpentEUR, &com.Hold.EUR)
	return textResult(b.String())
}

// create makes the commission's firewall, server and volume in that order,
// each after its row was written (Reserve), recording each provider id.
func (s *Server) create(ctx context.Context, com cloudres.Commission, in CommissionIn) (hcloud.Server, hcloud.Volume, error) {
	cloud := s.opts.Cloud
	fwRow, srvRow := com.Rows[0], com.Rows[1]
	fw, err := cloud.Provider.CreateFirewall(ctx, hcloud.CreateFirewallRequest{
		Name: fwRow.Name, Labels: fwRow.Labels, Rules: sshFirewallRules(cloud.Sources)})
	if err != nil {
		return hcloud.Server{}, hcloud.Volume{}, fmt.Errorf("create firewall: %w", err)
	}
	if _, err := cloud.Registry.SetLive(ctx, fwRow.ID, strconv.FormatInt(fw.ID, 10)); err != nil {
		return hcloud.Server{}, hcloud.Volume{}, err
	}
	ca, err := cloud.Signer.CAPublicKey(ctx)
	if err != nil {
		return hcloud.Server{}, hcloud.Volume{}, err
	}
	userData, err := commissionUserData(ca, principal(srvRow.ID))
	if err != nil {
		return hcloud.Server{}, hcloud.Volume{}, err
	}
	srv, err := cloud.Provider.CreateServer(ctx, hcloud.CreateServerRequest{
		Name: srvRow.Name, ServerType: in.ServerType, Location: cloudLocation, Image: cloudImage,
		UserData: userData, Labels: srvRow.Labels, Firewalls: []hcloud.FirewallRef{{Firewall: fw.ID}},
		PublicNet: &hcloud.PublicNet{EnableIPv4: true},
	})
	if err != nil {
		return hcloud.Server{}, hcloud.Volume{}, fmt.Errorf("create server: %w", err)
	}
	if _, err := cloud.Registry.SetLive(ctx, srvRow.ID, strconv.FormatInt(srv.ID, 10)); err != nil {
		return srv, hcloud.Volume{}, err
	}
	if srv.IPv4() == "" {
		return srv, hcloud.Volume{}, fmt.Errorf("server %d has no public IPv4", srv.ID)
	}
	if err := cloud.Store.SetAddress(ctx, srvRow.ID, srv.IPv4()); err != nil {
		return srv, hcloud.Volume{}, err
	}
	if len(com.Rows) < 3 {
		return srv, hcloud.Volume{}, nil
	}
	volRow := com.Rows[2]
	vol, err := cloud.Provider.CreateVolume(ctx, hcloud.CreateVolumeRequest{
		Name: volRow.Name, Size: in.VolumeGB, Server: srv.ID, Automount: true, Format: "ext4", Labels: volRow.Labels})
	if err != nil {
		return srv, hcloud.Volume{}, fmt.Errorf("create volume: %w", err)
	}
	if _, err := cloud.Registry.SetLive(ctx, volRow.ID, strconv.FormatInt(vol.ID, 10)); err != nil {
		return srv, vol, err
	}
	return srv, vol, nil
}

// abandon releases what a failed commission made and books it: rows whose
// resource never appeared close, the rest are deleted (or left deleting
// for the collector), and accrual returns the reservation's rest.
func (s *Server) abandon(ctx context.Context, com cloudres.Commission) string {
	cloud := s.opts.Cloud
	rows, err := cloud.Store.Group(ctx, com.Hold.ID)
	if err == nil {
		err = cloud.Collector.Release(ctx, rows)
	}
	if aerr := cloud.accrue(ctx); aerr != nil {
		err = errors.Join(err, aerr)
	}
	if err != nil {
		log.Printf("commission %d: release after a failed create: %v", com.Hold.ID, err)
		return "; release left to the collector: " + err.Error()
	}
	return ""
}

// sshFirewallRules: SSH only from the cluster nodes' IPv4s; outbound only
// DNS, HTTP and HTTPS (packages, images, downloads), as build VMs.
func sshFirewallRules(sources []string) []hcloud.FirewallRule {
	anywhere := []string{"0.0.0.0/0"}
	return []hcloud.FirewallRule{
		{Direction: "in", Protocol: "tcp", Port: "22", SourceIPs: sources},
		{Direction: "out", Protocol: "tcp", Port: "80", DestinationIPs: anywhere},
		{Direction: "out", Protocol: "tcp", Port: "443", DestinationIPs: anywhere},
		{Direction: "out", Protocol: "udp", Port: "53", DestinationIPs: anywhere},
		{Direction: "out", Protocol: "tcp", Port: "53", DestinationIPs: anywhere},
	}
}

var userDataTmpl = template.Must(template.New("cloud-init").Parse(`#cloud-config
ssh_pwauth: false
disable_root: false
write_files:
- path: /etc/ssh/factory-user-ca.pub
  permissions: '0644'
  content: |
    {{.CA}}
- path: /etc/ssh/factory-principals/root
  permissions: '0644'
  content: |
    {{.Principal}}
- path: /etc/ssh/sshd_config.d/10-factory-ca.conf
  permissions: '0644'
  content: |
    TrustedUserCAKeys /etc/ssh/factory-user-ca.pub
    AuthorizedPrincipalsFile /etc/ssh/factory-principals/%u
    AuthorizedKeysFile none
    PasswordAuthentication no
    KbdInteractiveAuthentication no
runcmd:
- [usermod, -p, '*', root]
- [chage, -d, '-1', root]
- [systemctl, restart, ssh]
`))

// commissionUserData makes sshd trust only certificates the factory CA
// signed for this server's principal, as root: no authorized keys, no
// passwords. Hetzner sets an expired root
// password on a server created without SSH keys, which blocks even a
// certificate login ("Password change required"): it is removed, with
// its expiry.
func commissionUserData(caPublicKey, principal string) (string, error) {
	if strings.ContainsAny(caPublicKey, "\r\n") {
		return "", errors.New("the CA public key is not one line")
	}
	var b strings.Builder
	err := userDataTmpl.Execute(&b, struct{ CA, Principal string }{caPublicKey, principal})
	return b.String(), err
}

// principal is the one SSH certificate principal server row id accepts.
func principal(id int64) string { return "resource-" + strconv.FormatInt(id, 10) }

// owned finds the task's row id among its resources.
func (s *Server) owned(ctx context.Context, c *toolCaller, id int64) (cloudres.Resource, bool, error) {
	rows, err := s.opts.Cloud.Store.Owned(ctx, c.task.ID)
	if err != nil {
		return cloudres.Resource{}, false, err
	}
	for _, r := range rows {
		if r.ID == id {
			return r, true, nil
		}
	}
	return cloudres.Resource{}, false, nil
}

func (s *Server) listResources(ctx context.Context, c *toolCaller) *mcp.CallToolResult {
	tool := policy.ToolListResources
	if r := s.admitCloud(c, tool); r != nil {
		return r
	}
	rows, err := s.opts.Cloud.Store.Owned(ctx, c.task.ID)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	var b strings.Builder
	for _, r := range rows {
		if r.Kind == cloudres.KindFirewall {
			continue
		}
		fmt.Fprintf(&b, "resource %d: %s %s, state %s", r.ID, r.Kind, r.Size, r.State)
		if r.Address != "" {
			fmt.Fprintf(&b, ", %s", r.Address)
		}
		fmt.Fprintf(&b, ", lease until %s, %.4f EUR/h, %.6f EUR so far (%d h), purpose %q\n",
			r.LeaseExpires.UTC().Format(time.RFC3339), r.PricePerHourEUR, r.AccruedEUR, r.AccruedHours, r.Purpose)
	}
	if b.Len() == 0 {
		b.WriteString("no commissioned resources")
	}
	s.auditTool(c, tool, "allowed", "", &c.task.SpentEUR, nil)
	return textResult(b.String())
}

func (s *Server) releaseResource(ctx context.Context, c *toolCaller, in ResourceIn) *mcp.CallToolResult {
	tool := policy.ToolRelease
	if r := s.admitCloud(c, tool); r != nil {
		return r
	}
	row, ok, err := s.owned(ctx, c, in.ResourceID)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	if !ok || row.HoldID == 0 {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("resource %d is not one of this task's resources", in.ResourceID))
	}
	if row.Lifetime == cloudres.LifetimeStanding {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("resource %d is the project's standing server: only a human on the console releases it", row.ID))
	}
	cloud := s.opts.Cloud
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createTimeout)
	defer cancel()
	rows, err := cloud.Store.Group(cctx, row.HoldID)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	if len(rows) == 0 {
		return textResult(fmt.Sprintf("resource %d is already released (state %s)", row.ID, row.State))
	}
	relErr := cloud.Collector.Release(cctx, rows)
	if err := cloud.accrue(cctx); err != nil {
		relErr = errors.Join(relErr, err)
	}
	if relErr != nil {
		s.auditTool(c, tool, "error_tool", fmt.Sprintf("resource %d: %v", row.ID, relErr), &c.task.SpentEUR, nil)
		return textResult(fmt.Sprintf("resource %d: deletion started; the collector finishes it within minutes (%v)", row.ID, relErr))
	}
	s.auditTool(c, tool, "allowed", fmt.Sprintf("resource %d released", row.ID), &c.task.SpentEUR, nil)
	return textResult(fmt.Sprintf("resource %d released: the server, its firewall and volume are deleted; the unused reservation returned to the task", row.ID))
}

func (s *Server) sshCertificate(ctx context.Context, c *toolCaller, in CertificateIn) *mcp.CallToolResult {
	tool := policy.ToolSSHCertificate
	if r := s.admitCloud(c, tool); r != nil {
		return r
	}
	key := strings.TrimSpace(in.PublicKey)
	if !sshPublicKey.MatchString(key) {
		return s.refuseTool(c, tool, "refused_arguments", "public_key must be one OpenSSH public key line (ssh-ed25519, ecdsa-sha2-nistp* or ssh-rsa)")
	}
	row, ok, err := s.owned(ctx, c, in.ResourceID)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	if !ok || row.Kind != cloudres.KindServer {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("resource %d is not one of this task's servers", in.ResourceID))
	}
	if row.State != cloudres.StateLive {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("server %d is %s", row.ID, row.State))
	}
	if row.Lifetime == cloudres.LifetimeStanding {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("server %d is the project's standing server: agents get no SSH to it", row.ID))
	}
	cert, ttl, err := s.sign(ctx, c, row, key)
	if err != nil {
		return s.cloudError(c, tool, "error_tool", err)
	}
	s.auditTool(c, tool, "allowed", fmt.Sprintf("certificate for %s@%s (resource %d), %s", loginUser, row.Address, row.ID, ttl), &c.task.SpentEUR, nil)
	return textResult(fmt.Sprintf("certificate for %s@%s, valid %s; save it next to your private key as <key>-cert.pub:\n%s", loginUser, row.Address, ttl, cert))
}

// sign certifies key for row's login user until the egress drops the
// server (the lease's cutoff), at most maxCertTTL.
func (s *Server) sign(ctx context.Context, c *toolCaller, row cloudres.Resource, key string) (string, time.Duration, error) {
	ttl := min(maxCertTTL, row.LeaseExpires.Add(-cloudres.LeaseCutoff).Sub(s.opts.Cloud.now()).Truncate(time.Second))
	if ttl < time.Minute {
		return "", 0, fmt.Errorf("server %d stays reachable for under a minute: its lease ends in under %s", row.ID, cloudres.LeaseCutoff+time.Minute)
	}
	cert, err := s.opts.Cloud.Signer.Sign(ctx, key, principal(row.ID), ttl, fmt.Sprintf("task=%s resource=%d", c.task.ID, row.ID))
	return cert, ttl, err
}

func (s *Server) requestStatus(ctx context.Context, c *toolCaller, in RequestStatusIn) *mcp.CallToolResult {
	tool := policy.ToolRequestStatus
	if r := s.admitCloud(c, tool); r != nil {
		return r
	}
	ap, ok, err := s.opts.Cloud.Store.Approval(ctx, in.RequestID, c.task.ID)
	if err != nil {
		return s.cloudError(c, tool, "error_store", err)
	}
	if !ok {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("approval request %d is not one of this task's resource requests", in.RequestID))
	}
	s.auditTool(c, tool, "allowed", fmt.Sprintf("request %d %s", ap.ID, ap.Status), &c.task.SpentEUR, nil)
	switch ap.Status {
	case "approved":
		return textResult(fmt.Sprintf("approval request %d approved: call commission_server again with the same arguments", ap.ID))
	case "denied":
		return textResult(fmt.Sprintf("approval request %d denied: %s. Work within the envelope.", ap.ID, ap.Note))
	}
	return textResult(fmt.Sprintf("approval request %d is pending: a human has not decided yet; poll again later or continue with other work", ap.ID))
}

func (s *Server) cloudError(c *toolCaller, tool, decision string, err error) *mcp.CallToolResult {
	s.auditTool(c, tool, decision, err.Error(), &c.task.SpentEUR, nil)
	return toolError(tool + " failed: " + err.Error())
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
