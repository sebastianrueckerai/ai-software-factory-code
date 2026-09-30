package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"maps"
	"math"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
)

// fromJSON must surface a decode failure rather than silently returning an
// empty slice, so corrupt required_capabilities/findings columns fail the
// read instead of quietly discarding data.
func TestFromJSONSurfacesDecodeErrors(t *testing.T) {
	if _, err := fromJSON([]byte(`not json`)); err == nil {
		t.Fatal("want an error for malformed JSON, got nil")
	}
	out, err := fromJSON(nil)
	if err != nil || out != nil {
		t.Fatalf("fromJSON(nil) = %v, %v, want nil, nil", out, err)
	}
	out, err = fromJSON([]byte(`["a","b"]`))
	if err != nil || !slices.Equal(out, []string{"a", "b"}) {
		t.Fatalf("fromJSON = %v, %v, want [a b], nil", out, err)
	}
}

// fakeStore is the in-memory Store for tests of everything that persists.
type fakeStore struct {
	tasks        map[string]Task
	sessions     map[string]Session
	started      map[string]bool
	finished     map[string]time.Time
	settled      map[string]bool
	intakeEvents []IntakeEvent
	deliveries   map[string]bool
	pruned       []time.Duration
	approvals    []ApprovalRequest
	gates        []string // "task gate since", in order
	projects     []Project
	alertsMu     sync.Mutex
	alerts       []Alert
	rolls        []RollRequest
	// monthSpend is MonthSpend's answer, monthRefusals MonthCapRefusal's
	// by task
	monthSpend    MonthSpend
	monthRefusals map[string]string
	// createFails fails that many CreateTask calls
	createFails int
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]Task{}, sessions: map[string]Session{},
		started: map[string]bool{}, finished: map[string]time.Time{}, settled: map[string]bool{}}
}

