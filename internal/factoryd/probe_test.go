package factoryd

import (
	"strings"
	"testing"
)

func TestProbeAlertIssue(t *testing.T) {
	a := SigNozAlert{Labels: map[string]string{"alertname": "x", "namespace": "demo-staging", "route": " <!-- hidden --> (fixes) #12"},
		Annotations: map[string]string{"value": "1", "summary": "(closes) #7  done"}, StartsAt: at(firedAt), Fingerprint: "fp"}
	title, body := alertIssue(a, "demo-app", "demo-staging")
	t.Logf("title %q", title)
	for _, l := range strings.Split(body, "\n") {
		t.Logf("line %q", l)
	}
}
