package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// queueEntry is one row of fakeStore's merge queues.
type queueEntry struct {
	project, task string
	at            time.Time
}

func (s *fakeStore) EnterQueue(ctx context.Context, project, taskID string) (QueueSpot, error) {
	i := slices.IndexFunc(s.queue, func(e queueEntry) bool { return e.task == taskID })
	spot := QueueSpot{New: i < 0}
	if i < 0 {
		s.queue = append(s.queue, queueEntry{project, taskID, time.Now()})
		i = len(s.queue) - 1
	}
	e := s.queue[i]
	spot.EnteredAt = e.at
	for _, o := range s.queue[:i] {
		if o.project == e.project {
			spot.Ahead++
		}
	}
	return spot, nil
}

func (s *fakeStore) QueueHead(ctx context.Context, project string) (string, bool, error) {
	for _, e := range s.queue {
		if e.project == project {
			return e.task, true, nil
		}
	}
	return "", false, nil
}

func (s *fakeStore) LeaveQueue(ctx context.Context, taskID string) (string, bool, error) {
	i := slices.IndexFunc(s.queue, func(e queueEntry) bool { return e.task == taskID })
	if i < 0 {
		return "", false, nil
	}
	project := s.queue[i].project
	s.queue = slices.Delete(s.queue, i, i+1)
	return project, true, nil
}

func (s *fakeStore) WaitingQueueHeads(ctx context.Context) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, e := range s.queue {
		if !seen[e.project] && s.tasks[e.task].Gate == GateMergeQueue {
			out = append(out, e.task)
		}
		seen[e.project] = true
	}
	return out, nil
}

// queueHarness is a gate harness on a process with merge queue project:
// ahead answers EnterQueue by its call (from 1, on the workflow's clock),
// sync answers SyncWithMain; the calls are recorded.
type queueHarness struct {
	*gateHarness
	ahead func(call int) int
	sync  func(from string) SyncResult
	// headAnswers answers HeadChecks, the approval stage's read of the
	// staged head (nil: green)
	headAnswers func(sha string) ChecksResult
	enters      []time.Duration // when each EnterQueue ran, since the start
	syncs       []string        // SyncWithMain's from
	leaves      []string        // LeaveQueue's why
	checks      []string        // the heads WaitChecks ran on
	reruns      []string        // RerunChecks' heads
}

func newQueueHarness(t *testing.T, queue string, checks func(sha string) ChecksResult, setup ...func(*gateHarness)) *queueHarness {
	q := &queueHarness{ahead: func(int) int { return 0 }, sync: func(from string) SyncResult { return SyncResult{From: from, To: from} }}
	q.gateHarness = newGateHarnessWith(t, TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}, setup...)
	q.taskConfig = &TaskConfig{Process: "spec", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: queue}
	env := q.env
	env.OnActivity(ActivityEnterQueue, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (int, error) {
		q.enters = append(q.enters, env.Now().Sub(q.started))
		return q.ahead(len(q.enters)), nil
	})
	env.OnActivity(ActivitySyncWithMain, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
			q.syncs = append(q.syncs, from)
			q.steps = append(q.steps, "sync "+from)
			return q.sync(from), nil
		})
	env.OnActivity(ActivityLeaveQueue, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, why string) error {
		q.leaves = append(q.leaves, why)
		q.steps = append(q.steps, "leave")
		return nil
	})
	env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, nil)
	env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
		q.checks = append(q.checks, sha)
		return checks(sha), nil
	})
	env.OnActivity(ActivityHeadChecks, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
		q.headChecks = append(q.headChecks, sha)
		if q.headAnswers != nil {
			return q.headAnswers(sha), nil
		}
		return ChecksResult{State: ChecksSuccess}, nil
	})
	env.OnActivity(ActivityRerunChecks, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, sha string) (string, error) {
		q.reruns = append(q.reruns, sha)
		return sha + "-rerun", nil
	})
	env.OnActivity(ActivityWithdrawApproval, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) error {
		q.steps = append(q.steps, "withdraw")
		return nil
	})
	return q
}

func green(string) ChecksResult { return ChecksResult{State: ChecksSuccess} }

