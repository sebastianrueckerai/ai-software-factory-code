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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// SourceAlert is the source of a task filed from a SigNoz alert.
const SourceAlert = "alert"

// refileAfter is how long after its task merged an alert still firing from
// the same start is filed once more: the merged fix did not fix it.
const refileAfter = 30 * time.Minute

// eventLabel, "true" on a rule, marks its firing as an event that
// happened, a Job's failure, not a state that holds: SigNoz's resolve only
// says the rule's window moved on, so it neither comments on nor ends the
// card.
const eventLabel = "event"

// restartWindowAlert is the only rule a roll of the factory's own
// Deployments holds (ruling 237): k8s/alerts/5xx-rate.json's "alert". The
// restarts and roll-not-live rules are what a broken roll trips, so they
// file at once.
const restartWindowAlert = "5xx rate above 5 %"

// rolloutSettle is how long after a rollout settled its alerts still count
// as the rollout's own (ruling 237's ten minutes): a rule reads a window of
// samples (the 5xx rule five minutes, evaluated each minute), so a burst
// that ends as the rollout settles fires for up to six minutes more, and
// what a rollout restarts without Flux waiting on it (CNPG's switchover of
// a Cluster the platform Kustomization applied) settles after Flux does.
const rolloutSettle = 10 * time.Minute

// rolloutCap is the longest a rollout's window lasts after Flux applied
// it, settled or not. A normal apply settles well inside it and keeps its
// whole settle: the platform pushes of 2026-10-15 settled seven and eight
// minutes after Flux applied them, the slowest HelmRelease six and a half;
// Helm gives up an action at its five-minute timeout and Kubernetes a
// Deployment at its ten-minute progress deadline. A rollout not settled
// by then holds no alert any more and raises its own (RolloutPass).
const rolloutCap = 30 * time.Minute

// fluxAlert is FluxPass's alert: an environment whose Flux Kustomization
// applied once stands not Ready (roadmap 259).
const fluxAlert = "Kustomization not Ready"

// fluxPrefix starts a fluxAlert's fingerprint, flux/<namespace>/<name>,
// and rolloutPrefix a stuck rollout's, rollout/<kind>/<namespace>/<name>
// (RolloutPass); SigNoz's fingerprints are hexadecimal.
const (
	fluxPrefix    = "flux/"
	rolloutPrefix = "rollout/"
)

// fromFlux reports whether fingerprint is of an alert factoryd raises
// from Flux's status, not SigNoz's.
func fromFlux(fingerprint string) bool {
	return strings.HasPrefix(fingerprint, fluxPrefix) || strings.HasPrefix(fingerprint, rolloutPrefix)
}

// fluxGrace is how long a Kustomization that applied once reads not
// Ready before its alert fires: ten of its one-minute reconciles, so a
// failure Flux's own retry heals (an API server or Forgejo blip) files
// nothing, while a broken environment files within the grace and a poll.
const fluxGrace = 10 * time.Minute

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
	// swept is SweepSigNoz's resolve, found rather than delivered: only
	// the issue of a task still running hears of it
	swept bool
}

// NamespaceLabeler reads a label of a namespace.
type NamespaceLabeler interface {
	NamespaceLabel(ctx context.Context, name, key string) (string, error)
}

// DeployedCommitReader reads the commit a Deployment of the factory's own
// namespace runs (PGStore over RollWatcher's rolls rows): the tree that
// was live when an alert on it fired. An intake wired with one pins a
// signal card's reproduction base at filing (roadmap 194).
type DeployedCommitReader interface {
	DeployedCommit(ctx context.Context, deployment string) (string, bool, error)
}

// FluxReader reads Flux's Kustomizations and HelmReleases (InClusterKube).
type FluxReader interface {
	KustomizationState(ctx context.Context, namespace, name string) (FluxState, error)
	// FluxRollouts are the newest rollouts of Flux's objects, the
	// factory's own Kustomization in namespace not one of them.
	FluxRollouts(ctx context.Context, namespace string) ([]Rollout, error)
}

// Rollout is one object's newest rollout as its status tells it: Flux
// applied a change to it at Applied, and it settled at Settled, zero while
// it still runs.
type Rollout struct {
	Kind, Namespace, Name string
	// Scope is the one namespace whose alerts the rollout holds, a project
	// environment's; "" holds every namespace's, as the platform's does.
	Scope            string
	Applied, Settled time.Time
	// Message is Flux's message on the Ready condition.
	Message string
}

func (r Rollout) object() string { return r.Kind + " " + r.Namespace + "/" + r.Name }

