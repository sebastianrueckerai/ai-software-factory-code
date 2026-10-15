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

// rowAlerts is the alerts rows the endpoint reads, counting the reads;
// err fails them.
type rowAlerts struct {
	*fakeStore
	err   error
	calls atomic.Int32
}

func (r *rowAlerts) NewestAlertRows(ctx context.Context, kind, namespace string) ([]Alert, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	return r.fakeStore.NewestAlertRows(ctx, kind, namespace)
}

// firingRow is an alerts row of kind alert in namespace, firing since
// starts_at unless resolved_at follows.
func firingRow(name, fingerprint, namespace, startsAt, resolvedAt, summary string) Alert {
	a := Alert{Kind: AlertSigNoz, Source: name, TaskID: "t-" + fingerprint, Fingerprint: fingerprint,
		Detail: map[string]any{"namespace": namespace, "starts_at": startsAt, "summary": summary}}
	if resolvedAt != "" {
		a.Detail["resolved_at"] = resolvedAt
	}
	return a
}

func alertsRig(t *testing.T) (*memProjects, *rowAlerts, *fakeProjectKube, http.Handler, string) {
	t.Helper()
	store := &memProjects{m: map[string]Project{}}
	token, hash := NewReadToken()
	p := demoProject()
	p.ReadTokenHash = hash
	store.m[p.Name] = p
	store.m["other"] = Project{Name: "other", Namespaces: []string{"other-ns"}, ReadTokenHash: HashToken("other-token")}
	alerts := &rowAlerts{fakeStore: newFakeStore()}
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
		t.Fatalf("the alerts rows read %d times; only the admitted request may read them", n)
	}
}

func TestProjectAlertsAnswer(t *testing.T) {
	_, alerts, kube, h, token := alertsRig(t)
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("nothing firing = %d %q", rec.Code, rec.Body)
	}

	ctx := context.Background()
	for _, a := range []Alert{
		firingRow("demo 5xx rate", "fp1", "demo-prod", "2026-09-28T17:00:00Z", "", "5xx above 1%"),
		firingRow("Kustomization not Ready", "flux/demo-prod/demo-app", "demo-prod", "2026-09-28T17:05:00Z", "",
			`Secret/demo-prod/db dry-run failed: secrets "db" not found`),
		// resolved after its start: no longer fires
		firingRow("demo p95", "fp2", "demo-prod", "2026-09-28T17:00:00Z", "2026-09-28T17:30:00Z", "p95 above 300 ms"),
		// fired again since its last resolve
		firingRow("demo restarts", "fp3", "demo-prod", "2026-09-28T18:00:00Z", "2026-09-28T17:30:00Z", "restarts"),
		// an older row of a fingerprint whose newest row resolved
		firingRow("demo saturation", "fp4", "demo-prod", "2026-09-28T16:00:00Z", "", "old"),
		firingRow("demo saturation", "fp4", "demo-prod", "2026-09-28T16:00:00Z", "2026-09-28T16:20:00Z", "old"),
		firingRow("staging only", "fp5", "demo-staging", "2026-09-28T17:00:00Z", "", "x"),
		{Kind: AlertEscalation, Source: "agent", TaskID: "t1", Fingerprint: "fp7", Detail: map[string]any{"namespace": "demo-prod", "starts_at": "2026-09-28T17:00:00Z"}},
	} {
		alerts.InsertAlert(ctx, a)
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
	want := []ProjectAlert{
		{Name: "demo 5xx rate", Namespace: "demo-prod", Source: "signoz", Since: time.Date(2026, 9, 28, 17, 0, 0, 0, time.UTC), Summary: "5xx above 1%"},
		{Name: "Kustomization not Ready", Namespace: "demo-prod", Source: "flux", Since: time.Date(2026, 9, 28, 17, 5, 0, 0, time.UTC),
			Summary: `Secret/demo-prod/db dry-run failed: secrets "db" not found`},
		{Name: "demo restarts", Namespace: "demo-prod", Source: "signoz", Since: time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC), Summary: "restarts"},
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
	for _, a := range alerts.alertRows() {
		alerts.MergeAlertDetail(ctx, a.ID, map[string]any{"resolved_at": "2026-09-28T19:00:00Z"})
	}
	calls := alerts.calls.Load()
	if again := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod", token); again.Body.String() != rec.Body.String() {
		t.Fatalf("second read within 30 s differs: %s", again.Body)
	}
	if staged := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token); strings.TrimSpace(staged.Body.String()) != "[]" {
		t.Fatalf("staging = %s", staged.Body)
	}
	if alerts.calls.Load() != calls {
		t.Fatal("a cached answer read the alerts rows again")
	}
}

// A noise row never holds a promotion, by its verdict: a roll window's own
// restart (ruling 237) or an alert a human closed as noise, though either
// still fires; a firing row judged real does.
func TestProjectAlertsNoiseHoldsNothing(t *testing.T) {
	_, alerts, _, h, token := alertsRig(t)
	ctx := context.Background()
	window := firingRow("5xx rate above 5 %", "fp-window", "demo-prod", "2026-09-28T17:00:00Z", "", "")
	window.Verdict, window.ResolvedBy, window.TaskID = VerdictNoise, "factoryd", ""
	closed := firingRow("demo p95", "fp-closed", "demo-prod", "2026-09-28T17:00:00Z", "", "p95 above 300 ms")
	closed.Verdict, closed.ResolvedBy = VerdictNoise, "sebastian"
	merged := firingRow("demo 5xx rate", "fp-merged", "demo-prod", "2026-09-28T17:00:00Z", "", "5xx above 1%")
	merged.Verdict, merged.ResolvedBy = VerdictReal, "merge"
	for _, a := range []Alert{window, closed, merged} {
		alerts.InsertAlert(ctx, a)
	}
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod", token)
	var got []ProjectAlert
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	want := ProjectAlert{Name: "demo 5xx rate", Namespace: "demo-prod", Source: "signoz", Since: time.Date(2026, 9, 28, 17, 0, 0, 0, time.UTC), Summary: "5xx above 1%"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("alerts = %+v, want [%+v]", got, want)
	}
}

