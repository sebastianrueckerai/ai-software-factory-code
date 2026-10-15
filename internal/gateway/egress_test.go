package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// answers resolves every host to ips.
func answers(ips ...net.IP) func(context.Context, string) ([]net.IP, error) {
	return func(context.Context, string) ([]net.IP, error) { return ips, nil }
}

// scripted answers each lookup from ans in order, recording the lookups.
func scripted(ans ...[]net.IP) func(context.Context, string) ([]net.IP, error) {
	var mu sync.Mutex
	n := 0
	return func(context.Context, string) ([]net.IP, error) {
		mu.Lock()
		defer mu.Unlock()
		ips := ans[min(n, len(ans)-1)]
		n++
		return ips, nil
	}
}

// dialRecorder notes every address it is asked to dial and returns one
// end of a pipe.
type dialRecorder struct {
	mu     sync.Mutex
	dialed []string
}

func (d *dialRecorder) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	a, b := net.Pipe()
	a.Close()
	return b, nil
}

func (d *dialRecorder) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.dialed)
}

type memAudit struct {
	mu   sync.Mutex
	rows []AuditRow
}

func (a *memAudit) InsertAudit(_ context.Context, row AuditRow) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, row)
	return nil
}

// guarded is a guard that resolves by resolve and records what it dials.
func guarded(resolve func(context.Context, string) ([]net.IP, error)) (*EgressGuard, *dialRecorder) {
	d := &dialRecorder{}
	return &EgressGuard{Resolve: resolve, Dial: d.dial}, d
}

// TestEgressGuardRefusesNonPublicAddresses: a resolver that answers a
// loopback, private, metadata, link-local, CGNAT, unspecified or
// multicast address is refused, the rule named with the host and the
// address, and nothing is dialed (roadmap 42).
func TestEgressGuardRefusesNonPublicAddresses(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		rule string
	}{
		{"127.0.0.1", "loopback (127/8, ::1)"},
		{"::1", "loopback (127/8, ::1)"},
		{"10.0.0.5", "private (10/8, 172.16/12, 192.168/16, fc00::/7)"},
		{"172.16.0.9", "private (10/8, 172.16/12, 192.168/16, fc00::/7)"},
		{"192.168.1.1", "private (10/8, 172.16/12, 192.168/16, fc00::/7)"},
		{"fd12:3456:789a::1", "private (10/8, 172.16/12, 192.168/16, fc00::/7)"},
		{"169.254.169.254", "the metadata address 169.254.169.254"},
		{"fd00:ec2::254", "the metadata address fd00:ec2::254"},
		{"169.254.1.1", "link-local (169.254/16, fe80::/10)"},
		{"fe80::1", "link-local (169.254/16, fe80::/10)"},
		{"100.64.0.7", "CGNAT (100.64/10)"},
		{"100.127.255.254", "CGNAT (100.64/10)"},
		{"0.0.0.0", "unspecified (0.0.0.0, ::)"},
		{"::", "unspecified (0.0.0.0, ::)"},
		{"224.0.0.5", "multicast (224/4, ff00::/8)"},
		{"ff02::1", "multicast (224/4, ff00::/8)"},
	} {
		g, d := guarded(answers(net.ParseIP(tc.ip)))
		_, err := g.DialContext(context.Background(), "tcp", "evil.example:443")
		if err == nil {
			t.Fatalf("%s: dial succeeded", tc.ip)
		}
		for _, want := range []string{"evil.example", tc.ip, tc.rule} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: refusal %q does not name %q", tc.ip, err, want)
			}
		}
		if dials := d.addrs(); len(dials) > 0 {
			t.Fatalf("%s: dialed %v despite the refusal", tc.ip, dials)
		}
	}
}

// TestEgressGuardPassesAPublicAddress: a public address passes and the
// guard dials the address it checked, not the name.
func TestEgressGuardPassesAPublicAddress(t *testing.T) {
	g, d := guarded(answers(net.ParseIP("192.0.2.10")))
	if _, err := g.DialContext(context.Background(), "tcp", "provider.example:443"); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
	if dials := d.addrs(); !slices.Equal(dials, []string{"192.0.2.10:443"}) {
		t.Fatalf("dialed %v, want the checked IP 192.0.2.10:443 alone", dials)
	}

	g6, d6 := guarded(answers(net.ParseIP("2001:db8::1")))
	if _, err := g6.DialContext(context.Background(), "tcp", "provider.example:443"); err != nil {
		t.Fatalf("public IPv6 address refused: %v", err)
	}
	if dials := d6.addrs(); !slices.Equal(dials, []string{"[2001:db8::1]:443"}) {
		t.Fatalf("dialed %v, want the checked IP [2001:db8::1]:443 alone", dials)
	}
}