// holds reports whether r's window holds an alert in namespace ns that
// starts at at: ns in r's Scope, at from Applied until rolloutSettle after
// Settled, rolloutCap after Applied at the latest.
func (r Rollout) holds(at time.Time, ns string) bool {
	end := r.Applied.Add(rolloutCap)
	if !r.Settled.IsZero() && r.Settled.Add(rolloutSettle).Before(end) {
		end = r.Settled.Add(rolloutSettle)
	}
	return !r.Applied.IsZero() && (r.Scope == "" || r.Scope == ns) && !at.Before(r.Applied) && at.Before(end)
}

// AlertBook is what the alert intake keeps: the task rows it files and one
// alerts row per issue, found again by the alert's fingerprint.
type AlertBook interface {
	Store
	MergeAlertDetail(ctx context.Context, id int64, detail map[string]any) error
	NewestAlertRows(ctx context.Context, kind, namespace string) ([]Alert, error)
	CloseClearedCard(ctx context.Context, alertID int64, taskID string) error
}

// AlertIntake serves POST /webhooks/signoz, SigNoz's webhook channel
// factoryd (basic auth from secret signoz-webhook). An alert's namespace
// (namespaceOf) names the project through the namespace's factory/project
// label;
// the first firing of a fingerprint files a bug issue on the project's
// repository as the bot and starts its task, a later firing is a comment
// on it while the task runs and a new issue once it ended, and a resolve
// is a comment. An alert no project owns is logged and dropped. A firing
// that starts inside a rollout's window is held, filed only if it still
// fires once no window holds (fileUnlessHeld). FluxPass takes factoryd's
// own alert of a broken environment the same way, and SweepSigNoz resolves
// the rows SigNoz no longer lists, so the alerts rows are the one answer
// to whether an alert fires.
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
	// inside its roll's window from a real one (item 127). Namespace is
	// the factory's own namespace, the one those Deployments run in. An
	// intake serving that namespace must name Kube: take answers a plain
	// error on this rule rather than dereference a nil reader.
	Kube      DeploymentReader
	Namespace string
	// Health reads the alerting service's Deployment into the card it
	// files; nil = the card carries no such section.
	Health DeploymentHealthReader
	// Rolls reads the deployed build's commit of a Deployment of the
	// factory's own namespace (Namespace), from the rolls the same
	// factoryd records: a signal card's filing pins it as the task's
	// reproduction base (roadmap 194). nil = the base is main's head at
	// the filing.
	Rolls DeployedCommitReader
	// LinkHosts is the policy's link_hosts, besides the Forgejo host and a
	// project's own environment hosts, allowed unmangled in a posted alert.
	LinkHosts func() []string
	// Flux reads the Kustomizations FluxPass watches and the rollouts
	// whose windows hold alerts: the platform's every alert, a project
	// environment's its namespace's; nil = no Flux rollout holds one.
	Flux FluxReader
	// SigNoz lists the alerts SigNoz holds firing, for SweepSigNoz.
	SigNoz AlertSource
	// Metrics counts the cards CloseCleared ends; nil counts none.
	Metrics *telemetry.Metrics

	// one notification at a time: two deliveries of a new fingerprint
	// file one issue
	mu        sync.Mutex
	projectOf map[string]string // namespace -> its factory/project label
	// a fluxAlert's fingerprint -> its first False read since the last Ready
	fluxSince map[string]time.Time
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
		if err := in.take(r.Context(), a, time.Now()); err != nil {
			if row, seen, rerr := in.Store.AlertByFingerprint(r.Context(), AlertSigNoz, a.Fingerprint); rerr == nil &&
				seen && in.replays(r.Context(), row, a) {
				// The failure sits after the alerts row, the dedupe key, was
				// written: whatever step of the delivery is left — the filing
				// under the row's task id, the fired-again comment on its
				// running task's issue, the resolve — a later delivery of the
				// firing walks again, and the sweep finishes a filing or a
				// resolve left behind. Answering 5xx here made SigNoz
				// redeliver the failure again and again, every retry another
				// 5xx request on factoryd, until the 5xx-rate rule fired on
				// factoryd itself (8 of 141 requests,
				// 2026-10-16T16:04:45Z). Accepted, not failed: the 5xx stays
				// for a failure before the row — the project read, the
				// insert — whose only repair is SigNoz's redelivery.
				log.Printf("signoz webhook: alert %s: %v: the row is written, a redelivery or the sweep finishes it", strconv.Quote(a.Fingerprint), err) // #nosec G706 -- strconv.Quote escapes the alert's text
				continue
			}
			errs = append(errs, fmt.Errorf("alert %s: %w", strconv.Quote(a.Fingerprint), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		// SigNoz sends the notification again on a 5xx, the repair for a
		// failure before the alerts row was written: after the row, a 5xx
		// only adds factoryd's own failed requests without adding repair
		log.Printf("signoz webhook: %v", err) // #nosec G706 -- strconv.Quote escapes the alert's text
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// replays reports whether alert a's failed delivery already wrote the
// durable state its remaining work replays against: its alerts row, the
// dedupe key, found as row. A resolve only finishes that row, whatever
// step of it failed, and a firing of the row's own start replays against
// it — an unfiled filing continued under the row's task id, a refile of a
// firing its task did not end tried again; the fired-again comment on the
// issue of the row's still-running task is posted by the next delivery of
// the firing. A delivery that files anew — no row, or a start of its own
// once the row's task ended — repairs only through SigNoz's redelivery,
// which the 5xx asks for; a task read that itself fails cannot tell the
// two apart, so the delivery fails for its redelivery.
func (in *AlertIntake) replays(ctx context.Context, row Alert, a SigNozAlert) bool {
	if a.Status == "resolved" || row.Detail["starts_at"] == stamp(a.StartsAt) {
		return true
	}
	t, filed, err := in.Store.Task(ctx, row.TaskID)
	return err == nil && filed && row.Detail["issue"] != nil && !Final(t.Status)
}

// take judges alert a at now, the time of its delivery or of the pass that
// found it.
func (in *AlertIntake) take(ctx context.Context, a SigNozAlert, now time.Time) error {
	name, ns := a.Labels["alertname"], namespaceOf(a.Labels)
	p, ok, err := in.project(ctx, ns)
	if err != nil {
		return err
	}
	// a resolve still closes the row of a project gone since it fired
	if (!ok && a.Status != "resolved") || a.Fingerprint == "" {
		log.Printf("signoz webhook: alert %s in namespace %s: no project owns it, dropped", strconv.Quote(name), strconv.Quote(ns)) // #nosec G706 -- quoted
		return nil
	}
	// The roll targets' Deployments are only read to judge the
	// restart-window rule, and an intake that serves this rule's namespace
	// must name that reader: without one there is nothing to judge on, so
	// the delivery fails plainly instead of dereferencing the nil reader.
	// The panic raised there answered the empty 500 SigNoz could not read
	// and retried until this very rule fired on factoryd's own 5xx share.
	if in.Kube == nil && name == restartWindowAlert && ns == in.Namespace {
		return fmt.Errorf("alert %s in namespace %s: no Deployment reader wired, cannot tell a roll's restart window from a real firing",
			strconv.Quote(name), strconv.Quote(ns))
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
		// the row is resolved even when its filing failed, so it no longer
		// fires; its issue, if its project still owns one, hears of it,
		// unless a sweep found the resolve after the task ended or the
		// alert is an event
		at := stamp(a.EndsAt)
		if !seen || row.Detail["resolved_at"] == at {
			return nil
		}
		if !filed && held(row) {
			// it ended before a window let it file: the rollout's own
			return in.Store.ResolveAlert(ctx, row.ID, AlertSigNoz, VerdictNoise, "factoryd", map[string]any{"resolved_at": at})
		}
		if filed && ok && !(a.swept && Final(t.Status)) && row.Detail["event"] != true {
			if _, err := in.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, "factoryd: resolved at "+at); err != nil {
				return err
			}
		}
		return in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"resolved_at": at})
	}
	at := stamp(a.StartsAt)
	if !seen {
		return in.fileUnlessHeld(ctx, a, p, ns, at, name)
	}
	if !filed {
		switch {
		case row.Detail["starts_at"] != at:
			// Alertmanager's fingerprint hashes the alert's label set, the
			// same across every firing of this rule in this namespace, so a
			// later, distinct start is not this row's redelivery: judged
			// again rather than trusted on the strength of the row's verdict
			// or filed under the task its failed filing never made. A held
			// row's firing ended unfiled: the rollout's own.
			if held(row) {
				if err := in.Store.ResolveAlert(ctx, row.ID, AlertSigNoz, VerdictNoise, "factoryd", map[string]any{}); err != nil {
					return err
				}
			}
			return in.fileUnlessHeld(ctx, a, p, ns, at, name)
		case row.Verdict != "":
			// the exact same firing redelivered: nothing
			return nil
		case held(row):
			// held for a rollout: filed once no rollout's window holds now
			if _, open, err := in.rolloutAt(ctx, now, name, ns); err != nil || open {
				return err
			}
		}
		// an earlier delivery wrote the row and failed before its task
		// was filed, or a rollout held it: file it now, under the row's
		// task id
		return in.fileTask(ctx, a, p, ns, row)
	}
	if row.Detail["starts_at"] == at {
		// the same firing again (Alertmanager resends a group's every
		// alert): nothing, unless the task's fix merged a while ago and it
		// still fires, filed once more; a human's close is their answer
		if t.Status == "done" && t.FinishedAt != nil && now.Sub(*t.FinishedAt) > refileAfter && row.Detail["refiled"] != true {
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
	return in.fileUnlessHeld(ctx, a, p, ns, at, name)
}

// FluxPass raises fluxAlert, through take as a SigNoz alert would, on each
// environment of factory project p whose Kustomization applied once and
// has read False for fluxGrace, with Flux's message and the action, and
// resolves it once the Kustomization is Ready again (roadmap 259). A path
// no green build wrote never applied, so it raises none; Unknown, Flux
// mid-reconcile, changes nothing. A fired alert's start is its alerts
// row's, so a restart of factoryd neither fires it anew nor misses its
// resolve.
func (in *AlertIntake) FluxPass(ctx context.Context, p Project, now time.Time) {
	for _, e := range p.Environments {
		s, err := in.Flux.KustomizationState(ctx, e.Namespace, p.Name)
		if err == nil {
			err = in.flux(ctx, p.Name, e, s, now)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("project %s: %s flux alert: %v", p.Name, e.Namespace, err)
		}
	}
}

// flux is FluxPass on environment e, whose Kustomization name reads s.
func (in *AlertIntake) flux(ctx context.Context, name string, e Environment, s FluxState, now time.Time) error {
	fp := fluxPrefix + e.Namespace + "/" + name
	in.mu.Lock()
	defer in.mu.Unlock()
	row, _, err := in.Store.AlertByFingerprint(ctx, AlertSigNoz, fp)
	if err != nil {
		return err
	}
	start, open := firingSince(row)
	a := SigNozAlert{Status: "firing", StartsAt: start, Fingerprint: fp,
		Labels: map[string]string{"alertname": fluxAlert, "namespace": e.Namespace, "path": e.Path, "kustomization": name},
		Annotations: map[string]string{"value": s.Message, "applied": s.Applied,
			"action": "fix on main what Flux's message names: main's green builds write " + e.Path + " to the deploy branch, " +
				"which Flux applies to " + e.Namespace + ". A cause outside the repository is a question for a human."}}
	switch {
	case s.Ready == "True":
		delete(in.fluxSince, fp)
		if !open {
			return nil
		}
		a.Status, a.EndsAt = "resolved", now
	case s.Ready != "False":
		return nil
	case s.Applied == "":
		delete(in.fluxSince, fp)
		return nil
	case !open:
		since, ok := in.fluxSince[fp]
		if !ok {
			if in.fluxSince == nil {
				in.fluxSince = map[string]time.Time{}
			}
			in.fluxSince[fp] = now
			return nil
		}
		if now.Sub(since) < fluxGrace {
			return nil
		}
		a.StartsAt = since
	}
	return in.take(ctx, a, now)
}

// SweepSigNoz resolves, through take as its lost resolve delivery would
// have, each SigNoz alerts row that still fires while SigNoz no longer
// lists its fingerprint as active, so a row stays true to SigNoz (the rows
// of the alerts factoryd raises from Flux are FluxPass's and RolloutPass's). The issue of a finished task hears
// nothing, so the stale rows a first sweep finds resolve quietly and only
// a running card's issue gets the comment. A held row SigNoz still lists
// goes to take as its redelivery would, which files it once no rollout's
// window holds now, and so does a row whose filing failed after it was
// written: the webhook accepts that delivery, this sweep is its repair. A
// list whose active alerts carry no fingerprint cannot
// be matched and resolves nothing. The rows are read before the list, so a
// row filed meanwhile waits for the next sweep.
func (in *AlertIntake) SweepSigNoz(ctx context.Context, now time.Time) error {
	rows, err := in.Store.NewestAlertRows(ctx, AlertSigNoz, "")
	if err != nil {
		return err
	}
	live, err := in.SigNoz.FiringAlerts(ctx)
	if err != nil {
		return fmt.Errorf("signoz: %w", err)
	}
	active := map[string]FiringAlert{}
	for _, a := range live {
		if a.Status.State != "active" {
			continue
		}
		fp := cmp.Or(a.Fingerprint, a.Labels["fingerprint"])
		if fp == "" {
			return fmt.Errorf("signoz lists the active alert %s without a fingerprint: no row resolved", strconv.Quote(a.Labels["alertname"]))
		}
		active[fp] = a
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	var errs []error
	for _, row := range rows {
		f, listed := active[row.Fingerprint]
		var a SigNozAlert
		switch _, firing := firingSince(row); {
		case !firing || fromFlux(row.Fingerprint):
			continue
		case !listed:
			ns, _ := row.Detail["namespace"].(string)
			a = SigNozAlert{Status: "resolved", EndsAt: now, Fingerprint: row.Fingerprint,
				Labels: map[string]string{"alertname": row.Source, "namespace": ns}, swept: true}
		case row.Detail["issue"] == nil && (held(row) || row.Detail["starts_at"] == stamp(f.StartsAt)):
			// held for a rollout, or a filing that failed after its row was
			// written: taken as its redelivery would, which files it
			a = SigNozAlert{Status: "firing", Labels: f.Labels, Annotations: f.Annotations, StartsAt: f.StartsAt,
				GeneratorURL: f.GeneratorURL, Fingerprint: row.Fingerprint}
		default:
			continue
		}
		if err := in.take(ctx, a, now); err != nil {
			errs = append(errs, fmt.Errorf("alert %s: %w", strconv.Quote(row.Fingerprint), err))
		}
	}
	return errors.Join(errs...)
}

// CloseCleared closes each human card of a cleared SigNoz alert that no
// person touched: the row's verdict is unjudged, the task closed before
// its issue, so the issue-closed webhook finds it final and judges
// nothing. A card an agent works, a row still firing or judged, a card
// that left waiting-human, and a card a person commented on, labelled
// noise, answered or parked are left as they are, and so is an event's
// card (eventLabel): no resolve clears it. A touch is marked on the row,
// so a touched card is never read again.
func (in *AlertIntake) CloseCleared(ctx context.Context) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	rows, err := in.Store.NewestAlertRows(ctx, AlertSigNoz, "")
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		at, _ := row.Detail["resolved_at"].(string)
		if _, firing := firingSince(row); row.Verdict != "" || at == "" || firing || row.Detail["touched"] == true || row.Detail["event"] == true {
			continue
		}
		if err := in.closeCleared(ctx, row, at); err != nil {
			errs = append(errs, fmt.Errorf("alert %s: %w", strconv.Quote(row.Fingerprint), err))
		}
	}
	return errors.Join(errs...)
}

// touched reports whether a person acted on human card t: an intake row, an
// answer or park, a comment on its issue by anyone but the bot, or the
// noise label, a verdict that waits on the person's close.
func (in *AlertIntake) touched(ctx context.Context, t Task, repo Repo) (bool, error) {
	if touched, err := in.Store.TouchedByPerson(ctx, t.ID, in.Forgejo.BotUser); err != nil || touched {
		return touched, err
	}
	comments, err := in.Forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(comments, func(c IssueComment) bool { return c.Author != in.Forgejo.BotUser }) {
		return true, nil
	}
	labels, err := in.Forgejo.IssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber)
	return slices.Contains(labels, LabelNoise), err
}

