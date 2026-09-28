package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeAlerts struct {
	list  []FiringAlert
	err   error
	calls atomic.Int32
}

func (f *fakeAlerts) FiringAlerts(context.Context) ([]FiringAlert, error) {
	f.calls.Add(1)
	return f.list, f.err
}

func firing(state string, labels map[string]string, summary string) FiringAlert {
	a := FiringAlert{Labels: labels, Annotations: map[string]string{"summary": summary},
		StartsAt: time.Date(2026, 9, 28, 17, 0, 0, 0, time.UTC)}
	a.Status.State = state
	return a
}

func alertsRig(t *testing.T) (*memProjects, *fakeAlerts, *fakeProjectKube, http.Handler, string) {
	t.Helper()
	store := &memProjects{m: map[string]Project{}}
	token, hash := NewReadToken()
	p := demoProject()
	p.ReadTokenHash = hash
	store.m[p.Name] = p
	store.m["other"] = Project{Name: "other", Namespaces: []string{"other-ns"}, ReadTokenHash: HashToken("other-token")}
	alerts := &fakeAlerts{}
	kube := newFakeProjectKube()
	kube.deploys = map[string][]DeploymentState{}
	mux := http.NewServeMux()
	mux.Handle("GET /projects/{name}/alerts", ProjectAlertsHandler(store, alerts, kube))
	return store, alerts, kube, mux, token
}

func getAlerts(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProjectAlertsAuth(t *testing.T) {
	_, alerts, _, h, token := alertsRig(t)
	wrong := token[:63] + "0"
	if wrong == token {
		wrong = token[:63] + "1"
	}
	for name, tc := range map[string]struct {
		path, token string
		code        int
	}{
		"no bearer":         {"/projects/demo-app/alerts?namespace=demo-staging", "", http.StatusUnauthorized},
		"wrong token":       {"/projects/demo-app/alerts?namespace=demo-staging", wrong, http.StatusUnauthorized},
		"another project's": {"/projects/demo-app/alerts?namespace=demo-staging", "other-token", http.StatusUnauthorized},
		"unknown project":   {"/projects/nope/alerts?namespace=demo-staging", token, http.StatusUnauthorized},
		"hash as the token": {"/projects/demo-app/alerts?namespace=demo-staging", HashToken(token), http.StatusUnauthorized},
		"foreign namespace": {"/projects/demo-app/alerts?namespace=other-ns", token, http.StatusForbidden},
		"factory namespace": {"/projects/demo-app/alerts?namespace=factory", token, http.StatusForbidden},
		"no namespace":      {"/projects/demo-app/alerts", token, http.StatusForbidden},
		"own namespace":     {"/projects/demo-app/alerts?namespace=demo-prod", token, http.StatusOK},
	} {
		if rec := getAlerts(h, tc.path, tc.token); rec.Code != tc.code {
			t.Errorf("%s: %d, want %d: %s", name, rec.Code, tc.code, rec.Body)
		}
	}
	if n := alerts.calls.Load(); n != 1 {
		t.Fatalf("SigNoz read %d times; only the admitted request may read it", n)
	}
}

func TestProjectAlertsAnswer(t *testing.T) {
	_, alerts, kube, h, token := alertsRig(t)
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("nothing firing = %d %q", rec.Code, rec.Body)
	}

	alerts.list = []FiringAlert{
		firing("active", map[string]string{"alertname": "demo 5xx rate", "k8s.namespace.name": "demo-prod"}, "5xx above 1%"),
		firing("active", map[string]string{"alertname": "demo p95", "k8s_namespace_name": "demo-prod"}, "p95 above 300 ms"),
		firing("suppressed", map[string]string{"alertname": "silenced", "namespace": "demo-prod"}, "silenced"),
		firing("active", map[string]string{"alertname": "staging only", "namespace": "demo-staging"}, "x"),
		firing("active", map[string]string{"alertname": "no namespace"}, "x"),
	}
	kube.deploys["demo-prod"] = []DeploymentState{
		{Name: "demo-app", Generation: 4, ObservedGeneration: 4, Replicas: 1, StatusReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1},
		{Name: "ready", Generation: 1, ObservedGeneration: 1, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	rec = getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod", token)
	var got []ProjectAlert
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	since := time.Date(2026, 9, 28, 17, 0, 0, 0, time.UTC)
	want := []ProjectAlert{
		{Name: "demo 5xx rate", Namespace: "demo-prod", Source: "signoz", Since: since, Summary: "5xx above 1%"},
		{Name: "demo p95", Namespace: "demo-prod", Source: "signoz", Since: since, Summary: "p95 above 300 ms"},
		{Name: "rollout_not_ready", Namespace: "demo-prod", Source: "factoryd", Summary: "deployment demo-app: 1 old replicas pending termination"},
	}
	if len(got) != len(want) {
		t.Fatalf("alerts = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alert %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// cached for 30 s: the fix does not show before, and staging's answer
	// was cached empty although staging now has an alert
	alerts.list = nil
	calls := alerts.calls.Load()
	if again := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod", token); again.Body.String() != rec.Body.String() {
		t.Fatalf("second read within 30 s differs: %s", again.Body)
	}
	if staged := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token); strings.TrimSpace(staged.Body.String()) != "[]" {
		t.Fatalf("staging = %s", staged.Body)
	}
	if alerts.calls.Load() != calls {
		t.Fatal("a cached answer read SigNoz again")
	}
}

// A source that cannot be read never answers [] (a promotion would pass).
func TestProjectAlertsFailClosed(t *testing.T) {
	_, alerts, _, h, token := alertsRig(t)
	alerts.err = errors.New("connection refused")
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("SigNoz down = %d %s", rec.Code, rec.Body)
	}
	alerts.err = nil
	if rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token); rec.Code != http.StatusOK {
		t.Fatalf("a failure was cached: %d", rec.Code)
	}
}

// The SigNoz client logs in as the root account and logs in again once on
// a 401 (an expired access token).
func TestSigNozFiringAlerts(t *testing.T) {
	var logins, reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/sessions/context":
			if r.URL.Query().Get("email") != "root@example.com" {
				t.Errorf("context query %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"status":"success","data":{"exists":true,"orgs":[{"id":"org-1","name":"sf"}]}}`)
		case "POST /api/v2/sessions/email_password":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["email"] != "root@example.com" || body["password"] != "pw" || body["orgId"] != "org-1" {
				t.Errorf("login %v", body)
			}
			n := logins.Add(1)
			io.WriteString(w, `{"status":"success","data":{"accessToken":"tok-`+string(rune('0'+n))+`"}}`)
		case "GET /api/v1/alerts":
			n := reads.Add(1)
			if n == 2 && r.Header.Get("Authorization") == "Bearer tok-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"status":"success","data":[{"labels":{"alertname":"a","namespace":"demo-staging"},
				"annotations":{"summary":"s"},"startsAt":"2026-09-28T17:00:00Z","status":{"state":"active"}}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	s := &SigNoz{BaseURL: srv.URL, Email: "root@example.com", Password: "pw", HTTP: srv.Client()}
	for i := range 2 {
		got, err := s.FiringAlerts(context.Background())
		if err != nil || len(got) != 1 || got[0].namespace() != "demo-staging" || got[0].Status.State != "active" {
			t.Fatalf("read %d = %+v, %v", i, got, err)
		}
	}
	if logins.Load() != 2 || reads.Load() != 3 {
		t.Fatalf("logins %d, reads %d; want 2 and 3 (one 401 retried)", logins.Load(), reads.Load())
	}
}