// Two tasks of one project: the second waits behind the first in gate
// merge-queue (the card reads "merge queue, 1 ahead, for n min") and
// starts its turn at the first's queue_left, which signals queue_turn; a
// lost signal costs at most the 5-minute re-check.
func TestQueueOneAtATime(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		ctx := context.Background()
		s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, approval_requests CASCADE`); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a1", "a2", "b1"} {
			repo := "linkshort/linkshort"
			if id == "b1" {
				repo = "sf/other"
			}
			if err := s.CreateTask(ctx, Task{ID: id, Status: "in-review", Repository: repo}); err != nil {
				t.Fatal(err)
			}
		}
		enter := func(project, id string, ahead int, fresh bool) {
			t.Helper()
			spot, err := s.EnterQueue(ctx, project, id)
			if err != nil || spot.Ahead != ahead || spot.New != fresh || spot.EnteredAt.IsZero() {
				t.Fatalf("EnterQueue(%s) = %+v, %v; want %d ahead, new %v", id, spot, err, ahead, fresh)
			}
		}
		enter("linkshort/linkshort", "a1", 0, true)
		enter("linkshort/linkshort", "a2", 1, true)
		enter("sf/other", "b1", 0, true)
		enter("linkshort/linkshort", "a1", 0, false)
		enter("linkshort/linkshort", "a2", 1, false)
		if err := s.SetGate(ctx, "a2", GateMergeQueue, time.Now()); err != nil {
			t.Fatal(err)
		}
		board, err := s.BoardTasks(ctx, time.Now().Add(-time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		ahead := map[string]int{}
		for _, b := range board {
			ahead[b.ID] = b.QueueAhead
		}
		if want := map[string]int{"a1": 0, "a2": 1, "b1": 0}; fmt.Sprint(ahead) != fmt.Sprint(want) {
			t.Fatalf("board's queue ahead = %v, want %v", ahead, want)
		}
		if head, ok, err := s.QueueHead(ctx, "linkshort/linkshort"); head != "a1" || !ok || err != nil {
			t.Fatalf("QueueHead = %q %v %v", head, ok, err)
		}
		if heads, err := s.WaitingQueueHeads(ctx); len(heads) != 0 || err != nil {
			t.Fatalf("waiting heads before a1 leaves = %v %v: a1 holds its turn", heads, err)
		}
		if project, left, err := s.LeaveQueue(ctx, "a1"); project != "linkshort/linkshort" || !left || err != nil {
			t.Fatalf("LeaveQueue(a1) = %q %v %v", project, left, err)
		}
		if _, left, err := s.LeaveQueue(ctx, "a1"); left || err != nil {
			t.Fatalf("second LeaveQueue(a1) left %v %v", left, err)
		}
		if head, _, _ := s.QueueHead(ctx, "linkshort/linkshort"); head != "a2" {
			t.Fatalf("head after a1 left = %q", head)
		}
		if heads, err := s.WaitingQueueHeads(ctx); !slices.Equal(heads, []string{"a2"}) || err != nil {
			t.Fatalf("waiting heads = %v %v, want [a2]", heads, err)
		}
		enter("linkshort/linkshort", "a2", 0, false)
	})

	t.Run("activities", func(t *testing.T) {
		r, _, store := newTestRunner(t, Task{ID: "a1", Status: "in-review", Repository: "linkshort/linkshort", PRNumber: 9}, newFakeKube(), &fakeObjectStore{})
		store.CreateTask(context.Background(), Task{ID: "a2", Status: "in-review", Repository: "linkshort/linkshort", PRNumber: 10})
		ctx := context.Background()
		if ahead, err := r.EnterQueue(ctx, "a1"); ahead != 0 || err != nil {
			t.Fatalf("a1 ahead %d %v", ahead, err)
		}
		if ahead, err := r.EnterQueue(ctx, "a2"); ahead != 1 || err != nil {
			t.Fatalf("a2 ahead %d %v", ahead, err)
		}
		if a2 := store.tasks["a2"]; a2.Gate != GateMergeQueue || !a2.GateSince.Equal(store.queue[1].at) {
			t.Fatalf("a2's gate = %q since %v, want merge-queue since it entered %v", a2.Gate, a2.GateSince, store.queue[1].at)
		}
		if a1 := store.tasks["a1"]; a1.Gate == GateMergeQueue {
			t.Fatal("the head is in gate merge-queue")
		}
		// a re-check keeps the gate's start
		if _, err := r.EnterQueue(ctx, "a2"); err != nil || !store.tasks["a2"].GateSince.Equal(store.queue[1].at) {
			t.Fatalf("re-check moved the gate's start: %v", err)
		}
		if err := r.LeaveQueue(ctx, "a1", "pull request merged by factory-merge"); err != nil {
			t.Fatal(err)
		}
		wf := r.cfg.Workflows.(*fakeWorkflows)
		if want := []string{"a2 queue_turn  #0: "}; !slices.Equal(wf.signals, want) {
			t.Fatalf("signals = %q, want %q", wf.signals, want)
		}
		if ahead, err := r.EnterQueue(ctx, "a2"); ahead != 0 || err != nil {
			t.Fatalf("a2 after a1 left: ahead %d %v", ahead, err)
		}
		if a2 := store.tasks["a2"]; a2.Gate != "" {
			t.Fatalf("a2 at its turn is in gate %q, want none", a2.Gate)
		}
		steps := func(id string) []string {
			var out []string
			for _, l := range timelineLines(t, r, id) {
				if strings.HasPrefix(l["step"].(string), "queue_") {
					out = append(out, l["step"].(string))
				}
			}
			return out
		}
		if got := steps("a1"); !slices.Equal(got, []string{"queue_entered", "queue_turn", "queue_left"}) {
			t.Fatalf("a1's queue steps = %v", got)
		}
		if got := steps("a2"); !slices.Equal(got, []string{"queue_entered", "queue_turn"}) {
			t.Fatalf("a2's queue steps = %v", got)
		}
		// the turn ends the gate merge-queue of a task that waited
		for id, want := range map[string]any{"a1": nil, "a2": rfc3339(store.queue[0].at)} {
			lines := timelineLines(t, r, id)
			i := slices.IndexFunc(lines, func(l map[string]any) bool { return l["step"] == "queue_turn" })
			if lines[i]["started"] != want {
				t.Fatalf("%s's queue_turn started %v, want %v", id, lines[i]["started"], want)
			}
		}
	})

	t.Run("paused", func(t *testing.T) {
		r, _, store := newTestRunner(t, Task{ID: "a1", Status: "in-review", Repository: "linkshort/linkshort", PRNumber: 9}, newFakeKube(), &fakeObjectStore{})
		store.CreateTask(context.Background(), Task{ID: "fix", Status: "in-review", Repository: "linkshort/linkshort", PRNumber: 10})
		store.pauses = map[string]QueuePause{"linkshort/linkshort": {Project: "linkshort/linkshort", RunID: 5, SHA: "red1", FixTask: "fix"}}
		ctx := context.Background()
		// the queue's head waits behind the pause; the fix card takes the turn
		if ahead, err := r.EnterQueue(ctx, "a1"); ahead != 1 || err != nil || store.tasks["a1"].Gate != GateMergeQueue {
			t.Fatalf("a1 ahead %d %v, gate %q; want held by the pause", ahead, err, store.tasks["a1"].Gate)
		}
		if ahead, err := r.EnterQueue(ctx, "fix"); ahead != 0 || err != nil {
			t.Fatalf("the fix card ahead %d %v, want its turn past the pause", ahead, err)
		}
		var paused []map[string]any
		for _, l := range timelineLines(t, r, "a1") {
			if l["step"] == "queue_paused" {
				paused = append(paused, l)
			}
		}
		if len(paused) != 1 || paused[0]["sha"] != "red1" || paused[0]["fix"] != "fix" {
			t.Fatalf("queue_paused steps = %v, want one naming the red commit and the fix card", paused)
		}
		// a re-check records no second pause; the pause's end frees the head
		if _, err := r.EnterQueue(ctx, "a1"); err != nil || len(timelineLines(t, r, "a1")) != 2 {
			t.Fatalf("re-check: %v, timeline %v", err, timelineLines(t, r, "a1"))
		}
		delete(store.pauses, "linkshort/linkshort")
		if ahead, err := r.EnterQueue(ctx, "a1"); ahead != 0 || err != nil {
			t.Fatalf("a1 after the pause: ahead %d %v", ahead, err)
		}
	})

	t.Run("workflow", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, green)
		turn := false
		q.ahead = func(int) int {
			if turn {
				return 0
			}
			return 1
		}
		q.onSessions(pushed("s1", "c1"))
		q.env.RegisterDelayedCallback(func() {
			turn = true
			q.env.SignalWorkflow(SignalQueueTurn, TaskSignal{})
		}, 3*time.Minute)
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if want := []time.Duration{0, 3 * time.Minute}; !slices.Equal(q.enters, want) {
			t.Fatalf("EnterQueue at %v, want %v: the second at the first's queue_left", q.enters, want)
		}
		if want := []string{"policy", "repro c1", "review c1", "sync c1", "approval c1", "merge", "leave"}; !slices.Equal(q.steps, want) {
			t.Fatalf("steps = %v, want %v", q.steps, want)
		}
		if q.requested[0].Sub(q.started) != 3*time.Minute {
			t.Fatalf("approval requested at %v, want at the turn", q.requested[0].Sub(q.started))
		}
	})

	t.Run("recheck", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, green)
		q.ahead = func(call int) int { return max(0, 3-call) }
		q.onSessions(pushed("s1", "c1"))
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if want := []time.Duration{0, 5 * time.Minute, 10 * time.Minute}; !slices.Equal(q.enters, want) {
			t.Fatalf("EnterQueue at %v, want %v", q.enters, want)
		}
	})
}

// timelineLines are the timeline records of task id.
func timelineLines(t *testing.T, r *Runner, id string) []map[string]any {
	t.Helper()
	b, err := r.timeline.Objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// At its turn main is merged into the head: the checks run again on the
// head the sync made, and the approval and the merge are pinned to it;
// policy, review and audit are not run again. Red checks there go back
// to the agent with the prefix, never re-run as a head the agent left,
// and the task leaves the queue; its next head queues again.
// The cheap turn (roadmap 222, item 4): a sync that moves the head
// without a file in common with main's new commits runs no second checks
// stage. The approval comes at once on the synced head, and the merge
// reads the checks' green on the head they ran on.
func TestSyncSkipsTheRecheckWithoutAFileInCommon(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.sync = func(from string) SyncResult { return SyncResult{From: from, To: "m1", Main: "main1", Base: "b0"} }
	q.onSessions(pushed("s1", "c1"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(q.checks, []string{"c1"}) {
		t.Fatalf("checks ran on %v, want c1 alone", q.checks)
	}
	if want := []string{"policy", "repro c1", "review c1", "sync c1", "approval m1", "merge", "leave"}; !slices.Equal(q.steps, want) {
		t.Fatalf("steps = %v, want %v", q.steps, want)
	}
	if !slices.Equal(q.merges, []string{"9 m1 sebastian"}) || !slices.Equal(q.checkedHeads, []string{"c1"}) || !q.approvals[0].Gates.ChecksPassed {
		t.Fatalf("merges = %v, checked heads %v, checks passed %v; want the synced head merged on c1's green", q.merges, q.checkedHeads, q.approvals[0].Gates.ChecksPassed)
	}
}

func TestSyncRerunsChecksOnNewHead(t *testing.T) {
	t.Run("green", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, green)
		q.sync = func(from string) SyncResult {
			return SyncResult{From: from, To: "m1", Main: "main1", Overlap: []string{"a.go"}}
		}
		q.onSessions(pushed("s1", "c1"))
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if !slices.Equal(q.checks, []string{"c1", "m1"}) {
			t.Fatalf("checks ran on %v, want [c1 m1]", q.checks)
		}
		if want := []string{"policy", "repro c1", "review c1", "sync c1", "approval m1", "merge", "leave"}; !slices.Equal(q.steps, want) {
			t.Fatalf("steps = %v, want %v", q.steps, want)
		}
		if !slices.Equal(q.merges, []string{"9 m1 sebastian"}) || !q.approvals[0].Gates.ChecksPassed {
			t.Fatalf("merges = %v, checks passed %v", q.merges, q.approvals[0].Gates.ChecksPassed)
		}
	})

	t.Run("red", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, func(sha string) ChecksResult {
			if sha == "m1" {
				return ChecksResult{State: ChecksFailure, Findings: []string{"TestX failed on m1"}}
			}
			return ChecksResult{State: ChecksSuccess}
		})
		q.sync = func(from string) SyncResult {
			if from == "c1" {
				return SyncResult{From: from, To: "m1", Main: "main1", Overlap: []string{"a.go"}}
			}
			return SyncResult{From: from, To: from}
		}
		q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if len(q.reruns) != 0 {
			t.Fatalf("RerunChecks ran on %v: the synced head is factoryd's own", q.reruns)
		}
		if want := []string{"after main was merged in: TestX failed on m1"}; len(q.sessions) != 2 || !slices.Equal(q.sessions[1].Findings, want) {
			t.Fatalf("sessions %d, second's findings %q, want %q", len(q.sessions), q.sessions[1].Findings, want)
		}
		if len(q.leaves) != 2 || q.leaves[0] != "checks findings went back to the agent" {
			t.Fatalf("leaves = %q", q.leaves)
		}
		if want := []string{"policy", "repro c1", "review c1", "sync c1", "leave", "policy", "repro c2", "review c2", "sync c2", "approval c2", "merge", "leave"}; !slices.Equal(q.steps, want) {
			t.Fatalf("steps = %v, want %v", q.steps, want)
		}
	})

	// the agent leaves the synced head as it is: its red checks go back
	// again, not re-run as a head the agent could not act on
	t.Run("red, untouched", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, func(sha string) ChecksResult {
			if sha == "m1" {
				return ChecksResult{State: ChecksFailure, Findings: []string{"TestX failed on m1"}}
			}
			return ChecksResult{State: ChecksSuccess}
		})
		q.sync = func(from string) SyncResult {
			if from == "c1" {
				return SyncResult{From: from, To: "m1", Main: "main1", Overlap: []string{"a.go"}}
			}
			return SyncResult{From: from, To: from}
		}
		q.onSessions(pushed("s1", "c1"), pushed("s2", "m1"), pushed("s3", "c3"))
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if len(q.reruns) != 0 || !slices.Equal(q.checks, []string{"c1", "m1", "m1", "c3"}) {
			t.Fatalf("reruns %v, checks on %v: want no re-run of the synced head", q.reruns, q.checks)
		}
		if want := []string{"after main was merged in: TestX failed on m1"}; len(q.sessions) != 3 || !slices.Equal(q.sessions[2].Findings, want) {
			t.Fatalf("sessions %d, third's findings %q, want %q", len(q.sessions), q.sessions[2].Findings, want)
		}
	})
}

// The queue's sync only re-runs the checks on the head it made; the
// approval stage reads that new head itself before its request
// (versionApprovalGreen), so the request a human sees names a head whose
// combined status was green at its creation. A pending read sends the
// card back to the agent with the queue prefix, and the request exists
// only from the next head's green read.
func TestQueueSyncedHeadIsReadBeforeTheRequest(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.sync = func(from string) SyncResult {
		if from == "c1" {
			return SyncResult{From: from, To: "m1", Main: "main1", Overlap: []string{"a.go"}}
		}
		return SyncResult{From: from, To: from}
	}
	q.headAnswers = func(sha string) ChecksResult {
		if sha == "m1" {
			return ChecksResult{State: ChecksFailure, Findings: []string{`check "test" is still pending on m1 (running)`}}
		}
		return ChecksResult{State: ChecksSuccess}
	}
	q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(q.headChecks, []string{"m1", "c2"}) {
		t.Fatalf("the approval read %v, want the synced head m1 and then c2", q.headChecks)
	}
	if want := []string{"after main was merged in: " + `check "test" is still pending on m1 (running)`}; len(q.sessions) != 2 || !slices.Equal(q.sessions[1].Findings, want) {
		t.Fatalf("sessions %d, second's findings %q, want %q", len(q.sessions), q.sessions[1].Findings, want)
	}
	if len(q.approvals) != 1 || q.approvals[0].Commit != "c2" || !q.approvals[0].Gates.ChecksPassed {
		t.Fatalf("approvals = %+v, want one request, for c2 on green checks", q.approvals)
	}
	if want := []string{"policy", "repro c1", "review c1", "sync c1", "leave", "policy", "repro c2", "review c2", "sync c2", "approval c2", "merge", "leave"}; !slices.Equal(q.steps, want) {
		t.Fatalf("steps = %v, want %v", q.steps, want)
	}
	if len(q.leaves) != 2 || q.leaves[0] != "checks findings went back to the agent" {
		t.Fatalf("leaves = %q", q.leaves)
	}
	if !slices.Equal(q.merges, []string{"9 c2 sebastian"}) {
		t.Fatalf("merges = %v", q.merges)
	}
}

// A main that conflicts with the head (409 on the update) is backlog 70's
// conflict: the agent gets the "merge main" finding and the task leaves
// the queue.
func TestSyncConflictGoesToAgent(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.sync = func(from string) SyncResult {
		if from == "c1" {
			return SyncResult{From: from, Main: "main1", Conflict: "the branch conflicts with its base: status 409"}
		}
		return SyncResult{From: from, To: from}
	}
	q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	want := conflictVerdict("the branch conflicts with its base: status 409")
	if len(q.sessions) != 2 || !slices.Equal(q.sessions[1].Findings, want.findings) {
		t.Fatalf("second session's findings = %q, want %q", q.sessions[1].Findings, want.findings)
	}
	if len(q.leaves) != 2 || q.leaves[0] != want.reason {
		t.Fatalf("leaves = %q, want the first %q", q.leaves, want.reason)
	}
	if !slices.Equal(q.checks, []string{"c1", "c2"}) || !slices.Equal(q.merges, []string{"9 c2 sebastian"}) {
		t.Fatalf("checks %v, merges %v", q.checks, q.merges)
	}
}

// checksOnRead answers WaitChecks by head and read: answers[sha]'s nth on
// the head's nth read, the last repeating; a head it does not name is
// green.
func checksOnRead(answers map[string][]ChecksResult) func(string) ChecksResult {
	reads := map[string]int{}
	return func(sha string) ChecksResult {
		a, ok := answers[sha]
		if !ok {
			return ChecksResult{State: ChecksSuccess}
		}
		reads[sha]++
		return a[min(reads[sha], len(a))-1]
	}
}

// redTest is the test check red on sha, its log's tail named.
func redTest(sha string) ChecksResult {
	return ChecksResult{State: ChecksFailure, Findings: []string{
		`check "pr / test": failure — 1 test failed (https://git.example/sf/r/actions/runs/96)` + "\n--- FAIL: TestRace on " + sha}}
}

var redAtMerge = MergeResult{Refusal: "the checks are red on c1: the merge waits for a green head", RedHead: true}

// A queue head red at its merge takes its turn again (roadmap 202), its
// approval request withdrawn first: a transient failure runs its checks
// once more on an empty commit and the rule auto merges the green re-run
// without a card; a red main pauses the queue (one ahead), and the turn
// after it merges main's fix in, whose green head goes to a new card. No
// session runs.
func TestQueueRedHeadIsReStaged(t *testing.T) {
	t.Run("a transient failure", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, checksOnRead(map[string][]ChecksResult{
			"c1": {{State: ChecksSuccess}, redTest("c1")}}), autoGates)
		q.merge = []MergeResult{redAtMerge, {Merged: true}}
		q.onSessions(pushed("s1", "c1"))
		q.signalAfter(10*time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q (%v)", final, q.finals)
		}
		if !slices.Equal(q.reruns, []string{"c1"}) || !slices.Equal(q.checks, []string{"c1", "c1", "c1-rerun"}) {
			t.Fatalf("reruns %v, checks on %v; want c1 run again once, its own red read first", q.reruns, q.checks)
		}
		if want := []string{"9 c1 " + AutoApprover, "9 c1-rerun " + AutoApprover}; !slices.Equal(q.merges, want) {
			t.Fatalf("merges = %v, want %v", q.merges, want)
		}
		if want := []string{"policy", "repro c1", "review c1", "sync c1", "merge", "withdraw", "sync c1", "merge", "leave"}; !slices.Equal(q.steps, want) {
			t.Fatalf("steps = %v, want %v", q.steps, want)
		}
		if len(q.approvals) != 0 || len(q.sessions) != 1 || !slices.Equal(q.checkedHeads, []string{"", ""}) {
			t.Fatalf("approvals %+v, %d sessions, checked heads %v; want no card, no session, each head's own checks", q.approvals, len(q.sessions), q.checkedHeads)
		}
	})

	t.Run("a fixed main", func(t *testing.T) {
		q := newQueueHarness(t, policy.QueueProject, green)
		q.ahead = func(call int) int { return map[int]int{2: 1}[call] }
		q.sync = func(from string) SyncResult {
			if len(q.syncs) == 1 {
				return SyncResult{From: from, To: from, Main: "main1"}
			}
			return SyncResult{From: from, To: "m1", Main: "main2", Base: "main1"}
		}
		q.merge = []MergeResult{redAtMerge, {Merged: true}}
		q.onSessions(pushed("s1", "c1"))
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		q.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q (%v)", final, q.finals)
		}
		if len(q.enters) != 3 || q.enters[2]-q.enters[1] != queueRecheck {
			t.Fatalf("EnterQueue ran at %v, want the paused turn asked again after %s", q.enters, queueRecheck)
		}
		if len(q.reruns) != 0 || !slices.Equal(q.checks, []string{"c1", "m1"}) {
			t.Fatalf("reruns %v, checks on %v; want main's merge checked, no re-run", q.reruns, q.checks)
		}
		if want := []string{"policy", "repro c1", "review c1", "sync c1", "approval c1", "merge", "withdraw", "sync c1", "approval m1", "merge", "leave"}; !slices.Equal(q.steps, want) {
			t.Fatalf("steps = %v, want %v", q.steps, want)
		}
		if !slices.Equal(q.merges, []string{"9 c1 sebastian", "9 m1 sebastian"}) || !slices.Equal(q.approvals[1].WhyHuman, q.approvals[0].WhyHuman) || len(q.sessions) != 1 {
			t.Fatalf("merges %v, cards %+v, %d sessions", q.merges, q.approvals, len(q.sessions))
		}
	})
}