func (in *AlertIntake) closeCleared(ctx context.Context, row Alert, at string) error {
	t, ok, err := in.Store.Task(ctx, row.TaskID)
	if err != nil || !ok || t.Kind != "human" || t.Status != "waiting-human" {
		return err
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	switch touched, err := in.touched(ctx, t, repo); {
	case err != nil:
		return err
	case touched:
		return in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"touched": true})
	}
	if err := in.Store.CloseClearedCard(ctx, row.ID, t.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	countOutcome(ctx, in.Metrics, in.Store, t, "closed")
	mirrorTaskState(ctx, in.Forgejo, t, "closed")
	taskEnded(ctx, in.Timeline, in.Store, in.Forgejo, in.Workflows, t, "closed", "the alert cleared at "+at)
	return closeIssueAsFactory(ctx, in.Store, in.Forgejo, t, "factoryd: the alert cleared at "+at+"; nobody touched the card, so it closes.")
}

// RolloutPass raises an alert, through take as a SigNoz alert would, on
// each Flux object whose rollout has not settled rolloutCap after Flux
// applied it: its window no longer holds any alert, so a stuck rollout
// silences none, and the alert names the object, Flux's message and the
// action. It resolves once the rollout settled or the object is gone. The
// factory's own Deployments' rolls have SigNoz's roll-not-live rules.
func (in *AlertIntake) RolloutPass(ctx context.Context, now time.Time) error {
	if in.Flux == nil {
		return nil
	}
	rollouts, err := in.Flux.FluxRollouts(ctx, in.Namespace)
	if err != nil {
		return fmt.Errorf("flux rollouts: %w", err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	rows, err := in.Store.NewestAlertRows(ctx, AlertSigNoz, "")
	if err != nil {
		return err
	}
	var errs []error
	stuck := map[string]bool{}
	for _, r := range rollouts {
		capped := r.Applied.Add(rolloutCap)
		if r.Applied.IsZero() || !r.Settled.IsZero() || now.Before(capped) {
			continue
		}
		fp := rolloutPrefix + r.Kind + "/" + r.Namespace + "/" + r.Name
		stuck[fp] = true
		a := SigNozAlert{Status: "firing", StartsAt: capped, Fingerprint: fp,
			Labels: map[string]string{"alertname": r.Kind + " " + r.Name + " not settled", "namespace": r.Namespace, "kind": r.Kind, "name": r.Name},
			Annotations: map[string]string{"value": r.Message, "applied": stamp(r.Applied),
				"action": "find why Flux has not settled " + r.object() + " " + rolloutCap.String() + " after applying it, from its status and events; " +
					"its window holds no alert any more. A cause outside the repository is a question for a human."}}
		if err := in.take(ctx, a, now); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.object(), err))
		}
	}
	for _, row := range rows {
		if _, firing := firingSince(row); !firing || !strings.HasPrefix(row.Fingerprint, rolloutPrefix) || stuck[row.Fingerprint] {
			continue
		}
		ns, _ := row.Detail["namespace"].(string)
		a := SigNozAlert{Status: "resolved", EndsAt: now, Fingerprint: row.Fingerprint,
			Labels: map[string]string{"alertname": row.Source, "namespace": ns}}
		if err := in.take(ctx, a, now); err != nil {
			errs = append(errs, fmt.Errorf("alert %s: %w", strconv.Quote(row.Fingerprint), err))
		}
	}
	return errors.Join(errs...)
}