// With a digest asked and no Deployment in the namespace, the rollout has
// nothing to wait on: rollout_not_ready fires (a promotion never passes on
// an app that never deployed).
func TestAlertsDigestNoDeployment(t *testing.T) {
	_, _, _, h, token := alertsRig(t)
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging&digest=sha256:"+strings.Repeat("b", 64), token)
	var got []ProjectAlert
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	want := ProjectAlert{Name: "rollout_not_ready", Namespace: "demo-staging", Source: "factoryd", Summary: "no deployment in demo-staging"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("alerts = %+v, want [%+v]", got, want)
	}
}

// A Deployment whose pod template still names the old image is not the
// rollout being waited on, whatever RolledOut says about it.
func TestAlertsDigestNotRunning(t *testing.T) {
	_, _, kube, h, token := alertsRig(t)
	running := "sha256:" + strings.Repeat("a", 64)
	asked := "sha256:" + strings.Repeat("b", 64)
	kube.deploys["demo-prod"] = []DeploymentState{
		{Name: "linkshort", Generation: 1, ObservedGeneration: 1, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Images: []string{"git.aisoftwarefactory.world/sf/linkshort@" + running}},
	}
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod&digest="+asked, token)
	var got []ProjectAlert
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	want := ProjectAlert{Name: "rollout_not_ready", Namespace: "demo-prod", Source: "factoryd",
		Summary: "deployment linkshort runs " + running + ", not " + asked}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("alerts = %+v, want [%+v]", got, want)
	}
}

// Every image ending in @digest: the rollout being waited on is live.
func TestAlertsDigestRunning(t *testing.T) {
	_, _, kube, h, token := alertsRig(t)
	digest := "sha256:" + strings.Repeat("c", 64)
	kube.deploys["demo-prod"] = []DeploymentState{
		{Name: "linkshort", Generation: 1, ObservedGeneration: 1, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Images: []string{"git.aisoftwarefactory.world/sf/linkshort@" + digest}},
	}
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod&digest="+digest, token)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("running the asked digest = %d %s", rec.Code, rec.Body)
	}
}

// A Deployment whose pod template already names the new digest but whose
// pod is still starting or crash-looping is not the rollout being waited on
// either: the digest alone is not enough, RolledOut must hold too.
func TestAlertsDigestRunningNotRolledOut(t *testing.T) {
	_, _, kube, h, token := alertsRig(t)
	digest := "sha256:" + strings.Repeat("b", 64)
	kube.deploys["demo-prod"] = []DeploymentState{
		{Name: "linkshort", Generation: 4, ObservedGeneration: 4, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 0,
			Images: []string{"git.aisoftwarefactory.world/sf/linkshort@" + digest}},
	}
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod&digest="+digest, token)
	var got []ProjectAlert
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	want := ProjectAlert{Name: "rollout_not_ready", Namespace: "demo-prod", Source: "factoryd",
		Summary: "deployment linkshort runs " + digest + " but is not rolled out: 0 of 1 updated replicas available"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("alerts = %+v, want [%+v]", got, want)
	}
}

// Two digests for the same namespace must not share a cached answer, or a
// stale "[]" from a first, matching digest would hide a later mismatch.
func TestAlertsCacheKeyHasDigest(t *testing.T) {
	_, _, kube, h, token := alertsRig(t)
	digest := "sha256:" + strings.Repeat("d", 64)
	kube.deploys["demo-prod"] = []DeploymentState{
		{Name: "linkshort", Generation: 1, ObservedGeneration: 1, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Images: []string{"git.aisoftwarefactory.world/sf/linkshort@" + digest}},
	}
	same := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod&digest="+digest, token)
	if strings.TrimSpace(same.Body.String()) != "[]" {
		t.Fatalf("running the asked digest = %s", same.Body)
	}
	other := "sha256:" + strings.Repeat("e", 64)
	mismatch := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-prod&digest="+other, token)
	var got []ProjectAlert
	if err := json.Unmarshal(mismatch.Body.Bytes(), &got); err != nil {
		t.Fatalf("= %s", mismatch.Body)
	}
	if len(got) != 1 || got[0].Name != "rollout_not_ready" {
		t.Fatalf("a different digest reused the first digest's cached answer: %+v", got)
	}
}

// A source that cannot be read never answers [] (a promotion would pass).
func TestProjectAlertsFailClosed(t *testing.T) {
	_, alerts, _, h, token := alertsRig(t)
	alerts.err = errors.New("connection refused")
	rec := getAlerts(h, "/projects/demo-app/alerts?namespace=demo-staging", token)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the store down = %d %s", rec.Code, rec.Body)
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
		if err != nil || len(got) != 1 || got[0].Labels["namespace"] != "demo-staging" || got[0].Status.State != "active" {
			t.Fatalf("read %d = %+v, %v", i, got, err)
		}
	}
	if logins.Load() != 2 || reads.Load() != 3 {
		t.Fatalf("logins %d, reads %d; want 2 and 3 (one 401 retried)", logins.Load(), reads.Load())
	}
}
