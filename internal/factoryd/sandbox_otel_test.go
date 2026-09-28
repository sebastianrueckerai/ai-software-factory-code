package factoryd

import (
	"reflect"
	"testing"
)

const testCollector = "http://signoz-otel-collector.platform.svc.cluster.local:4318"

// Ruling 158: with a collector configured, the sandbox gains exactly one
// allow, SigNoz's collector pods on 4318, and its name in the DNS allowlist.
func TestWorkerEgressAdmitsTheCollectorOn4318Only(t *testing.T) {
	cfg := testRunnerConfig()
	without := (&Runner{cfg: cfg}).workerEgressSpec(nil)["egress"].([]any)
	cfg.OTLPEndpoint = testCollector
	with := (&Runner{cfg: cfg}).workerEgressSpec(nil)["egress"].([]any)
	if len(with) != len(without)+1 {
		t.Fatalf("%d egress rules, want %d", len(with), len(without)+1)
	}
	want := map[string]any{
		"toEndpoints": []any{map[string]any{
			"matchLabels": map[string]string{
				"k8s:io.kubernetes.pod.namespace": "platform",
				"app.kubernetes.io/instance":      "signoz",
				"app.kubernetes.io/component":     "otel-collector",
			},
		}},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "4318", "protocol": "TCP"}},
		}},
	}
	var found int
	for _, rule := range with {
		if reflect.DeepEqual(rule, want) {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("collector rule found %d times in %v", found, with)
	}
	names := func(egress []any) []any {
		for _, rule := range egress {
			ports := rule.(map[string]any)["toPorts"]
			if ports == nil {
				continue
			}
			if rules, ok := ports.([]any)[0].(map[string]any)["rules"].(map[string]any); ok {
				return rules["dns"].([]any)
			}
		}
		t.Fatal("no DNS rule")
		return nil
	}
	got, base := names(with), names(without)
	if len(got) != len(base)+1 || !reflect.DeepEqual(got[len(got)-1], map[string]any{"matchName": "signoz-otel-collector.platform.svc.cluster.local"}) {
		t.Fatalf("DNS names %v, want %v plus the collector", got, base)
	}
}

func TestWorkerPodCarriesTheCollectorEndpoint(t *testing.T) {
	env := func(cfg RunnerConfig) map[string]any {
		pod := (&Runner{cfg: cfg, forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(Task{ID: "t1"}, sessionSpec{id: "s-1"})
		vars := map[string]any{}
		for _, e := range pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]map[string]any) {
			vars[e["name"].(string)] = e["value"]
		}
		return vars
	}
	cfg := testRunnerConfig()
	if v, ok := env(cfg)["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
		t.Fatalf("endpoint %v without a collector", v)
	}
	cfg.OTLPEndpoint = testCollector
	if v := env(cfg)["OTEL_EXPORTER_OTLP_ENDPOINT"]; v != testCollector {
		t.Fatalf("OTEL_EXPORTER_OTLP_ENDPOINT = %v", v)
	}
}
