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
}

func ConfigFromEnv() Config {
	return Config{
		Audience:         getEnv("GATEWAY_AUDIENCE", defaultAudience),
		LiteLLMURL:       os.Getenv("LITELLM_URL"),
		LiteLLMMasterKey: os.Getenv("LITELLM_MASTER_KEY"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		Port:             getEnv("PORT", "8081"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