// A head still red after its third re-run leaves the queue, so the cards
// behind it merge, and waits outside it for a human: the escalation names
// the check with its log's tail, and no session runs. A reply puts it
// back at the queue's end as a new entry (one ahead now), where its own
// checks, green by then, go to a new card; a change request instead
// sends the agent, whose fix enters the queue anew.
func TestQueueRedHeadLeavesTheQueueAfterThreeReruns(t *testing.T) {
	const tail = `check "pr / test": failure — 1 test failed (https://git.example/sf/r/actions/runs/96)` + "\n--- FAIL: TestRace on c1-rerun-rerun-rerun"
	const asked = "the checks are still red after the merge queue's re-runs, so the pull request left the queue until a human answers\n\n" + tail
	harness := func(t *testing.T, last []ChecksResult) *queueHarness {
		q := newQueueHarness(t, policy.QueueProject, checksOnRead(map[string][]ChecksResult{
			"c1":                   {{State: ChecksSuccess}, redTest("c1")},
			"c1-rerun":             {redTest("c1-rerun")},
			"c1-rerun-rerun":       {redTest("c1-rerun-rerun")},
			"c1-rerun-rerun-rerun": last,
		}))
		q.merge = []MergeResult{redAtMerge, {Merged: true}}
		q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		return q
	}
	reran := func(t *testing.T, q *queueHarness) {
		t.Helper()
		if want := []string{"c1", "c1-rerun", "c1-rerun-rerun"}; !slices.Equal(q.reruns, want) {
			t.Fatalf("reruns = %v, want %v", q.reruns, want)
		}
		if !slices.Equal(q.escalated, []string{asked}) || q.leaves[0] != leftQueue {
			t.Fatalf("escalated %q, leaves %q; want the red check's tail asked outside the queue", q.escalated, q.leaves)
		}
	}

	t.Run("an answer", func(t *testing.T) {
		q := harness(t, []ChecksResult{redTest("c1-rerun-rerun-rerun"), redTest("c1-rerun-rerun-rerun"), {State: ChecksSuccess}})
		q.ahead = func(call int) int { return map[int]int{6: 1}[call] }
		q.onSessions(pushed("s1", "c1"))
		q.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the runner is back", Comment: 700})
		q.signalAfter(5*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q (%v)", final, q.finals)
		}
		reran(t, q)
		if len(q.enters) != 7 || q.enters[5] != 3*time.Hour || q.enters[6] != 3*time.Hour+queueRecheck {
			t.Fatalf("EnterQueue ran at %v, want the answer's re-entry at 3h behind one entry", q.enters)
		}
		if !slices.Contains(q.states, "in-review") || len(q.sessions) != 1 {
			t.Fatalf("states %v, %d sessions; want in review after the answer, no session", q.states, len(q.sessions))
		}
		if len(q.approvals) != 2 || q.approvals[1].Commit != "c1-rerun-rerun-rerun" || !slices.Equal(q.merges, []string{"9 c1 sebastian", "9 c1-rerun-rerun-rerun sebastian"}) {
			t.Fatalf("approvals %+v, merges %v", q.approvals, q.merges)
		}
	})

	t.Run("a change request", func(t *testing.T) {
		q := harness(t, []ChecksResult{redTest("c1-rerun-rerun-rerun")})
		q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		q.signalAfter(3*time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "TestRace is real: fix it"})
		q.signalAfter(5*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := q.run(t); final != "done" {
			t.Fatalf("final = %q (%v)", final, q.finals)
		}
		reran(t, q)
		if len(q.sessions) != 2 || !slices.Equal(q.sessions[1].Findings, []string{"reviewer sebastian requested changes: TestRace is real: fix it"}) {
			t.Fatalf("%d sessions, second's findings %q", len(q.sessions), q.sessions)
		}
		if len(q.enters) != 6 || q.enters[5] != 3*time.Hour || !slices.Equal(q.merges, []string{"9 c1 sebastian", "9 c2 sebastian"}) {
			t.Fatalf("EnterQueue ran at %v, merges %v; want c2 entering anew after the change request", q.enters, q.merges)
		}
	})
}

