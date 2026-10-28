package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	healthEventWindow = time.Hour
	healthEventMax    = 40
	healthMessageMax  = 200
	healthTextMax     = 12 << 10
	healthEventPage   = 500
	healthEventPages  = 20
	healthNamesMax    = 20
)

// DeploymentHealth is what factoryd reads of one Deployment: its
// ReplicaSets, pods with their containers' restarts and last terminations,
// and the last hour's events of those objects.
type DeploymentHealth struct {
	Namespace, Name           string
	Desired                   int
	Ready, Updated, Available int
	ReplicaSets               []HealthReplicaSet
	Pods                      []HealthPod
	Events                    []HealthEvent
}

type HealthReplicaSet struct {
	Name, Revision  string
	Replicas, Ready int
}

type HealthPod struct {
	Name, ReplicaSet, Phase string
	Ready                   bool
	Started, Deleting       time.Time
	Containers              []HealthContainer
}

// HealthContainer is a container's state: State is running, waiting or
// terminated, Reason the waiting or terminated reason, At when it started
// running or finished. Last is the earlier termination, nil without one.
type HealthContainer struct {
	Name     string
	Ready    bool
	Restarts int
	State    string
	Reason   string
	ExitCode int
	At       time.Time
	Last     *HealthTermination
}

type HealthTermination struct {
	Reason   string
	ExitCode int
	At       time.Time
}

type HealthEvent struct {
	Time       time.Time
	Kind, Name string
	Reason     string
	Count      int
	Message    string
}