// firingSince is the start alerts row a records, and whether it still
// fires: no resolve recorded at or after that start.
func firingSince(a Alert) (time.Time, bool) {
	at, _ := a.Detail["starts_at"].(string)
	resolved, _ := a.Detail["resolved_at"].(string)
	start, err := time.Parse(time.RFC3339, at)
	return start, err == nil && resolved < at
}

// held reports whether row is a firing a rollout's window held, not
// filed and not judged yet.
func held(row Alert) bool {
	return row.Verdict == "" && row.Detail["roll_window"] != nil
}

// fileUnlessHeld files a as a fresh issue and task, unless a rollout's
// window holds its start (rolloutAt): then a's row is written with the
// task id it would file under but no task, and the firing waits. Take,
// reached by every redelivery and by the sweep while SigNoz lists it,
// files it once no window holds; a resolve before that judges it the
// rollout's noise. An event (eventLabel) files at once: a Job that failed
// during a rollout still failed.
func (in *AlertIntake) fileUnlessHeld(ctx context.Context, a SigNozAlert, p Project, ns, at, name string) error {
	if a.Labels[eventLabel] == "true" {
		return in.file(ctx, a, p, ns, at, false)
	}
	r, hold, err := in.rolloutAt(ctx, a.StartsAt, name, ns)
	if err != nil {
		return err
	}
	if !hold {
		return in.file(ctx, a, p, ns, at, false)
	}
	row := alertRow(a, p, ns, at, false)
	row.Detail["roll_window"], row.Detail["applied"] = r.object(), stamp(r.Applied)
	if !r.Settled.IsZero() {
		row.Detail["settled"] = stamp(r.Settled)
	}
	if _, err := in.Store.InsertAlert(ctx, row); err != nil {
		return err
	}
	log.Printf("signoz webhook: alert %s in %s fired in the window of %s: held", strconv.Quote(name), ns, r.object()) // #nosec G706 -- quoted, ns is a checked DNS label
	return nil
}