// The escalation of a head that left the queue says what a reply does.
func TestQueueLeftEscalationSaysWhatAReplyDoes(t *testing.T) {
	task := Task{ID: "t1", Kind: "agent", Status: "waiting-human", Objective: "Links", Repository: "sf/r", SHA: "abc123", IssueNumber: 5, PRNumber: 9}
	runner, hub, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	if _, err := execActivity(t, runner, ActivityEscalate, "t1", leftQueue+"\n\n"+redTest("c1").Findings[0], []string(nil)); err != nil {
		t.Fatal(err)
	}
	want := "```\n\nReply on this issue to put the pull request back at the end of the merge queue, request changes on it to send the task back to the agent, or close the issue to fail the task."
	if len(hub.comments) != 1 || !strings.HasSuffix(hub.comments[0], want) {
		t.Fatalf("comments %q", hub.comments)
	}
}

// A check red at the merge that passes on its re-run, the same tree, is
// flaky: a checks_flaky step names it, no session hears of it, and the
// new card says no more than the first.
func TestQueueFlakyCheckIsAStepNotAFinding(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, checksOnRead(map[string][]ChecksResult{
		"c1": {{State: ChecksSuccess}, redTest("c1")}}))
	q.merge = []MergeResult{redAtMerge, {Merged: true}}
	q.onSessions(pushed("s1", "c1"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	q.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q (%v)", final, q.finals)
	}
	var flaky []map[string]any
	for _, r := range q.recorded {
		switch r.step {
		case "checks_flaky":
			flaky = append(flaky, r.detail)
		case "stage_findings":
			t.Fatalf("findings recorded: %v", r.detail)
		}
	}
	want := map[string]any{"commit": "c1", "head": "c1-rerun",
		"checks": []string{`check "pr / test": failure — 1 test failed (https://git.example/sf/r/actions/runs/96)`}}
	if len(flaky) != 1 || fmt.Sprint(flaky[0]) != fmt.Sprint(want) {
		t.Fatalf("checks_flaky steps = %v, want %v", flaky, want)
	}
	if len(q.sessions) != 1 || len(q.approvals) != 2 || q.approvals[1].Commit != "c1-rerun" || !slices.Equal(q.approvals[1].WhyHuman, q.approvals[0].WhyHuman) {
		t.Fatalf("%d sessions, approvals %+v; want no session and c1-rerun's card as c1's", len(q.sessions), q.approvals)
	}
	if !slices.Equal(q.merges, []string{"9 c1 sebastian", "9 c1-rerun sebastian"}) {
		t.Fatalf("merges = %v", q.merges)
	}
}