// TestEgressGuardAnswersRebindingWithNoConnection: a resolver that
// answers a public address first and a private one on a second lookup
// never leads to a connection to the private one (roadmap 42).
func TestEgressGuardAnswersRebindingWithNoConnection(t *testing.T) {
	g, d := guarded(scripted(
		[]net.IP{net.ParseIP("192.0.2.10")},
		[]net.IP{net.ParseIP("10.0.0.5")},
	))
	if _, err := g.DialContext(context.Background(), "tcp", "provider.example:443"); err != nil {
		t.Fatalf("the public first answer was refused: %v", err)
	}
	_, err := g.DialContext(context.Background(), "tcp", "provider.example:443")
	if err == nil || !strings.Contains(err.Error(), "10.0.0.5") || !strings.Contains(err.Error(), "private") {
		t.Fatalf("the private second answer was not refused naming it: %v", err)
	}
	if dials := d.addrs(); !slices.Equal(dials, []string{"192.0.2.10:443"}) {
		t.Fatalf("dialed %v, want the public first answer alone", dials)
	}
}

// TestEgressGuardRefusesAMixedAnswer: a name answering both a public and
// a forbidden address is refused whole, before any dial.
func TestEgressGuardRefusesAMixedAnswer(t *testing.T) {
	g, d := guarded(answers(net.ParseIP("192.0.2.10"), net.ParseIP("169.254.169.254")))
	_, err := g.DialContext(context.Background(), "tcp", "provider.example:443")
	if err == nil || !strings.Contains(err.Error(), "169.254.169.254") {
		t.Fatalf("mixed answer not refused on its metadata address: %v", err)
	}
	if dials := d.addrs(); len(dials) > 0 {
		t.Fatalf("dialed %v despite the refusal", dials)
	}
}

// TestEgressGuardChecksIPLiterals: an IP literal is checked as itself,
// without a lookup; a public literal is dialed as itself.
func TestEgressGuardChecksIPLiterals(t *testing.T) {
	resolver := func(context.Context, string) ([]net.IP, error) {
		t.Fatal("an IP literal must not be resolved")
		return nil, nil
	}
	for _, addr := range []string{"127.0.0.1:80", "[::1]:443", "[fd00:ec2::254]:443", "10.0.0.5:8080"} {
		g, d := guarded(resolver)
		_, err := g.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			t.Fatalf("%s: dial succeeded", addr)
		}
		if !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "the metadata address") && !strings.Contains(err.Error(), "private") {
			t.Fatalf("%s: refusal %q names no rule", addr, err)
		}
		if dials := d.addrs(); len(dials) > 0 {
			t.Fatalf("%s: dialed %v despite the refusal", addr, dials)
		}
	}
	g, d := guarded(resolver)
	if _, err := g.DialContext(context.Background(), "tcp", "192.0.2.99:443"); err != nil {
		t.Fatalf("public literal refused: %v", err)
	}
	if dials := d.addrs(); !slices.Equal(dials, []string{"192.0.2.99:443"}) {
		t.Fatalf("dialed %v, want the literal itself", dials)
	}
}

// TestEgressRefusalIsAudited: a refusal is recorded in the gateway's
// audit, naming the host, the address and the rule, under the task whose
// call asked for the connection.
func TestEgressRefusalIsAudited(t *testing.T) {
	audit := &memAudit{}
	g := &EgressGuard{Resolve: answers(net.ParseIP("10.0.0.5")), Dial: (&dialRecorder{}).dial, Audit: audit}
	if _, err := g.DialContext(context.Background(), "tcp", "evil.example:443"); err == nil {
		t.Fatal("dial succeeded")
	}
	rows := audit.rows
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one refusal", rows)
	}
	row := rows[0]
	if row.Decision != RefusedEgress || row.TaskID != "" || row.Kind != "" {
		t.Fatalf("row = %+v", row)
	}
	for _, want := range []string{"evil.example", "10.0.0.5", "private"} {
		if !strings.Contains(row.Reason, want) {
			t.Fatalf("reason %q does not name %q", row.Reason, want)
		}
	}

	ctx := context.WithValue(context.Background(), callerKey{}, &toolCaller{task: Task{ID: "t-1"}})
	if _, err := g.DialContext(ctx, "tcp", "evil.example:443"); err == nil {
		t.Fatal("dial succeeded")
	}
	if rows = audit.rows; len(rows) != 2 || rows[1].TaskID != "t-1" {
		t.Fatalf("audit rows = %+v, want the second under task t-1", rows)
	}
}

// TestEgressGuardClientChecksItsConnections: the client the guard builds
// checks every connection it makes, a loopback one included.
func TestEgressGuardClientChecksItsConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	client := (&EgressGuard{}).Client(time.Second)
	_, err := client.Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "loopback") || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("a client of the guard connected to %s: %v", srv.URL, err)
	}
}
