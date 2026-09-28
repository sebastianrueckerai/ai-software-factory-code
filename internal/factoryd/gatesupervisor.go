package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The gate supervisor's actions on a machine gate past its bound (ruling
// 141); escalate is the session supervisor's.
const (
	ActionWait   SupervisorAction = "wait"   // extend the run's bound once by its own length
	ActionRetry  SupervisorAction = "retry"  // stop the attempt; the retry policy runs it again
	ActionCancel SupervisorAction = "cancel" // fail the task with the diagnosis
)

// GateSupervision is one trip handed to SuperviseGate: the run's input and
// what it may still do (one wait and one retry per run).
type GateSupervision struct {
	Trip     GateTrip
	Input    []any
	CanWait  bool
	CanRetry bool
}

func (g GateSupervision) allowed() []SupervisorAction {
	var out []SupervisorAction
	if g.CanWait {
		out = append(out, ActionWait)
	}
	if g.CanRetry {
		out = append(out, ActionRetry)
	}
	return append(out, ActionCancel, ActionEscalate)
}

// GateDecision is the supervisor's answer; no action when the attempt had
// ended before it looked.
type GateDecision struct {
	Action         SupervisorAction `json:"action"`
	Diagnosis      string           `json:"diagnosis"`
	Recommendation string           `json:"recommendation,omitempty"`
}

// gateBundle is what the gate supervisor reads, assembled by factoryd with
// read-only calls; the model gets no tools.
type gateBundle struct {
	Activity  string             `json:"activity"`
	Attempt   int32              `json:"attempt"`
	Bound     string             `json:"bound"`
	Elapsed   string             `json:"elapsed"`
	Objective string             `json:"objective"`
	Input     string             `json:"input"`    // the activity's input as JSON, its head
	Timeline  string             `json:"timeline"` // the task's timeline so far, JSONL, its tail
	LastError string             `json:"last_error,omitempty"`
	State     map[string]any     `json:"state,omitempty"`
	Allowed   []SupervisorAction `json:"allowed_actions"`
}

const (
	bundleTimeline = 8 << 10  // bytes of the timeline's tail
	bundleInput    = 8 << 10  // bytes of the activity's input
	bundleLogLines = 100      // sandbox log lines
	bundleLogBytes = 12 << 10 // and at most this many bytes of them
	bundleReads    = time.Minute
)

const gateSupervisorPrompt = `You supervise the machine gates of an autonomous software factory. One activity of a task ran past its time bound: a check wait, an agent session, a reproduction run, a model review, a merge or another gate step. Read the diagnosis bundle: the task's objective and timeline so far, the activity's input, its bound and the time elapsed, its last error, and the state factoryd read for it (check runs and the CI runner queue, the sandbox pod with its log tail, the pull request's mergeability, the model review's last result). Then choose exactly one of allowed_actions:
- "wait": the activity makes progress and needs more time (checks queued or running, a pod pulling its image). Its bound is extended once by its own length.
- "retry": the attempt is stuck and a fresh attempt is likely to succeed (a hung poll, a lost connection). The attempt is stopped and runs again.
- "cancel": the activity cannot succeed and no human could change that. The task fails with your diagnosis as the reason.
- "escalate": a human must decide or act (access, infrastructure, a judgement). The task waits for a human with your diagnosis and recommendation.
Answer with one JSON object and nothing else: {"action": "wait|retry|cancel|escalate", "diagnosis": "...", "recommendation": "..."}. diagnosis (what is happening and why) is required, recommendation (what a human should do) for escalate; each under 1000 characters.`