// The re-stage withdraws the request the red merge left open: the
// console's approve then finds none pending and says so, instead of an
// approval the workflow would drop.
func TestWithdrawApprovalClosesTheRequest(t *testing.T) {
	r, store, _ := newGateRunner(t, gateTask, &gateForgejo{})
	ctx := context.Background()
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", PRNumber: 9})
	if _, err := execActivity(t, r, ActivityWithdrawApproval, "t1"); err != nil {
		t.Fatal(err)
	}
	a := store.approvals[0]
	if a.Status != "denied" || a.DecidedBy != FactoryDecider || a.DecisionNote != "withdrawn: the checks were red at the merge; the merge queue runs them again, and a green head gets a new request" {
		t.Fatalf("approval = %+v, want withdrawn by factoryd", a)
	}
	wf := &fakeWorkflows{}
	if err := ApproveMerge(ctx, store, wf, "t1", 9, "sebastian"); err == nil || len(wf.signals) != 0 {
		t.Fatalf("ApproveMerge = %v, signals %q; want the request not open, no signal", err, wf.signals)
	}
}

// After a cheap turn the approval stage reads the green of the head the
// checks ran on, not the synced head whose checks have only started: the
// synced head merges on that green, and no session hears of its pending
// checks.
func TestCheapTurnReadsTheCheckedHead(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.sync = func(from string) SyncResult {
		if from == "c1" {
			return SyncResult{From: from, To: "m1", Main: "main1", Base: "b0"}
		}
		return SyncResult{From: from, To: from}
	}
	reads := 0
	q.headAnswers = func(sha string) ChecksResult {
		if sha == "m1" {
			if reads++; reads == 1 {
				return ChecksResult{State: ChecksFailure, Findings: []string{`check "test" is still pending on m1 (running)`}}
			}
		}
		return ChecksResult{State: ChecksSuccess}
	}
	q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(q.headChecks, []string{"c1"}) || len(q.sessions) != 1 {
		t.Fatalf("the approval read %v, %d sessions; want c1's green read, no session", q.headChecks, len(q.sessions))
	}
	if !slices.Equal(q.merges, []string{"9 m1 sebastian"}) || !slices.Equal(q.checkedHeads, []string{"c1"}) {
		t.Fatalf("merges %v on checked heads %v; want m1 merged on c1's green", q.merges, q.checkedHeads)
	}
}

