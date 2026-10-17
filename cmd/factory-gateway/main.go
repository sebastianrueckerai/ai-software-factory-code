package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/openbao"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

var version = "dev"

func main() {
	cfg := gateway.ConfigFromEnv()
	if cfg.LiteLLMURL == "" || cfg.LiteLLMMasterKey == "" {
		log.Fatal("LITELLM_URL and LITELLM_MASTER_KEY are required")
	}
	if cfg.FirecrawlKey == "" || cfg.ReplicateKey == "" || cfg.FalKey == "" || cfg.FactorydToken == "" || cfg.HetznerAgentKey == "" {
		log.Fatal("FIRECRAWL_KEY, REPLICATE_KEY, FAL_AI_KEY, FACTORYD_TOKEN and HETZNER_AGENT_KEY are required")
	}
	sshSources, err := cloudres.IPv4Sources(strings.Split(cfg.AgentSSHSources, ","))
	if err != nil {
		log.Fatalf("AGENT_SSH_SOURCES: %v", err)
	}

	// SIGTERM (a roll) drains the server and flushes the telemetry within
	// the pod's grace period
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	otelProviders, err := telemetry.Setup(ctx, "factory-gateway")
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	tel, err := gateway.NewTelemetry(otelProviders.Tracer, otelProviders.Meter, otelProviders.Logger,
		cfg.LiteLLMMasterKey, cfg.FirecrawlKey, cfg.ReplicateKey, cfg.FalKey, cfg.FactorydToken, cfg.HetznerAgentKey)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	pg, err := gateway.OpenStore(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open task store: %v", err)
	}
	// every audit row goes to Postgres and, as a log record, to OTel
	store := tel.Audited(pg)
	auth, err := gateway.NewAuth(gateway.AuthOptions{
		Audience:   cfg.Audience,
		FetchBase:  gateway.InClusterFetchBase(),
		CACertPath: gateway.InClusterCACertPath, // missing bundle fails boot above
	})
	if err != nil {
		log.Fatalf("init auth: %v", err)
	}
	// the agent Hetzner project holds only commissioned resources: its
	// collector deletes whatever the registry does not know
	hetzner := hcloud.New(cfg.HetznerAgentKey)
	registry := cloudres.PG{DB: pg.DB()}
	cloud := &gateway.Cloud{
		Provider: hetzner,
		Store:    cloudres.AgentStore{DB: pg.DB()},
		Registry: registry,
		Collector: &cloudres.Collector{Project: cloudres.AgentProject, Registry: registry, Provider: hetzner,
			OwnerDone: gateway.TaskOwnerDone(store), NodeIPs: sshSources, Interval: cloudres.AgentInterval},
		Signer: &openbao.Client{BaseURL: cfg.OpenBaoURL, AuthRole: "factory-gateway", SSHRole: "factory-agent",
			TokenFile: openbao.TokenPath, HTTP: &http.Client{Timeout: 15 * time.Second}},
		Sources: sshSources,
		Audit:   store,
	}
	lite := &gateway.LiteLLM{BaseURL: cfg.LiteLLMURL, MasterKey: cfg.LiteLLMMasterKey}
	factoryd := &gateway.Factoryd{BaseURL: cfg.FactorydURL, Token: cfg.FactorydToken, HTTP: &http.Client{Timeout: 30 * time.Second}}
	// the tool connectors' clients dial through the egress guard (roadmap
	// 42): every connection on an agent's behalf resolves once, refuses
	// non-public addresses and dials the address it checked; LiteLLM,
	// Factoryd and the in-cluster clients keep their plain clients
	egress := &gateway.EgressGuard{Audit: store}
	srv := gateway.NewServer(gateway.ServerOptions{
		Auth:       auth,
		Store:      store,
		Minter:     lite,
		Spend:      lite,
		Prices:     &policy.PriceBook{BaseURL: cfg.LiteLLMURL, MasterKey: cfg.LiteLLMMasterKey},
		LiteLLMURL: cfg.LiteLLMURL,
		Firecrawl: &gateway.Firecrawl{BaseURL: "https://api.firecrawl.dev", Key: cfg.FirecrawlKey,
			HTTP: egress.Client(90 * time.Second)},
		// Prefer: wait (at most 60 s) bounds the create call inside the client
		// timeout; the cancel and the read after it are timed per request
		Replicate: &gateway.Replicate{BaseURL: "https://api.replicate.com", Key: cfg.ReplicateKey,
			HTTP: egress.Client(75 * time.Second)},
		Fal: &gateway.Fal{RunURL: "https://fal.run", APIURL: "https://api.fal.ai", Key: cfg.FalKey,
			HTTP: egress.Client(120 * time.Second)},
		Escalator:        factoryd,
		PlanFiler:        factoryd,
		HoldoutVerdicter: factoryd,
		Delegator:        factoryd,
		Cloud:            cloud,
		Telemetry:        tel,
		FactorydToken:    cfg.FactorydToken,
	})
	go cloud.Run(ctx)

	addr := ":" + cfg.Port
	log.Printf("factory-gateway %s listening on %s", version, addr)
	calls, endCalls := context.WithCancel(context.Background())
	server := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(version),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return calls },
	}
	go func() {
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	drainCalls(server, endCalls, callDrain, callSettle)
	flush, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFlush()
	if err := otelProviders.Shutdown(flush); err != nil {
		log.Printf("telemetry shutdown: %v", err)
	}
}

// callDrain is how long a roll lets the calls in flight run on: the
// longest model call seen ran 200 s, and the pod's grace period
// (k8s/deployment.yaml) leaves room past it. callSettle is how long a call
// cut at the drain's end takes to book its hold and write its audit row.
const (
	callDrain  = 270 * time.Second
	callSettle = 10 * time.Second
)

// drainCalls ends server on a roll: new calls go to the next pod, the calls
// in flight run on for up to drain, and any still running past it are
// cancelled (endCalls ends their request context), so each one's handler
// books its hold and audits the call within settle instead of dying with
// the process and leaving the hold for the sweep to book in full.
func drainCalls(server *http.Server, endCalls context.CancelFunc, drain, settle time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := server.Shutdown(ctx); err == nil {
		return
	}
	log.Printf("shutdown: calls still in flight after %s: cancelling them", drain)
	endCalls()
	ctx, cancel = context.WithTimeout(context.Background(), settle)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