// rolloutAt is a rollout whose window holds at for the alert name in
// namespace ns; false when none does. A platform rollout Flux applies
// holds every alert, a project environment's the alerts of its namespace,
// and a roll of the factory's own Deployments only the restart-window
// rule in their namespace (ruling 237).
func (in *AlertIntake) rolloutAt(ctx context.Context, at time.Time, name, ns string) (Rollout, bool, error) {
	var rollouts []Rollout
	if name == restartWindowAlert && ns == in.Namespace {
		checked := map[string]bool{}
		for _, target := range RollTargets {
			if checked[target.Deployment] {
				continue
			}
			checked[target.Deployment] = true
			d, err := in.Kube.Deployment(ctx, in.Namespace, target.Deployment)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return Rollout{}, false, err
			}
			applied, _ := d.applied()
			live, _ := d.live()
			rollouts = append(rollouts, Rollout{Kind: "Deployment", Namespace: in.Namespace, Name: target.Deployment, Scope: in.Namespace,
				Applied: applied, Settled: live})
		}
	}
	if in.Flux != nil {
		flux, err := in.Flux.FluxRollouts(ctx, in.Namespace)
		if err != nil {
			return Rollout{}, false, fmt.Errorf("flux rollouts: %w", err)
		}
		rollouts = append(rollouts, flux...)
	}
	for _, r := range rollouts {
		if r.holds(at, ns) {
			return r, true, nil
		}
	}
	return Rollout{}, false, nil
}