// A pass ends with no checked head: after a cheap turn's card went back
// to the agent, the next pass reads and merges on its own head's checks.
func TestCheckedHeadEndsWithItsPass(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.sync = func(from string) SyncResult {
		if from == "c1" {
			return SyncResult{From: from, To: "m1", Main: "main1", Base: "b0"}
		}
		return SyncResult{From: from, To: from}
	}
	q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	q.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename it"})
	q.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(q.headChecks, []string{"c1", "c2"}) {
		t.Fatalf("the approvals read %v, want c1 for the cheap turn and c2 for the next pass", q.headChecks)
	}
	if !slices.Equal(q.merges, []string{"9 c2 sebastian"}) || !slices.Equal(q.checkedHeads, []string{""}) {
		t.Fatalf("merges %v on checked heads %v; want c2 merged on its own checks", q.merges, q.checkedHeads)
	}
}

// Every way out of the turn leaves the queue: the merge, a return to the
// agent, a failure, a close while waiting; Finish leaves it too and wakes
// the new head.
func TestQueueReleasedOnEveryExit(t *testing.T) {
	for _, c := range []struct {
		name   string
		ahead  func(int) int
		signal func(q *queueHarness)
		final  string
		leaves []string
	}{
		{"merge", nil, func(q *queueHarness) {
			q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		}, "done", []string{"pull request merged by factory-merge after sebastian's approval"}},
		{"back to the agent", nil, func(q *queueHarness) {
			q.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename it"})
			q.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		}, "done", []string{"changes requested", "pull request merged by factory-merge after sebastian's approval"}},
		{"failure", nil, func(q *queueHarness) {
			q.signalAfter(time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
		}, "failed", []string{"pull request closed without merge"}},
		{"close while waiting", func(int) int { return 2 }, func(q *queueHarness) {
			q.signalAfter(time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		}, "closed", []string{ReasonIssueClosed + " without merge"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := newQueueHarness(t, policy.QueueProject, green)
			if c.ahead != nil {
				q.ahead = c.ahead
			}
			q.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", mock.Anything).Return(false, nil)
			q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
			c.signal(q)
			if final := q.run(t); final != c.final {
				t.Fatalf("final = %q (%v), want %q", final, q.finals, c.final)
			}
			if !slices.Equal(q.leaves, c.leaves) {
				t.Fatalf("leaves = %q, want %q", q.leaves, c.leaves)
			}
		})
	}

	t.Run("finish", func(t *testing.T) {
		r, _, store := newTestRunner(t, Task{ID: "a1", Status: "in-review", Repository: "linkshort/linkshort", PRNumber: 9}, newFakeKube(), &fakeObjectStore{})
		store.CreateTask(context.Background(), Task{ID: "a2", Status: "in-review", Repository: "linkshort/linkshort", PRNumber: 10})
		ctx := context.Background()
		for _, id := range []string{"a1", "a2"} {
			if _, err := r.EnterQueue(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Finish(ctx, "a1", "done", "pull request merged"); err != nil {
			t.Fatal(err)
		}
		if len(store.queue) != 1 || store.queue[0].task != "a2" {
			t.Fatalf("queue after a1's Finish = %+v", store.queue)
		}
		if wf := r.cfg.Workflows.(*fakeWorkflows); !slices.Contains(wf.signals, "a2 queue_turn  #0: ") {
			t.Fatalf("signals = %q: the new head is not woken", wf.signals)
		}
	})
}

// With merge queue none the approval stage is today's: no queue activity
// runs, and the steps are the ones before the queue existed.
func TestQueueNoneUnchanged(t *testing.T) {
	q := newQueueHarness(t, policy.QueueNone, green)
	q.onSessions(pushed("s1", "c1"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(q.enters)+len(q.syncs)+len(q.leaves) != 0 {
		t.Fatalf("queue activities ran: enters %v, syncs %v, leaves %v", q.enters, q.syncs, q.leaves)
	}
	if want := []string{"policy", "repro c1", "review c1", "approval c1", "merge"}; !slices.Equal(q.steps, want) {
		t.Fatalf("steps = %v, want %v", q.steps, want)
	}
}

// The heal pass signals queue_turn to every queue's head still waiting in
// gate merge-queue (a lost signal), never to a head at its turn or to an
// entry behind a head.
func TestHealSignalsQueueHead(t *testing.T) {
	in, store, _ := newTestIntake(t)
	in.Projects = codeRepoOnly{}
	ctx := context.Background()
	for _, tk := range []Task{
		{ID: "a1", Status: "in-review", Kind: "agent", Repository: "linkshort/linkshort", Gate: GateMergeQueue},
		{ID: "a2", Status: "in-review", Kind: "agent", Repository: "linkshort/linkshort", Gate: GateMergeQueue},
		{ID: "b1", Status: "in-review", Kind: "agent", Repository: "sf/other", Gate: GateApproval},
		{ID: "b2", Status: "in-review", Kind: "agent", Repository: "sf/other", Gate: GateMergeQueue},
	} {
		store.CreateTask(ctx, tk)
		store.SetGate(ctx, tk.ID, tk.Gate, time.Now())
		store.EnterQueue(ctx, tk.Repository, tk.ID)
	}
	HealPass(ctx, store, in.Workflows, in, &Completion{Store: store})
	var turns []string
	for _, s := range workflowsOf(in).signals {
		if strings.Contains(s, " "+SignalQueueTurn+" ") {
			turns = append(turns, s)
		}
	}
	if want := []string{"a1 queue_turn  #0: "}; !slices.Equal(turns, want) {
		t.Fatalf("queue_turn signals = %q, want %q", turns, want)
	}
}