type kubeContainerState struct {
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Terminated *struct {
		Reason     string    `json:"reason"`
		ExitCode   int       `json:"exitCode"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated"`
}

type kubeOwner struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// DeploymentHealth reads Deployment name of namespace with its ReplicaSets,
// pods and the last hour's events before now. ErrNotFound: no such
// Deployment; ErrForbidden: factoryd may not read one of the kinds.
func (k *InClusterKube) DeploymentHealth(ctx context.Context, namespace, name string, now time.Time) (DeploymentHealth, error) {
	var d struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas     int `json:"readyReplicas"`
			UpdatedReplicas   int `json:"updatedReplicas"`
			AvailableReplicas int `json:"availableReplicas"`
		} `json:"status"`
	}
	if err := k.getJSON(ctx, "/apis/apps/v1/namespaces/"+namespace+"/deployments/"+name, &d); err != nil {
		return DeploymentHealth{}, err
	}
	h := DeploymentHealth{Namespace: namespace, Name: name, Desired: 1,
		Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas, Available: d.Status.AvailableReplicas}
	if d.Spec.Replicas != nil {
		h.Desired = *d.Spec.Replicas
	}
	// the labels narrow the lists; the owners decide, so a Deployment that
	// selects by expressions reads too
	var sel []string
	for k, v := range d.Spec.Selector.MatchLabels {
		sel = append(sel, k+"="+v)
	}
	slices.Sort(sel)
	query := ""
	if len(sel) > 0 {
		query = "?" + url.Values{"labelSelector": {strings.Join(sel, ",")}}.Encode()
	}
	base := "/apis/apps/v1/namespaces/" + namespace

	var rss struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				Owners      []struct {
					Kind string `json:"kind"`
					UID  string `json:"uid"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, base+"/replicasets"+query, &rss); err != nil {
		return h, err
	}
	owned := map[string]bool{}
	for _, rs := range rss.Items {
		if !slices.ContainsFunc(rs.Metadata.Owners, func(o struct {
			Kind string `json:"kind"`
			UID  string `json:"uid"`
		}) bool {
			return o.Kind == "Deployment" && o.UID == d.Metadata.UID
		}) {
			continue
		}
		r := HealthReplicaSet{Name: rs.Metadata.Name, Revision: rs.Metadata.Annotations["deployment.kubernetes.io/revision"], Replicas: 1, Ready: rs.Status.ReadyReplicas}
		if rs.Spec.Replicas != nil {
			r.Replicas = *rs.Spec.Replicas
		}
		owned[r.Name] = true
		h.ReplicaSets = append(h.ReplicaSets, r)
	}
	sort.Slice(h.ReplicaSets, func(i, j int) bool {
		a, _ := strconv.Atoi(h.ReplicaSets[i].Revision)
		b, _ := strconv.Atoi(h.ReplicaSets[j].Revision)
		if a != b {
			return a > b
		}
		return h.ReplicaSets[i].Name < h.ReplicaSets[j].Name
	})

	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string      `json:"name"`
				DeletionTimestamp time.Time   `json:"deletionTimestamp"`
				Owners            []kubeOwner `json:"ownerReferences"`
			} `json:"metadata"`
			Status struct {
				Phase      string    `json:"phase"`
				StartTime  time.Time `json:"startTime"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					Name         string             `json:"name"`
					Ready        bool               `json:"ready"`
					RestartCount int                `json:"restartCount"`
					State        kubeContainerState `json:"state"`
					LastState    kubeContainerState `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/pods"+query, &pods); err != nil {
		return h, err
	}
	for _, p := range pods.Items {
		i := slices.IndexFunc(p.Metadata.Owners, func(o kubeOwner) bool { return o.Kind == "ReplicaSet" && owned[o.Name] })
		if i < 0 {
			continue
		}
		hp := HealthPod{Name: p.Metadata.Name, ReplicaSet: p.Metadata.Owners[i].Name, Phase: p.Status.Phase,
			Started: p.Status.StartTime, Deleting: p.Metadata.DeletionTimestamp}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" {
				hp.Ready = c.Status == "True"
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			c := HealthContainer{Name: cs.Name, Ready: cs.Ready, Restarts: cs.RestartCount}
			switch s := cs.State; {
			case s.Running != nil:
				c.State, c.At = "running", s.Running.StartedAt
			case s.Waiting != nil:
				c.State, c.Reason = "waiting", s.Waiting.Reason
			case s.Terminated != nil:
				c.State, c.Reason, c.ExitCode, c.At = "terminated", s.Terminated.Reason, s.Terminated.ExitCode, s.Terminated.FinishedAt
			}
			if t := cs.LastState.Terminated; t != nil {
				c.Last = &HealthTermination{Reason: t.Reason, ExitCode: t.ExitCode, At: t.FinishedAt}
			}
			hp.Containers = append(hp.Containers, c)
		}
		h.Pods = append(h.Pods, hp)
	}
	sort.Slice(h.Pods, func(i, j int) bool { return h.Pods[i].Name < h.Pods[j].Name })

	type event struct {
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
		Reason         string    `json:"reason"`
		Message        string    `json:"message"`
		Count          int       `json:"count"`
		LastTimestamp  time.Time `json:"lastTimestamp"`
		EventTime      time.Time `json:"eventTime"`
		FirstTimestamp time.Time `json:"firstTimestamp"`
		Series         *struct {
			Count            int       `json:"count"`
			LastObservedTime time.Time `json:"lastObservedTime"`
		} `json:"series"`
	}
	// a busy namespace's events come in pages, each well under call's cap
	var events []event
	for page, next := 0, ""; page < healthEventPages; page++ {
		q := url.Values{"limit": {strconv.Itoa(healthEventPage)}}
		if next != "" {
			q.Set("continue", next)
		}
		var evs struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []event `json:"items"`
		}
		if err := k.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/events?"+q.Encode(), &evs); err != nil {
			return h, err
		}
		events = append(events, evs.Items...)
		if next = evs.Metadata.Continue; next == "" {
			break
		}
	}
	// a pod's name is its ReplicaSet's and five random characters, and a
	// Deployment keeps its old ReplicaSets: the events of pods already gone
	// stay the Deployment's, another workload's never are
	ownedPod := func(pod string) bool {
		i := strings.LastIndexByte(pod, '-')
		return i > 0 && len(pod)-i-1 == 5 && owned[pod[:i]]
	}
	for _, e := range events {
		o := e.InvolvedObject
		switch {
		case o.Kind == "Deployment" && o.Name == name:
		case o.Kind == "ReplicaSet" && owned[o.Name]:
		case o.Kind == "Pod" && ownedPod(o.Name):
		default:
			continue
		}
		at, count := e.LastTimestamp, e.Count
		if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
			at, count = e.Series.LastObservedTime, max(count, e.Series.Count)
		}
		if at.IsZero() {
			at = e.EventTime
		}
		if at.IsZero() {
			at = e.FirstTimestamp
		}
		if at.IsZero() || at.Before(now.Add(-healthEventWindow)) {
			continue
		}
		h.Events = append(h.Events, HealthEvent{Time: at, Kind: o.Kind, Name: o.Name, Reason: e.Reason, Count: max(count, 1), Message: e.Message})
	}
	sort.SliceStable(h.Events, func(i, j int) bool { return h.Events[i].Time.Before(h.Events[j].Time) })
	if len(h.Events) > healthEventMax {
		h.Events = h.Events[len(h.Events)-healthEventMax:]
	}
	return h, nil
}

func healthStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

// Text is the health as plain lines, cut on a line boundary at about 12 KiB.
func (h DeploymentHealth) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Deployment %s in namespace %s: %d of %d ready (%d updated, %d available)\n", h.Name, h.Namespace, h.Ready, h.Desired, h.Updated, h.Available)
	// the ReplicaSets that hold pods by name, the old ones a Deployment keeps
	// at zero by their number
	var rs []string
	older := 0
	for _, r := range h.ReplicaSets {
		if r.Replicas == 0 && r.Ready == 0 {
			older++
			continue
		}
		rev := "no revision"
		if r.Revision != "" {
			rev = "revision " + r.Revision
		}
		rs = append(rs, fmt.Sprintf("%s (%s, %d of %d ready)", r.Name, rev, r.Ready, r.Replicas))
	}
	if older > 0 {
		rs = append(rs, fmt.Sprintf("%d older at 0 replicas", older))
	}
	b.WriteString("ReplicaSets: " + cmp.Or(strings.Join(rs, ", "), "none") + "\n")
	if len(h.Pods) == 0 {
		b.WriteString("Pods: none\n")
	} else {
		b.WriteString("Pods:\n")
	}
	for _, p := range h.Pods {
		ready := "not ready"
		if p.Ready {
			ready = "ready"
		}
		fmt.Fprintf(&b, "- %s (ReplicaSet %s): %s, %s", p.Name, p.ReplicaSet, p.Phase, ready)
		if !p.Started.IsZero() {
			b.WriteString(", started " + healthStamp(p.Started))
		}
		if !p.Deleting.IsZero() {
			b.WriteString(", deleting since " + healthStamp(p.Deleting))
		}
		b.WriteString("\n")
		for _, c := range p.Containers {
			fmt.Fprintf(&b, "  container %s: %s", c.Name, plural(c.Restarts, "restart"))
			switch c.State {
			case "running":
				b.WriteString(", running since " + healthStamp(c.At))
			case "waiting":
				b.WriteString(", waiting: " + cmp.Or(c.Reason, "no reason given"))
			case "terminated":
				fmt.Fprintf(&b, ", terminated %s: %s, exit code %d", healthStamp(c.At), cmp.Or(c.Reason, "no reason given"), c.ExitCode)
			}
			if c.Last == nil {
				b.WriteString(", no earlier termination")
			} else {
				fmt.Fprintf(&b, ", last terminated %s: %s, exit code %d", healthStamp(c.Last.At), cmp.Or(c.Last.Reason, "no reason given"), c.Last.ExitCode)
			}
			b.WriteString("\n")
		}
	}
	if len(h.Events) == 0 {
		b.WriteString("Events of the last hour: none\n")
	} else {
		b.WriteString("Events of the last hour, oldest first (Kubernetes' own counts):\n")
	}
	for _, e := range h.Events {
		fmt.Fprintf(&b, "- %s %s %s %s x%d: %s\n", healthStamp(e.Time), e.Kind, e.Name, e.Reason, e.Count, capRunes(oneLine(e.Message), healthMessageMax))
	}
	out := strings.TrimRight(b.String(), "\n")
	if len(out) <= healthTextMax {
		return out
	}
	cut := strings.LastIndex(out[:healthTextMax], "\n")
	return fmt.Sprintf("%s\n[%d bytes cut]", out[:cut], len(out)-cut)
}

// HealthRefusal is a read's refusal as one sentence naming the state and
// the fitting action, with the HTTP status a handler answers it with.
type HealthRefusal struct {
	Status int
	Msg    string
}

func (e *HealthRefusal) Error() string { return e.Msg }

// DeploymentHealthReader is the Kubernetes reads DeploymentHealthText needs.
type DeploymentHealthReader interface {
	DeploymentHealth(ctx context.Context, namespace, name string, now time.Time) (DeploymentHealth, error)
	Deployments(ctx context.Context, namespace string) ([]DeploymentState, error)
}

var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// DeploymentHealthText reads deployment of project p: in namespace, which
// must be one of the project's, or, with none given, in the first of the
// project's namespaces that holds it. A *HealthRefusal says what to change.
func DeploymentHealthText(ctx context.Context, r DeploymentHealthReader, p Project, deployment, namespace string, now time.Time) (string, error) {
	refuse := func(status int, f string, args ...any) error {
		return &HealthRefusal{Status: status, Msg: fmt.Sprintf(f, args...)}
	}
	if len(deployment) == 0 || len(deployment) > 253 || !dnsSubdomain.MatchString(deployment) {
		return "", refuse(http.StatusBadRequest, "deployment must be a Deployment's name (lower-case letters, digits, '-' and '.', at most 253 characters): give the name as the cluster lists it")
	}
	if namespace != "" && !dnsLabel.MatchString(namespace) {
		return "", refuse(http.StatusBadRequest, "namespace must be a namespace's name (lower-case letters, digits and '-', at most 63 characters): give one or leave it out")
	}
	if len(p.Namespaces) == 0 {
		return "", refuse(http.StatusForbidden, "project %s has no namespaces: add one to the project's file before reading a Deployment", p.Name)
	}
	search := p.Namespaces
	if namespace != "" {
		if !slices.Contains(p.Namespaces, namespace) {
			return "", refuse(http.StatusForbidden, "namespace %s is not one of project %s's namespaces (%s): name one of them or leave namespace out", namespace, p.Name, strings.Join(p.Namespaces, ", "))
		}
		search = []string{namespace}
	}
	// a namespace that refuses or fails does not stop the search: it is the
	// answer only when no other holds the Deployment
	var failed error
	for _, ns := range search {
		h, err := r.DeploymentHealth(ctx, ns, deployment, now)
		switch {
		case err == nil:
			return h.Text(), nil
		case errors.Is(err, ErrNotFound):
		case failed != nil:
		case errors.Is(err, ErrForbidden):
			failed = refuse(http.StatusForbidden, "factoryd may not read pods and events in namespace %s: bind ClusterRole factoryd-health to ServiceAccount factory/factoryd there", ns)
		default:
			log.Printf("deployment health %s/%s: %v", ns, deployment, err)
			failed = refuse(http.StatusBadGateway, "the cluster did not answer the read of Deployment %s in namespace %s: try again shortly", deployment, ns)
		}
	}
	if failed != nil {
		return "", failed
	}
	return "", refuse(http.StatusNotFound, "no Deployment %s in namespace %s of project %s; %s; factoryd reads only the project's namespaces",
		deployment, strings.Join(search, " or "), p.Name, deploymentNames(ctx, r, search))
}

// deploymentNames names the Deployments of namespaces for a refusal, sorted
// and at most healthNamesMax of them.
func deploymentNames(ctx context.Context, r DeploymentHealthReader, namespaces []string) string {
	var names []string
	for _, ns := range namespaces {
		ds, err := r.Deployments(ctx, ns)
		if err != nil {
			log.Printf("deployment health: list the Deployments of %s: %v", ns, err)
			return "check the name"
		}
		for _, d := range ds {
			names = append(names, d.Name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	switch {
	case len(names) == 0:
		return "it has no Deployment"
	case len(names) > healthNamesMax:
		return fmt.Sprintf("its Deployments: %s and %d more", strings.Join(names[:healthNamesMax], ", "), len(names)-healthNamesMax)
	}
	return "its Deployments: " + strings.Join(names, ", ")
}

// HealthStore is what the health hand-over reads: the task and its project.
type HealthStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
}

// DeploymentHealthHandler is POST /internal/deployment-health, the
// gateway's hand-over of an agent's read of a Deployment of its project
// (behind the gateway's bearer).
func DeploymentHealthHandler(store HealthStore, kube DeploymentHealthReader, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TaskID     string `json:"task_id"`
			Deployment string `json:"deployment"`
			Namespace  string `json:"namespace"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.TaskID == "" {
			http.Error(w, "task_id is required", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		t, ok, err := store.Task(ctx, in.TaskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no task "+in.TaskID, http.StatusNotFound)
			return
		}
		if Final(t.Status) {
			http.Error(w, "task "+in.TaskID+" is "+t.Status, http.StatusConflict)
			return
		}
		p, ok, err := store.ProjectByRepository(ctx, t.Repository)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no project owns the repository "+t.Repository+" of task "+in.TaskID, http.StatusConflict)
			return
		}
		if !p.Active() {
			http.Error(w, "project "+p.Name+" of task "+in.TaskID+" is not active: its Deployments are not read", http.StatusConflict)
			return
		}
		text, err := DeploymentHealthText(ctx, kube, p, in.Deployment, in.Namespace, now())
		var no *HealthRefusal
		switch {
		case errors.As(err, &no):
			http.Error(w, no.Msg, no.Status)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"text": text}) // #nosec G104 -- best-effort response write
		}
	}
}