// file writes the alert's row, the dedupe key, with the id of the task it
// files, then files that task; refiled when the same firing outlived the
// task of the last one. A delivery that fails after the row is written is
// accepted (ServeHTTP) and finished by the sweep, or a redelivery, under
// the same task id (take).
func (in *AlertIntake) file(ctx context.Context, a SigNozAlert, p Project, ns, at string, refiled bool) error {
	row := alertRow(a, p, ns, at, refiled)
	var err error
	if row.ID, err = in.Store.InsertAlert(ctx, row); err != nil {
		return err
	}
	return in.fileTask(ctx, a, p, ns, row)
}

// alertRow is the alerts row of a on project p, with the id of the task
// it files.
func alertRow(a SigNozAlert, p Project, ns, at string, refiled bool) Alert {
	link := ""
	if safeLink(a.GeneratorURL) {
		link = a.GeneratorURL
	}
	return Alert{Kind: AlertSigNoz, Source: oneLine(a.Labels["alertname"]), TaskID: NewTaskID(), Fingerprint: a.Fingerprint,
		Detail: map[string]any{"project": p.Name, "namespace": ns, "repository": p.Repo().String(),
			"starts_at": at, "link": link, "labels": a.Labels, "refiled": refiled, "event": a.Labels[eventLabel] == "true",
			"summary": oneLine(cmp.Or(a.Annotations["summary"], a.Annotations["description"], a.Annotations["value"]))}}
}

