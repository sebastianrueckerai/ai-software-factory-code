package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// Roll is one roll of the code repository's main: the push of the commit
// that changed the Deployments' images, Flux applying it, and both
// Deployments available on it.
type Roll struct {
	SHA       string
	PushedAt  time.Time
	AppliedAt time.Time
	LiveAt    time.Time
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
			Spec podImages `json:"spec"`
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

type podImages struct {
	Containers []struct {
		Image string `json:"image" yaml:"image"`
	} `json:"containers" yaml:"containers"`
}

func (p podImages) images() []string {
	var out []string
	for _, c := range p.Containers {
		out = append(out, c.Image)
	}
	slices.Sort(out)
	return out
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
	RollRecorded(ctx context.Context, sha string) (bool, error)
	// RecordRoll inserts r once; false when its sha is already recorded.
	RecordRoll(ctx context.Context, r Roll) (bool, error)
}

// RollWatcher records factoryd's own roll once, at boot: the newest main
// commit that touched k8s/, if it changed a Deployment's images, once both
// Deployments run them and are available on them. The push is the created
// time of the commit's push run. A boot that finds the roll recorded
// (factoryd restarts during a roll) does nothing.
type RollWatcher struct {
	Forgejo             *Forgejo
	Owner, Repo, Branch string
	Namespace           string
	// Deployments maps each Deployment to its manifest in the repository.
	Deployments map[string]string
	Kube        DeploymentReader
	Store       RollStore
	Metrics     *telemetry.Metrics
	Poll        time.Duration
	Timeout     time.Duration

	sha, parent string
	images      map[string][]string
}

// Run polls until the roll is recorded, found recorded, or Timeout passes.
func (w *RollWatcher) Run(ctx context.Context) {
	deadline := time.Now().Add(w.Timeout)
	var last string
	for {
		done, err := w.step(ctx)
		if err != nil && err.Error() != last {
			log.Printf("roll: %v", err)
			last = err.Error()
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("roll %s: not live on both Deployments within %s; not recorded", w.sha, w.Timeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.Poll):
		}
	}
}

func (w *RollWatcher) step(ctx context.Context) (bool, error) {
	if w.sha == "" {
		sha, parent, err := w.Forgejo.LatestCommit(ctx, w.Owner, w.Repo, w.Branch, "k8s")
		if err != nil {
			return false, err
		}
		w.sha, w.parent = sha, parent
	}
	recorded, err := w.Store.RollRecorded(ctx, w.sha)
	if err != nil || recorded {
		return recorded, err
	}
	if w.images == nil {
		images, err := w.imagesAt(ctx, w.sha)
		if err != nil {
			return false, err
		}
		changed := w.parent == ""
		if !changed {
			before, err := w.imagesAt(ctx, w.parent)
			if err != nil {
				return false, err
			}
			for name := range images {
				changed = changed || !slices.Equal(images[name], before[name])
			}
		}
		if !changed {
			log.Printf("roll: newest k8s commit %s changed no Deployment image; no roll to record", w.sha)
			return true, nil
		}
		w.images = images
	}
	var applied, live time.Time
	for name, want := range w.images {
		d, err := w.Kube.Deployment(ctx, w.Namespace, name)
		if err != nil {
			return false, err
		}
		if !slices.Equal(d.Spec.Template.Spec.images(), want) {
			return false, nil
		}
		a, ok := d.applied()
		if !ok {
			return false, fmt.Errorf("deployment %s has no kustomize-controller entry", name)
		}
		l, ok := d.live()
		if !ok || l.Before(a) {
			return false, nil
		}
		applied, live = latest(applied, a), latest(live, l)
	}
	runs, err := w.Forgejo.ActionRuns(ctx, w.Owner, w.Repo, url.Values{"event": {"push"}, "head_sha": {w.sha}, "limit": {"1"}})
	if err != nil {
		return false, err
	}
	if len(runs) == 0 {
		log.Printf("roll %s: no push run for the commit; not recorded", w.sha)
		return true, nil
	}
	pushed := runs[0].Created
	inserted, err := w.Store.RecordRoll(ctx, Roll{SHA: w.sha, PushedAt: pushed, AppliedAt: applied, LiveAt: live})
	if err != nil || !inserted {
		return err == nil, err
	}
	apply, ready := applied.Sub(pushed).Seconds(), live.Sub(applied).Seconds()
	w.Metrics.RollApply.Record(ctx, apply)
	w.Metrics.RollReady.Record(ctx, ready)
	w.Metrics.RollDuration.Record(ctx, live.Sub(pushed).Seconds())
	log.Printf("roll %s live: push to apply %.0f s, apply to ready %.0f s", w.sha, apply, ready)
	return true, nil
}

// imagesAt reads each Deployment's container images from its manifest at ref.
func (w *RollWatcher) imagesAt(ctx context.Context, ref string) (map[string][]string, error) {
	images := map[string][]string{}
	for name, path := range w.Deployments {
		raw, err := w.Forgejo.FileAt(ctx, w.Owner, w.Repo, ref, path)
		if err != nil {
			return nil, err
		}
		img, err := manifestImages(raw, name)
		if err != nil {
			return nil, fmt.Errorf("%s at %s: %w", path, ref, err)
		}
		images[name] = img
	}
	return images, nil
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// manifestImages reads the container images of Deployment name from a
// multi-document manifest.
func manifestImages(raw []byte, name string) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec podImages `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no Deployment %s", name)
		}
		if err != nil {
			return nil, err
		}
		if doc.Kind == "Deployment" && doc.Metadata.Name == name {
			return doc.Spec.Template.Spec.images(), nil
		}
	}
}
