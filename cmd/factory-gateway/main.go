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

	ctx := context.Background()
	store, err := gateway.OpenStore(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open task store: %v", err)
	}
	auth, err := gateway.NewAuth(gateway.AuthOptions{
		Audience:   cfg.Audience,
		FetchBase:  gateway.InClusterFetchBase,
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
