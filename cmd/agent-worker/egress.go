//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

const (
	// probeTimeout bounds each egress probe. The policy drops what it
	// denies, so the metadata and direct IP probes take all of it on every
	// start; the probes run at once, so the start pays it once.
	probeTimeout = 2 * time.Second
	// metadataAddr is the cloud metadata service the policy denies outright.
	metadataAddr = "169.254.169.254:80"
)

// egressProbe holds the sandbox to its egress policy (factoryd's
// workerEgressSpec) before any agent runs in it: the gateway, the one path
// the policy admits, answers, and nothing else does. A refused, dropped or
// unresolved connection passes and only an answer fails, so no service
// outside the factory has to answer for a session to start. A gateway or
// a DNS that does not answer is transient; a sandbox more open than its
// policy, or not the sandbox's uid, is not.
type egressProbe struct {
	gateway  string // the gateway's base URL
	canary   string // host:port, a public name outside the DNS allowlist
	blocked  string // a name outside the DNS allowlist, the canary's
	metadata string // ip:port
	directIP string // ip:port, a public address no rule admits
	uid      int    // the process's
	timeout  time.Duration
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	lookup   func(ctx context.Context, host string) ([]string, error)
}

// sandboxProbe is the probe of the sandbox e runs in.
func sandboxProbe(e workerEnv) egressProbe {
	name, _, _ := net.SplitHostPort(e.egressCanary) // checked in loadEnv
	return egressProbe{
		gateway: e.gatewayURL, canary: e.egressCanary, blocked: name, metadata: metadataAddr, directIP: e.egressDirectIP,
		uid: os.Geteuid(), timeout: probeTimeout,
		dial: (&net.Dialer{}).DialContext, lookup: net.DefaultResolver.LookupHost,
	}
}

// run runs every probe at once, each bounded by p.timeout; its error names
// each probe that found the sandbox other than its policy, and what
// answered.
func (p egressProbe) run(ctx context.Context) error {
	probes := []func(context.Context) error{
		p.answers,
		func(ctx context.Context) error { return p.closed(ctx, "canary", p.canary) },
		func(ctx context.Context) error { return p.closed(ctx, "metadata", p.metadata) },
		func(ctx context.Context) error { return p.closed(ctx, "direct IP", p.directIP) },
		p.refused,
		func(context.Context) error { return checkUID(p.uid) },
	}
	errs := make([]error, len(probes))
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, p.timeout)
			defer cancel()
			errs[i] = probe(ctx)
		})
	}
	wg.Wait()
	var failed []string
	permanent := false
	for _, err := range errs {
		if err != nil {
			failed = append(failed, err.Error())
			permanent = permanent || !errors.As(err, new(transient))
		}
	}
	switch {
	case len(failed) == 0:
		return nil
	case permanent:
		return errors.New(strings.Join(failed, "; "))
	}
	return transient{errors.New(strings.Join(failed, "; "))}
}

// answers is the gateway probe: without its answer the sandbox may have
// no network at all, and the other probes would prove nothing.
func (p egressProbe) answers(ctx context.Context) error {
	health := strings.TrimSuffix(p.gateway, "/") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health, nil)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: p.dial, DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return transient{fmt.Errorf("gateway: no answer from %s within %v", health, p.timeout)}
		}
		return transient{fmt.Errorf("gateway: no answer from %s: %w", health, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return transient{fmt.Errorf("gateway: %s answered %s", health, resp.Status)}
	}
	return nil
}

// closed is the probe called name of addr, which no rule admits.
func (p egressProbe) closed(ctx context.Context, name, addr string) error {
	conn, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return nil
	}
	defer conn.Close()
	return fmt.Errorf("%s %s answered: connected to %s", name, addr, conn.RemoteAddr())
}

// refused is the DNS probe: the allowlist refuses a name outside it at
// once, so no lookup an agent makes hangs.
func (p egressProbe) refused(ctx context.Context) error {
	addrs, err := p.lookup(ctx, p.blocked)
	switch {
	case err == nil:
		return fmt.Errorf("blocked name %s resolved to %s", p.blocked, strings.Join(addrs, ", "))
	case ctx.Err() != nil:
		return transient{fmt.Errorf("blocked name %s: no answer within %v, where the allowlist refuses at once", p.blocked, p.timeout)}
	}
	return nil
}

func checkUID(uid int) error {
	if uid != factoryd.SandboxUID {
		return fmt.Errorf("uid: runs as %d, the sandbox's is %d", uid, factoryd.SandboxUID)
	}
	return nil
}
