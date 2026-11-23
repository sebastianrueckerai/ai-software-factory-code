package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProjectAlert is one entry of GET /projects/{name}/alerts: an alert
// firing in the namespace, SigNoz's or factoryd's own of a Kustomization
// (roadmap 259), or rollout_not_ready for a Deployment whose rollout has
// not finished (review-task64 I3: a promotion never passes on a rollout
// that never finished).
type ProjectAlert struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Source    string    `json:"source"` // signoz | flux | factoryd
	Since     time.Time `json:"since,omitzero"`
	Summary   string    `json:"summary"`
}

// FiringAlert is one alert SigNoz holds as firing.
type FiringAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	// GeneratorURL is the alert's link into SigNoz.
	GeneratorURL string `json:"generatorURL"`
	// Fingerprint is Alertmanager's label-set hash, top level of its
	// message; empty when the answer carries none.
	Fingerprint string `json:"fingerprint"`
	Status      struct {
		State string `json:"state"`
	} `json:"status"`
}

// AlertSource lists the alerts firing now.
type AlertSource interface {
	FiringAlerts(ctx context.Context) ([]FiringAlert, error)
}

// DeploymentLister lists a namespace's Deployments with their rollout.
type DeploymentLister interface {
	Deployments(ctx context.Context, namespace string) ([]DeploymentState, error)
}

// AlertRowReader reads the newest alerts row of kind of each fingerprint
// whose detail names namespace (PGStore.NewestAlertRows).
type AlertRowReader interface {
	NewestAlertRows(ctx context.Context, kind, namespace string) ([]Alert, error)
}

// ProjectReader finds a project row.
type ProjectReader interface {
	Project(ctx context.Context, name string) (Project, bool, error)
}

// alertsCacheFor is how long one namespace's answer serves.
const alertsCacheFor = 30 * time.Second

// ProjectRollRecorder records a project roll the CI rollout wait
// confirmed: one row per repository and digest (roadmap 198).
type ProjectRollRecorder interface {
	RecordProjectRoll(ctx context.Context, repository, namespace, digest, commit string, readyAt time.Time) error
}

// The roll row's shapes: the digest form every image carries, the deploy
// commit a git sha, or none. The poller names both, so the shapes hold at
// the write.
var (
	rollDigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	rollCommitRe = regexp.MustCompile(`^([0-9a-f]{7,40})?$`)
)

