package factoryd

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// RollTarget is one image reference the code repository pins under k8s/:
// the manifest that holds it, the registry repository main CI pushes it to
// (its last path element), and the Deployment a change of it restarts.
type RollTarget struct {
	Name       string
	File       string
	Image      string
	Deployment string
}

// RollTargets are the factory's own images (ruling 178): main CI's image
// jobs push factory-gateway (both control-plane binaries), opencode-worker
// and ci-runner; the worker and runner references are factoryd's env.
var RollTargets = []RollTarget{
	{Name: "factoryd", File: "k8s/factoryd.yaml", Image: "factory-gateway", Deployment: "factoryd"},
	{Name: "gateway", File: "k8s/deployment.yaml", Image: "factory-gateway", Deployment: "factory-gateway"},
	{Name: "worker", File: "k8s/factoryd.yaml", Image: "opencode-worker", Deployment: "factoryd"},
	{Name: "ci-runner", File: "k8s/factoryd.yaml", Image: "ci-runner", Deployment: "factoryd"},
}

// pinnedRef finds the one digest-pinned reference to image in a manifest:
// host/path/image@sha256:<64 hex>.
func pinnedRef(content []byte, image string) (ref, digest string, err error) {
	re := regexp.MustCompile(`[A-Za-z0-9.:-]+(?:/[a-z0-9._-]+)*/` + regexp.QuoteMeta(image) + `@(sha256:[0-9a-f]{64})`)
	found := re.FindAllSubmatch(content, -1)
	if len(found) != 1 {
		return "", "", fmt.Errorf("%d digest-pinned references to %s, want exactly 1", len(found), image)
	}
	return string(found[0][0]), string(found[0][1]), nil
}

// Roll is one image's roll: the push of the commit that changed it, Flux
// applying it, and its Deployment available on it.
type Roll struct {
	SHA        string
	Image      string // the roll target's name
	Deployment string
	PushedAt   time.Time
	AppliedAt  time.Time
	LiveAt     time.Time
}

// Deployment is the slice of apps/v1 Deployment the roll record reads.
type Deployment struct {
	Metadata struct {
		ManagedFields []struct {
			Manager string    `json:"manager"`
			Time    time.Time `json:"time"`
		} `json:"managedFields"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
					Env   []struct {
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type           string    `json:"type"`
			Status         string    `json:"status"`
			Reason         string    `json:"reason"`
			LastUpdateTime time.Time `json:"lastUpdateTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// carries reports whether the pod template holds ref as an image or an
// env value.
func (d Deployment) carries(ref string) bool {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Image == ref {
			return true
		}
		for _, e := range c.Env {
			if e.Value == ref {
				return true
			}
		}
	}
	return false
}

// applied is when Flux last wrote the spec: its kustomize-controller
// managedFields entry.
func (d Deployment) applied() (time.Time, bool) {
	var at time.Time
	for _, f := range d.Metadata.ManagedFields {
		if f.Manager == "kustomize-controller" && f.Time.After(at) {
			at = f.Time
		}
	}
	return at, !at.IsZero()
}

// live is when the Deployment's newest ReplicaSet became available.
func (d Deployment) live() (time.Time, bool) {
	for _, c := range d.Status.Conditions {
		if c.Type == "Progressing" && c.Status == "True" && c.Reason == "NewReplicaSetAvailable" {
			return c.LastUpdateTime, true
		}
	}
	return time.Time{}, false
}

type DeploymentReader interface {
	Deployment(ctx context.Context, namespace, name string) (Deployment, error)
}

type RollStore interface {
	// RollTaskPRs are the pull requests of the repository's roll tasks.
	RollTaskPRs(ctx context.Context, repository string) ([]int64, error)
	RollRecorded(ctx context.Context, sha, image string) (bool, error)
	// RecordRoll inserts r once; false when its sha and image are recorded.
	RecordRoll(ctx context.Context, r Roll) (bool, error)
	// MarkRollsFailed fails the opened roll requests of the roll task
	// whose pull request landed the watched roll (RollFailed).
	MarkRollsFailed(ctx context.Context, repository string, pr int64) error
}

// RollWatcher records the rolls of the code repository's main: the newest
// commit that touched k8s/ (the merge of a pull request that did), for
// every roll target whose reference it changed against its first parent, once the target's Deployment runs it and is
// available on it. The push is the created time of the commit's push run.
// It watches from boot on, so a roll that restarts only the gateway is
// measured too; factoryd's own roll is measured by the factoryd it starts.
type RollWatcher struct {
	Forgejo             *Forgejo
	Owner, Repo, Branch string
	Token               string // reads the repository's Actions
	Namespace           string
	Kube                DeploymentReader
	Store               RollStore
	Metrics             *telemetry.Metrics
	Poll                time.Duration
	// Timeout bounds the wait for one commit's Deployments.
	Timeout time.Duration

	path     string // the newest commit the k8s/ path query answered
	sha      string // the commit measured: path, or the merge that landed it
	pr       int64  // the pull request that landed sha; 0 when none did
	parent   string
	deadline time.Time
	done     bool
	pending  map[string][]rollChange // Deployment -> its changed targets
	pushed   time.Time
}

type rollChange struct {
	target RollTarget
	ref    string
}

// Run polls until ctx ends.
func (w *RollWatcher) Run(ctx context.Context) {
	var last string
	for {
		if err := w.step(ctx); err != nil && err.Error() != last {
			log.Printf("roll: %v", err)
			last = err.Error()
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.Poll):
		}
	}
}

