//go:build unix

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/health"
)

// listening is a local address that accepts connections: a target the
// sandbox reaches.
func listening(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Addr().String()
}

// refusing is a local address that refuses connections. Its port is
// privileged, so no test's listener takes it: a freed ephemeral port goes
// to whichever process binds next, and a console test's fake model server
// took this probe's GET /healthz there.
const refusing = "127.0.0.1:1"

// restarting is the URL of a gateway that answers 503, as while it
// restarts.
func restarting(t *testing.T) string {
	t.Helper()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(gateway.Close)
	return gateway.URL
}

func refuseLookup(_ context.Context, name string) ([]string, error) {
	return nil, &net.DNSError{Err: "server misbehaving", Name: name}
}

// closedSandbox is the probe of a sandbox exactly as open as its policy:
// the gateway answers, every other target refuses, the DNS refuses the
// blocked name, and the uid is the sandbox's.
func closedSandbox(t *testing.T) egressProbe {
	t.Helper()
	gateway := httptest.NewServer(health.Handler("factory-gateway", "test"))
	t.Cleanup(gateway.Close)
	return egressProbe{
		gateway: gateway.URL, canary: refusing, blocked: "canary.example", metadata: refusing, directIP: refusing,
		uid: factoryd.SandboxUID, timeout: time.Second, dial: (&net.Dialer{}).DialContext, lookup: refuseLookup,
	}
}

// A sandbox as closed as its policy passes, and fast: a refused
// connection or name ends its probe at once, not at the bound.
func TestEgressProbePassesAClosedSandbox(t *testing.T) {
	p := closedSandbox(t)
	began := time.Now()
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	t.Logf("the probe of a closed sandbox took %v", took)
	if took >= p.timeout {
		t.Fatalf("the probe took %v, its bound %v", took, p.timeout)
	}
}

// Every probe that finds the sandbox more open than its policy, or the
// gateway not answering, fails the probe with its name and what answered,
// in the probes' order.
func TestEgressProbeNamesWhatAnswered(t *testing.T) {
	for name, open := range map[string]func(t *testing.T, p *egressProbe) string{
		"canary": func(t *testing.T, p *egressProbe) string {
			p.canary = listening(t)
			return "canary " + p.canary + " answered: connected to " + p.canary
		},
		"metadata": func(t *testing.T, p *egressProbe) string {
			p.metadata = listening(t)
			return "metadata " + p.metadata + " answered: connected to " + p.metadata
		},
		"direct IP": func(t *testing.T, p *egressProbe) string {
			p.directIP = listening(t)
			return "direct IP " + p.directIP + " answered: connected to " + p.directIP
		},
		"blocked name": func(t *testing.T, p *egressProbe) string {
			p.lookup = func(context.Context, string) ([]string, error) { return []string{"192.0.2.7", "2001:db8::7"}, nil }
			return "blocked name canary.example resolved to 192.0.2.7, 2001:db8::7"
		},
		"root": func(t *testing.T, p *egressProbe) string {
			p.uid = 0
			return "uid: runs as 0, the sandbox's is 1000"
		},
		"another uid": func(t *testing.T, p *egressProbe) string {
			p.uid = 1001
			return "uid: runs as 1001, the sandbox's is 1000"
		},
		"gateway unhealthy": func(t *testing.T, p *egressProbe) string {
			gateway := restarting(t)
			p.gateway = gateway + "/"
			return "gateway: " + gateway + "/healthz answered 503 Service Unavailable"
		},
		"three at once": func(t *testing.T, p *egressProbe) string {
			p.canary, p.directIP, p.uid = listening(t), listening(t), 0
			return "canary " + p.canary + " answered: connected to " + p.canary +
				"; direct IP " + p.directIP + " answered: connected to " + p.directIP +
				"; uid: runs as 0, the sandbox's is 1000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := closedSandbox(t)
			want := open(t, &p)
			if err := p.run(context.Background()); err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
		})
	}
}

