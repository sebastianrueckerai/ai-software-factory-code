package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// writeSeams are the stand-ins of the write tools' ports the tell-a-card
// rig leaves out: alerts, resources, the GitHub export, the scratchpad,
// the routines and the signals factoryd takes (a merge approval, a retry,
// a replan). Each call is recorded as one line, so a test asserts the side
// effect a tool had.
type writeSeams struct {
	mu        sync.Mutex
	calls     []string
	alerts    map[int64]factoryd.Alert
	resources map[int64]cloudres.Resource
	mirror    *factoryd.Mirror
	notes     factoryd.Notes
	routines  map[string]factoryd.RoutineView
	runs      []factoryd.RoutineRun
	deps      map[string][]string
	nextIssue int64
	imageCard factoryd.Task
	redChecks bool
}

func newWriteSeams() *writeSeams {
	return &writeSeams{alerts: map[int64]factoryd.Alert{}, resources: map[int64]cloudres.Resource{},
		routines: map[string]factoryd.RoutineView{}, deps: map[string][]string{}, nextIssue: 50}
}

func (w *writeSeams) log(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, fmt.Sprintf(format, args...))
}

// Calls answers every recorded port call, oldest first.
func (r *AcceptanceRig) Calls() []string {
	r.ws.mu.Lock()
	defer r.ws.mu.Unlock()
	return slices.Clone(r.ws.calls)
}

// wire seats the seams in cfg.
func (w *writeSeams) wire(cfg *console.Config) {
	cfg.Alerts, cfg.Resources, cfg.Mirrors, cfg.Notes, cfg.Routines = w, w, w, w, w
	cfg.ApproveMerge = func(_ context.Context, id string, pr int64, by string) error {
		w.log("approve %s pr %d by %s", id, pr, by)
		return nil
	}
	cfg.RerunChecks = func(_ context.Context, id string, pr int64, by string) error {
		w.log("rerun %s pr %d by %s", id, pr, by)
		return nil
	}
	cfg.CloseFactoryPR = func(_ context.Context, id string, pr int64, by, words string) error {
		w.log("close factory pr %s pr %d by %s: %s", id, pr, by, words)
		return nil
	}
	cfg.GrantCriteria = func(_ context.Context, id, by string, approval int64) (int, error) {
		w.log("grant %s approval %d by %s", id, approval, by)
		return 2, nil
	}
	cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		w.log("criterion %s %s %s by %s: %s", id, cid, verdict, by, reason)
		return nil
	}
	cfg.CloseHeldPlan = func(_ context.Context, id, by, note string, _ bool) (int, string, error) {
		w.log("close plan %s by %s: %s", id, by, note)
		return 0, factoryd.PlanCloseWords(by, note), nil
	}
	cfg.ConfirmDeliveredCriteria = func(_ context.Context, id, by, line string) (int, error) {
		w.log("confirm %s by %s: %s", id, by, line)
		return 1, nil
	}
	cfg.RetryCard = func(_ context.Context, id, by string) (int64, error) {
		w.log("retry %s by %s", id, by)
		return 61, nil
	}
	cfg.Replan = func(_ context.Context, id, by, comment string, offMain bool) (string, error) {
		w.log("replan %s by %s off_main %t", id, by, offMain)
		return "Sent " + id + " back to the planner.", nil
	}
	cfg.CloseCard = func(_ context.Context, id, by string) error {
		w.log("close %s by %s", id, by)
		return nil
	}
	cfg.RequestImage = func(_ context.Context, repository, tool, need, image, by string, _ func(string) error) (factoryd.ImageAsk, error) {
		w.log("image %s %s for %s by %s", repository, tool, need, by)
		w.mu.Lock()
		defer w.mu.Unlock()
		return factoryd.ImageAsk{Card: w.imageCard, Target: "default", Filed: true}, nil
	}
	cfg.Escalate = func(_ context.Context, repository string, q factoryd.HumanQuestion, by string) (int64, error) {
		w.log("escalate %s %q by %s", repository, q.Ask, by)
		return 70, nil
	}
	cfg.GitHubSync = func(_ context.Context, project, by string) (string, error) {
		w.log("sync %s by %s", project, by)
		return "synced", nil
	}
	cfg.AddCollaborator = func(_ context.Context, repo factoryd.Repo, user, permission string) error {
		w.log("collaborator %s %s %s", repo, user, permission)
		return nil
	}
	cfg.SetIssueDependency = func(_ context.Context, repo factoryd.Repo, number, dependency int64, remove bool) error {
		w.log("issue dependency %s#%d on #%d remove %t", repo, number, dependency, remove)
		return nil
	}
}

// Alerts.

func (w *writeSeams) Alerts(context.Context, int) ([]factoryd.Alert, error) { return nil, nil }

