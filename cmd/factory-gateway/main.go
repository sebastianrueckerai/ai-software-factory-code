package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var version = "dev"

func main() {
	cfg := gateway.ConfigFromEnv()
	if cfg.LiteLLMURL == "" || cfg.LiteLLMMasterKey == "" {
		log.Fatal("LITELLM_URL and LITELLM_MASTER_KEY are required")
	}
	if cfg.FirecrawlKey == "" || cfg.ReplicateKey == "" || cfg.FalKey == "" || cfg.FactorydToken == "" {
		log.Fatal("FIRECRAWL_KEY, REPLICATE_KEY, FAL_AI_KEY and FACTORYD_TOKEN are required")
	}

	ctx := context.Background()
	store, err := gateway.OpenStore(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open task store: %v", err)
	}
	auth, err := gateway.NewAuth(gateway.AuthOptions{
		Audience:   cfg.Audience,
		FetchBase:  gateway.InClusterFetchBase(),
		CACertPath: gateway.InClusterCACertPath, // missing bundle fails boot above
	})
	if err != nil {
		log.Fatalf("init auth: %v", err)
	}
	lite := &gateway.LiteLLM{BaseURL: cfg.LiteLLMURL, MasterKey: cfg.LiteLLMMasterKey}
	srv := gateway.NewServer(gateway.ServerOptions{
		Auth:       auth,
		Store:      store,
		Minter:     lite,
		Prices:     &policy.PriceBook{BaseURL: cfg.LiteLLMURL, MasterKey: cfg.LiteLLMMasterKey},
		LiteLLMURL: cfg.LiteLLMURL,
		Firecrawl: &gateway.Firecrawl{BaseURL: "https://api.firecrawl.dev", Key: cfg.FirecrawlKey,
			HTTP: &http.Client{Timeout: 90 * time.Second}},
		// Prefer: wait 60 s, then a cancel and a read: the client timeout and
		// the policy's max_seconds cover both
		Replicate: &gateway.Replicate{BaseURL: "https://api.replicate.com", Key: cfg.ReplicateKey, Wait: 60 * time.Second,
			HTTP: &http.Client{Timeout: 75 * time.Second}},
		Fal: &gateway.Fal{RunURL: "https://fal.run", APIURL: "https://api.fal.ai", Key: cfg.FalKey,
			HTTP: &http.Client{Timeout: 120 * time.Second}},
		Escalator: &gateway.Factoryd{BaseURL: cfg.FactorydURL, Token: cfg.FactorydToken, HTTP: &http.Client{Timeout: 30 * time.Second}},
	})

	addr := ":" + cfg.Port
	log.Printf("factory-gateway %s listening on %s", version, addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(version),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