func (w *RollWatcher) step(ctx context.Context) error {
	path, parent, err := w.Forgejo.LatestCommit(ctx, w.Owner, w.Repo, w.Branch, "k8s")
	if err != nil {
		return err
	}
	if path != w.path {
		sha, pr, err := w.landed(ctx, path, &parent)
		if err != nil {
			return err
		}
		w.path, w.sha, w.parent, w.pr = path, sha, parent, pr
		w.deadline, w.done, w.pending, w.pushed = time.Now().Add(w.Timeout), false, nil, time.Time{}
	}
	if w.done {
		return nil
	}
	if w.pending == nil {
		if err := w.judge(ctx, w.parent); err != nil {
			return err
		}
		if w.done {
			return nil
		}
	}
	for dep, changes := range w.pending {
		recorded, err := w.measure(ctx, dep, changes)
		if err != nil {
			return err
		}
		if recorded {
			delete(w.pending, dep)
		}
		if w.done {
			return nil
		}
	}
	if len(w.pending) == 0 {
		w.done = true
		w.verdict(ctx, "success", "measured live: the deployments took the digest")
		return nil
	}
	if time.Now().After(w.deadline) {
		log.Printf("roll %s: %s not live within %s; not recorded", w.sha, strings.Join(slices.Sorted(maps.Keys(w.pending)), ", "), w.Timeout)
		w.verdict(ctx, "failure", fmt.Sprintf("not live within %s; the deployments did not take the digest", w.Timeout))
		if w.pr > 0 {
			if err := w.Store.MarkRollsFailed(ctx, w.Owner+"/"+w.Repo, w.pr); err != nil {
				return err
			}
		}
		w.done = true
	}
	return nil
}

// verdict reports the watcher's measurement of the sha it watched as the
// factory/roll commit status CI's rollout job polls; the roller keeps the
// skips, the watcher owns the rolls it measured. A status the API refuses
// is logged, not fatal.
func (w *RollWatcher) verdict(ctx context.Context, state, description string) {
	if err := w.Forgejo.CreateCommitStatus(ctx, w.Owner, w.Repo, w.sha,
		CommitStatus{State: state, Context: rollStatusContext, Description: description}); err != nil {
		log.Printf("roll %s: report %s: %v", w.sha, state, err)
	}
}

// landed maps the path query's commit to the commit that landed it on
// main. History simplification answers a merged pull request's head (the
// roll's branch tip, on the fork) rather than its merge, which is what
// has the push run: the merge, compared with its first parent, is the
// roll, for every commit of the branch.
//
// The newest roll task's pull request is read by its number first; any
// other (a human's or an agent's that touched k8s/) is looked for among
// the closed pull requests updated last.
func (w *RollWatcher) landed(ctx context.Context, path string, parent *string) (sha string, pr int64, err error) {
	var merged []MergedPullRequest
	rolls, err := w.Store.RollTaskPRs(ctx, w.Owner+"/"+w.Repo)
	if err != nil {
		return "", 0, err
	}
	if len(rolls) > 0 {
		p, err := w.Forgejo.PullRequestState(ctx, w.Owner, w.Repo, slices.Max(rolls))
		if err != nil {
			return "", 0, err
		}
		if p.Merged {
			merged = append(merged, MergedPullRequest{Number: p.Number, MergeCommit: p.MergeCommit, HeadSHA: p.HeadSHA})
		}
	}
	if !slices.ContainsFunc(merged, func(p MergedPullRequest) bool { return p.HeadSHA == path }) {
		recent, err := w.Forgejo.RecentMergedPullRequests(ctx, w.Owner, w.Repo)
		if err != nil {
			return "", 0, err
		}
		merged = append(merged, recent...)
	}
	for _, p := range merged {
		if p.HeadSHA != path || p.MergeCommit == "" {
			continue
		}
		parents, err := w.Forgejo.CommitParents(ctx, w.Owner, w.Repo, p.MergeCommit)
		if err != nil {
			return "", 0, err
		}
		if len(parents) == 0 {
			return "", 0, fmt.Errorf("merge %s of pull request #%d has no parent", p.MergeCommit, p.Number)
		}
		*parent = parents[0]
		return p.MergeCommit, p.Number, nil
	}
	return path, 0, nil
}