func (w *writeSeams) SetAlertVerdict(_ context.Context, id int64, verdict, by string, _ map[string]any) error {
	w.mu.Lock()
	a, ok := w.alerts[id]
	if ok && a.Verdict == "" {
		a.Verdict, a.ResolvedBy = verdict, by
		w.alerts[id] = a
	} else {
		ok = false
	}
	w.mu.Unlock()
	if !ok {
		return factoryd.ErrNotFound
	}
	w.log("alert %d %s by %s", id, verdict, by)
	return nil
}

func (w *writeSeams) AlertByID(_ context.Context, id int64) (factoryd.Alert, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.alerts[id]
	return a, ok, nil
}

// Resources.

func (w *writeSeams) Commissions(context.Context, time.Time) ([]cloudres.Resource, error) {
	return nil, nil
}
func (w *writeSeams) Use(context.Context, string) (cloudres.EnvelopeUse, error) {
	return cloudres.EnvelopeUse{}, nil
}
func (w *writeSeams) Standing(context.Context, string) (cloudres.StandingUse, error) {
	return cloudres.StandingUse{}, nil
}

func (w *writeSeams) Promote(_ context.Context, id int64, by string, _ func(string) policy.Standing) error {
	w.log("promote %d by %s", id, by)
	return nil
}

func (w *writeSeams) EndLease(_ context.Context, id int64, by string) error {
	w.log("release %d by %s", id, by)
	return nil
}

func (w *writeSeams) Commission(_ context.Context, id int64) (cloudres.Resource, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	res, ok := w.resources[id]
	return res, ok, nil
}

// Mirrors.

func (w *writeSeams) Mirror(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mirror == nil || w.mirror.Direction != direction {
		return factoryd.Mirror{}, false, nil
	}
	m := *w.mirror
	m.Token = ""
	return m, true, nil
}

func (w *writeSeams) MirrorToken(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mirror == nil || w.mirror.Direction != direction {
		return factoryd.Mirror{}, false, nil
	}
	return *w.mirror, true, nil
}

func (w *writeSeams) PutMirror(_ context.Context, m factoryd.Mirror) error {
	w.mu.Lock()
	w.mirror = &m
	w.mu.Unlock()
	w.log("mirror %s %s %s mode %s", m.Project, m.Direction, m.Remote, m.Mode)
	return nil
}

func (w *writeSeams) SetImportPR(context.Context, string, int64) error     { return nil }
func (w *writeSeams) DeleteMirror(context.Context, string, string) error   { return nil }
func (w *writeSeams) ClearGitHubFailures(context.Context, string) error    { return nil }
func (w *writeSeams) SyncPushMirrors(context.Context, factoryd.Repo) error { return nil }
func (w *writeSeams) PushMirrors(context.Context, factoryd.Repo) ([]factoryd.PushMirror, error) {
	return nil, nil
}
func (w *writeSeams) DeletePushMirror(context.Context, factoryd.Repo, string) error { return nil }

func (w *writeSeams) AddPushMirror(_ context.Context, repo factoryd.Repo, address, username, _ string) error {
	w.log("push mirror %s to %s as %s", repo, address, username)
	return nil
}

// Notes.

func (w *writeSeams) Notes(context.Context, string) (factoryd.Notes, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.notes, nil
}

func (w *writeSeams) PutNotes(_ context.Context, _, login, content, base string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if base != w.notes.Hash() {
		return false, nil
	}
	w.notes = factoryd.Notes{Content: content, By: login, At: time.Now()}
	w.calls = append(w.calls, "notes "+content)
	return true, nil
}

// Routines.

func (w *writeSeams) Views(context.Context, string) ([]factoryd.RoutineView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []factoryd.RoutineView
	for _, v := range w.routines {
		out = append(out, v)
	}
	return out, nil
}

func (w *writeSeams) Runs(_ context.Context, _, routine string, limit int) ([]factoryd.RoutineRun, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []factoryd.RoutineRun
	for i := len(w.runs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, w.runs[i])
	}
	return out, nil
}

func (w *writeSeams) RunOfTask(context.Context, string) (factoryd.RoutineRun, bool, error) {
	return factoryd.RoutineRun{}, false, nil
}

func (w *writeSeams) Skip(_ context.Context, project, routine string, skip bool, by string) error {
	w.mu.Lock()
	_, ok := w.routines[routine]
	w.mu.Unlock()
	if !ok {
		return factoryd.ErrNotFound
	}
	w.log("skip %s %s %t by %s", project, routine, skip, by)
	return nil
}

func (w *writeSeams) NeedsMe(context.Context, string, string, *bool, string) error { return nil }

