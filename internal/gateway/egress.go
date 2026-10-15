package gateway

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// The gateway's half of the egress guards (roadmap 42): one dialer every
// connection the gateway makes on an agent's behalf goes through. It
// resolves the name once, refuses everything that is not a checked public
// address and dials the address it checked, so a second resolution (DNS
// rebinding) cannot change the target.
//
// The guarded clients are the MCP tool connectors (Firecrawl, Replicate,
// Fal): every connection they make serves an agent's tool call, and the
// agent's arguments steer the request (today only validated path parts,
// and the guard is in place the day a connector fetches an address an
// agent chose; a provider's redirect is steered the same way). The calls
// to LiteLLM, whose address is the gateway's configuration and not an
// agent's choice, stay as they are; so do Factoryd and the JWKS fetch,
// which reach in-cluster services on private addresses, and the cloud
// connector's provider clients, whose addresses are configuration too.

// RefusedEgress is the audit decision of a connection the egress guard
// refused: the reason names the host, the address and the rule.
const RefusedEgress = "refused_egress"

// metadataIPs are the cloud metadata services' addresses, refused by
// their own rule before the broader link-local and private rules they
// sit in.
var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),
	net.ParseIP("fd00:ec2::254"),
}

// cgnat is the shared-address space 100.64/10, which no net.IP rule
// covers.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// egressRule names the rule ip breaks; "" = a checked public address.
func egressRule(ip net.IP) string {
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return "the metadata address " + m.String()
		}
	}
	switch {
	case ip.IsLoopback():
		return "loopback (127/8, ::1)"
	case ip.IsPrivate():
		return "private (10/8, 172.16/12, 192.168/16, fc00::/7)"
	case ip.IsLinkLocalUnicast():
		return "link-local (169.254/16, fe80::/10)"
	case cgnat.Contains(ip):
		return "CGNAT (100.64/10)"
	case ip.IsUnspecified():
		return "unspecified (0.0.0.0, ::)"
	case ip.IsMulticast():
		return "multicast (224/4, ff00::/8)"
	}
	return ""
}

// refusalReason is a refusal's record: the host, the address and the rule.
func refusalReason(host string, ip net.IP, rule string) string {
	return fmt.Sprintf("host %s address %s refused: %s", host, ip, rule)
}

// EgressGuard is the gateway's one dialer for connections made on an
// agent's behalf: DialContext resolves once, checks and dials the checked
// IP. Tests substitute Resolve and Dial.
type EgressGuard struct {
	// Resolve resolves a host name once; nil = the net resolver.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// Dial dials a checked address; nil = the net dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Audit records a refusal in the gateway's audit; nil = no record.
	Audit interface {
		InsertAudit(ctx context.Context, row AuditRow) error
	}
}

// egressDialer is the dialer under the guard, with the default transport's
// own timeouts.
var egressDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

func (g *EgressGuard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if g.Resolve != nil {
		return g.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func (g *EgressGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if g.Dial != nil {
		return g.Dial(ctx, network, addr)
	}
	return egressDialer.DialContext(ctx, network, addr)
}

// DialContext resolves addr's host once (an IP literal is checked as
// itself), refuses the connection when any address a rule names is among
// the answers, and dials the address it checked, never the name again.
func (g *EgressGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = g.resolve(ctx, host); err != nil {
		return nil, fmt.Errorf("egress: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: host %s resolves to no address", host)
	}
	// one forbidden answer refuses the whole connection: a name answering
	// both public and private is the rebinding the guard exists for
	for _, ip := range ips {
		if rule := egressRule(ip); rule != "" {
			g.refuse(ctx, host, ip, rule)
			return nil, fmt.Errorf("egress: %s", refusalReason(host, ip, rule))
		}
	}
	var derr error
	for _, ip := range ips {
		conn, err := g.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		derr = err
	}
	return nil, derr
}

// refuse records the refusal in the gateway's audit, naming the task
// whose call asked for the connection when its context names one.
func (g *EgressGuard) refuse(ctx context.Context, host string, ip net.IP, rule string) {
	if g.Audit == nil {
		return
	}
	row := AuditRow{Decision: RefusedEgress, Reason: refusalReason(host, ip, rule)}
	if c, _ := ctx.Value(callerKey{}).(*toolCaller); c != nil {
		row.TaskID = c.task.ID
	}
	if err := g.Audit.InsertAudit(context.WithoutCancel(ctx), row); err != nil {
		log.Printf("insert audit row (egress: %s): %v", row.Reason, err)
	}
}

// Client is an HTTP client whose every connection the guard checked, with
// the default transport's settings and the given overall timeout: the
// tool connectors' clients.
func (g *EgressGuard) Client(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = g.DialContext
	return &http.Client{Timeout: timeout, Transport: transport}
}
