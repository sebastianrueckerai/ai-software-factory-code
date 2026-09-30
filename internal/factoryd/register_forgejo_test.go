package factoryd

import (
	"context"
	"net/http"
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
// cluster: every Forgejo call registration makes, on the real API, before
// and after main's first commit, then once more changing nothing.
func TestRegisterOnForgejo(t *testing.T) {
	base := os.Getenv("FORGEJO_TEST_URL")
	if base == "" {
		t.Skip("FORGEJO_TEST_URL not set")
	}
	adminUser := os.Getenv("FORGEJO_TEST_ADMIN")
	f := &Forgejo{BaseURL: base, BotUser: "factory-agent", BotPassword: os.Getenv("FORGEJO_TEST_BOT_PASSWORD"),
		ProjectsToken: os.Getenv("FORGEJO_TEST_PROJECTS_TOKEN")}
	store := &memProjects{m: map[string]Project{"demo-app": demoProject()}}
	a := AdminAuth{User: adminUser, Password: os.Getenv("FORGEJO_TEST_ADMIN_PASSWORD")}
	reg := &Registrar{Forgejo: f, Kube: newFakeProjectKube(), Store: store,
		OpenBao:    &fakeKV{data: map[string]string{"username": a.User, "password": a.Password}},
		Policy:     func() policy.Document { return policy.Document{Approvers: []string{adminUser}} },
		WebhookURL: "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo", WebhookSecret: "hook-secret",
		FactoryNamespace: "factory", PullSecret: "forgejo-registry", GitURL: base, RegistryHost: "git.example",
		MergeUser: "factory-merge", ProjectsUser: "factory-projects",
		HostKey: func(context.Context) (string, error) { return "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ", nil }}
	logs := captureLog(t)
	ctx := context.Background()
	p := demoProject()
	if _, err := reg.Register(ctx, p.Name, false); err != nil {
		t.Fatalf("first: %v\n%s", err, logs)
	}
	first := logs.String()
	if !strings.Contains(first, "branch deploy: "+string(NoMain)) {
		t.Fatalf("first run on an empty repository:\n%s", first)
	}
	if _, err := f.adminCall(a)(ctx, http.MethodPost, "/api/v1/repos/"+p.Repository+"/contents/README.md",
		mustJSON(map[string]string{"content": "ZGVtbwo=", "branch": "main"}), http.StatusCreated); err != nil {
		t.Fatalf("main's first commit: %v", err)
	}
	logs.Reset()
	if _, err := reg.Register(ctx, p.Name, false); err != nil {
		t.Fatalf("second: %v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "branch deploy: created") {
		t.Fatalf("second run after main's first commit:\n%s", logs)
	}
	logs.Reset()
	if _, err := reg.Register(ctx, p.Name, false); err != nil {
		t.Fatalf("third: %v\n%s", err, logs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.HasSuffix(line, ": no change") && !strings.HasSuffix(line, ": ready") {
			t.Errorf("third run: %s", line)
		}
	}
	t.Logf("first run:\n%s", first)
}