func (w *writeSeams) RunNow(_ context.Context, project, routine string) error {
	w.mu.Lock()
	_, ok := w.routines[routine]
	if ok {
		w.runs = append(w.runs, factoryd.RoutineRun{Run: len(w.runs) + 1, Routine: routine, Trigger: factoryd.TriggerRunNow,
			Outcome: "held", Note: "the last card is still open"})
	}
	w.mu.Unlock()
	if !ok {
		return factoryd.ErrNotFound
	}
	w.log("run now %s %s", project, routine)
	return nil
}

// Forgejo: the writes the issue tools make, recorded in fgCalls.

// serveWrites answers the write endpoints serveForgejo leaves out; it
// reports whether it took the request. The rig's lock is held.
func (a *AcceptanceRig) serveWrites(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	const issues = "/api/v1/repos/sf/code/issues"
	switch {
	case r.Method == http.MethodGet && path == "/api/v1/repos/sf/code/labels":
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`[{"id":1,"name":"agent"},{"id":2,"name":"plan"},{"id":3,"name":"human"}]`)) // #nosec G104 -- test harness stub response
		} else {
			_, _ = w.Write([]byte(`[]`)) // #nosec G104 -- test harness stub response
		}
	case r.Method == http.MethodPost && path == issues:
		a.ws.mu.Lock()
		a.ws.nextIssue++
		n := a.ws.nextIssue
		a.ws.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"number":%d}`, n) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/dependencies"):
		_, _ = w.Write([]byte(`[]`)) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodPatch && strings.HasPrefix(path, issues+"/"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body) // #nosec G104 -- test harness stub request
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"title": body["title"], "body": body["body"]}) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/repos/sf/code/pulls/"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`)) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodDelete && strings.HasPrefix(path, issues+"/comments/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		return false
	}
	return true
}

// objects is the code project's objects as the policy and the projects
// read name them: the seed's, or the factory's when a test seats a
// project the console may invite to and export. The rig's lock is held.
func (a *AcceptanceRig) objects() string {
	if a.factoryObjects {
		return policy.ObjectsFactory
	}
	return policy.ObjectsSeed
}

// SeatFactoryObjects makes the code project one whose objects the
// factory owns.
func (a *AcceptanceRig) SeatFactoryObjects() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.factoryObjects = true
}

// SeatDependencies gives the code project's process a dependencies block,
// so its cards may wait on each other.
func (a *AcceptanceRig) SeatDependencies() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dependencies = true
}

// SeatRoutine makes routine one the project's file names, listed on the
// board and run by the routines port.
func (a *AcceptanceRig) SeatRoutine(rt policy.Routine, nextWords string) {
	a.mu.Lock()
	a.routines = append(a.routines, rt)
	a.mu.Unlock()
	a.ws.mu.Lock()
	defer a.ws.mu.Unlock()
	a.ws.routines[rt.Name] = factoryd.RoutineView{Project: accProjectName, Routine: rt, NextWords: nextWords}
}

// AddCard puts card k on the code project's board.
func (a *AcceptanceRig) AddCard(k factoryd.Task) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k.Repository = "sf/code"
	a.tasks = append(a.tasks, factoryd.BoardTask{Task: k})
}

// PendRequest opens pending approval request id of kind for taskID with
// spec as its JSON.
func (a *AcceptanceRig) PendRequest(id int64, kind, taskID string, pr int64, spec string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvals = append(a.approvals, factoryd.ApprovalRequest{ID: id, Kind: kind, TaskID: taskID, Project: "sf/code",
		PRNumber: pr, Status: "pending", Spec: []byte(spec)})
}

// SeatAsk stores the escalation row card taskID's ask is, for the
// analysis tool to enrich.
func (a *AcceptanceRig) SeatAsk(taskID, question string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgs = append(a.msgs, accMessage{ChatMessage: factoryd.ChatMessage{ID: a.nextID(), Role: "assistant", Kind: factoryd.MessageEscalation,
		TaskID: taskID, Content: question, Conversation: 1, At: time.Now()}, Repository: "sf/code", Login: login})
}

// Writes answers every Forgejo call that changed something, "METHOD path"
// in order.
func (r *AcceptanceRig) Writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.fgCalls {
		if f := strings.Fields(c); f[0] != http.MethodGet {
			out = append(out, f[0]+" "+f[1])
		}
	}
	return out
}

func (w *writeSeams) seatAlert(a factoryd.Alert) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.alerts[a.ID] = a
}

func (w *writeSeams) seatResource(res cloudres.Resource) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resources[res.ID] = res
}

func (w *writeSeams) seatImageCard(k factoryd.Task) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.imageCard = k
}

func (w *writeSeams) seatMirror(m factoryd.Mirror) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mirror = &m
}
