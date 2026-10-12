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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// SourceAlert is the source of a task filed from a SigNoz alert.
const SourceAlert = "alert"

// refileAfter is how long after its task merged an alert still firing from
// the same start is filed once more: the merged fix did not fix it.
const refileAfter = 30 * time.Minute

// restartWindowAlert is the only rule a roll's restart trips for nothing
// (ruling 237): k8s/alerts/5xx-rate.json's "alert". The restarts and
// roll-not-live rules are what a broken roll trips, so they still file.
const restartWindowAlert = "5xx rate above 5 %"

// restartWindow is how long after a Deployment is live its alerts still
// count as the roll's own restart, not a real problem (ruling 237).
const restartWindow = 10 * time.Minute

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
	AlertByFingerprint(ctx context.Context, kind, fingerprint string) (Alert, bool, error)
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
	// Timeline records the events of the cards it files.
	Timeline *Timeline
	Budget   func() policy.Budget
	// Kube reads a roll target's Deployment, to tell a restartWindowAlert
	// inside its restart window from a real one (item 127). Namespace is
	// the factory's own namespace, the one those Deployments run in.
	Kube      DeploymentReader
	Namespace string
	// LinkHosts is the policy's link_hosts, besides the Forgejo host and a
	// project's own environment hosts, allowed unmangled in a posted alert.
	LinkHosts func() []string

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
		// SigNoz sends the notification again on a 5xx. The alerts row, the
		// dedupe key, is written first with its task's id, so the retry files
		// that one task; a failure between the issue and the task row leaves
		// the first issue without a task
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
	row, seen, err := in.Store.AlertByFingerprint(ctx, AlertSigNoz, a.Fingerprint)
	if err != nil {
		return err
	}
	t, filed, err := in.Store.Task(ctx, row.TaskID)
	if err != nil {
		return err
	}
	repo := p.Repo()
	if a.Status == "resolved" {
		at := stamp(a.EndsAt)
		if !filed || row.Detail["resolved_at"] == at {
			return nil
		}
		if _, err := in.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, "factoryd: resolved at "+at); err != nil {
			return err
		}
		return in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"resolved_at": at})
	}
	at := stamp(a.StartsAt)
	if !seen {
		return in.fileUnlessRestartWindow(ctx, a, p, ns, at, name)
	}
	if !filed {
		if row.Verdict != "" {
			if row.Detail["starts_at"] == at {
				// the exact same firing redelivered: nothing
				return nil
			}
			// Alertmanager's fingerprint hashes the alert's label set, the
			// same across every firing of this rule in this namespace, so a
			// later, distinct start is not this row's redelivery: judged
			// again rather than trusted on the strength of the first verdict
			return in.fileUnlessRestartWindow(ctx, a, p, ns, at, name)
		}
		// an earlier delivery wrote the row and failed before its task
		// was filed: file it now, under the row's task id
		return in.fileTask(ctx, a, p, ns, row)
	}
	if row.Detail["starts_at"] == at {
		// the same firing again (Alertmanager resends a group's every
		// alert): nothing, unless the task's fix merged a while ago and it
		// still fires, filed once more; a human's close is their answer
		if t.Status == "done" && t.FinishedAt != nil && time.Since(*t.FinishedAt) > refileAfter && row.Detail["refiled"] != true {
			return in.file(ctx, a, p, ns, at, true)
		}
		return nil
	}
	if !Final(t.Status) {
		msg := "factoryd: the alert fired again at " + at + ".\n\n" + fenced(sanitize.Outbound("value="+oneLine(a.Annotations["value"]), in.hostsFor(p)))
		if _, err := in.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, msg); err != nil {
			return err
		}
		return in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"starts_at": at})
	}
	// the fingerprint's task already closed: a roll can retrigger this same
	// fingerprint (item 127, fix round 2), so it is judged again rather than
	// filed outright
	return in.fileUnlessRestartWindow(ctx, a, p, ns, at, name)
}

// fileUnlessRestartWindow files a as a fresh issue and task, unless it is
// the restart-window rule firing inside a roll's restart window, in which
// case judgeRestartWindow has already recorded it as noise.
func (in *AlertIntake) fileUnlessRestartWindow(ctx context.Context, a SigNozAlert, p Project, ns, at, name string) error {
	noise, err := in.judgeRestartWindow(ctx, a, name, ns)
	if err != nil {
		return err
	}
	if noise {
		return nil
	}
	return in.file(ctx, a, p, ns, at, false)
}

