package gateway

import "testing"

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{
			name: "defaults",
			want: Config{Audience: "factory-gateway", Port: "8081", FactorydURL: "http://factoryd.factory.svc.cluster.local:8080"},
		},
		{
			name: "all set",
			env: map[string]string{
				"GATEWAY_AUDIENCE":   "other-aud",
				"LITELLM_URL":        "http://litellm.litellm.svc.cluster.local:4000",
				"LITELLM_MASTER_KEY": "sk-master",
				"DATABASE_URL":       "postgres://user:pass@db:5432/gateway",
				"PORT":               "9090",
				"FIRECRAWL_KEY":      "fc",
				"REPLICATE_KEY":      "r8",
				"FAL_AI_KEY":         "fal",
				"FACTORYD_URL":       "http://fd:8080",
				"FACTORYD_TOKEN":     "tok",
			},
			want: Config{
				Audience:         "other-aud",
				LiteLLMURL:       "http://litellm.litellm.svc.cluster.local:4000",
				LiteLLMMasterKey: "sk-master",
				DatabaseURL:      "postgres://user:pass@db:5432/gateway",
				Port:             "9090",
				FirecrawlKey:     "fc",
				ReplicateKey:     "r8",
				FalKey:           "fal",
				FactorydURL:      "http://fd:8080",
				FactorydToken:    "tok",
			},
		},
		{
			name: "empty database url falls through to pgx pg vars",
			env:  map[string]string{"PGHOST": "db-host"},
			want: Config{Audience: "factory-gateway", Port: "8081", FactorydURL: "http://factoryd.factory.svc.cluster.local:8080"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := ConfigFromEnv(); got != tt.want {
				t.Fatalf("ConfigFromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
