package pg

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

var holdParkAt = time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)

type holdPark struct {
	s   *PGStore
	r   *Runner
	b   BoundedWorkflows
	wf  *fakeWorkflows
	ctx context.Context
}

// holdParkRig is a project sf/r at work_in_progress bound and surge 0, over
// a Postgres store, with cards as given.
func holdParkRig(t *testing.T, name string, bound int, cards ...Task) *holdPark {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	zero := 0
	v := policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
		"r": {Repository: "sf/r", WorkInProgress: bound, Surge: &zero}}}}
	wf := &fakeWorkflows{}
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	b := BoundedWorkflows{Workflows: wf, Store: s, Policy: func() policy.Tree { return v.Tree }}
	cfg.Workflows = b
	for i, c := range cards {
		c.Repository, c.Kind, c.BudgetEUR, c.IssueNumber = "sf/r", "agent", 1, int64(i+1)
		if c.Track == "" {
			c.Track = policy.TrackDirect
		}
		c.Source = "webhook"
		if err := s.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	return &holdPark{s: s, r: PgRigRunner(s, &fakeObjectStore{}, cfg, nil, &Forgejo{BaseURL: "http://127.0.0.1:1", BotUser: "factory-agent", BotPassword: "bot-pw"}), wf: wf, ctx: ctx, b: b}
}

func (h *holdPark) hold(t *testing.T, id, release string) {
	t.Helper()
	if ok, err := h.s.SetCardHold(h.ctx, id, CardHold{Reason: "out of the way", Release: release, By: "sebastian", At: holdParkAt}); err != nil || !ok {
		t.Fatalf("SetCardHold(%s) = %v, %v", id, ok, err)
	}
}

func TestAdmitResumeParksAHeldRunningCard(t *testing.T) {
	h := holdParkRig(t, "hold_park_running", 1, Task{ID: "a", Status: "running"}, Task{ID: "b", Status: "queued"})
	h.hold(t, "a", "")
	s, err := h.r.AdmitResume(h.ctx, "a", holdParkAt)
	if err != nil || s.Granted {
		t.Fatalf("AdmitResume = %+v, %v; want not granted", s, err)
	}
	a := readTask(t, h.s, "a")
	if a.Status != "queued" || a.ResumedAt == nil || !a.ResumedAt.Equal(holdParkAt) || a.QueueReason != "on hold: out of the way" {
		t.Fatalf("a = %s, resumed %v, reason %q", a.Status, a.ResumedAt, a.QueueReason)
	}
	if !slices.Equal(h.wf.started, []string{"b"}) {
		t.Fatalf("started = %v, want b admitted into the freed slot", h.wf.started)
	}
	if b := readTask(t, h.s, "b"); b.AdmittedAt == nil {
		t.Fatal("b was not admitted")
	}
}

func TestAHeldWaiterBlocksNoFreshCard(t *testing.T) {
	h := holdParkRig(t, "hold_park_fresh", 2, Task{ID: "a", Status: "running"}, Task{ID: "b", Status: "queued"})
	h.hold(t, "a", "")
	if err := h.s.SetResumeWait(h.ctx, "a", "on hold: out of the way", holdParkAt); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Start(h.ctx, "b"); err != nil {
		t.Fatal(err)
	}
	b := readTask(t, h.s, "b")
	if b.AdmittedAt == nil || b.QueueReason != "" || !slices.Equal(h.wf.started, []string{"b"}) {
		t.Fatalf("b admitted %v, reason %q, started %v; want it admitted with no reason", b.AdmittedAt, b.QueueReason, h.wf.started)
	}
}

func TestAHeldWaiterIsNotAhead(t *testing.T) {
	h := holdParkRig(t, "hold_park_ahead", 1, Task{ID: "a", Status: "running"}, Task{ID: "c", Status: "waiting-human"})
	h.hold(t, "a", "")
	if err := h.s.SetResumeWait(h.ctx, "a", "on hold: out of the way", holdParkAt); err != nil {
		t.Fatal(err)
	}
	if err := h.s.SetResumeWait(h.ctx, "c", "resumed: x", holdParkAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err := h.r.AdmitResume(h.ctx, "c", holdParkAt.Add(time.Minute))
	if err != nil || !s.Granted {
		t.Fatalf("AdmitResume(c) = %+v, %v; want granted past the held card", s, err)
	}
}

func TestTheReleaseCardEndsTheParkedHold(t *testing.T) {
	h := holdParkRig(t, "hold_park_release", 1, Task{ID: "a", Status: "running"}, Task{ID: "r", Status: "running"})
	h.hold(t, "a", "r")
	if err := h.s.SetResumeWait(h.ctx, "a", "on hold: out of the way", holdParkAt); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB().ExecContext(h.ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 'r'`); err != nil {
		t.Fatal(err)
	}
	s, err := h.r.AdmitResume(h.ctx, "a", holdParkAt)
	if err != nil || !s.Granted {
		t.Fatalf("AdmitResume = %+v, %v; want granted", s, err)
	}
	if a := readTask(t, h.s, "a"); a.Hold != nil || a.Status != "running" {
		t.Fatalf("a = %s, hold %+v; want running, no hold", a.Status, a.Hold)
	}
	evs, err := h.s.TaskEventsOfKind(h.ctx, "a", EventCardHoldEnded, 5)
	if err != nil || len(evs) != 1 || evs[0].Detail["release"] != float64(2) {
		t.Fatalf("hold-ended events = %+v, %v; want one naming release issue 2", evs, err)
	}
}

func TestAdmitResumeParksAHeldCardBackFromReview(t *testing.T) {
	h := holdParkRig(t, "hold_park_review", 2, Task{ID: "a", Status: "in-review", Gate: GateChecks})
	if _, err := h.s.DB().ExecContext(h.ctx, `UPDATE tasks SET hold_at = $1, hold_by = 'sebastian', hold_reason = 'out of the way' WHERE id = 'a'`, holdParkAt); err != nil {
		t.Fatal(err)
	}
	s, err := h.r.AdmitResume(h.ctx, "a", holdParkAt)
	if err != nil || s.Granted {
		t.Fatalf("AdmitResume = %+v, %v; want not granted", s, err)
	}
	a := readTask(t, h.s, "a")
	if a.Status != "queued" || a.ResumedAt == nil || !a.ResumedAt.Equal(holdParkAt) || a.QueueReason != "on hold: out of the way" {
		t.Fatalf("a = %s, resumed %v, reason %q", a.Status, a.ResumedAt, a.QueueReason)
	}
}
