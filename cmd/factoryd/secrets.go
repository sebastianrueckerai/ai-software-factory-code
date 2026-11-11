package main

import (
	"log"
	"net/url"
	"strings"
)

// daemonSecrets are the credentials factoryd loads at boot, by env name
// and whether the daemon needs them. The daemon's scrubber and the
// commands that print text built from its records (export-fixture) load
// this one list, so no secret is scrubbed in one and missed in the other.
var daemonSecrets = []struct {
	name     string
	required bool
}{
	{"FACTORYD_DATABASE_URL", true},
	{"S3_ACCESS_KEY_ID", true},
	{"S3_SECRET_ACCESS_KEY", true},
	{"LITELLM_MASTER_KEY", true},
	{"GATEWAY_TOKEN", true},
	{"SIGNOZ_PASSWORD", true},
	{"HETZNER_CI_KEY", true},
	{"INTAKE_TOKEN", true},
	{"FORGEJO_WEBHOOK_SECRET", true},
	{"SIGNOZ_WEBHOOK_PASSWORD", true},
	{"CONSOLE_CLIENT_SECRET", true},
	{"CONSOLE_SESSION_KEY", true},
	{"REGISTRY_READ_TOKEN", true},
	{"FORGEJO_BOT_PASSWORD", true},
	{"FORGEJO_CI_ORG_TOKEN", true},
	{"FORGEJO_PROJECTS_TOKEN", true},
	{"FORGEJO_CI_REGISTRAR_TOKEN", true},
	{"FORGEJO_MERGE_TOKEN", true},
	{"FORGEJO_CONFIG_TOKEN", true},
	{"FORGEJO_GATE_TOKEN", true},
	{"FORGEJO_CHAT_TOKEN", true},
	{"TELEGRAM_BOT_KEY", false},
	{"DISCORD_CLIENT_SECRET", false},
	{"DISCORD_BOT_TOKEN", false},
	{"FORGEJO_DOCS_TOKEN", false},
}

// loadSecrets loads daemonSecrets and registers each for scrubbing. A
// strict load stops the process on a required one that is missing; a
// lenient load takes what is set, for a command that needs few of them.
func loadSecrets(strict bool) map[string]string {
	out := make(map[string]string, len(daemonSecrets))
	for _, s := range daemonSecrets {
		if strict && s.required {
			out[s.name] = mustSecret(s.name)
		} else {
			out[s.name] = optionalSecret(s.name)
		}
	}
	return out
}

// daemonURLs are the endpoints factoryd reaches, by env name, with the
// default each has when the variable is unset. S3_ENDPOINT and the
// telemetry endpoint have none.
var daemonURLs = map[string]string{
	"CONSOLE_PUBLIC_URL":          "https://console.aisoftwarefactory.world",
	"FACTORYD_WEBHOOK_URL":        "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo",
	"FORGEJO_PUBLIC_URL":          "https://git.aisoftwarefactory.world",
	"FORGEJO_SSH_ADDR":            "forgejo-ssh.forgejo.svc.cluster.local:22",
	"FORGEJO_URL":                 "https://git.aisoftwarefactory.world",
	"GATEWAY_URL":                 "http://factory-gateway.factory.svc.cluster.local:8081",
	"LITELLM_URL":                 "http://litellm.platform.svc:4000",
	"OPENBAO_URL":                 "http://openbao.platform.svc.cluster.local:8200",
	"REGISTRY_HOST":               "git.aisoftwarefactory.world",
	"SIGNOZ_URL":                  "http://signoz.platform.svc.cluster.local:8080",
	"WORKER_GIT_BASE":             "http://forgejo-http.forgejo.svc.cluster.local:3000",
	"EGRESS_CANARY":               "example.com:443",
	"EGRESS_DIRECT_IP":            "1.1.1.1:443",
	"TEMPORAL_ADDRESS":            "temporal-frontend.platform.svc.cluster.local:7233",
	"S3_ENDPOINT":                 "",
	"OTEL_EXPORTER_OTLP_ENDPOINT": "",
}

// daemonURL is the endpoint env name configures, else its default.
func daemonURL(name string) string { return env(name, daemonURLs[name]) }

// mustURL is daemonURL for an endpoint with no default: the process stops
// when it is unset.
func mustURL(name string) string {
	v := daemonURL(name)
	if v == "" {
		log.Fatalf("%s is required", name)
	}
	return v
}

// daemonHosts are the host names of every endpoint factoryd is configured
// with, the terms a published fixture must not carry.
func daemonHosts() []string {
	var hosts []string
	for name := range daemonURLs {
		v := daemonURL(name)
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "//" + v
		}
		if u, err := url.Parse(v); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}