// fileTask files the issue and task of the alert a on project p whose row,
// written first, names the task's id: a delivery that fails here leaves the
// row, so the sweep or a redelivery files the same one task (take).
func (in *AlertIntake) fileTask(ctx context.Context, a SigNozAlert, p Project, ns string, row Alert) error {
	repo := p.Repo()
	sha, err := in.Forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, "")
	if err != nil {
		return fmt.Errorf("base sha: %w", err)
	}
	base, err := in.reproBase(ctx, a, ns, sha)
	if err != nil {
		return err
	}
	title, body := alertIssue(a, p.Name, ns, in.hostsFor(p))
	body += in.healthSection(ctx, a, p, ns)
	if row.Detail["refiled"] == true {
		body = "factoryd: the alert still fires from the same start " + refileAfter.String() + " after the fix of its last issue merged.\n\n" + body
	}
	if w, ok := row.Detail["roll_window"].(string); ok {
		body = "factoryd: the alert fired while " + w + " rolled out and still fires " + rolloutSettle.String() + " after the rollouts settled.\n\n" + body
	}
	t := Task{ID: row.TaskID, RiskClass: "standard", BudgetEUR: in.Budget().ClampBudget(0),
		Objective: title + "\n\n" + body, SHA: sha, Source: SourceAlert,
		// the reproduction runs against the tree that was live when the
		// alert fired, even though main has moved on (roadmap 194)
		ReproBase: base,
		// the card's pre-check finds its alert by fingerprint before its
		// first model call (roadmap 181, Stage 18 item 3.12)
		AlertFingerprint: a.Fingerprint,
		// factoryd files as the bot; the brief marks the text as telemetry
		Author: in.Forgejo.BotUser}
	// A capacity alert goes to a human card even on a project with agents:
	// a coding agent can only raise a limit, the decision needs evidence.
	if p.Agents == policy.AgentsNone || a.Labels["capacity"] != "" {
		why := "the project runs no agents"
		if p.Agents != policy.AgentsNone {
			why = "a capacity alert needs a person's evidence: a coding agent can only raise a limit"
		}
		q := alertQuestion(a, p.Name, ns, why, body)
		q.Ask = sanitize.Outbound(q.Ask, in.hostsFor(p))
		t, err = EscalateToHuman(ctx, in.Store, in.Forgejo, in.Timeline, repo, q, "factoryd", t, "bug")
	} else {
		t, _, err = FileAgentTask(ctx, in.Store, in.Forgejo, in.Workflows, in.Timeline, p, title, body, []string{"bug"}, t)
	}
	if err != nil {
		return err
	}
	if err := in.Store.MergeAlertDetail(ctx, row.ID, map[string]any{"issue": t.IssueNumber, "repro_base": t.ReproBase}); err != nil {
		log.Printf("signoz webhook: alert row %d: record issue #%d: %v", row.ID, t.IssueNumber, err)
	}
	log.Printf("signoz webhook: alert %s in %s: issue #%d on %s, task %s", strconv.Quote(a.Labels["alertname"]), ns, t.IssueNumber, repo, t.ID) // #nosec G706 -- quoted, ns is a checked DNS label
	return nil
}

