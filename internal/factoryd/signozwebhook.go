package factoryd

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SigNozAlert is one alert of a SigNoz v0.143.0 webhook notification (the
// Alertmanager v4 message).
type SigNozAlert struct {
	Status       string            `json:"status"` // firing | resolved
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// NamespaceLabeler reads a label of a namespace.
type NamespaceLabeler interface {
	NamespaceLabel(ctx context.Context, name, key string) (string, error)
}

// AlertBook is what the alert intake keeps: the task rows it files and one
// alerts row per issue, found again by the alert's fingerprint.
type AlertBook interface {
	CreateTask(ctx context.Context, t Task) error
	Task(ctx context.Context, id string) (Task, bool, error)
	InsertAlert(ctx context.Context, a Alert) (int64, error)
	AlertByFingerprint(ctx context.Context, fingerprint string) (Alert, bool, error)
	MergeAlertDetail(ctx context.Context, id int64, detail map[string]any) error
}

// AlertIntake serves POST /webhooks/signoz, SigNoz's webhook channel
// factoryd (basic auth from secret signoz-webhook). An alert's namespace
// label, which the signoz-objects job sets on a project's rule, names the
// project through the namespace's factory/project label;
// the first firing of a fingerprint files a bug issue on the project's
// repository as the bot and starts its task, a later firing is a comment
// on it while the task runs and a new issue once it ended, and a resolve
// is a comment. An alert no project owns is logged and dropped.
type AlertIntake struct {
	User, Password string
	Namespaces     NamespaceLabeler
	Projects       ProjectReader
	Store          AlertBook
	Forgejo        *Forgejo
	Workflows      Workflows
	Budget         func() policy.Budget

	// one notification at a time: two deliveries of a new fingerprint
	// file one issue
	mu        sync.Mutex
	projectOf map[string]string // namespace -> its factory/project label
}

func (in *AlertIntake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || in.Password == "" ||
		subtle.ConstantTimeCompare([]byte(user), []byte(in.User)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(in.Password)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="factoryd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var msg struct {
		Alerts []SigNozAlert `json:"alerts"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&msg); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	var errs []error
	for _, a := range msg.Alerts {
		if err := in.take(r.Context(), a); err != nil {
			errs = append(errs, fmt.Errorf("alert %s: %w", strconv.Quote(a.Fingerprint), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		// SigNoz sends the notification again on a 5xx; every step above
		// is keyed by fingerprint and firing time, so a retry repeats none
		log.Printf("signoz webhook: %v", err) // #nosec G706 -- strconv.Quote escapes the alert's text
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (in *AlertIntake) take(ctx context.Context, a SigNozAlert) error {
	// the namespace label the signoz-objects job sets on a project's rule,
	// never a label the rule's query produced
	name, ns := a.Labels["alertname"], a.Labels["namespace"]
	p, ok, err := in.project(ctx, ns)
	if err != nil {
		return err
	}
	if !ok || a.Fingerprint == "" {
		log.Printf("signoz webhook: alert %s in namespace %s: no project owns it, dropped", strconv.Quote(name), strconv.Quote(ns)) // #nosec G706 -- quoted
		return nil
	}
	row, seen, err := in.Store.AlertByFingerprint(ctx, a.Fingerprint)
	if err != nil {
		return err
	}
	repo := p.Repo()
	if a.Status == "resolved" {
		at := stamp(a.EndsAt)
		if !seen || row.Detail["resolved_at"] == at {
			return nil
		}
		if _, err := in.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, issueOf(row), "factoryd: resolved at "+at); err != nil {
			return err
		}
		return in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"resolved_at": at})
	}
	at := stamp(a.StartsAt)
	if seen && row.Detail["starts_at"] == at {
		return nil // the same firing again: Alertmanager resends a group's every alert
	}
	if seen {
		t, ok, err := in.Store.Task(ctx, row.TaskID)
		if err != nil {
			return err
		}
		if ok && !Final(t.Status) {
			msg := "factoryd: the alert fired again at " + at + ".\n\n" + fenced("value="+oneLine(a.Annotations["value"]))
			if _, err := in.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, issueOf(row), msg); err != nil {
				return err
			}
			return in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"starts_at": at})
		}
	}
	return in.file(ctx, a, p, ns, at)
}

// file files the alert's bug issue and task, then its alerts row.
func (in *AlertIntake) file(ctx context.Context, a SigNozAlert, p Project, ns, at string) error {
	repo := p.Repo()
	sha, err := in.Forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, "")
	if err != nil {
		return fmt.Errorf("base sha: %w", err)
	}
	title, body := alertIssue(a, p.Name, ns)
	t, ref, err := FileTask(ctx, in.Store, in.Forgejo, in.Workflows, repo, title, body, []string{"bug"}, Task{
		ID: NewTaskID(), RiskClass: "standard", BudgetEUR: in.Budget().ClampBudget(0),
		Objective: title + "\n\n" + body, SHA: sha, Source: "alert",
		// factoryd files as the bot; the values from the alert stand fenced
		Author: in.Forgejo.BotUser, AuthorWriter: true,
	})
	if err != nil {
		return err
	}
	link := ""
	if safeLink(a.GeneratorURL) {
		link = a.GeneratorURL
	}
	if _, err := in.Store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: oneLine(a.Labels["alertname"]), TaskID: t.ID, Fingerprint: a.Fingerprint,
		Detail: map[string]any{"project": p.Name, "namespace": ns, "repository": repo.String(), "issue": ref.Number,
			"starts_at": at, "link": link, "labels": a.Labels}}); err != nil {
		return err
	}
	log.Printf("signoz webhook: alert %s in %s: issue #%d on %s, task %s", strconv.Quote(a.Labels["alertname"]), ns, ref.Number, repo, t.ID) // #nosec G706 -- quoted, ns is a checked DNS label
	return nil
}

// project is the project owning namespace ns: its factory/project label,
// read once, naming a managed, registered project that lists ns.
func (in *AlertIntake) project(ctx context.Context, ns string) (Project, bool, error) {
	if !dnsLabel.MatchString(ns) {
		return Project{}, false, nil
	}
	name, ok := in.projectOf[ns]
	if !ok {
		label, err := in.Namespaces.NamespaceLabel(ctx, ns, ProjectLabel)
		if errors.Is(err, ErrNotFound) {
			return Project{}, false, nil
		}
		if err != nil {
			return Project{}, false, fmt.Errorf("namespace %s: %w", ns, err)
		}
		if label == "" {
			return Project{}, false, nil
		}
		if in.projectOf == nil {
			in.projectOf = map[string]string{}
		}
		in.projectOf[ns], name = label, label
	}
	p, ok, err := in.Projects.Project(ctx, name)
	if err != nil || !ok {
		return Project{}, false, err
	}
	return p, p.Managed && p.Minted() && slices.Contains(p.Namespaces, ns), nil
}

// alertIssue is the bug issue of alert a: title "alert: <name> in <ns>"
// and a body with the firing time, the SigNoz link, the value and
// annotations and the labels, the values one line each inside fences.
func alertIssue(a SigNozAlert, project, ns string) (title, body string) {
	name := oneLine(a.Labels["alertname"])
	var b strings.Builder
	fmt.Fprintf(&b, "factoryd: SigNoz fires the alert %s in namespace %s of project %s.\n\nFiring since %s.", name, ns, project, stamp(a.StartsAt))
	if safeLink(a.GeneratorURL) {
		b.WriteString(" SigNoz: " + a.GeneratorURL)
	}
	notes := []string{"value=" + oneLine(cmp.Or(a.Annotations["value"], "not reported"))}
	for _, k := range slices.Sorted(maps.Keys(a.Annotations)) {
		if k != "value" {
			notes = append(notes, k+"="+oneLine(a.Annotations[k]))
		}
	}
	var labels []string
	for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
		if k != "ruleSource" {
			labels = append(labels, oneLine(k)+"="+oneLine(a.Labels[k]))
		}
	}
	fmt.Fprintf(&b, "\n\nValue and annotations:\n\n%s\n\nLabels:\n\n%s\n\nfiled by factoryd from SigNoz alert %s",
		fenced(strings.Join(notes, "\n")), fenced(strings.Join(labels, "\n")), oneLine(a.Fingerprint))
	return "alert: " + name + " in " + ns, b.String()
}

// alertVerdict is the verdict a SigNoz alert's task gives it when it ends
// final for reason: real on a merge, noise on its issue closed without one.
func alertVerdict(final, reason string) string {
	switch {
	case final == "done":
		return VerdictReal
	case strings.HasPrefix(reason, "issue closed"):
		return VerdictNoise
	}
	return ""
}

func namespaceOf(labels map[string]string) string {
	for _, l := range namespaceLabels {
		if v := labels[l]; v != "" {
			return v
		}
	}
	return ""
}

func issueOf(a Alert) int64 {
	switch n := a.Detail["issue"].(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func safeLink(u string) bool {
	return (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && !strings.ContainsAny(u, " \n\t()<>[]")
}