// SuperviseGate asks the supervisor model what to do with a long gate's
// attempt past its bound, acts on that attempt when it still runs here
// (wait extends its bound, retry stops it for the retry policy), judges
// the alert with the action and records it on the timeline; the workflow
// acts on cancel and escalate. factoryd runs one replica, so the attempt
// that tripped runs in this process or has ended. The call is metered and
// booked to the task like every model call; a failed or refused call and
// a malformed answer escalate. The bundle is scrubbed whole.
func (r *Runner) SuperviseGate(ctx context.Context, taskID string, in GateSupervision) (GateDecision, error) {
	defer beatUntilDone(ctx, "gate supervisor")()
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return GateDecision{}, err
	}
	workflowID := activity.GetInfo(ctx).WorkflowExecution.ID
	var d GateDecision
	late := false
	if !r.cfg.Gates.Running(workflowID, in.Trip.ActivityID, in.Trip.Attempt) {
		d = GateDecision{Diagnosis: "the attempt ended before the supervisor looked"}
	} else {
		readCtx, cancel := context.WithTimeout(ctx, bundleReads)
		bundle, secrets := r.gateBundle(readCtx, t, workflowID, in)
		cancel()
		content, err := r.supervisor.Chat(ctx, t.ID, r.factoryModel(t, policy.PurposeGateSupervisor), policy.PurposeGateSupervisor, gateSupervisorPrompt, scrubbed(bundle, redact.New(secrets...)))
		if err != nil {
			d = GateDecision{Action: ActionEscalate, Diagnosis: "the gate supervisor could not be asked: " + err.Error(),
				Recommendation: fmt.Sprintf("look at %s of task %s yourself", in.Trip.Activity, t.ID)}
		} else {
			d = parseGateDecision(content, bundle.Allowed)
		}
		// the gate may have ended during the call (ruling 171): the answer
		// is recorded and acts on nothing
		switch {
		case !r.cfg.Gates.Running(workflowID, in.Trip.ActivityID, in.Trip.Attempt):
			late = true
		case d.Action == ActionWait:
			r.cfg.Gates.Wait(workflowID, in.Trip.ActivityID, in.Trip.Attempt)
		case d.Action == ActionRetry:
			r.cfg.Gates.Retry(workflowID, in.Trip.ActivityID, in.Trip.Attempt)
		}
	}
	verdict := tripVerdict(d.Action)
	if d.Action == "" {
		verdict = VerdictNoise
	}
	detail := map[string]any{"action": string(d.Action), "diagnosis": d.Diagnosis, "recommendation": d.Recommendation}
	if late {
		detail["note"] = "gate ended before the answer"
	}
	if err := r.store.ResolveAlert(ctx, in.Trip.AlertID, verdict, "supervisor", detail); err != nil {
		log.Printf("task %s: judge alert %d: %v", t.ID, in.Trip.AlertID, err)
	}
	detail["activity"], detail["attempt"], detail["alert"] = in.Trip.Activity, in.Trip.Attempt, in.Trip.AlertID
	r.record(ctx, t.ID, "gate_supervisor", detail)
	return d, nil
}

// gateBundle reads what the supervisor needs for the activity, and the
// task's git tokens the bundle is scrubbed of; a read that fails puts its
// error in the bundle instead.
func (r *Runner) gateBundle(ctx context.Context, t Task, workflowID string, in GateSupervision) (gateBundle, []string) {
	input, _ := json.Marshal(in.Input) // #nosec G104 -- the workflow's decoded arguments re-encode
	b := gateBundle{Activity: in.Trip.Activity, Attempt: in.Trip.Attempt, Bound: in.Trip.Bound.String(),
		Elapsed: time.Since(in.Trip.Started).Round(time.Second).String(), Objective: t.Objective,
		Input: textutil.Truncate(string(input), bundleInput), Allowed: in.allowed()}
	tl, err := r.objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
	if err != nil {
		b.Timeline = "unreadable: " + err.Error()
	} else {
		b.Timeline = textutil.Tail(string(tl), bundleTimeline)
	}
	if last, err := r.cfg.LastFailure(ctx, workflowID, in.Trip.ActivityID); err != nil {
		b.LastError = "unreadable: " + err.Error()
	} else {
		b.LastError = last
	}
	// the task's git tokens exist only while its sandbox runs: after the
	// session its namespace, and factoryd's binding in it, are gone
	var secrets []string
	if ns, _, ok := r.sandboxPod(t.ID); ok {
		tokens, err := r.kube.GetSecretData(ctx, ns, "task-git")
		if err != nil && !errors.Is(err, ErrNotFound) {
			b.State = map[string]any{"withheld": "the task's tokens are unreadable to scrub the bundle: " + err.Error()}
			b.Timeline, b.LastError, b.Input = "", "", ""
			return b, nil
		}
		secrets = slices.Collect(maps.Values(tokens))
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return b, secrets
	}
	switch in.Trip.Activity {
	case ActivityWaitChecks:
		sha := ""
		if len(in.Input) > 1 {
			sha, _ = in.Input[1].(string)
		}
		checks := map[string]any{"commit": sha, "runner_queue": r.cfg.CIQueue.Waiting()}
		if cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha); err != nil {
			checks["error"] = err.Error()
		} else {
			checks["state"], checks["statuses"] = cs.State, cs.Statuses
		}
		b.State = map[string]any{"checks": checks}
	case ActivityRunSession, ActivityGateReproduction:
		b.State = map[string]any{"pod": r.podState(ctx, t.ID)}
	case ActivityMerge:
		pr := map[string]any{"number": t.PRNumber}
		if st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber); err != nil {
			pr["error"] = err.Error()
		} else {
			pr["state"], pr["merged"], pr["mergeable"], pr["head"] = st.State, st.Merged, st.Mergeable, st.HeadSHA
		}
		b.State = map[string]any{"pull_request": pr}
	case ActivityModelReview:
		review := map[string]any{"model": r.factoryModel(t, policy.PurposeReview), "last_result": "none on this task's timeline"}
		if step := lastStep(tl, "model_review"); step != nil {
			review["last_result"] = step
		}
		b.State = map[string]any{"review": review}
	}
	return b, secrets
}