// reproBase is the reproduction base a signal card pins at its filing
// (roadmap 194): the commit that was live when the alert fired — the
// deployed build's, when the intake can read the roll of the Deployment
// the alert names in the factory's own namespace — else main's head at
// the filing. GateReproduction runs the base against this tree even
// though main has moved on; SetTaskSHA's session-start stamping of the
// branch base never touches it.
func (in *AlertIntake) reproBase(ctx context.Context, a SigNozAlert, ns, mainHead string) (string, error) {
	if in.Rolls == nil || ns != in.Namespace {
		return mainHead, nil
	}
	deployment := alertService(a.Labels)
	if deployment == "" {
		return mainHead, nil
	}
	sha, ok, err := in.Rolls.DeployedCommit(ctx, deployment)
	if err != nil {
		return "", fmt.Errorf("deployed commit of %s: %w", deployment, err)
	}
	if !ok {
		return mainHead, nil
	}
	return sha, nil
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

// healthSection is the Deployment of the alert's service as factoryd reads
// it, for the filed card: "" without a reader, a service or a namespace of
// the project.
func (in *AlertIntake) healthSection(ctx context.Context, a SigNozAlert, p Project, ns string) string {
	dep := alertService(a.Labels)
	if in.Health == nil || dep == "" || !slices.Contains(p.Namespaces, ns) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	text, err := DeploymentHealthText(ctx, in.Health, p, dep, ns, time.Now())
	if err != nil {
		text = fmt.Sprintf("factoryd could not read the Deployment `%s` in `%s`: %s", dep, ns, oneLine(err.Error()))
	} else {
		text = fmt.Sprintf("The Deployment `%s` in `%s` as factoryd read it when it filed this card:\n\n%s", dep, ns, fenced(text))
	}
	return "\n\n" + sanitize.Outbound(text, in.hostsFor(p))
}

// alertService is the alert's service label: the first of service,
// service.name, k8s.deployment.name and deployment, one line.
func alertService(labels map[string]string) string {
	return oneLine(cmp.Or(labels["service"], labels["service.name"], labels["k8s.deployment.name"], labels["deployment"]))
}

// alertParts is what alert a is called: its name, " on service <s>" (empty
// without a service) and its source, SigNoz or Flux.
func alertParts(a SigNozAlert) (name, on, source string) {
	name, source = oneLine(a.Labels["alertname"]), "SigNoz"
	if fromFlux(a.Fingerprint) {
		source = "Flux"
	}
	if s := alertService(a.Labels); s != "" {
		on = " on service " + s
	}
	return name, on, source
}

// alertIssue is the bug issue of alert a: title "alert: <name> on
// <service> in <ns>" (no "on" without a service) and a body with the
// firing time, the SigNoz link, the value and annotations and the labels,
// the values one line each inside fences. The text goes to Forgejo, so it
// is sanitised Outbound: hosts is the links left unmangled.
func alertIssue(a SigNozAlert, project, ns string, hosts []string) (title, body string) {
	name, on, source := alertParts(a)
	if s := alertService(a.Labels); s != "" {
		title = "alert: " + name + " on " + s + " in " + ns
	} else {
		title = "alert: " + name + " in " + ns
	}
	var b strings.Builder
	fmt.Fprintf(&b, "factoryd: %s fires the alert %s%s in namespace %s of project %s.\n\nFiring since %s.", source, name, on, ns, project, stamp(a.StartsAt))
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
	fmt.Fprintf(&b, "\n\nValue and annotations:\n\n%s\n\nLabels:\n\n%s\n\nfiled by factoryd from %s alert %s",
		fenced(strings.Join(notes, "\n")), fenced(strings.Join(labels, "\n")), source, oneLine(a.Fingerprint))
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
// unjudged on every other end, the unknown merge state included.
func alertVerdict(final, reason string) string {
	switch {
	case final == "done":
		return VerdictReal
	case strings.HasPrefix(reason, ReasonIssueClosed) && !strings.HasPrefix(reason, ReasonIssueClosed+ReasonMergeUnknown):
		return VerdictNoise
	}
	return VerdictUnjudged
}

// namespaceLabels are the labels an alert's namespace arrives under, the
// first set winning: the label the signoz-objects job sets on a project's
// rule, then the OTel resource attribute and its Prometheus form a rule's
// query groups by. No app forges these: SigNoz's collector drops what an
// app's own telemetry names and sets the sending pod's namespace (the
// infra repository's signoz HelmRelease).
var namespaceLabels = []string{"namespace", "k8s.namespace.name", "k8s_namespace_name"}

// namespaceOf is the namespace an alert's labels name (namespaceLabels).
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