// judge finds the targets the commit changed and not yet recorded, by
// Deployment.
func (w *RollWatcher) judge(ctx context.Context, parent string) error {
	pending := map[string][]rollChange{}
	changed := false
	for _, t := range RollTargets {
		now, err := w.refAt(ctx, w.sha, t)
		if err != nil {
			return err
		}
		if parent != "" {
			if before, err := w.refAt(ctx, parent, t); err != nil {
				return err
			} else if before == now {
				continue
			}
		}
		changed = true
		recorded, err := w.Store.RollRecorded(ctx, w.sha, t.Name)
		if err != nil {
			return err
		}
		if !recorded {
			pending[t.Deployment] = append(pending[t.Deployment], rollChange{t, now})
		}
	}
	if !changed {
		log.Printf("roll: newest k8s commit %s changed no image; no roll to record", w.sha)
	}
	w.pending, w.done = pending, len(pending) == 0
	return nil
}

func (w *RollWatcher) refAt(ctx context.Context, ref string, t RollTarget) (string, error) {
	raw, err := w.Forgejo.FileAt(ctx, w.Owner, w.Repo, ref, t.File)
	if err != nil {
		return "", err
	}
	pinned, _, err := pinnedRef(raw, t.Image)
	if err != nil {
		return "", fmt.Errorf("%s at %s: %w", t.File, ref, err)
	}
	return pinned, nil
}

// measure records dep's roll once it runs every changed reference and is
// available after Flux applied it.
func (w *RollWatcher) measure(ctx context.Context, dep string, changes []rollChange) (bool, error) {
	d, err := w.Kube.Deployment(ctx, w.Namespace, dep)
	if err != nil {
		return false, err
	}
	for _, c := range changes {
		if !d.carries(c.ref) {
			return false, nil
		}
	}
	applied, ok := d.applied()
	if !ok {
		return false, fmt.Errorf("deployment %s has no kustomize-controller entry", dep)
	}
	live, ok := d.live()
	if !ok || live.Before(applied) {
		return false, nil
	}
	if w.pushed.IsZero() {
		// a few: the commit's nightly run, filed under push too, drops out
		runs, err := w.Forgejo.ActionRuns(ctx, w.Token, w.Owner, w.Repo, url.Values{"event": {"push"}, "head_sha": {w.sha}, "limit": {"5"}})
		if err != nil {
			return false, err
		}
		if len(runs) == 0 {
			log.Printf("roll %s: no push run for the commit; not recorded", w.sha)
			w.done = true
			return false, nil
		}
		w.pushed = runs[0].Created
	}
	inserted := false
	var names []string
	for _, c := range changes {
		ok, err := w.Store.RecordRoll(ctx, Roll{SHA: w.sha, Image: c.target.Name, Deployment: dep, PushedAt: w.pushed, AppliedAt: applied, LiveAt: live})
		if err != nil {
			return false, err
		}
		inserted = inserted || ok
		names = append(names, c.target.Name)
	}
	if !inserted {
		return true, nil
	}
	apply, ready := applied.Sub(w.pushed).Seconds(), live.Sub(applied).Seconds()
	attrs := metric.WithAttributes(attribute.String("deployment", dep))
	w.Metrics.RollApply.Record(ctx, apply, attrs)
	w.Metrics.RollReady.Record(ctx, ready, attrs)
	w.Metrics.RollDuration.Record(ctx, live.Sub(w.pushed).Seconds(), attrs)
	log.Printf("roll %s %s live (%s): push to apply %.0f s, apply to ready %.0f s", w.sha, dep, strings.Join(names, ", "), apply, ready)
	return true, nil
}