// lastStep is the timeline's last step named step, nil when none.
func lastStep(timeline []byte, step string) map[string]any {
	lines := bytes.Split(timeline, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal(lines[i], &m) == nil && m["step"] == step {
			return m
		}
	}
	return nil
}

// podState is the task's running sandbox pod: phase, conditions and the
// log's last lines.
func (r *Runner) podState(ctx context.Context, taskID string) map[string]any {
	ns, name, ok := r.sandboxPod(taskID)
	if !ok {
		return map[string]any{"name": "", "note": "no sandbox pod is running"}
	}
	out := map[string]any{"name": name}
	if d, err := r.kube.PodDiagnosis(ctx, ns, name); err != nil {
		out["error"] = err.Error()
	} else {
		out["phase"], out["conditions"], out["containers"] = d.Phase, d.Conditions, d.Containers
	}
	if tail, err := r.kube.PodLogTail(ctx, ns, name, bundleLogLines); err != nil {
		out["log"] = "unreadable: " + err.Error()
	} else {
		out["log"] = textutil.Tail(tail, bundleLogBytes)
	}
	return out
}

// scrubbed is the bundle as JSON values with every string scrubbed.
func scrubbed(b gateBundle, s *redact.Scrubber) any {
	raw, _ := json.Marshal(b) // #nosec G104 -- plain values
	var v any
	_ = json.Unmarshal(raw, &v)
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return s.String(x)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for k := range x {
				x[k] = walk(x[k])
			}
		}
		return v
	}
	return walk(v)
}

// parseGateDecision validates the answer like parseDecision: one JSON
// object, a known and allowed action, a diagnosis, a recommendation for
// escalate. Anything else escalates.
func parseGateDecision(content string, allowed []SupervisorAction) GateDecision {
	invalid := func(why string) GateDecision {
		return GateDecision{Action: ActionEscalate, Diagnosis: "supervisor answer invalid (" + why + "): " + textutil.Truncate(content, 300),
			Recommendation: "read the bundle on the alert's timeline and decide"}
	}
	var d GateDecision
	if err := decodeAnswer(content, &d); err != nil {
		return invalid(err.Error())
	}
	d.Diagnosis = textutil.Truncate(strings.TrimSpace(d.Diagnosis), maxDecisionText)
	d.Recommendation = textutil.Truncate(strings.TrimSpace(d.Recommendation), maxDecisionText)
	switch {
	case !slices.Contains([]SupervisorAction{ActionWait, ActionRetry, ActionCancel, ActionEscalate}, d.Action):
		return invalid(fmt.Sprintf("unknown action %q", d.Action))
	case d.Diagnosis == "":
		return invalid("no diagnosis")
	case d.Action == ActionEscalate && d.Recommendation == "":
		return invalid("escalate without a recommendation")
	case !slices.Contains(allowed, d.Action):
		return GateDecision{Action: ActionEscalate, Diagnosis: fmt.Sprintf("%q is not allowed on this run any more; the supervisor's diagnosis: %s", d.Action, d.Diagnosis),
			Recommendation: cmp.Or(d.Recommendation, "decide whether the activity may run on")}
	}
	return d
}