// ProjectAlertsHandler serves GET /projects/{name}/alerts?namespace=&digest=:
// the bearer is the project's FACTORY_READ_TOKEN (401 otherwise, an unknown
// project too), the namespace one of the project's (403 otherwise). The
// alerts that fire are read from their alerts rows, the one record of
// SigNoz's alerts and factoryd's alike: each fingerprint's newest row while
// no resolve came after its start, unless its verdict is noise (a firing
// that ended while a rollout's window held it, or a human's close). With no
// digest, a Deployment lags only when its own rollout has not finished. With
// a digest (a staging rollout's wait), a namespace lags when it holds no
// Deployment, when a Deployment's images do not all end in @digest, or when
// they do but its rollout has not finished either — the digest alone does
// not mean the new pod is up. The answer is a JSON array, [] when nothing
// fires, cached 30 s per project, namespace and digest; a source that
// cannot be read answers 502, never [].
func ProjectAlertsHandler(projects ProjectReader, alerts AlertRowReader, deploys DeploymentLister, rolls ProjectRollRecorder) http.Handler {
	type entry struct {
		at   time.Time
		body []byte
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ns := r.PathValue("name"), r.URL.Query().Get("namespace")
		digest := r.URL.Query().Get("digest")
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
		key := name + "/" + ns + "/" + digest
		mu.Lock()
		e, hit := cache[key]
		mu.Unlock()
		if !hit || time.Since(e.at) >= alertsCacheFor {
			list, confirmed, err := namespaceAlerts(r.Context(), alerts, deploys, ns, digest)
			if err != nil {
				log.Printf("project %q alerts for %q: %v", name, ns, err) // #nosec G706 -- %q escapes every control character
				http.Error(w, "cannot read the alerts: "+err.Error(), http.StatusBadGateway)
				return
			}
			if confirmed {
				commit := r.URL.Query().Get("commit")
				// the rollout the wait asked for is live: recorded once, the
				// row is the project's deploy fact. The poller names the
				// digest and the commit, so only a sha256 digest and a hex
				// commit (or none) reach the table; a refused or failed
				// write never fails the answer the promotion reads
				if !rollDigestRe.MatchString(digest) || !rollCommitRe.MatchString(commit) {
					log.Printf("project %q roll with digest %q, commit %q: not a shape the table keeps", name, digest, commit) // #nosec G706 -- %q escapes every control character
				} else if err := rolls.RecordProjectRoll(context.WithoutCancel(r.Context()), p.Repository, ns, digest, commit, time.Now()); err != nil {
					log.Printf("project %q roll %s in %s: %v", name, digest, ns, err) // #nosec G706 -- %q escapes every control character
				}
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

func namespaceAlerts(ctx context.Context, alerts AlertRowReader, deploys DeploymentLister, ns, digest string) (list []ProjectAlert, confirmed bool, err error) {
	rows, err := alerts.NewestAlertRows(ctx, AlertSigNoz, ns)
	if err != nil {
		return nil, false, fmt.Errorf("alerts: %w", err)
	}
	out := []ProjectAlert{}
	for _, a := range rows {
		since, firing := firingSince(a)
		if !firing || a.Verdict == VerdictNoise {
			continue
		}
		source := "signoz"
		if fromFlux(a.Fingerprint) {
			source = "flux"
		}
		summary, _ := a.Detail["summary"].(string)
		out = append(out, ProjectAlert{Name: a.Source, Namespace: ns, Source: source, Since: since, Summary: summary})
	}
	ds, err := deploys.Deployments(ctx, ns)
	if err != nil {
		return nil, false, fmt.Errorf("deployments: %w", err)
	}
	if digest == "" {
		for _, d := range ds {
			if ok, why := d.RolledOut(); !ok {
				out = append(out, ProjectAlert{Name: "rollout_not_ready", Namespace: ns, Source: "factoryd",
					Summary: "deployment " + d.Name + ": " + why})
			}
		}
		return out, false, nil
	}
	confirmed = len(ds) > 0
	if len(ds) == 0 {
		out = append(out, ProjectAlert{Name: "rollout_not_ready", Namespace: ns, Source: "factoryd",
			Summary: "no deployment in " + ns})
	}
	for _, d := range ds {
		if running, ok := digestRunning(d.Images, digest); !ok {
			confirmed = false
			out = append(out, ProjectAlert{Name: "rollout_not_ready", Namespace: ns, Source: "factoryd",
				Summary: "deployment " + d.Name + " runs " + running + ", not " + digest})
		} else if ok, why := d.RolledOut(); !ok {
			confirmed = false
			out = append(out, ProjectAlert{Name: "rollout_not_ready", Namespace: ns, Source: "factoryd",
				Summary: "deployment " + d.Name + " runs " + digest + " but is not rolled out: " + why})
		}
	}
	return out, confirmed, nil
}

// RecordProjectRoll writes the project roll a CI rollout wait confirmed:
// one row per repository and digest; the first confirmation wins, and a
// later one repairs an empty commit (a poll whose workflow predates the
// template's commit parameter).
func (s *PGStore) RecordProjectRoll(ctx context.Context, repository, namespace, digest, commit string, readyAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO project_rolls (repository, namespace, digest, commit, ready_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (repository, digest) DO UPDATE
		SET commit = EXCLUDED.commit WHERE project_rolls.commit = ''`,
		repository, namespace, digest, commit, readyAt)
	return err
}

// digestRunning reports whether every one of a Deployment's images ends in
// @digest; when one does not, it also gives what that image runs instead
// (or "no image" / "no digest" when there is none to compare).
func digestRunning(images []string, digest string) (string, bool) {
	if len(images) == 0 {
		return "no image", false
	}
	suffix := "@" + digest
	for _, img := range images {
		if strings.HasSuffix(img, suffix) {
			continue
		}
		if _, after, found := strings.Cut(img, "@"); found {
			return after, false
		}
		return "no digest", false
	}
	return "", true
}

// SigNoz reads the alerts SigNoz v0.143.0 holds firing (GET /api/v1/alerts,
// its Alertmanager's list) and the nodes' kernel kill lines (POST
// /api/v5/query_range) with a bearer from its email/password login, the
// viewer-role user of secret factory/signoz-factoryd. A 401 logs in again
// once. One call runs at a time; a caller whose context ends while it waits
// gives up.
type SigNoz struct {
	BaseURL, Email, Password string
	HTTP                     *http.Client // nil = 15 s timeout

	once  sync.Once
	slot  chan struct{}
	token string
}

// lock takes the client's one slot, or fails when ctx ends first.
func (s *SigNoz) lock(ctx context.Context) error {
	s.once.Do(func() { s.slot = make(chan struct{}, 1) })
	select {
	case s.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SigNoz) unlock() { <-s.slot }

func (s *SigNoz) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// do sends one bearer request with the slot held by the caller; a 401 logs in
// again once. It returns the status and at most 4 MiB of the body.
func (s *SigNoz) do(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		if s.token == "" {
			if err := s.login(ctx); err != nil {
				return 0, nil, err
			}
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, rd) // #nosec G704 -- SigNoz's in-cluster address from the environment
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := s.client().Do(req) // #nosec G704 -- SigNoz's in-cluster address from the environment
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close() // #nosec G104 -- read fully above
		if err != nil {
			return 0, nil, err
		}
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			s.token = ""
			continue
		}
		return res.StatusCode, body, nil
	}
}

func (s *SigNoz) FiringAlerts(ctx context.Context) ([]FiringAlert, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	code, body, err := s.do(ctx, http.MethodGet, "/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET /api/v1/alerts: status %d", code)
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

// KernelLines reads the kill lines of the nodes' kernel logs in [from, to]
// from SigNoz, where the nodes' Talos send them as service talos-kmsg.
func (s *SigNoz) KernelLines(ctx context.Context, from, to time.Time, podUID string) ([]KernelKill, error) {
	filter := KernelFilter
	if podUID != "" {
		if !kubeUID.MatchString(podUID) {
			return nil, fmt.Errorf("kernel lines: %q is no Kubernetes pod UID, so no query names it", podUID)
		}
		filter += " AND body CONTAINS '" + podUID + "'"
	}
	query := map[string]any{
		"schemaVersion": "v1",
		"start":         from.UnixMilli(),
		"end":           to.UnixMilli(),
		"requestType":   "raw",
		"compositeQuery": map[string]any{"queries": []any{map[string]any{
			"type": "builder_query",
			"spec": map[string]any{
				"name":   "A",
				"signal": "logs",
				"filter": map[string]any{"expression": filter},
				"order": []any{
					map[string]any{"key": map[string]any{"name": "timestamp"}, "direction": "asc"},
					map[string]any{"key": map[string]any{"name": "id"}, "direction": "asc"},
				},
				"limit": 1000,
			},
		}}},
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	code, body, err := s.do(ctx, http.MethodPost, "/api/v5/query_range", mustJSON(query))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("POST /api/v5/query_range: status %d", code)
	}
	var out struct {
		Data struct {
			Data struct {
				Results []struct {
					NextCursor string `json:"nextCursor"`
					Rows       []struct {
						Timestamp time.Time `json:"timestamp"`
						Data      struct {
							Body             string             `json:"body"`
							AttributesNumber map[string]float64 `json:"attributes_number"`
							ResourcesString  map[string]string  `json:"resources_string"`
						} `json:"data"`
					} `json:"rows"`
				} `json:"results"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("POST /api/v5/query_range: %w", err)
	}
	var kills []KernelKill
	for _, r := range out.Data.Data.Results {
		if r.NextCursor != "" {
			log.Printf("kernel lines %s to %s: more rows than the limit, next cursor %q", from.Format(time.RFC3339), to.Format(time.RFC3339), r.NextCursor) // #nosec G706 -- %q escapes every control character
		}
		for _, row := range r.Rows {
			k, ok := ParseKernelLine(strings.TrimSpace(row.Data.Body), row.Data.ResourcesString["k8s.node.name"], int64(row.Data.AttributesNumber["seq"]), row.Timestamp)
			if ok {
				kills = append(kills, k)
			}
		}
	}
	return kills, nil
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