func (s *fakeStore) CreateTask(ctx context.Context, t Task) error {
	if s.createFails > 0 {
		s.createFails--
		return errors.New("insert failed")
	}
	if t.Kind == "" {
		t.Kind = "agent"
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.Kind == "agent" && t.Status != "backlog" && t.Gate == "" {
		t.Gate, t.GateSince = GateIntake, &t.CreatedAt
	}
	s.tasks[t.ID] = t
	return nil
}

func (s *fakeStore) OpenReportTask(ctx context.Context, t Task) (Task, error) {
	if have, ok, _ := s.ReportOf(ctx, t.Parent, t.Source); ok {
		return have, nil
	}
	return t, s.CreateTask(ctx, t)
}

func (s *fakeStore) ReportOf(ctx context.Context, parent, source string) (Task, bool, error) {
	for _, t := range s.tasks {
		if t.Parent == parent && t.Source == source {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

func (s *fakeStore) Task(ctx context.Context, id string) (Task, bool, error) {
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *fakeStore) TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error) {
	for _, t := range s.tasks {
		if t.Repository == repository && t.IssueNumber == issueNumber && issueNumber != 0 {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

func (s *fakeStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	for _, have := range s.intakeEvents {
		if (e.DeliveryID != "" && have.DeliveryID == e.DeliveryID) ||
			(e.CommentID != 0 && have.TaskID == e.TaskID && have.Kind == e.Kind && have.CommentID == e.CommentID) {
			return 0, nil
		}
	}
	e.ID = int64(len(s.intakeEvents) + 1)
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	s.intakeEvents = append(s.intakeEvents, e)
	return e.ID, nil
}

func (s *fakeStore) IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	var out []IntakeEvent
	for _, e := range s.intakeEvents {
		if e.TaskID == taskID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) SetSignalled(ctx context.Context, id int64, signalled bool) (bool, error) {
	for i := range s.intakeEvents {
		if s.intakeEvents[i].ID == id {
			changed := s.intakeEvents[i].Signalled != signalled
			s.intakeEvents[i].Signalled = signalled
			return changed, nil
		}
	}
	return false, fmt.Errorf("intake event %d: no such row", id)
}

func (s *fakeStore) UnaddressedChangeRequests(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	var latest time.Time
	for _, sess := range s.sessions {
		if sess.TaskID == taskID && sess.CreatedAt.After(latest) {
			latest = sess.CreatedAt
		}
	}
	var out []IntakeEvent
	for _, e := range s.intakeEvents {
		if e.TaskID == taskID && e.Signalled && slices.Contains(changeRequestKinds, e.Kind) && e.CreatedAt.After(latest) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) TransitionTask(ctx context.Context, id string, from []string, to string) error {
	t, ok := s.tasks[id]
	if !ok || !slices.Contains(from, t.Status) {
		return fmt.Errorf("task %s: not found (not in %v, cannot move to %s)", id, from, to)
	}
	t.Status = to
	if Final(to) {
		now := time.Now()
		s.finished[id] = now
		t.FinishedAt, t.Gate, t.GateSince = &now, "", nil
	}
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) MarkStarted(ctx context.Context, id string) error {
	t, ok := s.tasks[id]
	if !ok || !slices.Contains([]string{"queued", "running", "in-review", "waiting-human", "waiting-poster"}, t.Status) {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.Status = "running"
	s.tasks[id] = t
	s.started[id] = true
	return nil
}

func (s *fakeStore) SetTaskPR(ctx context.Context, id string, number int64) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.PRNumber = number
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) MarkHalfWay(ctx context.Context, id string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.HalfWayDone = true
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) SetAgentSummary(ctx context.Context, id, summary string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.AgentSummary = summary
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) ToHumanLane(ctx context.Context, id string) (bool, error) {
	t, ok := s.tasks[id]
	if !ok || t.Kind != "agent" || t.Status != "queued" {
		return false, nil
	}
	t.Kind, t.Status, t.Gate, t.GateSince, t.Lane, t.Process = "human", "waiting-human", "", nil, "", ""
	s.tasks[id] = t
	return true, nil
}

func (s *fakeStore) BackfillProcess(ctx context.Context, repository, process string) error {
	for id, t := range s.tasks {
		if t.Repository == repository && t.Lane != "" && t.Process == "" && !Final(t.Status) {
			t.Process = process
			s.tasks[id] = t
		}
	}
	return nil
}

func (s *fakeStore) EnterLane(ctx context.Context, id, lane, class, sha string) (bool, error) {
	t, ok := s.tasks[id]
	if !ok || t.Status != "backlog" {
		return false, nil
	}
	now := time.Now()
	t.Status, t.Lane, t.RequiredCapabilities, t.SHA, t.Gate, t.GateSince = "queued", lane, []string{class}, sha, GateIntake, &now
	s.tasks[id] = t
	return true, nil
}

// testSeeded are the seeded tokens a test's Forgejo holds.
var testSeeded = map[string]string{policy.SecretCIRegistrar: "registrar-tok", policy.SecretMerge: "merge-tok", policy.SecretConfig: "config-tok"}

// seedProject is repository as the code repository's project: seed, agents
// coder, the registrar token reading and running its Actions, the merge
// token merging.
func seedProject(repository string) Project {
	return Project{Name: strings.ReplaceAll(repository, "/", "-"), Repository: repository, MergeRule: "human", Objects: "seed", Agents: "coder",
		File: true, Tokens: policy.SeedTokens{Read: policy.SecretCIRegistrar, Actions: policy.SecretCIRegistrar, Merge: policy.SecretMerge},
		Process: "maintenance", Lanes: maintenanceLanes}
}

// ProjectByRepository: with no projects set, every repository is a seed
// project agents work on, as the code repository is.
func (s *fakeStore) ProjectByRepository(ctx context.Context, repository string) (Project, bool, error) {
	if s.projects == nil {
		return seedProject(repository), true, nil
	}
	for _, p := range s.projects {
		if p.Repository == repository {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

func (s *fakeStore) SetTaskUsage(ctx context.Context, id string, u TaskUsage) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.Usage = u
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) DeliverySeen(ctx context.Context, id string) (bool, error) {
	return s.deliveries[id], nil
}

func (s *fakeStore) RecordDelivery(ctx context.Context, id string) error {
	if s.deliveries == nil {
		s.deliveries = map[string]bool{}
	}
	s.deliveries[id] = true
	return nil
}

func (s *fakeStore) PruneDeliveries(ctx context.Context, age time.Duration) error {
	s.pruned = append(s.pruned, age)
	return nil
}

func (s *fakeStore) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	var tasks []Task
	for _, t := range s.tasks {
		if t.Status == status {
			tasks = append(tasks, t)
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority > tasks[j].Priority
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, nil
}

func (s *fakeStore) RecentlyFinishedTasks(ctx context.Context, within time.Duration) ([]string, error) {
	var ids []string
	for id, at := range s.finished {
		if st := s.tasks[id].Status; (st == "done" || st == "failed") && time.Since(at) < within {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *fakeStore) SettleSpend(ctx context.Context, id string, reported float64) (bool, error) {
	t, ok := s.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s: not found", id)
	}
	changed := reported > t.SpentEUR
	t.SpentEUR = max(t.SpentEUR, reported)
	s.tasks[id] = t
	return changed, nil
}

func (s *fakeStore) UnsettledTasks(ctx context.Context, olderThan time.Duration) ([]string, error) {
	var ids []string
	for id, at := range s.finished {
		if st := s.tasks[id].Status; (st == "done" || st == "failed") && time.Since(at) >= olderThan && !s.settled[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *fakeStore) SettleSpendFinal(ctx context.Context, id string, reported float64) (bool, error) {
	t, ok := s.tasks[id]
	if !ok || s.settled[id] {
		return false, fmt.Errorf("task %s: not found", id)
	}
	changed := reported != t.SpentEUR
	t.SpentEUR = reported
	s.tasks[id] = t
	s.settled[id] = true
	return changed, nil
}

func (s *fakeStore) InsertSession(ctx context.Context, sess Session) error {
	s.sessions[sess.ID] = sess
	if t, ok := s.tasks[sess.TaskID]; ok {
		t.ModelAlias = sess.Model
		s.tasks[sess.TaskID] = t
	}
	return nil
}

func (s *fakeStore) Session(ctx context.Context, id string) (Session, bool, error) {
	sess, ok := s.sessions[id]
	return sess, ok, nil
}

func (s *fakeStore) UpdateSession(ctx context.Context, id string, findings []string, restartState []byte) error {
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	sess.Findings = findings
	sess.RestartState = restartState
	s.sessions[id] = sess
	return nil
}

var _ Store = (*fakeStore)(nil)

// Requires a disposable Postgres: FACTORYD_TEST_DATABASE_URL must point at a
// database this test may wipe. Skipped otherwise; CI stays hermetic.
func (s *fakeStore) SetTaskRisk(ctx context.Context, id, class string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.RiskClass = class
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) OpenApproval(ctx context.Context, a ApprovalRequest) (int64, error) {
	for i := range s.approvals {
		if a.Kind == "merge" && s.approvals[i].TaskID == a.TaskID && s.approvals[i].Kind == "merge" && s.approvals[i].Status == "pending" {
			s.approvals[i].Status, s.approvals[i].DecidedBy, s.approvals[i].DecisionNote = "denied", FactoryDecider, "superseded by a newer request"
		}
	}
	a.ID, a.Status = int64(len(s.approvals)+1), "pending"
	s.approvals = append(s.approvals, a)
	return a.ID, nil
}

func (s *fakeStore) ResolveApprovals(ctx context.Context, taskID, kind, status, by, note string) error {
	for i := range s.approvals {
		if s.approvals[i].TaskID == taskID && (kind == "" || s.approvals[i].Kind == kind) && s.approvals[i].Status == "pending" {
			s.approvals[i].Status, s.approvals[i].DecidedBy, s.approvals[i].DecisionNote = status, by, note
		}
	}
	return nil
}

func (s *fakeStore) NoteApproval(ctx context.Context, taskID, kind, note string) error {
	for i := range s.approvals {
		if s.approvals[i].TaskID == taskID && s.approvals[i].Kind == kind && s.approvals[i].Status == "pending" {
			s.approvals[i].DecisionNote = note
		}
	}
	return nil
}

func (s *fakeStore) ApprovalRequests(ctx context.Context, status string) ([]ApprovalRequest, error) {
	var out []ApprovalRequest
	for _, a := range s.approvals {
		if a.Status == status {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) SetGate(ctx context.Context, id, gate string, since time.Time) error {
	t, ok := s.tasks[id]
	if !ok || t.Status == "done" || t.Status == "failed" {
		return nil
	}
	t.Gate, t.GateSince = gate, &since
	s.tasks[id] = t
	s.gates = append(s.gates, id+" "+gate+" "+since.UTC().Format(time.RFC3339Nano))
	return nil
}

func (s *fakeStore) InsertAlert(ctx context.Context, a Alert) (int64, error) {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	if a.Kind == AlertScreening && slices.ContainsFunc(s.alerts, func(b Alert) bool { return b.Kind == AlertScreening && b.TaskID == a.TaskID }) {
		return 0, nil
	}
	if a.Kind == AlertMonthCap && slices.ContainsFunc(s.alerts, func(b Alert) bool { return b.Kind == AlertMonthCap && b.Fingerprint == a.Fingerprint }) {
		return 0, nil
	}
	a.ID = int64(len(s.alerts) + 1)
	s.alerts = append(s.alerts, a)
	return a.ID, nil
}

func (s *fakeStore) MonthSpend(ctx context.Context, at time.Time) (MonthSpend, error) {
	return s.monthSpend, nil
}

func (s *fakeStore) MonthCapRefusal(ctx context.Context, taskID string, since time.Time) (string, bool, error) {
	reason, ok := s.monthRefusals[taskID]
	return reason, ok, nil
}

func (s *fakeStore) SetAlertTask(ctx context.Context, id int64, taskID string, detail map[string]any) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.ID == id {
			a.TaskID, a.Detail = taskID, maps.Clone(a.Detail)
			if a.Detail == nil {
				a.Detail = map[string]any{}
			}
			maps.Copy(a.Detail, detail)
			s.alerts[i] = a
			return nil
		}
	}
	return ErrNotFound
}

func (s *fakeStore) ResolveAlert(ctx context.Context, id int64, kind, verdict, by string, detail map[string]any) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.ID == id && a.Kind == kind && a.Verdict == "" {
			now := time.Now()
			a.Verdict, a.ResolvedBy, a.ResolvedAt = verdict, by, &now
			a.Detail = maps.Clone(a.Detail)
			if a.Detail == nil {
				a.Detail = map[string]any{}
			}
			maps.Copy(a.Detail, detail)
			s.alerts[i] = a
			return nil
		}
	}
	return fmt.Errorf("open gate_bound alert: %w", ErrNotFound)
}

func (s *fakeStore) AlertByFingerprint(ctx context.Context, kind, fingerprint string) (Alert, bool, error) {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i := len(s.alerts) - 1; i >= 0; i-- {
		if a := s.alerts[i]; a.Kind == kind && a.Fingerprint == fingerprint {
			a.Detail = maps.Clone(a.Detail)
			return a, true, nil
		}
	}
	return Alert{}, false, nil
}

func (s *fakeStore) MergeAlertDetail(ctx context.Context, id int64, detail map[string]any) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.ID == id {
			a.Detail = maps.Clone(a.Detail)
			if a.Detail == nil {
				a.Detail = map[string]any{}
			}
			maps.Copy(a.Detail, detail)
			s.alerts[i] = a
			return nil
		}
	}
	return ErrNotFound
}

func (s *fakeStore) JudgeTaskAlerts(ctx context.Context, taskID, kind, verdict, by string) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.TaskID == taskID && a.Kind == kind && a.Verdict == "" {
			now := time.Now()
			a.Verdict, a.ResolvedBy, a.ResolvedAt = verdict, by, &now
			s.alerts[i] = a
		}
	}
	return nil
}

func (s *fakeStore) alertRows() []Alert {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	return slices.Clone(s.alerts)
}

func TestPGStore(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()

	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, ledger, holds, approval_requests`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Booting twice must be safe: migrations are idempotent.
	s2, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("second OpenStore: %v", err)
	}
	defer s2.db.Close()

	in := Task{
		ID:                   "t1",
		Priority:             2,
		RequiredCapabilities: []string{"repository.write", "shell"},
		RiskClass:            "high",
		BudgetEUR:            10,
		Status:               "queued",
		Objective:            "Add the runner",
		Repository:           "sf/ai-software-factory-code",
		SHA:                  "abc123",
		Kind:                 "agent",
		IssueNumber:          42,
		Source:               "api",
		ReproCommand:         "go test ./internal/x",
		CreatedAt:            time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC),
	}
	if err := s.CreateTask(ctx, in); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("Task = (%v, %v), want (true, nil)", ok, err)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) || got.Gate != GateIntake || got.GateSince == nil || !got.GateSince.Equal(in.CreatedAt) {
		t.Fatalf("task created %s, gate %q since %v; want intake since %s", got.CreatedAt, got.Gate, got.GateSince, in.CreatedAt)
	}
	got.CreatedAt, got.Gate, got.GateSince = in.CreatedAt, "", nil
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("task = %+v, want %+v", got, in)
	}
	byIssue, ok, err := s.TaskByIssue(ctx, "sf/ai-software-factory-code", 42)
	if err != nil || !ok || byIssue.ID != "t1" {
		t.Fatalf("TaskByIssue = (%+v, %v, %v), want (t1, true, nil)", byIssue, ok, err)
	}
	if _, ok, err := s.TaskByIssue(ctx, "sf/ai-software-factory-code", 999); err != nil || ok {
		t.Fatalf("TaskByIssue unknown issue = (%v, %v), want (false, nil)", ok, err)
	}
	if id, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "looks good"}); err != nil || id == 0 {
		t.Fatalf("InsertIntakeEvent = (%d, %v)", id, err)
	}
	// a comment is recorded once per task and kind, whichever path saw it first
	commentEvent, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "pull_request_comment", Author: "sebastian", Body: "nit", CommentID: 80, Trusted: true})
	if err != nil || commentEvent == 0 {
		t.Fatalf("InsertIntakeEvent comment = (%d, %v)", commentEvent, err)
	}
	if again, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "pull_request_comment", Author: "sebastian", Body: "nit", CommentID: 80, DeliveryID: "d-80"}); err != nil || again != 0 {
		t.Fatalf("InsertIntakeEvent same comment = (%d, %v), want 0", again, err)
	}
	// a claim is exclusive: the second claimant does not signal
	if claimed, err := s.SetSignalled(ctx, commentEvent, true); err != nil || !claimed {
		t.Fatalf("SetSignalled first claim = (%v, %v), want (true, nil)", claimed, err)
	}
	if claimed, err := s.SetSignalled(ctx, commentEvent, true); err != nil || claimed {
		t.Fatalf("SetSignalled second claim = (%v, %v), want (false, nil)", claimed, err)
	}
	if _, err := s.SetSignalled(ctx, 999999, true); err == nil {
		t.Fatal("SetSignalled on no row = nil, want an error")
	}
	events, err := s.IntakeEvents(ctx, "t1")
	if err != nil || len(events) != 2 || events[1].CommentID != 80 || !events[1].Trusted || !events[1].Signalled || events[0].Signalled {
		t.Fatalf("IntakeEvents = %+v, %v", events, err)
	}
	// unaddressed: signalled change requests newer than the latest session
	if got, err := s.UnaddressedChangeRequests(ctx, "t1"); err != nil || len(got) != 1 || got[0].ID != commentEvent {
		t.Fatalf("UnaddressedChangeRequests before any session = %+v, %v", got, err)
	}
	if err := s.InsertSession(ctx, Session{ID: "s-after", TaskID: "t1", CreatedAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UnaddressedChangeRequests(ctx, "t1"); err != nil || len(got) != 0 {
		t.Fatalf("UnaddressedChangeRequests after a session = %+v, %v, want none", got, err)
	}
	if seen, err := s.DeliverySeen(ctx, "d-1"); err != nil || seen {
		t.Fatalf("unseen delivery = (%v, %v)", seen, err)
	}
	for range 2 {
		if err := s.RecordDelivery(ctx, "d-1"); err != nil {
			t.Fatalf("RecordDelivery: %v", err)
		}
	}
	if seen, err := s.DeliverySeen(ctx, "d-1"); err != nil || !seen {
		t.Fatalf("recorded delivery = (%v, %v), want seen", seen, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET received_at = now() - interval '31 days' WHERE id = 'd-1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivery(ctx, "d-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneDeliveries(ctx, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"d-1": false, "d-2": true} {
		if seen, _ := s.DeliverySeen(ctx, id); seen != want {
			t.Fatalf("after prune %s seen = %v, want %v", id, seen, want)
		}
	}
	for i := range 2 {
		id, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "again", DeliveryID: "d-9"})
		if err != nil || (id == 0) != (i == 1) {
			t.Fatalf("InsertIntakeEvent with delivery, round %d = (%d, %v)", i, id, err)
		}
	}
	if err := s.SetTaskPR(ctx, "t1", 12); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t1"); got.PRNumber != 12 || got.HalfWayDone {
		t.Fatalf("pr_number = %d, half_way_done = %v, want 12 and false", got.PRNumber, got.HalfWayDone)
	}
	if err := s.MarkHalfWay(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t1"); !got.HalfWayDone {
		t.Fatal("half_way_done not kept")
	}
	usage := TaskUsage{AgentTime: 90 * time.Second, Attempts: 2, ExtraTime: time.Minute, ExtraAttempts: 1, PosterAttempts: 1}
	if err := s.SetTaskUsage(ctx, "t1", usage); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t1"); got.Usage != usage {
		t.Fatalf("usage = %+v, want %+v", got.Usage, usage)
	}
	if err := s.SetTaskUsage(ctx, "nope", usage); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetTaskUsage unknown task = %v, want ErrNotFound", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 0 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	var intakeCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_events WHERE task_id = 't1'`).Scan(&intakeCount); err != nil {
		t.Fatal(err)
	}
	if intakeCount != 3 {
		t.Fatalf("intake_events for t1 = %d, want 3 (the redelivered d-9 and the re-recorded comment 80 count once each)", intakeCount)
	}

	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT started_at FROM tasks`); err == nil {
		t.Fatal("started_at still exists: nothing read it, migration 0013 drops it")
	}
	for _, step := range [][2]string{{"running", "in-review"}} {
		if err := s.TransitionTask(ctx, "t1", []string{step[0]}, step[1]); err != nil {
			t.Fatalf("TransitionTask %s -> %s: %v", step[0], step[1], err)
		}
	}
	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted from in-review: %v", err)
	}
	if err := s.TransitionTask(ctx, "t1", []string{"running"}, "waiting-human"); err != nil {
		t.Fatalf("TransitionTask running -> waiting-human: %v", err)
	}
	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted from waiting-human (a human answered): %v", err)
	}
	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT started_at FROM tasks`); err == nil {
		t.Fatal("started_at still exists: nothing read it, migration 0013 drops it")
	}
	got, _, _ = s.Task(ctx, "t1")
	if got.Status != "running" {
		t.Fatalf("status = %q", got.Status)
	}
	if err := s.TransitionTask(ctx, "t1", []string{"queued"}, "done"); err == nil {
		t.Fatal("a running task moved from queued")
	}
	if err := s.TransitionTask(ctx, "nope", []string{"running"}, "closing"); err == nil {
		t.Fatal("TransitionTask on a missing task must fail")
	}
	if err := s.CreateTask(ctx, Task{ID: "t0", Priority: 1, BudgetEUR: 1, Status: "running", Objective: "x"}); err != nil {
		t.Fatalf("CreateTask t0: %v", err)
	}
	running, err := s.ListTasksByStatus(ctx, "running")
	if err != nil || len(running) != 2 || running[0].ID != "t1" || running[0].Source != "api" {
		t.Fatalf("running = (%+v, %v), want t1 (priority 2, source round-tripped) before t0", running, err)
	}
	if err := s.TransitionTask(ctx, "t1", []string{"running"}, "closing"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStarted(ctx, "t1"); err == nil {
		t.Fatal("MarkStarted revived a closing task")
	}

	// spend settlement: finished tasks inside the window, raised to at least
	// LiteLLM's figure on every call, never lowered
	for _, row := range []struct {
		id     string
		status string
	}{{"s-done", "done"}, {"s-failed", "failed"}, {"s-old", "done"}, {"s-run", "running"}} {
		if err := s.CreateTask(ctx, Task{ID: row.id, BudgetEUR: 1, SpentEUR: 0.5, Status: "closing"}); err != nil {
			t.Fatal(err)
		}
		if err := s.TransitionTask(ctx, row.id, []string{"closing"}, row.status); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, 'model', 'coding-cheap', 0.5)`, row.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '2 hours' WHERE id = 's-old'`); err != nil {
		t.Fatal(err)
	}
	ids, err := s.RecentlyFinishedTasks(ctx, time.Hour)
	sort.Strings(ids)
	if err != nil || !reflect.DeepEqual(ids, []string{"s-done", "s-failed"}) {
		t.Fatalf("recently finished = (%v, %v), want [s-done s-failed]", ids, err)
	}
	// an open tool hold never touches LiteLLM key spend, so it does not
	// starve the task of settlement
	if _, err := s.db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('s-failed', 'tool', 'web_scrape', 0.02, now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.RecentlyFinishedTasks(ctx, time.Hour); err != nil || !reflect.DeepEqual(ids, []string{"s-done", "s-failed"}) {
		t.Fatalf("recently finished with an open tool hold = (%v, %v), want [s-done s-failed]", ids, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM holds`); err != nil {
		t.Fatal(err)
	}
	// a finished task with a call still holding headroom is left to that call
	if _, err := s.db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('s-failed', 'model', 'coding-cheap', 0.1, now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.RecentlyFinishedTasks(ctx, time.Hour); err != nil || !reflect.DeepEqual(ids, []string{"s-done"}) {
		t.Fatalf("recently finished with an open hold = (%v, %v), want [s-done]", ids, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM holds`); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		id       string
		reported float64
	}{{"s-done", 0.75}, {"s-failed", 0.25}, {"s-done", 0.625}, {"s-done", 0.875}} {
		if _, err := s.SettleSpend(ctx, step.id, step.reported); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]float64{"s-done": 0.875, "s-failed": 0.5} {
		if got, _, _ := s.Task(ctx, id); got.SpentEUR != want {
			t.Fatalf("%s spent = %v, want %v (max of booked and every LiteLLM figure)", id, got.SpentEUR, want)
		}
	}
	// each raise is booked as its delta: 0.5 -> 0.75, then 0.75 -> 0.875
	var settled []string
	rows, err := s.db.QueryContext(ctx, `SELECT task_id || ' ' || kind || ' ' || ref || ' ' || eur::text FROM ledger WHERE kind = 'settlement' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		settled = append(settled, l)
	}
	if want := []string{"s-done settlement litellm 0.25", "s-done settlement litellm 0.125"}; !reflect.DeepEqual(settled, want) {
		t.Fatalf("ledger = %q, want %q", settled, want)
	}
	// settlement weighs LiteLLM's model spend against the booked model spend
	// only: a task that booked 0.75 of models and 0.25 of tools (spent 1.0)
	// while LiteLLM reports 1.0 of models under-booked 0.25; LiteLLM's next
	// 1.125 raises it by another 0.125, 0.9 by nothing
	if err := s.CreateTask(ctx, Task{ID: "s-tool", BudgetEUR: 2, SpentEUR: 1, Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('s-tool', 'model', 'coding-cheap', 0.75), ('s-tool', 'tool', 'web_scrape', 0.25)`); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		reported, want float64
	}{{1, 1.25}, {1.125, 1.375}, {0.9, 1.375}} {
		if _, err := s.SettleSpend(ctx, "s-tool", step.reported); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.Task(ctx, "s-tool"); got.SpentEUR != step.want {
			t.Fatalf("LiteLLM %v: spent = %v, want %v", step.reported, got.SpentEUR, step.want)
		}
	}
	// a difference below a nano-euro is float noise and books nothing
	if _, err := s.SettleSpend(ctx, "s-tool", 1.125+5e-10); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "s-tool"); got.SpentEUR != 1.375 {
		t.Fatalf("after 5e-10: spent = %v, want 1.375", got.SpentEUR)
	}
	var toolSettled string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(eur::text, ' ' ORDER BY id) FROM ledger WHERE task_id = 's-tool' AND kind = 'settlement'`).Scan(&toolSettled); err != nil || toolSettled != "0.25 0.125" {
		t.Fatalf("s-tool settlement rows = %q (%v), want 0.25 0.125", toolSettled, err)
	}
	// 2e-9 above the booked 1.125 is a real raise: one row of it
	if _, err := s.SettleSpend(ctx, "s-tool", 1.125+2e-9); err != nil {
		t.Fatal(err)
	}
	var n int
	var last float64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), (array_agg(eur::float8 ORDER BY id DESC))[1] FROM ledger WHERE task_id = 's-tool' AND kind = 'settlement'`).Scan(&n, &last); err != nil ||
		n != 3 || math.Abs(last-2e-9) > 1e-12 {
		t.Fatalf("after 2e-9: %d settlement rows, last %v (%v); want 3, 2e-9", n, last, err)
	}
	if _, err := s.SettleSpend(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SettleSpend of an unknown task = %v, want ErrNotFound", err)
	}
	// the final settlement (ruling 196): a task past the window, two
	// reviews the gate cut booked at their hold of 0.1447 each on top of
	// 0.1873 of answered calls (0.4767), while LiteLLM charged 0.2751: set
	// to 0.2751, the difference a negative settlement row on the task's
	// own day, and settled once
	if err := s.CreateTask(ctx, Task{ID: "s-cut", BudgetEUR: 2, SpentEUR: 0.4767, Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '3 hours' WHERE id = 's-cut'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ('s-cut', current_date - 1, 'model', 'coding-cheap', 0.1873), ('s-cut', current_date - 1, 'model', 'review/review-frontier', 0.1447), ('s-cut', current_date - 1, 'model', 'review/review-frontier', 0.1447)`); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.UnsettledTasks(ctx, 2*time.Hour); err != nil || !reflect.DeepEqual(ids, []string{"s-cut", "s-old"}) {
		t.Fatalf("unsettled past 2h = (%v, %v), want [s-cut s-old]", ids, err)
	}
	if _, err := s.SettleSpendFinal(ctx, "s-cut", 0.2751); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "s-cut"); math.Abs(got.SpentEUR-0.2751) > 1e-9 {
		t.Fatalf("s-cut spent = %v, want 0.2751 (LiteLLM's final figure)", got.SpentEUR)
	}
	var cutRow string
	if err := s.db.QueryRowContext(ctx, `SELECT ledger.ref || ' ' || round(ledger.eur::numeric, 4)::text || ' ' || (ledger.day = (tasks.finished_at::date))::text FROM ledger JOIN tasks ON tasks.id = ledger.task_id WHERE ledger.task_id = 's-cut' AND ledger.kind = 'settlement'`).Scan(&cutRow); err != nil || cutRow != "litellm-final -0.2016 true" {
		t.Fatalf("s-cut settlement row = %q (%v), want litellm-final -0.2016 on the task's finish day", cutRow, err)
	}
	if ids, err := s.UnsettledTasks(ctx, 2*time.Hour); err != nil || !reflect.DeepEqual(ids, []string{"s-old"}) {
		t.Fatalf("unsettled after the final settlement = (%v, %v), want [s-old]", ids, err)
	}
	if _, err := s.SettleSpendFinal(ctx, "s-cut", 0.1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second final settlement = %v, want ErrNotFound (settled once)", err)
	}
	// LiteLLM's figure equal to the booked spend marks the task settled with no row
	if _, err := s.SettleSpendFinal(ctx, "s-old", 0.5); err != nil {
		t.Fatal(err)
	}
	var oldRows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger WHERE task_id = 's-old' AND kind = 'settlement'`).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatalf("s-old settlement rows = %d (%v), want 0", oldRows, err)
	}
	if ids, err := s.UnsettledTasks(ctx, 2*time.Hour); err != nil || len(ids) != 0 {
		t.Fatalf("unsettled after both = (%v, %v), want none", ids, err)
	}

	_, ok, err = s.Task(ctx, "nope")
	if err != nil || ok {
		t.Fatalf("unknown task = (%v, %v), want (false, nil)", ok, err)
	}

	restart, _ := json.Marshal(map[string]any{"last_step": 3})
	when := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	sessIn := Session{ID: "s1", TaskID: "t1", Findings: []string{"repo cloned"}, RestartState: restart, CreatedAt: when}
	if err := s.InsertSession(ctx, sessIn); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	sess, ok, err := s.Session(ctx, "s1")
	if err != nil || !ok {
		t.Fatalf("Session = (%v, %v), want (true, nil)", ok, err)
	}
	var rs map[string]any
	if err := json.Unmarshal(sess.RestartState, &rs); err != nil {
		t.Fatalf("restart_state is not JSON: %v", err)
	}
	if sess.TaskID != "t1" || !reflect.DeepEqual(sess.Findings, []string{"repo cloned"}) ||
		!reflect.DeepEqual(rs, map[string]any{"last_step": float64(3)}) {
		t.Fatalf("session = %+v restart=%v", sess, rs)
	}

	restart2, _ := json.Marshal(map[string]any{"last_step": 7})
	if err := s.UpdateSession(ctx, "s1", []string{"repo cloned", "tests green"}, restart2); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	sess, _, _ = s.Session(ctx, "s1")
	rs = nil
	json.Unmarshal(sess.RestartState, &rs)
	if len(sess.Findings) != 2 || !reflect.DeepEqual(rs, map[string]any{"last_step": float64(7)}) {
		t.Fatalf("updated session = %+v restart=%v", sess, rs)
	}

	// Order independence: a task row created the way the gateway seeds one
	// (only its columns) must survive factoryd's boot and gain the defaults.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('t-gw', 5, 0, 'running')`,
	); err != nil {
		t.Fatalf("gateway-style seed: %v", err)
	}
	if err := schema.Migrate(ctx, s2.db); err != nil {
		t.Fatalf("migrate over gateway-created row: %v", err)
	}
	gw, ok, err := s2.Task(ctx, "t-gw")
	if err != nil || !ok || gw.Priority != 0 || gw.RiskClass != "standard" || len(gw.RequiredCapabilities) != 0 ||
		gw.Kind != "agent" || gw.IssueNumber != 0 {
		t.Fatalf("gateway-seeded task = %+v (%v, %v)", gw, ok, err)
	}

	// the capability backfill gives pre-policy agent tasks "coder", once
	if _, err := s.db.ExecContext(ctx, `DELETE FROM backfills WHERE name = '0014_task_capabilities';
		DELETE FROM schema_migrations WHERE name = 'factoryd/0014_task_capabilities.sql';
		INSERT INTO tasks (id, budget_eur, status, kind, required_capabilities) VALUES
		('b-agent', 1, 'done', 'agent', 'null'), ('b-human', 1, 'done', 'human', 'null')`); err != nil {
		t.Fatal(err)
	}
	if err := schema.Migrate(ctx, s2.db); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][]string{"b-agent": {"coder"}, "b-human": nil, "t-gw": {"coder"}} {
		if got, _, _ := s2.Task(ctx, id); !reflect.DeepEqual(got.RequiredCapabilities, want) {
			t.Fatalf("%s capabilities after the backfill = %v, want %v", id, got.RequiredCapabilities, want)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET required_capabilities = '[]' WHERE id = 't-gw'`); err != nil {
		t.Fatal(err)
	}
	if err := schema.Migrate(ctx, s2.db); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s2.Task(ctx, "t-gw"); len(got.RequiredCapabilities) != 0 {
		t.Fatalf("the backfill ran twice: t-gw capabilities = %v", got.RequiredCapabilities)
	}
}

func TestPGStoreApprovals(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, approval_requests`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "a1", Status: "in-review", Repository: "sf/r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskRisk(ctx, "a1", "high"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "a1"); got.RiskClass != "high" {
		t.Fatalf("risk class = %q", got.RiskClass)
	}
	first, err := s.OpenApproval(ctx, ApprovalRequest{Kind: "merge", TaskID: "a1", Project: "sf/r", PRNumber: 9, Spec: json.RawMessage(`{"risk_class":"high"}`), Reason: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.OpenApproval(ctx, ApprovalRequest{Kind: "merge", TaskID: "a1", Project: "sf/r", PRNumber: 9, Spec: json.RawMessage(`{"risk_class":"normal"}`), Reason: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	eur := 3.5
	res, err := s.OpenApproval(ctx, ApprovalRequest{Kind: "resource", TaskID: "a1", Project: "sf/r", Reason: "a VM", MonthlyEUR: &eur})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.ApprovalRequests(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != second || pending[0].PRNumber != 9 || string(pending[0].Spec) != `{"risk_class": "normal"}` ||
		pending[1].ID != res || pending[1].PRNumber != 0 || *pending[1].MonthlyEUR != 3.5 {
		t.Fatalf("pending = %+v", pending)
	}
	denied, _ := s.ApprovalRequests(ctx, "denied")
	if len(denied) != 1 || denied[0].ID != first || denied[0].DecidedBy != "factoryd" || denied[0].DecidedAt == nil {
		t.Fatalf("denied = %+v", denied)
	}
	// the database refuses a second pending merge request of a task
	if _, err := s.db.ExecContext(ctx, `INSERT INTO approval_requests (kind, task_id, project) VALUES ('merge', 'a1', 'sf/r')`); err == nil {
		t.Fatal("a second pending merge request was accepted")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO approval_requests (kind, task_id, project, status) VALUES ('merge', 'a2', 'sf/r', 'closed')`); err == nil {
		t.Fatal("an unknown status was accepted")
	}
	// a merge approves the merge request only: the task's resource
	// request is no merge's business
	if err := s.ResolveApprovals(ctx, "a1", "merge", "approved", "sebastian", "merged"); err != nil {
		t.Fatal(err)
	}
	approved, _ := s.ApprovalRequests(ctx, "approved")
	if len(approved) != 1 || approved[0].ID != second || approved[0].DecidedBy != "sebastian" || approved[0].DecisionNote != "merged" {
		t.Fatalf("approved = %+v", approved)
	}
	// the console decides one pending request of its kind, once
	if err := s.DecideApproval(ctx, res, "merge", "denied", "sebastian", "wrong kind"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deciding a resource request as a merge: %v", err)
	}
	if err := s.DecideApproval(ctx, res, "resource", "denied", "sebastian", "too big"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideApproval(ctx, res, "resource", "approved", "sebastian", "changed my mind"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a decided request was decided again: %v", err)
	}
	denied, _ = s.ApprovalRequests(ctx, "denied")
	if len(denied) != 2 || denied[1].ID != res || denied[1].DecidedBy != "sebastian" || denied[1].DecisionNote != "too big" || denied[1].DecidedAt == nil {
		t.Fatalf("denied = %+v", denied)
	}
	// a refused merge is noted on the pending merge request only
	if _, err := s.OpenApproval(ctx, ApprovalRequest{Kind: "merge", TaskID: "a3", Project: "sf/r", PRNumber: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteApproval(ctx, "a3", "merge", "waiting: Forgejo refused the merge"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ApprovalRequests(ctx, "pending"); len(p) != 1 || p[0].DecisionNote != "waiting: Forgejo refused the merge" || p[0].Status != "pending" {
		t.Fatalf("noted = %+v", p)
	}
	if err := s.ResolveApprovals(ctx, "a3", "", "denied", "factoryd", "done"); err != nil {
		t.Fatal(err)
	}
	// the task's end denies whatever of every kind is still pending
	third, err := s.OpenApproval(ctx, ApprovalRequest{Kind: "resource", TaskID: "a1", Project: "sf/r", Reason: "another VM"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApprovals(ctx, "a1", "", "denied", "factoryd", "the task ended"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.ApprovalRequests(ctx, "pending"); len(pending) != 0 {
		t.Fatalf("pending after the end = %+v (third %d)", pending, third)
	}
}

func TestPGStoreRollsAndAlertCounts(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE rolls, alerts`); err != nil {
		t.Fatal(err)
	}
	r := Roll{SHA: "abc", Image: "gateway", Deployment: "factory-gateway",
		PushedAt: at("2026-09-28T10:05:40Z"), AppliedAt: at("2026-09-28T10:07:11Z"), LiveAt: at("2026-09-28T10:09:50Z")}
	if ok, err := s.RollRecorded(ctx, "abc", "gateway"); err != nil || ok {
		t.Fatalf("recorded before insert: %v %v", ok, err)
	}
	if ok, err := s.RecordRoll(ctx, r); err != nil || !ok {
		t.Fatalf("first record: %v %v", ok, err)
	}
	second := r
	second.PushedAt = at("2026-09-28T11:00:00Z")
	if ok, err := s.RecordRoll(ctx, second); err != nil || ok {
		t.Fatalf("second record of the same sha and image: %v %v", ok, err)
	}
	var pushed time.Time
	var deployment string
	if err := s.db.QueryRowContext(ctx, `SELECT pushed_at, deployment FROM rolls WHERE sha = 'abc' AND image = 'gateway'`).Scan(&pushed, &deployment); err != nil ||
		!pushed.Equal(r.PushedAt) || deployment != "factory-gateway" {
		t.Fatalf("the first row stands: pushed %v deployment %q, %v", pushed, deployment, err)
	}
	if ok, err := s.RollRecorded(ctx, "abc", "gateway"); err != nil || !ok {
		t.Fatalf("recorded: %v %v", ok, err)
	}
	if ok, err := s.RollRecorded(ctx, "abc", "worker"); err != nil || ok {
		t.Fatalf("another image of the same commit recorded: %v %v", ok, err)
	}
	worker := r
	worker.Image, worker.Deployment = "worker", "factoryd"
	if ok, err := s.RecordRoll(ctx, worker); err != nil || !ok {
		t.Fatalf("record another image of the same commit: %v %v", ok, err)
	}

	for _, a := range []Alert{
		{Kind: AlertGateBound, Source: "WaitChecks", TaskID: "t1", BoundS: 600},
		{Kind: AlertGateBound, Source: "WaitChecks", TaskID: "t1", BoundS: 600},
	} {
		if _, err := s.InsertAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	esc, err := s.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlertVerdict(ctx, esc, VerdictReal, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: AlertInfoRequest, Source: "agent", TaskID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.JudgeTaskAlerts(ctx, "t1", AlertInfoRequest, VerdictUnjudged, FactoryDecider); err != nil {
		t.Fatal(err)
	}
	counts, err := s.AlertCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(counts, func(a, b AlertCount) int { return strings.Compare(a.Kind, b.Kind) })
	if want := []AlertCount{{"escalation", "real", 1}, {"gate_bound", "open", 2}, {"info_request", "unjudged", 1}}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts %v, want %v", counts, want)
	}
}

// Migration 0026 on a real Postgres: the author and the agent's summary
// on the row, a backlog card outside every gate until it enters a lane, closed
// as a final state, the info_request alert kind, projects by repository.
func TestPGStoreIntakeAuto(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, ledger, holds, approval_requests, alerts`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-sug", Status: "backlog", Objective: "o", Repository: "sf/r", Kind: "agent",
		IssueNumber: 7, Source: "webhook", Author: "outsider", Lane: "backlog", Process: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Task(ctx, "t-sug")
	if err != nil || got.Author != "outsider" || got.AuthorWriter || got.Gate != "" || got.GateSince != nil || len(got.RequiredCapabilities) != 0 {
		t.Fatalf("backlog row = %+v, %v; want author outsider, outside account, no gate, no class", got, err)
	}
	if ok, err := s.EnterLane(ctx, "t-sug", "ready", "coder", "abc"); err != nil || !ok {
		t.Fatalf("enter: %v %v", ok, err)
	}
	if ok, err := s.EnterLane(ctx, "t-sug", "ready", "coder", "def"); err != nil || ok {
		t.Fatalf("second entry: %v %v, want false", ok, err)
	}
	got, _, _ = s.Task(ctx, "t-sug")
	if got.Status != "queued" || got.Lane != "ready" || got.Process != "maintenance" || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) ||
		got.SHA != "abc" || got.Gate != GateIntake || got.GateSince == nil {
		t.Fatalf("entered row = %+v; want queued in lane ready as coder at abc in the intake gate", got)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-w", Status: "queued", Objective: "o", Repository: "sf/r", Kind: "agent",
		IssueNumber: 8, Source: "webhook", Author: "sebastian", AuthorWriter: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentSummary(ctx, "t-w", "fixed it\nnot_reproduced: x"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionTask(ctx, "t-w", []string{"queued"}, "waiting-poster"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStarted(ctx, "t-w"); err != nil {
		t.Fatalf("waiting-poster to running: %v", err)
	}
	if err := s.TransitionTask(ctx, "t-w", []string{"running"}, "closed"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Task(ctx, "t-w")
	if !got.AuthorWriter || got.AgentSummary != "fixed it\nnot_reproduced: x" || got.FinishedAt == nil || got.Gate != "" {
		t.Fatalf("closed row = %+v; want writer, the summary, finished, no gate", got)
	}
	if ids, err := s.RecentlyFinishedTasks(ctx, time.Hour); err != nil || !slices.Contains(ids, "t-w") {
		t.Fatalf("recently finished = %v, %v; want t-w", ids, err)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: AlertInfoRequest, Source: "agent", TaskID: "t-w"}); err != nil {
		t.Fatalf("info_request alert: %v", err)
	}
	if _, ok, err := s.ProjectByRepository(ctx, "sf/nothing"); err != nil || ok {
		t.Fatalf("unknown repository: %v %v", ok, err)
	}
}

// Migration 0027 on a real Postgres: a SigNoz alert row found again by its
// fingerprint (the newest one), its detail merged, and the verdict set once
// on a task's open alert rows only.
func TestPGStoreConfigRejectedAlert(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE alerts`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t-alert", Fingerprint: "c1"}); err != nil {
		t.Fatal(err)
	}
	// a refused configuration commit is its own kind, keyed by the commit
	if _, ok, err := s.AlertByFingerprint(ctx, AlertConfigRejected, "c1"); err != nil || ok {
		t.Fatalf("config_rejected before any row: %v %v", ok, err)
	}
	rejected, err := s.InsertAlert(ctx, Alert{Kind: AlertConfigRejected, Source: "loader", Fingerprint: "c1", Detail: map[string]any{"commit": "c1"}})
	if err != nil {
		t.Fatalf("insert config_rejected: %v", err)
	}
	if err := s.SetAlertTask(ctx, rejected, "t-cfg", map[string]any{"issue": 41}); err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AlertByFingerprint(ctx, AlertConfigRejected, "c1")
	if err != nil || !ok || a.ID != rejected || a.Verdict != "" || a.TaskID != "t-cfg" || a.Detail["issue"] != float64(41) || a.Detail["commit"] != "c1" {
		t.Fatalf("config_rejected by fingerprint: %+v %v %v, want row %d open on t-cfg", a, ok, err, rejected)
	}
	if err := s.ResolveAlert(ctx, rejected, AlertGateBound, VerdictReal, "factoryd", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve as another kind = %v, want ErrNotFound", err)
	}
	if err := s.ResolveAlert(ctx, rejected, AlertConfigRejected, VerdictReal, "factoryd", map[string]any{"fixed_by": "c2"}); err != nil {
		t.Fatal(err)
	}
	a, _, _ = s.AlertByFingerprint(ctx, AlertConfigRejected, "c1")
	if a.Verdict != VerdictReal || a.ResolvedBy != "factoryd" || a.ResolvedAt == nil || a.Detail["fixed_by"] != "c2" {
		t.Fatalf("resolved config_rejected = %+v", a)
	}
}

func TestPGStoreSigNozAlerts(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE alerts`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AlertByFingerprint(ctx, AlertSigNoz, "fp1"); err != nil || ok {
		t.Fatalf("before any row: %v %v", ok, err)
	}
	old, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t-old", Fingerprint: "fp1", Detail: map[string]any{"issue": 3}})
	if err != nil {
		t.Fatal(err)
	}
	newest, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t-new", Fingerprint: "fp1", Detail: map[string]any{"issue": 9}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "p95", TaskID: "t-new", Fingerprint: "fp2"}); err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AlertByFingerprint(ctx, AlertSigNoz, "fp1")
	if err != nil || !ok || a.ID != newest || a.TaskID != "t-new" || a.Fingerprint != "fp1" || a.Detail["issue"] != float64(9) {
		t.Fatalf("by fingerprint: %+v %v %v, want row %d", a, ok, err, newest)
	}
	if err := s.MergeAlertDetail(ctx, newest, map[string]any{"resolved_at": "2026-09-28T20:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	a, _, _ = s.AlertByFingerprint(ctx, AlertSigNoz, "fp1")
	if a.Detail["issue"] != float64(9) || a.Detail["resolved_at"] != "2026-09-28T20:00:00Z" {
		t.Fatalf("merged detail %v", a.Detail)
	}
	if err := s.JudgeTaskAlerts(ctx, "t-new", AlertSigNoz, VerdictReal, "merge"); err != nil {
		t.Fatal(err)
	}
	if err := s.JudgeTaskAlerts(ctx, "t-new", AlertSigNoz, VerdictNoise, "sebastian"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.AlertCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(counts, func(a, b AlertCount) int { return strings.Compare(a.Verdict, b.Verdict) })
	if want := []AlertCount{{"alert", "open", 1}, {"alert", "real", 2}}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts %v, want %v (row %d stays open)", counts, want, old)
	}
}

// Migration 0028 on a real Postgres: a report task opens once per parent
// and source, keeps its parent, and is found again; a screening alert is
// judged apart from the task's SigNoz alerts.
func TestPGStoreReportTasksAndScreeningAlerts(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, alerts CASCADE`); err != nil {
		t.Fatal(err)
	}
	parent := Task{ID: "t-parent", RiskClass: "standard", BudgetEUR: 2, Status: "backlog", Objective: "o", Repository: "sf/r", IssueNumber: 5, Source: "webhook"}
	if err := s.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	report := Task{ID: "t-report", RiskClass: "standard", BudgetEUR: 2, Status: "queued", Objective: "o", Repository: "sf/r", Source: SourcePresort,
		Parent: "t-parent", RequiredCapabilities: []string{"analyzer"}}
	got, err := s.OpenReportTask(ctx, report)
	if err != nil || got.ID != "t-report" || got.Parent != "t-parent" || got.RequiredCapabilities[0] != "analyzer" {
		t.Fatalf("open: %+v %v", got, err)
	}
	again := report
	again.ID = "t-other"
	if got, err := s.OpenReportTask(ctx, again); err != nil || got.ID != "t-report" {
		t.Fatalf("open again: %+v %v, want the first row", got, err)
	}
	if _, ok, _ := s.Task(ctx, "t-other"); ok {
		t.Fatal("a second report row exists")
	}
	if got, ok, err := s.ReportOf(ctx, "t-parent", SourcePresort); err != nil || !ok || got.ID != "t-report" {
		t.Fatalf("report of: %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.ReportOf(ctx, "t-parent", SourceScreening); err != nil || ok {
		t.Fatalf("no screening: %v %v", ok, err)
	}
	if p, _, _ := s.Task(ctx, "t-parent"); p.Parent != "" {
		t.Fatalf("parent's parent = %q", p.Parent)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: AlertScreening, Source: "outside account", TaskID: "t-parent"}); err != nil {
		t.Fatal(err)
	}
	// a retried screening finds its row: no second one, id 0
	if id, err := s.InsertAlert(ctx, Alert{Kind: AlertScreening, Source: "outside account", TaskID: "t-parent"}); err != nil || id != 0 {
		t.Fatalf("second screening alert: id %d, %v", id, err)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t-parent", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.JudgeTaskAlerts(ctx, "t-parent", AlertScreening, VerdictNoise, "sebastian"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.AlertCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(counts, func(a, b AlertCount) int { return strings.Compare(a.Kind, b.Kind) })
	if want := []AlertCount{{"alert", "open", 1}, {"screening", "noise", 1}}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts %v, want %v", counts, want)
	}
}

// Settlement sets spent_eur to the task's ledger sum instead of adding its
// delta: a task whose spend predates the ledger holds LiteLLM's figure in
// spent_eur and no model row, so an added delta counted that spend twice
// (41 tasks at the final pass of 2026-09-29 20:38:20Z). Each call reports
// whether it changed the task's spend.
func TestPGStoreSettlesSpendFromTheLedger(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "pre-final", BudgetEUR: 2, SpentEUR: 0.5, Status: "done"},
		{ID: "pre-window", BudgetEUR: 2, SpentEUR: 0.5, Status: "done"},
		{ID: "same", BudgetEUR: 2, SpentEUR: 0.25, Status: "done"},
	} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '3 hours' WHERE id IN ('pre-final', 'pre-window', 'same');
		INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('same', 'model', 'coding-cheap', 0.25)`); err != nil {
		t.Fatal(err)
	}
	spent := func(id string) float64 {
		t.Helper()
		got, _, err := s.Task(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got.SpentEUR
	}

	// no model row: the whole 0.5 is booked as the settlement, and spent_eur
	// is that row, not 0.5 + 0.5
	if changed, err := s.SettleSpendFinal(ctx, "pre-final", 0.5); err != nil || !changed {
		t.Fatalf("SettleSpendFinal(pre-final) = (%v, %v), want (true, nil)", changed, err)
	}
	if got := spent("pre-final"); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("pre-final spent = %v, want 0.5 (its ledger sum)", got)
	}
	// inside the window likewise: 0.625 booked, spent 0.625, not 1.125
	if changed, err := s.SettleSpend(ctx, "pre-window", 0.625); err != nil || !changed {
		t.Fatalf("SettleSpend(pre-window) = (%v, %v), want (true, nil)", changed, err)
	}
	if got := spent("pre-window"); math.Abs(got-0.625) > 1e-9 {
		t.Fatalf("pre-window spent = %v, want 0.625 (its ledger sum)", got)
	}
	// the same figure again books nothing and changes nothing
	if changed, err := s.SettleSpend(ctx, "pre-window", 0.625); err != nil || changed {
		t.Fatalf("second SettleSpend(pre-window) = (%v, %v), want (false, nil)", changed, err)
	}
	// LiteLLM's figure equal to the booked model spend: settled, unchanged
	if changed, err := s.SettleSpendFinal(ctx, "same", 0.25); err != nil || changed {
		t.Fatalf("SettleSpendFinal(same) = (%v, %v), want (false, nil)", changed, err)
	}
	if got := spent("same"); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("same spent = %v, want 0.25", got)
	}
}

// The month is the UTC calendar month over the ledger, standing lines left
// out, whatever the session's zone; the cap counts as reached at the
// month's first refused_month_cap of a model call. Hand-derived at
// 2026-10-01 05:00 UTC (2026-09-30 22:00 in Los Angeles): 30 on 2026-09-30
// and a standing 10 do not count, 0.03 on 2026-10-01 and 0.01 on
// 2026-10-02 do: 0.04.
func TestPGStoreMonthCap(t *testing.T) {
	u, err := url.Parse(schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("timezone", "America/Los_Angeles")
	u.RawQuery = q.Encode()
	ctx := context.Background()
	s, err := OpenStore(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE alerts, ledger, audit;
		INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
			('sep', '2026-09-30', 'model', 'x', 30),
			('oct1', '2026-10-01', 'model', 'x', 0.03),
			('standing:sf/app', '2026-10-01', 'standing', 'resource/1', 10),
			('oct2', '2026-10-02', 'model', 'x', 0.01);
		INSERT INTO audit (task_id, decision, reason, tool, ts) VALUES
			('u', 'refused_month_cap', 'last month', '', '2026-09-30 23:59:59Z'),
			('r', 'refused_month_cap', 'the reservation exceeds the month''s remaining headroom', 'commission_server', '2026-10-01 00:10:00Z'),
			('t', 'refused_day_cap', 'the day', '', '2026-10-01 00:20:00Z')`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	m, err := s.MonthSpend(ctx, at)
	if err != nil || m.Month != "2026-10" || math.Abs(m.EUR-0.04) > 1e-12 || m.Refusal != "" {
		t.Fatalf("MonthSpend = (%+v, %v), want 2026-10, 0.04 EUR, no model call refused", m, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, ts) VALUES
		('t', 'refused_month_cap', 'the month', '2026-10-01 00:30:00Z'), ('t', 'refused_month_cap', 'later', '2026-10-01 00:40:00Z')`); err != nil {
		t.Fatal(err)
	}
	if m, err := s.MonthSpend(ctx, at); err != nil || m.Refusal != "the month" {
		t.Fatalf("MonthSpend after a model call's refusal = (%+v, %v), want the month's first", m, err)
	}
	if reason, ok, err := s.MonthCapRefusal(ctx, "t", time.Date(2026, 10, 1, 0, 35, 0, 0, time.UTC)); err != nil || !ok || reason != "later" {
		t.Fatalf("t's refusal since 00:35 = (%q, %v, %v), want later", reason, ok, err)
	}
	if _, ok, err := s.MonthCapRefusal(ctx, "t", time.Date(2026, 10, 1, 0, 45, 0, 0, time.UTC)); err != nil || ok {
		t.Fatalf("t's refusal since 00:45 = %v, %v, want none", ok, err)
	}
	if reason, ok, err := s.MonthCapRefusal(ctx, "r", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil || !ok || !strings.Contains(reason, "reservation") {
		t.Fatalf("r's refusal = (%q, %v, %v), want the reservation's", reason, ok, err)
	}
	if _, ok, err := s.MonthCapRefusal(ctx, "v", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil || ok {
		t.Fatalf("v's refusal = %v, %v, want none", ok, err)
	}
	month := m.Month
	// one alert per month and percent
	first, err := s.InsertAlert(ctx, Alert{Kind: AlertMonthCap, Source: "50", Fingerprint: month + "/50"})
	if err != nil || first == 0 {
		t.Fatalf("first 50 percent alert = %d, %v", first, err)
	}
	if again, err := s.InsertAlert(ctx, Alert{Kind: AlertMonthCap, Source: "50", Fingerprint: month + "/50"}); err != nil || again != 0 {
		t.Fatalf("second 50 percent alert = %d, %v, want 0", again, err)
	}
	if next, err := s.InsertAlert(ctx, Alert{Kind: AlertMonthCap, Source: "80", Fingerprint: month + "/80"}); err != nil || next == 0 {
		t.Fatalf("80 percent alert = %d, %v", next, err)
	}
	// SigNoz alerts share a fingerprint freely
	for range 2 {
		if id, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", Fingerprint: month + "/50"}); err != nil || id == 0 {
			t.Fatalf("SigNoz alert = %d, %v", id, err)
		}
	}
}