// Each probe is bounded and they run at once: with every target silent, a
// dropped connection passes, a lookup or a gateway that does not answer
// within the bound fails with it, and the probe takes one bound, not one
// a probe.
func TestEgressProbeBoundsEachProbe(t *testing.T) {
	silent := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	p := egressProbe{
		gateway: "http://factory-gateway.factory.svc.cluster.local:8081", canary: "canary.example:443", blocked: "canary.example",
		metadata: metadataAddr, directIP: "192.0.2.7:443", uid: factoryd.SandboxUID, timeout: 300 * time.Millisecond,
		dial:   func(ctx context.Context, _, _ string) (net.Conn, error) { return nil, silent(ctx) },
		lookup: func(ctx context.Context, _ string) ([]string, error) { return nil, silent(ctx) },
	}
	began := time.Now()
	err := p.run(context.Background())
	took := time.Since(began)
	t.Logf("the probe of a silent sandbox took %v", took)
	want := "gateway: no answer from http://factory-gateway.factory.svc.cluster.local:8081/healthz within 300ms" +
		"; blocked name canary.example: no answer within 300ms, where the allowlist refuses at once"
	if err == nil || err.Error() != want || !errors.As(err, new(transient)) {
		t.Fatalf("err = %#v\nwant the transient %s", err, want)
	}
	if took < p.timeout || took >= 2*p.timeout {
		t.Fatalf("the probe took %v, want one bound of %v", took, p.timeout)
	}
}

// A service the sandbox needs that does not answer, as while it restarts,
// fails the probe for a later start to pass; a sandbox more open than its
// policy, or not the sandbox's uid, fails it for good, even beside one.
func TestEgressProbeRetriesOnlyAServiceThatDidNotAnswer(t *testing.T) {
	silent := func(ctx context.Context, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for name, tc := range map[string]struct {
		open      func(t *testing.T, p *egressProbe)
		transient bool
	}{
		"gateway restarting": {func(t *testing.T, p *egressProbe) { p.gateway = restarting(t) }, true},
		"gateway down":       {func(t *testing.T, p *egressProbe) { p.gateway = "http://" + refusing }, true},
		"DNS silent":         {func(t *testing.T, p *egressProbe) { p.lookup = silent }, true},
		"canary open":        {func(t *testing.T, p *egressProbe) { p.canary = listening(t) }, false},
		"root":               {func(t *testing.T, p *egressProbe) { p.uid = 0 }, false},
		"gateway restarting, direct IP open": {func(t *testing.T, p *egressProbe) {
			p.gateway, p.directIP = restarting(t), listening(t)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			p := closedSandbox(t)
			p.timeout = 100 * time.Millisecond
			tc.open(t, &p)
			err := p.run(context.Background())
			if err == nil || errors.As(err, new(transient)) != tc.transient {
				t.Fatalf("err = %#v, want one transient %v", err, tc.transient)
			}
		})
	}
}

// The canary is a name, its name the DNS probe's, and the direct IP an
// IP, each with a port; the probe holds the sandbox to them, the metadata
// address and the sandbox's uid.
func TestLoadEnvReadsTheEgressProbeTargets(t *testing.T) {
	setRequiredEnv(t)
	e, err := loadEnv()
	if err != nil {
		t.Fatal(err)
	}
	p := sandboxProbe(e)
	if p.gateway != "http://gw" || p.canary != "example.com:443" || p.blocked != "example.com" ||
		p.metadata != "169.254.169.254:80" || p.directIP != "192.0.2.7:443" || p.timeout != 2*time.Second {
		t.Fatalf("probe = %+v", p)
	}
	for name, values := range map[string][]string{
		"EGRESS_CANARY":    {"", "example.com", ":443", "example.com:https", "example.com:0", "example.com:65536", "192.0.2.7:443", "[2001:db8::7]:443"},
		"EGRESS_DIRECT_IP": {"", "192.0.2.7", "192.0.2.7:x", "one.one.one.one:443"},
	} {
		for _, v := range values {
			setRequiredEnv(t)
			t.Setenv(name, v)
			if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v", name, v, err)
			}
		}
	}
}
