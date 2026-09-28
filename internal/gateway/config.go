package gateway

import "os"

// Config is the gateway's environment. DATABASE_URL may be empty: pgx then
// assembles the connection from the PG* variables (PGHOST, PGPORT, ...).
type Config struct {
	Audience         string
	LiteLLMURL       string
	LiteLLMMasterKey string
	DatabaseURL      string
	Port             string
	// the tool connectors' keys (secret factory/gateway-tool-keys) and
	// factoryd's internal endpoint with its bearer (factory/gateway-factoryd)
	FirecrawlKey  string
	ReplicateKey  string
	FalKey        string
	FactorydURL   string
	FactorydToken string
}

func ConfigFromEnv() Config {
	return Config{
		Audience:         getEnv("GATEWAY_AUDIENCE", defaultAudience),
		LiteLLMURL:       os.Getenv("LITELLM_URL"),
		LiteLLMMasterKey: os.Getenv("LITELLM_MASTER_KEY"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		Port:             getEnv("PORT", "8081"),
		FirecrawlKey:     os.Getenv("FIRECRAWL_KEY"),
		ReplicateKey:     os.Getenv("REPLICATE_KEY"),
		FalKey:           os.Getenv("FAL_AI_KEY"),
		FactorydURL:      getEnv("FACTORYD_URL", "http://factoryd.factory.svc.cluster.local:8080"),
		FactorydToken:    os.Getenv("FACTORYD_TOKEN"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
