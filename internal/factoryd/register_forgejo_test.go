package factoryd

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestRegisterOnForgejo runs Register against a real Forgejo 15 (a
// disposable one: FORGEJO_TEST_URL, its site admin FORGEJO_TEST_ADMIN and
// FORGEJO_TEST_ADMIN_PASSWORD, users factory-projects, factory-merge and
// factory-agent (password FORGEJO_TEST_BOT_PASSWORD), and factory-projects'
// write:repository token FORGEJO_TEST_PROJECTS_TOKEN), with a fake
// cluster: every Forgejo call registration makes, on the real API, twice.
func TestRegisterOnForgejo(t *testing.T) {
	base := os.Getenv("FORGEJO_TEST_URL")
	if base == "" {
		t.Skip("FORGEJO_TEST_URL not set")
	}
	adminUser := os.Getenv("FORGEJO_TEST_ADMIN")
	f := &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: os.Getenv("FORGEJO_TEST_BOT_PASSWORD"),
		ProjectsToken: os.Getenv("FORGEJO_TEST_PROJECTS_TOKEN")}
	store := &memProjects{m: map[string]Project{}}
	reg := &Registrar{Forgejo: f, Kube: newFakeProjectKube(), Store: store,
		Policy:     func() policy.Document { return policy.Document{Approvers: []string{adminUser}} },
		WebhookURL: "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo", WebhookSecret: "hook-secret",
		FactoryNamespace: "factory", PullSecret: "forgejo-registry", GitURL: base, RegistryHost: "git.example",
		MergeUser: "factory-merge", ProjectsUser: "factory-projects"}
	logs := captureLog(t)
	a := AdminAuth{User: adminUser, Password: os.Getenv("FORGEJO_TEST_ADMIN_PASSWORD")}
	ctx := context.Background()
	p := demoProject()
	if _, err := reg.Register(ctx, p, a, false); err != nil {
		t.Fatalf("first: %v\n%s", err, logs)
	}
	first := logs.String()
	logs.Reset()
	if _, err := reg.Register(ctx, p, a, false); err != nil {
		t.Fatalf("second: %v\n%s", err, logs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.HasSuffix(line, ": no change") && !strings.HasSuffix(line, ": ready") && !strings.HasSuffix(line, ": ensured") &&
			!strings.HasSuffix(line, "(its settings are the console's)") {
			t.Errorf("second run: %s", line)
		}
	}
	t.Logf("first run:\n%s", first)
}
