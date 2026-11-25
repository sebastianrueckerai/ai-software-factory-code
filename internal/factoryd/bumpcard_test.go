package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The bump's card through the real admission and the real state moves: the
// bump pass files it with its pull request on its row, the work bound
// admits it while it stays queued, and its run takes the pull request from
// the queue into review. Green checks land it with no session; a red check
// goes to a session from the pull request's head. Over a full bound it
// waits queued until a slot frees.
func TestABumpCardEntersReviewFromTheQueue(t *testing.T) {
	red := ChecksResult{State: ChecksFailure, Findings: []string{"image-build: the store lacks node v24.22.4 node-v24.22.4-linux-x64.tar.xz"}}
	for name, c := range map[string]struct {
		full     bool
		checks   []ChecksResult
		sessions []string
	}{
		"green checks land it alone":    {checks: []ChecksResult{{State: ChecksSuccess}}},
		"a red check goes to a session": {checks: []ChecksResult{red, {State: ChecksSuccess}}, sessions: []string{"c7"}},
		"a full bound keeps it queued":  {full: true, checks: []ChecksResult{{State: ChecksSuccess}}},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBumpRun(t, bumped)
			pulled := &fakeWorkflows{}
			b.r.cfg.Workflows = BoundedWorkflows{Workflows: pulled, Store: b.store, Policy: func() policy.Tree { return b.r.cfg.Policy().Tree }}
			ctx, repo := context.Background(), "sf/ai-software-factory-code"
			if c.full {
				for i := range b.r.cfg.Policy().Tree.WorkInProgressOf(repo) {
					id := fmt.Sprintf("t-busy%d", i)
					b.store.tasks[id] = Task{ID: id, Repository: repo, Kind: "agent", Track: "direct", Status: "running"}
				}
			}
			if _, err := b.r.RoutineRun(ctx, RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "run-now", Due: time.Now()}); err != nil {
				t.Fatal(err)
			}
			card := routineCard(t, b.store)
			if c.full {
				if card.Status != "queued" || card.AdmittedAt != nil || len(pulled.started) > 0 || !strings.HasPrefix(card.QueueReason, WorkBoundReason) {
					t.Fatalf("card %s %v %q, started %v: want it waiting queued behind the bound", card.Status, card.AdmittedAt, card.QueueReason, pulled.started)
				}
				busy := b.store.tasks["t-busy0"]
				busy.Status = "done"
				b.store.tasks["t-busy0"] = busy
				pullQueued(ctx, b.store, b.r.cfg.Workflows, repo)
				card, _, _ = b.store.Task(ctx, card.ID)
			}
			if card.Status != "queued" || card.AdmittedAt == nil || card.PRNumber != 2400 || !slices.Equal(pulled.started, []string{card.ID}) {
				t.Fatalf("card %s %v pr %d, started %v: want it admitted and still queued with its pull request", card.Status, card.AdmittedAt, card.PRNumber, pulled.started)
			}

			h := newWorkflowHarness(t)
			// Reconcile reads the row's status and pull request as they stand
			h.state = RunState{Status: card.Status, Source: card.Source, PR: PullRequest{Number: card.PRNumber, URL: "https://git.example/sf/ai-software-factory-code/pulls/2400"}, Head: "c7"}
			var moves []string
			h.setState = func(ctx context.Context, state string) error {
				err := b.r.SetState(ctx, card.ID, state)
				row, _, _ := b.store.Task(ctx, card.ID)
				moves = append(moves, state+" → "+row.Status)
				return err
			}
			// verify takes the open pull request of the card's branch
			h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: card.PRNumber, URL: h.state.PR.URL}, nil)
			n := 0
			h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
				n++
				return c.checks[min(n, len(c.checks))-1], nil
			})
			h.onSessions(pushed("s1", "c8"))
			copied := h.onCopy(nil, nil)
			order := h.activityOrder()
			h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 2400})
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v, moves %v", final, h.finals, moves)
			}
			if !slices.Contains(moves, "in-review → in-review") || slices.ContainsFunc(moves, func(m string) bool { return strings.HasSuffix(m, "→ queued") }) {
				t.Errorf("moves %v: want the row in review, never left queued", moves)
			}
			var from []string
			for _, s := range h.sessions {
				from = append(from, s.StartRef)
			}
			if !slices.Equal(from, c.sessions) {
				t.Errorf("sessions from %v, want %v", from, c.sessions)
			}
			// the pull request's head is copied and checked before any session
			if (*copied)[0] != "c7" || len(c.sessions) > 0 && !before(*order, ActivityWaitChecks, ActivityRunSession) {
				t.Errorf("copies %v, order %v: want c7 copied and checked first", *copied, *order)
			}
		})
	}
}

func routineCard(t *testing.T, s *fakeStore) Task {
	t.Helper()
	for _, c := range s.tasks {
		if c.Source == "routine" {
			return c
		}
	}
	t.Fatal("no routine card")
	return Task{}
}