// judgeRestartWindow records a's row as noise and reports true when name is
// the restart-window rule firing in the factory's own namespace inside a
// roll target's restart window (item 127); otherwise it reports false and
// leaves filing it to the caller. Without Kube or Namespace wired there is no
// Deployment to read: judging then panics on the nil pointer, and the 500 the
// panic answers is what SigNoz retried until this very rule fired on
// factoryd's own 5xx share — so an intake that cannot judge files instead.
func (in *AlertIntake) judgeRestartWindow(ctx context.Context, a SigNozAlert, name, ns string) (bool, error) {
	if name != restartWindowAlert || ns != in.Namespace {
		return false, nil
	}
	if in.Kube == nil || in.Namespace == "" {
		// an intake that cannot read the roll targets' Deployments cannot
		// judge: file like any other rule rather than lose the alert to the
		// panic on the nil reader
		log.Printf("signoz webhook: alert %s in %s: no Deployment reader wired, filing without the restart-window check",
			strconv.Quote(name), strconv.Quote(ns)) // #nosec G706 -- quoted, ns is a checked DNS label
		return false, nil
	}
	dep, applied, live, inWindow, err := in.inRestartWindow(ctx, a.StartsAt)
	if err != nil {
		return false, err
	}
	if !inWindow {
		return false, nil
	}
	return true, in.recordRestartWindowNoise(ctx, a, dep, applied, live)
}

// inRestartWindow reports whether starts lies in a roll target Deployment's
// restart window, in the factory's own namespace: from Flux applying the
// spec to restartWindow after the Deployment is live, open-ended while the
// rollout still runs (live not reached yet). deployment names the
// Deployment the window matched.
func (in *AlertIntake) inRestartWindow(ctx context.Context, starts time.Time) (deployment string, applied, live time.Time, ok bool, err error) {
	checked := map[string]bool{}
	for _, target := range RollTargets {
		if checked[target.Deployment] {
			continue
		}
		checked[target.Deployment] = true
		d, derr := in.Kube.Deployment(ctx, in.Namespace, target.Deployment)
		if derr != nil {
			if errors.Is(derr, ErrNotFound) {
				continue
			}
			return "", time.Time{}, time.Time{}, false, derr
		}
		at, hasApplied := d.applied()
		if !hasApplied || starts.Before(at) {
			continue
		}
		if up, hasLive := d.live(); hasLive {
			if starts.Before(up.Add(restartWindow)) {
				return target.Deployment, at, up, true, nil
			}
			continue
		}
		return target.Deployment, at, time.Time{}, true, nil
	}
	return "", time.Time{}, time.Time{}, false, nil
}

// recordRestartWindowNoise writes a's row as noise, resolved by factoryd,
// because its startsAt lies in deployment's restart window: no issue or
// task files. Its starts_at, read back by judgeRestartWindow, tells this
// exact firing's redelivery from a later firing of the same fingerprint
// (Alertmanager's fingerprint never changes between firings of this rule),
// which is judged again rather than trusted on the strength of this one.
func (in *AlertIntake) recordRestartWindowNoise(ctx context.Context, a SigNozAlert, deployment string, applied, live time.Time) error {
	detail := map[string]any{"roll_window": "roll window of " + deployment, "applied": stamp(applied), "starts_at": stamp(a.StartsAt)}
	if !live.IsZero() {
		detail["live"] = stamp(live)
	}
	_, err := in.Store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: oneLine(a.Labels["alertname"]),
		Fingerprint: a.Fingerprint, Verdict: VerdictNoise, ResolvedBy: "factoryd", Detail: detail})
	return err
}

// file writes the alert's row, the dedupe key, with the id of the task it
// files, then files that task; refiled when the same firing outlived the
// task of the last one. A delivery that fails after the row is written is
// finished by SigNoz's retry under the same task id (take).
func (in *AlertIntake) file(ctx context.Context, a SigNozAlert, p Project, ns, at string, refiled bool) error {
	link := ""
	if safeLink(a.GeneratorURL) {
		link = a.GeneratorURL
	}
	row := Alert{Kind: AlertSigNoz, Source: oneLine(a.Labels["alertname"]), TaskID: NewTaskID(), Fingerprint: a.Fingerprint,
		Detail: map[string]any{"project": p.Name, "namespace": ns, "repository": p.Repo().String(),
			"starts_at": at, "link": link, "labels": a.Labels, "refiled": refiled}}
	var err error
	if row.ID, err = in.Store.InsertAlert(ctx, row); err != nil {
		return err
	}
	return in.fileTask(ctx, a, p, ns, row)
}

