package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProjectAlert is one entry of GET /projects/{name}/alerts: a SigNoz alert
// firing for the namespace, or rollout_not_ready for a Deployment whose
// rollout has not finished (review-task64 I3: a promotion never passes on
// a rollout that never finished).
type ProjectAlert struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Source    string    `json:"source"` // signoz | factoryd
	Since     time.Time `json:"since,omitzero"`
	Summary   string    `json:"summary"`
}

// FiringAlert is one alert SigNoz holds as firing.
type FiringAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	Status      struct {
		State string `json:"state"`
	} `json:"status"`
}

// namespaceLabels are the label names a namespace arrives under: the OTel
// resource attribute, its Prometheus form, and Kubernetes' own.
var namespaceLabels = []string{"k8s.namespace.name", "k8s_namespace_name", "namespace"}

func (a FiringAlert) namespace() string {
	for _, l := range namespaceLabels {
		if v := a.Labels[l]; v != "" {
			return v
		}
	}
	return ""
}

// AlertSource lists the alerts firing now.
type AlertSource interface {
	FiringAlerts(ctx context.Context) ([]FiringAlert, error)
}

// DeploymentLister lists a namespace's Deployments with their rollout.
type DeploymentLister interface {
	Deployments(ctx context.Context, namespace string) ([]DeploymentState, error)
}

// ProjectReader finds a project row.
type ProjectReader interface {
	Project(ctx context.Context, name string) (Project, bool, error)
}

// alertsCacheFor is how long one namespace's answer serves.
const alertsCacheFor = 30 * time.Second

// ProjectAlertsHandler serves GET /projects/{name}/alerts?namespace=: the
// bearer is the project's FACTORY_READ_TOKEN (401 otherwise, an unknown
// project too), the namespace one of the project's (403 otherwise). The
// answer is a JSON array, [] when nothing fires, cached 30 s per project
// and namespace; a source that cannot be read answers 502, never [].
func ProjectAlertsHandler(projects ProjectReader, alerts AlertSource, deploys DeploymentLister) http.Handler {
	type entry struct {
		at   time.Time
		body []byte
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ns := r.PathValue("name"), r.URL.Query().Get("namespace")
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok, err := projects.Project(r.Context(), name)
		if err != nil {
			log.Printf("project %q alerts: %v", name, err) // #nosec G706 -- %q escapes every control character
			http.Error(w, "cannot read the project", http.StatusInternalServerError)
			return
		}
		if !ok || !p.ReadTokenMatches(token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !slices.Contains(p.Namespaces, ns) {
			http.Error(w, "namespace "+strconv.Quote(ns)+" is not one of project "+name+"'s", http.StatusForbidden)
			return
		}
		key := name + "/" + ns
		mu.Lock()
		e, hit := cache[key]
		mu.Unlock()
		if !hit || time.Since(e.at) >= alertsCacheFor {
			list, err := namespaceAlerts(r.Context(), alerts, deploys, ns)
			if err != nil {
				log.Printf("project %q alerts for %q: %v", name, ns, err) // #nosec G706 -- %q escapes every control character
				http.Error(w, "cannot read the alerts: "+err.Error(), http.StatusBadGateway)
				return
			}
			e = entry{at: time.Now(), body: mustJSON(list)}
			mu.Lock()
			cache[key] = e
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(e.body) // #nosec G104 -- best-effort response write
	})
}

func namespaceAlerts(ctx context.Context, alerts AlertSource, deploys DeploymentLister, ns string) ([]ProjectAlert, error) {
	firing, err := alerts.FiringAlerts(ctx)
	if err != nil {
		return nil, fmt.Errorf("signoz: %w", err)
	}
	out := []ProjectAlert{}
	for _, a := range firing {
		if a.Status.State != "active" || a.namespace() != ns {
			continue
		}
		out = append(out, ProjectAlert{Name: a.Labels["alertname"], Namespace: ns, Source: "signoz", Since: a.StartsAt,
			Summary: cmp.Or(a.Annotations["summary"], a.Annotations["description"])})
	}
	ds, err := deploys.Deployments(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("deployments: %w", err)
	}
	for _, d := range ds {
		if ok, why := d.RolledOut(); !ok {
			out = append(out, ProjectAlert{Name: "rollout_not_ready", Namespace: ns, Source: "factoryd",
				Summary: "deployment " + d.Name + ": " + why})
		}
	}
	return out, nil
}

// SigNoz reads the alerts SigNoz v0.143.0 holds firing: GET /api/v1/alerts
// (its Alertmanager's list) with a bearer from its email/password login,
// the root account of secret platform/signoz-root. A 401 logs in again once.
type SigNoz struct {
	BaseURL, Email, Password string
	HTTP                     *http.Client // nil = 15 s timeout

	mu    sync.Mutex
	token string
}

func (s *SigNoz) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *SigNoz) FiringAlerts(ctx context.Context) ([]FiringAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if s.token == "" {
			if err := s.login(ctx); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/api/v1/alerts", nil) // #nosec G704 -- SigNoz's in-cluster address from the environment
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		res, err := s.client().Do(req) // #nosec G704 -- SigNoz's in-cluster address from the environment
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close() // #nosec G104 -- read fully above
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			s.token = ""
			continue
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET /api/v1/alerts: status %d", res.StatusCode)
		}
		var out struct {
			Status string        `json:"status"`
			Data   []FiringAlert `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.Status != "success" {
			return nil, errors.Join(fmt.Errorf("GET /api/v1/alerts: status %q", out.Status), err)
		}
		return out.Data, nil
	}
}

func (s *SigNoz) login(ctx context.Context) error {
	q := url.Values{"email": {s.Email}, "ref": {s.BaseURL}}
	var session struct {
		Data struct {
			Orgs []struct {
				ID string `json:"id"`
			} `json:"orgs"`
		} `json:"data"`
	}
	if err := s.json(ctx, http.MethodGet, "/api/v2/sessions/context?"+q.Encode(), nil, &session); err != nil {
		return err
	}
	if len(session.Data.Orgs) == 0 {
		return errors.New("signoz login: no organization for the account")
	}
	var login struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	payload := mustJSON(map[string]string{"email": s.Email, "password": s.Password, "orgId": session.Data.Orgs[0].ID})
	if err := s.json(ctx, http.MethodPost, "/api/v2/sessions/email_password", payload, &login); err != nil {
		return err
	}
	if login.Data.AccessToken == "" {
		return errors.New("signoz login: no access token in the answer")
	}
	s.token = login.Data.AccessToken
	return nil
}

func (s *SigNoz) json(ctx context.Context, method, path string, payload []byte, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, bytes.NewReader(payload)) // #nosec G704 -- SigNoz's in-cluster address from the environment
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.client().Do(req) // #nosec G704 -- SigNoz's in-cluster address from the environment
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("signoz %s %s: status %d", method, strings.SplitN(path, "?", 2)[0], res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(into)
}