// fileTask files the issue and task of the alert a on project p whose row,
// written first, names the task's id: a delivery that fails here leaves the
// row, so SigNoz's retry files the same one task (take).
func (in *AlertIntake) fileTask(ctx context.Context, a SigNozAlert, p Project, ns string, row Alert) error {
	repo := p.Repo()
	sha, err := in.Forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, "")
	if err != nil {
		return fmt.Errorf("base sha: %w", err)
	}
	title, body := alertIssue(a, p.Name, ns, in.hostsFor(p))
	if row.Detail["refiled"] == true {
		body = "factoryd: the alert still fires from the same start " + refileAfter.String() + " after the fix of its last issue merged.\n\n" + body
	}
	t := Task{ID: row.TaskID, RiskClass: "standard", BudgetEUR: in.Budget().ClampBudget(0),
		Objective: title + "\n\n" + body, SHA: sha, Source: SourceAlert,
		// factoryd files as the bot; the brief marks the text as telemetry
		Author: in.Forgejo.BotUser}
	var ref IssueRef
	if p.Agents == policy.AgentsNone {
		t.Kind = "human"
		t, ref, err = FileTask(ctx, in.Store, in.Forgejo, repo, title, body, []string{"bug", "human"}, t)
	} else {
		t, ref, err = FileAgentTask(ctx, in.Store, in.Forgejo, in.Workflows, in.Timeline, p, title, body, []string{"bug"}, t)
	}
	if err != nil {
		return err
	}
	if err := in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"issue": ref.Number}); err != nil {
		log.Printf("signoz webhook: alert row %d: record issue #%d: %v", row.ID, ref.Number, err)
	}
	log.Printf("signoz webhook: alert %s in %s: issue #%d on %s, task %s", strconv.Quote(a.Labels["alertname"]), ns, ref.Number, repo, t.ID) // #nosec G706 -- quoted, ns is a checked DNS label
	return nil
}

// hostsFor is Outbound's hosts argument for an alert on project p: the
// policy's link_hosts, the Forgejo host and p's own environment hosts
// (the same hosts (*Runner).untrusted allows an agent's posted text).
func (in *AlertIntake) hostsFor(p Project) []string {
	var hosts []string
	if in.LinkHosts != nil {
		hosts = append(hosts, in.LinkHosts()...)
	}
	if u, err := url.Parse(in.Forgejo.PublicURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	for _, e := range p.Environments {
		if e.Hostname != "" {
			hosts = append(hosts, e.Hostname)
		}
	}
	return hosts
}

// project is the project owning namespace ns: its factory/project label,
// read once, naming an active project that lists ns.
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
	return p, p.Active() && slices.Contains(p.Namespaces, ns), nil
}

// alertIssue is the bug issue of alert a: title "alert: <name> on
// <service> in <ns>" (no "on" without a service) and a body with the
// firing time, the SigNoz link, the value and annotations and the labels,
// the values one line each inside fences. The text goes to Forgejo, so it
// is sanitised Outbound: hosts is the links left unmangled.
func alertIssue(a SigNozAlert, project, ns string, hosts []string) (title, body string) {
	name, on := oneLine(a.Labels["alertname"]), ""
	if s := oneLine(cmp.Or(a.Labels["service"], a.Labels["service.name"], a.Labels["k8s.deployment.name"], a.Labels["deployment"])); s != "" {
		on = " on service " + s
		title = "alert: " + name + " on " + s + " in " + ns
	} else {
		title = "alert: " + name + " in " + ns
	}
	var b strings.Builder
	fmt.Fprintf(&b, "factoryd: SigNoz fires the alert %s%s in namespace %s of project %s.\n\nFiring since %s.", name, on, ns, project, stamp(a.StartsAt))
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
	return sanitize.Outbound(title, hosts), sanitize.Outbound(b.String(), hosts)
}

// The final reasons of a task whose issue a human closed; the merge state
// may be unknown.
const (
	ReasonIssueClosed  = "issue closed"
	ReasonMergeUnknown = "; merge state unknown: "
)

// alertVerdict is the verdict a SigNoz alert's task gives it when it ends
// final for reason: real on a merge, noise on its issue closed without one,
// none while the merge state is unknown.
func alertVerdict(final, reason string) string {
	switch {
	case final == "done":
		return VerdictReal
	case strings.HasPrefix(reason, ReasonIssueClosed) && !strings.HasPrefix(reason, ReasonIssueClosed+ReasonMergeUnknown):
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

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// oneLine is s on one line, whitespace collapsed; sanitize.Outbound, applied
// to the whole issue or comment before it posts, neutralises any mention.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func safeLink(u string) bool {
	return (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && !strings.ContainsAny(u, " \n\t()<>[]")
}
